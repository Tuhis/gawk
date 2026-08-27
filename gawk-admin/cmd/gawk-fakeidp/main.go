// Command gawk-fakeidp is a TEST-ONLY OIDC issuer for the docs/41 compose
// lane and the kind e2e tier: discovery, a JWKS, a real authorization-code +
// PKCE flow the portal SPA can drive from a browser, refresh-token rotation
// for its silent renew, and a /mint endpoint for scripts.
//
// It auto-approves every authorization request as one fixed operator — there
// is no login page, no consent and no user database, which is exactly what
// makes it TEST ONLY: anyone who can reach it is the operator. It is NOT part
// of the gawk-admin image (deploy/Dockerfile builds only ./cmd/gawk-admin)
// and must never be; the dev stack and the e2e job build it into their own
// throwaway image (dev/Dockerfile.fakeidp).
//
// -issuer is the URL the discovery document advertises, every endpoint URL is
// built on, and tokens carry as `iss`. In the compose lane it is the portal's
// own /idp path (gawk-admin's -dev-oidc-proxy), so ONE URL reaches this
// server from the developer's browser and from the gawk-admin container.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

func main() {
	var (
		addr     = flag.String("addr", ":8080", "listen address")
		issuer   = flag.String("issuer", "", "the issuer URL this IdP advertises and stamps into `iss` (required)")
		clientID = flag.String("client-id", "gawk-admin-spa", "the public client /authorize accepts")
		audience = flag.String("audience", "gawk-admin", "the `aud` minted tokens carry")
		role     = flag.String("role", "operator", "the role minted tokens carry at resource_access.<audience>.roles")
		subject  = flag.String("subject", "e2e-operator", "the `sub` minted tokens carry")
		email    = flag.String("email", "operator@e2e.invalid", "the `email` minted tokens carry")
		lifetime = flag.Duration("lifetime", 15*time.Minute, "access-token lifetime")
	)
	flag.Parse()
	if *issuer == "" {
		log.Fatal("gawk-fakeidp: -issuer is required")
	}

	idp, err := newIDP(idpConfig{
		Issuer:   *issuer,
		ClientID: *clientID,
		Audience: *audience,
		Role:     *role,
		Subject:  *subject,
		Email:    *email,
		Lifetime: *lifetime,
	})
	if err != nil {
		log.Fatalf("gawk-fakeidp: %v", err)
	}
	log.Printf("gawk-fakeidp: serving issuer %s on %s (TEST ONLY — auto-approves and mints for anyone who asks)", *issuer, *addr)
	log.Fatal(http.ListenAndServe(*addr, idp))
}

type idpConfig struct {
	Issuer   string
	ClientID string
	Audience string
	Role     string
	Subject  string
	Email    string
	Lifetime time.Duration
	// Now is the clock; nil means time.Now. A test seam.
	Now func() time.Time
}

const keyID = "fakeidp-key"

// codeRecord is one outstanding authorization code — single use, PKCE-bound.
type codeRecord struct {
	challenge   string
	nonce       string
	redirectURI string
	expires     time.Time
}

type idp struct {
	cfg  idpConfig
	key  *rsa.PrivateKey
	jwks []byte
	mux  *http.ServeMux

	mu      sync.Mutex
	codes   map[string]codeRecord
	refresh map[string]bool
}

func newIDP(cfg idpConfig) (*idp, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Lifetime <= 0 {
		cfg.Lifetime = 15 * time.Minute
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	i := &idp{
		cfg:     cfg,
		key:     key,
		codes:   map[string]codeRecord{},
		refresh: map[string]bool{},
	}
	i.jwks, err = marshalJWKS(&key.PublicKey)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET /keys", i.keys)
	mux.HandleFunc("GET /authorize", i.authorize)
	mux.HandleFunc("POST /token", i.token)
	mux.HandleFunc("POST /mint", i.mint)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	i.mux = mux
	return i, nil
}

func (i *idp) ServeHTTP(w http.ResponseWriter, r *http.Request) { i.mux.ServeHTTP(w, r) }

// discovery is the document both consumers read: the SPA (from the browser)
// for authorization_endpoint/token_endpoint, gawk-admin (server-side) for the
// issuer match and jwks_uri. Every URL is issuer-based, which is what the
// dev-oidc-proxy makes valid in both worlds. end_session_endpoint is
// deliberately absent: this IdP keeps no session, so a portal Sign out has
// nothing to end — the SPA treats that as documented (§4.8).
func (i *idp) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                i.cfg.Issuer,
		"authorization_endpoint":                i.cfg.Issuer + "/authorize",
		"token_endpoint":                        i.cfg.Issuer + "/token",
		"jwks_uri":                              i.cfg.Issuer + "/keys",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"subject_types_supported":               []string{"public"},
	})
}

func (i *idp) keys(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(i.jwks)
}

