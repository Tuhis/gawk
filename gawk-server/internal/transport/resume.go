// Resume tokens (R17 W2, docs/22 Decision 7): the second gate on
// /publish/{id}. A token is a truncated HMAC over the normalized broadcast
// ID, keyed by a key every relay pod can derive — so any pod can mint and
// verify with no shared storage, which is what lets a broadcaster claim its
// ID on a pod that has never seen it (the pod then *creates* the hub instead
// of 404ing, making broadcasts survive relay restarts). It also closes the
// pre-W2 graced-ID hijack: knowing a broadcast ID plus the global publish
// secret no longer suffices to take over someone else's broadcast.
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

	"github.com/Tuhis/gawk/gawk-server/internal/config"
)

// resumeTokenBytes is the minted token length: 128 bits of HMAC-SHA256 —
// far beyond brute force over a query parameter, small enough for a URL.
const resumeTokenBytes = 16

// resumeKeyInfo is the HKDF info string binding the derived key to this use
// (docs/22: "gawk-resume-v1"). Rotating the publish secret rotates the key,
// revoking every outstanding token — the rotation story.
const resumeKeyInfo = "gawk-resume-v1"

type resumeTokens struct {
	key []byte
}

// newResumeTokens derives the token key. Precedence (docs/22 Decision 7):
// HKDF from the publish secret when one is set (rotating it revokes all
// tokens); else the chart-managed ResumeTokenKey; else a fresh per-process
// random key — the dev fallback, giving exactly today's process-lifetime
// reclaim semantics.
func newResumeTokens(cfg config.Config) *resumeTokens {
	if cfg.PublishSecret != "" {
		key, err := hkdf.Key(sha256.New, []byte(cfg.PublishSecret), nil, resumeKeyInfo, 32)
		if err != nil {
			// Unreachable with SHA-256 and a 32-byte request; fail loud.
			panic("transport: hkdf key derivation failed: " + err.Error())
		}
		return &resumeTokens{key: key}
	}
	if len(cfg.ResumeTokenKey) > 0 {
		return &resumeTokens{key: cfg.ResumeTokenKey}
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
