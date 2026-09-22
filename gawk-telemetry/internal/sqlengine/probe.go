package sqlengine

// The view-health probe.
//
// The console broke in production twice over — every `rollups` query failing
// on view drift, every unpruned `sessions` query on memory — and each was found
// weeks later by an operator who happened to reach for it (BUGS.md,
// 2026-08-20, and again 2026-09-22). Nothing exercised the console between
// those visits, so nothing could notice. The probe is that exercise: it asks
// each view a cheap question on a timer, through the same Query path an
// operator uses, and the answer is exported as a metric an alert can watch.
//
// It lives here rather than behind the build tag so the verdict logic — what
// counts as broken, what counts as merely empty — is tested in every build.

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// viewSource is where one view's rows live on disk.
type viewSource struct {
	glob string
	// hive is whether the path carries `date=…/broadcast=…` partition keys.
	hive bool
}

// viewSources is the one definition of the views' on-disk layout, shared by
// the engine that registers them and the probe that checks them.
func viewSources(root string) map[string]viewSource {
	return map[string]viewSource{
		"sessions":    {filepath.Join(root, "sessions", "date=*", "broadcast=*", "*.ndjson*"), true},
		"rollups":     {filepath.Join(root, "rollups", "*.ndjson"), false},
		"relay":       {filepath.Join(root, "relay", "date=*", "*.ndjson*"), true},
		"annotations": {filepath.Join(root, "annotations", "annotations.ndjson"), false},
	}
}

// Querier is the part of an Engine the probe needs.
type Querier interface {
	Query(sql string) (*Result, error)
}

// ViewHealth is one view's probe verdict.
type ViewHealth struct {
	Name string `json:"name"`
	// HasData is whether the view's tree holds at least one file.
	HasData bool `json:"hasData"`
	// OK is whether the view answers. A view with no data is OK: a client-only
	// fleet has no relay partitions, and an empty tree is not a broken one.
	// A view WITH data that does not answer is the failure the probe exists to
	// catch.
	OK  bool   `json:"ok"`
	Err string `json:"error,omitempty"`
}

// Probe asks every view one question and reports which ones fail to answer.
//
// The questions are chosen to exercise binding — where view drift fails —
// over the WHOLE tree, while scanning as little as possible: the partitioned
// views are counted for today only, which hive pruning turns into one
// directory, but binding still reads every file's schema (union_by_name), so a
// drifted or unreadable partition anywhere still fails the probe.
func Probe(q Querier, root string, now time.Time) []ViewHealth {
	names := make([]string, 0, 4)
	for _, d := range viewDocs(nil) {
		names = append(names, d.Name)
	}
	srcs := viewSources(root)
	out := make([]ViewHealth, 0, len(names))
	for _, name := range names {
		src := srcs[name]
		h := ViewHealth{Name: name}
		matches, _ := filepath.Glob(src.glob)
		h.HasData = len(matches) > 0
		if !h.HasData {
			h.OK = true
			out = append(out, h)
			continue
		}
		stmt := "SELECT count(*) FROM " + name
		if src.hive {
			stmt += fmt.Sprintf(" WHERE date = '%s'", now.UTC().Format(store.DateLayout))
		}
		if _, err := q.Query(stmt); err != nil {
			h.Err = err.Error()
		} else {
			h.OK = true
		}
		out = append(out, h)
	}
	return out
}
