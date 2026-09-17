package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// SecretPrefix marks a webhook secret whose signing key is the base64 after
// the prefix — the Standard Webhooks convention (docs/52 D5). Portal-created
// webhooks generate secrets in this form; chart-provided ones may be either.
const SecretPrefix = "whsec_"

// ErrInvalidSecret is returned for a `whsec_` secret whose remainder is not
// base64: a receiver's Standard Webhooks library would refuse to construct a
// verifier from it, so nothing signed with it could ever be verified.
var ErrInvalidSecret = errors.New("a whsec_ secret must be base64 after the prefix")

// SigningKey derives the HMAC key from a webhook's secret as configured: the
// D5 key-derivation rule, in one function used by both webhook sources.
//
// A secret that starts with SecretPrefix is base64-decoded after the prefix
// and the bytes are the key; any other secret string is the key verbatim.
// The rule exists because Standard Webhooks libraries expect `whsec_` and
// decode it themselves — a receiver must be able to paste the same string
// into its library that the operator put in the Secret. It is a derivation,
// never a place a key is stored in a new form.
//
// It is called once at configuration time (a chart-defined webhook with an
// undecodable secret refuses to start; a portal-created one is a 400) and
// again at every send, where it cannot fail for a secret that passed.
func SigningKey(secret string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok {
		return []byte(secret), nil
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSecret, err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: it decodes to nothing", ErrInvalidSecret)
	}
	return key, nil
}
