//go:build linux

package main

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
)

var (
	cgnatV4 = netip.MustParsePrefix("100.64.0.0/10")
	ulaV6   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// publishRoutes polls the gateway's netmap and writes the routes it can reach beyond the
// always-steered CGNAT + ULA — i.e. the accepted subnet-router + app-connector CIDRs — to
// <dir>/<group>.routes (one CIDR per line, sorted). The node agent reads the same hostPath
// and steers them onto member pods of MirrorRoutes-enabled groups, so egress follows DNS
// resolution. Atomic write; the file changes only when the route set does.
func publishRoutes(ctx context.Context, lc *local.Client, group, dir string) {
	path := filepath.Join(dir, group+".routes")
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var last string
	for {
		if st, err := lc.Status(ctx); err == nil {
			content := strings.Join(reachableRoutes(st), "\n")
			if content != last {
				if writeAtomic(path, content) == nil {
					last = content
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reachableRoutes returns the non-CGNAT/ULA, non-default prefixes advertised by tailnet peers
// (subnet-router + app-connector CIDRs) — exactly the routes a member lacks today.
func reachableRoutes(st *ipnstate.Status) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range st.Peer {
		ips := p.AllowedIPs
		if ips == nil {
			continue
		}
		for i := 0; i < ips.Len(); i++ {
			pfx := ips.At(i)
			if pfx.Bits() == 0 {
				continue // default route — exit-node territory, not mirrored
			}
			a := pfx.Addr()
			if cgnatV4.Contains(a) || ulaV6.Contains(a) {
				continue // peer's own CGNAT/ULA host routes — already steered
			}
			s := pfx.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func writeAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
