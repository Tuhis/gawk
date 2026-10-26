//go:build duckdb

package sqlengine

// The cgo build (§8 Q1's resolution). Compiled only with `-tags duckdb`, which
// is what the deployed image uses; a fresh clone gets nodriver.go instead.
//
// The views registered here are the D11 recipe made permanent. `read_json`
// with `hive_partitioning=1` prunes by path rather than scanning, which is the
// whole reason the store's layout is `date=…/broadcast=…` — so an operator's
// `WHERE date = '2026-07-29'` costs one directory instead of the tree.
//
// The columns are named, not sniffed: stats objects grow every milestone (D15's
// "version skew is permanent"), so two partitions genuinely have different
// columns, and the list is the union of every file's, learned a file at a time
// (learn.go). Sniffing it instead — `union_by_name=1` — re-read every file in
// the tree on every query, which is what took the sessions view down.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
)

type engine struct {
	db   *sql.DB
	opts Options

	// turn is the engine's one-at-a-time slot: held for a WHOLE Query —
	// re-registration, the query, reading its rows — and for Open's
	// registration. The single connection already serialised queries; the
	// slot makes that order explicit, and acquiring it honours the query's
	// context, so a query waiting behind another still times out.
	turn chan struct{}

	// mu guards views and nothing else, and is NEVER held across a database
	// call. It used to be (register held it through ExecContext) while Query
	// took it with its rows — the connection — still open: two overlapping
	// queries each held what the other needed until the first one timed out
	// (PR #350 review).
	mu    sync.Mutex
	views []ViewDoc

	// only, when non-nil, is the set of views this engine registers; the rest
	// stay unregistered and are reported unavailable. A query worker registers
	// just the views its statement names (openScoped): registering one stats
	// every file in its tree, and sniffs any it has not seen.
	only map[string]bool

	// regErr is why each unavailable view is unavailable, when something
	// failed rather than the tree being empty. Guarded by mu, like views.
	regErr map[string]error

	// learned is each view's schema as this engine last saw it (learn.go),
	// and duckdb the library version that sniffed it. Touched only with the
	// turn held.
	learned map[string]*learned
	duckdb  string

	// until, if set, ends the learning budget no later than this — a
	// worker's parent kills it at its own deadline, which the worker's
	// per-statement clocks do not know about.
	until time.Time
}

// testHookRowsOpen runs inside Query while its rows (and so the engine's one
// connection) are held. Nil in production; the concurrency test uses it to
// start a second query at exactly that point.
var testHookRowsOpen func()

// Open builds an in-memory DuckDB with views over the store's partitions.
func Open(opts Options) (Engine, error) { return openScoped(opts, nil, time.Time{}) }

