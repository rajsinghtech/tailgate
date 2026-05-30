// tailgate-operator reconciles EgressGroup CRDs into per-group gateway DaemonSets +
// OAuth-minted authkeys.
package main

import (
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/controller"
	"github.com/rajsinghtech/tailgate/internal/tsclient"
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme")
		os.Exit(1)
	}
	if err := egressv1.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "manager")
		os.Exit(1)
	}

	tailnet := getenv("TS_TAILNET", "-")
	ts := tsclient.New(
		tailnet,
		os.Getenv("TS_OAUTH_CLIENT_ID"),
		os.Getenv("TS_OAUTH_CLIENT_SECRET"),
	)
	r := &controller.EgressGroupReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		TS:        ts,
		Namespace: getenv("POD_NAMESPACE", "tailgate-system"),
		GWImage:   getenv("GW_IMAGE", "tailgate-gateway:dev"),
		Tailnet:   tailnet,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "setup reconciler")
		os.Exit(1)
	}
	log.Info("starting tailgate-operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
