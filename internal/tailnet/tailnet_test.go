package tailnet

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// loadEnv reads ../../.env (code/.env) into the process env if present, so the
// test can find TS_ORG_OAUTH_CLIENT_ID/SECRET locally without exporting them.
func loadEnv(t *testing.T) {
	t.Helper()
	for _, p := range []string{".env", "../../.env"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if ok && os.Getenv(strings.TrimSpace(k)) == "" {
				os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
		f.Close()
	}
}

// TestLifecycle creates a real throwaway tailnet, applies an ACL, mints an authkey,
// and deletes it — exercising the full verified create+destroy recipe. Skips if no
// org creds are available. defer Close guarantees no leak even on failure.
func TestLifecycle(t *testing.T) {
	loadEnv(t)
	id, sec := os.Getenv("TS_ORG_OAUTH_CLIENT_ID"), os.Getenv("TS_ORG_OAUTH_CLIENT_SECRET")
	if id == "" || sec == "" {
		t.Skip("TS_ORG_OAUTH_CLIENT_ID/SECRET not set (code/.env) — skipping live tailnet lifecycle test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c := New(id, sec)
	name := "tailgate-selftest-" + time.Now().UTC().Format("150405")
	eg, err := c.Create(ctx, name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created tailnet %s (dnsName=%s, childClient=%s…)", eg.DisplayName, eg.DNSName, eg.ClientID[:6])
	// Guarantee teardown no matter what fails below.
	defer func() {
		if err := eg.Close(context.Background()); err != nil {
			t.Errorf("CLOSE FAILED — leaked tailnet %s: %v", eg.DNSName, err)
		} else {
			t.Logf("deleted tailnet %s", eg.DNSName)
		}
	}()

	acl := []byte(`{
  "tagOwners": { "tag:ci": ["autogroup:admin"], "tag:k8s": ["autogroup:admin"] },
  "acls": [ { "action": "accept", "src": ["*"], "dst": ["*:*"] } ]
}`)
	if err := eg.ApplyACL(ctx, acl); err != nil {
		t.Fatalf("apply acl: %v", err)
	}
	key, err := eg.MintAuthKey(ctx, []string{"tag:ci"})
	if err != nil {
		t.Fatalf("mint authkey: %v", err)
	}
	if !strings.HasPrefix(key, "tskey-") {
		t.Fatalf("authkey has unexpected prefix: %q", key[:min(10, len(key))])
	}
	t.Logf("minted authkey (%d chars, prefix %s…)", len(key), key[:10])
}
