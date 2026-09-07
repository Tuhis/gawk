package readapi

// TH3's history browser (docs/36 §4 Wave 1).
//
// Two rules shape every line of this file.
//
// **UD4 — filtering, sorting, bucketing and pagination are server-side.**
// Shipping 14 days of rollups to a browser to filter them there is the same
// category error as returning 80 fields to a model. So the browser sends a
// query and receives a page; it never holds the unfiltered set.
//
// **UD1 — the machine surface does not move.** `ListSessions`/`ListBroadcasts`
// and their `/v1/sessions`/`/v1/broadcasts` routes are untouched, down to the
// bytes. This is a SECOND, UI-shaped surface at `/v1/history/*` with its own
// declared bounds, not a widening of the first: MCP's default response is what
// the 32 KB ceiling is asserted against, and a browser wanting one more column
// is not a reason to spend a model's context on it.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// DefaultHistoryLimit is one page of the history browser. Larger than the MCP
// row limit because a scrolling virtualized list is a different consumer from a
// context window (UD1, UD12).
const DefaultHistoryLimit = 200

// MaxHistoryLimit bounds what one request may ask for.
const MaxHistoryLimit = 2000

// HistoryQuery is TH3's filter set. Every field is optional; the zero value is
// "everything in range, newest first".
type HistoryQuery struct {
	From, To     time.Time
	BroadcastKey string
	// RoomKey (R42, RM8) scopes the page to one room's sessions — every
	// broadcast attached to it, every viewer that watched from inside it.
	// This filter plus client-side grouping by broadcastKey IS the rooms
	// view; no separate rooms endpoint exists.
	RoomKey      string
	Role         string
	Verdict      string
	Browser      string
	OS           string
	AppVersion   string
	DeliveryMode string
	// HasFindings and Distrusted are three-state on purpose: nil is "do not
	// filter", which is a different query from "only rows WITHOUT findings".
	// A bool would have made the second unaskable.
	HasFindings *bool
	Distrusted  *bool
	// Sort is one of severity | start | duration | stalls. Anything else falls
	// back to start, which is the ordering a list of events has by default.
	Sort string
	// Asc flips the sort. The defaults are chosen per column so that "worst
	// first" and "newest first" both need no flag.
	Asc    bool
	Cursor string
	Limit  int
}

// HistoryRow is TH3's row: the existing SessionSummary projection plus the
// columns a human scanning a list needs and a model does not.
type HistoryRow struct {
	SessionSummary
	// RoomKey (R42, RM8) is on the HISTORY row, not on SessionSummary: the
	// summary is `/v1/sessions`' MCP default and UD1 keeps that byte-identical.
	RoomKey      string `json:"roomKey,omitempty"`
	AppVersion   string `json:"appVersion,omitempty"`
	EndedAtMs    int64  `json:"endedAtMs,omitempty"`
	DeliveryMode string `json:"deliveryMode,omitempty"`
	// Verdict is the WORST finding's sentence from the STORED report (UD2).
	// Never recomputed: it was computed while the raw window still existed, and
	// by now that window may be pruned.
	Verdict string `json:"verdict,omitempty"`
	// Findings is how many rules fired, so a row can show "3 findings" without
	// the page fetching every report.
	Findings int `json:"findings"`
	// ComputedAtMs is when the stored verdict was computed — UD2 again: a
	// verdict is a claim made at a time, under the thresholds of that day.
	ComputedAtMs int64 `json:"computedAtMs,omitempty"`
	// RollupOnly marks a row whose raw window has been pruned. Opening it gets
	// the rollup view, not an empty chart (UD10).
	RollupOnly bool `json:"rollupOnly,omitempty"`
	// Live marks a session the projection currently holds open.
	Live bool `json:"live,omitempty"`
}

// HistoryPage is one page plus everything needed to state what is NOT in it.
type HistoryPage struct {
	Rows []HistoryRow `json:"rows"`
	// Total is the match count BEFORE paging, so a list can say "1 of 340".
	Total      int    `json:"total"`
	NextCursor string `json:"nextCursor,omitempty"`
	Coverage   `json:"coverage"`
}

