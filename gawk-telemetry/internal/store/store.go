// Package store is R28's files-first persistence layer (docs/33 D3).
//
//	/data/
//	  sessions/date=2026-07-26/broadcast=<12 hex>/<sessionId>.ndjson[.gz]
//	  rollups/date=2026-07-26.ndjson          # permanent, one line per session
//	  relay/date=2026-07-26/<pod>.ndjson[.gz] # scraped relay snapshots (D5)
//
// `date=`/`broadcast=` are hive-style partitions, so both plain Go and a
// DuckDB `read_json_auto(..., hive_partitioning=1)` prune by path instead of
// scanning — and retention becomes **a directory delete, not a query**.
//
// A session file is written PLAIN while the session is open and gzipped on
// finalize (session end, or an idle timeout for sessions that simply vanish —
// a browser tab closed mid-stream sends no final batch). Appending to a gzip
// stream is legal but awkward to reason about under crash; a startup sweep
// that finalizes orphaned `.ndjson` files is a directory scan and obviously
// correct.
//
// One file per session means **one writer per file** — no interleaving, no
// locking beyond the handle cache below.
//
// The arithmetic that justifies files over any real database: ten viewers ×
// 2 h/day ≈ 5 MB/day, ≈ 75 MB across the whole 14-day window. A PVC and a
// prune loop cover it; ClickHouse would be answering a question nobody asked.
package store

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DateLayout is the partition key format. UTC, always: a store whose partition
// boundaries move with a server's local time is one whose retention window is
// unpredictable.
const DateLayout = "2006-01-02"

const (
	sessionsDir = "sessions"
	rollupsDir  = "rollups"
	relayDir    = "relay"
)

// sessionIDRe and broadcastKeyRe are the ONLY things standing between a
// request-supplied identifier and the filesystem. Both are fixed-width hex by
// construction (a sessionId is hex(nonce), a broadcast key is hex of a 6-byte
// digest), so an exact-match pattern is both sufficient and airtight — no
// separator, no dot, no traversal component can pass.
var (
	sessionIDRe    = regexp.MustCompile(`^[0-9a-f]{24}$`)
	broadcastKeyRe = regexp.MustCompile(`^[0-9a-f]{12}$`)
	podNameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	datePartRe     = regexp.MustCompile(`^date=(\d{4}-\d{2}-\d{2})$`)
)

// ErrBadIdentifier rejects anything that could reach outside the data
// directory. Returned before any path is built.
var ErrBadIdentifier = errors.New("store: invalid identifier")

// ErrNotFound reports a session with no stored file.
var ErrNotFound = errors.New("store: not found")

// errBusy reports an append to a file the orphan sweep is finalizing right
// now. Transient by construction — the sweep holds a path only for the length
// of one gzip.
var errBusy = errors.New("store: session file is being finalized; retry")

// Store owns the data directory. Safe for concurrent use.
type Store struct {
	root string

	mu   sync.Mutex
	open map[string]*openFile
	// claimed paths are being gzipped and unlinked by the orphan sweep. An
	// append to one must wait rather than write into a file about to be
	// removed.
	claimed map[string]bool
	nowFn   func() time.Time
	closed  bool
}

// claim reserves a path for an exclusive operation (the orphan sweep's
// gzip+unlink). It fails if the path is open for writing or already claimed.
// While a claim is held, `append` refuses to open the file, so an arriving
// batch is a visible error rather than lines written into a doomed inode.
func (s *Store) claim(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[path] != nil || s.claimed[path] {
		return false
	}
	s.claimed[path] = true
	return true
}

func (s *Store) release(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed, path)
}

// testHookSweepBeforeGzip runs inside the sweep's claim, between claiming a
// path and compressing it. Nil in production; a test uses it to make the
// append-during-sweep race deterministic instead of hoping for a timing
// window.
var testHookSweepBeforeGzip func(path string)

type openFile struct {
	f        *os.File
	w        *bufio.Writer
	path     string
	lastUsed time.Time
}

// Options configure a Store.
type Options struct {
	// Root is the data directory (a PVC mount in a cluster).
	Root string
	// Now is injectable so tests drive partitioning and idle timeouts without
	// wall clocks.
	Now func() time.Time
}

