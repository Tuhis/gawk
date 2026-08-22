package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// DefaultInterval is the reconcile/janitor period (docs/42 §4.6: "one loop,
// 60 s, plus immediate runs after every mutation").
//
// 60 s is a HEALING interval, not the enforcement path: a ban's CR is written
// inline by the mutation that created it, and relays evaluate `expiresAt`
// themselves, so this loop only has to close the window left by a crash
// between the row write and the CR write.
const DefaultInterval = time.Minute

// Records is the Postgres surface the reconciler needs. Narrow on purpose: it
// makes the "Postgres unreachable ⇒ no CR garbage collection" rule (§6)
// directly testable, which a concrete *store.Store dependency would not.
type Records interface {
	ExpireDueBans(ctx context.Context, now time.Time) ([]store.Ban, error)
	ListBans(ctx context.Context, state string) ([]store.Ban, error)
	CreateBan(ctx context.Context, b store.Ban) (store.Ban, error)
	AppendEvent(ctx context.Context, e store.Event) (store.Event, error)
}

// ReconcilerOptions configure a Reconciler.
type ReconcilerOptions struct {
	Records Records
	Bans    BanClient
	Log     *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Interval is the sweep period; 0 means DefaultInterval.
	Interval time.Duration
	// Record persists an event together with its webhook fan-out in one
	// transaction (AP7, the dispatcher's Record). nil means no notification
	// fan-out — events then land via Records.AppendEvent alone, in Postgres
	// and the portal feed, which docs/42 §4.10 explicitly allows ("zero
	// configured webhooks is fine").
	Record func(ctx context.Context, ev store.Event) (store.Event, error)
}

// Reconciler converges the `bans` table and the Ban CRs in both directions.
//
// Two of its behaviours are rules rather than implementation details:
//
//   - **It never garbage-collects a CR while Postgres is unreachable** (§6).
//     Without the record store it cannot tell "this ban was lifted" from "I
//     cannot see the record", and guessing wrong un-bans someone.
//   - **An unknown CR is ADOPTED, never deleted.** `kubectl apply` of a Ban is
//     the documented break-glass path for when gawk-admin is down (§6), and a
//     reconciler that tidied those away would silently disarm it.
type Reconciler struct {
	opts ReconcilerOptions
	log  *slog.Logger
	kick chan struct{}
}

// NewReconciler builds a Reconciler.
func NewReconciler(opts ReconcilerOptions) (*Reconciler, error) {
	if opts.Records == nil {
		return nil, fmt.Errorf("kube: ReconcilerOptions.Records is required")
	}
	if opts.Bans == nil {
		return nil, fmt.Errorf("kube: ReconcilerOptions.Bans is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	return &Reconciler{opts: opts, log: opts.Log, kick: make(chan struct{}, 1)}, nil
}

// Run sweeps until ctx ends. It is LEADER-ONLY work (D16): every replica
// serves API traffic, but only the leader expires bans and collects CRs, so
// two replicas cannot both emit `ban.expired` for one ban.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	r.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
		case <-r.kick:
			r.sweep(ctx)
		}
	}
}

// Kick asks for an immediate sweep. Non-blocking and coalescing: a burst of
// mutations costs one extra sweep, not one per mutation.
//
// It is best-effort by design. A mutation served by a NON-leader replica has
// already written its own CR inline (Project), so the sweep it cannot trigger
// is a healing pass, not the enforcement path.
func (r *Reconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *Reconciler) sweep(ctx context.Context) {
	if err := r.ReconcileOnce(ctx); err != nil {
		r.log.Warn("ban reconcile failed", "err", err)
	}
}

// Project writes one ban row's CR: upsert while it is active, delete once it
// is not.
//
// Called INLINE by the mutation that created or lifted the ban, from whichever
// replica served the request — deterministic CR names make that safe from any
// replica, and it is what makes a kill take effect in the time of one API call
// rather than by the next sweep.
func (r *Reconciler) Project(ctx context.Context, b store.Ban) error {
	if b.State == store.BanActive {
		return r.opts.Bans.Upsert(ctx, b.Record(), b.ID.String())
	}
	name := b.CRName
	if name == "" {
		var err error
		if name, err = moderation.CRName(b.Target); err != nil {
			return err
		}
	}
	return r.opts.Bans.Delete(ctx, name)
}

