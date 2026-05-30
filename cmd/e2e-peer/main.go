// e2e-peer is a throwaway tsnet node that joins the (ephemeral) tailnet and serves
// HTTP "ok" on :80. It prints "PEER_IP=<cgnat>" once up. Used as the egress target in
// the datapath e2e: a member pod must reach this peer's CGNAT IP through the gateway.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"tailscale.com/tsnet"
)

func main() {
	key := os.Getenv("TS_AUTHKEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "TS_AUTHKEY required")
		os.Exit(1)
	}
	s := &tsnet.Server{
		Hostname:  "tailgate-e2e-target",
		Ephemeral: true,
		AuthKey:   key,
		Dir:       mustTempDir(),
	}
	defer s.Close()
	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	// Wait for an IP, then announce it.
	go func() {
		lc, _ := s.LocalClient()
		for {
			st, err := lc.Status(context.Background())
			if err == nil && st.Self != nil && len(st.Self.TailscaleIPs) > 0 {
				for _, ip := range st.Self.TailscaleIPs {
					if ip.Is4() {
						fmt.Printf("PEER_IP=%s\n", ip.String())
						return
					}
				}
			}
			time.Sleep(time.Second)
		}
	}()
	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
}

func mustTempDir() string {
	d, err := os.MkdirTemp("", "e2e-peer-*")
	if err != nil {
		panic(err)
	}
	return d
}
