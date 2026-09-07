// Package ingest is R28's public write surface (docs/33 TM3).
//
// It is the ONLY part of gawk-telemetry a viewer's browser can reach, so its
// posture is deliberately narrow:
//
//   - **The envelope is protocol** (D15): wrong type, missing field, unknown
//     version, bad/expired/tampered token, oversize body, malformed
//     samples/events STRUCTURE ⇒ 400 or 401 and **nothing written**. A batch is
//     all-or-nothing; a partial write would leave a session's timeline with a
//     hole that reads exactly like a network gap.
//   - **The stats payload is data** (D15): tolerant reader, never rejects. See
//     internal/schema for why version skew makes this mandatory rather than
//     merely kind.
//   - **Zero PII** (D8): the source IP is never read, stored or logged. Rate
//     limiting uses it WITHOUT persisting it — the same posture R2's connection
//     limiter takes.
//
// The token is what makes an unauthenticated public write surface tolerable:
// it is a stateless HMAC any relay pod mints and this service verifies with no
// lookup, so the ingest accepts records only from clients that actually
// connected to a relay in this fleet (D2).
package ingest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// Batch is the ingest envelope (docs/33 §4.3). Pointer fields are how a
// MISSING field is told from a zero one: `seq: 0` is legitimate (it is the
// first batch), and a `final` that was never sent must not read as false by
// accident when the strict stance says the field is required.
type Batch struct {
	V            *int   `json:"v"`
	Token        string `json:"token"`
	Role         string `json:"role"`
	BroadcastKey string `json:"broadcastKey"`
	// RoomKey is R42's optional room handle (docs/44 §4.10, RM8): the relay's
	// HMAC'd room code, the same 6-byte shape as BroadcastKey, sent only by a
	// session that started inside a room. Absent or empty means "no room".
	RoomKey     string  `json:"roomKey"`
	Seq         *int    `json:"seq"`
	Final       *bool   `json:"final"`
	App         AppInfo `json:"app"`
	StartedAtMs *int64  `json:"startedAtMs"`
	// Stats stays RAW at envelope-decode time and is decoded separately below.
	// That split is load-bearing, not stylistic: Go's JSON decoder ERRORS on a
	// number that overflows float64, so decoding stats inline would let one
	// absurd value inside a PAYLOAD reject the whole batch — exactly the
	// strictness D15 rules out for payload data. Deferring it turns that into
	// a drop-and-count.
	Samples []struct {
		TMs   *float64        `json:"tMs"`
		Stats json.RawMessage `json:"stats"`
	} `json:"samples"`
	Events []struct {
		TMs    *float64 `json:"tMs"`
		Kind   string   `json:"kind"`
		Detail string   `json:"detail"`
	} `json:"events"`
	Truncated bool `json:"truncated"`
}

// AppInfo identifies the producing client. `Version` IS the schema version
// (D15) — no separate field, because every sample is already attributable to
// the exact release that produced it. `Browser` is a client CLASS
// ("Chrome 152", "gawk-broadcast"), reduced on the device: the raw user-agent
// string never leaves it.
type AppInfo struct {
	Version string `json:"version"`
	Surface string `json:"surface"`
	Browser string `json:"browser"`
	OS      string `json:"os"`
}

// Accepted is one validated batch, ready for the store. `SessionID` is the
// AUTHENTICATED one — derived from the verified token's nonce, never from
// anything the client asserted separately.
type Accepted struct {
	SessionID    string
	BroadcastKey string
	// RoomKey is empty for a session outside any room. When set it is 12 hex
	// chars; see Validate for why it is shape-checked but not authenticated.
	RoomKey     string
	Role        string
	Seq         int
	Final       bool
	Truncated   bool
	App         AppInfo
	StartedAtMs int64
	ReceivedAt  time.Time
	Samples     []Sample
	Events      []Event
	Anomalies   schema.Anomalies
}

// Sample is one sanitized stats observation.
type Sample struct {
	TMs   float64        `json:"tMs"`
	Stats map[string]any `json:"stats"`
}

// Event is one narrative point.
type Event struct {
	TMs    float64 `json:"tMs"`
	Kind   string  `json:"kind"`
	Detail string  `json:"detail,omitempty"`
}

// Sink consumes accepted batches. Implemented by the writer in TM3 and by the
// live projection in TM8 — one ingest path, several consumers.
type Sink interface {
	Accept(Accepted) error
}

