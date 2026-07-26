// Telemetry session tokens (R28 TM1, docs/33 D2 + §4.2).
//
// The load-bearing gap R28 closes is that /statusz names a subscriber by a
// random per-session key and NOTHING ever tells that client its own key — so
// the relay's view of a viewer and the viewer's view of itself are two
// datasets that cannot be joined. "Per-viewer experience" is exactly that
// join. A token minted here is both halves of the fix: the sessionId inside
// it appears on both sides, and the tag around it is what makes an
// unauthenticated public write surface tolerable — the ingest accepts records
// only from clients that actually connected to a relay in this fleet.
//
//	token = expHour (uint32 BE) ‖ nonce (12 B) ‖ tag (8 B)     # 24 bytes
//	tag   = HMAC-SHA256(key, expHour ‖ nonce ‖ broadcastKey ‖ role)[:8]
//
// Stateless by construction, exactly R17's resume-token pattern: any pod
// mints, the telemetry service verifies with one HMAC — no lookup, no shared
// database, no chatter with the relay. Expiry is a field rather than a bucket
// sweep, and role is bound into the tag so a viewer's token cannot submit
// broadcaster-shaped records.
//
// This lives in the public wire package (not internal/) for the same reason
// the rest of it does: gawk-telemetry is a separate module and must consume
// the real implementation rather than mirror it (R14 Decision 1).
//
// Tokens are never logged. Only the sessionId — hex(nonce) — is ever stored,
// by either end.
package wire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	// TelemetrySessionTokenSize is the minted token length: 4-byte expiry
	// hour, 12-byte nonce, 8-byte tag.
	TelemetrySessionTokenSize = 24
	// TelemetryNonceSize is the random part, and the sessionId's source.
	// 96 bits: collision-free across every session this fleet will ever carry,
	// and short enough that a sessionId stays a readable 24-char handle.
	TelemetryNonceSize = 12
	// TelemetryTagSize truncates the HMAC to 64 bits. The token is not a
	// long-lived credential — it expires in a day and can only pollute the one
	// session it names — so 64 bits of forgery resistance is the right point
	// on the size/strength curve for a field that also rides a 35-byte
	// fixed-width wire message.
	TelemetryTagSize = 8
	// TelemetrySessionIDLen is the hex length of a sessionId.
	TelemetrySessionIDLen = TelemetryNonceSize * 2
	// TelemetryTokenTTL is how long a minted token stays valid. A day covers
	// every plausible broadcast; a token outliving its session is only good
	// for submitting records attributed to a session that already ended
	// (docs/33 §8 records the periodic-re-hello fix as deferred until a
	// broadcast actually outlives this).
	TelemetryTokenTTL = 24 * time.Hour
	// TelemetryKeySize is the fleet Secret's length, matching R17's statsKey /
	// resumeTokenKey / internalPsk shape.
	TelemetryKeySize = 32
)

// TelemetryRole distinguishes the two session shapes a token may authorize.
// Bound into the tag, so a viewer's token cannot submit broadcaster records.
type TelemetryRole string

const (
	// TelemetryRoleViewer is a /subscribe/{id} session.
	TelemetryRoleViewer TelemetryRole = "viewer"
	// TelemetryRoleBroadcaster is a /publish or /publish/{id} session.
	TelemetryRoleBroadcaster TelemetryRole = "broadcaster"
)

// ValidTelemetryRole reports whether r is a role this build knows. The ingest
// checks it before verifying, so an unknown role is a 400 rather than a
// mysterious tag mismatch.
func ValidTelemetryRole(r TelemetryRole) bool {
	return r == TelemetryRoleViewer || r == TelemetryRoleBroadcaster
}

// Sentinel errors from the token functions. Check with errors.Is.
var (
	// ErrTelemetryTokenInvalid covers every malformed or unauthentic token:
	// wrong length, bad hex, unknown role, tampered broadcast key, wrong
	// fleet key. Deliberately one error — an attacker learns nothing from
	// which check failed, and no caller behaves differently.
	ErrTelemetryTokenInvalid = errors.New("wire: invalid telemetry token")
	// ErrTelemetryTokenExpired is separate because it IS actionable: a client
	// holding one needs a fresh hello, not a bug report.
	ErrTelemetryTokenExpired = errors.New("wire: expired telemetry token")
	// ErrTelemetryKey indicates a fleet key that is not TelemetryKeySize
	// bytes. Minting with a short key would produce tokens the verifier
	// silently accepts under an equally short key, so it fails loudly.
	ErrTelemetryKey = errors.New("wire: invalid telemetry key")
)

