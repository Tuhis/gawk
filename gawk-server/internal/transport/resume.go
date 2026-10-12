// Resume tokens (docs/22): the second gate on /publish/{id}. A token is a
// truncated HMAC over the normalized broadcast ID, keyed by a fleet-shared
// key every relay pod holds — so any pod can mint and verify with no shared
// storage, which lets a broadcaster claim its ID on a pod that has never seen
// it (the pod creates the hub instead of 404ing, so broadcasts survive relay
// restarts).
//
// Hijack scope: with an explicit -resume-token-key, which never leaves the
// server side, knowing a broadcast ID plus the publish secret does not
// suffice to take over someone else's broadcast. With only a publish secret
// the key is derived from it, so every secret-holder can compute every ID's
// token offline — that stops everyone lacking the secret but gates nothing
// between broadcasters. Fleet deployments should set the explicit key.
//
// Tokens are never logged.
package transport

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
)

// resumeTokenBytes is the minted token length: 128 bits of HMAC-SHA256 —
// far beyond brute force over a query parameter, small enough for a URL.
const resumeTokenBytes = 16

// resumeKeyInfo is the HKDF info string binding the secret-derived key to
// this use. In secret-derived mode, rotating the
// publish secret rotates the key, revoking every outstanding token.
const resumeKeyInfo = "gawk-resume-v1"

// resumeTokens mints and verifies resume and room creator tokens.
type resumeTokens struct {
	key []byte
}

// newResumeTokens derives the token key. Precedence: an explicit
// ResumeTokenKey wins — it stays server-side, making the token a real
// per-broadcast ownership proof (see the package comment). Otherwise HKDF
// from the publish secret keeps zero-config deployments working. Otherwise a
// fresh per-process random key: reclaim works only for the process lifetime.
func newResumeTokens(cfg config.Config) *resumeTokens {
	if len(cfg.ResumeTokenKey) > 0 {
		return &resumeTokens{key: cfg.ResumeTokenKey}
	}
	if cfg.PublishSecret != "" {
		key, err := hkdf.Key(sha256.New, []byte(cfg.PublishSecret), nil, resumeKeyInfo, 32)
		if err != nil {
			// Unreachable with SHA-256 and a 32-byte request; fail loud.
			panic("transport: hkdf key derivation failed: " + err.Error())
		}
		return &resumeTokens{key: key}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("transport: crypto/rand unavailable: " + err.Error())
	}
	return &resumeTokens{key: key}
}

// mint returns the resume token for a normalized broadcast ID.
func (rt *resumeTokens) mint(normalizedID string) []byte {
	mac := hmac.New(sha256.New, rt.key)
	mac.Write([]byte(normalizedID))
	return mac.Sum(nil)[:resumeTokenBytes]
}

// verify reports whether tokenHex is the valid token for a normalized
// broadcast ID. Constant-time; an unparseable or wrong-length token is
// simply invalid.
func (rt *resumeTokens) verify(normalizedID, tokenHex string) bool {
	token, err := hex.DecodeString(tokenHex)
	if err != nil || len(token) != resumeTokenBytes {
		return false
	}
	return subtle.ConstantTimeCompare(token, rt.mint(normalizedID)) == 1
}

// roomCreatorDomain is the domain-separation prefix of a room creator token
// (docs/44): room codes come from the same alphabet and length as broadcast
// IDs, so without it a broadcast's resume token would be the creator token of
// an identically named room and vice versa. Only the room construction is
// prefixed, so outstanding resume tokens keep verifying.
const roomCreatorDomain = "gawk-room-creator-v1"

// MintCreator returns the creator token for a normalized room code
// (roomsrv.Tokens).
func (rt *resumeTokens) MintCreator(code string) []byte {
	mac := hmac.New(sha256.New, rt.key)
	mac.Write([]byte(roomCreatorDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return mac.Sum(nil)[:resumeTokenBytes]
}

// VerifyCreator reports whether token is the creator token for a normalized
// room code (roomsrv.Tokens). Constant-time.
func (rt *resumeTokens) VerifyCreator(code string, token []byte) bool {
	return len(token) == resumeTokenBytes && subtle.ConstantTimeCompare(token, rt.MintCreator(code)) == 1
}

// VerifyResume reports whether token is the raw resume token for a
// broadcast ID — the room attach proof (roomsrv.Tokens). The ID
// is normalized here because the token is minted over the normalized form.
func (rt *resumeTokens) VerifyResume(broadcastID string, token []byte) bool {
	normID, err := broadcastid.Normalize(broadcastID)
	if err != nil || len(token) != resumeTokenBytes {
		return false
	}
	return subtle.ConstantTimeCompare(token, rt.mint(normID)) == 1
}
