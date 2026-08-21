package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

func idTarget(v string) moderation.Target {
	return moderation.Target{Type: moderation.TargetBroadcastID, Value: v}
}

func ipTarget(v string) moderation.Target {
	return moderation.Target{Type: moderation.TargetIP, Value: v}
}

func mustCreate(t *testing.T, s *store.Store, b store.Ban) store.Ban {
	t.Helper()
	out, err := s.CreateBan(t.Context(), b)
	if err != nil {
		t.Fatalf("CreateBan(%v): %v", b.Target, err)
	}
	return out
}

// The one-active-ban-per-target rule is the database's, and it must surface as
// ErrDuplicateActive so the API can answer 409 with the ban that already
// exists (docs/42 §4.7).
func TestCreateBanRejectsSecondActiveTarget(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	first := mustCreate(t, s, store.Ban{Target: idTarget("abc234"), Reason: "spam", CreatedBy: "op@example.com"})
	if first.Target.Value != "ABC234" {
		t.Fatalf("target not normalized on write: %q", first.Target.Value)
	}
	if first.CRName != "ban-id-abc234" {
		t.Fatalf("cr name = %q, want the deterministic moderation.CRName", first.CRName)
	}

	// Same target, differently spelled: normalization must make it collide.
	_, err := s.CreateBan(ctx, store.Ban{Target: idTarget("ABC234"), CreatedBy: "op"})
	if !errors.Is(err, store.ErrDuplicateActive) {
		t.Fatalf("second active ban on the same target = %v, want ErrDuplicateActive", err)
	}

	// And the API can find the ban to report.
	existing, err := s.ActiveBanForTarget(ctx, idTarget("abc234"))
	if err != nil {
		t.Fatalf("ActiveBanForTarget: %v", err)
	}
	if existing.ID != first.ID {
		t.Fatalf("ActiveBanForTarget returned %v, want %v", existing.ID, first.ID)
	}

	// A bare address and its /32 are the same IP target, too.
	mustCreate(t, s, store.Ban{Target: ipTarget("203.0.113.7"), CreatedBy: "op"})
	_, err = s.CreateBan(ctx, store.Ban{Target: ipTarget("203.0.113.7/32"), CreatedBy: "op"})
	if !errors.Is(err, store.ErrDuplicateActive) {
		t.Fatalf("203.0.113.7/32 after 203.0.113.7 = %v, want ErrDuplicateActive", err)
	}
}

// Once a ban leaves `active` it never returns, and only `active` rows may be
// removed. That is the whole state machine.
func TestBanStateTransitionsAreOneWay(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	// active → removed
	b := mustCreate(t, s, store.Ban{Target: idTarget("aaaaaa"), CreatedBy: "op"})
	removed, _, err := s.RemoveBan(ctx, b.ID, "op2@example.com")
	if err != nil {
		t.Fatalf("RemoveBan: %v", err)
	}
	if removed.State != store.BanRemoved || removed.RemovedBy != "op2@example.com" || removed.RemovedAt == nil {
		t.Fatalf("removed ban = %+v", removed)
	}
	// Removing again is idempotent, not an error, and does not resurrect it.
	again, _, err := s.RemoveBan(ctx, b.ID, "op3")
	if err != nil {
		t.Fatalf("second RemoveBan: %v", err)
	}
	if again.State != store.BanRemoved || again.RemovedBy != "op2@example.com" {
		t.Fatalf("second RemoveBan changed the row: %+v", again)
	}
	// The target is free again: a removed ban does not hold the unique index.
	mustCreate(t, s, store.Ban{Target: idTarget("aaaaaa"), CreatedBy: "op"})

	// active → expired
	past := time.Now().Add(-time.Minute)
	exp := mustCreate(t, s, store.Ban{Target: idTarget("bbbbbb"), CreatedBy: "op", ExpiresAt: &past})
	moved, err := s.ExpireDueBans(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpireDueBans: %v", err)
	}
	if len(moved) != 1 || moved[0].ID != exp.ID || moved[0].State != store.BanExpired {
		t.Fatalf("ExpireDueBans returned %+v", moved)
	}
	// Re-running the sweep must not re-report it: nothing double-emits
	// ban.expired during a leadership handover.
	moved, err = s.ExpireDueBans(ctx, time.Now())
	if err != nil || len(moved) != 0 {
		t.Fatalf("second ExpireDueBans returned %d rows, err=%v", len(moved), err)
	}
	// expired → removed is not a transition the state machine has.
	if _, _, err := s.RemoveBan(ctx, exp.ID, "op"); !errors.Is(err, store.ErrNotActive) {
		t.Fatalf("RemoveBan on an expired ban = %v, want ErrNotActive", err)
	}
	got, err := s.GetBan(ctx, exp.ID)
	if err != nil || got.State != store.BanExpired {
		t.Fatalf("expired ban is now %v (err=%v)", got.State, err)
	}
}

