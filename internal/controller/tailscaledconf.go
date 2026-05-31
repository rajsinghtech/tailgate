package controller

import (
	"encoding/json"
	"fmt"

	"tailscale.com/ipn"
	"tailscale.com/types/opt"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

// Config file paths inside the gateway pod. The authkey is mounted as a file and
// referenced with the "file:" prefix so it never lands in the rendered config (and so
// rotating the Secret doesn't churn the config). The config itself is a projected
// ConfigMap mount the entrypoint watches for hot-reload.
const (
	gwConfigDir   = "/etc/tailgate"
	gwConfigPath  = gwConfigDir + "/tailscaled.json"
	gwAuthKeyPath = gwConfigDir + "/authkey"
)

// renderGatewayConfig builds the declarative tailscaled config (ConfigVAlpha, version
// "alpha0") for a group's gateway from the EgressGroup spec. The gateway is a CLIENT:
// it accepts routes and DNS but never advertises. DNS is forced on (MagicDNS/split-DNS
// is core to egress and required by app connectors). Tags are NOT a config field — they
// ride on the authkey (minted with tags by tsclient) — so they must stay out of here or
// conffile's DisallowUnknownFields rejects the file.
func renderGatewayConfig(eg *egressv1.EgressGroup, exitNodeID string) ([]byte, error) {
	authKeyRef := "file:" + gwAuthKeyPath
	hostname := gatewayName(eg.Name)
	netfilter := "off" // we own MASQUERADE + forwarding; keep tailscaled out of netfilter

	cfg := ipn.ConfigVAlpha{
		Version:  "alpha0",
		Enabled:  opt.NewBool(true),  // wantRunning: bring the node up from the config
		Locked:   opt.NewBool(false), // operator owns the file; keep `tailscale set` usable
		AuthKey:  &authKeyRef,
		Hostname: &hostname,

		AcceptDNS:     opt.NewBool(true),             // FORCED ON — MagicDNS + split-DNS
		AcceptRoutes:  opt.NewBool(acceptRoutes(eg)), // subnet-router + app-connector routes
		NetfilterMode: &netfilter,
	}

	if en := eg.Spec.ExitNode; en != nil && exitNodeID != "" {
		cfg.ExitNode = &exitNodeID
		cfg.AllowLANWhileUsingExitNode = opt.NewBool(en.AllowLANAccess)
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("render tailscaled config: %w", err)
	}
	return b, nil
}

// acceptRoutes defaults to true (a client egress gateway normally wants whatever the
// tailnet makes reachable); an explicit spec value wins.
func acceptRoutes(eg *egressv1.EgressGroup) bool {
	return eg.Spec.AcceptRoutesEnabled()
}
