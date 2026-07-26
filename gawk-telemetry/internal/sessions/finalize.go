package sessions

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// Verdicter computes a session's stored verdict at finalize (TM6). Optional:
// TM3 alone runs with none, and the row then simply carries no `verdict` —
// which an old-row reader must tolerate anyway (D4's additive rule).
type Verdicter func(rollup.Row, rollup.Input) json.RawMessage

// RollupFinalizer returns a Finalizer that computes the permanent row from a
// session's stored timeline and appends it (docs/33 TM5).
//
// Reading the row back out of the STORED lines rather than accumulating it as
// batches arrive is deliberate: the row is then computed from exactly what a
// later reader will see, so a bug that mangles a line on the way to disk shows
// up in the rollup instead of being papered over by an in-memory copy that was
// never written.
func RollupFinalizer(st *store.Store, log *slog.Logger, verdict Verdicter, now func() time.Time) Finalizer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return func(live Live, lines [][]byte) {
		in := ParseTimeline(live, lines)
		if in.EndedAtMs == 0 {
			in.EndedAtMs = now().UnixMilli()
		}
		row := rollup.Compute(in)
		if verdict != nil {
			row.Verdict = verdict(row, in)
		}
		b, err := json.Marshal(row)
		if err != nil {
			log.Warn("rollup marshal failed", "session", live.Ref.SessionID, "err", err)
			return
		}
		// The rollup goes in the partition of the DAY THE SESSION ENDED, which
		// is what "sessions from yesterday" means to someone asking. The raw
		// file stays in its start-day partition; the row carries both
		// timestamps, so nothing is ambiguous.
		date := time.UnixMilli(in.EndedAtMs).UTC().Format(store.DateLayout)
		if err := st.AppendRollup(date, b); err != nil {
			log.Warn("rollup append failed", "session", live.Ref.SessionID, "err", err)
		}
	}
}

// ParseTimeline turns a session's stored NDJSON back into rollup input.
// Exported because the read API replays exactly the same parse for a stored
// session (one parser, so a stored row and a re-read timeline can never
// disagree about what the session contained).
func ParseTimeline(live Live, lines [][]byte) rollup.Input {
	in := rollup.Input{
		SessionID:    live.Ref.SessionID,
		BroadcastKey: live.Ref.BroadcastKey,
		Role:         live.Role,
		AppVersion:   live.App.Version,
		Browser:      live.App.Browser,
		OS:           live.App.OS,
		StartedAtMs:  live.StartedAtMs,
		EndedCleanly: live.EndedCleanly,
		Anomalies:    live.Anomalies,
		SeqGaps:      live.SeqGaps,
		Truncated:    live.Truncated,
	}
	if !live.LastSeen.IsZero() {
		in.EndedAtMs = live.LastSeen.UnixMilli()
	}
	var lastReceived int64
	for _, ln := range lines {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			// A line that will not parse is a line a reader would skip too;
			// skipping it here keeps the rollup consistent with what any
			// consumer of the same file sees.
			continue
		}
		// Identity from the RECORDS, not only from the live state. Every stored
		// line carries its own sessionId/broadcastKey/role precisely so a
		// session read back from disk is self-describing — the read API passes
		// an empty Live, and without this the role would be lost and every
		// role-scoped rule silently skipped.
		if in.Role == "" && rec.Role != "" {
			in.Role = rec.Role
		}
		if in.BroadcastKey == "" && rec.BroadcastKey != "" {
			in.BroadcastKey = rec.BroadcastKey
		}
		if in.SessionID == "" && rec.SessionID != "" {
			in.SessionID = rec.SessionID
		}
		switch rec.Kind {
		case "meta":
			if rec.App != nil {
				if in.AppVersion == "" {
					in.AppVersion = rec.App.Version
				}
				if in.Browser == "" {
					in.Browser = rec.App.Browser
				}
				if in.OS == "" {
					in.OS = rec.App.OS
				}
			}
			if in.StartedAtMs == 0 {
				in.StartedAtMs = rec.StartedAtMs
			}
		case "sample":
			in.Samples = append(in.Samples, rollup.Sample{TMs: rec.TMs, Stats: rec.Stats})
		case "event":
			in.Events = append(in.Events, rollup.Event{TMs: rec.TMs, Kind: rec.Event, Detail: rec.Detail})
		}
		// The service's own clock on the last line we saw. Used only when the
		// caller has no live state to supply an end time (the read path).
		if rec.ReceivedAtMs > lastReceived {
			lastReceived = rec.ReceivedAtMs
		}
	}
	if in.EndedAtMs == 0 {
		in.EndedAtMs = lastReceived
	}
	return in
}
