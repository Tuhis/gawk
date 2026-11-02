package status

import (
	"math"
	"time"
)

// Status is the GET /whip/status body (D9). camelCase, and every field the
// gateway doesn't know is omitted, except warnings, which is always present
// (possibly empty) so the app can iterate it without a nil check. It holds no
// IPs and no frame data, by construction: nothing in it is a string the
// client or the relay supplied, except the codec name the gateway chose.
type Status struct {
	State      string      `json:"state"`
	Codec      string      `json:"codec,omitempty"`
	Resolution *Resolution `json:"resolution,omitempty"`
	FPS        float64     `json:"fps,omitempty"`
	GOPMs      int         `json:"gopMs,omitempty"`
	// BitrateKbps, LossPct and Viewers are pointers where zero is a real
	// answer (no loss, nobody watching) that must not be omitted.
	BitrateKbps  int        `json:"bitrateKbps,omitempty"`
	LossPct      *float64   `json:"lossPct,omitempty"`
	Viewers      *uint32    `json:"viewers,omitempty"`
	Warnings     []Warning  `json:"warnings"`
	LastError    *ErrorInfo `json:"lastError,omitempty"`
	RetryAfterMs int64      `json:"retryAfterMs,omitempty"`
}

// Resolution is the coded picture size from the SPS (after cropping).
type Resolution struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Warning is one entry of warnings[]; Reason is from WarningReasons.
type Warning struct {
	Reason string `json:"reason"`
	GOPMs  int    `json:"gopMs,omitempty"`
}

// ErrorInfo is lastError; Reason is from ErrorReasons.
type ErrorInfo struct {
	Reason string `json:"reason"`
	GOPMs  int    `json:"gopMs,omitempty"`
	Codec  string `json:"codec,omitempty"`
}

// Snapshot is what a session measured, kept in the record so an ended
// session still reports its last values ("the key's current or last
// session", D9).
type Snapshot struct {
	Codec       string    `json:"codec,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	FPS         float64   `json:"fps,omitempty"`
	GOPMs       int       `json:"gopMs,omitempty"`
	BitrateKbps int       `json:"bitrateKbps,omitempty"`
	LossPct     *float64  `json:"lossPct,omitempty"`
	Viewers     *uint32   `json:"viewers,omitempty"`
	Warnings    []Warning `json:"warnings,omitempty"`
}

// Record is everything the gateway holds for one key (by the hash of the
// full key, never the ID: D9). It is JSON so D10's takeover can move it to
// another pod unchanged.
type Record struct {
	// UpdatedAt is wall-clock: D10 compares records across pods by it.
	UpdatedAt time.Time `json:"updatedAt"`
	// State is StateLive or StateEnded; a record with neither is idle.
	State   string    `json:"state,omitempty"`
	Session *Snapshot `json:"session,omitempty"`
	// LastError and LastErrorAt: the reason the key's last session ended,
	// or the last refusal of a POST before a session existed.
	LastError   *ErrorInfo `json:"lastError,omitempty"`
	LastErrorAt time.Time  `json:"lastErrorAt"`
	// BackoffUntil is D7's backoff end (zero or past: none runs), and
	// BackoffLevel how many backoffs this record has started: the next one
	// lasts Initial × 2^level, capped. The level lives as long as the record,
	// so a key that stays away for a record lifetime after its last backoff
	// ends starts again at 30 s. Retry now (DELETE /whip/status) clears the
	// backoff but keeps the level.
	BackoffUntil time.Time `json:"backoffUntil"`
	BackoffLevel int       `json:"backoffLevel,omitempty"`
	// Version is bumped on every change, so a takeover's ack drops exactly
	// the copy the receiving pod adopted (D10) and not a newer one.
	Version uint64 `json:"version"`
}

// BackoffRunning reports whether D7's backoff refuses POSTs at now.
func (r *Record) BackoffRunning(now time.Time) bool {
	return !r.BackoffUntil.IsZero() && now.Before(r.BackoffUntil)
}

// Live reports whether the record describes a live session.
func (r *Record) Live() bool { return r.State == StateLive }

// expiresAt is when a non-live record is forgotten: the record lifetime
// after its last change or after its backoff's end, whichever is later. A
// record therefore outlives its backoff by one lifetime, so OBS's reconnect
// right after a backoff ends still finds the ladder's level and the next
// end climbs it (30 s → 60 s → … → 10 min) instead of starting over.
func (r *Record) expiresAt(ttl time.Duration) time.Time {
	from := r.UpdatedAt
	if r.BackoffUntil.After(from) {
		from = r.BackoffUntil
	}
	return from.Add(ttl)
}

// Status renders the record (D9). A live record's measured values come from
// the session itself; the gateway passes them as live, which wins over the
// stored snapshot.
func (r *Record) Status(now time.Time, live *Snapshot) Status {
	st := Status{State: StateIdle, Warnings: []Warning{}}
	if r == nil {
		return st
	}
	switch r.State {
	case StateLive, StateEnded:
		st.State = r.State
	}
	snap := r.Session
	if live != nil {
		snap = live
	}
	if snap != nil && st.State != StateIdle {
		st.Codec = snap.Codec
		if snap.Width > 0 && snap.Height > 0 {
			st.Resolution = &Resolution{Width: snap.Width, Height: snap.Height}
		}
		st.FPS = snap.FPS
		st.GOPMs = snap.GOPMs
		st.BitrateKbps = snap.BitrateKbps
		if st.State == StateLive {
			// Loss and viewers describe the stream now; an ended session has
			// neither, and a stale "3 watching" would mislead.
			st.LossPct = snap.LossPct
			st.Viewers = snap.Viewers
			if len(snap.Warnings) > 0 {
				st.Warnings = append(st.Warnings, snap.Warnings...)
			}
		}
	}
	if r.LastError != nil {
		e := *r.LastError
		st.LastError = &e
	}
	if r.BackoffRunning(now) {
		st.RetryAfterMs = int64(math.Ceil(float64(r.BackoffUntil.Sub(now)) / float64(time.Millisecond)))
	}
	return st
}

// BackoffPolicy is D7's ladder: Initial, doubling per repeat, capped at Max.
type BackoffPolicy struct {
	Initial time.Duration
	Max     time.Duration
}

// Duration is the backoff a record at level gets.
func (p BackoffPolicy) Duration(level int) time.Duration {
	d := p.Initial
	for i := 0; i < level && d < p.Max; i++ {
		d *= 2
	}
	if d > p.Max {
		d = p.Max
	}
	return d
}

// StartBackoff starts the next rung on r.
func (p BackoffPolicy) StartBackoff(r *Record, now time.Time) time.Duration {
	d := p.Duration(r.BackoffLevel)
	r.BackoffUntil = now.Add(d)
	r.BackoffLevel++
	return d
}
