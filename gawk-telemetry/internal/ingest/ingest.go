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
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// Batch is the ingest envelope (docs/33 §4.3). Pointer fields are how a
// MISSING field is told from a zero one: `seq: 0` is legitimate (it is the
// first batch), and a `final` that was never sent must not read as false by
// accident when the strict stance says the field is required.
type Batch struct {
	V            *int    `json:"v"`
	Token        string  `json:"token"`
	Role         string  `json:"role"`
	BroadcastKey string  `json:"broadcastKey"`
	Seq          *int    `json:"seq"`
	Final        *bool   `json:"final"`
	App          AppInfo `json:"app"`
	StartedAtMs  *int64  `json:"startedAtMs"`
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
	Role         string
	Seq          int
	Final        bool
	Truncated    bool
	App          AppInfo
	StartedAtMs  int64
	ReceivedAt   time.Time
	Samples      []Sample
	Events       []Event
	Anomalies    schema.Anomalies
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
	// RatePerSec / Burst bound one source IP's request rate. The IP is used
	// and NEVER persisted (D8) — the limiter's map is keyed by a truncated
	// form and swept, and nothing it holds reaches a file or a log line.
	RatePerSec float64
	Burst      float64
	// AllowedOrigins enables CORS for the SPLIT-ORIGIN deployment only — an
	// operator who pointed config.telemetryUrl at a different host (D1). The
	// default same-origin deployment needs none of this and gets none of it:
	// an empty list means no CORS headers and no preflight handling at all,
	// so the common case has no cross-origin surface to reason about.
	//
	// Note what this does NOT rescue: `sendBeacon` cannot perform a
	// preflight, so in split-origin mode the unload flush is lost and only
	// the periodic one survives. That is a property of the browser, not of
	// this allowlist, and it is why same-origin is the default.
	AllowedOrigins []string
}

// Handler serves POST /v1/ingest.
type Handler struct {
	opts    Options
	log     *slog.Logger
	limiter *ipLimiter
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
		opts.RatePerSec = 5
	}
	if opts.Burst <= 0 {
		opts.Burst = 20
	}
	return &Handler{
		opts:    opts,
		log:     opts.Log,
		limiter: newIPLimiter(opts.RatePerSec, opts.Burst, opts.Now),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS, only where an operator explicitly split the origins. Echoing the
	// request's Origin (rather than "*") keeps the allowlist meaningful and
	// keeps the response uncacheable across origins.
	origin := r.Header.Get("Origin")
	if origin != "" && h.originAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	if r.Method == http.MethodOptions {
		if origin == "" || !h.originAllowed(origin) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
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
	if !h.limiter.allow(clientIPBucket(r)) {
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
	if err := h.opts.Sink.Accept(accepted); err != nil {
		h.log.Warn("ingest sink failed", "session", accepted.SessionID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// 204: there is nothing useful to say back, and a body would only be
	// parsed by a client that should be fire-and-forget.
	w.WriteHeader(http.StatusNoContent)
}

// originAllowed reports whether an explicitly-listed origin may post here.
// Exact match only: a prefix or suffix rule on an origin is how allowlists
// get bypassed.
func (h *Handler) originAllowed(origin string) bool {
	for _, o := range h.opts.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
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

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// clientIPBucket derives a rate-limit key from the request WITHOUT persisting
// it (D8). The value never leaves this process's memory, never reaches a file
// and never reaches a log line — the limiter's whole state is swept on a
// timer.
func clientIPBucket(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Behind the frontend Ingress every request arrives from the proxy, so an
	// X-Forwarded-For would be the real discriminator. It is deliberately NOT
	// consulted: it is client-controlled, trivially spoofed to defeat exactly
	// this limiter, and consulting it would mean handling real client IPs the
	// service has promised not to handle. Rate limiting here is a cheap
	// backstop, not the security boundary — the token is.
	return host
}

// ipLimiter is a small token-bucket map. Deliberately not a general-purpose
// limiter: it holds nothing but a float and a timestamp per bucket, and it is
// swept so a long-running process cannot accumulate a record of who connected.
type ipLimiter struct {
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

func newIPLimiter(rate, burst float64, now func() time.Time) *ipLimiter {
	return &ipLimiter{rate: rate, burst: burst, now: now, buckets: map[string]*bucket{}, lastGC: now()}
}

func (l *ipLimiter) allow(key string) bool {
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
