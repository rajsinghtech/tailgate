// Package cni implements tailgate's chained, route-only CNI plugin. It does NOT set
// up the datapath itself — it records each pod's netns (keyed by PodIP) for the agent,
// then passes the previous plugin's result through unchanged. Selection + wiring is the
// agent's job (gateway-side, informer-driven), so this plugin runs harmlessly on every
// pod.
package cni

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	"github.com/rajsinghtech/tailgate/internal/netinfo"
)

type netConf struct {
	cnitypes.NetConf
}

func printEmptyResult(stdin []byte) error {
	var v struct {
		CNIVersion string `json:"cniVersion"`
	}
	_ = json.Unmarshal(stdin, &v)
	if v.CNIVersion == "" {
		v.CNIVersion = "0.3.1"
	}
	return cnitypes.PrintResult(&current.Result{CNIVersion: v.CNIVersion}, v.CNIVersion)
}

func extractIPv4(prevResultJSON string) (string, error) {
	var r current.Result
	if err := json.Unmarshal([]byte(prevResultJSON), &r); err != nil {
		return "", err
	}
	for _, ip := range r.IPs {
		if v4 := ip.Address.IP.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 in prevResult")
}

func parse(stdin []byte) (*netConf, []byte, error) {
	var c netConf
	if err := json.Unmarshal(stdin, &c); err != nil {
		return nil, nil, err
	}
	if c.PrevResult == nil {
		return &c, nil, nil
	}
	pr, err := json.Marshal(c.PrevResult)
	return &c, pr, err
}

// CmdAdd records the pod netns keyed by its IPv4. If the pod is an EgressGroup
// member, it also creates ts0 inside the pod netns NOW — before the sandbox
// boots — so gVisor's netstack (which scrapes the netns only at sandbox start)
// picks up the interface. Otherwise the agent wires async after the pod is
// Running (works for non-gVisor runtimes). Then re-emits prevResult.
//
// This function MUST NOT fail — a CNI plugin crash leaves the pod stuck in
// ContainerCreating indefinitely. All best-effort work is recovered.
func CmdAdd(args *skel.CmdArgs) (outErr error) {
	defer func() {
		if r := recover(); r != nil {
			outErr = nil // swallow panics — CNI ADD must never fail
		}
	}()
	c, prev, err := parse(args.StdinData)
	if err != nil {
		// Can't parse — but we must not block pod creation or emit empty stdout
		// (Multus treats empty stdout as a CNI failure). Return a valid empty result.
		return printEmptyResult(args.StdinData)
	}
	if prev == nil {
		// Some runtimes/delegates (notably Multus wrapping a conflist) may invoke
		// tailgate-cni without prevResult. We can't derive the pod IP, so skip
		// pre-wiring, but still emit a valid result so CNI ADD succeeds.
		return printEmptyResult(args.StdinData)
	}
	if ip, err := extractIPv4(string(prev)); err == nil {
		info := netinfo.PodNetInfo{PodIP: ip, Netns: args.Netns, IfName: args.IfName}
		_ = netinfo.Write(info)
		// Best-effort: if the pod is a member, create ts0 before the sandbox boots.
		// Run with a hard 5s deadline in a goroutine — if it blocks (e.g. netns
		// Set hangs in a nested container), we don't hold up pod creation. The
		// agent will wire it async as a fallback.
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			done := make(chan struct{})
			go func() {
				defer close(done)
				SetupMemberIfMember(args.Args, info)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}()
		wg.Wait()
	}
	if c.PrevResult == nil {
		return printEmptyResult(args.StdinData)
	}
	return cnitypes.PrintResult(c.PrevResult, c.CNIVersion)
}

// CmdDel removes the pod's net-info record and cleans up any host-side veth
// peer left behind (if the agent hadn't moved it into the gateway yet).
// Must not fail — a CNI DEL failure can leave stale state.
func CmdDel(args *skel.CmdArgs) (outErr error) {
	defer func() {
		if r := recover(); r != nil {
			outErr = nil
		}
	}()
	_, prev, err := parse(args.StdinData)
	if err != nil || prev == nil {
		return nil
	}
	if ip, err := extractIPv4(string(prev)); err == nil {
		_ = netinfo.Remove(ip)
		deleteHostPeer(ip)
	}
	return nil
}

// CmdCheck passes prevResult through.
func CmdCheck(args *skel.CmdArgs) error {
	c, _, err := parse(args.StdinData)
	if err != nil || c.PrevResult == nil {
		return err
	}
	return cnitypes.PrintResult(c.PrevResult, c.CNIVersion)
}
