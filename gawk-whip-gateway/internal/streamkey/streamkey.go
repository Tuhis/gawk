// Package streamkey parses the WHIP bearer token, the stream key
// gawk1_<ID>_<token> (docs/83 D2).
//
// <ID> is the 6-character broadcast code and <token> the relay's 32-hex
// resume token for it. The ID is normalised exactly as the relay normalises
// it, through gawk-server's public moderation.Normalize (which wraps the
// relay's internal broadcastid package), so the alphabet has one definition
// (CODE-REVIEW.md) and a lowercase code in a pasted key reaches the same
// broadcast. The token is not normalised: the relay mints lowercase hex, and
// accepting uppercase would make two spellings of one key hash to two
// records.
package streamkey

import (
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/moderation"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
)

// Prefix versions the key format and makes a leaked key findable by secret
// scanners (D2).
const Prefix = "gawk1_"

// TokenLen is the resume token's length in hex characters (16 bytes).
const TokenLen = 32

// ErrMalformed is every parse failure: the gateway answers 401 and records
// nothing (there is no key to record against).
var ErrMalformed = errors.New("streamkey: malformed stream key")

// Key is a parsed stream key.
type Key struct {
	// ID is the normalised (uppercase) broadcast code.
	ID string
	// Token is the resume token, 32 lowercase hex characters.
	Token string
	// Hash is the SHA-256 of the canonical key (Prefix + ID + "_" + Token):
	// what records, sessions and peer lookups are keyed by.
	Hash status.Hash
}

// String renders the canonical key. It is a credential: never log it.
func (k Key) String() string { return Prefix + k.ID + "_" + k.Token }

// Parse parses a stream key.
func Parse(s string) (Key, error) {
	rest, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return Key{}, ErrMalformed
	}
	id, token, ok := strings.Cut(rest, "_")
	if !ok || len(token) != TokenLen || strings.ContainsAny(id, " \t\r\n") {
		return Key{}, ErrMalformed
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return Key{}, ErrMalformed
		}
	}
	norm, err := moderation.Normalize(moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: id}})
	if err != nil {
		return Key{}, ErrMalformed
	}
	k := Key{ID: norm.Target.Value, Token: token}
	k.Hash = sha256.Sum256([]byte(k.String()))
	return k, nil
}

// FromAuthorization parses "Authorization: Bearer <key>".
func FromAuthorization(h string) (Key, error) {
	scheme, cred, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return Key{}, ErrMalformed
	}
	return Parse(strings.TrimSpace(cred))
}
