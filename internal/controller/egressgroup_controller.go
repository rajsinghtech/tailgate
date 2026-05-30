package controller

import (
	"context"
	"strings"

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
	cm, err := gatewayConfigMap(&eg, r.Namespace)
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
	eg.Status.AdvertisedRoutes = strings.Join(eg.Spec.Routes, ",")
	if err := r.Status().Update(ctx, &eg); err != nil {
		return ctrl.Result{}, err
	}
	l.Info("reconciled", "group", eg.Name)
	return ctrl.Result{}, nil
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
