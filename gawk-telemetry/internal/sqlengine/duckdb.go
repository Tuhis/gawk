//go:build duckdb

package sqlengine

// The cgo build (§8 Q1's resolution). Compiled only with `-tags duckdb`, which
// is what the deployed image uses; a fresh clone gets nodriver.go instead.
//
// The views registered here are the D11 recipe made permanent. `read_json_auto`
// with `hive_partitioning=1` prunes by path rather than scanning, which is the
// whole reason the store's layout is `date=…/broadcast=…` — so an operator's
// `WHERE date = '2026-07-29'` costs one directory instead of the tree.
//
// `union_by_name=1` is not optional here: stats objects grow every milestone
// (D15's "version skew is permanent"), so two partitions genuinely have
// different columns, and positional union would either fail or silently
// misalign them.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
)

type engine struct {
	db   *sql.DB
	opts Options

	// mu guards views. The registrations themselves are serialised by the
	// single connection; the lock keeps the catalogue coherent for Views()
	// callers that never go through Query.
	mu    sync.Mutex
	views []ViewDoc
}

// viewDrift is DuckDB's binder error for a view whose SELECT * no longer binds
// to the column list recorded at CREATE VIEW time. Matched on the message
// because the driver types it only as a generic binder error; the drift test
// pins the text, so a DuckDB upgrade that rewords it fails CI rather than
// silently disabling the retry.
const viewDrift = "Contents of view were altered"

// Open builds an in-memory DuckDB with views over the store's partitions.
func Open(opts Options) (Engine, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("sqlengine: Root is required")
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	// One connection. The engine is an operator console, not a serving path,
	// and a single connection makes the timeout below the only concurrency
	// control that has to be reasoned about.
	db.SetMaxOpenConns(1)
	if err := applyBudget(db, opts); err != nil {
		db.Close()
		return nil, err
	}

	e := &engine{db: db, opts: opts, views: viewDocs(func(string) bool { return false })}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()
	e.register(ctx, true)
	return e, nil
}

// applyBudget sets the engine's resource budget (Options). These are SETs on
// the engine's own connection, never reachable from a query: Check refuses
// SET, so an operator cannot raise the budget from the console.
func applyBudget(db *sql.DB, opts Options) error {
	stmts := []string{
		fmt.Sprintf("SET threads=%d", opts.threads()),
		// An unordered scan streams; an ordered one buffers to preserve file
		// order. A query that cares about order says ORDER BY.
		"SET preserve_insertion_order=false",
	}
	if opts.MemoryLimit > 0 {
		stmts = append(stmts, fmt.Sprintf("SET memory_limit='%dB'", opts.MemoryLimit))
	}
	if opts.SpillLimit > 0 {
		dir := SpillDir(opts.Root)
		// Whatever a previous process left is garbage: spill files are only
		// meaningful to the query that wrote them.
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("sqlengine: clearing spill dir: %w", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("sqlengine: spill dir: %w", err)
		}
		stmts = append(stmts,
			fmt.Sprintf("SET temp_directory='%s'", dir),
			fmt.Sprintf("SET max_temp_directory_size='%dB'", opts.SpillLimit))
	} else {
		// Empty disables spilling outright, rather than letting DuckDB default
		// to a .tmp beside the working directory on a read-only root.
		stmts = append(stmts, "SET temp_directory=''")
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("sqlengine: %s: %w", s, err)
		}
	}
	return nil
}

// register (re)creates the views; all=false touches only the unavailable ones.
//
// Registration is NOT a one-off at startup, and must not go back to being one.
// DuckDB binds a view's column names and types when it is created, and these
// views sit over globs that keep growing for the life of the pod: the first
// partition that added a field (every milestone does, D15) or widened a type
// made every query on that view — even count(*) — fail with "Contents of view
// were altered" until the pod restarted. Likewise a tree that was empty at boot
// (annotations before the first note, relay before the first scrape) stayed
// unqueryable. Query heals both.
func (e *engine) register(ctx context.Context, all bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ok := map[string]bool{}
	for _, v := range e.views {
		ok[v.Name] = v.Available
	}
	for name, src := range viewSources(e.opts.Root) {
		if ok[name] && !all {
			continue
		}
		hive := 0
		if src.hive {
			hive = 1
		}
		stmt := fmt.Sprintf(
			`CREATE OR REPLACE VIEW %s AS SELECT * FROM read_json_auto('%s', hive_partitioning=%d, union_by_name=1, ignore_errors=true)`,
			name, src.glob, hive)
		// A view whose tree is empty simply fails to register, and that is not
		// an error worth refusing to start over: a fleet with no relay
		// configured has no relay/ partitions at all, and the console should
		// still answer questions about sessions. The failure is REPORTED via
		// ViewDoc.Available rather than swallowed. A failed re-registration
		// drops the old view, so Available and the catalogue never disagree.
		if _, err := e.db.ExecContext(ctx, stmt); err == nil {
			ok[name] = true
		} else {
			ok[name] = false
			_, _ = e.db.ExecContext(ctx, "DROP VIEW IF EXISTS "+name)
		}
	}
	e.views = viewDocs(func(n string) bool { return ok[n] })
}

// Compiled reports that this build carries an engine.
func Compiled() bool { return true }

// Views is the catalogue, for parity with the stub build.
func Views() []ViewDoc { return viewDocs(func(string) bool { return true }) }

func (e *engine) Views() []ViewDoc {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ViewDoc(nil), e.views...)
}

func (e *engine) Close() error { return e.db.Close() }

func (e *engine) Query(q string) (*Result, error) {
	stmt, err := Check(q)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.timeout())
	defer cancel()

	started := time.Now()
	// A view missing since boot costs one failed glob to retry, so it is
	// retried every time. A full re-registration re-sniffs every partition, so
	// that runs only when DuckDB reports drift — once, and then the query is
	// retried against the rebound views.
	e.register(ctx, false)
	rows, err := e.db.QueryContext(ctx, stmt)
	if err != nil && strings.Contains(err.Error(), viewDrift) {
		e.register(ctx, true)
		rows, err = e.db.QueryContext(ctx, stmt)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, _ := rows.ColumnTypes()
	res := &Result{Columns: cols, Views: e.Views()}
	for _, t := range types {
		res.Types = append(res.Types, t.DatabaseTypeName())
	}

	limit := e.opts.rowLimit()
	for rows.Next() {
		if len(res.Rows) >= limit {
			res.Truncated = true
			break
		}
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, c := range cells {
			// []byte is how the driver hands back BLOB and some decimal shapes.
			// JSON-encoding it as base64 would put an unreadable string in an
			// ops console, so it becomes text — which is what the operator was
			// looking at in the NDJSON anyway.
			if b, ok := c.([]byte); ok {
				cells[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, cells)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.RowCount = len(res.Rows)
	res.ElapsedMs = time.Since(started).Milliseconds()
	return res, nil
}
