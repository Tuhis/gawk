package sqlengine

// The shadow catalogue: checking a statement without reading anything (R82,
// docs/84 D2).
//
// The console's "Ask in English" box lets a model check its draft before the
// operator sees it. Checking against the real views is not data-free: a
// dynamic `PIVOT … ON col` with no IN list runs `SELECT DISTINCT col` while
// binding, so a binder error's candidate list carried the values of `os`, and
// whether `SELECT "Windows" FROM (PIVOT …)` bound answered whether a value
// exists. File table functions (`read_json_auto`, `glob`) sniff files at bind
// time and can quote them in an error (docs/84 §12.6).
//
// So Explain never touches the store. It binds the statement in a fresh,
// in-memory DuckDB holding one EMPTY table per view, created from the learned
// column list (learn.go) — the same list the views bind to, so a statement
// that binds here binds there — with external access off and the
// configuration locked. There is no row and no file to leak, whatever the
// syntax. Refusing PIVOT by keyword would leave the next construct open.
//
// The catalogue is read from the learned schemas each time, so it is never
// staler than the views: a column the last console query learned is in the
// next check.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// ShadowViews are the views the shadow catalogue describes. `annotations` is
// left out on purpose: its rows are operator prose, and asking about it is out
// of R82's scope (docs/84 §6).
var ShadowViews = []string{"rollups", "sessions", "relay"}

// MaxExplainBytes bounds a check's answer (docs/84 D2): enough for any binder
// error DuckDB writes, short enough that a pathological one costs little.
const MaxExplainBytes = 1500

// ExplainOK is a check's answer for a statement that binds.
const ExplainOK = "OK"

// Column is one column of a view, as the views bind to it.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ViewSchema is one view's columns, in the order the view has them.
type ViewSchema struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
}

// Explainer checks a statement in the shadow catalogue. The isolated engine
// implements it; the in-process one does not, because the service never opens
// DuckDB itself.
type Explainer interface {
	// Explain answers ExplainOK or "ERROR: " and DuckDB's text, truncated to
	// MaxExplainBytes. A statement Check refuses is answered with the refusal,
	// without starting a worker. An error means the checker itself failed.
	Explain(ctx context.Context, sql string) (string, error)
	// Schema is the shadow catalogue's columns: the learned schema.
	Schema() ([]ViewSchema, error)
}

// partitionColumns are the hive keys each view adds after its file columns,
// in the order DuckDB adds them (by name: `broadcast` before `date`).
// `date` is a DATE because every value is one (`date=2026-07-29`); the
// broadcast key is hex, and a key that happened to be all digits would still
// be a key, so it is text.
var partitionColumns = map[string][]Column{
	"sessions": {{"broadcast", "VARCHAR"}, {"date", "DATE"}},
	"relay":    {{"date", "DATE"}},
}