// authorize auto-approves: no login page, no consent — the request IS the
// user. PKCE is REQUIRED even here, so the SPA's flow is exercised as a
// strict provider would, not as a lenient one happens to allow.
func (i *idp) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" {
		http.Error(w, "response_type must be code", http.StatusBadRequest)
		return
	}
	if got := q.Get("client_id"); got != i.cfg.ClientID {
		http.Error(w, fmt.Sprintf("unknown client_id %q (this IdP serves %q)", got, i.cfg.ClientID), http.StatusBadRequest)
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "redirect_uri is required", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "PKCE (S256) is required", http.StatusBadRequest)
		return
	}

	code := randomToken()
	i.mu.Lock()
	i.codes[code] = codeRecord{
		challenge:   q.Get("code_challenge"),
		nonce:       q.Get("nonce"),
		redirectURI: redirectURI,
		expires:     i.cfg.Now().Add(5 * time.Minute),
	}
	i.mu.Unlock()

	back, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "redirect_uri is not a URL", http.StatusBadRequest)
		return
	}
	bq := back.Query()
	bq.Set("code", code)
	if state := q.Get("state"); state != "" {
		bq.Set("state", state)
	}
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// token serves both grants the SPA uses: the PKCE code exchange, and refresh
// with rotation (the old refresh token dies on use — the strict-provider
// behaviour §4.8 asks deployments to enable, so the SPA's replace-on-renew
// path is what gets exercised).
func (i *idp) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request", err.Error())
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code := r.PostForm.Get("code")
		i.mu.Lock()
		rec, ok := i.codes[code]
		delete(i.codes, code) // single use, spent even on failure
		i.mu.Unlock()
		if !ok || i.cfg.Now().After(rec.expires) {
			tokenError(w, "invalid_grant", "unknown or expired code")
			return
		}
		if rec.redirectURI != r.PostForm.Get("redirect_uri") {
			tokenError(w, "invalid_grant", "redirect_uri does not match the authorization request")
			return
		}
		if s256(r.PostForm.Get("code_verifier")) != rec.challenge {
			tokenError(w, "invalid_grant", "PKCE verification failed")
			return
		}
		i.respondTokens(w, rec.nonce)
	case "refresh_token":
		token := r.PostForm.Get("refresh_token")
		i.mu.Lock()
		ok := i.refresh[token]
		delete(i.refresh, token) // rotation: the old one dies on use
		i.mu.Unlock()
		if !ok {
			tokenError(w, "invalid_grant", "unknown, rotated or revoked refresh token")
			return
		}
		i.respondTokens(w, "")
	default:
		tokenError(w, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (i *idp) respondTokens(w http.ResponseWriter, nonce string) {
	refresh := randomToken()
	i.mu.Lock()
	i.refresh[refresh] = true
	i.mu.Unlock()

	resp := map[string]any{
		"access_token":  i.signAccessToken(),
		"token_type":    "Bearer",
		"expires_in":    int(i.cfg.Lifetime.Seconds()),
		"refresh_token": refresh,
	}
	// An id_token only where an authorization request supplied a nonce to
	// bind it to — which is also what exercises the SPA's nonce check.
	if nonce != "" {
		resp["id_token"] = i.signIDToken(nonce)
	}
	writeJSON(w, resp)
}

// mint is the script door (e2e/admin-assert.sh, dev/stack_test.sh): one POST,
// one operator access token, no browser required.
func (i *idp) mint(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"access_token": i.signAccessToken()})
}

func (i *idp) signAccessToken() string {
	now := i.cfg.Now()
	return i.sign(map[string]any{
		"iss":   i.cfg.Issuer,
		"aud":   i.cfg.Audience,
		"sub":   i.cfg.Subject,
		"email": i.cfg.Email,
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(i.cfg.Lifetime).Unix(),
		"resource_access": map[string]any{
			i.cfg.Audience: map[string]any{"roles": []any{i.cfg.Role}},
		},
	})
}

func (i *idp) signIDToken(nonce string) string {
	now := i.cfg.Now()
	return i.sign(map[string]any{
		"iss":   i.cfg.Issuer,
		"aud":   i.cfg.ClientID,
		"sub":   i.cfg.Subject,
		"email": i.cfg.Email,
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(i.cfg.Lifetime).Unix(),
		"nonce": nonce,
	})
}

func (i *idp) sign(claims map[string]any) string {
	raw, err := json.Marshal(claims)
	if err != nil {
		panic(err) // a map[string]any of scalars cannot fail to marshal
	}
	return oidctest.SignIDToken(i.key, keyID, oidc.RS256, string(raw))
}

// marshalJWKS renders the one signing key as a JWK Set.
func marshalJWKS(pub *rsa.PublicKey) ([]byte, error) {
	return json.Marshal(map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": keyID,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func tokenError(w http.ResponseWriter, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