// Coverage is UD10 as a wire type: what this answer does and does not rest on.
//
// The failure it exists to prevent is concluding "nothing was wrong" from
// "nothing was kept". A range with no data and a range the service cannot
// answer look identical in a bare array of rows, and they are completely
// different facts.
type Coverage struct {
	// RawFromMs is the raw-retention boundary. Sessions that started before it
	// have had their per-sample timeline pruned; their permanent rollup rows
	// remain (D4).
	RawFromMs int64 `json:"rawFromMs"`
	// RollupsFromMs is the oldest rollup partition on disk — the true left edge
	// of what this service knows anything about.
	RollupsFromMs int64 `json:"rollupsFromMs,omitempty"`
	// RetentionDays is the configured raw window, so the UI can name it.
	RetentionDays int `json:"retentionDays,omitempty"`
	// Note states, in one sentence, any way the requested range exceeds what is
	// answerable. Empty means the range is fully covered.
	Note string `json:"note,omitempty"`
}

// SearchSessions is the history browser's query (TH3).
func (a *API) SearchSessions(q HistoryQuery) (*HistoryPage, error) {
	rows, err := a.rollups(q.From)
	if err != nil {
		return nil, err
	}
	cov, err := a.coverage(q)
	if err != nil {
		return nil, err
	}
	liveIDs := a.liveSessionIDs()

	out := make([]HistoryRow, 0, len(rows))
	for _, r := range rows {
		hr, ok := a.historyRow(r, q, cov.RawFromMs, liveIDs)
		if ok {
			out = append(out, hr)
		}
	}
	sortHistory(out, q)
	total := len(out)
	page, next := paginate(out, q.Cursor, q.Limit, DefaultHistoryLimit, MaxHistoryLimit)
	return &HistoryPage{Rows: page, Total: total, NextCursor: next, Coverage: cov}, nil
}

func (a *API) historyRow(r rollup.Row, q HistoryQuery, rawFromMs int64, live map[string]bool) (HistoryRow, bool) {
	if q.BroadcastKey != "" && r.BroadcastKey != q.BroadcastKey {
		return HistoryRow{}, false
	}
	if q.RoomKey != "" && r.RoomKey != q.RoomKey {
		return HistoryRow{}, false
	}
	if q.Role != "" && r.Role != q.Role {
		return HistoryRow{}, false
	}
	if q.Browser != "" && r.Browser != q.Browser {
		return HistoryRow{}, false
	}
	if q.OS != "" && r.OS != q.OS {
		return HistoryRow{}, false
	}
	if q.AppVersion != "" && r.AppVersion != q.AppVersion {
		return HistoryRow{}, false
	}
	delivery := r.Config["deliveryMode"]
	if q.DeliveryMode != "" && delivery != q.DeliveryMode {
		return HistoryRow{}, false
	}
	// The range is applied to the row itself, not only to the partition scan:
	// `rollups(since)` prunes whole DAYS, so a query for "since 14:00" would
	// otherwise return everything from midnight.
	if !q.From.IsZero() && r.StartedAt > 0 && r.StartedAt < q.From.UnixMilli() {
		return HistoryRow{}, false
	}
	if !q.To.IsZero() && r.StartedAt > q.To.UnixMilli() {
		return HistoryRow{}, false
	}

	sev := severityOfRow(r)
	if q.Verdict != "" && string(sev) != q.Verdict {
		return HistoryRow{}, false
	}
	rep := storedReport(r)
	if q.HasFindings != nil && (len(rep.Findings) > 0) != *q.HasFindings {
		return HistoryRow{}, false
	}
	distrust := distrustReason(r)
	if q.Distrusted != nil && (distrust != "") != *q.Distrusted {
		return HistoryRow{}, false
	}

	hr := HistoryRow{
		SessionSummary: SessionSummary{
			SessionID: r.SessionID, BroadcastKey: r.BroadcastKey, Role: r.Role,
			Browser: r.Browser, OS: r.OS, StartedAtMs: r.StartedAt, DurationMs: r.DurationMs,
			Severity: sev, Stalls: r.Stalls, Reconnects: r.Reconnects,
			RelayCoverage: r.RelayCoverage, Distrust: distrust,
		},
		RoomKey:    r.RoomKey,
		AppVersion: r.AppVersion, EndedAtMs: r.EndedAt, DeliveryMode: delivery,
		Findings: len(rep.Findings), Live: live[r.SessionID],
		// A session that started before the boundary has no raw window left,
		// whatever its rollup still says. Stated on the row so the list can mark
		// it rather than the detail page discovering it.
		RollupOnly: r.StartedAt > 0 && r.StartedAt < rawFromMs,
	}
	if len(rep.Findings) > 0 {
		hr.Verdict = rep.Findings[0].Verdict
	}
	return hr, true
}

