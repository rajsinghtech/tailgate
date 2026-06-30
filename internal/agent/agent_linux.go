//go:build linux

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/netinfo"
	"github.com/rajsinghtech/tailgate/internal/wiring"
)

const mirrorDir = "/run/tailgate" // hostPath the gateway publishes <group>.routes into

// cniCredsDir is where the agent writes API credentials for the CNI plugin.
const cniCredsDir = "/run/tailgate/cni"

// writeCNICreds writes the server URL, SA token, and CA cert into the CNI
// credentials directory so the CNI plugin can query the kube API at ADD time.
// The agent has a mounted SA token (for its own informers); we write a copy for
// the CNI binary (which runs in the pod's container context, not the agent's).
// Called on every sync pass to handle bound-token rotation.
func writeCNICreds(serverURL, saTokenDir string) error {
	if serverURL == "" || saTokenDir == "" {
		return nil
	}
	if err := os.MkdirAll(cniCredsDir, 0o755); err != nil {
		return err
	}
	// Token
	token, err := os.ReadFile(filepath.Join(saTokenDir, "token"))
	if err != nil {
		return fmt.Errorf("read sa token: %w", err)
	}
	// CA cert
	ca, err := os.ReadFile(filepath.Join(saTokenDir, "ca.crt"))
	if err != nil {
		return fmt.Errorf("read sa ca.crt: %w", err)
	}
	for name, data := range map[string][]byte{
		"server": []byte(serverURL),
		"token":  token,
		"ca.crt": ca,
	} {
		tmp, err := os.CreateTemp(cniCredsDir, ".tmp-*")
		if err != nil {
			return err
		}
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		_ = os.Chmod(tmp.Name(), 0o644)
		if err := os.Rename(tmp.Name(), filepath.Join(cniCredsDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// mirroredRoutes reads the gateway's published reachable routes for a route-mirroring group
// (subnet-router + app-connector CIDRs), dropping any that overlap cluster ranges so cluster
// traffic is never steered into the gateway. Returns nil (no mirroring) otherwise.
func (a *Agent) mirroredRoutes(group string, g *egressv1.EgressGroup) []string {
	if g == nil || !g.Spec.MirrorRoutesEnabled() {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(mirrorDir, group+".routes"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line = strings.TrimSpace(line); line == "" || a.overlapsCluster(line) {
			continue
		}
		out = append(out, line)
	}
	return out
}

func (a *Agent) overlapsCluster(cidr string) bool {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return true
	}
	for _, c := range a.ClusterCIDRs {
		if cp, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil && cp.Overlaps(p) {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func subtractStrings(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

const groupLabel = "tailgate.dev/group"

// Agent is the node-local route-wiring loop.
type Agent struct {
	C            client.Client
	Node         string
	GatewayNS    string
	Log          *slog.Logger
	ClusterCIDRs []string
	// ServerURL is the kube API server URL (written to /run/tailgate/cni/server
	// for the CNI plugin to query membership at ADD time).
	ServerURL string
	// SATokenDir is the projected service-account token mount path (default
	// /var/run/secrets/kubernetes.io/serviceaccount).
	SATokenDir string
}

type wired struct {
	info     netinfo.PodNetInfo
	gwNs     string
	mirrored []string // routes mirrored from the gateway's netmap (sorted; for diff + withdrawal)
}

// Run polls every interval until ctx is done, wiring member pods and unwiring gone ones.
func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	done := map[string]wired{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		// Write CNI creds on every pass so the CNI plugin always has a fresh token.
		if err := writeCNICreds(a.ServerURL, a.SATokenDir); err != nil {
			a.Log.Warn("write cni creds", "err", err)
		}
		if err := a.sync(ctx, done); err != nil {
			a.Log.Error("sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) sync(ctx context.Context, done map[string]wired) error {
	var groups egressv1.EgressGroupList
	if err := a.C.List(ctx, &groups); err != nil {
		return err
	}
	if len(groups.Items) == 0 {
		return nil
	}
	var pods corev1.PodList
	if err := a.C.List(ctx, &pods); err != nil {
		return err
	}
	nsCache := map[string]map[string]string{}
	gwCache := map[string]string{}
	present := map[string]bool{}

	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != a.Node || p.Namespace == a.GatewayNS {
			continue
		}
		if p.Status.PodIP == "" || p.Status.Phase != corev1.PodRunning {
			continue
		}
		grp := wiring.MatchGroup(p, a.nsLabels(ctx, p.Namespace, nsCache), groups.Items)
		if grp == "" {
			continue
		}
		ip := p.Status.PodIP
		present[ip] = true
		gwNs, err := a.gwNsCached(ctx, grp, gwCache)
		if err != nil {
			a.Log.Warn("gateway netns not ready", "group", grp, "err", err)
			continue
		}
		g := findGroup(groups.Items, grp)
		mir := a.mirroredRoutes(grp, g)
		if w, ok := done[ip]; ok && w.gwNs == gwNs && equalStrings(w.mirrored, mir) {
			continue
		}
		memberNs, err := netnsForPodUID(string(p.UID))
		if err != nil {
			a.Log.Warn("member netns not found yet", "pod", p.Name, "err", err)
			continue
		}
		info := netinfo.PodNetInfo{PodIP: ip, Netns: memberNs, IfName: "eth0"}
		var exit *ExitOpts
		if g != nil && g.Spec.ExitNode != nil {
			exit = &ExitOpts{ClusterCIDRs: a.ClusterCIDRs}
		}
		var stale []string
		if w, ok := done[ip]; ok {
			stale = subtractStrings(w.mirrored, mir)
		}
		if err := Wire(info, gwNs, append(routeSet(), mir...), stale, exit); err != nil {
			a.Log.Error("wire", "pod", p.Name, "err", err)
			continue
		}
		a.Log.Info("wired member to gateway", "pod", p.Name, "group", grp, "ip", ip, "gwNs", gwNs, "mirrored", len(mir))
		done[ip] = wired{info: info, gwNs: gwNs, mirrored: mir}
	}

	for ip, w := range done {
		if present[ip] {
			continue
		}
		if err := Unwire(w.info, w.gwNs); err != nil {
			a.Log.Warn("unwire", "ip", ip, "err", err)
		}
		delete(done, ip)
	}
	return nil
}

func (a *Agent) gwNsCached(ctx context.Context, group string, cache map[string]string) (string, error) {
	if ns, ok := cache[group]; ok {
		if ns == "" {
			return "", errNoGateway
		}
		return ns, nil
	}
	ns, err := a.gatewayNetns(ctx, group)
	if err != nil {
		cache[group] = ""
		return "", err
	}
	cache[group] = ns
	return ns, nil
}

func (a *Agent) gatewayNetns(ctx context.Context, group string) (string, error) {
	var pods corev1.PodList
	if err := a.C.List(ctx, &pods, client.InNamespace(a.GatewayNS), client.MatchingLabels{groupLabel: group}); err != nil {
		return "", err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == a.Node && p.Status.Phase == corev1.PodRunning {
			return netnsForPodUID(string(p.UID))
		}
	}
	return "", errNoGateway
}

func (a *Agent) nsLabels(ctx context.Context, ns string, cache map[string]map[string]string) map[string]string {
	if l, ok := cache[ns]; ok {
		return l
	}
	var nsObj corev1.Namespace
	if err := a.C.Get(ctx, client.ObjectKey{Name: ns}, &nsObj); err != nil {
		cache[ns] = map[string]string{}
	} else {
		cache[ns] = nsObj.Labels
	}
	return cache[ns]
}

func findGroup(groups []egressv1.EgressGroup, name string) *egressv1.EgressGroup {
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i]
		}
	}
	return nil
}

var errNoGateway = errString("no running node-local gateway pod")

type errString string

func (e errString) Error() string { return string(e) }
