package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

func newReconciler(t *testing.T, recs kube.Records, bans kube.BanClient, now func() time.Time,
	record func(context.Context, store.Event) (store.Event, error)) *kube.Reconciler {
	t.Helper()
	r, err := kube.NewReconciler(kube.ReconcilerOptions{
		Records: recs, Bans: bans, Now: now, Record: record,
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	return r
}

func crNames(t *testing.T, c kube.BanClient) []string {
	t.Helper()
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := make([]string, 0, len(list))
	for _, o := range list {
		out = append(out, o.Name)
	}
	return out
}

// Row → CR: an active row with no CR gets one. This is the crash-healing half
// of the reconciler (§4.1 step 2: "the 60 s reconcile loop heals any crash
// between the two").
func TestReconcileCreatesMissingCR(t *testing.T) {
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t)
	r := newReconciler(t, recs, crs, time.Now, nil)

	b := recs.add(store.Ban{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC234"},
		Reason: "spam", CreatedBy: "op@example.com"})

	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	list, err := crs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != b.CRName || list[0].BanID != b.ID.String() {
		t.Fatalf("CR after reconcile = %+v", list)
	}

	// A second pass must not rewrite an already-correct CR.
	counting := &countingBans{inner: crs}
	r2 := newReconciler(t, recs, counting, time.Now, nil)
	if err := r2.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second ReconcileOnce: %v", err)
	}
	if up, del := counting.counts(); up != 0 || del != 0 {
		t.Fatalf("a converged pass wrote %d upserts and %d deletes", up, del)
	}
}

// CR → row: an operator-applied CR (no ban-id annotation) is ADOPTED into
// Postgres, never deleted. `kubectl apply` with gawk-admin down is the
// documented break-glass path (§6) and this is what keeps it armed.
func TestReconcileAdoptsUnknownCRNeverDeletesIt(t *testing.T) {
	ban := &moderation.Ban{
		TypeMeta:   metav1.TypeMeta{APIVersion: moderation.SchemeGroupVersion.String(), Kind: moderation.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "ban-id-zzz234", Namespace: testNamespace},
		Spec: moderation.BanSpec{
			Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ZZZ234"},
			Reason: "emergency ban applied with kubectl",
		},
	}
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t, ban)
	var enqueued []store.Event
	r := newReconciler(t, recs, crs, time.Now, func(ctx context.Context, e store.Event) (store.Event, error) {
		saved, err := recs.AppendEvent(ctx, e)
		enqueued = append(enqueued, saved)
		return saved, err
	})

	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	rows := recs.snapshot()
	if len(rows) != 1 {
		t.Fatalf("adoption produced %d rows: %+v", len(rows), rows)
	}
	if rows[0].CreatedBy != kube.AdoptedBy {
		t.Fatalf("adopted row createdBy = %q, want %q", rows[0].CreatedBy, kube.AdoptedBy)
	}
	if rows[0].Target.Value != "ZZZ234" || rows[0].Reason != ban.Spec.Reason {
		t.Fatalf("adopted row = %+v", rows[0])
	}
	if names := crNames(t, crs); len(names) != 1 || names[0] != "ban-id-zzz234" {
		t.Fatalf("adoption disturbed the CR set: %v", names)
	}
	// The adopted CR is stamped so later passes recognize it as managed.
	list, _ := crs.List(context.Background())
	if list[0].BanID != rows[0].ID.String() {
		t.Fatalf("adopted CR was not stamped: %+v", list[0])
	}
	if got := recs.eventTypes(); len(got) != 1 || got[0] != store.EventBanCreated {
		t.Fatalf("adoption events = %v, want one %s", got, store.EventBanCreated)
	}
	if len(enqueued) != 1 {
		t.Fatalf("adoption enqueued %d webhook events, want 1", len(enqueued))
	}

	// A second pass must be a no-op: the target now has an active row.
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second ReconcileOnce: %v", err)
	}
	if rows := recs.snapshot(); len(rows) != 1 {
		t.Fatalf("re-adoption created %d rows", len(rows))
	}
}

