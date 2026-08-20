// Package storetest gives every package that needs a real Postgres a fresh,
// isolated database and a clear skip when none is configured.
//
// Two properties matter and neither is negotiable:
//
//   - **`go test ./...` stays green on a machine without Postgres.** The
//     database tests skip with a message naming the environment variable,
//     rather than failing and training everyone to ignore red.
//   - **Every test gets its own database**, not a shared schema. The store's
//     invariants are database-level (a partial unique index, SKIP LOCKED
//     claims), so tests must be able to run in parallel and under -race
//     without seeing each other's rows — and the migration tests need a
//     `schema_migrations` table nobody else is advancing.
//
// It is a non-test package so the store, kube, api and (later) notify test
// suites can all use it; it has no non-test callers.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// EnvDSN names the environment variable holding a DSN to a throwaway Postgres.
// CI sets it (AP8 wires the service container); locally it points at the
// scratch container documented in the R39 working notes.
const EnvDSN = "GAWK_ADMIN_TEST_DSN"

// BaseDSN returns the configured DSN, skipping the test when it is unset.
func BaseDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("%s is not set: skipping the Postgres-backed test (set it to e.g. postgres://gawk:gawk@127.0.0.1:55432/gawkadmin?sslmode=disable)", EnvDSN)
	}
	return dsn
}

// FreshDSN creates an empty database and returns its DSN. The database is
// dropped when the test ends.
//
// EMPTY is the point for the migration tests: they must be able to assert that
// a serving process finds no schema and refuses to create one.
func FreshDSN(t testing.TB) string {
	t.Helper()
	base := BaseDSN(t)

	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random database suffix: %v", err)
	}
	name := "gawk_admin_test_" + hex.EncodeToString(buf[:])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvDSN, err)
	}
	defer admin.Close(context.Background())
	// CREATE DATABASE cannot be parameterized; the name is hex we just
	// generated, so there is nothing to inject.
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		// WITH (FORCE) so a pool that outlived its test cannot pin the
		// database and turn cleanup into a hang.
		_, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvDSN, err)
	}
	u.Path = "/" + name
	return u.String()
}

// New returns a migrated, open Store on a database of its own.
func New(t testing.TB) *store.Store {
	t.Helper()
	dsn := FreshDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}