func TestExpireDueBansLeavesPermanentAndFutureBans(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	future := time.Now().Add(time.Hour)
	mustCreate(t, s, store.Ban{Target: idTarget("cccccc"), CreatedBy: "op"})                     // permanent
	mustCreate(t, s, store.Ban{Target: idTarget("dddddd"), CreatedBy: "op", ExpiresAt: &future}) // not yet due

	moved, err := s.ExpireDueBans(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpireDueBans: %v", err)
	}
	if len(moved) != 0 {
		t.Fatalf("ExpireDueBans swept %d rows that were not due", len(moved))
	}
	active, err := s.ListBans(ctx, store.FilterActive)
	if err != nil || len(active) != 2 {
		t.Fatalf("active bans = %d (err=%v)", len(active), err)
	}
}

func TestListBansFilters(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	keep := mustCreate(t, s, store.Ban{Target: idTarget("eeeeee"), CreatedBy: "op"})
	drop := mustCreate(t, s, store.Ban{Target: idTarget("ffffff"), CreatedBy: "op"})
	if _, _, err := s.RemoveBan(ctx, drop.ID, "op"); err != nil {
		t.Fatalf("RemoveBan: %v", err)
	}

	active, err := s.ListBans(ctx, store.FilterActive)
	if err != nil {
		t.Fatalf("ListBans(active): %v", err)
	}
	if len(active) != 1 || active[0].ID != keep.ID {
		t.Fatalf("ListBans(active) = %+v", active)
	}
	all, err := s.ListBans(ctx, store.FilterAll)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListBans(all) = %d rows (err=%v)", len(all), err)
	}
	// An unrecognized filter must not silently widen to "all".
	if _, err := s.ListBans(ctx, "activee"); err == nil {
		t.Fatalf("ListBans with an unknown filter unexpectedly succeeded")
	}
}

