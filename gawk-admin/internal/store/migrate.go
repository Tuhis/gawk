package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgx5 "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx/v5", required by pgx5.WithInstance

	"github.com/Tuhis/gawk/gawk-admin/migrations"
)

// MinSchemaVersion is the oldest schema version this build can serve against.
//
// Raising it is a deliberate act with a cost: a pod whose database is older
// refuses to serve (readyz false) rather than running half-broken queries.
// Under the expand-contract policy (§4.15) it should lag the newest migration
// by at least one release, so that the previous app version keeps working
// against a freshly-migrated database and rollback stays "redeploy the
// previous version".
const MinSchemaVersion uint = 1

// migrationsTable is golang-migrate's bookkeeping table. Named explicitly
// rather than left to the library default so that renaming it later is a
// visible change here, not a silent behavioural one.
const migrationsTable = "schema_migrations"

// Migrate applies every pending migration and returns.
//
// This is the ONLY code path in gawk-admin that runs DDL, and it is reachable
// only from the `migrate` subcommand — the Helm pre-install/pre-upgrade hook
// Job, or an operator's break-glass invocation (§4.15/D18). The serving
// process calls Store.Ready instead, which reads the version and never writes.
//
// Concurrency: golang-migrate's pgx/v5 driver takes a pg_advisory_lock for the
// duration, and reads the current version only after acquiring it. Two Jobs
// racing (a retried hook, two clusters pointed at one database) therefore
// serialize: one applies, the other finds nothing to do. Both return nil.
func Migrate(ctx context.Context, dsn string) error {
	m, db, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	// Cancelling the context aborts an in-flight migration rather than leaving
	// the Job hanging on the advisory lock forever.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// The inner select is the leak guard: if Up already returned,
			// nothing is listening on GracefulStop and a bare send would
			// block for the life of the process.
			select {
			case m.GracefulStop <- true:
			case <-done:
			}
		case <-done:
		}
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// MigrateVersion reports the applied schema version without a live Store —
// what the `migrate` subcommand prints, and what a break-glass operator asks
// for. ok is false when no migration has ever been applied.
func MigrateVersion(dsn string) (version uint, dirty bool, ok bool, err error) {
	m, db, err := newMigrator(dsn)
	if err != nil {
		return 0, false, false, err
	}
	defer db.Close()
	v, d, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("store: schema version: %w", err)
	}
	return v, d, true, nil
}

func newMigrator(dsn string) (*migrate.Migrate, *sql.DB, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("store: migration source: %w", err)
	}
	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("store: open for migration: %w", err)
	}
	drv, err := pgx5.WithInstance(db, &pgx5.Config{MigrationsTable: migrationsTable})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("store: migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", drv)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("store: migrator: %w", err)
	}
	return m, db, nil
}

// SchemaVersion reads golang-migrate's bookkeeping table through the serving
// pool. It is a plain SELECT: the serving process must be able to answer "what
// schema am I on?" without the ability to change it.
//
// ok is false when the table does not exist or holds no row — a database that
// has never been migrated. Ready treats that as version 0, i.e. too old.
func (s *Store) SchemaVersion(ctx context.Context) (version uint, dirty bool, err error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, migrationsTable).Scan(&exists); err != nil {
		return 0, false, fmt.Errorf("store: schema version: %w", err)
	}
	if !exists {
		return 0, false, nil
	}
	var v int64
	err = s.pool.QueryRow(ctx, `SELECT version, dirty FROM `+migrationsTable+` LIMIT 1`).Scan(&v, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		// The table exists but is empty: a database rolled all the way back,
		// or one whose first migration is mid-flight. Version 0.
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: schema version: %w", err)
	}
	if v < 0 {
		v = 0
	}
	return uint(v), dirty, nil
}
