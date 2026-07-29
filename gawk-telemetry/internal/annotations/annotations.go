// Package annotations is R31's ONE write path (docs/36 UD16, TH8).
//
// A note pinned to a session, a broadcast, or a moment on a timeline:
// "switched to WiFi here", "this is the R30 regression". Everything else in
// this service describes what a client or a relay measured; this describes what
// an operator concluded, and the two must never be mixed.
//
// Three properties are load-bearing:
//
//   - **Never written into a session file.** A raw partition stays exactly what
//     a client sent, byte for byte. An annotation living beside a sample would
//     make the store's most valuable property — that a session file is a
//     verbatim record — quietly untrue.
//   - **Permanent, like rollups.** An annotation outliving the samples it
//     describes is the NORMAL case and the entire point: the raw window is 30
//     days, the note about what happened in it is forever. So this lives beside
//     rollups/, which `Store.Prune` does not walk.
//   - **Append-only, with tombstones.** A delete appends a record marking an id
//     dead rather than rewriting the file. Rewriting a permanent artifact in
//     place to remove one line is how the other lines get lost.
//
// R28's zero-PII rule governs COLLECTED data and is untouched here: this is
// free text the operator typed, on the operator's own PVC.
package annotations

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxTextBytes bounds one note. Generous for a sentence about what happened,
// small enough that the permanent file cannot be filled by a paste accident.
const MaxTextBytes = 4096

// MaxAuthorBytes bounds the optional author label.
const MaxAuthorBytes = 128

// ErrNotFound reports a delete of an id that was never written.
var ErrNotFound = errors.New("annotations: not found")

// ErrInvalid reports a malformed annotation.
var ErrInvalid = errors.New("annotations: invalid")

var (
	sessionIDRe    = regexp.MustCompile(`^[0-9a-f]{24}$`)
	broadcastKeyRe = regexp.MustCompile(`^[0-9a-f]{12}$`)
	idRe           = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// Annotation is one operator note.
type Annotation struct {
	ID          string `json:"id"`
	CreatedAtMs int64  `json:"createdAtMs"`
	// Scope. At least one of SessionID / BroadcastKey / AtMs must be set — a
	// note pinned to nothing is a note nobody will ever find again.
	SessionID    string `json:"sessionId,omitempty"`
	BroadcastKey string `json:"broadcastKey,omitempty"`
	// AtMs is an ABSOLUTE moment (UD5). A note carrying one renders as a marker
	// on every chart whose range covers it, whatever else it is scoped to.
	AtMs   int64  `json:"atMs,omitempty"`
	Text   string `json:"text"`
	Author string `json:"author,omitempty"`
	// Deleted marks a tombstone record. Never returned by List.
	Deleted bool `json:"deleted,omitempty"`
}

// Query filters a listing. A zero Query returns every live annotation.
type Query struct {
	SessionID    string
	BroadcastKey string
	// FromMs/ToMs select annotations whose AtMs falls inside the range. A note
	// with no AtMs is scope-pinned rather than time-pinned and is returned
	// whenever its scope matches, because "the note about this session" does not
	// stop being about it outside some window.
	FromMs, ToMs int64
}

// Store is the append-only annotation file.
type Store struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// Options configure a Store.
type Options struct {
	// Root is the data directory — the same one the session store owns. The
	// file lands at <root>/annotations/annotations.ndjson, a sibling of
	// rollups/ and deliberately NOT under sessions/ or relay/, which are the two
	// trees Prune deletes.
	Root string
	Now  func() time.Time
}

// New opens (and creates) the annotation store.
func New(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, errors.New("annotations: Root is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	dir := filepath.Join(opts.Root, "annotations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "annotations.ndjson"), now: opts.Now}, nil
}

// Path is where the annotations live. Exported so a test can assert that
// nothing else in the tree was touched.
func (s *Store) Path() string { return s.path }

// Add validates and appends one annotation, returning it with its assigned id
// and creation time.
func (s *Store) Add(a Annotation) (Annotation, error) {
	a.Text = strings.TrimSpace(a.Text)
	if a.Text == "" {
		return Annotation{}, fmt.Errorf("%w: text is required", ErrInvalid)
	}
	if len(a.Text) > MaxTextBytes {
		return Annotation{}, fmt.Errorf("%w: text is longer than %d bytes", ErrInvalid, MaxTextBytes)
	}
	if len(a.Author) > MaxAuthorBytes {
		return Annotation{}, fmt.Errorf("%w: author is longer than %d bytes", ErrInvalid, MaxAuthorBytes)
	}
	if a.SessionID != "" && !sessionIDRe.MatchString(a.SessionID) {
		return Annotation{}, fmt.Errorf("%w: session id %q", ErrInvalid, a.SessionID)
	}
	if a.BroadcastKey != "" && !broadcastKeyRe.MatchString(a.BroadcastKey) {
		return Annotation{}, fmt.Errorf("%w: broadcast key %q", ErrInvalid, a.BroadcastKey)
	}
	if a.SessionID == "" && a.BroadcastKey == "" && a.AtMs == 0 {
		return Annotation{}, fmt.Errorf("%w: pin the note to a session, a broadcast or a moment", ErrInvalid)
	}

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Annotation{}, err
	}
	a.ID = hex.EncodeToString(raw[:])
	a.CreatedAtMs = s.now().UnixMilli()
	a.Deleted = false
	if err := s.append(a); err != nil {
		return Annotation{}, err
	}
	return a, nil
}

// Delete appends a tombstone. Deleting an id that does not exist is an error
// rather than a silent success: the UI's delete button must be able to tell
// "gone" from "never there".
func (s *Store) Delete(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("%w: id %q", ErrInvalid, id)
	}
	live, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := live[id]; !ok {
		return ErrNotFound
	}
	return s.append(Annotation{ID: id, Deleted: true, CreatedAtMs: s.now().UnixMilli()})
}

// List returns matching live annotations, newest-pinned first.
func (s *Store) List(q Query) ([]Annotation, error) {
	live, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Annotation, 0, len(live))
	for _, a := range live {
		if q.SessionID != "" && a.SessionID != q.SessionID {
			continue
		}
		if q.BroadcastKey != "" && a.BroadcastKey != q.BroadcastKey {
			continue
		}
		if a.AtMs > 0 {
			if q.FromMs > 0 && a.AtMs < q.FromMs {
				continue
			}
			if q.ToMs > 0 && a.AtMs > q.ToMs {
				continue
			}
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].AtMs, out[j].AtMs
		if ai == 0 {
			ai = out[i].CreatedAtMs
		}
		if aj == 0 {
			aj = out[j].CreatedAtMs
		}
		if ai != aj {
			return ai > aj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) append(a Annotation) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	// Synced before the handle is returned. A note an operator has been told was
	// saved, and which a crash then loses, is worse than a save that failed
	// loudly — and this file sees a handful of writes a day, so the cost is not
	// a consideration.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// load replays the file into the live set. Later records win, so a tombstone
// removes an id and a re-added id would replace it.
func (s *Store) load() (map[string]Annotation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Annotation{}, nil
		}
		return nil, err
	}
	live := map[string]Annotation{}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var a Annotation
		if err := json.Unmarshal([]byte(ln), &a); err != nil || a.ID == "" {
			// A line that will not parse is skipped, exactly as the session
			// reader does: a permanent artifact must stay readable past one bad
			// record.
			continue
		}
		if a.Deleted {
			delete(live, a.ID)
			continue
		}
		live[a.ID] = a
	}
	return live, nil
}