// sortHistory applies UD4's server-side ordering. The per-column default
// direction is chosen so the useful order needs no flag: worst first for
// severity, newest first for time, longest first for duration and stalls.
func sortHistory(rows []HistoryRow, q HistoryQuery) {
	less := func(i, j int) bool { return rows[i].StartedAtMs > rows[j].StartedAtMs }
	switch q.Sort {
	case "severity":
		less = func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.Severity.Rank() != b.Severity.Rank() {
				return a.Severity.Rank() > b.Severity.Rank()
			}
			return a.StartedAtMs > b.StartedAtMs
		}
	case "duration":
		less = func(i, j int) bool { return rows[i].DurationMs > rows[j].DurationMs }
	case "stalls":
		less = func(i, j int) bool { return rows[i].Stalls > rows[j].Stalls }
	}
	sort.SliceStable(rows, less)
	if q.Asc {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
}

// BroadcastRow is TH3's broadcast-level row.
type BroadcastRow struct {
	BroadcastSummary
	// RoomKey (R42, RM8) is the room any of this broadcast's sessions reported.
	// On the history row only, for the same UD1 reason as HistoryRow.RoomKey.
	RoomKey string `json:"roomKey,omitempty"`
	// Broadcaster names the broadcaster session, so a row links straight to the
	// side that can explain a fleet-wide symptom.
	Broadcaster string `json:"broadcasterSessionId,omitempty"`
	AppVersion  string `json:"appVersion,omitempty"`
	Findings    int    `json:"findings"`
	RollupOnly  bool   `json:"rollupOnly,omitempty"`
}

// BroadcastPage is one page of broadcasts.
type BroadcastPage struct {
	Rows       []BroadcastRow `json:"rows"`
	Total      int            `json:"total"`
	NextCursor string         `json:"nextCursor,omitempty"`
	Coverage   `json:"coverage"`
}

// SearchBroadcasts is the broadcast half of the history browser.
func (a *API) SearchBroadcasts(q HistoryQuery) (*BroadcastPage, error) {
	rows, err := a.rollups(q.From)
	if err != nil {
		return nil, err
	}
	cov, err := a.coverage(q)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*BroadcastRow{}
	for _, r := range rows {
		if !q.From.IsZero() && r.StartedAt > 0 && r.StartedAt < q.From.UnixMilli() {
			continue
		}
		if !q.To.IsZero() && r.StartedAt > q.To.UnixMilli() {
			continue
		}
		if q.BroadcastKey != "" && r.BroadcastKey != q.BroadcastKey {
			continue
		}
		if q.RoomKey != "" && r.RoomKey != q.RoomKey {
			continue
		}
		if q.AppVersion != "" && r.AppVersion != q.AppVersion {
			continue
		}
		b, ok := byKey[r.BroadcastKey]
		if !ok {
			b = &BroadcastRow{BroadcastSummary: BroadcastSummary{
				BroadcastKey: r.BroadcastKey, WorstVerdict: rules.SeverityOK,
			}}
			byKey[r.BroadcastKey] = b
		}
		if b.RoomKey == "" && r.RoomKey != "" {
			b.RoomKey = r.RoomKey
		}
		b.Sessions++
		if r.Role == "viewer" {
			b.Viewers++
		} else if r.Role == "broadcaster" {
			b.Broadcaster = r.SessionID
			if r.AppVersion != "" {
				b.AppVersion = r.AppVersion
			}
		}
		if b.FirstSeenMs == 0 || (r.StartedAt > 0 && r.StartedAt < b.FirstSeenMs) {
			b.FirstSeenMs = r.StartedAt
		}
		if r.EndedAt > b.LastSeenMs {
			b.LastSeenMs = r.EndedAt
		}
		if v := severityOfRow(r); v.Rank() > b.WorstVerdict.Rank() {
			b.WorstVerdict = v
		}
		b.Findings += len(storedReport(r).Findings)
		if r.StartedAt > 0 && r.StartedAt < cov.RawFromMs {
			b.RollupOnly = true
		}
	}
	if a.live != nil {
		for _, lb := range a.live.Snapshot().Live {
			if b, ok := byKey[lb.BroadcastKey]; ok {
				b.Live = true
				if v := maxSeverity(lb.Severity, lb.WorstViewer); v.Rank() > b.WorstVerdict.Rank() {
					b.WorstVerdict = v
				}
			}
		}
	}

	out := make([]BroadcastRow, 0, len(byKey))
	for _, b := range byKey {
		out = append(out, *b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if q.Sort == "severity" && out[i].WorstVerdict.Rank() != out[j].WorstVerdict.Rank() {
			return out[i].WorstVerdict.Rank() > out[j].WorstVerdict.Rank()
		}
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		return out[i].LastSeenMs > out[j].LastSeenMs
	})
	total := len(out)
	page, next := paginate(out, q.Cursor, q.Limit, DefaultHistoryLimit, MaxHistoryLimit)
	return &BroadcastPage{Rows: page, Total: total, NextCursor: next, Coverage: cov}, nil
}

