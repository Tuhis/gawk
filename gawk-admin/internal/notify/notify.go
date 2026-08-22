// Package notify is gawk-admin's operator-notification pipe (R39, docs/42
// §4.10, D9): every moderation event fans out to every enabled webhook, each
// signed with its own HMAC-SHA256 key, retried on a fixed schedule, and left
// permanently visible in the portal's events view.
//
// Four rules shape everything here.
//
//   - **D8 is absolute: no raw broadcast ID and no IP address ever reaches a
//     payload.** Webhooks transit third-party push infrastructure
//     (ntfy/Slack/Matrix) and a raw broadcast ID is a join capability. The
//     receiver gets the HMAC'd key and a portal link; acting requires logging
//     in. The mechanism is structural — Payload has no field to put either in,
//     and only the payload keys store declares webhook-safe
//     (store.PayloadReason, store.PayloadSummary, store.PayloadEnforcement)
//     are copied out of an event's jsonb; the third of those is read through
//     an accessor with a closed vocabulary, so it cannot carry free text.
//   - **The dispatcher is leader-only, and correctness does not depend on
//     that.** Run is started from kube.Election.OnLeading (D16), but claims go
//     through FOR UPDATE SKIP LOCKED, so two dispatchers overlapping across a
//     leadership handover skip each other's rows rather than double-sending.
//     Nothing here assumes a single writer.
//   - **A failed delivery must be SEEN** (§4.10). Every outcome — including
//     the terminal one — is written back to the delivery row with its
//     last_error, because R40's "a flag must reach a human" posture inherits
//     this pipe, and a page that silently never arrived is the failure this
//     surface exists to prevent.
//   - **Enqueue owns the fan-out.** internal/api guarantees only that each
//     persisted event is offered exactly once; this package is the one that
//     knows both webhook sources (chart-defined config and UI-created rows)
//     and holds the signing secrets.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// retrySchedule is docs/42 §4.10's ladder: attempts at +5 s, +30 s, +2 m,
// +10 m, then state = failed. Five attempts total — the first send plus these
// four retries.
//
// It is indexed by the delivery's ALREADY-INCREMENTED attempt count, because
// store.ClaimDueDeliveries increments on claim rather than on completion: a
// row coming back from its first claim carries Attempts == 1, and the next
// attempt for it is retrySchedule[0].
//
// An array rather than a slice so its length is a compile-time constant and
// MaxAttempts can be one too — the budget is contract, not configuration.
var retrySchedule = [...]time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// MaxAttempts is the retry budget: the first send plus len(retrySchedule)
// retries. Exported so a test — and a reader — can state the number without
// recounting the ladder.
const MaxAttempts = len(retrySchedule) + 1

// Defaults for Options.
const (
	// DefaultPollInterval is how often the leader looks for due deliveries.
	// The retry ladder's finest step is 5 s, so a 2 s poll never turns a
	// scheduled retry into a materially late one, and an event enqueued by a
	// NON-leader replica (whose Kick reaches no running loop) still pages
	// within a couple of seconds.
	DefaultPollInterval = 2 * time.Second
	// DefaultBatchSize is how many due rows one claim takes.
	DefaultBatchSize = 20
	// DefaultConcurrency is how many sends run at once. More than one on
	// purpose: an operator typically has two or three webhooks, and one dead
	// endpoint burning its whole timeout must not delay the one that works.
	DefaultConcurrency = 4
	// DefaultRequestTimeout bounds a single send.
	DefaultRequestTimeout = 10 * time.Second
)

// maxDrainRounds bounds one Run iteration so a large backlog cannot turn the
// dispatch loop into an unyielding hot loop.
const maxDrainRounds = 50

// deliveryNamespace derives X-Gawk-Delivery from a delivery row's ID.
//
// Deriving rather than generating makes the header STABLE across retries,
// which is what lets a receiver deduplicate: the same (event, webhook) always
// announces the same delivery UUID, so a receiver that already acted on
// attempt 2 can ignore attempt 3 after its own 200 was lost. A fresh UUID per
// attempt would make every retry look like a new page.
var deliveryNamespace = uuid.MustParse("1f2c7a54-3b8e-4d61-9c0a-5e7d8f2b1a63")

