//go:build linux

package cni

import (
	"context"
	"log/slog"

	"github.com/rajsinghtech/tailgate/internal/netinfo"
	"github.com/rajsinghtech/tailgate/internal/wiring"
)

var cniLog = slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelWarn}))

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// SetupMemberIfMember checks whether the pod being added (identified by CNI args)
// is an EgressGroup member and, if so, creates ts0 inside the pod netns right now
// — before the sandbox boots. This is what makes tailgate work with gVisor, which
// seeds its netstack from the netns only at sandbox start and never hot-plugs later.
//
// Best-effort: if the kube API is unreachable, creds are missing, or the pod
// doesn't match any group, this silently returns. The pod starts without ts0 and
// the agent will wire it async (correct for non-gVisor runtimes; gVisor pods will
// need a restart once the agent has processed them).
//
// podIP and netns come from prevResult + args.Netns in CmdAdd.
func SetupMemberIfMember(args string, info netinfo.PodNetInfo) {
	creds, err := LoadKubeCreds(CNIDir)
	if err != nil {
		// Agent hasn't written creds yet — degraded mode, agent will wire async.
		return
	}
	kc, err := NewKubeClient(creds)
	if err != nil {
		cniLog.Warn("cni kube client", "err", err)
		return
	}
	cniArgs := ParseCNIArgs(args)
	group, err := CheckMembership(context.Background(), kc, cniArgs)
	if err != nil {
		cniLog.Warn("cni membership check", "pod", cniArgs["K8S_POD_NAME"], "err", err)
		return
	}
	if group == "" {
		return // not a member — nothing to do
	}
	routes := wiring.RouteSet()
	if err := wiring.SetupMember(info.PodIP, info.Netns, routes); err != nil {
		cniLog.Error("cni setup member", "pod", cniArgs["K8S_POD_NAME"], "group", group, "err", err)
		return
	}
	cniLog.Info("cni pre-wired ts0 for member", "pod", cniArgs["K8S_POD_NAME"], "group", group)
}

// deleteHostPeer removes the host-side gateway peer veth for a pod IP. Called
// from CmdDel to clean up a peer that was left on the host by CNI ADD before
// the agent ever moved it into the gateway netns.
func deleteHostPeer(podIP string) {
	wiring.DeleteHostPeer(podIP)
}