// Options configure a Handler.
type Options struct {
	// Key is the fleet telemetry key. Required; without one no token can be
	// verified and the service should not be running at all.
	Key  []byte
	Sink Sink
	Log  *slog.Logger
	Now  func() time.Time
	// RatePerSec / Burst bound what the PROCESS will accept in total, before a
	// body is read — a cheap backstop against load, not a fairness mechanism.
	// It must be sized for the whole fleet: every browser reaches this through
	// the frontend Ingress, so there is no per-client discrimination available
	// here that a client could not forge (D8 — no IP is consulted or stored).
	RatePerSec float64
	Burst      float64
	// SessionRatePerSec / SessionBurst bound ONE verified session, applied
	// after the token check. This is where fairness lives: the session id is
	// authenticated, so unlike an IP or a header it cannot be borrowed.
	SessionRatePerSec float64
	SessionBurst      float64
}

// Handler serves POST /v1/ingest.
type Handler struct {
	opts Options
	log  *slog.Logger
	// Two tiers, in this order (review finding 2):
	//
	//   - `global` bounds the work this process will do at all, before a body
	//     is even read. It is one bucket for the whole fleet because that is
	//     what it actually was before — keyed on RemoteAddr, it degenerated to
	//     one bucket behind the frontend Ingress, just with a per-IP budget
	//     that capped the service far below its own target. Sizing it honestly
	//     is better than a per-IP number that only looks per-IP.
	//   - `perSession` is the real fairness bound, applied AFTER the token is
	//     verified: an authenticated session id is an identity the service can
	//     trust, unlike a header, and using it means no client IP is ever
	//     consulted, stored or bucketed (D8) — the promise the old comment made
	//     and the code did not quite keep.
	global     *keyLimiter
	perSession *keyLimiter
}

