// Package tailnet drives the Tailscale org API to create and destroy throwaway
// ("ephemeral") tailnets for tests. Verified recipe (against api.tailscale.com,
// 2026-05-29):
//
//	create:  POST /api/v2/organizations/-/tailnets        (ORG oauth token)
//	         -> {id, dnsName, oauthClient:{id,secret}}     (a per-tailnet client)
//	acl/keys POST /api/v2/tailnet/<dnsName>/{acl,keys}     (CHILD oauth token)
//	delete:  DELETE /api/v2/tailnet/<dnsName>              (CHILD oauth token)
//
// The org token can create+list but CANNOT delete a child tailnet ("not a
// member"); deletion requires a token minted from the child oauthClient that the
// create call returns. Keep those child creds for teardown or the tailnet leaks.
package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	tsapi "tailscale.com/client/tailscale/v2"
)

const apiBase = "https://api.tailscale.com/api/v2"

// Client holds the org-level OAuth credentials used to create tailnets.
type Client struct {
	orgID, orgSecret string
	hc               *http.Client
}

// New returns a Client authenticated by an org OAuth client (id+secret) that has
// the tailnet-create capability.
func New(orgClientID, orgSecret string) *Client {
	return &Client{orgID: orgClientID, orgSecret: orgSecret, hc: &http.Client{Timeout: 30 * time.Second}}
}

// Ephemeral is a throwaway tailnet. Close() deletes it. ClientID/ClientSecret are
// the per-tailnet OAuth client the create call returned — use them as the operator's
// OAuth creds (mint authkeys) and to delete.
type Ephemeral struct {
	c            *Client
	ID           string
	DNSName      string
	DisplayName  string
	ClientID     string
	ClientSecret string
	ts           *tsapi.Client // lazily built child client (ACL + keys)
}

// api returns the official client scoped to this tailnet, authed by its child OAuth
// client. Used for the in-tailnet operations the v2 client supports (ACL, keys); the
// org-level create/delete below stay hand-rolled because the client has no tenant API.
func (e *Ephemeral) api() *tsapi.Client {
	if e.ts == nil {
		e.ts = &tsapi.Client{
			Tailnet: e.DNSName,
			Auth:    &tsapi.OAuth{ClientID: e.ClientID, ClientSecret: e.ClientSecret},
		}
	}
	return e.ts
}

// oauthToken exchanges OAuth client credentials for an access token.
func (c *Client) oauthToken(ctx context.Context, clientID, secret string) (string, error) {
	form := url.Values{"client_id": {clientID}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.do(req, &out); err != nil {
		return "", fmt.Errorf("oauth token: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("oauth token: empty access_token")
	}
	return out.AccessToken, nil
}

// Create provisions a fresh tailnet. name must match ^[a-zA-Z0-9' -]+$.
func (c *Client) Create(ctx context.Context, name string) (*Ephemeral, error) {
	tok, err := c.oauthToken(ctx, c.orgID, c.orgSecret)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"displayName": name, "tailnetName": name})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/organizations/-/tailnets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		ID          string `json:"id"`
		DNSName     string `json:"dnsName"`
		DisplayName string `json:"displayName"`
		OAuthClient struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		} `json:"oauthClient"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, fmt.Errorf("create tailnet %q: %w", name, err)
	}
	if out.DNSName == "" || out.OAuthClient.Secret == "" {
		return nil, fmt.Errorf("create tailnet %q: missing dnsName/oauthClient in response", name)
	}
	return &Ephemeral{
		c: c, ID: out.ID, DNSName: out.DNSName, DisplayName: out.DisplayName,
		ClientID: out.OAuthClient.ID, ClientSecret: out.OAuthClient.Secret,
	}, nil
}

// childToken mints an access token scoped to this tailnet (admin of itself).
func (e *Ephemeral) childToken(ctx context.Context) (string, error) {
	return e.c.oauthToken(ctx, e.ClientID, e.ClientSecret)
}

// ApplyACL sets the tailnet policy file (HuJSON). Needed so tags exist before keys.
func (e *Ephemeral) ApplyACL(ctx context.Context, hujson []byte) error {
	if err := e.api().PolicyFile().Set(ctx, string(hujson), ""); err != nil {
		return fmt.Errorf("apply acl: %w", err)
	}
	return nil
}

// MintAuthKey creates an ephemeral, preauthorized, tagged auth key (for the test peer).
func (e *Ephemeral) MintAuthKey(ctx context.Context, tags []string) (string, error) {
	var req tsapi.CreateKeyRequest
	req.Capabilities.Devices.Create.Ephemeral = true
	req.Capabilities.Devices.Create.Preauthorized = true
	req.Capabilities.Devices.Create.Tags = tags
	req.ExpirySeconds = 3600
	key, err := e.api().Keys().CreateAuthKey(ctx, req)
	if err != nil {
		return "", fmt.Errorf("mint authkey: %w", err)
	}
	if key.Key == "" {
		return "", fmt.Errorf("mint authkey: empty key")
	}
	return key.Key, nil
}

// Close deletes the tailnet (idempotent-ish: a 404 is treated as already gone).
func (e *Ephemeral) Close(ctx context.Context) error {
	tok, err := e.childToken(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, apiBase+"/tailnet/"+url.PathEscape(e.DNSName), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if err := e.c.do(req, nil); err != nil {
		return fmt.Errorf("delete tailnet %q: %w", e.DNSName, err)
	}
	return nil
}

// do sends req, decodes a 2xx JSON body into out (if non-nil), else returns an error
// carrying the status + body.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("decode %s: %w", req.URL.Path, err)
		}
	}
	return nil
}
