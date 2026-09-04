// Package sessions turns accepted ingest batches into stored session files
// and, at finalize, into permanent rollup rows (docs/33 TM3 + TM5).
//
// It owns the one piece of state the ingest path itself deliberately has none
// of: what is currently open. A session is "open" from its first batch until
// either a `final` batch arrives (the clean end — a viewer that stopped
// watching, a broadcaster that stopped broadcasting) or it goes quiet for
// longer than the idle timeout (the ordinary end — a browser tab closed
// mid-stream sends no final batch, and neither does a crashed one).
//
// Both paths converge on the same finalize: gzip the raw file, compute the
// rollup, append the permanent row. That symmetry is why "a session that
// vanished" and "a session that ended" produce identical artifacts, differing
// only in an `endedCleanly` flag on the row.
package sessions

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// DefaultIdleTimeout is how long a session may go unheard-from before it is
// finalized. Comfortably longer than the 10 s batch cadence plus a retry
// chain, so a client riding out a network blip is not finalized underneath
// itself and then re-opened as a second session.
const DefaultIdleTimeout = 2 * time.Minute

// Record is one stored NDJSON line. Every line is self-describing: the reader
// never depends on file position or on a header line, so a truncated file is
// still readable up to its last complete line.
type Record struct {
	// Kind is "meta", "sample" or "event".
	Kind string `json:"kind"`
	// SessionID/BroadcastKey/Role repeat on every line. Cheap (they gzip to
	// nothing across near-identical records) and they make any single line
	// meaningful on its own — which is what makes the DuckDB recipe in D11
	// work over a glob without a join.
	SessionID    string `json:"sessionId"`
	BroadcastKey string `json:"broadcastKey"`
	Role         string `json:"role"`
	// RoomKey (R42, RM8) repeats on every line for the same reason the three
	// above do: a room's sessions are found with one filter over the glob.
	// Omitted when the session was not in a room, so pre-R42 files and
	// room-less sessions are byte-identical to before.
	RoomKey string `json:"roomKey,omitempty"`

	// Meta lines only.
	App         *ingest.AppInfo `json:"app,omitempty"`
	StartedAtMs int64           `json:"startedAtMs,omitempty"`

	// Sample/event lines.
	TMs    float64        `json:"tMs,omitempty"`
	Stats  map[string]any `json:"stats,omitempty"`
	Event  string         `json:"event,omitempty"`
	Detail string         `json:"detail,omitempty"`

	// ReceivedAtMs is the SERVICE's clock, not the client's. Keeping both is
	// what lets a client with a skewed clock still be placed on a fleet
	// timeline (D7's provenance instinct, applied to time).
	ReceivedAtMs int64 `json:"receivedAtMs"`
}

// Live is the in-memory state of one open session.
type Live struct {
	Ref store.SessionRef
	// RoomKey is the HMAC'd room this session reported, or empty (R42, RM8).
	RoomKey      string
	Role         string
	App          ingest.AppInfo
	StartedAtMs  int64
	FirstSeen    time.Time
	LastSeen     time.Time
	Samples      int
	Events       int
	Anomalies    schema.Anomalies
	Truncated    bool
	SeqGaps      int
	nextExpected int
	metaWritten  bool
	EndedCleanly bool
}

// Finalizer is called once per session when it ends, with the session's full
// stored line set. TM5 supplies the rollup computation; TM3 alone can run with
// a nil one.
type Finalizer func(Live, [][]byte)

// Observer sees every accepted batch as it lands, for the TM8 live projection.
// Called on the ingest goroutine, so it must be cheap and must not block.
type Observer func(Live, ingest.Accepted)

// Options configure a Writer.
type Options struct {
	Store       *store.Store
	Log         *slog.Logger
	Now         func() time.Time
	IdleTimeout time.Duration
	Finalize    Finalizer
	Observe     Observer
}

// Writer implements ingest.Sink.
type Writer struct {
	opts Options
	log  *slog.Logger

	mu   sync.Mutex
	live map[string]*Live
	// finalized remembers sessions whose permanent rollup row has already been
	// written. A late batch — a retry that outlived the idle sweep, or a final
	// flush that lost the race — must NOT re-create the session, because
	// finalizing it again writes a SECOND rollup row for one session and
	// duplicate rows corrupt every query over the permanent artifact (D4).
	// Such a batch is dropped and counted instead: losing a few seconds of
	// samples is strictly better than losing the ability to trust a count.
	finalized   map[string]time.Time
	lateBatches uint64
	duplicates  uint64
}

