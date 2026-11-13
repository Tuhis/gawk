package sqlengine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The allowlist is the only thing standing between an ops console and a
// `COPY … TO` over the data directory, and it must hold in EVERY build — the
// cgo-free one has no engine to test against, so the rule is tested
// independently of one. A security-relevant rule that only exists in the
// configuration nobody runs locally is a rule nobody checks.
func TestCheckRefusesEverythingThatIsNotARead(t *testing.T) {
	for _, q := range []string{
		"",
		"   ",
		"COPY rollups TO '/data/oops.csv'",
		"INSTALL httpfs",
		"LOAD httpfs",
		"ATTACH '/etc/passwd' AS p",
		"CREATE TABLE t AS SELECT 1",
		"DELETE FROM rollups",
		"UPDATE rollups SET role = 'x'",
		"INSERT INTO rollups VALUES (1)",
		"PRAGMA disable_verification",
		// The engine's budget (Options) is set on its own connection; a query
		// must not be able to lift it.
		"SET memory_limit='100GB'",
		"SET threads=64",
		"RESET memory_limit",
		// The one that actually matters: a second statement hiding behind an
		// allowed first verb walks straight past a first-verb allowlist.
		"SELECT 1; COPY rollups TO '/data/oops.csv'",
	} {
		if _, err := Check(q); !errors.Is(err, ErrRefused) {
			t.Errorf("Check(%q) = %v, want a refusal", q, err)
		}
	}
}

func TestCheckAllowsOrdinaryReads(t *testing.T) {
	for _, q := range []string{
		"SELECT 1",
		"select role, count(*) from rollups group by role",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"DESCRIBE rollups",
		"SUMMARIZE rollups",
		"SHOW TABLES",
		"EXPLAIN SELECT 1",
		"FROM rollups SELECT role", // DuckDB's FROM-first form
		"(SELECT 1) UNION (SELECT 2)",
		// A trailing semicolon is punctuation, not a second statement.
		"SELECT 1;",
	} {
		got, err := Check(q)
		if err != nil {
			t.Errorf("Check(%q) = %v, want allowed", q, err)
			continue
		}
		if strings.HasSuffix(got, ";") {
			t.Errorf("Check(%q) left a trailing semicolon", q)
		}
	}
}

// The catalogue is answerable without an engine, because "there is no engine
// here" and "there is nothing to query" are different facts and an operator on
// a laptop build should be able to tell which one they hit.
func TestViewsAreDescribedInEveryBuild(t *testing.T) {
	views := Views()
	if len(views) == 0 {
		t.Fatal("no views described")
	}
	names := map[string]bool{}
	for _, v := range views {
		names[v.Name] = true
		if v.Desc == "" {
			t.Errorf("view %q has no description", v.Name)
		}
	}
	for _, want := range []string{"sessions", "rollups", "relay", "annotations"} {
		if !names[want] {
			t.Errorf("view %q is missing from the catalogue", want)
		}
	}
	if Compiled() != compiledExpectation {
		t.Errorf("Compiled() = %v; the build tag and the report disagree", Compiled())
	}
}

func TestAutoMemoryLimitIsAQuarterOfTheContainerLimit(t *testing.T) {
	write := func(t *testing.T, rel, body string) string {
		t.Helper()
		root := t.TempDir()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}
	for _, c := range []struct {
		name string
		root string
		want int64
	}{
		{"cgroup v2, 2 GiB", write(t, "memory.max", "2147483648\n"), 512 << 20},
		{"cgroup v1, 2 GiB", write(t, "memory/memory.limit_in_bytes", "2147483648\n"), 512 << 20},
		// The chart's own default limit is 512Mi; a quarter would starve the
		// reader buffers, so the floor applies.
		{"floor", write(t, "memory.max", "536870912\n"), 128 << 20},
		{"v2 unlimited", write(t, "memory.max", "max\n"), 0},
		{"v1 unlimited", write(t, "memory/memory.limit_in_bytes", "9223372036854771712\n"), 0},
		{"no cgroup fs", t.TempDir(), 0},
	} {
		if got := AutoMemoryLimit(c.root); got != c.want {
			t.Errorf("%s: AutoMemoryLimit = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "1024": 1024, "512MiB": 512 << 20, "512 MiB": 512 << 20,
		"1GiB": 1 << 30, "2gb": 2e9, "100MB": 100e6, "64KiB": 64 << 10, "10B": 10,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "auto", "-1MiB", "1.5GiB", "12XB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted a non-size", in)
		}
	}
}