func TestGetBanNotFound(t *testing.T) {
	s := storetest.New(t)
	if _, err := s.GetBan(t.Context(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBan(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := s.ActiveBanForTarget(t.Context(), idTarget("zzzzzz")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ActiveBanForTarget(unknown) = %v, want ErrNotFound", err)
	}
}

func TestCreateBanRejectsUnnormalizableTarget(t *testing.T) {
	s := storetest.New(t)
	_, err := s.CreateBan(t.Context(), store.Ban{Target: ipTarget("not-an-address"), CreatedBy: "op"})
	if !errors.Is(err, moderation.ErrInvalidTarget) {
		t.Fatalf("CreateBan with a malformed CIDR = %v, want ErrInvalidTarget", err)
	}
}

// A ban whose expiry has passed is not active, whatever the janitor's sweep
// clock says. Relays evaluate expiresAt against their own clocks (§4.2), so a
// kill's cooldown lapsing means the broadcast is live and unenforced — while
// this lookup is the 409 gate on re-killing it. Answering "already banned"
// there is up to a minute of a live abusive broadcast in the happy case, and
// unbounded whenever no replica holds the leader Lease.
func TestActiveBanForTargetIgnoresALapsedRow(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	past := time.Now().Add(-time.Minute).UTC()
	first := mustCreate(t, s, store.Ban{Target: idTarget("abc234"), Reason: "kill cooldown",
		CreatedBy: "op@example.com", ExpiresAt: &past})

	if _, err := s.ActiveBanForTarget(ctx, idTarget("abc234")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ActiveBanForTarget on a lapsed ban = %v, want ErrNotFound", err)
	}
	// The row itself has not moved yet: only the janitor writes `expired`.
	if got, err := s.GetBan(ctx, first.ID); err != nil || got.State != store.BanActive {
		t.Fatalf("row state = %v (err %v), want it untouched until the sweep", got.State, err)
	}
	// A permanent ban on the same shape of target is unaffected.
	perm := mustCreate(t, s, store.Ban{Target: idTarget("bbb234"), CreatedBy: "op"})
	got, err := s.ActiveBanForTarget(ctx, idTarget("bbb234"))
	if err != nil || got.ID != perm.ID {
		t.Fatalf("ActiveBanForTarget on a permanent ban = %v, %v", got.ID, err)
	}
}

// The partial unique index knows only `state = 'active'`, so the lapsed row
// still blocks the INSERT that replaces it. Clearing one target's lapsed rows
// inline is what lets a re-kill happen in the time of one API call rather than
// waiting for a sweep that may have no leader to run it.
func TestExpireLapsedBansForTargetClearsTheWayForAReBan(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	past := time.Now().Add(-time.Minute).UTC()
	future := time.Now().Add(time.Hour).UTC()
	lapsed := mustCreate(t, s, store.Ban{Target: idTarget("abc234"), CreatedBy: "op", ExpiresAt: &past})
	live := mustCreate(t, s, store.Ban{Target: idTarget("ccc234"), CreatedBy: "op", ExpiresAt: &future})

	if _, err := s.CreateBan(ctx, store.Ban{Target: idTarget("abc234"), CreatedBy: "op"}); !errors.Is(err, store.ErrDuplicateActive) {
		t.Fatalf("precondition: a lapsed row must still collide on the index, got %v", err)
	}

	moved, err := s.ExpireLapsedBansForTarget(ctx, idTarget("ABC234"), time.Now())
	if err != nil {
		t.Fatalf("ExpireLapsedBansForTarget: %v", err)
	}
	if len(moved) != 1 || moved[0].ID != lapsed.ID || moved[0].State != store.BanExpired {
		t.Fatalf("moved = %+v, want just the lapsed row as expired", moved)
	}
	// Only this target, and only the lapsed row: an unexpired ban elsewhere
	// must not be swept up by a mutation on a different target.
	if got, err := s.GetBan(ctx, live.ID); err != nil || got.State != store.BanActive {
		t.Fatalf("an unrelated live ban was expired: %+v (err %v)", got, err)
	}

	replacement := mustCreate(t, s, store.Ban{Target: idTarget("abc234"), Reason: "again", CreatedBy: "op"})
	if replacement.ID == lapsed.ID {
		t.Fatal("the replacement must be a new row: the state machine is one-way")
	}

	// A second call has nothing to move — which is what makes it safe for two
	// replicas to race it without double-emitting ban.expired.
	again, err := s.ExpireLapsedBansForTarget(ctx, idTarget("abc234"), time.Now())
	if err != nil || len(again) != 0 {
		t.Fatalf("second call moved %+v (err %v), want nothing", again, err)
	}
}

// RemoveBan must SAY whether it moved the row. The unban is idempotent on
// purpose, but its caller emits an audit event and a signed webhook delivery
// per call — so without this the second click writes a second ban.removed and
// double-pages every receiver with a distinct delivery ID, which receiver-side
// dedup cannot catch.
func TestRemoveBanReportsWhetherItTransitioned(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	b := mustCreate(t, s, store.Ban{Target: idTarget("ddd234"), CreatedBy: "op"})

	removed, changed, err := s.RemoveBan(ctx, b.ID, "op2@example.com")
	if err != nil || !changed {
		t.Fatalf("first RemoveBan: changed=%v err=%v", changed, err)
	}
	if removed.State != store.BanRemoved || removed.RemovedBy != "op2@example.com" {
		t.Fatalf("removed = %+v", removed)
	}

	again, changed, err := s.RemoveBan(ctx, b.ID, "op3@example.com")
	if err != nil {
		t.Fatalf("second RemoveBan: %v", err)
	}
	if changed {
		t.Fatal("the second RemoveBan reported a transition it did not make")
	}
	if again.RemovedBy != "op2@example.com" || !again.RemovedAt.Equal(*removed.RemovedAt) {
		t.Fatalf("the replay rewrote the removal: %+v", again)
	}
}
