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
// wrote (docs/gotchas.md). Relay lines are edited at the byte level instead
// (relay.go). Lines that do not parse, and lines with nothing to fix, are
// written back byte for byte. A file with nothing to fix is not rewritten at
// all; one that is goes through a temp file, fsync and rename.
//
// It runs before the store, the scraper and the real ingest handler exist,
// so nothing appends concurrently (the ingest listener is up behind a gate
// that answers /healthz and 503s the rest). It is attempted once: an
// in-progress marker precedes the first rewrite and a done marker records
// completion, so a boot after a death or a failure skips it (Run).
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
	UnreadableFilesSkipped  int `json:"unreadableFilesSkipped"`
}

// MarkerPath is where a completed run is recorded.
func MarkerPath(root string) string { return filepath.Join(root, "repairs", ID+".done") }

// StartedPath is the in-progress marker, written before the first file is
// touched. Present without MarkerPath, it means an attempt died or failed;
// deleting it is how an operator asks for one more attempt.
func StartedPath(root string) string { return filepath.Join(root, "repairs", ID+".started") }

// ErrPreviousAttempt is returned, with nothing read or changed, when an
// earlier attempt started and never completed.
var ErrPreviousAttempt = errors.New("a previous history repair attempt did not complete; not retrying")

// testHookBeforeRewrite, when set, runs before each file is replaced.
var testHookBeforeRewrite func(path string)

// Run repairs the data dir at root unless its markers say it already has, or
// already tried. ran is false when nothing was read.
//
// One attempt, ever: the in-progress marker goes down before any file is
// touched and stays, so a boot after an OOM kill, a liveness kill or a
// returned error finds it without the done marker and returns
// ErrPreviousAttempt instead of trying again. A returned error is treated
// like a death on purpose: the failures this can hit (a full volume, an
// unreadable directory) recur, and retrying would turn each boot into the
// same multi-minute ingest outage. Every rewrite is atomic and repaired data
// is a fixed point, so a partial run leaves consistent data and an operator
// re-run (delete the in-progress marker) finishes the job.
func Run(root string) (s Summary, ran bool, err error) {
	s.ID = ID
	if _, err := os.Stat(MarkerPath(root)); err == nil {
		return s, false, nil
	} else if !os.IsNotExist(err) {
		return s, false, err
	}
	if _, err := os.Stat(StartedPath(root)); err == nil {
		// Not repairing, but a death mid-rewrite may have left a temp file,
		// and relay readers read every file in a partition.
		if err := removeTemps(root); err != nil {
			return s, false, err
		}
		return s, false, ErrPreviousAttempt
	} else if !os.IsNotExist(err) {
		return s, false, err
	}
	if err := os.MkdirAll(filepath.Dir(StartedPath(root)), 0o755); err != nil {
		return s, false, err
	}
	started := fmt.Sprintf("{\"id\":%q,\"startedAt\":%q}\n", ID, time.Now().UTC().Format(time.RFC3339))
	if err := atomicWrite(StartedPath(root), false, func(w io.Writer) error {
		_, err := io.WriteString(w, started)
		return err
	}); err != nil {
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
	err = atomicWrite(MarkerPath(root), false, func(w io.Writer) error {
		_, err := w.Write(append(b, '\n'))
		return err
	})
	return s, true, err
}

// removeTemps deletes every in-flight rewrite an interrupted run left
// (readDir removes them as it lists).
func removeTemps(root string) error {
	if _, err := readDir(filepath.Join(root, "rollups")); err != nil {
		return err
	}
	dates, err := readDir(filepath.Join(root, "relay"))
	if err != nil {
		return err
	}
	for _, d := range dates {
		if d.IsDir() {
			if _, err := readDir(filepath.Join(root, "relay", d.Name())); err != nil {
				return err
			}
		}
	}
	return nil
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
	if testHookBeforeRewrite != nil {
		testHookBeforeRewrite(path)
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