// EXPLAIN ANALYZE runs the statement it explains. With "explain" on the
// allowlist and nothing looking past it, `EXPLAIN ANALYZE COPY … TO` wrote a
// file into the store from the console.
func TestCheckLooksPastExplainAtTheStatementItRuns(t *testing.T) {
	refused := []string{
		"EXPLAIN ANALYZE COPY (SELECT 1) TO '/data/rollups/x.ndjson'",
		"explain analyze copy (select 1) to 'x'",
		"EXPLAIN COPY (SELECT 1) TO 'x'",
		"EXPLAIN (ANALYZE) COPY (SELECT 1) TO 'x'",
		"EXPLAIN (ANALYZE, FORMAT JSON) COPY (SELECT 1) TO 'x'",
		"EXPLAIN ANALYZE VERBOSE INSERT INTO t VALUES (1)",
		"EXPLAIN ANALYZE\nDELETE FROM rollups",
		"EXPLAIN ANALYZE /* c */ COPY (SELECT 1) TO 'x'",
		"EXPLAIN ANALYZE/**/COPY (SELECT 1) TO 'x'",
		"EXPLAIN EXPLAIN ANALYZE COPY (SELECT 1) TO 'x'",
		"EXPLAIN (FORMAT JSON",
		// A parenthesis inside a comment or a string must not move where the
		// option list is taken to end (PR #783 review).
		"EXPLAIN (ANALYZE /* ) SELECT */) COPY (SELECT 1) TO 'x'",
		"EXPLAIN (ANALYZE -- ) SELECT\n) COPY (SELECT 1) TO 'x'",
		"EXPLAIN (ANALYZE, FORMAT ')') SELECT */ COPY (SELECT 1) TO 'x'",
		"EXPLAIN (ANALYZE, FORMAT \")\") SELECT */ COPY (SELECT 1) TO 'x'",
		// Only a reserved word can open a parenthesised statement: SHOW and
		// friends could be read as option names.
		"EXPLAIN (SHOW) COPY (SELECT 1) TO 'x'",
		"EXPLAIN (DESCRIBE, ANALYZE) COPY (SELECT 1) TO 'x'",
		"EXPLAIN (PIVOT /* */) COPY (SELECT 1) TO 'x'",
		"EXPLAIN ANALYZE",
		"EXPLAIN",
	}
	for _, q := range refused {
		if _, err := Check(q); !errors.Is(err, ErrRefused) {
			t.Errorf("Check(%q) = %v, want ErrRefused", q, err)
		}
	}
	allowed := []string{
		"EXPLAIN SELECT 1",
		"EXPLAIN ANALYZE SELECT count(*) FROM sessions",
		"explain analyze with x as (select 1) select * from x",
		"EXPLAIN (FORMAT JSON) SELECT 1",
		"EXPLAIN (ANALYZE, FORMAT JSON) SELECT 1",
		"EXPLAIN ANALYZE (SELECT 1) UNION ALL (SELECT 2)",
		"EXPLAIN (SELECT 1)",
		"EXPLAIN ANALYZE FROM rollups",
		"EXPLAIN ANALYZE\n  SELECT 1;",
		"EXPLAIN ANALYZE SELECT count(*) FROM rollups WHERE role = 'viewer' -- (why",
		"EXPLAIN (ANALYZE, FORMAT JSON)\nSELECT /* ( */ ')' FROM rollups",
		"EXPLAIN ( FORMAT  json ,ANALYZE )SELECT 1",
		"EXPLAIN (WITH x AS (SELECT 1) SELECT * FROM x)",
	}
	for _, q := range allowed {
		if _, err := Check(q); err != nil {
			t.Errorf("Check(%q) = %v, want it allowed", q, err)
		}
	}
}
