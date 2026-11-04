package sqlengine

// The learned schema's on-disk shape (learn.go), outside the build tag: the
// shadow catalogue (shadow.go) reads it in the service, which never opens
// DuckDB itself.

import "path/filepath"

// learnedVersion is the on-disk format. A file in any other format, or
// learned by another DuckDB, is relearned from scratch: a DuckDB upgrade can
// sniff the same file differently.
const learnedVersion = 1

// learned is one view's schema and the files it covers.
type learned struct {
	Version int    `json:"version"`
	DuckDB  string `json:"duckdb"`
	// Columns is the row type as a STRUCT, unknowns written NULL.
	Columns string `json:"columns"`
	// Files maps each covered file, relative to Root, to [size, mtime ns].
	Files map[string][2]int64 `json:"files"`

	cols *colType
}

// SchemaDir is where the learned schemas live: a dot-directory at the data
// root, like SpillDir, so no view glob matches it.
func SchemaDir(root string) string { return filepath.Join(root, ".sql-schema") }
