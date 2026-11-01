// Package repair is the one-time rewrite of stored telemetry history that the
// owner decided on 2026-11-01 (docs/33 §4.17), despite D4's permanence: two
// known corruptions are removed from the files on disk, once, at startup.
//
//  1. **#376's latency.** Viewers before the client fix published the
//     broadcaster tab's age (seconds to 48 minutes) as capToRenderMs and
//     liveEdgeDriftMs. The raw samples are gone, so a poisoned Stat cannot be
//     recomputed: a rollup Series entry whose max exceeds
//     rollup.MaxPlausibleLatencyMs is DELETED — absent means "could not be
//     computed" (D15). Never clamped, never re-derived.
//  2. **R30 stripe legs.** Before #657 the scraper stored a striped viewer's
//     four legs as if they were viewers, with no marker, and JoinRelay took
//     the row's subscriber facts from whichever line came last. On a striped
//     viewer's rollup row the subscriber-derived Relay keys are deleted (they
//     may be a leg's); broadcast-derived ones stay. In the raw relay files
//     (still inside the retention window) duplicate audience entries of one
//     session in one scrape round are marked `stripeLeg: true`, all but one.
//
// Every changed row says so in `repairs` (stable ids below). Rows are
// rewritten through map[string]json.RawMessage, never rollup.Row: decoding
// into the current struct would silently drop every field a newer release
// wrote (docs/gotchas.md). Lines that do not parse, and lines with nothing to
// fix, are written back byte for byte. A file with nothing to fix is not
// rewritten at all; one that is goes through a temp file, fsync and rename.
//
// It runs before the store is opened for ingest and before any listener, so
// nothing appends concurrently. A marker file records completion; every step
// is idempotent, so a crash before the marker simply runs it again.
package repair

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
)

const (
	// ID names this repair; its marker is <data>/repairs/<ID>.done.
	ID = "2026-11-history"
	// LatencyRepair is recorded on a row whose capToRenderMs and/or
	// liveEdgeDriftMs Stat was deleted as implausible (#376).
	LatencyRepair = "latency-implausible-2026-11"
	// StripeLegRelayRepair is recorded on a striped viewer's row whose
	// subscriber-derived Relay keys were deleted, since they may be a leg's.
	StripeLegRelayRepair = "stripe-leg-relay-2026-11"

	// tmpSuffix marks a rewrite in flight. Removed at the start of every run:
	// relay readers list every file in a partition, so a crash's leftover
	// would otherwise be read as one more pod.
	tmpSuffix = ".repair-tmp"
)

// subscriberRelayKeys are the rollup Relay keys JoinRelay takes from the
// viewer's own subscriber observation. ingressFramesLost and framesRelayed
// come from the broadcast and are right regardless.
var subscriberRelayKeys = []string{"dropped", "keyframesDropped", "carrierQueueOverflow", "carrierRecordsDropped", "dvrResyncs"}

var (
	rollupFileRe = regexp.MustCompile(`^(?:date=)?\d{4}-\d{2}-\d{2}\.ndjson$`)
	datePartRe   = regexp.MustCompile(`^date=\d{4}-\d{2}-\d{2}$`)
)

// Summary is what one run changed. It is logged and written into the marker.
type Summary struct {
	ID          string        `json:"id"`
	CompletedAt string        `json:"completedAt,omitempty"`
	Rollups     RollupSummary `json:"rollups"`
	Relay       RelaySummary  `json:"relay"`
}

// RollupSummary counts the permanent rows changed.
type RollupSummary struct {
	FilesRewritten       int `json:"filesRewritten"`
	RowsChanged          int `json:"rowsChanged"`
	LatencySeriesRemoved int `json:"latencySeriesRemoved"`
	StripeLegRelayRows   int `json:"stripeLegRelayRows"`
	RelayKeysRemoved     int `json:"relayKeysRemoved"`
	UnparsedLinesKept    int `json:"unparsedLinesKept"`
}

// RelaySummary counts the raw relay entries marked as stripe legs.
type RelaySummary struct {
	DatesScanned            int `json:"datesScanned"`
	FilesRewritten          int `json:"filesRewritten"`
	SubscriberLinesMarked   int `json:"subscriberLinesMarked"`
	SubscriberDetailsMarked int `json:"subscriberDetailsMarked"`
	UnparsedLinesKept       int `json:"unparsedLinesKept"`
}

// MarkerPath is where a completed run is recorded.
func MarkerPath(root string) string { return filepath.Join(root, "repairs", ID+".done") }

