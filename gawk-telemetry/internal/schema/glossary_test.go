package schema

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// storedShape is every stored path (storedshape's golden), keyed by view, with
// list markers dropped: `rollups.verdict.findings[].id` is `verdict.findings.id`
// of rollups.
func storedShape(t *testing.T) map[string]map[string]bool {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "storedshape", "testdata", "stored-shape.golden"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path := strings.Fields(line)[0]
		view, rest, ok := strings.Cut(path, ".")
		if !ok {
			continue
		}
		if out[view] == nil {
			out[view] = map[string]bool{}
		}
		out[view][strings.ReplaceAll(rest, "[]", "")] = true
	}
	return out
}

// codeValues are the values the code defines, read from the code that defines
// them: the web app's describeClient (browser families and os), the native
// distributions' identities, and the roles the field tables are keyed by.
func codeValues(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range Catalogue() {
		for _, r := range f.Roles {
			out[r] = true
		}
	}
	lit := regexp.MustCompile(`["']([A-Za-z][A-Za-z0-9 -]*)["']`)
	for _, src := range []string{
		"gawk-app/src/lib/telemetry.ts",
		"gawk-broadcast-desktop/crates/engine/src/lib.rs",
		"gawk-ios/rust/crates/core/src/identity.rs",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", src))
		if err != nil {
			t.Fatalf("reading %s, which defines values the glossary names: %v", src, err)
		}
		for _, m := range lit.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// notIdentifiers are backticked words that are neither a column nor a value:
// SQL the glossary tells the model to use or to avoid, and the function its
// browser values come from.
var notIdentifiers = map[string]bool{
	"max": true, "sum": true, "unnest": true,
	"day": true, "share": true, "value": true, "from": true, "scope": true,
	"describeClient": true,
	// The removed Go Linux broadcaster's distribution name (R56 LX9): no code
	// writes it any more, and rollups from before keep it for good.
	"gawk-broadcast": true,
}

// glossaryProblems is every backticked span in glossary that is neither an
// expression, a stored column, a catalogued field nor a value the code defines.
func glossaryProblems(t *testing.T, glossary string) []string {
	t.Helper()
	shape := storedShape(t)
	values := codeValues(t)
	fields := map[string]bool{}
	for _, f := range Catalogue() {
		fields[f.Name] = true
	}
	views := map[string]bool{"rollups": true, "sessions": true, "relay": true}
	partitions := map[string]bool{"date": true, "broadcast": true}
	version := regexp.MustCompile(`^\d+$`)

	resolves := func(id string) bool {
		if views[id] || partitions[id] {
			return true
		}
		id = strings.ReplaceAll(id, "[]", "")
		for _, paths := range shape {
			if paths[id] {
				return true
			}
		}
		// series.<field> and counters.<field> are maps keyed by the catalogue.
		if head, field, ok := strings.Cut(id, "."); ok && (head == "series" || head == "counters") {
			return field == "<field>" || fields[field]
		}
		return false
	}

	var out []string
	for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(glossary, -1) {
		s := m[1]
		switch {
		case strings.ContainsAny(s, "()='/*+"):
			// An expression the glossary recommends, not a name.
			continue
		case notIdentifiers[s], resolves(s), values[s]:
			continue
		}
		// A browser value with its major version: the family must be defined.
		if fam, ver, ok := strings.Cut(s, " "); ok && values[fam] && version.MatchString(ver) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// docs/84 D6: every backticked identifier in the glossary resolves, and every
// value it names is one the code defines.
func TestGlossaryNamesOnlyWhatExists(t *testing.T) {
	if n := strings.Count(Glossary, "`"); n < 40 {
		t.Fatalf("found %d backticks; the glossary did not embed", n)
	}
	for _, s := range glossaryProblems(t, Glossary) {
		t.Errorf("glossary names `%s`, which is neither a stored column, a catalogued field nor a value the code defines", s)
	}
}

// The check must be able to fail: a renamed column, an unknown field and a
// value sampled from a store are each caught.
func TestGlossaryCheckCatchesWhatDoesNotExist(t *testing.T) {
	doctored := Glossary + "\n- `verdict.findingz[].id`, `counters.noSuchCounter`, `Netscape 4`, `Linux Mint`.\n"
	got := strings.Join(glossaryProblems(t, doctored), ",")
	if got != "verdict.findingz[].id,counters.noSuchCounter,Netscape 4,Linux Mint" {
		t.Errorf("problems = %q", got)
	}
}
