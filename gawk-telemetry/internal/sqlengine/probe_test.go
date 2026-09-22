package sqlengine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeQuerier struct {
	fail  map[string]bool // view name → the query on it fails
	asked []string
}

func (f *fakeQuerier) Query(q string) (*Result, error) {
	f.asked = append(f.asked, q)
	for name := range f.fail {
		if strings.Contains(q, "FROM "+name) {
			return nil, errors.New("Binder Error: Contents of view were altered")
		}
	}
	return &Result{}, nil
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byName(hs []ViewHealth) map[string]ViewHealth {
	m := map[string]ViewHealth{}
	for _, h := range hs {
		m[h.Name] = h
	}
	return m
}

// A view with data that does not answer is the failure; an empty tree is not.
// Conflating the two would page every client-only fleet about its (absent)
// relay partitions.
func TestProbeFailsOnlyAViewThatHasDataAndDoesNotAnswer(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "rollups", "date=2026-09-22.ndjson"))
	touch(t, filepath.Join(root, "sessions", "date=2026-09-22", "broadcast=1a2b3c4d5e6f", "s.ndjson.gz"))
	q := &fakeQuerier{fail: map[string]bool{"rollups": true}}

	got := byName(Probe(q, root, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)))
	if len(got) != 4 {
		t.Fatalf("probed %d views, want all 4: %v", len(got), got)
	}
	if h := got["rollups"]; h.OK || !h.HasData || h.Err == "" {
		t.Errorf("rollups has data and fails: %+v", h)
	}
	if h := got["sessions"]; !h.OK || !h.HasData {
		t.Errorf("sessions has data and answers: %+v", h)
	}
	for _, name := range []string{"relay", "annotations"} {
		if h := got[name]; !h.OK || h.HasData {
			t.Errorf("%s is empty, which is not broken: %+v", name, h)
		}
	}
	// An empty tree is never queried: there is nothing to bind.
	for _, s := range q.asked {
		if strings.Contains(s, "FROM relay") || strings.Contains(s, "FROM annotations") {
			t.Errorf("an empty view was queried: %s", s)
		}
	}
}

// The partitioned views are asked about today only — the probe must bind the
// whole tree without scanning it, every few minutes, on a pod that also
// carries ingest.
func TestProbeScansOnlyTodaysPartition(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "sessions", "date=2026-09-21", "broadcast=1a2b3c4d5e6f", "s.ndjson.gz"))
	touch(t, filepath.Join(root, "relay", "date=2026-09-21", "pod-0.ndjson"))
	q := &fakeQuerier{}
	// 23:30 in Helsinki is 20:30 UTC: the store's partitions are UTC dates.
	helsinki := time.FixedZone("EEST", 3*3600)
	Probe(q, root, time.Date(2026, 9, 22, 23, 30, 0, 0, helsinki))
	for _, want := range []string{
		"SELECT count(*) FROM sessions WHERE date = '2026-09-22'",
		"SELECT count(*) FROM relay WHERE date = '2026-09-22'",
	} {
		found := false
		for _, s := range q.asked {
			found = found || s == want
		}
		if !found {
			t.Errorf("probe did not ask %q; asked %v", want, q.asked)
		}
	}
}