// ReconcileOnce runs one full convergence pass.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	now := r.opts.Now()

	// 1. Expiry first, so the CR pass below already sees the post-expiry
	//    truth and deletes the CRs of bans that just lapsed.
	expired, err := r.opts.Records.ExpireDueBans(ctx, now)
	if err != nil {
		return fmt.Errorf("expire due bans: %w", err)
	}
	for _, b := range expired {
		r.emitExpired(ctx, b)
	}

	// 2. The record store is the authority. If it cannot be read, the pass
	//    ends here: no CR is created, updated or deleted (§6). Enforcement
	//    continues from the CRs already in the cluster.
	active, err := r.opts.Records.ListBans(ctx, store.FilterActive)
	if err != nil {
		return fmt.Errorf("list active bans: %w", err)
	}
	crs, err := r.opts.Bans.List(ctx)
	if err != nil {
		return fmt.Errorf("list ban CRs: %w", err)
	}

	byName := make(map[string]BanObject, len(crs))
	for _, cr := range crs {
		byName[cr.Name] = cr
	}
	activeByTarget := make(map[moderation.Target]store.Ban, len(active))

	// 3. Every active row must have a CR that matches it.
	for _, b := range active {
		activeByTarget[b.Target] = b
		name := b.CRName
		if name == "" {
			if name, err = moderation.CRName(b.Target); err != nil {
				r.log.Warn("ban row has an unnamable target", "banId", b.ID, "err", err)
				continue
			}
		}
		cr, ok := byName[name]
		if ok && cr.Err == nil && cr.BanID == b.ID.String() && sameSpec(cr.Record, b) {
			continue
		}
		if err := r.opts.Bans.Upsert(ctx, b.Record(), b.ID.String()); err != nil {
			r.log.Warn("projecting ban to a CR failed", "banId", b.ID, "crName", name, "err", err)
			continue
		}
		r.log.Info("ban CR reconciled", "banId", b.ID, "crName", name, "targetType", b.Target.Type)
	}

	// 4. Every CR must correspond to an active row — by adoption when it is
	//    the operator's, by deletion only when it is ours.
	for _, cr := range crs {
		if cr.Err != nil {
			// Never act on an object we could not understand. Logged every
			// pass on purpose: it is a stuck object needing a human.
			r.log.Warn("ban CR could not be read; leaving it untouched", "crName", cr.Name, "err", cr.Err)
			continue
		}
		if row, stillActive := activeByTarget[cr.Record.Target]; stillActive {
			// Still enforced — but an un-annotated object here is one nothing
			// will ever clean up: the adoption arm below is unreachable while
			// the target has an active row, so a stamp that failed (or was
			// never attempted, because another replica won the adoption race
			// and returned on ErrDuplicateActive) would leave the CR orphaned
			// for good. Stamping is the retry.
			if cr.BanID == "" {
				if err := r.opts.Bans.Adopt(ctx, cr.Name, row.ID.String()); err != nil {
					r.log.Warn("stamping an unrecorded ban CR failed", "crName", cr.Name, "err", err)
					continue
				}
				r.log.Info("stamped a ban CR whose target was already recorded", "crName", cr.Name, "banId", row.ID)
			}
			continue
		}
		if cr.BanID == "" {
			r.adopt(ctx, cr)
			continue
		}
		if err := r.opts.Bans.Delete(ctx, cr.Name); err != nil {
			r.log.Warn("deleting a lapsed ban CR failed", "crName", cr.Name, "err", err)
			continue
		}
		r.log.Info("lapsed ban CR deleted", "crName", cr.Name, "targetType", cr.Record.Target.Type)
	}
	return nil
}

