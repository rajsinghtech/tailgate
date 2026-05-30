// tailgate-gateway is the per-group gateway entrypoint. It runs tailscaled in
// kernel-TUN mode inside the gateway pod's netns, driven entirely by a declarative
// config file (tailscaled --config) that the operator renders from the EgressGroup
// spec. It enables IP forwarding, MASQUERADEs forwarded cluster traffic onto
// tailscale0 (SNAT-to-tag), and watches the config file: when the operator changes
// the spec (exit node, accept-routes, DNS), the ConfigMap updates and we call
// LocalAPI ReloadConfig — tailscaled re-applies prefs with NO restart, so the node
// identity and tunnels stay up. The agent stitches member veths into this netns.
// `tailgate-gateway ready` is the readiness probe.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"time"

	"tailscale.com/client/local"
)

const (
	sock       = "/var/run/tailscale/tailscaled.sock"
	configPath = "/etc/tailgate/tailscaled.json"
	gwBridge   = "tgbr0" // the agent enslaves member veths here (must match internal/agent)
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
	sh("sysctl", "-w", "net.ipv4.ip_forward=1")
	sh("sysctl", "-w", "net.ipv6.conf.all.forwarding=1")
	// Disable reverse-path filtering: member egress is policy-routed into the TUN via the
	// fwmark table, so replies (esp. exit-node internet egress) arrive on tailscale0 while the
	// main-table route to that source is via eth0 — strict rp_filter would drop them as
	// martians. Effective rp_filter = max(all, per-iface), so zero `all` + `default` (template
	// for tailscale0, created later by tailscaled) + eth0. NetfilterMode=off means tailscaled
	// doesn't install its own connmark/rp_filter-compat rules, so we own this.
	sh("sysctl", "-w", "net.ipv4.conf.all.rp_filter=0")
	sh("sysctl", "-w", "net.ipv4.conf.default.rp_filter=0")
	sh("sysctl", "-w", "net.ipv4.conf.eth0.rp_filter=0")
	// Permissive FORWARD so pod<->tailscale0 forwarding works regardless of veth name.
	sh("iptables", "-P", "FORWARD", "ACCEPT")

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

	// The gateway's OWN traffic uses the MAIN table: eth0 for cluster/control + DERP, and
	// these routes to reach tailnet peers directly (incl. the exit-node peer's CGNAT IP and
	// 100.100.100.100 MagicDNS): v4 CGNAT + v6 ULA (peers + 4via6).
	sh("ip", "route", "replace", "100.64.0.0/10", "dev", "tailscale0")
	sh("ip", "-6", "route", "replace", "fd7a:115c:a1e0::/48", "dev", "tailscale0")

	// FORWARDED member traffic (ingress via the agent's bridge) is marked and policy-routed
	// into the TUN, so tailscaled routes EACH destination per its netmap: CGNAT peers,
	// accepted subnet-router / app-connector CIDRs, and the exit node for 0.0.0.0/0. This is
	// required because tailscaled installs accepted/exit routes in its policy table 52, which
	// forwarded+MASQUERADEd traffic never hits — the main table alone would leak member
	// traffic out eth0. Marking by ingress bridge keeps the gateway's own egress on main.
	const fwMark, fwTable = "0x7717", "7717"
	sh("iptables", "-t", "mangle", "-A", "PREROUTING", "-i", gwBridge, "-j", "MARK", "--set-mark", fwMark)
	sh("ip", "rule", "add", "fwmark", fwMark, "lookup", fwTable, "priority", "1000")
	sh("ip", "route", "replace", "default", "dev", "tailscale0", "table", fwTable)
	sh("ip", "-6", "rule", "add", "fwmark", fwMark, "lookup", fwTable, "priority", "1000")
	sh("ip", "-6", "route", "replace", "default", "dev", "tailscale0", "table", fwTable)

	// SNAT forwarded traffic onto the tailnet (source = this gateway's tailnet IP).
	sh("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", "tailscale0", "-j", "MASQUERADE")
	sh("ip6tables", "-t", "nat", "-A", "POSTROUTING", "-o", "tailscale0", "-j", "MASQUERADE")

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

func sh(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
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
