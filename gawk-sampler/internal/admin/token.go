// Package admin is the sampler's side of gawk-admin's flagger API (docs/71
// D2, D9, D20): the shard's target list, content-flag POSTs and sample
// batches, authenticated by an OIDC client-credentials token whose client
// carries only `flagger` (D18).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource fetches and caches a client-credentials access token. The token
// endpoint comes from the issuer's discovery document, so a Keycloak realm
// and the dev stack's fake IdP need no extra knob.
type TokenSource struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scope, when non-empty, is requested with the grant.
	Scope  string
	Client *http.Client
	// Now defaults to time.Now.
	Now func() time.Time

	mu       sync.Mutex
	tokenURL string
	token    string
	expiry   time.Time
}

// tokenSlack is how long before expiry a token is renewed, so a request never
// leaves with a token that dies in flight.
const tokenSlack = 30 * time.Second

func (s *TokenSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Token returns a valid access token, fetching one when the cached token is
// missing or near expiry.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expiry) {
		return s.token, nil
	}
	if s.tokenURL == "" {
		u, err := s.discover(ctx)
		if err != nil {
			return "", err
		}
		s.tokenURL = u
	}
	// client_secret_post: the credentials ride in the form. Basic would need
	// them form-encoded first (RFC 6749 §2.3.1), which IdPs disagree on
	// decoding; the form has one encoding everyone reads the same way.
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.ClientID},
		"client_secret": {s.ClientSecret},
	}
	if s.Scope != "" {
		form.Set("scope", s.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("admin: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The body is the IdP's error object; it names no secret.
		return "", fmt.Errorf("admin: token request: status %d: %s", resp.StatusCode, oneLine(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("admin: token response carries no access_token")
	}
	life := time.Duration(tok.ExpiresIn) * time.Second
	if life <= 0 {
		life = time.Minute
	}
	slack := min(tokenSlack, life/2)
	s.token, s.expiry = tok.AccessToken, s.now().Add(life-slack)
	return s.token, nil
}

// Invalidate drops the cached token, after gawk-admin refused it.
func (s *TokenSource) Invalidate() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}

func (s *TokenSource) discover(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("admin: oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("admin: oidc discovery: status %d", resp.StatusCode)
	}
	var doc struct {
		Issuer        string `json:"issuer"`
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("admin: oidc discovery: %w", err)
	}
	if strings.TrimRight(doc.Issuer, "/") != s.Issuer {
		return "", fmt.Errorf("admin: oidc discovery: issuer %q, want %q", doc.Issuer, s.Issuer)
	}
	u, err := url.Parse(doc.TokenEndpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("admin: oidc discovery: bad token_endpoint %q", doc.TokenEndpoint)
	}
	return doc.TokenEndpoint, nil
}

func oneLine(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
