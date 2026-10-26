package sqlengine

import "testing"

// Every type the learned schema stores is written back out and read in again
// by the next worker; anything that does not survive the trip is a column that
// silently changes type between two queries.
func TestColumnTypesSurviveAWriteAndARead(t *testing.T) {
	for _, s := range []string{
		"BIGINT",
		"TIMESTAMP WITH TIME ZONE",
		"DECIMAL(18,3)",
		"BIGINT[]",
		"JSON[][]",
		"MAP(VARCHAR, BIGINT)",
		`STRUCT("FramesDroppedAtSend" BIGINT, "we ird""q" VARCHAR, "nested" STRUCT("x" DOUBLE[]))`,
		`STRUCT("m" MAP(VARCHAR, STRUCT("a" BOOLEAN)))[]`,
		"NULL",
	} {
		ty, err := parseType(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got := ty.String(); got != s {
			t.Errorf("%s came back as %s", s, got)
		}
	}
}

// DESCRIBE quotes a field name only when it has to; the learned form always
// does. Both must read as the same type.
func TestColumnTypesReadDescribesUnquotedNames(t *testing.T) {
	ty, err := parseType(`STRUCT(fps DOUBLE, codec VARCHAR, "we ird""q" BIGINT, nested STRUCT(x BIGINT, y VARCHAR))`)
	if err != nil {
		t.Fatal(err)
	}
	want := `STRUCT("fps" DOUBLE, "codec" VARCHAR, "we ird""q" BIGINT, "nested" STRUCT("x" BIGINT, "y" VARCHAR))`
	if got := ty.String(); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestColumnTypesRefuseWhatTheyCannotRead(t *testing.T) {
	for _, s := range []string{"", "STRUCT(", "STRUCT(a)", "MAP(VARCHAR)", "BIGINT[", `STRUCT("a BIGINT)`, "DECIMAL(18,3", "BIGINT junk("} {
		if _, err := parseType(s); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
}

// The merge rules, as pure functions. learn_test.go pins the same pairs
// against DuckDB's own answer.
func TestMergeWidensAsTheSnifferWould(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"BIGINT", "BIGINT", "BIGINT"},
		{"BIGINT", "DOUBLE", "DOUBLE"},
		{"BIGINT", "VARCHAR", "JSON"},
		{"BOOLEAN", "BIGINT", "JSON"},
		{"DATE", "VARCHAR", "VARCHAR"},
		{"DATE", "TIMESTAMP", "VARCHAR"},
		{"NULL", "BIGINT", "BIGINT"},
		{"BIGINT", "NULL", "BIGINT"},
		{"JSON", "BIGINT", "JSON"},
		{"BIGINT[]", "VARCHAR[]", "JSON[]"},
		{"NULL[]", "BIGINT[]", "BIGINT[]"},
		{"BIGINT[]", "BIGINT", "JSON"},
		{`STRUCT("a" BIGINT)`, "BIGINT", "JSON"},
		{`STRUCT("a" BIGINT)`, "BIGINT[]", "JSON"},
		{`STRUCT("a" BIGINT)`, `STRUCT("b" VARCHAR)`, `STRUCT("a" BIGINT, "b" VARCHAR)`},
		{`STRUCT("a" BIGINT, "b" BIGINT)`, `STRUCT("b" DOUBLE, "c" VARCHAR)`, `STRUCT("a" BIGINT, "b" DOUBLE, "c" VARCHAR)`},
		{"MAP(VARCHAR, NULL)", `STRUCT("a" BIGINT)`, `STRUCT("a" BIGINT)`},
		{`STRUCT("a" BIGINT)`, "MAP(VARCHAR, NULL)", `STRUCT("a" BIGINT)`},
		{"MAP(VARCHAR, BIGINT)", `STRUCT("a" BIGINT)`, "JSON"},
		{"MAP(VARCHAR, BIGINT)", "MAP(VARCHAR, DOUBLE)", "MAP(VARCHAR, DOUBLE)"},
		// Case-insensitive, and the first spelling wins outright — DuckDB
		// reads only it, so the second's type never matters.
		{`STRUCT("FramesDroppedAtSend" BIGINT)`, `STRUCT("framesDroppedAtSend" DOUBLE)`, `STRUCT("FramesDroppedAtSend" BIGINT)`},
		{`STRUCT("a" BIGINT)`, `STRUCT("a" DOUBLE, "A" VARCHAR)`, `STRUCT("a" DOUBLE)`},
	} {
		a, err := parseType(c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := parseType(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := merge(a, b).String(); got != c.want {
			t.Errorf("%s ⊔ %s = %s, want %s", c.a, c.b, got, c.want)
		}
		if a.String() != c.a || b.String() != c.b {
			t.Errorf("%s ⊔ %s modified an input", c.a, c.b)
		}
	}
}

// A JSON leaf in a sniff is "never saw a value", not a conflict; once
// learned, a conflict is JSON for good. Emitted, an unknown is JSON.
func TestASniffedJSONLeafIsUnknownUntilSomethingSaysMore(t *testing.T) {
	s, err := parseType(`STRUCT(v JSON, w JSON[], m MAP(VARCHAR, JSON), n STRUCT(x JSON))`)
	if err != nil {
		t.Fatal(err)
	}
	learned := sniffed(s)
	if got, want := learned.String(), `STRUCT("v" NULL, "w" NULL[], "m" MAP(VARCHAR, NULL), "n" STRUCT("x" NULL))`; got != want {
		t.Fatalf("learned %s, want %s", got, want)
	}
	if got, want := learned.emit(), `STRUCT("v" JSON, "w" JSON[], "m" MAP(VARCHAR, JSON), "n" STRUCT("x" JSON))`; got != want {
		t.Fatalf("emitted %s, want %s", got, want)
	}
	conflict, _ := parseType("JSON")
	clean, _ := parseType("BIGINT")
	if got := merge(conflict, clean).String(); got != "JSON" {
		t.Fatalf("a learned conflict was narrowed to %s", got)
	}
}
