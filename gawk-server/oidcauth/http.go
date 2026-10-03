package oidcauth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

// Middleware authenticates the request and puts the caller's identity on the
// context (NewContext). It authorizes nothing: a token whose roles claim is
// missing or malformed authenticates with an empty role set, and RequireRole
// then refuses it with 403.
//
// Any invalid credential is answered 401 — or 429 once this client IP has
// spent its failure budget. While discovery is unresolved every request is
// answered 401 `idp_unavailable` without spending that budget.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, v.csp)

		if !v.Ready() {
			// We cannot judge this credential yet, so we refuse it — 401, not
			// 500: nothing is broken here, and an SPA's existing 401 handling
			// (refresh, then re-run the redirect flow) is the right reaction.
			// This costs the caller nothing: the failure is ours, so it does
			// not spend their invalid-credential budget.
			v.log.Debug("request refused while the issuer is unresolved", "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, CodeIDPUnavailable,
				"the identity provider is not reachable yet; retry shortly")
			return
		}

		raw, err := bearerToken(r)
		if err != nil {
			v.denyCredential(w, r, err)
			return
		}
		id, err := v.Verify(r.Context(), raw)
		if err != nil {
			v.denyCredential(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), id)))
	})
}

// RequireRole refuses a valid token that does not carry role (403). It must
// be wrapped by Middleware.
//
// The role is a parameter rather than Config.Role because a service can
// authorize more than one — gawk-admin's content-flag route authorizes the
// flagger role held by a client-credentials service identity (docs/42 §4.11)
// — and because rights must stay bound to a role, never to a merely-valid
// token.
//
// An empty role panics rather than returning a middleware: routes are wired at
// startup, so this is the same "refuse to boot" the config validation performs
// (docs/42 §4.8) — a RequireRole("") that quietly authorized everyone is
// precisely the failure that rule exists to prevent.
func (v *Verifier) RequireRole(role string) func(http.Handler) http.Handler {
	if strings.TrimSpace(role) == "" {
		panic("auth: RequireRole(\"\"): with no required role every valid token would be an operator")
	}
	return v.RequireAnyRole(role)
}

// RequireAnyRole refuses a valid token that carries none of roles (403).
//
// The role model is one role per capability, not a hierarchy (docs/50 D1): a
// route a second role may call names both, rather than one role implying the
// other. It panics on an empty list or an empty entry for RequireRole's reason
// — either would quietly admit every valid token.
func (v *Verifier) RequireAnyRole(roles ...string) func(http.Handler) http.Handler {
	if len(roles) == 0 {
		panic("auth: RequireAnyRole(): with no required role every valid token would be admitted")
	}
	for _, role := range roles {
		if strings.TrimSpace(role) == "" {
			panic("auth: RequireAnyRole with an empty role: every valid token would be admitted")
		}
	}
	roles = append([]string(nil), roles...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			setSecurityHeaders(w, v.csp)
			id, ok := FromContext(r.Context())
			if !ok {
				// Only reachable by wiring RequireRole without Middleware:
				// a programming error, not a client error (identity.go).
				v.log.Error("RequireRole reached without an authenticated identity; check the middleware wiring", "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, CodeInternal, "authentication middleware is not wired")
				return
			}
			for _, role := range roles {
				if id.HasRole(role) {
					next.ServeHTTP(w, r)
					return
				}
			}
			v.log.Debug("role missing", "roles", roles, "subject", id.Subject)
			if len(roles) == 1 {
				writeError(w, http.StatusForbidden, CodeForbidden, "this account does not hold the "+roles[0]+" role")
				return
			}
			writeError(w, http.StatusForbidden, CodeForbidden,
				"this account holds none of the roles "+strings.Join(roles, ", "))
		})
	}
}

// authConfig is an SPA's unauthenticated bootstrap payload: exactly the three
// values a public OIDC client needs to start the PKCE flow, and nothing else.
// No secret exists to leak — the SPA is a public client (docs/42 §4.8) — but
// this endpoint is world-readable wherever the service is exposed, so the
// struct is deliberately closed. Anything added here is published.
type authConfig struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
	Audience string `json:"audience"`
}