// errWebhookGone marks a delivery whose webhook no longer exists or has been
// disabled. It is terminal, not retryable: retrying cannot make a deleted
// webhook reappear, and a row that stayed pending forever would sit in the
// portal's events view claiming a page is still on its way.
var errWebhookGone = errors.New("notify: webhook is gone")

// Options configure a Dispatcher.
type Options struct {
	// Store is the queue and the event source. Required.
	Store *store.Store
	// Config supplies the chart-defined webhooks (with their secrets, resolved
	// from env at parse time) and ExternalURL for the payload's portalUrl.
	Config config.Config
	Log    *slog.Logger

	// Client is the outbound HTTP client. nil builds one with a bounded
	// timeout and the cross-origin redirect refusal (see newHTTPClient) — a
	// caller supplying its own is responsible for both.
	Client *http.Client

	// Now is the clock; nil means time.Now. It is the seam the retry-schedule
	// tests drive.
	Now func() time.Time

	// PollInterval, BatchSize, Concurrency and RequestTimeout default to the
	// Default* constants when zero.
	PollInterval   time.Duration
	BatchSize      int
	Concurrency    int
	RequestTimeout time.Duration
}

// Dispatcher fans events out to webhooks and drains the delivery queue.
//
// It implements api.Recorder and api.Tester; main.go hands the same value to
// both fields of api.Options and runs Run from kube.Election.OnLeading.
type Dispatcher struct {
	opts   Options
	log    *slog.Logger
	client *http.Client
	kick   chan struct{}
}

// The two seams internal/api declares. Asserted here so a signature drift
// breaks this package's build rather than main.go's wiring.
var (
	_ api.Recorder = (*Dispatcher)(nil)
	_ api.Tester   = (*Dispatcher)(nil)
)

// New builds a Dispatcher.
func New(opts Options) (*Dispatcher, error) {
	if opts.Store == nil {
		return nil, errors.New("notify: Options.Store is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	client := opts.Client
	if client == nil {
		client = newHTTPClient(opts.RequestTimeout)
	}
	return &Dispatcher{
		opts:   opts,
		log:    opts.Log,
		client: client,
		kick:   make(chan struct{}, 1),
	}, nil
}

func (d *Dispatcher) now() time.Time { return d.opts.Now() }

// Record persists one event AND queues one pending delivery per ENABLED
// webhook, merged across both sources (docs/42 D9), in a single Postgres
// transaction. It satisfies api.Recorder.
//
// One transaction on purpose (PR #280 round-2 review): as two calls, a crash
// or transient error between the event append and the delivery enqueue lost
// that event's fan-out forever — deliveries are only ever enqueued from this
// path, and §4.10's "a failed delivery must be seen" inherits it. The store
// reads the enabled UI-created set inside the same transaction; this side
// contributes only the chart-defined names, which are not rows there.
//
// Zero configured webhooks is explicitly fine (§4.10): the event still lands
// in Postgres and the portal feed, with nothing queued.
//
// Called on whichever replica served the mutation — not only the leader — so
// an event is queued the moment it is recorded even if the leader is mid
// handover. The Kick that follows wakes a loop only where one is running.
func (d *Dispatcher) Record(ctx context.Context, ev store.Event) (store.Event, error) {
	saved, err := d.opts.Store.AppendEventAndEnqueue(ctx, ev, d.configNames())
	if err != nil {
		return store.Event{}, err
	}
	d.log.Debug("moderation event recorded", "eventId", saved.ID, "type", saved.Type)
	d.Kick()
	return saved, nil
}

// configNames lists the enabled CHART-defined webhook names — the half of the
// D9 merge the store cannot see, because config webhooks are never rows. The
// UI-created half, and the config-wins collision rule, live in the store's
// transaction; resolve() applies the same precedence again at send time.
func (d *Dispatcher) configNames() []string {
	var names []string
	for _, h := range d.opts.Config.StaticWebhooks {
		if h.IsEnabled() {
			names = append(names, h.Name)
		}
	}
	return names
}

// Kick asks the local loop for an immediate pass. Non-blocking and coalescing,
// like kube.Reconciler.Kick; on a non-leader replica there is no loop to wake
// and the leader's next poll picks the rows up.
func (d *Dispatcher) Kick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Run drains the delivery queue until ctx ends.
//
// LEADER-ONLY work (D16): main.go starts it from kube.Election.OnLeading, whose
// context is cancelled the instant leadership is lost. Everything it starts
// stops with that context, and any row it claimed but never completed becomes
// due again after store.ClaimLease — which is why a handover costs at most one
// delayed notification and never a lost or duplicated one.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(d.opts.PollInterval)
	defer t.Stop()
	d.log.Info("webhook dispatcher started", "pollInterval", d.opts.PollInterval, "batchSize", d.opts.BatchSize)
	defer d.log.Info("webhook dispatcher stopped")
	d.drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.drain(ctx)
		case <-d.kick:
			d.drain(ctx)
		}
	}
}