// Expiry: the row flips, the CR is deleted, and exactly one ban.expired event
// is emitted and enqueued.
func TestReconcileExpiresBanAndDeletesCR(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	recs := newFakeRecords()
	recs.now = clock
	crs, _ := newFakeCRClient(t)
	var enqueued []store.Event
	r := newReconciler(t, recs, crs, clock, func(ctx context.Context, e store.Event) (store.Event, error) {
		saved, err := recs.AppendEvent(ctx, e)
		enqueued = append(enqueued, saved)
		return saved, err
	})

	expiry := now.Add(10 * time.Minute)
	b := recs.add(store.Ban{
		Target:            moderation.Target{Type: moderation.TargetBroadcastID, Value: "QQQ234"},
		ExpiresAt:         &expiry,
		CreatedBy:         "op@example.com",
		SourceBroadcastID: "QQQ234",
	})
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if names := crNames(t, crs); len(names) != 1 {
		t.Fatalf("CRs before expiry = %v", names)
	}

	// Past the cooldown.
	now = expiry.Add(time.Second)
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce (after expiry): %v", err)
	}

	rows := recs.snapshot()
	if len(rows) != 1 || rows[0].State != store.BanExpired {
		t.Fatalf("row after expiry = %+v", rows)
	}
	if names := crNames(t, crs); len(names) != 0 {
		t.Fatalf("expired ban's CR survived: %v", names)
	}
	types := recs.eventTypes()
	if len(types) != 1 || types[0] != store.EventBanExpired {
		t.Fatalf("events = %v, want one %s", types, store.EventBanExpired)
	}
	if len(enqueued) != 1 || enqueued[0].Type != store.EventBanExpired {
		t.Fatalf("enqueued = %+v", enqueued)
	}
	if enqueued[0].BroadcastID != "QQQ234" {
		t.Fatalf("expiry event lost the source broadcast: %+v", enqueued[0])
	}
	// The summary is webhook-safe: it must not name the banned broadcast.
	if s := enqueued[0].PayloadString(store.PayloadSummary); s == "" || contains(s, b.Target.Value) {
		t.Fatalf("summary %q must not carry the raw broadcast ID", s)
	}

	// A further pass must not re-emit.
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("third ReconcileOnce: %v", err)
	}
	if got := len(recs.eventTypes()); got != 1 {
		t.Fatalf("expiry emitted %d events across three passes, want 1", got)
	}
}

// The §6 rule, stated as a test: with Postgres unreachable the reconciler must
// not delete — or touch — a single CR. It cannot tell "this ban was lifted"
// from "I cannot see the record", and guessing wrong un-bans someone.
func TestReconcileNeverGCsWhilePostgresIsDown(t *testing.T) {
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t)
	counting := &countingBans{inner: crs}
	r := newReconciler(t, recs, counting, time.Now, nil)

	// Two CRs the reconciler would otherwise have opinions about: one it
	// manages whose row is gone, and one an operator applied.
	if err := crs.Upsert(context.Background(), idRecord("MMM234"), "orphan-ban-id"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := crs.Upsert(context.Background(), idRecord("NNN234"), ""); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	before := crNames(t, crs)

	recs.setDown(true)
	err := r.ReconcileOnce(context.Background())
	if !errors.Is(err, errRecordsDown) {
		t.Fatalf("ReconcileOnce with Postgres down = %v, want the store error", err)
	}
	if _, deletes := counting.counts(); deletes != 0 {
		t.Fatalf("the reconciler deleted %d CRs while Postgres was unreachable", deletes)
	}
	if after := crNames(t, crs); len(after) != len(before) {
		t.Fatalf("CR set changed while Postgres was down: %v -> %v", before, after)
	}

	// With Postgres back, the orphaned managed CR is collected and the
	// operator's is adopted.
	recs.setDown(false)
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce after recovery: %v", err)
	}
	names := crNames(t, crs)
	if len(names) != 1 || names[0] != "ban-id-nnn234" {
		t.Fatalf("after recovery CRs = %v, want only the adopted operator ban", names)
	}
}

// A Kubernetes API that cannot be listed must not cause row-side damage
// either: the pass reports the error and changes nothing.
func TestReconcileToleratesUnreachableKubernetes(t *testing.T) {
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t)
	counting := &countingBans{inner: crs, failList: true}
	r := newReconciler(t, recs, counting, time.Now, nil)

	recs.add(store.Ban{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "KKK234"}, CreatedBy: "op"})
	if err := r.ReconcileOnce(context.Background()); err == nil {
		t.Fatalf("ReconcileOnce with the k8s API down unexpectedly succeeded")
	}
	if up, del := counting.counts(); up != 0 || del != 0 {
		t.Fatalf("writes attempted against an unreachable API: %d upserts, %d deletes", up, del)
	}
}

// Project is the inline write the API performs on any replica: active bans
// upsert their CR, lifted bans delete it.
func TestProjectWritesAndDeletesInline(t *testing.T) {
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t)
	r := newReconciler(t, recs, crs, time.Now, nil)
	ctx := context.Background()

	b := recs.add(store.Ban{Target: moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7"}, CreatedBy: "op"})
	if err := r.Project(ctx, b); err != nil {
		t.Fatalf("Project(active): %v", err)
	}
	if names := crNames(t, crs); len(names) != 1 || names[0] != b.CRName {
		t.Fatalf("Project(active) produced %v, want %q", names, b.CRName)
	}

	b.State = store.BanRemoved
	if err := r.Project(ctx, b); err != nil {
		t.Fatalf("Project(removed): %v", err)
	}
	if names := crNames(t, crs); len(names) != 0 {
		t.Fatalf("Project(removed) left %v", names)
	}
}

// Two replicas projecting and reconciling the same ban concurrently must
// converge on ONE CR — deterministic names are the mechanism (§4.6).
func TestConcurrentReconcilersProduceNoDuplicateCRs(t *testing.T) {
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t)
	rA := newReconciler(t, recs, crs, time.Now, nil)
	rB := newReconciler(t, recs, crs, time.Now, nil)

	recs.add(store.Ban{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "PPP234"}, CreatedBy: "op"})

	done := make(chan error, 2)
	for _, r := range []*kube.Reconciler{rA, rB} {
		go func() { done <- r.ReconcileOnce(context.Background()) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent ReconcileOnce: %v", err)
		}
	}
	if names := crNames(t, crs); len(names) != 1 {
		t.Fatalf("two concurrent reconcilers produced %v", names)
	}
}

