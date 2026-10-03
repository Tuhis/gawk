package oidcauth_test

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
)

// RFC 9728 §3.3: a client discards a metadata document whose `resource` is not
// the identifier it derived the metadata URL from — so the derivation must
// reproduce the URL the client actually called, including behind a
// TLS-terminating Ingress (docs/55 D6).
func TestRequestResourceURLReproducesTheCalledURL(t *testing.T) {
	derive := oidcauth.RequestResourceURL("/mcp")
	for _, tc := range []struct {
		name  string
		host  string
		proto string // X-Forwarded-Proto; "" = absent
		tls   bool
		want  string
	}{
		{"ingress terminating TLS", "gawk-telemetry.kube.example", "https", false, "https://gawk-telemetry.kube.example/mcp"},
		{"same host, no header", "gawk-telemetry.kube.example", "", false, "http://gawk-telemetry.kube.example/mcp"},
		{"port-forward", "localhost:8081", "", false, "http://localhost:8081/mcp"},
		{"first value of a proxy chain", "h.example", "https, http", false, "https://h.example/mcp"},
		{"header case is folded", "h.example", "HTTPS", false, "https://h.example/mcp"},
		{"direct TLS", "h.example", "", true, "https://h.example/mcp"},
		{"an unusable header falls back to the connection", "h.example", "gopher", true, "https://h.example/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.Host = tc.host
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			} else {
				r.TLS = nil
			}
			if got := derive(r); got != tc.want {
				t.Errorf("resource = %q, want %q", got, tc.want)
			}
		})
	}
}

// RFC 9728 §3.1: the well-known segment is inserted between the host and the
// resource's path, never appended.
func TestProtectedResourceMetadataURLIsPathInserted(t *testing.T) {
	for in, want := range map[string]string{
		"https://h.example/mcp":         "https://h.example/.well-known/oauth-protected-resource/mcp",
		"http://localhost:8081/mcp":     "http://localhost:8081/.well-known/oauth-protected-resource/mcp",
		"https://h.example/admin/mcp":   "https://h.example/.well-known/oauth-protected-resource/admin/mcp",
		"https://h.example":             "https://h.example/.well-known/oauth-protected-resource",
		"https://h.example/mcp?x=1#top": "https://h.example/.well-known/oauth-protected-resource/mcp?x=1",
	} {
		if got := oidcauth.ProtectedResourceMetadataURL(in); got != want {
			t.Errorf("ProtectedResourceMetadataURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := oidcauth.ProtectedResourceMetadataPath("/mcp"); got != "/.well-known/oauth-protected-resource/mcp" {
		t.Errorf("ProtectedResourceMetadataPath(/mcp) = %q", got)
	}
}

func TestProtectedResourceHandlerServesTheMetadataDocument(t *testing.T) {
	const issuer = "https://id.example.test/realms/gawk"
	h := oidcauth.ProtectedResourceHandler(issuer, oidcauth.RequestResourceURL("/mcp"))

	r := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil)
	r.Host = "gawk-telemetry.kube.example"
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(doc) != 3 {
		t.Errorf("document carries %d fields, want exactly resource, authorization_servers, bearer_methods_supported: %v", len(doc), doc)
	}
	if doc["resource"] != "https://gawk-telemetry.kube.example/mcp" {
		t.Errorf("resource = %v", doc["resource"])
	}
	if as, _ := doc["authorization_servers"].([]any); len(as) != 1 || as[0] != issuer {
		t.Errorf("authorization_servers = %v, want exactly [%s]", doc["authorization_servers"], issuer)
	}
	if bm, _ := doc["bearer_methods_supported"].([]any); len(bm) != 1 || bm[0] != "header" {
		t.Errorf("bearer_methods_supported = %v, want [header]", doc["bearer_methods_supported"])
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("the metadata response skipped the security headers")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/.well-known/oauth-protected-resource/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

func TestProtectedResourceHandlerRefusesABlankIssuer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a metadata document naming no authorization server was built")
		}
	}()
	oidcauth.ProtectedResourceHandler(" ", oidcauth.RequestResourceURL("/mcp"))
}

// The MCP authorization spec: a 401 names the metadata document, so a client
// can discover the authorization server. Only 401s carry it — a 200 or a 403
// is not a challenge.
func TestChallengeAddsResourceMetadataToEvery401(t *testing.T) {
	status := http.StatusUnauthorized
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
	h := oidcauth.Challenge(oidcauth.RequestResourceURL("/mcp"))(inner)

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Host = "localhost:8081"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	want := `Bearer resource_metadata="http://localhost:8081/.well-known/oauth-protected-resource/mcp"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}

	// An implicit 200 (Write without WriteHeader) and an explicit 403 carry
	// no challenge.
	for _, s := range []int{0, http.StatusForbidden} {
		status = s
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte("x"))
		})
		rec := httptest.NewRecorder()
		oidcauth.Challenge(oidcauth.RequestResourceURL("/mcp"))(inner).ServeHTTP(rec, r)
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("status %d carried WWW-Authenticate %q", s, got)
		}
	}

	// With no resource, the challenge is the bare RFC 6750 scheme.
	status = http.StatusUnauthorized
	rec = httptest.NewRecorder()
	oidcauth.Challenge(nil)(inner).ServeHTTP(rec, r)
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate with no resource = %q, want Bearer", got)
	}
}

// The challenge wrapper must not hide the writer's Flusher: it sits in front
// of a streaming route.
func TestChallengeKeepsTheFlusher(t *testing.T) {
	var flushable bool
	h := oidcauth.Challenge(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flushable = w.(http.Flusher)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/live/stream", nil))
	if !flushable {
		t.Error("the wrapped writer is not an http.Flusher")
	}
}

// MCP's transport rules: a server must validate Origin to stop DNS rebinding.
// No browser client of an MCP endpoint exists here, so the header itself is
// the refusal (docs/55 D6, docs/56 D9).
func TestRefuseBrowserOrigin(t *testing.T) {
	reached := false
	h := oidcauth.RefuseBrowserOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	for _, origin := range []string{"https://evil.example", "http://localhost:8081", "null"} {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden || reached {
			t.Errorf("Origin %q: status %d, reached %v; want 403 before the handler", origin, rec.Code, reached)
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != oidcauth.CodeForbidden {
			t.Errorf("Origin %q: body %s, want the forbidden envelope", origin, rec.Body)
		}
	}
	reached = false
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusOK || !reached {
		t.Errorf("no Origin: status %d, reached %v; want the handler", rec.Code, reached)
	}
}
