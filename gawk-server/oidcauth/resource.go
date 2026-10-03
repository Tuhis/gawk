package oidcauth

// OAuth 2.0 Protected Resource Metadata (RFC 9728) and the bearer challenge
// that points at it — the resource-server half of the MCP authorization spec
// (protocol revision 2025-06-18), which is how an MCP client such as Claude
// Code discovers which authorization server to run the code+PKCE flow
// against (docs/55 D6).
//
// These are generic over WHERE the resource lives: each helper takes a
// function from the request to the resource's absolute URL. gawk-telemetry
// has no external-URL knob and is reached both at its Ingress host and on a
// port-forward, so it derives the URL from the request (RequestResourceURL);
// gawk-admin's /mcp (docs/56) has -external-url and passes a fixed one.

import (
	"net/http"
	"net/url"
	"strings"
)

// ProtectedResourceWellKnown is RFC 9728's well-known URI suffix.
const ProtectedResourceWellKnown = "/.well-known/oauth-protected-resource"

// RequestResourceURL returns a function deriving the absolute URL of the
// resource at path from the request that reached it: scheme, Host, path.
//
// The scheme is the first value of X-Forwarded-Proto when that names http or
// https, else whether the connection itself is TLS. A TLS-terminating Ingress
// proxies plain HTTP to the pod, so without the header a client that called
// https://…/mcp would be told the resource is http://…/mcp — and RFC 9728
// §3.3 has it discard a document whose `resource` differs from the URL it
// called. Trusting the header is safe for the same reason deriving from Host
// is: the document's only authority-bearing field is the configured issuer,
// and the client checks the echoed `resource` against its own URL.
func RequestResourceURL(path string) func(*http.Request) string {
	return func(r *http.Request) string {
		return requestScheme(r) + "://" + r.Host + path
	}
}

func requestScheme(r *http.Request) string {
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		first, _, _ := strings.Cut(fp, ",")
		switch p := strings.ToLower(strings.TrimSpace(first)); p {
		case "http", "https":
			return p
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// ProtectedResourceMetadataPath is where the metadata for a resource at
// resourcePath is served on the same origin: the well-known segment inserted
// before the path (RFC 9728 §3.1), so /mcp's lives at
// /.well-known/oauth-protected-resource/mcp. A terminating slash is removed
// first, as §3.1 requires.
func ProtectedResourceMetadataPath(resourcePath string) string {
	return ProtectedResourceWellKnown + strings.TrimSuffix(resourcePath, "/")
}

// ProtectedResourceMetadataURL turns a resource URL into its metadata URL
// (RFC 9728 §3.1). The fragment is dropped; the query, if any, is kept.
func ProtectedResourceMetadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	u.Path = ProtectedResourceMetadataPath(u.Path)
	u.RawPath = ""
	return u.String()
}

// protectedResourceMetadata is the RFC 9728 §2 document, deliberately closed:
// it is served unauthenticated wherever the resource is reachable.
type protectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// ProtectedResourceHandler serves the metadata document for the resource
// resourceURL derives, naming issuer as its only authorization server. Mount
// it, unauthenticated, at ProtectedResourceMetadataPath of the resource's
// path — and nowhere else: a copy at the bare well-known URI would describe
// the whole origin (§3.1).
//
// It panics on a blank issuer: routes are wired at startup, and a document
// naming no authorization server can never lead a client to a token.
func ProtectedResourceHandler(issuer string, resourceURL func(*http.Request) string) http.Handler {
	if strings.TrimSpace(issuer) == "" {
		panic("oidcauth: ProtectedResourceHandler with no issuer: the document would name no authorization server")
	}
	if resourceURL == nil {
		panic("oidcauth: ProtectedResourceHandler with no resource URL")
	}
	csp := buildCSP(issuer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, csp)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "use GET")
			return
		}
		writeJSON(w, http.StatusOK, protectedResourceMetadata{
			Resource:               resourceURL(r),
			AuthorizationServers:   []string{issuer},
			BearerMethodsSupported: []string{"header"},
		})
	})
}

// Challenge returns middleware that adds `WWW-Authenticate` to every 401 the
// wrapped handler writes — Middleware's own refusals included. With a
// resourceURL the challenge names the metadata document
// (`Bearer resource_metadata="…"`, the MCP authorization spec); with nil it
// is the bare RFC 6750 `Bearer`.
//
// Only a 401 is a challenge: a 403 (valid token, missing role) or a success
// carries no header.
func Challenge(resourceURL func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			value := "Bearer"
			if resourceURL != nil {
				value = `Bearer resource_metadata="` + ProtectedResourceMetadataURL(resourceURL(r)) + `"`
			}
			next.ServeHTTP(&challengeWriter{ResponseWriter: w, value: value}, r)
		})
	}
}

// challengeWriter stamps the challenge at the moment the status is written,
// which is the only moment the status is known.
type challengeWriter struct {
	http.ResponseWriter
	value       string
	wroteHeader bool
}

func (c *challengeWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.wroteHeader = true
		if code == http.StatusUnauthorized {
			c.Header().Set("WWW-Authenticate", c.value)
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *challengeWriter) Write(b []byte) (int, error) {
	c.wroteHeader = true
	return c.ResponseWriter.Write(b)
}

// Flush keeps a streaming route behind the wrapper streaming: an SSE handler
// type-asserts http.Flusher, and hiding it would turn the stream into a 501.
func (c *challengeWriter) Flush() {
	_ = http.NewResponseController(c.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *challengeWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// RefuseBrowserOrigin answers 403 to any request carrying an Origin header.
//
// The MCP transport requires a server to validate Origin, against DNS
// rebinding. A service with no configured origin has nothing to compare it
// with — and comparing with Host does not stop rebinding, which is the attack
// that controls Host — while no browser client of an MCP endpoint exists. So
// the header's presence is the refusal (docs/55 D6, docs/56 D9). Wrap it
// outside authentication: a refused origin never reaches token validation.
func RefuseBrowserOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header["Origin"]; present {
			writeError(w, http.StatusForbidden, CodeForbidden,
				"browser-originated requests are refused on this endpoint")
			return
		}
		next.ServeHTTP(w, r)
	})
}
