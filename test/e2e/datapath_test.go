//go:build e2e

// Package e2e is the kind-based end-to-end test for tailgate. It:
//  1. creates a throwaway ephemeral tailnet via the Tailscale org API (code/.env),
//  2. points the operator at it (creds secret + restart),
//  3. stands up an in-process tsnet CGNAT peer as the egress target,
//  4. applies an EgressGroup and member/non-member pods,
//  5. asserts the member reaches the peer THROUGH the gateway and the non-member can't,
//  6. deletes the tailnet (defer) — no leak.
//
// Requires: a kind cluster with the operator+agent+CRD deployed (see `make e2e`), and
// TS_ORG_OAUTH_CLIENT_ID/SECRET (code/.env). Run: make e2e
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
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
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"tailscale.com/tsnet"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/tailnet"
)

const tgNS = "tailgate-system"

func loadEnv() {
	for _, p := range []string{".env", "../../.env"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok && os.Getenv(strings.TrimSpace(k)) == "" {
				os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
		f.Close()
	}
}

func TestEgressDatapath(t *testing.T) {
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

	// 1. ephemeral tailnet (deleted at the end).
	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-e2e-go-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Logf("ephemeral tailnet %s (dnsName=%s)", eg.DisplayName, eg.DNSName)
	t.Cleanup(func() {
		if err := eg.Close(context.Background()); err != nil {
			t.Errorf("LEAKED tailnet %s: %v", eg.DNSName, err)
		} else {
			t.Logf("deleted tailnet %s", eg.DNSName)
		}
	})
	must(t, eg.ApplyACL(ctx, []byte(`{"tagOwners":{"tag:ci":["autogroup:admin"],"tag:k8s": ["autogroup:admin"]},"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`)), "acl")

	// 2. point the operator at this tailnet + restart it.
	upsertSecret(t, ctx, kc, eg)
	restartOperator(t, ctx, cs)

	// 3. in-process tsnet peer (the egress target) — dual-family.
	key, err := eg.MintAuthKey(ctx, []string{"tag:ci"})
	must(t, err, "mint peer authkey")
	peerV4, peerV6, _ := startPeer(t, ctx, key, "tailgate-e2e-target")
	t.Logf("peer addrs: v4=%s v6=%s", peerV4, peerV6)

	// 4. EgressGroup + member/non-member pods.
	cleanupObj(t, kc, &egressv1.EgressGroup{ObjectMeta: metav1.ObjectMeta{Name: "e2e"}})
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e"},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": "e2e"}}}},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, "e2e") }) // finalizer device-delete runs before tailnet Close

	member := runPod(t, ctx, cs, "e2e-member", map[string]string{"egress": "e2e"})
	waitGatewayReady(t, ctx, cs, "e2e")
	other := runPod(t, ctx, cs, "e2e-nonmember", map[string]string{"app": "web"})
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0))
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), other, *metav1.NewDeleteOptions(0))
	})

	// 5a. member reaches the peer over CGNAT v4 THROUGH the gateway (retry for warmup).
	curlMember := func(target string) string {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "6", "http://" + target + "/"})
		return strings.TrimSpace(out)
	}
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 100*time.Second, true, func(ctx context.Context) (bool, error) {
		out := curlMember(peerV4)
		t.Logf("member curl v4 -> %q", out)
		return out == "ok", nil
	}); err != nil {
		t.Fatalf("member never reached the tailnet peer over v4 through the gateway: %v", err)
	}
	t.Log("PASS: member reached the CGNAT v4 peer through the gateway")

	// Prove the datapath was programmed via NATIVE nftables (not legacy iptables). On stock
	// kind the legacy modules are present, so a regression to iptables would still pass the
	// curl above — this assertion is what catches it (and mirrors the nf_tables-only Talos
	// reality where legacy silently no-ops).
	verifyGatewayNetfilter(t, ctx, cfg, cs, "e2e")

	// 5b. family-independence: this kind cluster is v4-only, yet the member must reach
	// the peer's IPv6 ULA through the SAME gateway (the veth link is dual-stack). Proves
	// a v4-only cluster carries v6 / 4via6 tailnet traffic.
	if peerV6 == "" {
		t.Fatal("peer never got a v6 ULA — cannot test family-independence")
	}
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		out := curlMember("[" + peerV6 + "]")
		t.Logf("member curl v6 -> %q", out)
		return out == "ok", nil
	}); err != nil {
		t.Fatalf("member never reached the tailnet peer over v6 ULA through the gateway "+
			"(family-independence broken): %v", err)
	}
	t.Log("PASS: member reached the IPv6 ULA peer through the gateway (family-independence)")

	// 5c. non-member must NOT reach it (either family).
	if out, _, _ := execPod(ctx, cfg, cs, other, []string{"curl", "-sS", "-m", "6", "http://" + peerV4 + "/"}); strings.TrimSpace(out) == "ok" {
		t.Fatal("non-member reached the tailnet peer over v4 — selection is broken")
	}
	if out, _, _ := execPod(ctx, cfg, cs, other, []string{"curl", "-sS", "-m", "6", "http://[" + peerV6 + "]/"}); strings.TrimSpace(out) == "ok" {
		t.Fatal("non-member reached the tailnet peer over v6 — selection is broken")
	}
	t.Log("PASS: non-member was correctly denied on both families")
}

