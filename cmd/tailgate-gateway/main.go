//go:build linux

// tailgate-gateway is the per-group gateway entrypoint. It runs tailscaled in
// kernel-TUN mode inside the gateway pod's netns, driven entirely by a declarative
// config file (tailscaled --config) that the operator renders from the EgressGroup
// spec. It enables IP forwarding, MASQUERADEs forwarded cluster traffic onto
// tailscale0 (SNAT-to-tag), and watches the config file: when the operator changes
// the spec (exit node, accept-routes, DNS), the ConfigMap updates and we call
// LocalAPI ReloadConfig — tailscaled re-applies prefs with NO restart, so the node
// identity and tunnels stay up. The agent stitches member veths into this netns.
// The datapath (nftables NAT/mark + policy routing) is programmed natively via
// internal/netfilter — no iptables/ip shell-outs — so it works on nf_tables-only hosts
// (Talos+Cilium). `tailgate-gateway ready` is the readiness probe.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"time"

	"tailscale.com/client/local"

	"github.com/rajsinghtech/tailgate/internal/netfilter"
)

const (
	sock       = "/var/run/tailscale/tailscaled.sock"
	configPath = "/etc/tailgate/tailscaled.json"
	gwBridge   = "tgbr0" // the agent enslaves member veths here (must match internal/agent)

	fwMark  uint32 = 0x7717 // member traffic mark (must match the policy-routing rule)
	fwTable int    = 7717   // policy routing table the mark steers into
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ready":
			os.Exit(readyMain())
		case "live":
			os.Exit(liveMain())
		}
	}
	run()
}

// readyMain exits 0 once tailscaled has a CGNAT IP (node is up on the tailnet).
func readyMain() int {
	out, err := exec.Command("tailscale", "--socket="+sock, "ip", "-4").Output()
	if err != nil || len(out) < 4 || string(out[:4]) != "100." {
		return 1
	}
	return 0
}

// liveMain exits 0 while the tailscaled daemon is responsive (LocalAPI answers). It does
// NOT gate on tailnet connectivity — a transient DERP/control blip must not restart the
// pod (that would flap live member egress); only a wedged or dead daemon fails this.
func liveMain() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (&local.Client{Socket: sock}).Status(ctx); err != nil {
		return 1
	}
	return 0
}

func run() {
	must("ensure tun", ensureTun())
	// Enable forwarding + relax rp_filter (member egress is policy-routed asymmetrically).
	// NetfilterMode=off in the config keeps tailscaled out of netfilter so we own the datapath.
	must("forwarding/rp_filter", netfilter.EnableForwardingAndRelaxRPFilter())

	// tailscaled in the background, driven by the declarative --config file. Enabled=true
	// in the config brings the node up; the authkey (file:) carries the tags. Persistent
	// state keeps the node identity stable across pod restarts (the operator owns device
	// deletion via the API on teardown). NetfilterMode=off in the config keeps tailscaled
	// out of netfilter so our MASQUERADE/forwarding rules stand.
	go func() {
		_ = os.MkdirAll("/var/lib/tailscale", 0o700)
		c := exec.Command("tailscaled",
			"--state=/var/lib/tailscale/tailscaled.state",
			"--socket="+sock,
			"--tun=tailscale0",
			"--config="+configPath,
		)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "tailscaled exited:", err)
		}
	}()

	// Wait for the socket, then for the node to come up from the config.
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	for i := 0; i < 60; i++ {
		if readyMain() == 0 {
			break
		}
		time.Sleep(time.Second)
	}

	// Program the forward datapath natively (after the TUN exists). nftables: mark member
	// ingress on tgbr0 + MASQUERADE egress on tailscale0. netlink: fwmark -> table 7717
	// (default via the TUN, so tailscaled routes each destination per its netmap — CGNAT
	// peers, accepted subnet/app-connector CIDRs, exit node for 0.0.0.0/0), plus the
	// gateway's own CGNAT/ULA routes. Marking by ingress bridge keeps the gateway's own
	// egress on the main table. Fails LOUD (must) rather than the old silent iptables no-op.
	dp, err := netfilter.New()
	must("netfilter", err)
	must("masquerade + mark", dp.SetupMASQUERADE(gwBridge, fwMark, "tailscale0"))
	must("policy routing", netfilter.SetupPolicyRouting(fwMark, fwTable, "tailscale0"))

	fmt.Println("tailgate-gateway up:", getenv("TS_GROUP", "?"))

	// Watch the config file and hot-reload tailscaled prefs on change (exit node,
	// accept-routes, DNS) without a restart.
	watchConfig(context.Background(), &local.Client{Socket: sock}, configPath)
}

// watchConfig polls the config file and triggers a LocalAPI reload when its content
// changes. A content hash (not mtime) avoids spurious reloads; projected ConfigMap
// volumes swap a symlink on update, which os.ReadFile follows transparently. Kubelet
// propagation is the real latency floor (~minute); a short poll just debounces.
func watchConfig(ctx context.Context, lc *local.Client, path string) {
	last := hashFile(path)
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h := hashFile(path)
		if h == last || h == ([32]byte{}) {
			continue
		}
		// Commit the hash only on a successful reload; a failed reload leaves `last`
		// unchanged so the next poll retries instead of silently dropping the new config.
		if ok, err := lc.ReloadConfig(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "reload-config (will retry):", err)
		} else {
			last = h
			fmt.Println("tailgate-gateway: config changed, reloaded (applied:", ok, ")")
		}
	}
}

func hashFile(path string) [32]byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(b)
}

// ensureTun creates /dev/net/tun if absent (mknod c 10 200).
func ensureTun() error {
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		return nil
	}
	_ = os.MkdirAll("/dev/net", 0o755)
	return exec.Command("mknod", "/dev/net/tun", "c", "10", "200").Run()
}

func must(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
		os.Exit(1)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
