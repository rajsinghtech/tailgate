//go:build linux

package cni

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/rajsinghtech/tailgate/internal/netinfo"
	"github.com/rajsinghtech/tailgate/internal/wiring"
)

var cniLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

func cniDebug(msg string, args ...any) {
	f, err := os.OpenFile("/run/tailgate/cni/tailgate-cni.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	attrs := make([]any, 0, 2+len(args))
	attrs = append(attrs, "ts", time.Now().UTC().Format(time.RFC3339Nano))
	attrs = append(attrs, args...)
	slog.New(slog.NewTextHandler(f, nil)).Info(msg, attrs...)
}

// SetupMemberIfMember checks whether the pod being added (identified by CNI args)
// is an EgressGroup member and, if so, creates ts0 inside the pod netns right now
// — before the sandbox boots. This is what makes tailgate work with gVisor, which
// seeds its netstack from the netns only at sandbox start and never hot-plugs later.
//
// Best-effort with a 500ms total budget: if the kube API is busy, creds are
// missing, or the pod doesn't match, it returns silently. The agent wires async.
func SetupMemberIfMember(args string, info netinfo.PodNetInfo) {
	cniDebug("setup with prevResult", "podIP", info.PodIP, "netns", info.Netns, "args", args)
	creds, err := LoadKubeCreds(CNIDir)
	if err != nil {
		cniDebug("load creds failed", "err", err)
		return
	}
	kc, err := NewKubeClient(creds)
	if err != nil {
		cniLog.Warn("cni kube client", "err", err)
		cniDebug("new kube client failed", "err", err)
		return
	}
	cniArgs := ParseCNIArgs(args)
	group, err := CheckMembership(context.Background(), kc, cniArgs)
	if err != nil {
		cniDebug("membership failed", "err", err)
		return // timeout or API error — agent will wire async
	}
	if group == "" {
		cniDebug("not a member", "pod", cniArgs["K8S_POD_NAME"])
		return
	}
	routes := wiring.RouteSet()
	if err := wiring.SetupMember(info.PodIP, info.Netns, routes); err != nil {
		cniLog.Error("cni setup member", "pod", cniArgs["K8S_POD_NAME"], "group", group, "err", err)
		cniDebug("setup failed", "pod", cniArgs["K8S_POD_NAME"], "group", group, "err", err)
		return
	}
	cniDebug("setup ok", "pod", cniArgs["K8S_POD_NAME"], "group", group)
	cniLog.Info("cni pre-wired ts0 for member", "pod", cniArgs["K8S_POD_NAME"], "group", group)
}

// SetupMemberFromArgs handles Multus/NAD style invocation where tailgate-cni is
// called without prevResult. In that mode we don't know the pod IP at ADD time,
// so use K8S_POD_UID as the stable key for veth naming + member link address,
// then write a prewire record for the agent to move the correct gw peer later.
func SetupMemberFromArgs(args, netnsPath string) {
	cniDebug("setup without prevResult", "netns", netnsPath, "args", args)
	creds, err := LoadKubeCreds(CNIDir)
	if err != nil {
		cniDebug("load creds failed", "err", err)
		return
	}
	kc, err := NewKubeClient(creds)
	if err != nil {
		cniDebug("new kube client failed", "err", err)
		return
	}
	cniArgs := ParseCNIArgs(args)
	podUID := cniArgs["K8S_POD_UID"]
	if podUID == "" || netnsPath == "" {
		cniDebug("missing uid or netns", "uid", podUID, "netns", netnsPath)
		return
	}
	group, err := CheckMembership(context.Background(), kc, cniArgs)
	if err != nil || group == "" {
		cniDebug("membership empty", "pod", cniArgs["K8S_POD_NAME"], "group", group, "err", err)
		return
	}
	_, gwName := wiring.HostVethNames(podUID)
	if err := wiring.SetupMember(podUID, netnsPath, wiring.RouteSet()); err != nil {
		cniLog.Error("cni setup member (no prevResult)", "pod", cniArgs["K8S_POD_NAME"], "group", group, "err", err)
		cniDebug("setup without prevResult failed", "pod", cniArgs["K8S_POD_NAME"], "group", group, "err", err)
		return
	}
	_ = netinfo.WritePrewire(netinfo.PrewireInfo{PodUID: podUID, GwName: gwName})
	cniDebug("setup without prevResult ok", "pod", cniArgs["K8S_POD_NAME"], "group", group, "uid", podUID, "gwName", gwName)
}

// deleteHostPeer removes the host-side gateway peer veth for a pod IP. Called
// from CmdDel to clean up a peer that was left on the host by CNI ADD before
// the agent ever moved it into the gateway netns.
func deleteHostPeer(podIP string) {
	wiring.DeleteHostPeer(podIP)
}

func deleteHostLink(name string) {
	wiring.DeleteHostLink(name)
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