// New opens (and creates) the data directory.
func New(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, errors.New("store: Root is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	for _, d := range []string{sessionsDir, rollupsDir, relayDir} {
		if err := os.MkdirAll(filepath.Join(opts.Root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{
		root: opts.Root, open: map[string]*openFile{},
		claimed: map[string]bool{}, nowFn: opts.Now,
	}, nil
}

// Root returns the data directory.
func (s *Store) Root() string { return s.root }

// SessionRef names one session's storage location.
type SessionRef struct {
	Date         string // "2026-07-26"
	BroadcastKey string // 12 hex chars
	SessionID    string // 24 hex chars
}

// Validate rejects anything that could escape the data directory.
func (r SessionRef) Validate() error {
	if !datePartRe.MatchString("date=" + r.Date) {
		return fmt.Errorf("%w: date %q", ErrBadIdentifier, r.Date)
	}
	if !broadcastKeyRe.MatchString(r.BroadcastKey) {
		return fmt.Errorf("%w: broadcast key %q", ErrBadIdentifier, r.BroadcastKey)
	}
	if !sessionIDRe.MatchString(r.SessionID) {
		return fmt.Errorf("%w: session id %q", ErrBadIdentifier, r.SessionID)
	}
	return nil
}

func (s *Store) sessionPath(r SessionRef, gz bool) string {
	name := r.SessionID + ".ndjson"
	if gz {
		name += ".gz"
	}
	return filepath.Join(s.root, sessionsDir, "date="+r.Date, "broadcast="+r.BroadcastKey, name)
}

// AppendSession appends NDJSON lines to a session's open (plain) file,
// creating it if needed. Each element of `lines` is one complete JSON object
// with no trailing newline.
func (s *Store) AppendSession(r SessionRef, lines [][]byte) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return s.append(s.sessionPath(r, false), lines)
}

// AppendRollup appends one permanent rollup row. Rollups are never gzipped:
// they are a few thousand lines across the whole retention window, they must
// survive a raw-partition prune, and every read path opens them.
func (s *Store) AppendRollup(date string, line []byte) error {
	if !datePartRe.MatchString("date=" + date) {
		return fmt.Errorf("%w: date %q", ErrBadIdentifier, date)
	}
	return s.append(filepath.Join(s.root, rollupsDir, date+".ndjson"), [][]byte{line})
}

// AppendRelay appends scraped relay observations for one pod (D5).
func (s *Store) AppendRelay(date, pod string, lines [][]byte) error {
	if !datePartRe.MatchString("date=" + date) {
		return fmt.Errorf("%w: date %q", ErrBadIdentifier, date)
	}
	if !podNameRe.MatchString(pod) {
		return fmt.Errorf("%w: pod %q", ErrBadIdentifier, pod)
	}
	return s.append(filepath.Join(s.root, relayDir, "date="+date, pod+".ndjson"), lines)
}

func (s *Store) append(path string, lines [][]byte) error {
	if len(lines) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("store: closed")
	}
	of, ok := s.open[path]
	if !ok {
		if s.claimed[path] {
			// The orphan sweep is compressing this exact file. Refusing is the
			// point: writing here would put lines in an inode about to be
			// unlinked. The caller's retry lands after the sweep, and ingest
			// retries are idempotent (the writer drops a replayed seq), so the
			// batch is delayed rather than lost.
			return errBusy
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		of = &openFile{f: f, w: bufio.NewWriterSize(f, 64<<10), path: path}
		s.open[path] = of
	}
	of.lastUsed = s.nowFn()
	for _, ln := range lines {
		if _, err := of.w.Write(ln); err != nil {
			return err
		}
		if err := of.w.WriteByte('\n'); err != nil {
			return err
		}
	}
	// Flush every append. A buffered line lost to a crash is a line that never
	// existed as far as any reader is concerned, and the whole point of the
	// store is being able to look at a session that ended badly.
	return of.w.Flush()
}

// CloseIdle closes handles untouched for longer than d, bounding the open-file
// count on a busy fleet. Returns how many it closed.
func (s *Store) CloseIdle(d time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.nowFn().Add(-d)
	n := 0
	for path, of := range s.open {
		if of.lastUsed.After(cutoff) {
			continue
		}
		_ = of.w.Flush()
		_ = of.f.Close()
		delete(s.open, path)
		n++
	}
	return n
}

// Close flushes and closes every open handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var firstErr error
	for path, of := range s.open {
		if err := of.w.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := of.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.open, path)
	}
	return firstErr
}

