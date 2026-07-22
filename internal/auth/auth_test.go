package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestSignVerifySession(t *testing.T) {
	key := []byte("test-key-that-is-long-enough-32b")
	h := &Handler{cfg: Config{SessionKey: key}}
	sess := Session{Email: "alice@example.com", LoginName: "alice", Expires: time.Now().Add(time.Hour).Unix()}
	raw, err := h.signSession(sess)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := verifySession(raw, key)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Email != sess.Email {
		t.Fatalf("email = %q, want %q", got.Email, sess.Email)
	}
}

func TestVerifySessionRejectsTampered(t *testing.T) {
	key := []byte("test-key-that-is-long-enough-32b")
	h := &Handler{cfg: Config{SessionKey: key}}
	sess := Session{Email: "alice@example.com", Expires: time.Now().Add(time.Hour).Unix()}
	raw, err := h.signSession(sess)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tampered := raw[:len(raw)-4] + "AAAA"
	if _, err := verifySession(tampered, key); err == nil {
		t.Fatal("expected error for tampered session")
	}
}

func TestVerifySessionRejectsWrongKey(t *testing.T) {
	h := &Handler{cfg: Config{SessionKey: []byte("test-key-that-is-long-enough-32b")}}
	sess := Session{Email: "alice@example.com", Expires: time.Now().Add(time.Hour).Unix()}
	raw, _ := h.signSession(sess)
	if _, err := verifySession(raw, []byte("different-key-that-is-long-enough!!")); err == nil {
		t.Fatal("expected error for wrong key")
	}
}

func TestVerifySessionRejectsExpired(t *testing.T) {
	key := []byte("test-key-that-is-long-enough-32b")
	h := &Handler{cfg: Config{SessionKey: key}}
	sess := Session{Email: "alice@example.com", Expires: time.Now().Add(-time.Hour).Unix()}
	raw, _ := h.signSession(sess)
	if _, err := verifySession(raw, key); err == nil {
		t.Fatal("expected error for expired session")
	}
}

func TestIsAdmin(t *testing.T) {
	h := &Handler{cfg: Config{AdminEmails: []string{"admin@example.com"}}}
	if !h.IsAdmin(&Session{Email: "admin@example.com"}) {
		t.Fatal("admin should be detected")
	}
	if h.IsAdmin(&Session{Email: "user@example.com"}) {
		t.Fatal("non-admin should not be detected")
	}
	if h.IsAdmin(nil) {
		t.Fatal("nil session should not be admin")
	}
}

func TestStateCache(t *testing.T) {
	c := newStateCache()
	c.put("abc")
	if !c.take("abc") {
		t.Fatal("first take should succeed")
	}
	if c.take("abc") {
		t.Fatal("second take should fail (consumed)")
	}
	if c.take("never-existed") {
		t.Fatal("nonexistent state should fail")
	}
}

func TestSessionContext(t *testing.T) {
	ctx := context.Background()
	if SessionFromContext(ctx) != nil {
		t.Fatal("expected nil session from empty context")
	}
	sess := &Session{Email: "test@example.com"}
	ctx = WithSession(ctx, sess)
	if SessionFromContext(ctx) != sess {
		t.Fatal("expected session from context")
	}
}

func TestLoginURL(t *testing.T) {
	h := &Handler{cfg: Config{
		ClientID:    "app-test",
		RedirectURL: "https://ui.example.com/callback",
	}}
	u := h.LoginURL("xyz123")
	if !contains(u, "client_id=app-test") {
		t.Fatalf("missing client_id in %s", u)
	}
	if !contains(u, "state=xyz123") {
		t.Fatalf("missing state in %s", u)
	}
	if !contains(u, "response_type=code") {
		t.Fatalf("missing response_type in %s", u)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsInner(s, substr))
}

func containsInner(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRandomTokenUniqueness(t *testing.T) {
	a := randomToken()
	b := randomToken()
	if a == b {
		t.Fatal("random tokens should be unique")
	}
	if len(a) < 16 {
		t.Fatalf("token too short: %s", a)
	}
}

func TestHMAC(t *testing.T) {
	key := []byte("secret")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("data"))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if sig == "" {
		t.Fatal("empty signature")
	}
}
