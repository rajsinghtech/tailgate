package controller

import (
	"context"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/tsclient"
	"github.com/rajsinghtech/tailgate/internal/wiring"
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

	// Compute the member-node set for auto-follow scheduling. In static-pin mode
	// (spec.gateway.nodeSelector set) this is unused — the DaemonSet uses the selector.
	memberNodes := r.memberNodes(ctx, &eg, l)
	if err := r.applyOwned(ctx, &eg, gatewayDaemonSet(&eg, r.Namespace, r.GWImage, r.Tailnet, memberNodes)); err != nil {
		return ctrl.Result{}, err
	}

	eg.Status.GatewayHostname = gatewayName(eg.Name)
	eg.Status.ResolvedExitNode = exitNodeID
	eg.Status.GatewayNodes = memberNodes
	if err := r.Status().Update(ctx, &eg); err != nil {
		return ctrl.Result{}, err
	}
	l.Info("reconciled", "group", eg.Name, "memberNodes", memberNodes)
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

// memberNodes lists pods matching the EgressGroup's selector and returns the distinct
// node names they are scheduled on. Used for auto-follow scheduling: the gateway
// DaemonSet lands only on these nodes. In static-pin mode (spec.gateway.nodeSelector
// set) the caller ignores the result and the DaemonSet uses the selector. Returns nil
// when no pods match or the selector is empty (matches nothing per wiring.MatchGroup).
//
// A pod counts as soon as it has a nodeName (is scheduled), not when it's Running —
// the gateway needs to be on the node before the member pod's agent wiring runs, so
// triggering on schedule rather than Running minimizes the gap. The pod watch predicate
// fires on nodeName changes (Pending→scheduled), which is what drives the re-scope.
func (r *EgressGroupReconciler) memberNodes(ctx context.Context, eg *egressv1.EgressGroup, l logr.Logger) []string {
	sel := eg.Spec.Selector
	// An empty selector (no podSelector AND no namespaceSelector) matches nothing —
	// wiring.MatchGroup skips it. Short-circuit to avoid a full pod list.
	if sel.PodSelector == nil && sel.NamespaceSelector == nil {
		return nil
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		l.Error(err, "list pods for member-node computation")
		return nil
	}

	// Build a namespace-label cache so wiring.MatchGroup can evaluate namespaceSelector.
	nsCache := map[string]map[string]string{}
	matchPods := []egressv1.EgressGroup{*eg}

	seen := map[string]bool{}
	var nodes []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" {
			continue // not scheduled yet
		}
		nsLabels, ok := nsCache[p.Namespace]
		if !ok {
			var nsObj corev1.Namespace
			if err := r.Get(ctx, client.ObjectKey{Name: p.Namespace}, &nsObj); err != nil {
				nsLabels = map[string]string{}
			} else {
				nsLabels = nsObj.Labels
			}
			nsCache[p.Namespace] = nsLabels
		}
		if wiring.MatchGroup(p, nsLabels, matchPods) == "" {
			continue
		}
		if !seen[p.Spec.NodeName] {
			seen[p.Spec.NodeName] = true
			nodes = append(nodes, p.Spec.NodeName)
		}
	}
	return nodes
}

// allEgressGroups is the map function for the pod watch: it enqueues every EgressGroup
// for reconciliation when a relevant pod event fires. The predicate (set in
// SetupWithManager) filters to pod create/delete and scheduling/label changes, so the
// noise is bounded. With a handful of EgressGroups this is cheaper than evaluating
// selectors in the map function (which would need namespace-label lookups).
func (r *EgressGroupReconciler) allEgressGroups(ctx context.Context, _ client.Object) []reconcile.Request {
	var groups egressv1.EgressGroupList
	if err := r.List(ctx, &groups); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(groups.Items))
	for i := range groups.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: groups.Items[i].Name}})
	}
	return reqs
}

// podScheduleChanged is the predicate for the pod watch: it fires on pod create/delete
// and on updates where spec.nodeName or metadata.labels changed — the only transitions
// that affect the member-node set. Spec/status-only updates (image pulls, condition
// flips) are ignored to avoid unnecessary reconciles.
var podScheduleChanged = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	DeleteFunc: func(event.DeleteEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldPod, okOld := e.ObjectOld.(*corev1.Pod)
		newPod, okNew := e.ObjectNew.(*corev1.Pod)
		if !okOld || !okNew {
			return true
		}
		return oldPod.Spec.NodeName != newPod.Spec.NodeName ||
			!reflect.DeepEqual(oldPod.Labels, newPod.Labels)
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
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
		// Watch pods so auto-follow scheduling reacts to member pods appearing on
		// new nodes or draining from old ones. The predicate filters to scheduling
		// transitions; the map function enqueues all EgressGroups (selector matching
		// happens in the reconcile loop).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.allEgressGroups), builder.WithPredicates(podScheduleChanged)).
		Named("egressgroup").
		Complete(r)
}