// openScoped is Open registering only the named views; nil means all of them.
// until, if set, is when the caller stops waiting for this engine at all.
func openScoped(opts Options, only map[string]bool, until time.Time) (Engine, error) {
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

	e := &engine{
		db: db, opts: opts,
		turn:    make(chan struct{}, 1),
		views:   viewDocs(func(string) bool { return false }),
		only:    only,
		regErr:  map[string]error{},
		learned: map[string]*learned{},
		until:   until,
	}
	// The learned schemas are keyed by the library that sniffed them.
	if err := db.QueryRow("SELECT library_version FROM pragma_version()").Scan(&e.duckdb); err != nil {
		db.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()
	if err := e.acquire(ctx); err != nil {
		db.Close()
		return nil, err
	}
	defer e.release()
	e.register(ctx)
	return e, nil
}

func (e *engine) acquire(ctx context.Context) error {
	select {
	case e.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *engine) release() { <-e.turn }

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

// register (re)creates the views, each over its learned column list.
//
// Registration is NOT a one-off at startup, and must not go back to being one.
// DuckDB binds a view's column names and types when it is created, and these
// views sit over globs that keep growing for the life of the pod: the first
// partition that added a field (every milestone does, D15) or widened a type
// used to make every query on that view fail until the pod restarted, and a
// tree that was empty at boot (annotations before the first note, relay before
// the first scrape) stayed unqueryable. So Query registers every time; with
// the columns learned, that is a stat per file and a sniff of only what is new.
//
// The caller holds the turn. e.mu is taken only to read and to publish the
// catalogue, never across ExecContext.
func (e *engine) register(ctx context.Context) {
	ok := map[string]bool{}
	for _, v := range e.Views() {
		ok[v.Name] = v.Available
	}
	// errs starts as the last verdicts, so a registration cut short keeps
	// the reasons of the views it did not reach.
	e.mu.Lock()
	errs := make(map[string]error, len(e.regErr))
	for n, err := range e.regErr {
		errs[n] = err
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.views = viewDocs(func(n string) bool { return ok[n] })
		e.regErr = errs
		e.mu.Unlock()
	}()
	for name, src := range viewSources(e.opts.Root) {
		if e.only != nil && !e.only[name] {
			continue
		}
		delete(errs, name)
		// A tree with no files cannot register, so there is nothing to try:
		// without this, an empty relay/ cost a failing DDL on every query.
		matches, _ := filepath.Glob(src.glob)
		if len(matches) == 0 {
			if ok[name] {
				_, _ = e.db.ExecContext(ctx, "DROP VIEW IF EXISTS "+name)
			}
			ok[name] = false
			continue
		}
		hive := 0
		if src.hive {
			hive = 1
		}
		// records='true' with the columns: without it, a one-column list
		// leaves the record type to auto-detection, which sniffs again.
		cols, err := e.columns(ctx, name, matches)
		if err == nil {
			_, err = e.db.ExecContext(ctx, fmt.Sprintf(
				`CREATE OR REPLACE VIEW %s AS SELECT * FROM read_json(%s, columns=%s, format='newline_delimited', records='true', hive_partitioning=%d, ignore_errors=true)`,
				name, sqlString(src.glob), cols, hive))
		}
		// A view that fails to register is not an error worth refusing to
		// start over: the console should still answer questions about the
		// others. The failure is REPORTED via ViewDoc.Available, and its
		// reason through Query, rather than swallowed. A failed
		// re-registration drops the old view, so Available and the catalogue
		// never disagree — and a view still learning its columns is never
		// left registered over a list that misses files.
		switch {
		case err == nil:
			ok[name] = true
		case ctx.Err() != nil:
			// The clock ran out, not the view: say nothing about this or any
			// remaining view, rather than marking healthy views down (and
			// attempting a DROP on a dead context) because a query was slow.
			if err := e.regErrOf(name); err != nil {
				errs[name] = err
			}
			return
		default:
			ok[name] = false
			errs[name] = err
			_, _ = e.db.ExecContext(ctx, "DROP VIEW IF EXISTS "+name)
		}
	}
}

// Compiled reports that this build carries an engine.
func Compiled() bool { return true }

// Views is the catalogue, for parity with the stub build.
func Views() []ViewDoc { return viewDocs(func(string) bool { return true }) }

// regErrOf is why view name failed to register, or nil.
func (e *engine) regErrOf(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.regErr[name]
}

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
	if err := e.acquire(ctx); err != nil {
		return nil, err
	}
	defer e.release()
	// Every query re-registers, so a field or a tree that appeared since the
	// last one is in the views it reads. The catalogue is snapshotted after
	// registration and before the rows (the connection) are held.
	e.register(ctx)
	views := e.Views()
	rows, err := e.db.QueryContext(ctx, stmt)
	if err != nil {
		// A statement over a view that failed to register fails as "table
		// does not exist"; the reason it is missing is the useful answer.
		for _, v := range referencedViews(stmt) {
			if rerr := e.regErrOf(v); rerr != nil {
				return nil, rerr
			}
		}
		return nil, err
	}
	defer rows.Close()
	if testHookRowsOpen != nil {
		testHookRowsOpen()
	}

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, _ := rows.ColumnTypes()
	res := &Result{Columns: cols, Views: views}
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