// New builds the ingest handler.
func New(opts Options) (*Handler, error) {
	if len(opts.Key) != wire.TelemetryKeySize {
		return nil, fmt.Errorf("ingest: telemetry key must be %d bytes", wire.TelemetryKeySize)
	}
	if opts.Sink == nil {
		return nil, errors.New("ingest: Sink is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RatePerSec <= 0 {
		opts.RatePerSec = DefaultGlobalRatePerSec
	}
	if opts.Burst <= 0 {
		opts.Burst = DefaultGlobalBurst
	}
	if opts.SessionRatePerSec <= 0 {
		opts.SessionRatePerSec = DefaultSessionRatePerSec
	}
	if opts.SessionBurst <= 0 {
		opts.SessionBurst = DefaultSessionBurst
	}
	return &Handler{
		opts:       opts,
		log:        opts.Log,
		global:     newKeyLimiter(opts.RatePerSec, opts.Burst, opts.Now),
		perSession: newKeyLimiter(opts.SessionRatePerSec, opts.SessionBurst, opts.Now),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// R37 (docs/40 D17): wildcard CORS, unconditionally, on the INGEST
	// listener only. Any gawk UI's sessions may report to any relay
	// operator's ingest — that is the cross-relay telemetry feature itself.
	// Wildcard is safe here because authentication is the in-body HMAC
	// token: no cookies, no credentialed requests, nothing ambient for a
	// foreign origin to ride on; every batch still dies without a valid
	// token for THIS fleet's key. (The pre-R37 origin allowlist was a
	// second `allowedOrigins` for every operator to forget, and the browsers
	// it gated could always POST via non-CORS carriers anyway.)
	//
	// The unload flush pairs with this server-side half via a CORS-safelisted
	// content type (text/plain) that sendBeacon can send cross-origin
	// without the preflight it cannot perform during unload; the envelope
	// bytes are identical, and this handler has never dispatched on
	// Content-Type.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.global.allow(globalBucket) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, schema.MaxBodyBytes+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > schema.MaxBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	accepted, status, err := h.Validate(body)
	if err != nil {
		// Deliberately terse: the client cannot act on detail, and a verbose
		// rejection is a probing oracle on a public endpoint. The reason is
		// logged (without the IP or the token).
		h.log.Debug("ingest rejected", "status", status, "err", err)
		w.WriteHeader(status)
		return
	}
	// Per-session, and therefore only now: the identity this bounds is the one
	// the token proved. A session that talks far faster than its own flush
	// cadence is either broken or hostile, and either way it must not spend
	// the fleet's budget.
	if !h.perSession.allow(accepted.SessionID) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if err := h.opts.Sink.Accept(accepted); err != nil {
		h.log.Warn("ingest sink failed", "session", accepted.SessionID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// 204: there is nothing useful to say back, and a body would only be
	// parsed by a client that should be fire-and-forget.
	w.WriteHeader(http.StatusNoContent)
}

// Validate applies the D15 split: strict envelope, tolerant payload. Exported
// so the ingest contract can be tested without an HTTP round trip.
//
// It NEVER partially accepts: either the whole batch is returned or nothing
// is, which is what makes "no partial write" a property of the design rather
// than of the caller's discipline.
func (h *Handler) Validate(body []byte) (Accepted, int, error) {
	// --- envelope: strict ---------------------------------------------------
	// A wrongly-typed envelope field fails here, which is the intent: unlike
	// the stats payload, the envelope is protocol. Unknown envelope keys are
	// tolerated (it may grow), but every declared one must have its type.
	var b Batch
	if err := json.Unmarshal(body, &b); err != nil {
		return Accepted{}, http.StatusBadRequest, fmt.Errorf("malformed envelope: %w", err)
	}

	if b.V == nil || *b.V != 1 {
		return Accepted{}, http.StatusBadRequest, errors.New("unsupported or missing envelope version")
	}
	if b.Seq == nil || *b.Seq < 0 {
		return Accepted{}, http.StatusBadRequest, errors.New("missing or negative seq")
	}
	if b.Final == nil {
		return Accepted{}, http.StatusBadRequest, errors.New("missing final")
	}
	if b.StartedAtMs == nil || *b.StartedAtMs <= 0 {
		return Accepted{}, http.StatusBadRequest, errors.New("missing or invalid startedAtMs")
	}
	if b.App.Version == "" || b.App.Surface == "" {
		return Accepted{}, http.StatusBadRequest, errors.New("missing app.version or app.surface")
	}
	role := wire.TelemetryRole(b.Role)
	if !wire.ValidTelemetryRole(role) {
		return Accepted{}, http.StatusBadRequest, fmt.Errorf("unknown role %q", b.Role)
	}
	// The role in the envelope and the surface in app must agree; disagreeing
	// would make a row's own identity ambiguous.
	if b.App.Surface != b.Role {
		return Accepted{}, http.StatusBadRequest, errors.New("role and app.surface disagree")
	}
	broadcastKey, err := hex.DecodeString(b.BroadcastKey)
	if err != nil || len(broadcastKey) != wire.TelemetryBroadcastKeySize {
		// This is also what stops a client reporting the RAW broadcast ID: the
		// field must be exactly the 6-byte obfuscated digest, and the token's
		// tag is computed over it, so anything else fails below anyway.
		return Accepted{}, http.StatusBadRequest, errors.New("invalid broadcastKey")
	}
	if b.RoomKey != "" {
		// Same shape rule as broadcastKey, so a client can no more report a
		// raw room code here than a raw broadcast ID above. Unlike
		// broadcastKey, the room key is NOT covered by the token's tag: the
		// token binds broadcastKey + role only (wire.MintTelemetrySessionToken),
		// and a session may enter a room after it obtained the token. So this
		// field is trusted only as a grouping HINT — shape-validated envelope,
		// not an authenticated identity — and nothing downstream may treat it
		// as proof of room membership.
		roomKey, err := hex.DecodeString(b.RoomKey)
		if err != nil || len(roomKey) != wire.RoomKeySize {
			return Accepted{}, http.StatusBadRequest, errors.New("invalid roomKey")
		}
	}
	token, err := hex.DecodeString(b.Token)
	if err != nil || len(token) != wire.TelemetrySessionTokenSize {
		return Accepted{}, http.StatusUnauthorized, errors.New("invalid token encoding")
	}

	sessionID, err := wire.VerifyTelemetrySessionToken(h.opts.Key, token, broadcastKey, role, h.opts.Now())
	if err != nil {
		// Expired and invalid are both 401: a client cannot act differently on
		// either (it needs a fresh session), and distinguishing them on a
		// public endpoint is a free oracle.
		return Accepted{}, http.StatusUnauthorized, fmt.Errorf("token rejected: %w", err)
	}

	if len(b.Samples) > schema.MaxSamplesPerBatch {
		return Accepted{}, http.StatusBadRequest, schema.ErrTooManySamples
	}
	if len(b.Events) > schema.MaxEventsPerBatch {
		return Accepted{}, http.StatusBadRequest, schema.ErrTooManyEvents
	}

	// --- payload: tolerant, but its STRUCTURE is envelope ------------------
	known := schema.FieldsForRole(b.Role)
	var anomalies schema.Anomalies
	samples := make([]Sample, 0, len(b.Samples))
	for i, s := range b.Samples {
		if s.TMs == nil || !isFinite(*s.TMs) {
			return Accepted{}, http.StatusBadRequest, fmt.Errorf("sample %d: missing or non-finite tMs", i)
		}
		// The SHAPE of stats is envelope (it must be an object); its CONTENTS
		// are payload. UseNumber keeps every number as an unconverted string
		// so the sanitizer can drop an unrepresentable one instead of the
		// decoder rejecting the batch over it.
		raw := map[string]any{}
		dec := json.NewDecoder(bytes.NewReader(s.Stats))
		dec.UseNumber()
		if err := dec.Decode(&raw); err != nil {
			return Accepted{}, http.StatusBadRequest, fmt.Errorf("sample %d: stats is not an object", i)
		}
		clean, an := schema.SanitizeStats(raw, known)
		anomalies.Add(an)
		samples = append(samples, Sample{TMs: *s.TMs, Stats: clean})
	}
	events := make([]Event, 0, len(b.Events))
	for i, e := range b.Events {
		if e.TMs == nil || !isFinite(*e.TMs) {
			return Accepted{}, http.StatusBadRequest, fmt.Errorf("event %d: missing or non-finite tMs", i)
		}
		if e.Kind == "" {
			return Accepted{}, http.StatusBadRequest, fmt.Errorf("event %d: empty kind", i)
		}
		events = append(events, Event{
			TMs:    *e.TMs,
			Kind:   clip(e.Kind, 64),
			Detail: clip(e.Detail, 256),
		})
	}

	return Accepted{
		SessionID:    sessionID,
		BroadcastKey: b.BroadcastKey,
		RoomKey:      b.RoomKey,
		Role:         b.Role,
		Seq:          *b.Seq,
		Final:        *b.Final,
		Truncated:    b.Truncated,
		App:          b.App,
		StartedAtMs:  *b.StartedAtMs,
		ReceivedAt:   h.opts.Now(),
		Samples:      samples,
		Events:       events,
		Anomalies:    anomalies,
	}, http.StatusNoContent, nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// clip bounds a string to n bytes. A plain byte slice can land inside a
// multi-byte rune (an emoji or accented letter in a free-text event detail),
// storing invalid UTF-8 that json.Marshal then silently rewrites to U+FFFD on
// every future read — corrupting the value rather than merely shortening it.
// So trim back to the last complete rune: never past n, only short of it,
// which is fine because n is a size CAP, not a target length.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// keyLimiter is a small token-bucket map. Deliberately not a general-purpose
// limiter: it holds nothing but a float and a timestamp per bucket, and it is
// swept so a long-running process cannot accumulate a record of who was here.
type keyLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newKeyLimiter(rate, burst float64, now func() time.Time) *keyLimiter {
	return &keyLimiter{rate: rate, burst: burst, now: now, buckets: map[string]*bucket{}, lastGC: now()}
}

func (l *keyLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	// Sweep on a timer: buckets are transient by design (D8), and a map that
	// grows for the process's life would be a de facto visitor log.
	if now.Sub(l.lastGC) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	b.last = now
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Limiter defaults. The global figures are sized from the product target
// rather than from a habit: ~1000 viewers on one hot broadcast, each flushing
// every 10 s, is ~100 requests/s steady, and unload flushes plus retry chains
// arrive in bursts around it. Bodies are already bounded at 1 MB and every
// accepted one is token-gated, so hundreds a second is cheap to serve and far
// below what the old effective ~50-session ceiling allowed.
//
// The burst covers the whole fleet flushing at once, which is not a
// hypothetical here: R17's rolling relay updates send every client to reconnect
// within the same second, and a limiter that sheds telemetry during a rollout
// would be blind exactly when a rollout goes wrong.
//
// The per-session figures are deliberately tight: a well-behaved client sends
// about one batch every 10 s, so 1/s with a burst of 10 covers a flush, an
// unload beacon and a full retry chain several times over while still catching
// a client that has come loose.
const (
	DefaultGlobalRatePerSec  = 300
	DefaultGlobalBurst       = 1200
	DefaultSessionRatePerSec = 1
	DefaultSessionBurst      = 10
)

// globalBucket is the single key the process-wide limiter uses. The limiter is
// a keyed map because the per-session tier needs one; the global tier is the
// degenerate case with one bucket, spelled out rather than special-cased.
const globalBucket = ""