// Run repairs the data dir at root unless its marker says it already has.
// ran is false when the marker was present and nothing was read. An error
// leaves no marker, so the next start runs it again — safely, since every
// rewrite is atomic and repaired data is a fixed point.
func Run(root string) (s Summary, ran bool, err error) {
	s.ID = ID
	if _, err := os.Stat(MarkerPath(root)); err == nil {
		return s, false, nil
	} else if !os.IsNotExist(err) {
		return s, false, err
	}
	if s.Rollups, err = repairRollups(filepath.Join(root, "rollups")); err != nil {
		return s, true, fmt.Errorf("repair rollups: %w", err)
	}
	if s.Relay, err = repairRelay(filepath.Join(root, "relay")); err != nil {
		return s, true, fmt.Errorf("repair relay: %w", err)
	}
	s.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return s, true, err
	}
	if err := os.MkdirAll(filepath.Dir(MarkerPath(root)), 0o755); err != nil {
		return s, true, err
	}
	err = atomicWrite(MarkerPath(root), false, func(w io.Writer) error {
		_, err := w.Write(append(b, '\n'))
		return err
	})
	return s, true, err
}

// --- rollups -------------------------------------------------------------

func repairRollups(dir string) (RollupSummary, error) {
	var sum RollupSummary
	entries, err := readDir(dir)
	if err != nil {
		return sum, err
	}
	for _, e := range entries {
		if e.IsDir() || !rollupFileRe.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return sum, err
		}
		lines := splitLines(data)
		changed := false
		for i, ln := range lines {
			out, ch, ok := repairRow(ln.body)
			if !ok {
				if len(bytes.TrimSpace(ln.body)) > 0 {
					sum.UnparsedLinesKept++
				}
				continue
			}
			if ch.latency == 0 && ch.relayKeys == 0 {
				continue
			}
			lines[i].body = out
			changed = true
			sum.RowsChanged++
			sum.LatencySeriesRemoved += ch.latency
			sum.RelayKeysRemoved += ch.relayKeys
			if ch.relayKeys > 0 {
				sum.StripeLegRelayRows++
			}
		}
		if !changed {
			continue
		}
		if err := atomicWrite(path, false, func(w io.Writer) error { return writeLines(w, lines) }); err != nil {
			return sum, err
		}
		sum.FilesRewritten++
	}
	return sum, nil
}

type rowChange struct{ latency, relayKeys int }

// repairRow returns the repaired line and what changed. ok is false for a
// line that is not a JSON object; the caller keeps it verbatim.
func repairRow(line []byte) (out []byte, ch rowChange, ok bool) {
	var row map[string]json.RawMessage
	if json.Unmarshal(line, &row) != nil || row == nil {
		return nil, ch, false
	}
	series := object(row["series"])

	for name, raw := range series {
		if !rollup.LatencySeries(name) {
			continue
		}
		var st struct {
			Max *float64 `json:"max"`
		}
		if json.Unmarshal(raw, &st) == nil && st.Max != nil && !rollup.PlausibleLatency(name, *st.Max) {
			delete(series, name)
			ch.latency++
		}
	}

	if role := str(row["role"]); role == "viewer" && striped(row, series) {
		relay := object(row["relay"])
		for _, k := range subscriberRelayKeys {
			if _, has := relay[k]; has {
				delete(relay, k)
				ch.relayKeys++
			}
		}
		if ch.relayKeys > 0 {
			setObject(row, "relay", relay)
		}
	}
	if ch.latency == 0 && ch.relayKeys == 0 {
		return line, ch, true
	}
	if ch.latency > 0 {
		setObject(row, "series", series)
	}

	var repairs []string
	if raw, has := row["repairs"]; has && json.Unmarshal(raw, &repairs) != nil {
		// Not a string array: a field this repair does not own. Leave the
		// row's other fixes in place but do not clobber it.
		return line, rowChange{}, true
	}
	if ch.latency > 0 {
		repairs = appendOnce(repairs, LatencyRepair)
	}
	if ch.relayKeys > 0 {
		repairs = appendOnce(repairs, StripeLegRelayRepair)
	}
	row["repairs"] = mustRaw(repairs)
	return encode(row), ch, true
}

// striped reports whether a viewer row ever ran R30 legs: it dialled one
// (the cumulative stripeLegDials counter), or its stripeActive gauge — 0
// unstriped, else the connection count (docs/35 §7) — was ever above zero.
func striped(row map[string]json.RawMessage, series map[string]json.RawMessage) bool {
	var counters map[string]float64
	if json.Unmarshal(row["counters"], &counters) == nil && counters["stripeLegDials"] > 0 {
		return true
	}
	var st struct {
		Max float64 `json:"max"`
	}
	return json.Unmarshal(series["stripeActive"], &st) == nil && st.Max > 0
}

// --- relay -----------------------------------------------------------------