// FinalizeSession gzips a session's plain file and removes the original. It is
// idempotent: an already-finalized session is a no-op, and a session with no
// file at all is not an error (a batch may have been rejected before anything
// was written).
func (s *Store) FinalizeSession(r SessionRef) error {
	if err := r.Validate(); err != nil {
		return err
	}
	plain := s.sessionPath(r, false)

	s.mu.Lock()
	if of, ok := s.open[plain]; ok {
		_ = of.w.Flush()
		_ = of.f.Close()
		delete(s.open, plain)
	}
	s.mu.Unlock()

	return gzipFile(plain, s.sessionPath(r, true))
}

// gzipFile compresses src to dst and removes src. A missing src is a no-op.
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()

	// Write to a temp file and rename: a crash mid-compress must never leave a
	// truncated .gz beside a deleted original, which would lose the session
	// outright. The rename is atomic within the directory.
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// A dst that already exists means this session was finalized once and came
	// back. Renaming over it would delete the first half outright, so the new
	// archive is APPENDED as a second gzip member — a concatenation of members
	// is a valid gzip file and gzip.Reader reads it transparently, so nothing
	// downstream has to know.
	if _, err := os.Stat(dst); err == nil {
		if err := appendFile(dst, tmp); err != nil {
			os.Remove(tmp)
			return err
		}
		os.Remove(tmp)
		return os.Remove(src)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}

// appendFile appends src's bytes to dst.
func appendFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// SweepOrphans finalizes every plain session file older than `idle` — the
// crash-recovery half of D3. A process that died mid-session leaves a
// `.ndjson` behind; a directory scan that gzips them is obviously correct in a
// way that appending to a gzip stream would not be. Returns how many it
// finalized.
//
// Called at startup and periodically: a browser tab closed mid-stream never
// sends a final batch either, so "orphaned" is the normal end of a session,
// not only a crash.
func (s *Store) SweepOrphans(idle time.Duration) (int, error) {
	root := filepath.Join(s.root, sessionsDir)
	cutoff := s.nowFn().Add(-idle)
	n := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a directory that vanished mid-walk is not our problem
		}
		if d.IsDir() || !strings.HasSuffix(path, ".ndjson") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		// Claim the path under the lock before touching it. A file this
		// process is actively writing is not an orphan (CloseIdle owns that
		// handle's lifetime), and — the race this closes — a batch arriving
		// between the check and the unlink would otherwise reopen the file and
		// append into an inode gzipFile is about to remove: those lines vanish,
		// and the recreated plain file is then permanently shadowed by the .gz,
		// since ReadSession prefers it. FinalizeSession has always done this
		// correctly under the lock; the asymmetry WAS the bug.
		if !s.claim(path) {
			return nil
		}
		if testHookSweepBeforeGzip != nil {
			testHookSweepBeforeGzip(path)
		}
		err = gzipFile(path, path+".gz")
		s.release(path)
		if err == nil {
			n++
		}
		return nil
	})
	return n, err
}

