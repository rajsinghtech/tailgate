//go:build linux

package agent

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/netinfo"
)

const groupLabel = "tailgate.dev/group"

// Agent is the node-local route-wiring loop.
type Agent struct {
	C         client.Client
	Node      string // this node's name (NODE_NAME)
	GatewayNS string // namespace where gateway DaemonSets run
	Log       *slog.Logger
	// ClusterCIDRs are the in-cluster ranges (pod/service/node) kept on the primary CNI
	// for exit-node members so cluster DNS/API/pod traffic never blackholes through the
	// full tunnel. Empty unless the group uses an exit node.
	ClusterCIDRs []string
}

type wired struct {
	info netinfo.PodNetInfo
	gwNs string
}

// Run polls every interval until ctx is done, wiring member pods and unwiring gone ones.
func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	done := map[string]wired{} // podIP -> wiring
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
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
	gwCache := map[string]string{} // group -> current gateway netns (per sync)
	present := map[string]bool{}

	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != a.Node || p.Namespace == a.GatewayNS {
			continue
		}
		if p.Status.PodIP == "" || p.Status.Phase != corev1.PodRunning {
			continue
		}
		grp := matchGroup(p, a.nsLabels(ctx, p.Namespace, nsCache), groups.Items)
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
		// Already wired to the CURRENT gateway? nothing to do. (If the gateway pod
		// restarted, its netns changed -> re-wire below.)
		if w, ok := done[ip]; ok && w.gwNs == gwNs {
			continue
		}
		memberNs, err := netnsForPodUID(string(p.UID))
		if err != nil {
			a.Log.Warn("member netns not found yet", "pod", p.Name, "err", err)
			continue
		}
		info := netinfo.PodNetInfo{PodIP: ip, Netns: memberNs, IfName: "eth0"}
		g := findGroup(groups.Items, grp)
		var exit *ExitOpts
		if g != nil && g.Spec.ExitNode != nil {
			exit = &ExitOpts{ClusterCIDRs: a.ClusterCIDRs}
		}
		if err := Wire(info, gwNs, routeSet(g), exit); err != nil {
			a.Log.Error("wire", "pod", p.Name, "err", err)
			continue
		}
		a.Log.Info("wired member to gateway", "pod", p.Name, "group", grp, "ip", ip, "gwNs", gwNs)
		done[ip] = wired{info: info, gwNs: gwNs}
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

// gwNsCached memoizes gatewayNetns per group for one sync pass.
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

// gatewayNetns finds the node-local gateway pod for group and returns its netns.
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
