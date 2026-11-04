//go:build duckdb

package sqlengine

import (
	"database/sql"
	"fmt"
	"strings"
)

// shadowSettings are applied after the catalogue is created and before the
// statement is bound. External access off refuses every file and network
// function; with extensions neither installed nor loaded on demand, and the
// configuration then locked, no statement can turn either back on.
var shadowSettings = []string{
	"SET threads=1",
	"SET memory_limit='256MB'",
	"SET temp_directory=''",
	"SET autoinstall_known_extensions=false",
	"SET autoload_known_extensions=false",
	"SET enable_external_access=false",
	"SET lock_configuration=true",
}

// explainShadow binds stmt in a fresh in-memory database holding the empty
// tables ddl creates (shadow.go). It answers ExplainOK or the error verdict;
// an error is a catalogue that could not be built.
func explainShadow(ddl []string, stmt string) (string, error) {
	db, err := openShadow(ddl)
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query("EXPLAIN " + stmt)
	if err != nil {
		return errorVerdict(err.Error()), nil
	}
	// EXPLAIN's rows are the plan, which names only the statement's own
	// tables and columns; they are drained, never returned.
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return errorVerdict(err.Error()), nil
	}
	return ExplainOK, nil
}

// openShadow is the shadow catalogue: the tables ddl creates, then the
// settings that close it off.
func openShadow(ddl []string) (*sql.DB, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, s := range append(append([]string(nil), ddl...), shadowSettings...) {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlengine: building the shadow catalogue: %s: %w", firstWords(s), err)
		}
	}
	return db, nil
}

func firstWords(s string) string {
	if f := strings.Fields(s); len(f) > 3 {
		return strings.Join(f[:3], " ") + " …"
	}
	return s
}
