package sqlengine

import (
	"errors"
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
