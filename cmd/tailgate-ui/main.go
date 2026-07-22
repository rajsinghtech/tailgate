// tailgate-ui is the management web UI for EgressGroups. In production it runs
// behind a gateway with tinyauth, which injects a Remote-Email header for
// identity. For standalone deployments, an optional Tailscale OAuth fallback
// is supported (set TAILGATE_UI_OAUTH_CLIENT_ID to enable it).
package main

import (
	"crypto/rand"
	"flag"
	"log/slog"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/auth"
	"github.com/rajsinghtech/tailgate/internal/ui"
)

var (
	version = "dev"
	commit  = "unknown"
)

var buildDate = "unknown"

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", getenv("TAILGATE_UI_ADDR", ":8080"), "listen address")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error("scheme", "err", err)
		os.Exit(1)
	}
	if err := egressv1.AddToScheme(scheme); err != nil {
		log.Error("scheme egress", "err", err)
		os.Exit(1)
	}

	cfg := ctrl.GetConfigOrDie()
	kc, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error("kube client", "err", err)
		os.Exit(1)
	}

	// Admin emails: comma-separated list from env.
	var admins []string
	if a := getenv("TAILGATE_UI_ADMIN_EMAILS", ""); a != "" {
		admins = append(admins, splitComma(a)...)
	}

	// OAuth fallback: only enabled when client ID is set. In gateway mode
	// (tinyauth injects Remote-Email), OAuth is not needed.
	var authHandler *auth.Handler
	oauthClientID := getenv("TAILGATE_UI_OAUTH_CLIENT_ID", "")
	if oauthClientID != "" {
		sessionKey := []byte(getenv("TAILGATE_UI_SESSION_KEY", ""))
		if len(sessionKey) < 32 {
			sessionKey = make([]byte, 32)
			_, _ = rand.Read(sessionKey)
			log.Info("generated ephemeral session key (set TAILGATE_UI_SESSION_KEY for persistence)")
		}
		authCfg := auth.Config{
			ClientID:     oauthClientID,
			ClientSecret: getenv("TAILGATE_UI_OAUTH_CLIENT_SECRET", ""),
			RedirectURL:  getenv("TAILGATE_UI_OAUTH_REDIRECT_URL", ""),
			SessionKey:   sessionKey,
		}
		authHandler = auth.NewHandler(authCfg, log)
		log.Info("OAuth fallback enabled (gateway mode is primary)")
	} else {
		log.Info("running in gateway mode (expects Remote-Email header from tinyauth)")
	}

	srv := ui.NewServer(kc, scheme, authHandler, admins, log)

	log.Info("starting tailgate-ui", "addr", *addr, "version", version, "commit", commit, "buildDate", buildDate)
	if err := listenAndServe(*addr, srv.Handler()); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func splitComma(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
