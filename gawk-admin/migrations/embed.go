// Package migrations carries gawk-admin's forward-only SQL schema history
// (R39, docs/42 §4.15) as an embedded filesystem.
//
// The files are embedded rather than read from disk so that the schema a
// release needs travels inside that release's own image: the Helm
// pre-install/pre-upgrade hook Job runs `gawk-admin migrate` from the same
// image the Deployment is about to roll, and there is no second artifact to
// keep in step (D18).
//
// The embed directive lives HERE, next to the .sql files, because a
// //go:embed path may not escape its own package directory — putting it in
// internal/store would have forced the migrations under internal/, away from
// the documented `gawk-admin/migrations/` location that the migration-lint CI
// gate (AP8) and every reviewer look at.
package migrations

import "embed"

// FS holds every migration file. golang-migrate's iofs source reads it at the
// root, so `iofs.New(migrations.FS, ".")` is the whole wiring.
//
// The `.up.sql` suffix is golang-migrate's file convention. It does NOT imply
// a `.down.sql` exists: down migrations are deliberately never written
// (docs/42 §4.15) — rollback is redeploying the previous application version,
// which the expand-contract policy guarantees works.
//
//go:embed *.sql
var FS embed.FS
