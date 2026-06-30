// Package cni implements tailgate's chained, route-only CNI plugin. It does NOT set
// up the datapath itself — it records each pod's netns (keyed by PodIP) for the agent,
// then passes the previous plugin's result through unchanged. Selection + wiring is the
// agent's job (gateway-side, informer-driven), so this plugin runs harmlessly on every
// pod.
package cni

import (
	"encoding/json"
	"fmt"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	"github.com/rajsinghtech/tailgate/internal/netinfo"
)

type netConf struct {
	cnitypes.NetConf
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
func CmdAdd(args *skel.CmdArgs) error {
	c, prev, err := parse(args.StdinData)
	if err != nil {
		return err
	}
	if prev == nil {
		return fmt.Errorf("tailgate-cni must be chained (no prevResult)")
	}
	if ip, err := extractIPv4(string(prev)); err == nil {
		info := netinfo.PodNetInfo{PodIP: ip, Netns: args.Netns, IfName: args.IfName}
		_ = netinfo.Write(info)
		// Best-effort: if the pod is a member, create ts0 before the sandbox boots.
		// Never errors out — CNI ADD must not fail because of our wiring.
		SetupMemberIfMember(args.Args, info)
	}
	return cnitypes.PrintResult(c.PrevResult, c.CNIVersion)
}

// CmdDel removes the pod's net-info record and cleans up any host-side veth
// peer left behind (if the agent hadn't moved it into the gateway yet).
func CmdDel(args *skel.CmdArgs) error {
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
