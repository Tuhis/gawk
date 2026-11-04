package ask

// The system prompt (docs/84 D5, D5a).
//
// Generated from what the server already serves — each view's ViewDoc, its
// LEARNED columns (what the views bind to today), the field catalogue and the
// glossary — so there is no second copy of the schema to drift (docs/36 D15's
// lesson). It is byte-stable between questions on the same day over the same
// schema, so the provider's prompt cache holds it: about 17.5k tokens full,
// a tenth of the price once cached (§12.2).
//
// Nothing in it is read from a row. The glossary states only enumerations the
// code defines (schema/glossary_test.go).

import (
	"fmt"
	"strings"
	"sync"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// rules is the prompt's fixed head: the NQ0 harness's, which every measured
// result in docs/84 §12 was produced with.
const rules = `You write DuckDB SQL for gawk-telemetry's read-only SQL console, from an operator's question in English.

Rules:
- One read-only statement (SELECT / WITH ...). No semicolons inside.
- You never see data. Use the check_sql tool to verify that a draft parses and binds; fix and re-check on error.
- Questions can be vague. Pick the most sensible interpretation, state it in one sentence, and prefer a query
  that answers it fully (e.g. several metrics side by side for "compare experience").
- Put a time or category column first when the result is a series, so it can be charted.
- When done, reply with exactly:
INTERPRETATION: <one sentence>
SQL:
` + "```sql\n<the query>\n```" + `
Today is %s.

`

// CheckToolName and CheckToolDescription define the model's one tool.
const (
	CheckToolName        = "check_sql"
	CheckToolDescription = "Parse and bind a DuckDB statement against the live schema (EXPLAIN). Returns OK or the DuckDB error. Never returns rows."
)

type promptInput struct {
	Today   string
	Views   []sqlengine.ViewSchema
	Context string
}

// buildPrompt is the system prompt for in.
func buildPrompt(in promptInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, rules, in.Today)
	b.WriteString("Views (DuckDB over NDJSON):\n")
	docs := map[string]string{}
	for _, d := range sqlengine.Views() {
		docs[d.Name] = d.Desc
	}
	cols := map[string][]sqlengine.Column{}
	for _, v := range in.Views {
		cols[v.Name] = v.Columns
	}
	for _, name := range sqlengine.ShadowViews {
		fmt.Fprintf(&b, "\n### %s\n%s", name, docs[name])
		switch {
		case in.Context == ContextSlim && name != "rollups":
			// Slim names the raw views so the model can say it needs one.
			b.WriteString(" (columns not listed here; prefer rollups)\n")
			continue
		case len(cols[name]) == 0:
			b.WriteString(" (no columns learned yet: this view has not been queried on this deployment)\n")
			continue
		}
		b.WriteString("\nColumns:\n")
		for _, c := range cols[name] {
			fmt.Fprintf(&b, "- %s %s\n", c.Name, c.Type)
		}
	}
	if in.Context == ContextFull {
		b.WriteString("\n### Field catalogue (stats fields; they appear as sessions.stats.<f>, rollups.series.<f> or rollups.counters.<f>)\n")
		for _, f := range schema.Catalogue() {
			if f.Legacy {
				continue
			}
			unit := ""
			if f.Unit != "" {
				unit = " [" + f.Unit + "]"
			}
			desc := ""
			if f.Desc != "" {
				desc = ": " + f.Desc
			}
			fmt.Fprintf(&b, "- %s (%s, %s)%s%s\n", f.Name, strings.Join(f.Roles, "/"), f.Semantic, unit, desc)
		}
	}
	b.WriteString("\n")
	b.WriteString(schema.Glossary)
	return b.String()
}

// promptCache keeps the last prompt, rebuilt when its input changes: a new
// day, or a learned schema that gained a column.
type promptCache struct {
	mu     sync.Mutex
	key    string
	prompt string
}

func (c *promptCache) get(in promptInput) string {
	var k strings.Builder
	k.WriteString(in.Today + "\x00" + in.Context)
	for _, v := range in.Views {
		k.WriteString("\x00" + v.Name)
		for _, col := range v.Columns {
			k.WriteString("\x01" + col.Name + "\x02" + col.Type)
		}
	}
	key := k.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	if key != c.key {
		c.key, c.prompt = key, buildPrompt(in)
	}
	return c.prompt
}
