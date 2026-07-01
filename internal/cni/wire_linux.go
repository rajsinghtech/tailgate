//go:build linux

package cni

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rajsinghtech/tailgate/internal/netinfo"
	"github.com/rajsinghtech/tailgate/internal/wiring"
)

var cniLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

// SetupMemberIfMember checks whether the pod being added (identified by CNI args)
// is an EgressGroup member and, if so, creates ts0 inside the pod netns right now
// — before the sandbox boots. This is what makes tailgate work with gVisor, which
// seeds its netstack from the netns only at sandbox start and never hot-plugs later.
//
// Best-effort with a 500ms total budget: if the kube API is busy, creds are
// missing, or the pod doesn't match, it returns silently. The agent wires async.
func SetupMemberIfMember(args string, info netinfo.PodNetInfo) {
	creds, err := LoadKubeCreds(CNIDir)
	if err != nil {
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
		return // timeout or API error — agent will wire async
	}
	if group == "" {
		return
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

// WriteMemberCache writes the group name for a pod IP so the CNI plugin can
// check membership with a single file read. Called by the agent.
func WriteMemberCache(podIP, group string) error {
	dir := filepath.Join(CNIDir, "members")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write([]byte(group)); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), filepath.Join(dir, podIP))
}

// RemoveMemberCache deletes the member cache file for a pod IP.
func RemoveMemberCache(podIP string) {
	_ = os.Remove(filepath.Join(CNIDir, "members", podIP))
}

// ReadMemberCache returns the group name for podIP, or "" if not cached.
// This is a single file read — no network, no API.
func ReadMemberCache(podIP string) string {
	b, err := os.ReadFile(filepath.Join(CNIDir, "members", podIP))
	if err != nil {
		return ""
	}
	return string(b)
}