// coverage states what the requested range can and cannot be answered from.
func (a *API) coverage(q HistoryQuery) (Coverage, error) {
	now := a.now()
	cov := Coverage{RetentionDays: a.retentionDays}
	if a.retentionDays > 0 {
		// The prune loop deletes whole date partitions strictly older than the
		// cutoff DAY, so the boundary is a midnight, not a rolling instant.
		// Reporting the rolling instant would mark rows rollup-only that still
		// have every sample on disk.
		cutoff := now.AddDate(0, 0, -a.retentionDays).UTC()
		cov.RawFromMs = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	}
	dates, err := a.store.RollupDates()
	if err != nil {
		return cov, err
	}
	if len(dates) > 0 {
		if t, err := time.Parse(store.DateLayout, dates[0]); err == nil {
			cov.RollupsFromMs = t.UnixMilli()
		}
	}
	switch {
	case cov.RollupsFromMs == 0:
		cov.Note = "this service has no stored rollups at all — the range is unanswerable, not empty"
	case !q.From.IsZero() && q.From.UnixMilli() < cov.RollupsFromMs:
		cov.Note = fmt.Sprintf("nothing is stored before %s; the earlier part of this range is unanswerable, not empty",
			time.UnixMilli(cov.RollupsFromMs).UTC().Format(time.RFC3339))
	case !q.From.IsZero() && cov.RawFromMs > 0 && q.From.UnixMilli() < cov.RawFromMs:
		cov.Note = fmt.Sprintf("raw samples are kept for %d days; rows before %s are rollup-only",
			a.retentionDays, time.UnixMilli(cov.RawFromMs).UTC().Format(time.RFC3339))
	}
	return cov, nil
}

// liveSessionIDs is the set of sessions the projection currently holds open, so
// a history row can say "this one is still running" without reading a file.
func (a *API) liveSessionIDs() map[string]bool {
	out := map[string]bool{}
	if a.live == nil {
		return out
	}
	snap := a.live.Snapshot()
	for _, b := range snap.Live {
		for _, s := range b.Sessions {
			out[s.SessionID] = true
		}
	}
	return out
}

// storedReport parses a row's stored verdict. UD2: rendered, never recomputed.
// A row from a build that predates verdicts simply yields an empty report,
// which is D4's additive rule working rather than an error.
func storedReport(r rollup.Row) rules.Report {
	var rep rules.Report
	if len(r.Verdict) > 0 {
		_ = json.Unmarshal(r.Verdict, &rep)
	}
	return rep
}

// paginate is an offset cursor over an already-sorted, already-filtered slice.
//
// An offset rather than a keyset cursor because the underlying set is rebuilt
// from the rollup partitions on every request anyway: a keyset would buy
// stability against concurrent inserts that this data shape cannot offer in the
// first place, at the cost of a compound cursor per sort column. The sort is
// stable, so a page boundary is reproducible for as long as the partition is.
func paginate[T any](rows []T, cursor string, limit, def, maxLimit int) ([]T, string) {
	if limit <= 0 {
		limit = def
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset, _ := strconv.Atoi(cursor)
	if offset < 0 || offset > len(rows) {
		offset = 0
	}
	end := offset + limit
	if end >= len(rows) {
		return rows[offset:], ""
	}
	return rows[offset:end], strconv.Itoa(end)
}
