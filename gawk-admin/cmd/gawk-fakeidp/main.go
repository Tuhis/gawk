// Command gawk-fakeidp is a TEST-ONLY OIDC issuer for the kind e2e tier
// (docs/42 §11.1's envtest/kind row): discovery, a JWKS, and a /mint endpoint
// that signs an operator access token — everything gawk-admin's verifier
// needs to authorize one scripted kill, and nothing more.
//
// It is the deployable sibling of internal/auth's fakeIDP test double, built
// on the same go-oidc oidctest rendering. It is NOT part of the gawk-admin
// image (deploy/Dockerfile builds only ./cmd/gawk-admin) and must never be:
// it signs whatever /mint is asked for with a key generated at startup. The
// e2e job builds it into its own throwaway image (e2e/Dockerfile.fakeidp)
// for a cluster that lives ~15 minutes.
//
// -issuer is the URL the discovery document advertises and tokens carry as
// `iss` — the IN-CLUSTER Service URL, whatever address a caller used to
// reach it, because go-oidc requires the document's issuer to equal the
// configured one while the e2e script mints through a port-forward.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

func main() {
	var (
		addr     = flag.String("addr", ":8080", "listen address")
		issuer   = flag.String("issuer", "", "the issuer URL this IdP advertises and stamps into `iss` (required)")
		audience = flag.String("audience", "gawk-admin", "the `aud` minted tokens carry")
		role     = flag.String("role", "operator", "the role minted tokens carry at resource_access.<audience>.roles")
		subject  = flag.String("subject", "e2e-operator", "the `sub` minted tokens carry")
		email    = flag.String("email", "operator@e2e.invalid", "the `email` minted tokens carry")
		lifetime = flag.Duration("lifetime", 15*time.Minute, "minted-token lifetime")
	)
	flag.Parse()
	if *issuer == "" {
		log.Fatal("gawk-fakeidp: -issuer is required")
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("gawk-fakeidp: generate key: %v", err)
	}
	const kid = "e2e-key"
	disc := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: kid, Algorithm: oidc.RS256}},
	}
	disc.SetIssuer(*issuer)

	mux := http.NewServeMux()
	// Discovery and the JWKS: what gawk-admin's verifier fetches.
	mux.Handle("/", disc)
	// The script's side: one POST, one signed operator token.
	mux.HandleFunc("POST /mint", func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now()
		claims := map[string]any{
			"iss":   *issuer,
			"aud":   *audience,
			"sub":   *subject,
			"email": *email,
			"iat":   now.Add(-time.Minute).Unix(),
			"exp":   now.Add(*lifetime).Unix(),
			"resource_access": map[string]any{
				*audience: map[string]any{"roles": []any{*role}},
			},
		}
		raw, err := json.Marshal(claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		token := oidctest.SignIDToken(key, kid, oidc.RS256, string(raw))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q}`, token)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("gawk-fakeidp: serving issuer %s on %s (TEST ONLY — mints for anyone who asks)", *issuer, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