// LearnedSchema reads the learned column list of each shadow view from the
// data directory (SchemaDir), in the view's order and with its partition
// columns. A view with nothing learned yet is absent: its files have not been
// queried, so nothing says what its columns are.
func LearnedSchema(root string) ([]ViewSchema, error) {
	var out []ViewSchema
	for _, name := range ShadowViews {
		b, err := os.ReadFile(filepath.Join(SchemaDir(root), name+".json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var l learned
		if err := json.Unmarshal(b, &l); err != nil || l.Version != learnedVersion {
			continue
		}
		t, err := parseType(l.Columns)
		if err != nil || t.kind != structKind || len(t.fields) == 0 {
			continue
		}
		v := ViewSchema{Name: name}
		for _, f := range t.fields {
			if !identifierRE.MatchString(f.name) {
				continue
			}
			var allow map[string]bool
			if name == "sessions" && f.name == "stats" {
				allow = statsFields
			}
			typ := safeType(f.t, allow, 1).emit()
			if len(typ) > maxShadowTypeBytes {
				// However a store grew it, no column outgrows its share
				// of the prompt.
				typ = jsonType
			}
			v.Columns = append(v.Columns, Column{f.name, typ})
			if len(v.Columns) >= maxShadowFields {
				break
			}
		}
		v.Columns = append(v.Columns, partitionColumns[name]...)
		out = append(out, v)
	}
	return out, nil
}

// The learned schema is not all the code's own words. Public ingest keeps an
// unknown stats key verbatim (schema.SanitizeStats, docs/36 D15), and the
// learner records it as a STRUCT field of `sessions.stats` — so a client could
// write a "column name" that is an instruction to the model, and have it put
// in every question's system prompt and in the binder errors a check returns
// (PR #706 review). The shadow catalogue therefore keeps only:
//
//   - names shaped like identifiers (identifierRE), at every depth;
//   - under `sessions.stats`, only the fields the schema tables define —
//     the rest are what a client invented — and an object field among them
//     (KindObject, KindAny) as JSON: ingest keeps its members verbatim, so an
//     identifier-shaped instruction would pass the pattern. `->>` reads a
//     member in the check and on the real STRUCT alike;
//   - at most maxShadowFields per struct the code does not bound (the stats
//     allowlist does), maxShadowDepth levels, and maxShadowTypeBytes per
//     column, past which the value is JSON.
//
// Every other struct is a Go type's (rollup.Row, relayscrape.Observation),
// whose names the code writes. A draft that reads a dropped field fails to
// bind in the check; the console still runs it.
var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

const (
	maxShadowFields    = 128
	maxShadowDepth     = 4
	maxShadowTypeBytes = 16 << 10
)

// statsFields is every stats field the schema tables define, both roles;
// statsObjects the ones whose members a client chooses.
var statsFields, statsObjects = func() (fields, objects map[string]bool) {
	fields, objects = map[string]bool{}, map[string]bool{}
	for _, table := range []map[string]schema.Kind{schema.ViewerFields, schema.BroadcasterFields} {
		for name, k := range table {
			fields[name] = true
			if k == schema.KindObject || k == schema.KindAny {
				objects[name] = true
			}
		}
	}
	return fields, objects
}()

// StatsObjectFields are the `sessions.stats` fields the shadow catalogue
// types as JSON, sorted, for the prompt to name.
func StatsObjectFields() []string {
	out := make([]string, 0, len(statsObjects))
	for name := range statsObjects {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// safeType is t with only the names the shadow catalogue may carry. allow,
// when set, is the only field names t's top level may keep.
func safeType(t *colType, allow map[string]bool, depth int) *colType {
	switch t.kind {
	case structKind:
		if depth > maxShadowDepth {
			return scalar(jsonType)
		}
		out := &colType{kind: structKind}
		for _, f := range t.fields {
			if !identifierRE.MatchString(f.name) || (allow != nil && !allow[f.name]) {
				continue
			}
			ft := safeType(f.t, nil, depth+1)
			if allow != nil && statsObjects[f.name] {
				ft = scalar(jsonType)
			}
			out.fields = append(out.fields, colField{f.name, ft})
			// An allowlisted struct is bounded by its allowlist; capping it
			// would cut catalogued fields (there are over 200 stats fields).
			if allow == nil && len(out.fields) >= maxShadowFields {
				break
			}
		}
		if len(out.fields) == 0 {
			return scalar(jsonType)
		}
		return out
	case listKind:
		return &colType{kind: listKind, elem: safeType(t.elem, nil, depth+1)}
	case mapKind:
		return &colType{kind: mapKind, key: safeType(t.key, nil, depth+1), elem: safeType(t.elem, nil, depth+1)}
	}
	return t
}

// shadowDDL is the CREATE TABLE per view.
func shadowDDL(views []ViewSchema) []string {
	out := make([]string, 0, len(views))
	for _, v := range views {
		cols := make([]string, len(v.Columns))
		for i, c := range v.Columns {
			cols[i] = quoteIdent(c.Name) + " " + c.Type
		}
		out = append(out, fmt.Sprintf("CREATE TABLE %s (%s)", quoteIdent(v.Name), strings.Join(cols, ", ")))
	}
	return out
}

// truncateVerdict bounds a check's answer on a rune boundary.
func truncateVerdict(s string) string {
	if len(s) <= MaxExplainBytes {
		return s
	}
	cut := MaxExplainBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// errorVerdict is a check's answer for a statement that does not bind.
func errorVerdict(msg string) string {
	return truncateVerdict("ERROR: " + strings.TrimSpace(msg))
}

// explainTimeout bounds one check. An EXPLAIN over empty tables answers in
// milliseconds; the rest is the worker's start.
const explainTimeout = 15 * time.Second

// Schema is the shadow catalogue's columns (Explainer).
func (e *isolated) Schema() ([]ViewSchema, error) { return LearnedSchema(e.opts.Root) }

// Explain checks sql in the shadow catalogue, in a worker of its own
// (Explainer). The worker gets the catalogue's DDL and the statement and
// nothing else: no data directory, no budget that could spill, and an empty
// environment, so not even a credential in the service's environment is
// within reach of a function that reads one.
func (e *isolated) Explain(ctx context.Context, sql string) (string, error) {
	stmt, err := Check(sql)
	if err != nil {
		return errorVerdict(strings.TrimPrefix(err.Error(), "sqlengine: ")), nil
	}
	views, err := e.Schema()
	if err != nil {
		return "", fmt.Errorf("sqlengine: reading the learned schema: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, explainTimeout)
	defer cancel()
	select {
	case e.checkTurn <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-e.checkTurn }()

	req, err := json.Marshal(workerRequest{Explain: true, Shadow: shadowDDL(views), SQL: stmt})
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, e.exe)
	cmd.Env = []string{WorkerEnv + "=1"}
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(req)
	var stdout bytes.Buffer
	stderr := &tailBuffer{limit: workerStderrLimit}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = workerKillGrace
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("sqlengine: the check did not finish within %s: %w", explainTimeout, ctx.Err())
	}
	var resp workerResponse
	if err := json.NewDecoder(&stdout).Decode(&resp); err != nil {
		if runErr == nil {
			runErr = err
		}
		return "", fmt.Errorf("sqlengine: check worker failed without answering (%v): %s", runErr, strings.TrimSpace(stderr.String()))
	}
	if resp.Err != "" {
		return "", errors.New(resp.Err)
	}
	if resp.Verdict == "" {
		return "", errors.New("sqlengine: check worker answered without a verdict")
	}
	return resp.Verdict, nil
}
