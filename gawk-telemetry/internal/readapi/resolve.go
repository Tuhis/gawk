package readapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Finding a specific stream on the dashboard is the one thing the zero-PII
// envelope makes awkward. The page can only ever show the OBFUSCATED broadcast
// key, because that is the only identity telemetry is allowed to learn: the raw
// six-character code is a join credential (~31^6, brute-forceable), R2 keyed
// /statusz by an HMAC for exactly that reason, and D8 makes the client
// structurally incapable of reporting one — the session token's HMAC binds the
// obfuscated key, so a batch carrying anything else is rejected before a byte is
// written.
//
// None of that is relaxed here. The mapping runs in ONE direction, server-side:
// an operator supplies a code they already hold, the service computes the digest
// the relay would have published for it, and the page highlights that row. The
// code is never stored, never logged and never reaches the client's telemetry;
// the statsKey never leaves the cluster.
//
// The cost, stated rather than buried: a service holding the statsKey can
// enumerate join codes for the broadcasts it has stored (31^6 HMACs is minutes
// of laptop time). That is why the key is OPTIONAL and unset by default —
// turning it on is a deliberate act, the same posture query_sql takes.

// maxCodeLen bounds the request body's code field. Deliberately not the
// six-character ID length: this is a sanity bound on untrusted input, not a
// validation of the alphabet. The alphabet has exactly one home per language
// (gawk-server/internal/broadcastid) and re-declaring it here to reject a typo
// would be a second copy that can drift. A wrong code simply resolves to a
// digest that matches no row on the page, which is the right answer anyway.
const maxCodeLen = 64

// obfuscate mirrors Registry.ObfuscateID (gawk-server hub.go:1354) — the same
// construction, deliberately spelled out rather than imported, because it lives
// in gawk-server's internal/ and is not reachable from this module. If that
// construction ever changes, this is the other end that must change with it.
func obfuscate(statsKey []byte, code string) string {
	mac := hmac.New(sha256.New, statsKey)
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil)[:6])
}

type resolveRequest struct {
	Code string `json:"code"`
	// Room (R42, RM8) is a room code — a dynamic six-character one or a static
	// slug. Exactly one of Code and Room is expected; the response carries the
	// key that matches the field that was sent.
	Room string `json:"room"`
}

type resolveResponse struct {
	BroadcastKey string `json:"broadcastKey,omitempty"`
	RoomKey      string `json:"roomKey,omitempty"`
}

// handleResolve serves POST /v1/resolve. POST rather than GET, with the code in
// the body: a join credential in a query string ends up in browser history, the
// Referer header and any proxy's access log, and D8's whole posture is about not
// writing this class of value down.
func (a *API) handleResolve(w http.ResponseWriter, r *http.Request) {
	if len(a.statsKey) == 0 {
		http.Error(w, "resolve is not configured: no stats key", http.StatusNotImplemented)
		return
	}
	var req resolveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if req.Room != "" {
		// R42 (RM8): a room key is the SAME construction over the room's
		// normalized code — roomsrv keys /statusz, metrics and the RoomState
		// `key` field with Registry.ObfuscateID(rooms.NormalizeCode(code)).
		// NormalizeCode LOWER-cases (the code doubles as a DNS-1123 CR name),
		// where broadcast IDs upper-case; getting that wrong would resolve
		// every room to a key that matches nothing. Only the case rule is
		// mirrored, not the slug alphabet — same reasoning as maxCodeLen.
		room := strings.ToLower(strings.TrimSpace(req.Room))
		if room == "" || len(room) > maxCodeLen {
			http.Error(w, "room must be 1..64 characters", http.StatusBadRequest)
			return
		}
		writeJSON(w, resolveResponse{RoomKey: obfuscate(a.statsKey, room)}, nil)
		return
	}
	// Upper-casing mirrors broadcastid.Normalize's tolerance, so a code pasted
	// from a chat message in the wrong case still finds its stream. This is
	// input handling, not the alphabet.
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if code == "" || len(code) > maxCodeLen {
		http.Error(w, "code must be 1..64 characters", http.StatusBadRequest)
		return
	}
	writeJSON(w, resolveResponse{BroadcastKey: obfuscate(a.statsKey, code)}, nil)
}
