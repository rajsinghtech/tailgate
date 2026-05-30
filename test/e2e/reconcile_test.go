//go:build e2e

// Live-reconcile e2e: prove that changing the EgressGroup spec re-renders the gateway
// config and tailscaled hot-reloads it WITHOUT restarting the pod. This is the
// production-resilience guarantee — operators can flip accept-routes / exit-node / DNS
// on a live group and member tunnels stay up (same node identity, no flap).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/tailnet"
)

func TestConfigReconcileNoRestart(t *testing.T) {
	loadEnv()
	orgID, orgSec := getOrgCreds()
	if orgID == "" || orgSec == "" {
		t.Skip("TS_ORG_OAUTH_CLIENT_ID/SECRET not set (code/.env) — skipping e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cfg, cs, kc := clients(t)

	// 1. ephemeral tailnet + point the operator at it.
	tn := tailnet.New(orgID, orgSec)
	eg, err := tn.Create(ctx, "tailgate-recon-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{"tagOwners":{"tag:ci":["autogroup:admin"],"tag:egress-recon":["autogroup:admin"]},"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "recon")
	restartOperator(t, ctx, cs)

	// 2. EgressGroup starting with acceptRoutes=false (RouteAll will be false on the node).
	ff := false
	name := "recon"
	forceCleanEG(t, ctx, cs, kc, name) // guarantee a FRESH gateway (no stale pod to read)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			AcceptRoutes: &ff,
			Selector:     egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) }) // runs before tailnet Close (LIFO)

	waitGatewayReady(t, ctx, cs, name)

	// 3. snapshot the gateway pod identity + the node's RouteAll pref (should be false).
	p0 := gatewayPod(t, ctx, cs, name)
	t.Logf("gateway pod %s uid=%s restarts=%d", p0.Name, p0.UID, restarts(p0))
	if got := routeAll(t, ctx, cfg, cs, p0.Name); got {
		t.Fatalf("precondition: RouteAll should start false, got true")
	}

	// 4. FLIP acceptRoutes=true on the live group.
	patchAcceptRoutes(t, ctx, kc, name, true)

	// 5. operator re-renders the ConfigMap.
	must(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		var cm corev1.ConfigMap
		if err := kc.Get(ctx, types.NamespacedName{Namespace: tgNS, Name: "tailgate-gw-" + name + "-config"}, &cm); err != nil {
			return false, nil
		}
		return strings.Contains(cm.Data["tailscaled.json"], `"acceptRoutes":true`), nil
	}), "configmap re-rendered with acceptRoutes:true")
	t.Log("PASS: operator re-rendered the gateway ConfigMap")

	// 6. the gateway hot-reloads: RouteAll becomes true. Kubelet projects the volume on
	// its sync cycle (~1-2m), then the entrypoint's poll calls ReloadConfig.
	must(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 200*time.Second, true, func(ctx context.Context) (bool, error) {
		return routeAll(t, ctx, cfg, cs, p0.Name), nil
	}), "gateway applied acceptRoutes via reload")
	t.Log("PASS: gateway applied the new acceptRoutes pref via hot-reload")

	// 6b. EXIT-NODE selection reconciles the same way. Set spec.ExitNode to a CGNAT IP
	// (stored as ExitNodeIP directly; no node resolution needed for the pref to apply) and
	// confirm the live node picks it up via reload.
	const exitIP = "100.64.0.9"
	patchExitNode(t, ctx, kc, name, exitIP)
	must(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 200*time.Second, true, func(ctx context.Context) (bool, error) {
		return exitNodeIP(t, ctx, cfg, cs, p0.Name) == exitIP, nil
	}), "gateway applied exitNode via reload")
	t.Log("PASS: gateway applied the exit-node selection via hot-reload")

	// 7. THE GUARANTEE: same pod, no restart — identity and tunnels never flapped across
	// BOTH the accept-routes and exit-node changes.
	p1 := gatewayPod(t, ctx, cs, name)
	if p1.UID != p0.UID {
		t.Fatalf("gateway pod was RECREATED (uid %s -> %s) — reconcile was disruptive", p0.UID, p1.UID)
	}
	if restarts(p1) != restarts(p0) {
		t.Fatalf("gateway container RESTARTED (%d -> %d) — reconcile was disruptive", restarts(p0), restarts(p1))
	}
	t.Logf("PASS: gateway reconciled in place (pod %s uid=%s restarts=%d, unchanged)", p1.Name, p1.UID, restarts(p1))
}

// --- helpers specific to the reconcile test ---

func clients(t *testing.T) (*rest.Config, *kubernetes.Clientset, ctrlclient.Client) {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	must(t, err, "kubeconfig")
	cs, err := kubernetes.NewForConfig(cfg)
	must(t, err, "clientset")
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = egressv1.AddToScheme(scheme)
	kc, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	must(t, err, "ctrl client")
	return cfg, cs, kc
}

