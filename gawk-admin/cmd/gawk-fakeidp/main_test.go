package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testIDP(t *testing.T) *idp {
	t.Helper()
	i, err := newIDP(idpConfig{
		Issuer:   "http://localhost:8088/idp",
		ClientID: "gawk-admin-spa",
		Audience: "gawk-admin",
		Role:     "operator",
		Subject:  "dev",
		Email:    "dev@e2e.invalid",
		Lifetime: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("newIDP: %v", err)
	}
	return i
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func postForm(t *testing.T, h http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func claimsOf(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", jwt)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	return m
}

// The whole flow the portal SPA drives, in the order it drives it: discovery,
// authorize (auto-approved, code + state echoed to the redirect URI), the
// PKCE code exchange, the id_token nonce, and refresh with rotation.
func TestAuthorizationCodeFlowEndToEnd(t *testing.T) {
	i := testIDP(t)

	disc := get(t, i, "/.well-known/openid-configuration")
	var doc struct {
		Issuer  string `json:"issuer"`
		AuthEP  string `json:"authorization_endpoint"`
		TokenEP string `json:"token_endpoint"`
		JWKS    string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(disc.Body.Bytes(), &doc); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if doc.Issuer != "http://localhost:8088/idp" || !strings.HasPrefix(doc.AuthEP, doc.Issuer) ||
		!strings.HasPrefix(doc.TokenEP, doc.Issuer) || !strings.HasPrefix(doc.JWKS, doc.Issuer) {
		t.Fatalf("discovery URLs are not issuer-based: %+v", doc)
	}

	const verifier = "the-spa-generated-verifier"
	auth := get(t, i, "/authorize?"+url.Values{
		"response_type":         {"code"},
		"client_id":             {"gawk-admin-spa"},
		"redirect_uri":          {"http://localhost:8088/"},
		"scope":                 {"openid profile email"},
		"state":                 {"st-1"},
		"nonce":                 {"n-1"},
		"code_challenge":        {s256(verifier)},
		"code_challenge_method": {"S256"},
		"audience":              {"gawk-admin"}, // the RFC 6749 §3.1 extension param: ignored
	}.Encode())
	if auth.Code != http.StatusFound {
		t.Fatalf("authorize = %d (%s), want 302", auth.Code, auth.Body.String())
	}
	back, err := url.Parse(auth.Header().Get("Location"))
	if err != nil {
		t.Fatalf("redirect location: %v", err)
	}
	if back.Query().Get("state") != "st-1" {
		t.Fatalf("state not echoed: %s", back)
	}
	code := back.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in the redirect: %s", back)
	}

	// A wrong verifier is refused, and it SPENDS the code (single use).
	bad := postForm(t, i, "/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {"http://localhost:8088/"}, "client_id": {"gawk-admin-spa"},
		"code_verifier": {"not-the-verifier"},
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("wrong verifier = %d, want 400", bad.Code)
	}
	spent := postForm(t, i, "/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {"http://localhost:8088/"}, "client_id": {"gawk-admin-spa"},
		"code_verifier": {verifier},
	})
	if spent.Code != http.StatusBadRequest {
		t.Fatalf("a spent code exchanged = %d, want 400", spent.Code)
	}

	// A fresh authorization, exchanged correctly.
	auth2 := get(t, i, "/authorize?"+url.Values{
		"response_type": {"code"}, "client_id": {"gawk-admin-spa"},
		"redirect_uri": {"http://localhost:8088/"}, "state": {"st-2"}, "nonce": {"n-2"},
		"code_challenge": {s256(verifier)}, "code_challenge_method": {"S256"},
	}.Encode())
	back2, _ := url.Parse(auth2.Header().Get("Location"))
	ok := postForm(t, i, "/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {back2.Query().Get("code")},
		"redirect_uri": {"http://localhost:8088/"}, "client_id": {"gawk-admin-spa"},
		"code_verifier": {verifier},
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("exchange = %d (%s), want 200", ok.Code, ok.Body.String())
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &tokens); err != nil {
		t.Fatalf("token response: %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.ExpiresIn <= 0 {
		t.Fatalf("token response incomplete: %+v", tokens)
	}

	// The access token carries the shape gawk-admin authorizes on.
	ac := claimsOf(t, tokens.AccessToken)
	if ac["iss"] != "http://localhost:8088/idp" || ac["aud"] != "gawk-admin" {
		t.Fatalf("access token iss/aud = %v/%v", ac["iss"], ac["aud"])
	}
	ra := ac["resource_access"].(map[string]any)["gawk-admin"].(map[string]any)
	if roles := ra["roles"].([]any); len(roles) != 1 || roles[0] != "operator" {
		t.Fatalf("roles = %v, want [operator]", ra["roles"])
	}
	// The id_token carries the nonce the SPA checks.
	if got := claimsOf(t, tokens.IDToken)["nonce"]; got != "n-2" {
		t.Fatalf("id_token nonce = %v, want n-2", got)
	}

	// Refresh rotates: the new pair works, the old refresh token is dead.
	ref := postForm(t, i, "/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken},
		"client_id": {"gawk-admin-spa"}, "scope": {"openid profile email"},
	})
	if ref.Code != http.StatusOK {
		t.Fatalf("refresh = %d (%s), want 200", ref.Code, ref.Body.String())
	}
	replay := postForm(t, i, "/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken},
		"client_id": {"gawk-admin-spa"},
	})
	if replay.Code != http.StatusBadRequest {
		t.Fatalf("a rotated refresh token replayed = %d, want 400", replay.Code)
	}
}

// The strictness that keeps the SPA honest: no PKCE, no code.
func TestAuthorizeRequiresPKCEAndTheClient(t *testing.T) {
	i := testIDP(t)
	noPKCE := get(t, i, "/authorize?"+url.Values{
		"response_type": {"code"}, "client_id": {"gawk-admin-spa"},
		"redirect_uri": {"http://localhost:8088/"},
	}.Encode())
	if noPKCE.Code != http.StatusBadRequest {
		t.Fatalf("authorize without PKCE = %d, want 400", noPKCE.Code)
	}
	wrongClient := get(t, i, "/authorize?"+url.Values{
		"response_type": {"code"}, "client_id": {"someone-else"},
		"redirect_uri":   {"http://localhost:8088/"},
		"code_challenge": {s256("v")}, "code_challenge_method": {"S256"},
	}.Encode())
	if wrongClient.Code != http.StatusBadRequest {
		t.Fatalf("authorize with a wrong client = %d, want 400", wrongClient.Code)
	}
}

// /mint stays: the script door for admin-assert.sh and stack_test.sh.
func TestMintIssuesAnOperatorToken(t *testing.T) {
	i := testIDP(t)
	rec := postForm(t, i, "/mint", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d", rec.Code)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
		t.Fatalf("mint response: %v (%s)", err, rec.Body.String())
	}
	if aud := claimsOf(t, out.AccessToken)["aud"]; aud != "gawk-admin" {
		t.Fatalf("minted aud = %v", aud)
	}
}