// A group is every audience-looking subscriber entry of one session in one
// scrape round. A round is one ScrapeOnce pass: every pod's observations
// carry the SAME atMs (relayscrape stamps them from one clock reading), and a
// leg may land on any pod behind the UDP LB — so the group spans all pod files
// of the date. Stored lines and the broadcast lines' subscriberDetails are
// grouped separately (detail) but judged by the same rule, so they agree.
type groupKey struct {
	atMs         int64
	broadcastKey string
	sessionID    string
	detail       bool
}

type loc struct{ file, line, idx int }

type group struct {
	n     int
	leg   bool // a member already says stripeLeg: post-#657, leave it
	score uint64
	keep  loc
	files uint64 // bitmask of file indices holding members; all ones past 64
}

type subProbe struct {
	SessionID        string `json:"sessionId"`
	Internal         bool   `json:"internal"`
	StripeLeg        bool   `json:"stripeLeg"`
	KeyframesSent    uint64 `json:"keyframesSent"`
	KeyframesDropped uint64 `json:"keyframesDropped"`
}

type obsProbe struct {
	Kind         string    `json:"kind"`
	AtMs         int64     `json:"atMs"`
	BroadcastKey string    `json:"broadcastKey"`
	SessionID    string    `json:"sessionId"`
	Subscriber   *subProbe `json:"subscriber"`
	Broadcast    *struct {
		SubscriberDetails []subProbe `json:"subscriberDetails"`
	} `json:"broadcast"`
}

