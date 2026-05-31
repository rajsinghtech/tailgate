//go:build e2e

// Agent rolling-update safety. The adversarial deploy-safety review found that an agent
// DaemonSet restart starts with an empty in-memory wiring map and would re-Wire every
// already-wired member — and the old Wire() unconditionally deleted+recreated the veth,
// blipping live egress on every agent roll (including the operator's own upgrades). This
// test proves the fix: capture the member's ts0 ifindex, restart the agent, and assert the
// ifindex is unchanged (the wiring was ADOPTED, not recreated) and egress never broke.
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestAgentRestartNoReWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

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

	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-e2e-restart-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Logf("ephemeral tailnet %s", eg.DNSName)
	t.Cleanup(func() {
		if err := eg.Close(context.Background()); err != nil {
			t.Errorf("LEAKED tailnet %s: %v", eg.DNSName, err)
		}
	})
	must(t, eg.ApplyACL(ctx, []byte(`{"tagOwners":{"tag:ci":["autogroup:admin"],"tag:k8s": ["autogroup:admin"]},"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`)), "acl")

	upsertSecret(t, ctx, kc, eg)
	restartOperator(t, ctx, cs)

	key, err := eg.MintAuthKey(ctx, []string{"tag:ci"})
	must(t, err, "mint peer authkey")
	peerV4, _, _ := startPeer(t, ctx, key, "tailgate-e2e-restart-target")
	t.Logf("peer v4=%s", peerV4)

	cleanupObj(t, kc, &egressv1.EgressGroup{ObjectMeta: metav1.ObjectMeta{Name: "e2e"}})
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e"},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": "e2e"}}}},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, "e2e") })

	waitGatewayReady(t, ctx, cs, "e2e")
	member := runPod(t, ctx, cs, "e2e-restart-member", map[string]string{"egress": "e2e"})
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0))
	})

	curlOK := func() bool {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "6", "http://" + peerV4 + "/"})
		return strings.TrimSpace(out) == "ok"
	}
	must(t, wait.PollUntilContextTimeout(ctx, 4*time.Second, 100*time.Second, true, func(ctx context.Context) (bool, error) {
		return curlOK(), nil
	}), "member reach peer pre-restart")
	t.Log("PASS: member reached the peer before the agent restart")

	before := ts0Ifindex(ctx, cfg, cs, member)
	t.Logf("member ts0 ifindex before agent restart: %q", before)
	if before == "" {
		t.Fatal("member has no ts0 — not wired")
	}

	// Roll the agent DaemonSet (the exact action a chart/image upgrade performs).
	restartAgent(t, ctx, cs)
	time.Sleep(8 * time.Second) // let the fresh agent run a couple of 3s sync passes

	after := ts0Ifindex(ctx, cfg, cs, member)
	t.Logf("member ts0 ifindex after agent restart: %q", after)
	if after == "" {
		t.Fatal("member lost ts0 after agent restart")
	}
	if after != before {
		t.Fatalf("agent restart RECREATED the member veth (ts0 ifindex %s -> %s) — this blips live egress. "+
			"Wire() must adopt an existing-and-correct wiring, not delete+recreate it.", before, after)
	}
	t.Log("PASS: agent restart did NOT recreate the member veth (ts0 ifindex unchanged) — no egress blip")

	if !curlOK() {
		t.Fatal("member lost egress after agent restart")
	}
	t.Log("PASS: member still reaches the peer after the agent restart")
}

func ts0Ifindex(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, pod string) string {
	out, _, _ := execPod(ctx, cfg, cs, pod, []string{"cat", "/sys/class/net/ts0/ifindex"})
	return strings.TrimSpace(out)
}

// restartAgent deletes the agent DaemonSet pods and waits for a fresh one to be Ready, with
// no pod still terminating — i.e. the rolling update has fully landed.
func restartAgent(t *testing.T, ctx context.Context, cs *kubernetes.Clientset) {
	t.Helper()
	_ = cs.CoreV1().Pods(tgNS).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "app=tailgate-agent"})
	must(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(tgNS).List(ctx, metav1.ListOptions{LabelSelector: "app=tailgate-agent"})
		if err != nil || len(pods.Items) == 0 {
			return false, nil
		}
		ready := false
		for _, p := range pods.Items {
			if p.DeletionTimestamp != nil {
				return false, nil // old pod still terminating
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					ready = true
				}
			}
		}
		return ready, nil
	}), "agent restart")
}
