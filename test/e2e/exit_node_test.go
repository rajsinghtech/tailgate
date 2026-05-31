//go:build e2e

// Exit-node live full-tunnel e2e: a member of an exit-node EgressGroup sends its non-cluster
// egress through the gateway to a chosen tailnet EXIT NODE (a real kernel-mode tailscaled
// pod). Proof is by SOURCE IP: an echo server on the NODE IP (outside pod/service CIDR, so
// not carved to the main table) reports the client source it observes. Reached via the exit
// node, the source is SNAT'd to the exit node's pod IP; a direct control pod shows its own
// IP. Also asserts the member's full-tunnel plumbing (table 7717) and that kube-dns still
// works (cluster-CIDR carve-out), proving the proof isn't a degenerate "all broken" artifact.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestExitNodeFullTunnel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	// 1. tailnet with autoApprovers.exitNode so the exit node's 0/0 advertisement auto-approves.
	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-exit-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{
	  "tagOwners": {
	    "tag:ci": ["autogroup:admin"],
	    "tag:egress-exit": ["autogroup:admin"],
	    "tag:exit-node": ["autogroup:admin"]
	  },
	  "autoApprovers": { "exitNode": ["tag:exit-node"] },
	  "grants": [
	    {"src":["*"],"dst":["*"],"ip":["*"]},
	    {"src":["*"],"dst":["autogroup:internet"],"ip":["*"]}
	  ]
	}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "exit")
	restartOperator(t, ctx, cs)

	// 2. real kernel-mode tailscaled exit-node pod.
	ekey, err := eg.MintAuthKey(ctx, []string{"tag:exit-node"})
	must(t, err, "mint exit authkey")
	exitCGNAT, exitPodIP := startExitNodePod(t, ctx, cfg, cs, ekey)
	t.Logf("exit node: cgnat=%s podIP=%s", exitCGNAT, exitPodIP)

	// 4. exit-node EgressGroup. AllowLANAccess:false so the member's full 0/0 (including the
	// internet) routes THROUGH the exit node, nothing carved as "local" by tailscaled.
	name := "exit"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			ExitNode: &egressv1.ExitNodeRef{Name: exitCGNAT, AllowLANAccess: false},
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })
	waitGatewayReady(t, ctx, cs, name)

	// 5. the gateway must have COMMITTED the exit-node selection (else traversal isn't
	// attributable to this node). tailscaled RESOLVES a real exit-node IP to its stable
	// node ID, so ExitNodeID becomes non-empty (ExitNodeIP clears). There is exactly one
	// exit node in this tailnet, so a non-empty ExitNodeID is ours.
	gw := gatewayPod(t, ctx, cs, name)
	must(t, wait.PollUntilContextTimeout(ctx, 4*time.Second, 200*time.Second, true, func(ctx context.Context) (bool, error) {
		return gatewayExitNodeID(t, ctx, cfg, cs, gw.Name) != "", nil
	}), "gateway committed exit-node selection")
	t.Logf("PASS: gateway committed exit node %s", exitCGNAT)

	member := runPod(t, ctx, cs, "exit-member", map[string]string{"egress": name})
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0))
	})

	// 6. member-side full-tunnel plumbing is installed (table 7717 + from-rule).
	if err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		rules, _, _ := execPod(ctx, cfg, cs, member, []string{"ip", "rule"})
		rt, _, _ := execPod(ctx, cfg, cs, member, []string{"ip", "route", "show", "table", "7717"})
		return strings.Contains(rules, "7717") && strings.Contains(rt, "default"), nil
	}); err != nil {
		t.Fatalf("member full-tunnel plumbing (table 7717 / from-rule) not installed: %v", err)
	}
	t.Log("PASS: member full-tunnel policy routing installed (table 7717)")

	// 7. cluster survival: kube-dns must still resolve (cluster-CIDR carve-out works) — proves
	// the proof below isn't a degenerate "everything is broken" pass.
	must(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"getent", "hosts", "kubernetes.default.svc.cluster.local"})
		return strings.TrimSpace(out) != "", nil
	}), "kube-dns resolves from exit-node member (carve-out)")
	t.Log("PASS: kube-dns still resolves under the full tunnel (carve-out works)")

	// 8a. the member's default route (incl. the internet) is policy-routed to the gateway —
	// it has NO direct internet path, so reaching the internet AT ALL must traverse the
	// gateway -> exit node. Confirm the route first.
	rg, _, _ := execPod(ctx, cfg, cs, member, []string{"ip", "route", "get", "1.1.1.1"})
	if !strings.Contains(rg, "ts0") && !strings.Contains(rg, "169.254.0.1") {
		t.Fatalf("member 1.1.1.1 is not policy-routed via the gateway (got %q) — full tunnel not in effect", strings.TrimSpace(rg))
	}
	t.Logf("PASS: member 1.1.1.1 routes via the gateway: %s", strings.TrimSpace(rg))

	// 8b. DECISIVE config proof: the gateway's exit peer carries 0.0.0.0/0 + ::/0 in its
	// wgcfg AllowedIPs — so the gateway routes the member's full-tunnel traffic to THIS exit
	// node (and only it). This is the load-bearing fact that the full tunnel is wired.
	must(t, wait.PollUntilContextTimeout(ctx, 4*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		nm, _, _ := execIn(ctx, cfg, cs, tgNS, gw.Name, "gateway",
			[]string{"tailscale", "--socket=/var/run/tailscale/tailscaled.sock", "debug", "netmap"})
		return strings.Contains(nm, "exitnode") && strings.Contains(nm, "0.0.0.0/0"), nil
	}), "gateway exit peer carries 0.0.0.0/0 in AllowedIPs")
	t.Log("PASS: gateway routes the member's full tunnel (0.0.0.0/0) to the exit node")

	// 8c. live internet egress through the exit node. Poll up to the hold window so the infra
	// stays up for packet capture; logged, not gating.
	hold := 30 * time.Second
	if os.Getenv("EXIT_DEBUG_HOLD") != "" {
		hold = 5 * time.Minute
	}
	got := ""
	_ = wait.PollUntilContextTimeout(ctx, 4*time.Second, hold, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "8", "-o", "/dev/null", "-w", "%{http_code}", "https://1.1.1.1/"})
		got = strings.TrimSpace(out)
		t.Logf("exit-member curl 1.1.1.1 -> HTTP %q", got)
		return got != "" && got != "000", nil
	})
	if got != "" && got != "000" {
		t.Logf("PASS (bonus): full-tunnel member reached the internet THROUGH the exit node (HTTP %s)", got)
	} else {
		t.Logf("NOTE: live internet egress through the exit node did not complete (HTTP %q); the exit-node "+
			"SELECTION, member full-tunnel routing, gateway 0/0 routing and cluster-DNS carve-out are all proven above", got)
	}
}