// NewWriter builds the session writer.
func NewWriter(opts Options) *Writer {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	return &Writer{
		opts: opts, log: opts.Log,
		live:      map[string]*Live{},
		finalized: map[string]time.Time{},
	}
}

// Accept stores one validated batch.
func (w *Writer) Accept(a ingest.Accepted) error {
	now := a.ReceivedAt
	if now.IsZero() {
		now = w.opts.Now()
	}

	w.mu.Lock()
	// A batch for a session that has already been finalized is dropped rather
	// than re-opening it (see the `finalized` field). The tombstone is kept
	// for one idle timeout — long enough to cover a retry chain, short enough
	// that a genuinely new session reusing the id (impossible in practice: the
	// nonce is random) would never collide.
	if when, done := w.finalized[a.SessionID]; done {
		if now.Sub(when) < w.opts.IdleTimeout {
			w.lateBatches++
			w.mu.Unlock()
			w.log.Debug("dropped a batch for an already-finalized session",
				"session", a.SessionID, "seq", a.Seq, "final", a.Final)
			return nil
		}
		delete(w.finalized, a.SessionID)
	}
	s, ok := w.live[a.SessionID]
	if !ok {
		s = &Live{
			Ref: store.SessionRef{
				// A session's partition is fixed at its FIRST batch. A session
				// spanning midnight therefore stays in one file rather than
				// splitting across two partitions, which would make the
				// "one writer per file" invariant a lie and leave half a
				// timeline in each.
				Date:         now.UTC().Format(store.DateLayout),
				BroadcastKey: a.BroadcastKey,
				SessionID:    a.SessionID,
			},
			RoomKey:     a.RoomKey,
			Role:        a.Role,
			App:         a.App,
			StartedAtMs: a.StartedAtMs,
			FirstSeen:   now,
		}
		w.live[a.SessionID] = s
	}
	// A room key that arrives on a LATER batch (a resumed session whose first
	// batch was lost, or a client that only learned its room after its first
	// flush) still attaches; a session never leaves a room by omitting it.
	if s.RoomKey == "" && a.RoomKey != "" {
		s.RoomKey = a.RoomKey
	}
	// Ingest is at-least-once: a POST the service processed but whose response
	// was lost is retried by both collectors, and appending it again would
	// count its samples and events twice in the permanent rollup. A batch
	// numbered below what this session has already seen is that retry, and it
	// is dropped — the client is told 204, because from its side the delivery
	// did succeed (review finding 8).
	if !ok && a.Seq > 0 {
		// A first-batch-for-this-session that is not seq 0 is a resumed
		// session, not a duplicate: accept it and let the gap accounting say
		// what was missed.
		s.nextExpected = a.Seq
	}
	if a.Seq < s.nextExpected {
		w.duplicates++
		w.mu.Unlock()
		w.log.Debug("dropped a duplicate batch", "session", a.SessionID, "seq", a.Seq)
		return nil
	}
	// A gap in `seq` means a batch was dropped after its retries (the client
	// says so by numbering, not by apologising). Recorded, never fatal — it is
	// exactly the "coverage is imperfect here" signal a verdict needs.
	if a.Seq > s.nextExpected {
		s.SeqGaps += a.Seq - s.nextExpected
	}
	s.nextExpected = a.Seq + 1
	s.LastSeen = now
	s.Samples += len(a.Samples)
	s.Events += len(a.Events)
	s.Anomalies.Add(a.Anomalies)
	if a.Truncated {
		s.Truncated = true
	}
	needMeta := !s.metaWritten
	s.metaWritten = true
	ref := s.Ref
	snapshot := *s
	w.mu.Unlock()

	lines := make([][]byte, 0, len(a.Samples)+len(a.Events)+1)
	recvMs := now.UnixMilli()
	if needMeta {
		app := a.App
		lines = appendJSON(lines, Record{
			Kind:         "meta",
			SessionID:    a.SessionID,
			BroadcastKey: a.BroadcastKey,
			RoomKey:      snapshot.RoomKey,
			Role:         a.Role,
			App:          &app,
			StartedAtMs:  a.StartedAtMs,
			ReceivedAtMs: recvMs,
		})
	}
	for _, sm := range a.Samples {
		lines = appendJSON(lines, Record{
			Kind:         "sample",
			SessionID:    a.SessionID,
			BroadcastKey: a.BroadcastKey,
			RoomKey:      snapshot.RoomKey,
			Role:         a.Role,
			TMs:          sm.TMs,
			Stats:        sm.Stats,
			ReceivedAtMs: recvMs,
		})
	}
	for _, ev := range a.Events {
		lines = appendJSON(lines, Record{
			Kind:         "event",
			SessionID:    a.SessionID,
			BroadcastKey: a.BroadcastKey,
			RoomKey:      snapshot.RoomKey,
			Role:         a.Role,
			TMs:          ev.TMs,
			Event:        ev.Kind,
			Detail:       ev.Detail,
			ReceivedAtMs: recvMs,
		})
	}

	if err := w.opts.Store.AppendSession(ref, lines); err != nil {
		return err
	}
	if w.opts.Observe != nil {
		w.opts.Observe(snapshot, a)
	}
	if a.Final {
		w.finalize(a.SessionID, true)
	}
	return nil
}

