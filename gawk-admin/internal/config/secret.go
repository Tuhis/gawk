package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// SecretPrefix is the optional marker Standard Webhooks puts in front of a
// base64 secret (docs/52 D5). Portal-created webhooks generate secrets in
// this form; chart-provided ones may carry it or not.
const SecretPrefix = "whsec_"

// ErrInvalidSecret is returned for a secret that is not base64 (after the
// optional prefix): a receiver's Standard Webhooks library would refuse to
// construct a verifier from it, so nothing signed with it could ever be
// verified.
var ErrInvalidSecret = errors.New("a webhook secret must be base64, optionally prefixed whsec_")

// SigningKey derives the HMAC key from a webhook's secret as configured: the
// D5 key-derivation rule, in one function used by both webhook sources.
//
// It is exactly what the Standard Webhooks reference libraries do: strip an
// OPTIONAL `whsec_` prefix, then ALWAYS base64-decode the remainder — the
// decoded bytes are the key. A secret is never used verbatim, because the
// libraries never do that either (revised 2026-09-18, PR #327 review: a raw
// secret would sign with different bytes than every library verifier derives
// from the same string, and one that is not base64 cannot construct a
// verifier at all). The whole point of adopting the standard is that a
// receiver can paste the same string into its library that the operator put
// in the Secret; this function keeps that promise for every secret gawk
// accepts. Generate one with `openssl rand -base64 32`.
//
// It is called once at configuration time (a chart-defined webhook with an
// undecodable secret refuses to start; a portal-created one is a 400) and
// again at every send, where it cannot fail for a secret that passed.
func SigningKey(secret string) ([]byte, error) {
	encoded := strings.TrimPrefix(secret, SecretPrefix)
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSecret, err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: it decodes to nothing", ErrInvalidSecret)
	}
	return key, nil
}
