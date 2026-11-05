//go:build duckdb

package ask

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// markers are values in the fixture store that cannot come from the schema,
// the prompt or the glossary.
var markers = []string{"ZZMARKER", "feedfacefeedface", "987654321"}

func markerStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("rollups/2026-11-01.ndjson",
		`{"sessionId":"feedfacefeedfacefeedface","role":"viewer","browser":"ZZMARKER 1","os":"Linux","durationMs":987654321,"stalls":3}`+"\n"+
			`{"sessionId":"0123456789abcdef01234567","role":"viewer","browser":"Firefox 159","os":"Windows","durationMs":60000,"stalls":0}`+"\n")
	write("sessions/date=2026-11-01/broadcast=0123456789ab/feedfacefeedfacefeedface.ndjson",
		`{"kind":"sample","sessionId":"feedfacefeedfacefeedface","role":"viewer","tMs":987654321,"stats":{"receivedFps":59}}`+"\n")
	write("relay/date=2026-11-01/pod-0.ndjson",
		`{"atMs":1762000000000,"pod":"pod-0","broadcastKey":"0123456789ab","broadcast":{"viewersGlobal":4}}`+"\n")
	return root
}

// docs/84 §4.2, the mode B test: three questions through a recording fake of
// the provider, with drafts that would leak if the check ran against the
// store. No recorded request may carry a marker the model did not write
// itself, and a PIVOT answers the same for a stored value as for an absent
// one.
func TestModeBNothingFromTheStoreReachesTheModel(t *testing.T) {
	root := markerStore(t)
	e, err := sqlengine.OpenIsolated(sqlengine.Options{Root: root, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	// The columns are learned the way the console learns them: by queries.
	for _, v := range sqlengine.ShadowViews {
		if _, err := e.Query("SELECT count(*) FROM " + v); err != nil {
			t.Fatal(err)
		}
	}

	dataGlob := filepath.Join(root, "rollups", "*.ndjson")
	pivotPresent := `SELECT "ZZMARKER 1" FROM (PIVOT rollups ON browser USING count(*))`
	pivotAbsent := `SELECT "ZZMARKER 2" FROM (PIVOT rollups ON browser USING count(*))`
	drafts := [][]string{
		{ // a valid query, and one that would return the markers if it ran
			"SELECT browser, count(*) FROM rollups GROUP BY browser",
			"SELECT sessionId, browser, durationMs FROM rollups ORDER BY durationMs DESC",
		},
		{ // a binder error, and a dynamic PIVOT with a wrong column name
			"SELECT nosuchcol FROM rollups",
			"SELECT nosuchcol FROM (PIVOT rollups ON browser USING count(*))",
		},
		{ // the PIVOT whose OK would confirm a value, and a file read
			pivotPresent,
			pivotAbsent,
			"SELECT * FROM read_json_auto('" + dataGlob + "')",
		},
	}

	var script []reply
	for qi, ds := range drafts {
		for di, d := range ds {
			script = append(script, anthropicToolUse("toolu_"+string(rune('a'+qi))+string(rune('0'+di)), sqlInput(d)))
		}
		script = append(script, anthropicFinal("A draft.", ds[0]))
	}
	f := newFake(t, script...)
	checker, ok := e.(sqlengine.Explainer)
	if !ok {
		t.Fatal("the isolated engine is not an Explainer")
	}
	s := newAnthropicService(t, f, checker, nil)
	for qi := range drafts {
		if _, err := s.Ask(context.Background(), "alice", "question "+string(rune('1'+qi))); err != nil {
			t.Fatalf("question %d: %v", qi+1, err)
		}
	}

	reqs := f.requests()
	if len(reqs) != len(script) {
		t.Fatalf("%d requests, want %d", len(reqs), len(script))
	}
	// What the model wrote itself is echoed back in later requests — its
	// drafts, and a binder error quoting an identifier it named. That is its
	// own text, not the store's.
	var own []string
	ident := regexp.MustCompile(`"([^"]+)"`)
	for _, ds := range drafts {
		for _, d := range ds {
			b, _ := json.Marshal(d)
			own = append(own, string(b[1:len(b)-1]), d)
			for _, m := range ident.FindAllStringSubmatch(d, -1) {
				own = append(own, `\"`+m[1]+`\"`)
			}
		}
	}
	for i, body := range reqs {
		for _, o := range own {
			body = strings.ReplaceAll(body, o, "")
		}
		for _, m := range markers {
			if strings.Contains(body, m) {
				t.Errorf("request %d carries %q from the store:\n%s", i, m, body)
			}
		}
	}

	// The two PIVOT verdicts, as the model received them.
	verdict := func(id string) string {
		re := regexp.MustCompile(`"tool_use_id":"` + id + `"[^}]*?"content":\[\{"text":"((?:[^"\\]|\\.)*)"`)
		for _, b := range reqs {
			if m := re.FindStringSubmatch(b); m != nil {
				return m[1]
			}
		}
		re = regexp.MustCompile(`"content":\[\{"text":"((?:[^"\\]|\\.)*)"[^\]]*\][^}]*"tool_use_id":"` + id + `"`)
		for _, b := range reqs {
			if m := re.FindStringSubmatch(b); m != nil {
				return m[1]
			}
		}
		t.Fatalf("no tool result for %s", id)
		return ""
	}
	present := strings.ReplaceAll(verdict("toolu_c0"), "ZZMARKER 1", "X")
	absent := strings.ReplaceAll(verdict("toolu_c1"), "ZZMARKER 2", "X")
	if present != absent {
		t.Errorf("a PIVOT answers differently for a stored value:\n  present: %s\n  absent:  %s", present, absent)
	}
	if !strings.HasPrefix(present, "ERROR") {
		t.Errorf("a PIVOT's output column bound in the shadow catalogue: %s", present)
	}
	if v := verdict("toolu_c2"); !strings.Contains(v, "ERROR") {
		t.Errorf("read_json_auto over the data directory: %s", v)
	}
	if v := verdict("toolu_a0"); v != "OK" {
		t.Errorf("a valid draft: %s", v)
	}
	if v := verdict("toolu_b0"); !strings.Contains(v, "nosuchcol") {
		t.Errorf("a binder error: %s", v)
	}
}
