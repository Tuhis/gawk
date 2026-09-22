package storedshape_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/annotations"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/storedshape"
)

const golden = "testdata/stored-shape.golden"

const header = `# The shape of every row the telemetry service stores, one "path type" per
# line, keyed by the SQL view that reads it. Enforced by storedshape_test.go:
# docs/33 D4, "additive forever".
#
# NEVER edit a recorded line to make the test pass. A changed type or a
# removed path means rows already on disk (rollups are permanent) disagree
# with rows written from now on, and the SQL views union them by name — the
# column turns to JSON for every query over history. Add a NEW field instead.
#
# New fields are recorded with:
#   GAWK_UPDATE_GOLDEN=1 go test ./internal/storedshape/
# which appends and never rewrites.
`

// kindType maps an ingest Kind to the JSON type a sanitized value has on
// disk: a known field of the wrong type is DROPPED at ingest (D15), so the
// Kind is exactly what the stored column holds.
var kindType = map[schema.Kind]string{
	schema.KindNumber: storedshape.Number,
	schema.KindBool:   storedshape.Boolean,
	schema.KindString: storedshape.String,
	schema.KindObject: storedshape.Object,
	schema.KindAny:    storedshape.Any,
}

// current is everything the service writes, as the views see it.
func current(t *testing.T) map[string]string {
	t.Helper()
	all := map[string]string{}
	add := func(m map[string]string) {
		for p, typ := range m {
			all[p] = typ
		}
	}
	add(storedshape.Of("rollups", reflect.TypeOf(rollup.Row{})))
	// Row.Verdict is pre-marshaled JSON; what goes into it is the diagnose()
	// report (cmd/gawk-telemetry rollupFinalize → API.DiagnoseRow), and it is
	// as permanent as the rest of the row.
	add(storedshape.Of("rollups.verdict", reflect.TypeOf(rules.Report{})))
	all["rollups.verdict"] = storedshape.Object
	add(storedshape.Of("sessions", reflect.TypeOf(sessions.Record{})))
	add(storedshape.Of("relay", reflect.TypeOf(relayscrape.Observation{})))
	add(storedshape.Of("annotations", reflect.TypeOf(annotations.Annotation{})))
	// The typed stats fields. Both roles' samples land in the one `sessions`
	// view, so they share one namespace (checked below).
	for _, table := range []map[string]schema.Kind{schema.ViewerFields, schema.BroadcasterFields} {
		for name, k := range table {
			typ, ok := kindType[k]
			if !ok {
				t.Fatalf("stats field %s has a Kind this test cannot map; add it to kindType", name)
			}
			all["sessions.stats."+name] = typ
		}
	}
	for p, typ := range all {
		if strings.HasPrefix(typ, "unsupported:") {
			t.Fatalf("%s: %s — teach storedshape.Of this kind", p, typ)
		}
	}
	return all
}

