// Package auth implements the Tailscale OAuth authorization-code flow for user
// identity. It is NOT the device-provisioning flow (which mints authkeys) — this
// is identity-only: we exchange the code for a token, call the Tailscale API to
// resolve the user's email, and issue a signed session cookie. No scopes are
// requested beyond what the consent screen requires; the token itself is never
// used after identity resolution.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	loginBase  = "https://login.tailscale.com"
	tokenURL   = "https://api.tailscale.com/api/v2/oauth/token"
	whoamiURL  = "https://api.tailscale.com/api/v2/tailnet/-/whoami"
	sessionMax = 24 * time.Hour
	cookieName = "tailgate-session"
)

// Config holds the OAuth app credentials and the redirect URL.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// SessionKey is the HMAC key for signing session cookies. Must be 32+ bytes.
	SessionKey []byte
	// AdminEmails is the allowlist of tailnet admins who can see all groups.
	// Optional; when empty only group owners can manage their groups.
	AdminEmails []string
}

// Session is the authenticated user's session, carried in a signed cookie.
type Session struct {
	Email     string `json:"email"`
	LoginName string `json:"login_name"`
	Expires   int64  `json:"exp"`
}

// Handler implements the OAuth flow: /login redirects to Tailscale, /callback
// exchanges the code and issues a session cookie. It is an http.Handler that
// mounts those two routes; wrap it in a mux or mount directly.
type Handler struct {
	cfg Config
	log *slog.Logger
}

// NewHandler returns an OAuth handler with the given config.
func NewHandler(cfg Config, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{cfg: cfg, log: log}
}

// LoginURL builds the Tailscale authorization URL for the given state.
func (h *Handler) LoginURL(state string) string {
	q := url.Values{
		"client_id":     {h.cfg.ClientID},
		"redirect_uri":  {h.cfg.RedirectURL},
		"response_type": {"code"},
		"state":         {state},
		"scope":         {"auth_keys:create:once"},
	}
	return loginBase + "/a/oauth_authorize?" + q.Encode()
}

// ServeHTTP dispatches /login and /callback.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/login":
		h.handleLogin(w, r)
	case "/callback":
		h.handleCallback(w, r)
	case "/logout":
		h.handleLogout(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := h.signState()
	http.Redirect(w, r, h.LoginURL(state), http.StatusFound)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.URL.Query().Get("error"); err != "" {
		http.Error(w, "auth denied: "+err, http.StatusForbidden)
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}
	if !h.verifyState(state) {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}

	tok, err := h.exchangeCode(r.Context(), code)
	if err != nil {
		h.log.Error("token exchange", "err", err)
		http.Error(w, "auth failed", http.StatusBadGateway)
		return
	}

	email, loginName, err := h.whoami(r.Context(), tok)
	if err != nil {
		h.log.Error("whoami", "err", err)
		http.Error(w, "identity resolution failed", http.StatusBadGateway)
		return
	}

	sess := Session{
		Email:     email,
		LoginName: loginName,
		Expires:   time.Now().Add(sessionMax).Unix(),
	}
	cookie, err := h.signSession(sess)
	if err != nil {
		h.log.Error("sign session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    cookie,
		Path:     "/",
		MaxAge:   int(sessionMax.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.cfg.ClientID},
		"client_secret": {h.cfg.ClientSecret},
		"redirect_uri":  {h.cfg.RedirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token exchange: %s: %s", resp.Status, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("empty access token")
	}
	return out.AccessToken, nil
}

func (h *Handler) whoami(ctx context.Context, token string) (email, loginName string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, whoamiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("whoami: %s: %s", resp.Status, body)
	}
	var out struct {
		LoginName string `json:"login_name"`
		Email     string `json:"email"`
		User      struct {
			LoginName string `json:"loginName"`
			Profile   struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	email = out.Email
	if email == "" {
		email = out.User.Profile.Email
	}
	loginName = out.LoginName
	if loginName == "" {
		loginName = out.User.LoginName
	}
	if email == "" {
		email = loginName
	}
	if email == "" {
		return "", "", fmt.Errorf("no email in whoami response")
	}
	return email, loginName, nil
}

// signSession creates a signed cookie value: base64(json).hmac.
func (h *Handler) signSession(s Session) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, h.cfg.SessionKey)
	mac.Write([]byte(b64))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return b64 + "." + sig, nil
}

// verifySession validates a cookie value and returns the session.
func verifySession(raw string, key []byte) (Session, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return Session{}, fmt.Errorf("malformed session")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return Session{}, fmt.Errorf("invalid signature")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, err
	}
	if time.Now().Unix() > s.Expires {
		return Session{}, fmt.Errorf("session expired")
	}
	return s, nil
}

// Middleware extracts the session from the cookie and puts it in context.
// Unauthenticated requests are redirected to /login.
func (h *Handler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		sess, err := verifySession(cookie.Value, h.cfg.SessionKey)
		if err != nil {
			h.log.Debug("session invalid", "err", err)
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		r = r.WithContext(WithSession(r.Context(), &sess))
		next.ServeHTTP(w, r)
	})
}

// SessionFromContext returns the session from context, or nil.
func SessionFromContext(ctx context.Context) *Session {
	if v, ok := ctx.Value(sessionKey{}).(*Session); ok {
		return v
	}
	return nil
}

// IsAdmin reports whether the session user is in the admin allowlist.
func (h *Handler) IsAdmin(s *Session) bool {
	if s == nil {
		return false
	}
	for _, e := range h.cfg.AdminEmails {
		if strings.EqualFold(e, s.Email) {
			return true
		}
	}
	return false
}

type sessionKey struct{}

func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// signState creates a self-verifying OAuth state token: random.hmac(random).time
// Any pod with the same session key can verify it — no shared state store needed
// (the UI runs with 2+ replicas, so an in-memory cache would fail when the
// callback hits a different pod than the one that issued the state).
func (h *Handler) signState() string {
	nonce := randomToken()
	mac := hmac.New(sha256.New, h.cfg.SessionKey)
	mac.Write([]byte(nonce))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return nonce + "." + sig
}

// verifyState validates a self-verifying state token issued by signState.
func (h *Handler) verifyState(state string) bool {
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return false
	}
	mac := hmac.New(sha256.New, h.cfg.SessionKey)
	mac.Write([]byte(parts[0]))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(parts[1]), []byte(expected))
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