func repairRelay(dir string) (RelaySummary, error) {
	var sum RelaySummary
	dates, err := readDir(dir)
	if err != nil {
		return sum, err
	}
	for _, d := range dates {
		if !d.IsDir() || !datePartRe.MatchString(d.Name()) {
			continue
		}
		sum.DatesScanned++
		if err := repairRelayDate(filepath.Join(dir, d.Name()), &sum); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func repairRelayDate(dir string, sum *RelaySummary) error {
	entries, err := readDir(dir)
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	// Pass 1: index the groups. Streaming, because a busy day's relay files
	// are far larger than the rollups, and the pod's memory limit is shared
	// with nothing yet but is not generous.
	groups := map[groupKey]*group{}
	add := func(k groupKey, s subProbe, at loc) {
		if s.Internal || k.sessionID == "" {
			return
		}
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
		}
		if s.StripeLeg {
			g.leg = true
			return
		}
		// The primary is the member that received keyframes: a leg never
		// does (docs/35 §5.1). On a tie the first in file order stays.
		if sc := score(s); g.n == 0 || sc > g.score {
			g.score, g.keep = sc, at
		}
		g.n++
		if at.file < 64 {
			g.files |= 1 << at.file
		} else {
			g.files = ^uint64(0)
		}
	}
	for fi, path := range files {
		err := eachLine(path, func(li int, body []byte) error {
			var o obsProbe
			if json.Unmarshal(body, &o) != nil {
				return nil
			}
			if o.Subscriber != nil {
				add(groupKey{o.AtMs, o.BroadcastKey, o.SessionID, false}, *o.Subscriber, loc{fi, li, -1})
			}
			if o.Broadcast != nil {
				for i, s := range o.Broadcast.SubscriberDetails {
					add(groupKey{o.AtMs, o.BroadcastKey, s.SessionID, true}, s, loc{fi, li, i})
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	var touched uint64
	for _, g := range groups {
		if g.n > 1 && !g.leg {
			touched |= g.files
		}
	}

	// Pass 2: rewrite only the files holding a duplicate.
	isLeg := func(k groupKey, s subProbe, at loc) bool {
		if s.Internal || s.StripeLeg || k.sessionID == "" {
			return false
		}
		g := groups[k]
		return g != nil && g.n > 1 && !g.leg && g.keep != at
	}
	for fi, path := range files {
		if fi < 64 && touched&(1<<fi) == 0 {
			continue
		}
		var lines []line
		marked := 0
		err := eachLineRaw(path, func(li int, ln line) error {
			lines = append(lines, ln)
			var o obsProbe
			if json.Unmarshal(ln.body, &o) != nil {
				if len(bytes.TrimSpace(ln.body)) > 0 {
					sum.UnparsedLinesKept++
				}
				return nil
			}
			markLine := o.Subscriber != nil && isLeg(groupKey{o.AtMs, o.BroadcastKey, o.SessionID, false}, *o.Subscriber, loc{fi, li, -1})
			var markIdx []int
			if o.Broadcast != nil {
				for i, s := range o.Broadcast.SubscriberDetails {
					if isLeg(groupKey{o.AtMs, o.BroadcastKey, s.SessionID, true}, s, loc{fi, li, i}) {
						markIdx = append(markIdx, i)
					}
				}
			}
			if !markLine && len(markIdx) == 0 {
				return nil
			}
			out, err := markLegs(ln.body, markLine, markIdx)
			if err != nil {
				return fmt.Errorf("%s line %d: %w", path, li+1, err)
			}
			lines[len(lines)-1].body = out
			marked++
			if markLine {
				sum.SubscriberLinesMarked++
			}
			sum.SubscriberDetailsMarked += len(markIdx)
			return nil
		})
		if err != nil {
			return err
		}
		if marked == 0 {
			continue
		}
		gz := strings.HasSuffix(path, ".gz")
		if err := atomicWrite(path, gz, func(w io.Writer) error { return writeLines(w, lines) }); err != nil {
			return err
		}
		sum.FilesRewritten++
	}
	return nil
}

func score(s subProbe) uint64 { return s.KeyframesSent + s.KeyframesDropped }

// markLegs sets stripeLeg on the line's subscriber object and/or on the given
// subscriberDetails elements, leaving every other field as it was.
func markLegs(body []byte, subscriber bool, details []int) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	if subscriber {
		sub := object(top["subscriber"])
		sub["stripeLeg"] = json.RawMessage("true")
		setObject(top, "subscriber", sub)
	}
	if len(details) > 0 {
		bc := object(top["broadcast"])
		var arr []json.RawMessage
		if err := json.Unmarshal(bc["subscriberDetails"], &arr); err != nil {
			return nil, err
		}
		for _, i := range details {
			el := object(arr[i])
			el["stripeLeg"] = json.RawMessage("true")
			arr[i] = encode(el)
		}
		bc["subscriberDetails"] = mustRaw(arr)
		setObject(top, "broadcast", bc)
	}
	return encode(top), nil
}

// --- JSON helpers ----------------------------------------------------------

func object(raw json.RawMessage) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m
}

// setObject stores m under key, or removes the key when m is empty: the
// struct's omitempty maps never write `{}`, and "absent" is what D15 means.
func setObject(row map[string]json.RawMessage, key string, m map[string]json.RawMessage) {
	if len(m) == 0 {
		delete(row, key)
		return
	}
	row[key] = encode(m)
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func appendOnce(list []string, id string) []string {
	for _, v := range list {
		if v == id {
			return list
		}
	}
	return append(list, id)
}

// encode marshals without HTML escaping, so strings in untouched RawMessage
// values keep their bytes ("<" is not rewritten to "<").
func encode(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // only maps of valid RawMessage reach here
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func mustRaw(v any) json.RawMessage { return encode(v) }

// --- file helpers ----------------------------------------------------------

// line is one stored line and whether it ended in a newline: a crash can
// leave a last line without one, and it is written back exactly so.
type line struct {
	body []byte
	nl   bool
}

func splitLines(data []byte) []line {
	var out []line
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			out = append(out, line{body: data})
			break
		}
		out = append(out, line{body: data[:i], nl: true})
		data = data[i+1:]
	}
	return out
}

func writeLines(w io.Writer, lines []line) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	for _, ln := range lines {
		if _, err := bw.Write(ln.body); err != nil {
			return err
		}
		if ln.nl {
			if err := bw.WriteByte('\n'); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

func openLines(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{zr, f}, nil
}

func eachLineRaw(path string, fn func(i int, ln line) error) error {
	rc, err := openLines(path)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil // an empty .gz
		}
		return err
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 64<<10)
	for i := 0; ; i++ {
		b, err := br.ReadBytes('\n')
		if len(b) > 0 {
			ln := line{body: b}
			if b[len(b)-1] == '\n' {
				ln = line{body: b[:len(b)-1], nl: true}
			}
			if ferr := fn(i, ln); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
}

func eachLine(path string, fn func(i int, body []byte) error) error {
	return eachLineRaw(path, func(i int, ln line) error { return fn(i, ln.body) })
}

// readDir lists dir, removing the temp files of an interrupted run. A missing
// dir is empty: a fresh install has nothing to repair.
func readDir(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), tmpSuffix) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// atomicWrite replaces path with what fill writes: a temp file in the same
// directory (so the rename cannot cross a filesystem), fsynced, renamed over
// the original, then the directory fsynced. gz keeps a .gz file gzip.
func atomicWrite(path string, gz bool, fill func(io.Writer) error) (err error) {
	dir, base := filepath.Split(path)
	tmp := filepath.Join(dir, "."+base+tmpSuffix)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	var w io.Writer = f
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(f)
		w = zw
	}
	if err = fill(w); err != nil {
		return err
	}
	if zw != nil {
		if err = zw.Close(); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(filepath.Clean(dir)); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