// Prune deletes raw partitions (sessions/ and relay/) whose date is strictly
// before `before`. Rollups are NEVER pruned — that split is the whole point of
// D4: the raw window is disposable, the per-session summary is permanent.
//
// Retention is a directory delete rather than a query, which is what the hive
// layout buys.
func (s *Store) Prune(before time.Time) (int, error) {
	cutoff := before.UTC().Format(DateLayout)
	removed := 0
	for _, dir := range []string{sessionsDir, relayDir} {
		entries, err := os.ReadDir(filepath.Join(s.root, dir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		for _, e := range entries {
			m := datePartRe.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			// Lexicographic comparison is correct for this layout and exact at
			// the boundary: a partition is removed only when its date sorts
			// STRICTLY before the cutoff, so the cutoff day itself survives.
			if m[1] >= cutoff {
				continue
			}
			if err := os.RemoveAll(filepath.Join(s.root, dir, e.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// ReadSession returns a session's stored lines, transparently handling the
// plain (live) and gzipped (finalized) forms.
func (s *Store) ReadSession(r SessionRef) ([][]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	// Flush anything buffered for this session so a read during a live session
	// sees what has been appended.
	s.mu.Lock()
	if of, ok := s.open[s.sessionPath(r, false)]; ok {
		_ = of.w.Flush()
	}
	s.mu.Unlock()

	// BOTH parts, in order. A session can have a .gz and a plain file at once:
	// one that goes quiet long enough to be finalized and then comes back — a
	// phone out of a tunnel, R19's own target user — appends to a fresh plain
	// file beside the archive. Reading only the .gz (which is what "prefer the
	// .gz" did) made every line after the resume invisible to the rollup while
	// looking perfectly healthy.
	archived, err := readLines(s.sessionPath(r, true))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	plain, err := readLines(s.sessionPath(r, false))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if archived == nil && plain == nil {
		return nil, ErrNotFound
	}
	return append(archived, plain...), nil
}

// FindSession locates a session by ID alone, scanning the date partitions
// newest-first. The read API takes a bare sessionId (it is what appears in a
// verdict, a dashboard URL and an MCP call), and this is what turns it back
// into a path without a second index to keep consistent.
func (s *Store) FindSession(sessionID string) (SessionRef, error) {
	if !sessionIDRe.MatchString(sessionID) {
		return SessionRef{}, fmt.Errorf("%w: session id %q", ErrBadIdentifier, sessionID)
	}
	dates, err := s.dates(sessionsDir)
	if err != nil {
		return SessionRef{}, err
	}
	for i := len(dates) - 1; i >= 0; i-- {
		dir := filepath.Join(s.root, sessionsDir, "date="+dates[i])
		buckets, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, b := range buckets {
			if !b.IsDir() || !strings.HasPrefix(b.Name(), "broadcast=") {
				continue
			}
			key := strings.TrimPrefix(b.Name(), "broadcast=")
			ref := SessionRef{Date: dates[i], BroadcastKey: key, SessionID: sessionID}
			if ref.Validate() != nil {
				continue
			}
			for _, gz := range []bool{true, false} {
				if _, err := os.Stat(s.sessionPath(ref, gz)); err == nil {
					return ref, nil
				}
			}
		}
	}
	return SessionRef{}, ErrNotFound
}

// ReadRollups returns every rollup line from partitions on or after `since`,
// oldest partition first.
func (s *Store) ReadRollups(since time.Time) ([][]byte, error) {
	cutoff := since.UTC().Format(DateLayout)
	entries, err := os.ReadDir(filepath.Join(s.root, rollupsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		date := strings.TrimSuffix(e.Name(), ".ndjson")
		if date < cutoff {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Flush live rollup handles so a just-finalized session is visible.
	s.mu.Lock()
	for _, of := range s.open {
		if strings.Contains(of.path, string(os.PathSeparator)+rollupsDir+string(os.PathSeparator)) {
			_ = of.w.Flush()
		}
	}
	s.mu.Unlock()

	var out [][]byte
	for _, n := range names {
		lines, err := readLines(filepath.Join(s.root, rollupsDir, n))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		out = append(out, lines...)
	}
	return out, nil
}

// ReadRelay returns scraped relay lines for one date across every pod.
func (s *Store) ReadRelay(date string) ([][]byte, error) {
	if !datePartRe.MatchString("date=" + date) {
		return nil, fmt.Errorf("%w: date %q", ErrBadIdentifier, date)
	}
	dir := filepath.Join(s.root, relayDir, "date="+date)

	s.mu.Lock()
	for _, of := range s.open {
		if strings.HasPrefix(of.path, dir) {
			_ = of.w.Flush()
		}
	}
	s.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out [][]byte
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lines, err := readLines(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, lines...)
	}
	return out, nil
}

// dates lists the date partitions under one top-level directory, sorted.
func (s *Store) dates(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if m := datePartRe.FindStringSubmatch(e.Name()); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out, nil
}

// readLines reads an NDJSON file, transparently gunzipping a .gz path.
func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	sc := bufio.NewScanner(r)
	// A stats sample is ~1.5 KB; a rollup row with a verdict can be larger.
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var out [][]byte
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		out = append(out, append([]byte(nil), line...))
	}
	// A truncated tail (killed mid-write, or read while being appended) is
	// expected, not exceptional: the complete lines are returned and the
	// partial one dropped, because the store exists to let you look at a
	// session that ended badly. `bufio.Scanner` reports that case by simply
	// stopping, so it needs no branch here.
	//
	// Anything else DOES get returned. Both branches used to return `out, nil`,
	// so `bufio.ErrTooLong` — a line past the 4 MB buffer — truncated the read
	// there and dropped every subsequent line in the file without a trace. The
	// 1 MB ingest bound makes that unreachable from ingest today, but rollup
	// and relay files are not ingest-bounded forever, and a reader that cannot
	// tell "the file ends here" from "I stopped reading here" is the wrong
	// thing to build a permanent artifact on.
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return out, err
	}
	return out, nil
}