// adopt records an operator-applied CR in Postgres so the portal can see it,
// the audit trail explains it, and the janitor can eventually expire it.
func (r *Reconciler) adopt(ctx context.Context, cr BanObject) {
	rec := cr.Record
	b := store.Ban{
		Target:    rec.Target,
		Reason:    rec.Reason,
		ExpiresAt: rec.ExpiresAt,
		CreatedBy: AdoptedBy,
	}
	if rec.CreatedBy != "" && rec.CreatedBy != AdoptedBy {
		// Keep the operator's own attribution when the CR carried one; the
		// point of AdoptedBy is "this did not come through the portal", and a
		// hand-written createdBy says that just as well.
		b.CreatedBy = rec.CreatedBy
	}
	created, err := r.opts.Records.CreateBan(ctx, b)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateActive) {
			// Another replica adopted it first, or a differently-named CR for
			// the same target is already recorded. Nothing to do — and, in
			// particular, nothing to delete.
			return
		}
		r.log.Warn("adopting an operator-applied ban CR failed", "crName", cr.Name, "err", err)
		return
	}
	r.log.Info("adopted an operator-applied ban CR", "crName", cr.Name, "banId", created.ID,
		"targetType", created.Target.Type, "createdBy", created.CreatedBy)

	// Stamp the CR the operator actually applied, BY ITS OWN NAME. Nothing
	// forces a hand-written Ban to be called `ban-id-*`, and Upsert always
	// addresses moderation.CRName — so stamping through it would annotate a
	// canonical twin and leave `emergency-ban-x` un-annotated: invisible to
	// unban (which deletes only the canonical name) and re-adopted by every
	// later sweep, one ban.created event and webhook per minute, while the
	// portal reports the ban lifted.
	//
	// A failure here is retried by the convergence pass above on the next
	// sweep, which is why that pass stamps too.
	if err := r.opts.Bans.Adopt(ctx, cr.Name, created.ID.String()); err != nil {
		r.log.Warn("stamping an adopted ban CR failed", "crName", cr.Name, "err", err)
	}

	ev := store.Event{
		Type:        store.EventBanCreated,
		OccurredAt:  r.opts.Now(),
		Actor:       created.CreatedBy,
		BroadcastID: rawBroadcastID(created),
		Payload:     eventPayload(created, store.Summarize(store.EventBanCreated, created.Target.Type, "", created.CreatedBy)),
	}
	r.record(ctx, ev)
}

func (r *Reconciler) emitExpired(ctx context.Context, b store.Ban) {
	// The CR is deleted by the convergence pass below/next: expiry is
	// evaluated by the relays themselves against their own clocks (§4.2), so
	// enforcement has already ended whether or not the object is gone yet.
	ev := store.Event{
		Type:        store.EventBanExpired,
		OccurredAt:  r.opts.Now(),
		Actor:       "system",
		BroadcastID: rawBroadcastID(b),
		Payload:     eventPayload(b, store.Summarize(store.EventBanExpired, b.Target.Type, "", "")),
	}
	r.record(ctx, ev)
	r.log.Info("ban expired", "banId", b.ID, "targetType", b.Target.Type)
}

func (r *Reconciler) record(ctx context.Context, ev store.Event) {
	// One call, one transaction: the event and its fan-out commit together, so
	// a crash here cannot record an adoption or expiry whose page was never
	// queued (the AppendEvent → EnqueueDeliveries window).
	rec := r.opts.Record
	if rec == nil {
		rec = r.opts.Records.AppendEvent
	}
	if _, err := rec(ctx, ev); err != nil {
		r.log.Warn("recording a moderation event failed", "type", ev.Type, "err", err)
	}
}

// sameSpec reports whether a CR already says what the row says. Comparing
// before writing keeps the reconcile loop from rewriting every CR once a
// minute for the life of a permanent ban.
func sameSpec(rec moderation.Record, b store.Ban) bool {
	if rec.Target != b.Target || rec.Reason != b.Reason || rec.CreatedBy != b.CreatedBy {
		return false
	}
	switch {
	case rec.ExpiresAt == nil && b.ExpiresAt == nil:
		return true
	case rec.ExpiresAt == nil || b.ExpiresAt == nil:
		return false
	default:
		return rec.ExpiresAt.Equal(*b.ExpiresAt)
	}
}

// rawBroadcastID is the event's raw-ID column: the broadcast the action was
// taken against. Portal and Postgres only — AP7 never copies this field into a
// webhook (D8).
func rawBroadcastID(b store.Ban) string {
	if b.SourceBroadcastID != "" {
		return b.SourceBroadcastID
	}
	if b.Target.Type == moderation.TargetBroadcastID {
		return b.Target.Value
	}
	return ""
}

// eventPayload is the portal-visible context for a ban event. It may carry the
// target — including an IP CIDR — because the payload is portal-and-Postgres
// data; only the named webhook-safe keys ever leave (store.PayloadReason /
// store.PayloadSummary).
func eventPayload(b store.Ban, summary string) json.RawMessage {
	payload := map[string]any{
		store.PayloadSummary: summary,
		"target":             b.Target,
		"banId":              b.ID.String(),
	}
	if b.Reason != "" {
		payload[store.PayloadReason] = b.Reason
	}
	if b.ExpiresAt != nil {
		payload["expiresAt"] = b.ExpiresAt.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
