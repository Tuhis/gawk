package store_test

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// D10 under a race (PR #500 review): a tripped POST that arrives while a
// dismissal is mid-transaction must come out muted, not raise a new flag
// that pages the operator who just dismissed. The sampler re-posts every few
// seconds while the operator clicks, so the two overlap in practice.
//
// The dismissal is held open by hand — the flag locked and resolved, the
// mute written, nothing committed — until the POST is waiting on the flag's
// row lock, then committed. A POST that read the mute before taking that
// lock saw none, found no unresolved flag once the lock was released, and
// raised; one that reads the mute after the lock sees the committed mute.
func TestATrippedPostRacingADismissalIsMuted(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	raised := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0)

	dismiss, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dismiss.Rollback(ctx) }()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT id FROM content_flags WHERE id = $1 FOR UPDATE`, []any{raised.Flag.ID}},
		{`UPDATE content_flags SET state = 'resolved', resolution = 'dismissed', resolved_at = $2, resolved_by = 'op@example.com'
			WHERE id = $1`, []any{raised.Flag.ID, t0.Add(time.Second)}},
		{`INSERT INTO screening_mutes (broadcast_id, muted_until, muted_by, flag_id) VALUES ('ABC234', $1, 'op@example.com', $2)`,
			[]any{t0.Add(30 * time.Minute), raised.Flag.ID}},
	} {
		if _, err := dismiss.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("hold the dismissal: %v", err)
		}
	}

	type result struct {
		res store.FlagPostResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := s.PostContentFlag(ctx, store.FlagPost{
			Body:  flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped),
			Actor: "gawk-sampler", Now: t0.Add(5 * time.Second),
		}, nil)
		done <- result{res, err}
	}()

	// Commit only once the POST is parked on the flag's row lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("the POST finished without waiting for the dismissal's lock: %+v, %v", r.res, r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the POST never waited on the dismissal's lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := dismiss.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.res.Status != screening.FlagStatusMuted {
		t.Fatalf("a tripped POST racing a dismissal: %q (flag %d), want muted", r.res.Status, r.res.Flag.ID)
	}
	unresolved, err := s.UnresolvedContentFlags(ctx, screening.ModeFlag)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved flags after the dismissal: %+v", unresolved)
	}
}