// MintTelemetrySessionToken mints a token for one session, using crypto/rand
// for the nonce. broadcastKey is the raw obfuscated-ID digest
// (TelemetryBroadcastKeySize bytes) — never the joinable broadcast ID.
func MintTelemetrySessionToken(key []byte, broadcastKey []byte, role TelemetryRole, now time.Time) ([]byte, error) {
	nonce := make([]byte, TelemetryNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("wire: crypto/rand unavailable: %w", err)
	}
	return mintTelemetrySessionToken(key, broadcastKey, role, now, nonce)
}

// mintTelemetrySessionToken is the deterministic core, split out so tests can
// pin golden bytes against a fixed nonce.
func mintTelemetrySessionToken(key []byte, broadcastKey []byte, role TelemetryRole, now time.Time, nonce []byte) ([]byte, error) {
	if len(key) != TelemetryKeySize {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrTelemetryKey, len(key), TelemetryKeySize)
	}
	if len(broadcastKey) != TelemetryBroadcastKeySize {
		return nil, fmt.Errorf("%w: broadcast key %d bytes, want %d", ErrTelemetryTokenInvalid, len(broadcastKey), TelemetryBroadcastKeySize)
	}
	if !ValidTelemetryRole(role) {
		return nil, fmt.Errorf("%w: unknown role %q", ErrTelemetryTokenInvalid, role)
	}
	if len(nonce) != TelemetryNonceSize {
		return nil, fmt.Errorf("%w: nonce %d bytes, want %d", ErrTelemetryTokenInvalid, len(nonce), TelemetryNonceSize)
	}

	token := make([]byte, 0, TelemetrySessionTokenSize)
	token = binary.BigEndian.AppendUint32(token, telemetryExpHour(now.Add(TelemetryTokenTTL)))
	token = append(token, nonce...)
	token = append(token, telemetryTag(key, token[:4+TelemetryNonceSize], broadcastKey, role)...)
	return token, nil
}

// VerifyTelemetrySessionToken checks a token against the fleet key, the
// broadcast key and the role it claims, and returns the sessionId it names.
// Constant-time; expiry is checked before the tag so an expired-but-authentic
// token reports the actionable error.
func VerifyTelemetrySessionToken(key, token, broadcastKey []byte, role TelemetryRole, now time.Time) (string, error) {
	if len(key) != TelemetryKeySize {
		return "", fmt.Errorf("%w: %d bytes, want %d", ErrTelemetryKey, len(key), TelemetryKeySize)
	}
	if len(token) != TelemetrySessionTokenSize || len(broadcastKey) != TelemetryBroadcastKeySize || !ValidTelemetryRole(role) {
		return "", ErrTelemetryTokenInvalid
	}
	if telemetryExpHour(now) > binary.BigEndian.Uint32(token[:4]) {
		return "", ErrTelemetryTokenExpired
	}
	want := telemetryTag(key, token[:4+TelemetryNonceSize], broadcastKey, role)
	if subtle.ConstantTimeCompare(token[4+TelemetryNonceSize:], want) != 1 {
		return "", ErrTelemetryTokenInvalid
	}
	return hex.EncodeToString(token[4 : 4+TelemetryNonceSize]), nil
}

// TelemetrySessionID returns the sessionId a token names, without verifying
// it. Only for the minting side, which just authenticated the token by
// construction — an ingest path must use VerifyTelemetrySessionToken, whose
// sessionId is the authenticated one.
func TelemetrySessionID(token []byte) (string, error) {
	if len(token) != TelemetrySessionTokenSize {
		return "", fmt.Errorf("%w: %d bytes, want %d", ErrTelemetryTokenInvalid, len(token), TelemetrySessionTokenSize)
	}
	return hex.EncodeToString(token[4 : 4+TelemetryNonceSize]), nil
}

// telemetryExpHour is unix hours, the token's expiry granularity. Wall clock,
// not monotonic: it must mean the same thing on a relay pod and on the
// telemetry service, which are different processes on possibly different
// nodes.
func telemetryExpHour(t time.Time) uint32 {
	return uint32(t.Unix() / 3600)
}

func telemetryTag(key, prefix, broadcastKey []byte, role TelemetryRole) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(prefix)
	mac.Write(broadcastKey)
	mac.Write([]byte(role))
	return mac.Sum(nil)[:TelemetryTagSize]
}