// drain runs dispatch passes until the queue stops handing back full batches.
func (d *Dispatcher) drain(ctx context.Context) {
	for range maxDrainRounds {
		if ctx.Err() != nil {
			return
		}
		n, err := d.DispatchOnce(ctx)
		if err != nil {
			// Postgres unreachable is the common cause and it heals itself;
			// the rows stay pending and due, so nothing is lost.
			d.log.Warn("claiming webhook deliveries failed", "err", err)
			return
		}
		if n < d.opts.BatchSize {
			return
		}
	}
}

// DispatchOnce claims one batch of due deliveries and sends them, returning
// how many it claimed.
//
// Exported as the test seam the retry-schedule and double-send tests drive
// directly: with a fake clock, one call is exactly one round of attempts, with
// no loop timing in the way.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	claimed, err := d.opts.Store.ClaimDueDeliveries(ctx, d.now(), d.opts.BatchSize)
	if err != nil {
		return 0, err
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	// Resolve each distinct webhook once per pass rather than once per row: a
	// burst of events for one webhook is the normal shape, and this is also
	// the only place a signing secret is read.
	targets := make(map[string]resolution, len(claimed))
	for _, del := range claimed {
		if _, ok := targets[del.WebhookName]; ok {
			continue
		}
		t, err := d.resolve(ctx, del.WebhookName)
		targets[del.WebhookName] = resolution{target: t, err: err}
	}

	sem := make(chan struct{}, d.opts.Concurrency)
	var wg sync.WaitGroup
	for _, del := range claimed {
		wg.Add(1)
		go func(del store.Delivery) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d.deliver(ctx, del, targets[del.WebhookName])
		}(del)
	}
	wg.Wait()
	return len(claimed), nil
}

type resolution struct {
	target target
	err    error
}

// target is one resolved webhook: where to send and what to sign with.
type target struct {
	name   string
	url    string
	secret string
	// source is api.SourceConfig or api.SourceUI. It rides along so every log
	// line says WHICH set a webhook came from — the first question when a
	// chart-defined pager is silent and a UI-created one is not.
	source string
}

