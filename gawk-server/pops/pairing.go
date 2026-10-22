package pops

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// pairingTokenBytes is the pairing string's entropy: 128 bits (docs/76
// item 7). credentialBytes is the credential's: 256 bits (item 8).
const (
	pairingTokenBytes = 16
	credentialBytes   = 32
	tokenGroup        = 5
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewPairingToken returns a fresh pairing token: 128 random bits, RFC 4648
// base32 without padding, upper case, a '-' every five characters so a
// human can read it aloud ("K7M2Q-9XDA4-…"). Every call is random, so two
// Re-pairs in the same second differ (docs/76 item 9).
func NewPairingToken() (string, error) {
	var raw [pairingTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("pops: pairing token: %w", err)
	}
	return groupToken(b32.EncodeToString(raw[:])), nil
}

func groupToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += tokenGroup {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+tokenGroup, len(s))])
	}
	return b.String()
}

// normalizeToken strips the presentation (dashes, case, whitespace).
func normalizeToken(tok string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(tok), "-", ""))
}

// FormatPairing builds the pairing string an operator copies once:
// "gawk-pair:" + the home relay endpoint (host:port) + "/" + token.
func FormatPairing(endpoint, token string) string {
	return PairingPrefix + endpoint + "/" + token
}

// ParsePairing splits a pasted pairing string into the home endpoint
// (host:port, brackets kept for IPv6) and the canonical token (upper case,
// grouped). It tolerates surrounding whitespace, lower case and missing
// dashes, and refuses anything that is not exactly 128 bits of base32.
func ParsePairing(s string) (endpoint, token string, err error) {
	s = strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(s, PairingPrefix)
	if !ok {
		return "", "", fmt.Errorf("%w: missing %q prefix", ErrBadPairing, PairingPrefix)
	}
	slash := strings.LastIndexByte(rest, '/')
	if slash < 0 {
		return "", "", fmt.Errorf("%w: no token", ErrBadPairing)
	}
	endpoint, tok := rest[:slash], rest[slash+1:]
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || strings.ContainsAny(host, "/@?#") {
		return "", "", fmt.Errorf("%w: endpoint must be host:port", ErrBadPairing)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("%w: bad port", ErrBadPairing)
	}
	norm := normalizeToken(tok)
	raw, err := b32.DecodeString(norm)
	if err != nil || len(raw) != pairingTokenBytes {
		return "", "", fmt.Errorf("%w: token is not 128 bits of base32", ErrBadPairing)
	}
	return endpoint, groupToken(norm), nil
}

// TokenSHA256 is the stored hash of a pairing token: hex SHA-256 of the
// token with its dashes stripped, upper-cased, so the presentation never
// changes the hash.
func TokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(normalizeToken(token)))
	return hex.EncodeToString(sum[:])
}

// NewCredential mints a PoP credential: 256 random bits, base64url without
// padding.
func NewCredential() (string, error) {
	var raw [credentialBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("pops: credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// CredentialSHA256 is the stored hash of a credential (hex).
func CredentialSHA256(cred string) string {
	sum := sha256.Sum256([]byte(cred))
	return hex.EncodeToString(sum[:])
}

func hexEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(a)), []byte(strings.ToLower(b))) == 1
}

// CredentialCurrent reports whether the recorded credential was paired with
// the current pairing string (docs/76 item 9). A -pops-file Pop has neither
// hash, so its credential is always current.
func (p *Pop) CredentialCurrent() bool {
	return p != nil && p.Status.Credential != nil &&
		p.Status.Credential.PairingTokenSHA256 == p.Spec.Pairing.TokenSHA256
}

// pastEnd reports whether the Pop's end date has passed.
func (p *Pop) pastEnd(now time.Time) bool {
	return p.Spec.NotAfter != nil && now.After(p.Spec.NotAfter.Time)
}

// CheckCredential authenticates a presented credential. The hash is
// compared first, in constant time, so a wrong credential learns nothing
// about the Pop's state: it is ErrCredentialNotCurrent (401) whether the
// Pop is revoked or not. A matching one is then ErrRevoked or ErrExpired
// (403) when the Pop says so, and ErrCredentialNotCurrent when a Re-pair
// superseded the string it was paired with.
func (p *Pop) CheckCredential(cred string, now time.Time) error {
	if p == nil || p.Status.Credential == nil || p.Status.Credential.SHA256 == "" || cred == "" ||
		!hexEqual(CredentialSHA256(cred), p.Status.Credential.SHA256) {
		return ErrCredentialNotCurrent
	}
	if p.Spec.Revoked {
		return ErrRevoked
	}
	if p.pastEnd(now) {
		return ErrExpired
	}
	if !p.CredentialCurrent() {
		return ErrCredentialNotCurrent
	}
	return nil
}

// Redeemable reports whether a pairing token (by its TokenSHA256) may be
// redeemed now: it must be the current string (else ErrPairingUnknown), not
// already redeemed (ErrPairingUsed), not past spec.pairing.expiresAt
// (ErrPairingExpired), and the Pop must be neither revoked nor past its end
// date. expiresAt limits only redemption, never a credential (item 8).
func (p *Pop) Redeemable(tokenSHA string, now time.Time) error {
	if p == nil || p.Spec.Pairing.TokenSHA256 == "" || tokenSHA == "" ||
		!hexEqual(tokenSHA, p.Spec.Pairing.TokenSHA256) {
		return ErrPairingUnknown
	}
	if p.CredentialCurrent() {
		return ErrPairingUsed
	}
	if p.Spec.Pairing.ExpiresAt != nil && now.After(p.Spec.Pairing.ExpiresAt.Time) {
		return ErrPairingExpired
	}
	if p.Spec.Revoked {
		return ErrRevoked
	}
	if p.pastEnd(now) {
		return ErrExpired
	}
	return nil
}
