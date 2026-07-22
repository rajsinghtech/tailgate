package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

type mockTS struct{ minted [][]string }

func (m *mockTS) MintAuthKey(_ context.Context, tags []string) (string, error) {
	m.minted = append(m.minted, tags)
	return "tskey-auth-test", nil
}
func (m *mockTS) DeleteDeviceByHostname(context.Context, string) error { return nil }
func (m *mockTS) ResolveExitNode(context.Context) (string, error) {
	return "100.64.0.42", nil
}
func (m *mockTS) TailnetName(context.Context) (string, error) {
	return "test.ts.net", nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := egressv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReconcileCreatesGatewaySecretService(t *testing.T) {
	s := newScheme(t)
	eg := &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       egressv1.EgressGroupSpec{},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(eg).
		WithStatusSubresource(&egressv1.EgressGroup{}).Build()
	m := &mockTS{}
	r := &EgressGroupReconciler{Client: cl, Scheme: s, TS: m, Namespace: "tailgate-system", GWImage: "tailgate-gateway:dev"}

	ctx := context.Background()
	// Reconcile twice: first adds finalizer (early return path may requeue), second provisions.
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	var sec corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Name: "tailgate-payments-authkey", Namespace: "tailgate-system"}, &sec); err != nil {
		t.Fatalf("authkey secret not created: %v", err)
	}
	if string(sec.Data["TS_AUTHKEY"]) != "tskey-auth-test" {
		t.Fatalf("wrong authkey: %q", sec.Data["TS_AUTHKEY"])
	}
	var ds appsv1.DaemonSet
	if err := cl.Get(ctx, types.NamespacedName{Name: "tailgate-gw-payments", Namespace: "tailgate-system"}, &ds); err != nil {
		t.Fatalf("gateway daemonset not created: %v", err)
	}
	if !*ds.Spec.Template.Spec.Containers[0].SecurityContext.Privileged {
		t.Fatal("gateway must be privileged (kernel TUN)")
	}
	if got := ds.Spec.Template.Spec.Containers[0].Image; got != "tailgate-gateway:dev" {
		t.Fatalf("gateway image = %q", got)
	}
	// Empty selector → no member pods → auto-follow schedules nowhere (sentinel).
	if aff := ds.Spec.Template.Spec.Affinity; aff == nil || aff.NodeAffinity == nil {
		t.Fatal("expected nodeAffinity for auto-follow (schedule nowhere)")
	}
	// idempotent: a third reconcile must not mint a second authkey
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatal(err)
	}
	if len(m.minted) != 1 {
		t.Fatalf("expected exactly 1 authkey mint, got %d", len(m.minted))
	}
	if got := m.minted[0]; len(got) != 1 || got[0] != "tag:k8s" {
		t.Fatalf("minted with wrong tags: %v", got)
	}
}

func TestAutoFollowSchedulesOnMemberNodes(t *testing.T) {
	s := newScheme(t)
	eg := &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tailgate.dev/egress": "true"},
				},
			},
		},
	}
	// Two member pods on different nodes + one non-member pod.
	memberA := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default", Labels: map[string]string{"tailgate.dev/egress": "true"}},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
	memberB := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default", Labels: map[string]string{"tailgate.dev/egress": "true"}},
		Spec:       corev1.PodSpec{NodeName: "node-2"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.2"},
	}
	nonMember := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", Labels: map[string]string{"app": "unrelated"}},
		Spec:       corev1.PodSpec{NodeName: "node-3"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.3"},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(eg, memberA, memberB, nonMember).
		WithStatusSubresource(&egressv1.EgressGroup{}).Build()
	r := &EgressGroupReconciler{Client: cl, Scheme: s, TS: &mockTS{}, Namespace: "tailgate-system", GWImage: "tailgate-gateway:dev"}

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	var ds appsv1.DaemonSet
	if err := cl.Get(ctx, types.NamespacedName{Name: "tailgate-gw-payments", Namespace: "tailgate-system"}, &ds); err != nil {
		t.Fatalf("gateway daemonset not created: %v", err)
	}
	aff := ds.Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("expected nodeAffinity for auto-follow")
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 {
		t.Fatalf("expected 1 term with 1 matchField, got %v", terms)
	}
	req := terms[0].MatchFields[0]
	if req.Key != "metadata.name" || req.Operator != corev1.NodeSelectorOpIn {
		t.Fatalf("wrong matchField: %+v", req)
	}
	if !containsAll(req.Values, "node-1", "node-2") || containsString(req.Values, "node-3") {
		t.Fatalf("auto-follow should target node-1+node-2 only, got %v", req.Values)
	}
	if ds.Spec.Template.Spec.NodeSelector != nil {
		t.Fatal("auto-follow mode should not set nodeSelector")
	}
}

func TestStaticNodeSelectorOptsOutOfAutoFollow(t *testing.T) {
	s := newScheme(t)
	eg := &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "gvisor-group"},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tailgate.dev/egress": "true"},
				},
			},
			Gateway: &egressv1.GatewaySpec{
				NodeSelector: map[string]string{"nodepool": "gvisor"},
			},
		},
	}
	member := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default", Labels: map[string]string{"tailgate.dev/egress": "true"}},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(eg, member).
		WithStatusSubresource(&egressv1.EgressGroup{}).Build()
	r := &EgressGroupReconciler{Client: cl, Scheme: s, TS: &mockTS{}, Namespace: "tailgate-system", GWImage: "tailgate-gateway:dev"}

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "gvisor-group"}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	var ds appsv1.DaemonSet
	if err := cl.Get(ctx, types.NamespacedName{Name: "tailgate-gw-gvisor-group", Namespace: "tailgate-system"}, &ds); err != nil {
		t.Fatalf("gateway daemonset not created: %v", err)
	}
	if got := ds.Spec.Template.Spec.NodeSelector; got == nil || got["nodepool"] != "gvisor" {
		t.Fatalf("expected nodeSelector nodepool=gvisor, got %v", got)
	}
	if ds.Spec.Template.Spec.Affinity != nil {
		t.Fatal("static-pin mode should not set nodeAffinity (auto-follow disabled)")
	}
}

func containsAll(slice []string, a, b string) bool {
	return containsString(slice, a) && containsString(slice, b)
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