// --- helpers ---

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func upsertSecret(t *testing.T, ctx context.Context, kc ctrlclient.Client, e *tailnet.Ephemeral) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tailgate-tailnet-creds", Namespace: tgNS},
		Data: map[string][]byte{
			"TS_TAILNET":             []byte(e.DNSName),
			"TS_OAUTH_CLIENT_ID":     []byte(e.ClientID),
			"TS_OAUTH_CLIENT_SECRET": []byte(e.ClientSecret),
		},
	}
	var cur corev1.Secret
	if err := kc.Get(ctx, types.NamespacedName{Namespace: tgNS, Name: s.Name}, &cur); apierrors.IsNotFound(err) {
		must(t, kc.Create(ctx, s), "create creds secret")
	} else {
		cur.Data = s.Data
		must(t, kc.Update(ctx, &cur), "update creds secret")
	}
	// also clear any prior per-group authkey so the operator re-mints against this tailnet
	_ = kc.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tailgate-e2e-authkey", Namespace: tgNS}})
}

func restartOperator(t *testing.T, ctx context.Context, cs *kubernetes.Clientset) {
	t.Helper()
	_ = cs.CoreV1().Pods(tgNS).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "app=tailgate-operator"})
	must(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(tgNS).List(ctx, metav1.ListOptions{LabelSelector: "app=tailgate-operator"})
		if err != nil {
			return false, nil
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
				return true, nil
			}
		}
		return false, nil
	}), "operator restart")
}

// startPeer brings up an in-process tsnet node serving "ok" on :80 (both families) and
// returns its CGNAT v4, ULA v6, and MagicDNS FQDN (trailing dot stripped).
func startPeer(t *testing.T, ctx context.Context, authkey, hostname string) (v4, v6, fqdn string) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "e2e-peer-*")
	s := &tsnet.Server{Hostname: hostname, Ephemeral: true, AuthKey: authkey, Dir: dir}
	t.Cleanup(func() { _ = s.Close() })
	ln, err := s.Listen("tcp", ":80")
	must(t, err, "peer listen")
	go func() {
		_ = serveOK(ln)
	}()
	lc, err := s.LocalClient()
	must(t, err, "peer localclient")
	must(t, wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		st, err := lc.Status(ctx)
		if err != nil || st.Self == nil {
			return false, nil
		}
		for _, a := range st.Self.TailscaleIPs {
			if a.Is4() {
				v4 = a.String()
			} else if a.Is6() {
				v6 = a.String()
			}
		}
		fqdn = strings.TrimSuffix(st.Self.DNSName, ".")
		return v4 != "" && v6 != "" && fqdn != "", nil
	}), "peer get IPs")
	return v4, v6, fqdn
}

func waitGatewayReady(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, group string) {
	t.Helper()
	must(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 150*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(tgNS).List(ctx, metav1.ListOptions{LabelSelector: "tailgate.dev/group=" + group})
		if err != nil {
			return false, nil
		}
		for _, p := range pods.Items {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return true, nil
				}
			}
		}
		return false, nil
	}), "gateway ready")
}

func runPod(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, name string, labels map[string]string) string {
	t.Helper()
	_ = cs.CoreV1().Pods("default").Delete(ctx, name, *metav1.NewDeleteOptions(0))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "c", Image: "nicolaka/netshoot", Command: []string{"sleep", "3600"},
		}}},
	}
	must(t, wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err), nil
	}), "create pod "+name)
	must(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		got, err := cs.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, c := range got.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}), "pod ready "+name)
	return name
}

func execPod(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, pod string, cmd []string) (string, string, error) {
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace("default").SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: "c", Command: cmd, Stdout: true, Stderr: true}, clientgoscheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	var out, errb bytes.Buffer
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &errb})
	return out.String(), errb.String(), err
}

// verifyGatewayNetfilter execs `nft list table inet tailgate` in a gateway pod and asserts
// the mark + masquerade rules are present — proving the gateway used native nftables.
func verifyGatewayNetfilter(t *testing.T, ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, group string) {
	t.Helper()
	pods, err := cs.CoreV1().Pods(tgNS).List(ctx, metav1.ListOptions{LabelSelector: "tailgate.dev/group=" + group})
	must(t, err, "list gateway pods")
	if len(pods.Items) == 0 {
		t.Fatalf("no gateway pod for group %q", group)
	}
	gw := pods.Items[0].Name
	out, errOut, err := execIn(ctx, cfg, cs, tgNS, gw, "gateway", []string{"nft", "list", "table", "inet", "tailgate"})
	if err != nil {
		t.Fatalf("nft list table inet tailgate in %s failed: %v (stderr: %s) — datapath not in nftables", gw, err, errOut)
	}
	for _, want := range []string{`iifname "tgbr0"`, `oifname "tailscale0"`, "masquerade", "7717"} {
		if !strings.Contains(out, want) {
			t.Fatalf("gateway nft table missing %q; ruleset:\n%s", want, out)
		}
	}
	t.Logf("PASS: gateway %s programmed the native nft datapath (table inet tailgate)", gw)
}

func cleanupObj(t *testing.T, kc ctrlclient.Client, obj ctrlclient.Object) {
	_ = kc.Delete(context.Background(), obj)
}

func serveOK(ln net.Listener) error {
	return http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
}
