package controller

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/tsclient"
)

const finalizer = "tailgate.dev/finalizer"

// EgressGroupReconciler reconciles an EgressGroup into a gateway Deployment +
// Service + an OAuth-minted authkey Secret.
type EgressGroupReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	TS        tsclient.Client
	Namespace string
	GWImage   string
	Tailnet   string // backing tailnet (dnsName); keys the gateway's persisted-state dir
}

// Reconcile implements the control loop.
func (r *EgressGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	var eg egressv1.EgressGroup
	if err := r.Get(ctx, req.NamespacedName, &eg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !eg.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&eg, finalizer) {
			if r.TS != nil {
				if err := r.TS.DeleteDeviceByHostname(ctx, gatewayName(eg.Name)); err != nil {
					l.Error(err, "deleting gateway tailnet device (will retry)")
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&eg, finalizer)
			return ctrl.Result{}, r.Update(ctx, &eg)
		}
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&eg, finalizer) {
		if err := r.Update(ctx, &eg); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureAuthKeySecret(ctx, &eg); err != nil {
		return ctrl.Result{}, err
	}
	exitNodeID := r.effectiveExitNode(ctx, &eg, l)
	cm, err := gatewayConfigMap(&eg, r.Namespace, exitNodeID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyOwned(ctx, &eg, cm); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyOwned(ctx, &eg, gatewayDaemonSet(&eg, r.Namespace, r.GWImage, r.Tailnet)); err != nil {
		return ctrl.Result{}, err
	}

	eg.Status.GatewayHostname = gatewayName(eg.Name)
	eg.Status.ResolvedExitNode = exitNodeID
	if err := r.Status().Update(ctx, &eg); err != nil {
		return ctrl.Result{}, err
	}
	l.Info("reconciled", "group", eg.Name)
	// Re-resolve an auto-selected exit node periodically so a node going offline fails over.
	if en := eg.Spec.ExitNode; en != nil && isAutoExitNode(en.Name) {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

// effectiveExitNode is the concrete exit node to pin. It mirrors the native --exit-node value
// space: an explicit ref (IP / MagicDNS name / StableID) is echoed verbatim, while "auto" is
// resolved to a concrete node from the tailnet (the declarative config can't carry "auto").
// Resolution failure is logged and yields "" rather than failing the whole reconcile.
func (r *EgressGroupReconciler) effectiveExitNode(ctx context.Context, eg *egressv1.EgressGroup, l logr.Logger) string {
	en := eg.Spec.ExitNode
	if en == nil || en.Name == "" {
		return ""
	}
	if !isAutoExitNode(en.Name) {
		return en.Name // explicit ref, passed through like `tailscale set --exit-node=<ref>`
	}
	if r.TS == nil {
		return ""
	}
	id, err := r.TS.ResolveExitNode(ctx)
	if err != nil {
		l.Info("exit-node auto-selection found no candidate", "group", eg.Name, "err", err.Error())
		return ""
	}
	return id
}

// isAutoExitNode reports whether name is the native auto expression ("auto" / "auto:any" / any
// "auto:" prefix), matching how tailscaled's ParseAutoExitNodeString treats the value.
func isAutoExitNode(name string) bool {
	return name == "auto" || strings.HasPrefix(name, "auto:")
}

// applyOwned create-or-updates obj with eg as controller owner (for GC).
func (r *EgressGroupReconciler) applyOwned(ctx context.Context, eg *egressv1.EgressGroup, obj client.Object) error {
	if r.Scheme != nil {
		if err := controllerutil.SetControllerReference(eg, obj, r.Scheme); err != nil {
			return err
		}
	}
	existing, _ := obj.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	// preserve the cluster-assigned ClusterIP on Service updates
	if svc, ok := obj.(*corev1.Service); ok {
		if cur, ok2 := existing.(*corev1.Service); ok2 {
			svc.Spec.ClusterIP = cur.Spec.ClusterIP
		}
	}
	return r.Update(ctx, obj)
}

// SetupWithManager wires the controller + owned-object watches.
func (r *EgressGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&egressv1.EgressGroup{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Named("egressgroup").
		Complete(r)
}