// deliver performs one attempt and records its outcome.
func (d *Dispatcher) deliver(ctx context.Context, del store.Delivery, r resolution) {
	if r.err != nil {
		if errors.Is(r.err, errWebhookGone) {
			d.finish(ctx, del, r.err, true)
			return
		}
		// A transient store failure is not the receiver's fault: retry on the
		// normal ladder rather than spending the budget on our own outage.
		d.log.Warn("resolving a webhook for delivery failed", "webhook", del.WebhookName,
			"deliveryId", del.ID, "attempts", del.Attempts, "err", r.err)
		d.finish(ctx, del, r.err, false)
		return
	}

	ev, err := d.opts.Store.GetEvent(ctx, del.EventID)
	if err != nil {
		d.finish(ctx, del, fmt.Errorf("loading event %d: %w", del.EventID, err), errors.Is(err, store.ErrNotFound))
		return
	}
	body, err := marshal(buildPayload(ev, d.opts.Config.ExternalURL))
	if err != nil {
		// Unrenderable payload: retrying cannot fix it.
		d.finish(ctx, del, err, true)
		return
	}

	status, err := d.send(ctx, r.target, ev.Type, DeliveryID(del.ID), body)
	if err != nil {
		d.log.Warn("webhook delivery failed", "webhook", r.target.name, "source", r.target.source,
			"deliveryId", del.ID, "eventId", del.EventID, "attempts", del.Attempts, "status", status, "err", err)
		d.finish(ctx, del, err, false)
		return
	}
	if err := d.opts.Store.MarkDelivered(ctx, del.ID, d.now()); err != nil {
		d.log.Warn("recording a delivered webhook failed", "webhook", r.target.name, "deliveryId", del.ID, "err", err)
		return
	}
	d.log.Info("webhook delivered", "webhook", r.target.name, "source", r.target.source,
		"deliveryId", del.ID, "eventId", del.EventID, "attempts", del.Attempts, "status", status)
}

// finish writes the outcome of a failed attempt: either the next rung of the
// retry ladder or the terminal failed state.
//
// terminal short-circuits the ladder for a failure retrying cannot fix (the
// webhook was deleted, the event vanished). Everything else spends the budget,
// because "the receiver is down" is exactly what the ladder is for.
func (d *Dispatcher) finish(ctx context.Context, del store.Delivery, cause error, terminal bool) {
	msg := cause.Error()
	delay, ok := retryDelay(del.Attempts)
	if terminal || !ok {
		if err := d.opts.Store.MarkDeliveryFailed(ctx, del.ID, msg); err != nil {
			d.log.Warn("recording a failed webhook delivery failed", "deliveryId", del.ID, "err", err)
			return
		}
		d.log.Error("webhook delivery given up", "webhook", del.WebhookName, "deliveryId", del.ID,
			"eventId", del.EventID, "attempts", del.Attempts, "err", msg)
		return
	}
	if err := d.opts.Store.ScheduleRetry(ctx, del.ID, d.now().Add(delay), msg); err != nil {
		d.log.Warn("scheduling a webhook retry failed", "deliveryId", del.ID, "err", err)
	}
}

// retryDelay maps an already-incremented attempt count onto the next rung of
// the ladder. ok is false once the budget is spent.
func retryDelay(attempts int) (time.Duration, bool) {
	idx := attempts - 1
	if idx < 0 {
		// Defensive: a claim always increments, so attempts >= 1. Treating 0
		// as "about to make the first retry" keeps a hand-written row from
		// skipping the ladder entirely.
		idx = 0
	}
	if idx >= len(retrySchedule) {
		return 0, false
	}
	return retrySchedule[idx], true
}

// DeliveryID is the X-Gawk-Delivery value for a delivery row: a UUID derived
// from the row ID, so every retry of the same (event, webhook) repeats it and
// a receiver can deduplicate.
func DeliveryID(rowID int64) string {
	return uuid.NewSHA1(deliveryNamespace, []byte(strconv.FormatInt(rowID, 10))).String()
}

