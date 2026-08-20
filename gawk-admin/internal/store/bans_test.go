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
	removed, err := s.RemoveBan(ctx, b.ID, "op2@example.com")
	if err != nil {
		t.Fatalf("RemoveBan: %v", err)
	}
	if removed.State != store.BanRemoved || removed.RemovedBy != "op2@example.com" || removed.RemovedAt == nil {
		t.Fatalf("removed ban = %+v", removed)
	}
	// Removing again is idempotent, not an error, and does not resurrect it.
	again, err := s.RemoveBan(ctx, b.ID, "op3")
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
	if _, err := s.RemoveBan(ctx, exp.ID, "op"); !errors.Is(err, store.ErrNotActive) {
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
	if _, err := s.RemoveBan(ctx, drop.ID, "op"); err != nil {
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