func TestStoredShapesAreAdditiveOnly(t *testing.T) {
	cur := current(t)
	b, err := os.ReadFile(golden)
	if os.IsNotExist(err) && os.Getenv("GAWK_UPDATE_GOLDEN") != "" {
		b, err = nil, nil // first recording
	}
	if err != nil {
		t.Fatalf("%v (create it with GAWK_UPDATE_GOLDEN=1)", err)
	}
	rec, err := storedshape.Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	d := storedshape.Compare(rec, cur)

	for _, c := range d.Changed {
		t.Errorf("stored field changed type: %s\n"+
			"\trows already on disk keep the old type, and the SQL views union them by name —\n"+
			"\tthe column becomes JSON over all history. Add a new field instead (docs/33 D4).", c)
	}
	for _, p := range d.Removed {
		t.Errorf("stored field removed or renamed: %s\n"+
			"\trows already on disk keep it forever; readers and queries must still find it (docs/33 D4).", p)
	}
	if len(d.Added) == 0 {
		return
	}
	// A rename shows up as one Removed plus one Added; recording the Added
	// half would bless the new name and leave only the removal to argue with.
	if len(d.Changed)+len(d.Removed) > 0 {
		t.Errorf("not recording new fields while recorded ones changed or disappeared:\n\t%s",
			strings.Join(d.Added, "\n\t"))
		return
	}
	if os.Getenv("GAWK_UPDATE_GOLDEN") == "" {
		t.Errorf("new stored fields are not recorded yet:\n\t%s\n"+
			"record them with: GAWK_UPDATE_GOLDEN=1 go test ./internal/storedshape/",
			strings.Join(d.Added, "\n\t"))
		return
	}
	// Append-only: recorded lines are carried over as they are, so this can
	// never bless a Changed or Removed entry.
	merged := map[string]string{}
	for p, typ := range rec {
		merged[p] = typ
	}
	for _, line := range d.Added {
		p, typ, _ := strings.Cut(line, " ")
		merged[p] = typ
	}
	if err := os.WriteFile(golden, storedshape.Format(header, merged), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d new stored field(s)", len(d.Added))
}

// A stats field typed differently by the two roles is a type change inside ONE
// column: both roles' samples are read through the same `sessions` view.
func TestStatsFieldsAgreeAcrossRoles(t *testing.T) {
	for name, vk := range schema.ViewerFields {
		if bk, ok := schema.BroadcasterFields[name]; ok && bk != vk {
			t.Errorf("stats field %q is Kind %v for viewers and %v for broadcasters; the sessions view unions both", name, vk, bk)
		}
	}
}

// The hive-partitioned views get a column per path key — sessions/date=…/
// broadcast=…, relay/date=… — so a stored top-level field of the same name
// collides with the partition column.
func TestNoStoredFieldShadowsAPartitionKey(t *testing.T) {
	cur := current(t)
	var bad []string
	for view, keys := range map[string][]string{
		"sessions": {"date", "broadcast"},
		"relay":    {"date"},
	} {
		for _, key := range keys {
			if _, ok := cur[view+"."+key]; ok {
				bad = append(bad, view+"."+key)
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("stored fields collide with hive partition keys: %v", bad)
	}
}

func TestOfFlattensLikeEncodingJSON(t *testing.T) {
	type inner struct {
		N int `json:"n"`
	}
	type Embedded struct {
		E bool `json:"e"`
	}
	type row struct {
		Embedded
		S       string            `json:"s,omitempty"`
		P       *inner            `json:"p"`
		M       map[string]*inner `json:"m"`
		L       []string          `json:"l"`
		A       any               `json:"a"`
		Q       int64             `json:"q,string"`
		Skipped string            `json:"-"`
		hidden  int
		Untag   float64
	}
	got := storedshape.Of("v", reflect.TypeOf(row{}))
	want := map[string]string{
		"v.e": "boolean", "v.s": "string",
		"v.p": "object", "v.p.n": "number",
		"v.m": "map", "v.m.*": "object", "v.m.*.n": "number",
		"v.l": "array", "v.l[]": "string",
		"v.a": "any", "v.q": "string", "v.Untag": "number",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Of =\n%v\nwant\n%v", got, want)
	}
}

func TestCompareSeparatesTheThreeCases(t *testing.T) {
	d := storedshape.Compare(
		map[string]string{"a": "number", "b": "string", "c": "boolean"},
		map[string]string{"a": "string", "c": "boolean", "d": "number"},
	)
	if !reflect.DeepEqual(d.Changed, []string{"a: number → string"}) ||
		!reflect.DeepEqual(d.Removed, []string{"b"}) ||
		!reflect.DeepEqual(d.Added, []string{"d number"}) {
		t.Errorf("Compare = %+v", d)
	}
}

func TestParseRejectsAMalformedGolden(t *testing.T) {
	for _, in := range []string{"a\n", "a number extra\n", "a number\na string\n"} {
		if _, err := storedshape.Parse(strings.NewReader(in)); err == nil {
			t.Errorf("Parse(%q) accepted it", in)
		}
	}
	m, err := storedshape.Parse(strings.NewReader("# c\n\na number\n"))
	if err != nil || m["a"] != "number" || len(m) != 1 {
		t.Errorf("Parse = %v, %v", m, err)
	}
}

// The golden is only a guard if it lives where the test reads it.
func TestGoldenIsCheckedIn(t *testing.T) {
	if _, err := os.Stat(filepath.Join("testdata", "stored-shape.golden")); err != nil {
		t.Fatal(err)
	}
}
