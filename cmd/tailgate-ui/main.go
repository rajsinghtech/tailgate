// tailgate-ui is the management web UI for EgressGroups. It authenticates users
// via Tailscale OAuth (identity-only) and reads/writes EgressGroup CRs through
// the kube API. Users see their own groups; admins see all. The operator is
// unchanged — it reconciles the CRs the UI writes.
package main

import (
	"crypto/rand"
	"flag"
	"log/slog"
	"os"

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

	// Session key: generate a random one if not provided (resets cookies on restart,
	// which is fine for dev). In production, set TAILGATE_UI_SESSION_KEY to a stable
	// 32-byte hex string so sessions survive restarts.
	sessionKey := []byte(getenv("TAILGATE_UI_SESSION_KEY", ""))
	if len(sessionKey) < 32 {
		sessionKey = make([]byte, 32)
		_, _ = rand.Read(sessionKey)
		log.Info("generated ephemeral session key (set TAILGATE_UI_SESSION_KEY for persistence)")
	}

	// Admin emails: comma-separated list from env.
	var admins []string
	if a := getenv("TAILGATE_UI_ADMIN_EMAILS", ""); a != "" {
		for _, e := range splitComma(a) {
			admins = append(admins, e)
		}
	}

	authCfg := auth.Config{
		ClientID:     getenv("TAILGATE_UI_OAUTH_CLIENT_ID", ""),
		ClientSecret: getenv("TAILGATE_UI_OAUTH_CLIENT_SECRET", ""),
		RedirectURL:  getenv("TAILGATE_UI_OAUTH_REDIRECT_URL", ""),
		SessionKey:   sessionKey,
		AdminEmails:  admins,
	}
	if authCfg.ClientID == "" || authCfg.ClientSecret == "" {
		log.Error("TAILGATE_UI_OAUTH_CLIENT_ID and TAILGATE_UI_OAUTH_CLIENT_SECRET are required")
		os.Exit(1)
	}
	if authCfg.RedirectURL == "" {
		log.Error("TAILGATE_UI_OAUTH_REDIRECT_URL is required (e.g. https://tailgate-ui.corp.ts.net/callback)")
		os.Exit(1)
	}

	authHandler := auth.NewHandler(authCfg, log)
	srv := ui.NewServer(kc, scheme, authHandler, log)

	log.Info("starting tailgate-ui", "addr", *addr, "version", version, "commit", commit, "buildDate", buildDate)
	if err := listenAndServe(*addr, srv.Handler()); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func splitComma(s string) []string {
	var out []string
	for _, v := range splitStr(s, ",") {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func splitStr(s, sep string) []string {
	var out []string
	rest := s
	for {
		i := indexOf(rest, sep)
		if i < 0 {
			out = append(out, rest)
			break
		}
		out = append(out, rest[:i])
		rest = rest[i+len(sep):]
	}
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
