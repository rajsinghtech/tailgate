// tailgate-operator reconciles EgressGroup CRDs into per-group gateway DaemonSets +
// OAuth-minted authkeys.
package main

import (
	"context"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/controller"
	"github.com/rajsinghtech/tailgate/internal/tsclient"
	tgwebhook "github.com/rajsinghtech/tailgate/internal/webhook"
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

	cfg := ctrl.GetConfigOrDie()
	certDir := getenv("WEBHOOK_CERT_DIR", "/tmp/k8s-webhook-server/serving-certs")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:        scheme,
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{CertDir: certDir, Port: 9443}),
	})
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

	// DNS mutating webhook: self-bootstrap a serving cert (no cert-manager) and register the
	// pod mutator that gives dns-enabled members native tailnet DNS.
	ns := getenv("POD_NAMESPACE", "tailgate-system")
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "direct client")
		os.Exit(1)
	}
	if err := tgwebhook.EnsureCerts(context.Background(), direct, certDir,
		getenv("WEBHOOK_SERVICE", "tailgate-operator-webhook"), ns,
		getenv("WEBHOOK_CONFIG", "tailgate-operator")); err != nil {
		log.Error(err, "webhook certs")
		os.Exit(1)
	}
	mgr.GetWebhookServer().Register("/mutate-v1-pod", &admission.Webhook{
		Handler: &tgwebhook.DNSMutator{Client: mgr.GetClient(), Decoder: admission.NewDecoder(scheme), Tailnet: tailnet},
	})

	log.Info("starting tailgate-operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
