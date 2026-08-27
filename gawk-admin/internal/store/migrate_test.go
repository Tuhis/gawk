package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
)

// Migrating twice must be a no-op the second time — the Helm hook Job runs on
// every upgrade, most of which change nothing.
func TestMigrateIsIdempotent(t *testing.T) {
	dsn := storetest.FreshDSN(t)
	ctx := t.Context()

	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	v1, dirty, ok, err := store.MigrateVersion(dsn)
	if err != nil || !ok || dirty {
		t.Fatalf("version after first migrate: v=%d dirty=%v ok=%v err=%v", v1, dirty, ok, err)
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	v2, dirty, ok, err := store.MigrateVersion(dsn)
	if err != nil || !ok || dirty {
		t.Fatalf("version after second migrate: v=%d dirty=%v ok=%v err=%v", v2, dirty, ok, err)
	}
	if v1 != v2 {
		t.Fatalf("version moved on a no-op migrate: %d -> %d", v1, v2)
	}
	if v1 < store.MinSchemaVersion {
		t.Fatalf("migrated schema %d is below this build's minimum %d", v1, store.MinSchemaVersion)
	}
}

// Two migrate steps racing — a retried Helm hook, two clusters pointed at one
// database — must serialize on the advisory lock: both succeed, the schema is
// applied exactly once.
func TestMigrateParallelRunsAreSerialized(t *testing.T) {
	dsn := storetest.FreshDSN(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = store.Migrate(context.Background(), dsn)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("parallel migrate %d: %v", i, err)
		}
	}
	v, dirty, ok, err := store.MigrateVersion(dsn)
	if err != nil || !ok {
		t.Fatalf("version after parallel migrate: ok=%v err=%v", ok, err)
	}
	if dirty {
		t.Fatalf("schema left dirty by concurrent migrations")
	}
	if v != store.MinSchemaVersion {
		t.Fatalf("version = %d, want %d", v, store.MinSchemaVersion)
	}

	// "Applied once" is only meaningful if the objects exist exactly once, so
	// assert the shape rather than just the bookkeeping row.
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN ('bans','moderation_events','webhooks','webhook_deliveries')`).Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected the four schema tables, found %d", n)
	}
}

// The serving process must never run DDL (docs/42 §4.15/D18). Opening a store
// against a database that has never been migrated must leave it empty and
// refuse readiness — not "helpfully" create the schema.
func TestServingPathNeverRunsDDL(t *testing.T) {
	dsn := storetest.FreshDSN(t)
	ctx := t.Context()

	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	err = s.Ready(ctx)
	if !errors.Is(err, store.ErrSchemaTooOld) {
		t.Fatalf("Ready on an unmigrated database = %v, want ErrSchemaTooOld", err)
	}
	// Exercise a read path too: a query against a missing table must fail,
	// never create anything.
	if _, listErr := s.ListBans(ctx, store.FilterActive); listErr == nil {
		t.Fatalf("ListBans against an unmigrated database unexpectedly succeeded")
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if n != 0 {
		t.Fatalf("the serving path created %d table(s) in an empty database — it must never run DDL", n)
	}
}

// A schema older than the binary's minimum: refuse to serve, and do not move
// the schema version while refusing.
func TestReadyRefusesTooOldSchemaWithoutMigrating(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	if err := s.Ready(ctx); err != nil {
		t.Fatalf("Ready on a freshly migrated database: %v", err)
	}
	before, _, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}

	// Stand in for a future build whose minimum has moved past this database.
	s.MinVersion = before + 5
	err = s.Ready(ctx)
	if !errors.Is(err, store.ErrSchemaTooOld) {
		t.Fatalf("Ready with MinVersion above the schema = %v, want ErrSchemaTooOld", err)
	}
	after, _, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version after refusal: %v", err)
	}
	if after != before {
		t.Fatalf("schema version moved while refusing to serve: %d -> %d", before, after)
	}
}