// resolve turns a webhook name into its URL and signing secret, looking in the
// chart-defined set first and the database second.
//
// A disabled or deleted webhook resolves to errWebhookGone: an operator who
// parked a webhook does not want its queued pages arriving later.
func (d *Dispatcher) resolve(ctx context.Context, name string) (target, error) {
	for _, h := range d.opts.Config.StaticWebhooks {
		if h.Name != name {
			continue
		}
		if !h.IsEnabled() {
			return target{}, fmt.Errorf("%w: chart-defined webhook %q is disabled", errWebhookGone, name)
		}
		return target{name: h.Name, url: h.URL, secret: h.Secret, source: api.SourceConfig}, nil
	}
	row, err := d.opts.Store.GetWebhookByName(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return target{}, fmt.Errorf("%w: webhook %q no longer exists", errWebhookGone, name)
		}
		return target{}, err
	}
	if !row.Enabled {
		return target{}, fmt.Errorf("%w: webhook %q is disabled", errWebhookGone, name)
	}
	return target{name: row.Name, url: row.URL, secret: row.Secret, source: api.SourceUI}, nil
}

// send posts one signed payload and reports the HTTP status (0 when the
// request never completed).
func (d *Dispatcher) send(ctx context.Context, t target, eventType, deliveryID string, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, d.opts.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("building the request: %w", cleanURLError(err))
	}
	ts := d.now().UTC().Unix()
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set(HeaderEvent, eventType)
	req.Header.Set(HeaderDelivery, deliveryID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(t.secret, ts, body))

	resp, err := d.client.Do(req)
	if err != nil {
		// A CheckRedirect refusal arrives here with a non-nil response whose
		// body net/http has already closed, so there is nothing to release.
		// cleanURLError keeps the webhook URL's path — often a credential —
		// out of last_error and out of the log.
		return 0, cleanURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		// Drain a bounded amount so the connection can be reused rather than
		// abandoned mid-body on every single delivery.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBody))
		return resp.StatusCode, nil
	}
	snippet := readErrorBody(resp.Body)
	if snippet != "" {
		return resp.StatusCode, fmt.Errorf("receiver answered %s: %s", resp.Status, snippet)
	}
	return resp.StatusCode, fmt.Errorf("receiver answered %s", resp.Status)
}

// TestWebhook sends a synthetic signed `test` event and returns the outcome.
// It satisfies api.Tester.
//
// It writes nothing to Postgres — no event row, no delivery row — because a
// test is not a moderation action; the audit trail must not fill with probes.
// That is also why the outcome is returned rather than left for the events
// view to render.
//
// A DISABLED webhook is still testable: the operator asked for this one send
// explicitly, and proving a parked channel works before enabling it is exactly
// when the button is useful.
func (d *Dispatcher) TestWebhook(ctx context.Context, name string) (api.TestResult, error) {
	t, err := d.testTarget(ctx, name)
	if err != nil {
		// The webhook could not be addressed at all — the API turns this into
		// a 503 with the message, distinct from "the receiver said no", which
		// is a successful test with a failing outcome.
		return api.TestResult{}, err
	}
	body, err := marshal(testPayload(d.now(), d.opts.Config.ExternalURL))
	if err != nil {
		return api.TestResult{}, err
	}
	deliveryID := uuid.New().String()
	status, sendErr := d.send(ctx, t, EventTest, deliveryID, body)
	result := api.TestResult{OK: sendErr == nil, Status: status, DeliveryID: deliveryID}
	if sendErr != nil {
		result.Error = sendErr.Error()
		d.log.Warn("webhook test send failed", "webhook", name, "source", t.source, "status", status, "err", sendErr)
		return result, nil
	}
	d.log.Info("webhook test send delivered", "webhook", name, "source", t.source, "status", status)
	return result, nil
}

// testTarget resolves a name for a test send, tolerating the disabled state
// that resolve treats as terminal for a queued delivery.
func (d *Dispatcher) testTarget(ctx context.Context, name string) (target, error) {
	for _, h := range d.opts.Config.StaticWebhooks {
		if h.Name == name {
			return target{name: h.Name, url: h.URL, secret: h.Secret, source: api.SourceConfig}, nil
		}
	}
	row, err := d.opts.Store.GetWebhookByName(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return target{}, fmt.Errorf("no webhook named %q", name)
		}
		return target{}, err
	}
	return target{name: row.Name, url: row.URL, secret: row.Secret, source: api.SourceUI}, nil
}