func gatewayPod(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, group string) *corev1.Pod {
	t.Helper()
	pods, err := cs.CoreV1().Pods(tgNS).List(ctx, metav1.ListOptions{LabelSelector: "tailgate.dev/group=" + group})
	must(t, err, "list gateway pods")
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			return &pods.Items[i]
		}
	}
	t.Fatalf("no gateway pod for group %s", group)
	return nil
}

func restarts(p *corev1.Pod) int32 {
	if len(p.Status.ContainerStatuses) == 0 {
		return -1
	}
	return p.Status.ContainerStatuses[0].RestartCount
}

// routeAll reads the gateway tailscaled prefs and returns RouteAll (accept-routes).
func routeAll(t *testing.T, ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, pod string) bool {
	out, _, err := execIn(ctx, cfg, cs, tgNS, pod, "gateway",
		[]string{"tailscale", "--socket=/var/run/tailscale/tailscaled.sock", "debug", "prefs"})
	if err != nil {
		return false
	}
	var prefs struct {
		RouteAll bool `json:"RouteAll"`
	}
	if err := json.Unmarshal([]byte(out), &prefs); err != nil {
		return false
	}
	return prefs.RouteAll
}

func patchAcceptRoutes(t *testing.T, ctx context.Context, kc ctrlclient.Client, name string, v bool) {
	t.Helper()
	var eg egressv1.EgressGroup
	must(t, kc.Get(ctx, types.NamespacedName{Name: name}, &eg), "get egressgroup")
	eg.Spec.AcceptRoutes = &v
	must(t, kc.Update(ctx, &eg), "patch acceptRoutes")
}

func patchExitNode(t *testing.T, ctx context.Context, kc ctrlclient.Client, name, nodeID string) {
	t.Helper()
	var eg egressv1.EgressGroup
	must(t, kc.Get(ctx, types.NamespacedName{Name: name}, &eg), "get egressgroup")
	eg.Spec.ExitNode = &egressv1.ExitNodeRef{NodeID: nodeID}
	must(t, kc.Update(ctx, &eg), "patch exitNode")
}

// exitNodeIP reads the gateway tailscaled prefs and returns ExitNodeIP.
func exitNodeIP(t *testing.T, ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, pod string) string {
	out, _, err := execIn(ctx, cfg, cs, tgNS, pod, "gateway",
		[]string{"tailscale", "--socket=/var/run/tailscale/tailscaled.sock", "debug", "prefs"})
	if err != nil {
		return ""
	}
	var prefs struct {
		ExitNodeIP string `json:"ExitNodeIP"`
	}
	if err := json.Unmarshal([]byte(out), &prefs); err != nil {
		return ""
	}
	return prefs.ExitNodeIP
}

func execIn(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, ns, pod, container string, cmd []string) (string, string, error) {
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdout: true, Stderr: true}, clientgoscheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	var out, errb bytes.Buffer
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &errb})
	return out.String(), errb.String(), err
}

// upsertSecretFor points the operator at tailnet e and clears the named group's authkey.
func upsertSecretFor(t *testing.T, ctx context.Context, kc ctrlclient.Client, e *tailnet.Ephemeral, group string) {
	t.Helper()
	upsertSecret(t, ctx, kc, e)
	_ = kc.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tailgate-" + group + "-authkey", Namespace: tgNS}})
}

func getOrgCreds() (string, string) {
	return os.Getenv("TS_ORG_OAUTH_CLIENT_ID"), os.Getenv("TS_ORG_OAUTH_CLIENT_SECRET")
}

// forceCleanEG removes any stale EgressGroup of this name — clearing its finalizer so a
// previous run's now-deleted tailnet can't wedge it — and waits for its gateway pods to
// disappear, so the test reads a genuinely fresh gateway.
func forceCleanEG(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, kc ctrlclient.Client, name string) {
	t.Helper()
	var eg egressv1.EgressGroup
	if err := kc.Get(ctx, types.NamespacedName{Name: name}, &eg); err == nil {
		eg.Finalizers = nil
		_ = kc.Update(ctx, &eg)
		_ = kc.Delete(ctx, &eg)
	}
	_ = wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, _ := cs.CoreV1().Pods(tgNS).List(ctx, metav1.ListOptions{LabelSelector: "tailgate.dev/group=" + name})
		var cur egressv1.EgressGroup
		egGone := apierrors.IsNotFound(kc.Get(ctx, types.NamespacedName{Name: name}, &cur))
		return egGone && len(pods.Items) == 0, nil
	})
}

// deleteEGWait deletes an EgressGroup and waits for it to be fully gone — the finalizer
// runs the gateway device-delete against the STILL-ALIVE tailnet. Call before tearing the
// tailnet down (register the tailnet Close cleanup first so LIFO runs this earlier).
func deleteEGWait(kc ctrlclient.Client, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_ = kc.Delete(ctx, &egressv1.EgressGroup{ObjectMeta: metav1.ObjectMeta{Name: name}})
	_ = wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		var eg egressv1.EgressGroup
		return apierrors.IsNotFound(kc.Get(ctx, types.NamespacedName{Name: name}, &eg)), nil
	})
}