// ConfigHandler serves GET /auth/config, unauthenticated (docs/42 §4.8): the
// configured issuer, Config.ClientID and audience.
//
// It panics when Config.ClientID is blank: routes are wired at startup, and a
// bootstrap that hands the SPA no client ID is a configuration that can never
// log anybody in.
func (v *Verifier) ConfigHandler() http.Handler {
	if strings.TrimSpace(v.clientID) == "" {
		panic("oidcauth: ConfigHandler with no Config.ClientID: the SPA could never start a login")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, v.csp)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "use GET")
			return
		}
		writeJSON(w, http.StatusOK, authConfig{
			Issuer:   v.issuer,
			ClientID: v.clientID,
			Audience: v.audience,
		})
	})
}

// denyCredential answers a request whose credential did not validate, and
// spends one of this client IP's failure tokens.
//
// Only failures spend tokens, and the budget decides the *status code* rather
// than short-circuiting the request. That ordering is deliberate: behind an
// Ingress every operator can share one observed source IP, so a scheme that
// refused requests from a "hot" IP before validating them would let one
// fuzzing loop lock a paged operator out (docs/42 §4.8, D7). Here a valid
// token always passes, whatever the attacker in the next NAT session is doing,
// and the attacker's own responses degrade to 429 within `burst` requests.
func (v *Verifier) denyCredential(w http.ResponseWriter, r *http.Request, cause error) {
	ip := clientIP(r)
	if !v.limiter.Allow(ip) {
		// Debug: the IP must not appear above Debug (docs/42 §5).
		v.log.Debug("invalid credentials rate-limited", "ip", ip, "path", r.URL.Path)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, CodeRateLimited, "too many invalid credentials; slow down")
		return
	}
	v.log.Debug("rejected credential", "ip", ip, "path", r.URL.Path, "err", cause)
	// The client learns nothing beyond "not accepted": which check failed is
	// useful only to someone probing the boundary. The detail is in the log.
	writeError(w, http.StatusUnauthorized, CodeUnauthorized, "a valid bearer token is required")
}

// bearerToken extracts the credential. A missing or malformed Authorization
// header is an invalid credential like any other — there is no anonymous mode.
func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", errors.New("no Authorization header")
	}
	scheme, value, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("Authorization header is not a Bearer credential")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty Bearer credential")
	}
	return value, nil
}

// clientIP keys the failure budget. r.RemoteAddr only — X-Forwarded-For is
// attacker-controlled, and honouring it without a trusted-proxy list would let
// one client mint a fresh budget per request, which is worse than the coarse
// bucketing an Ingress imposes.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// The `code` values this package writes into the error envelope.
//
// They are CONTRACT: a client branches on `code`. They matter more than most,
// because these are the codes a caller meets FIRST — a missing, expired or
// unprivileged token is the failure every new integration hits before it ever
// reaches a handler.
//
// gawk-admin's R48 OpenAPI drift check derives its `ErrorCode` enum by
// walking the `Code*` string constants of its own internal/api AND of this
// package's source, so a code added here without a line in gawk-admin's
// `openapi.yaml` fails its `go test`. Keep them named constants with literal
// values: anything else is invisible to that walk.
const (
	// CodeUnauthorized: no credential, or one this deployment will not accept.
	// Deliberately says nothing about WHICH check failed.
	CodeUnauthorized = "unauthorized"
	// CodeIDPUnavailable: discovery has not resolved yet, so no credential can
	// be judged. A 401 rather than a 500 — nothing is broken, and retrying is
	// the right reaction.
	CodeIDPUnavailable = "idp_unavailable"
	// CodeForbidden: a VALID token whose identity lacks the required role.
	CodeForbidden = "forbidden"
	// CodeRateLimited: too many invalid credentials from one address. Carries
	// Retry-After.
	CodeRateLimited = "rate_limited"
	// CodeMethodNotAllowed is /auth/config's alone. gawk-admin's OpenAPI
	// document does not list it: under its /api/v1 a method mismatch matches
	// the catch-all instead and answers 404.
	CodeMethodNotAllowed = "method_not_allowed"
	// CodeInternal: the middleware is not wired. A programming error, not a
	// client one.
	CodeInternal = "internal"
)

// apiError is the error envelope docs/42 §4.7 specifies:
// `{"error":{"code":"…","message":"…"}}`. A consumer's own handlers render
// the same shape; the shape is one line, so that is a restatement, not a fork
// of logic.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: apiErrorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Nothing this package emits is cacheable: /auth/config is a bootstrap the
	// SPA re-reads on load, and an auth failure must never be served from a
	// cache to the next request.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
