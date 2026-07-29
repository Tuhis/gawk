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
	"path/filepath"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
)

type engine struct {
	db    *sql.DB
	opts  Options
	views []ViewDoc
}

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

	e := &engine{db: db, opts: opts}
	globs := map[string]string{
		"sessions":    filepath.Join(opts.Root, "sessions", "date=*", "broadcast=*", "*.ndjson*"),
		"rollups":     filepath.Join(opts.Root, "rollups", "*.ndjson"),
		"relay":       filepath.Join(opts.Root, "relay", "date=*", "*.ndjson*"),
		"annotations": filepath.Join(opts.Root, "annotations", "annotations.ndjson"),
	}
	ok := map[string]bool{}
	for name, glob := range globs {
		hive := 1
		if name == "rollups" || name == "annotations" {
			hive = 0
		}
		stmt := fmt.Sprintf(
			`CREATE VIEW %s AS SELECT * FROM read_json_auto('%s', hive_partitioning=%d, union_by_name=1, ignore_errors=true)`,
			name, glob, hive)
		// A view whose tree is empty simply fails to register, and that is not
		// an error worth refusing to start over: a fleet with no relay
		// configured has no relay/ partitions at all, and the console should
		// still answer questions about sessions. The failure is REPORTED via
		// ViewDoc.Available rather than swallowed.
		if _, err := db.Exec(stmt); err == nil {
			ok[name] = true
		}
	}
	e.views = viewDocs(func(n string) bool { return ok[n] })
	return e, nil
}

// Compiled reports that this build carries an engine.
func Compiled() bool { return true }

// Views is the catalogue, for parity with the stub build.
func Views() []ViewDoc { return viewDocs(func(string) bool { return true }) }

func (e *engine) Views() []ViewDoc { return e.views }

func (e *engine) Close() error { return e.db.Close() }

func (e *engine) Query(q string) (*Result, error) {
	stmt, err := Check(q)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.timeout())
	defer cancel()

	started := time.Now()
	rows, err := e.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, _ := rows.ColumnTypes()
	res := &Result{Columns: cols, Views: e.views}
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