func contains(haystack, needle string) bool {
	return needle != "" && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// The break-glass CR an operator applied is stamped WHERE IT IS — under the
// name they chose, which nothing forces to be `ban-id-*`.
//
// Stamping through Upsert instead writes a canonical TWIN and leaves the
// operator's own object un-annotated, which is unreachable in both directions:
// a portal unban deletes only the canonical name, so every relay keeps
// enforcing 451, and the next sweep sees an un-annotated CR with no active row
// and re-adopts it — a fresh active row plus a fresh ban.created event and
// webhook, once a minute, while the portal says "unbanned".
func TestReconcileStampsTheOperatorsOwnCRNotATwin(t *testing.T) {
	ban := &moderation.Ban{
		TypeMeta:   metav1.TypeMeta{APIVersion: moderation.SchemeGroupVersion.String(), Kind: moderation.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "emergency-ban-x", Namespace: testNamespace},
		Spec: moderation.BanSpec{
			Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ZZZ234"},
			Reason: "applied with kubectl while gawk-admin was down",
		},
	}
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t, ban)
	var enqueued []store.Event
	r := newReconciler(t, recs, crs, time.Now, func(ctx context.Context, e store.Event) (store.Event, error) {
		saved, err := recs.AppendEvent(ctx, e)
		enqueued = append(enqueued, saved)
		return saved, err
	})
	ctx := context.Background()

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	rows := recs.snapshot()
	if len(rows) != 1 {
		t.Fatalf("adoption produced %d rows: %+v", len(rows), rows)
	}
	adopted := rows[0]

	list, err := crs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "emergency-ban-x" {
		t.Fatalf("adoption wrote a canonical twin instead of stamping the operator's object: %+v", list)
	}
	if list[0].BanID != adopted.ID.String() {
		t.Fatalf("CR %q banId = %q, want %q: an un-annotated object survives every unban and is "+
			"re-adopted by every sweep", list[0].Name, list[0].BanID, adopted.ID)
	}

	// A second pass must not adopt it again.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("second ReconcileOnce: %v", err)
	}
	if n := countType(enqueued, store.EventBanCreated); n != 1 {
		t.Fatalf("ban.created emitted %d times for one break-glass CR", n)
	}
	if rows := recs.snapshot(); len(rows) != 1 {
		t.Fatalf("a second sweep re-adopted the CR: %+v", rows)
	}

	// The unban: the row leaves `active`, the portal's inline Project deletes
	// the canonical name, and the sweep must clear whatever is left — the
	// operator's object included, because it is now annotated as ours.
	removed := recs.remove(adopted.ID)
	if err := r.Project(ctx, removed); err != nil {
		t.Fatalf("Project(removed): %v", err)
	}
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce after unban: %v", err)
	}
	if names := crNames(t, crs); len(names) != 0 {
		t.Fatalf("after an unban the fleet still enforces %v", names)
	}
	if n := countType(enqueued, store.EventBanCreated); n != 1 {
		t.Fatalf("the audit stream oscillated: ban.created emitted %d times", n)
	}
}

// The stamp is retried. A stamp that failed (or was skipped because another
// replica adopted the target first) leaves an un-annotated CR whose target now
// HAS an active row — the one case the adoption arm never reaches again,
// because convergence short-circuits on "still active". Left unstamped it is
// the same permanent orphan.
func TestReconcileStampsAnUnannotatedCRWhoseTargetIsAlreadyRecorded(t *testing.T) {
	ban := &moderation.Ban{
		TypeMeta:   metav1.TypeMeta{APIVersion: moderation.SchemeGroupVersion.String(), Kind: moderation.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "emergency-ban-y", Namespace: testNamespace},
		Spec: moderation.BanSpec{
			Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "YYY234"},
			Reason: "applied with kubectl",
		},
	}
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t, ban)
	r := newReconciler(t, recs, crs, time.Now, nil)
	ctx := context.Background()

	// The row already exists — the state a replica that lost the adoption race
	// (store.ErrDuplicateActive) leaves behind.
	row := recs.add(store.Ban{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "YYY234"},
		Reason: "applied with kubectl", CreatedBy: kube.AdoptedBy})

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	list, err := crs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, cr := range list {
		if cr.Name == "emergency-ban-y" && cr.BanID != row.ID.String() {
			t.Fatalf("CR %q was left un-annotated (banId=%q): no unban can ever reach it",
				cr.Name, cr.BanID)
		}
	}
}

func countType(evs []store.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}
