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
func (m *mockTS) ResolveExitNode(context.Context, string, string) (string, error) {
	return "100.64.0.42", nil
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
		Spec:       egressv1.EgressGroupSpec{Routes: []string{"10.50.0.0/16"}},
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
	// idempotent: a third reconcile must not mint a second authkey
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatal(err)
	}
	if len(m.minted) != 1 {
		t.Fatalf("expected exactly 1 authkey mint, got %d", len(m.minted))
	}
	if got := m.minted[0]; len(got) != 1 || got[0] != "tag:egress-payments" {
		t.Fatalf("minted with wrong tags: %v", got)
	}
}
