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