// SweepIdle finalizes sessions that have gone quiet. Returns how many.
//
// This is the ordinary end of a session, not the exceptional one: a closed
// browser tab sends no final batch, and neither does a crashed client or one
// whose network vanished — which are precisely the sessions worth having.
func (w *Writer) SweepIdle() int {
	cutoff := w.opts.Now().Add(-w.opts.IdleTimeout)
	w.mu.Lock()
	var stale []string
	for id, s := range w.live {
		if s.LastSeen.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	w.mu.Unlock()

	for _, id := range stale {
		w.finalize(id, false)
	}
	return len(stale)
}

// FinalizeAll ends every open session — the shutdown path, so a redeploy does
// not leave a fleet's worth of sessions to the orphan sweep.
func (w *Writer) FinalizeAll() {
	w.mu.Lock()
	ids := make([]string, 0, len(w.live))
	for id := range w.live {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	for _, id := range ids {
		w.finalize(id, false)
	}
}

// LateBatches counts batches dropped because their session was already
// finalized. A nonzero value on a healthy fleet means the idle timeout is
// shorter than the clients' flush cadence — the configuration that shreds one
// session into several rows.
func (w *Writer) LateBatches() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lateBatches
}

// Duplicates counts batches dropped because their sequence number had already
// been stored. A steady trickle is normal on a lossy network — it is the
// at-least-once delivery working — and it is counted so it can never be
// mistaken for the client sending more data than it does.
func (w *Writer) Duplicates() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.duplicates
}

// LiveSessions returns a snapshot of what is currently open.
func (w *Writer) LiveSessions() []Live {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Live, 0, len(w.live))
	for _, s := range w.live {
		out = append(out, *s)
	}
	return out
}

func (w *Writer) finalize(sessionID string, clean bool) {
	w.mu.Lock()
	s, ok := w.live[sessionID]
	if !ok {
		w.mu.Unlock()
		return
	}
	delete(w.live, sessionID)
	w.finalized[sessionID] = w.opts.Now()
	// Bound the tombstone map: entries older than two idle timeouts can no
	// longer suppress anything.
	cutoff := w.opts.Now().Add(-2 * w.opts.IdleTimeout)
	for id, when := range w.finalized {
		if when.Before(cutoff) {
			delete(w.finalized, id)
		}
	}
	s.EndedCleanly = clean
	snapshot := *s
	w.mu.Unlock()

	// Read the raw lines BEFORE gzipping: the rollup is computed from the
	// session's whole timeline, and reading the plain file avoids a
	// compress-then-decompress round trip on every session end.
	lines, err := w.opts.Store.ReadSession(snapshot.Ref)
	if err != nil {
		w.log.Warn("session finalize: read failed", "session", sessionID, "err", err)
	}
	if w.opts.Finalize != nil {
		w.opts.Finalize(snapshot, lines)
	}
	if err := w.opts.Store.FinalizeSession(snapshot.Ref); err != nil {
		w.log.Warn("session finalize: gzip failed", "session", sessionID, "err", err)
	}
}

func appendJSON(dst [][]byte, r Record) [][]byte {
	b, err := json.Marshal(r)
	if err != nil {
		// Unreachable: every field is a plain type or a map that already
		// survived json.Unmarshal. Dropping the line is still better than
		// failing a whole batch over one record.
		return dst
	}
	return append(dst, b)
}