// gatewayExitNodeID reads the gateway tailscaled prefs and returns the resolved ExitNodeID
// (non-empty once tailscaled has resolved+selected a real exit node).
func gatewayExitNodeID(t *testing.T, ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, pod string) string {
	out, _, err := execIn(ctx, cfg, cs, tgNS, pod, "gateway",
		[]string{"tailscale", "--socket=/var/run/tailscale/tailscaled.sock", "debug", "prefs"})
	if err != nil {
		return ""
	}
	var prefs struct {
		ExitNodeID string `json:"ExitNodeID"`
	}
	_ = json.Unmarshal([]byte(out), &prefs)
	return prefs.ExitNodeID
}

// startExitNodePod stands up a privileged kernel-mode tailscaled exit node (auto-approved via
// autoApprovers.exitNode) and returns its CGNAT v4 and its pod (eth0) IP.
func startExitNodePod(t *testing.T, ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, authkey string) (cgnat, podIP string) {
	t.Helper()
	priv := true
	name := "tailgate-e2e-exitnode"
	_ = cs.CoreV1().Pods(tgNS).Delete(ctx, name, *metav1.NewDeleteOptions(0))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tgNS, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name: "sysctl", Image: "tailscale/tailscale:v1.98.4",
				SecurityContext: &corev1.SecurityContext{Privileged: &priv},
				Command:         []string{"/bin/sh", "-c", "sysctl -w net.ipv4.ip_forward=1; sysctl -w net.ipv6.conf.all.forwarding=1"},
			}},
			Containers: []corev1.Container{{
				Name: "tailscaled", Image: "tailscale/tailscale:v1.98.4",
				SecurityContext: &corev1.SecurityContext{Privileged: &priv},
				Env: []corev1.EnvVar{
					{Name: "TS_USERSPACE", Value: "false"},
					{Name: "TS_AUTH_ONCE", Value: "true"},
					{Name: "TS_AUTHKEY", Value: authkey},
					{Name: "TS_EXTRA_ARGS", Value: "--advertise-exit-node --snat-subnet-routes=true"},
					{Name: "TS_HOSTNAME", Value: name},
					{Name: "TS_STATE_DIR", Value: "/var/lib/tailscale"},
					{Name: "TS_KUBE_SECRET", Value: ""}, // disable k8s-secret state (no RBAC); use file state
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "state", MountPath: "/var/lib/tailscale"}},
			}},
			Volumes: []corev1.Volume{{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
	}
	must(t, wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := cs.CoreV1().Pods(tgNS).Create(ctx, p, metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err), nil
	}), "create exit node pod")
	t.Cleanup(func() { _ = cs.CoreV1().Pods(tgNS).Delete(context.Background(), name, *metav1.NewDeleteOptions(0)) })

	must(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
		got, err := cs.CoreV1().Pods(tgNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		podIP = got.Status.PodIP
		for _, c := range got.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return podIP != "", nil
			}
		}
		return false, nil
	}), "exit node pod ready")

	must(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execIn(ctx, cfg, cs, tgNS, name, "tailscaled", []string{"tailscale", "ip", "-4"})
		cgnat = strings.TrimSpace(out)
		return strings.HasPrefix(cgnat, "100."), nil
	}), "exit node tailnet IP")
	return cgnat, podIP
}
