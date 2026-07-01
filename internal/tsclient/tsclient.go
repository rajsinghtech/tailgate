// Package tsclient is the slice of the Tailscale API the operator needs during
// reconciliation: mint a tagged authkey for a group's gateway, and delete that
// gateway's device by hostname on teardown (finalizer). It wraps the official
// client (tailscale.com/client/tailscale/v2), authenticating with an OAuth client
// (id+secret) scoped to a single tailnet — for the e2e that is the per-run
// ephemeral tailnet's bundled child client.
package tsclient

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	tsapi "tailscale.com/client/tailscale/v2"
)

// authKeyTTL is the gateway authkey lifetime. The key is reusable and the operator
// owns device deletion, so this only bounds how long an unused key stays valid.
const authKeyTTL = 90 * 24 * time.Hour

// Client is the reconcile-time Tailscale API surface (mockable in tests).
type Client interface {
	MintAuthKey(ctx context.Context, tags []string) (string, error)
	DeleteDeviceByHostname(ctx context.Context, hostname string) error
	// ResolveExitNode returns the tailnet IP of an eligible exit node — one advertising the
	// default route (0.0.0.0/0), preferring a control-connected node. Resolves "auto".
	ResolveExitNode(ctx context.Context) (string, error)
	// TailnetName returns the tailnet's DNS suffix (e.g. "corp.ts.net") by listing
	// devices and extracting it from the first device's MagicDNS FQDN. Used by the
	// DNS webhook to prepend the tailnet to member pods' search list so bare
	// MagicDNS node names resolve. Returns "" if no devices exist yet.
	TailnetName(ctx context.Context) (string, error)
}

type api struct{ ts *tsapi.Client }

// New returns a Client for tailnet (dnsName or "-"), authenticated by an OAuth client.
func New(tailnet, oauthID, oauthSecret string) Client {
	return &api{ts: &tsapi.Client{
		Tailnet: tailnet,
		Auth:    &tsapi.OAuth{ClientID: oauthID, ClientSecret: oauthSecret},
	}}
}

func (a *api) MintAuthKey(ctx context.Context, tags []string) (string, error) {
	// reusable so gateway pods (re)auth from one key; NOT ephemeral — the operator owns
	// the device lifecycle (finalizer deletes it via the API), and the gateway persists
	// its tailscaled state, so the node identity is stable across restarts.
	var req tsapi.CreateKeyRequest
	req.Capabilities.Devices.Create.Reusable = true
	req.Capabilities.Devices.Create.Ephemeral = false
	req.Capabilities.Devices.Create.Preauthorized = true
	req.Capabilities.Devices.Create.Tags = tags
	req.ExpirySeconds = int64(authKeyTTL.Seconds())
	req.Description = "tailgate gateway"

	key, err := a.ts.Keys().CreateAuthKey(ctx, req)
	if err != nil {
		return "", fmt.Errorf("create authkey: %w", err)
	}
	if key.Key == "" {
		return "", fmt.Errorf("empty authkey")
	}
	return key.Key, nil
}

func (a *api) DeleteDeviceByHostname(ctx context.Context, hostname string) error {
	devices, err := a.ts.Devices().List(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}
	for _, d := range devices {
		if d.Hostname == hostname {
			if err := a.ts.Devices().Delete(ctx, d.ID); err != nil {
				return fmt.Errorf("delete device %s: %w", d.ID, err)
			}
			return nil
		}
	}
	return nil // already gone
}

// ResolveExitNode returns the tailnet IPv4 of an eligible exit node — one advertising the
// default route (0.0.0.0/0) — preferring a control-connected (online) node. The returned IP is
// the concrete node the gateway pins, since the declarative tailscaled config cannot express
// "auto" (the conffile parses a non-IP exit-node string as a literal node ID, not an expression).
func (a *api) ResolveExitNode(ctx context.Context) (string, error) {
	devices, err := a.ts.Devices().List(ctx, tsapi.WithFields(tsapi.IncludeFieldsAll))
	if err != nil {
		return "", fmt.Errorf("list devices: %w", err)
	}
	var fallback string
	for i := range devices {
		d := &devices[i]
		if !advertisesDefault(d.AdvertisedRoutes) {
			continue
		}
		addr := tailnetV4(d.Addresses)
		if addr == "" {
			continue
		}
		if d.ConnectedToControl {
			return addr, nil // prefer an online exit node
		}
		if fallback == "" {
			fallback = addr
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no eligible exit node advertising 0.0.0.0/0")
}

func advertisesDefault(routes []string) bool {
	for _, r := range routes {
		if r == "0.0.0.0/0" {
			return true
		}
	}
	return false
}

func tailnetV4(addrs []string) string {
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
			return a
		}
	}
	return ""
}

// TailnetName resolves the tailnet DNS suffix (e.g. "corp.ts.net") by listing
// devices and extracting it from the first device's MagicDNS FQDN. A device's
// Name field is "<hostname>.<tailnet-suffix>" (e.g. "node1.corp.ts.net"), so
// the tailnet suffix is everything after the first dot. Returns "" if the
// tailnet has no devices yet.
func (a *api) TailnetName(ctx context.Context) (string, error) {
	devices, err := a.ts.Devices().List(ctx)
	if err != nil {
		return "", fmt.Errorf("list devices for tailnet name: %w", err)
	}
	for _, d := range devices {
		name := strings.TrimSuffix(d.Name, ".")
		if idx := strings.Index(name, "."); idx > 0 {
			return name[idx+1:], nil
		}
	}
	return "", nil
}
