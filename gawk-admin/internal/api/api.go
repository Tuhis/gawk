// Package api serves gawk-admin's /api/v1 (R39, docs/42 §4.7).
//
// **Authentication is injected, never implemented here.** Options.Authn and
// Options.RequireRole come from internal/auth; this package only READS the
// identity that middleware put on the request context (internal/identity).
// The two packages do not import each other, so the authentication mechanism
// and the routes it protects stay independently buildable and testable — and
// swapping the mechanism later touches one package rather than every handler.
//
// Three rules run through every handler:
//
//   - **No cookies, ever** (D17). There is no session, no CSRF machinery, and
//     a Set-Cookie on any response is a bug.
//   - **Raw broadcast IDs and IP addresses are allowed here** — this is the
//     OIDC-gated admin surface D8 scopes the relaxation to — but they must
//     never reach a webhook payload. Handlers persist events; AP7's dispatcher
//     copies only the named webhook-safe payload keys.
//   - **Postgres is the commitment point.** A mutation that has written its
//     `bans` row has happened; the Ban CR is a projection the reconciler heals.
//     Handlers therefore write the row, project the CR inline, and grade the
//     result on which of the two writes landed — see the status matrix below.
//
// A ban is two writes in two systems that cannot share a transaction, so a
// mutation has three outcomes, not two:
//
//	row written, CR projected → 201 Created (kill, create) · 204 No Content (unban)
//	row written, CR failed    → 202 Accepted, body = the ban with enforcement.inSync:false
//	row not written           → 5xx (503 when Postgres is unreachable, 500 otherwise)
//
// The verdict does not stop at the response. Every handler projects BEFORE it
// records, so the same grade rides into the event's payload and its summary
// (store.EnforcementState) and out to the webhook: an operator whose phone
// says "banned" while the relay is not enforcing would be reading the same lie
// a 201 would have told, only further from the portal that could correct it.
//
// The middle row is a SUCCESS: the record is durable and the reconciler —
// precisely RFC 9110 §15.3.3's "another process or server" — finishes the job
// within a minute. R39 answered 502 there, which was wrong twice over: nothing
// acted as a gateway, and calling a committed ban a failure invites a
// re-submit that now 409s against the row that does exist. The last row is the
// one where nothing happened at all: no CR is written, no event is emitted,
// and no ban comes back.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/identity"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// Error codes in the {"error":{"code","message"}} envelope. The SPA branches
// on `code`, so these are contract, not log text.
const (
	CodeBadRequest      = "bad_request"
	CodeNotFound        = "not_found"
	CodeDuplicateActive = "duplicate_active"
	CodeDuplicateName   = "duplicate_name"
	CodeSourceImmutable = "source_immutable"
	CodeInternal        = "internal"
	CodeUnavailable     = "unavailable"
	CodeInvalidTarget   = "invalid_target"
	CodeNotActive       = "ban_not_active"
)

// Projector writes one ban row's Ban CR. Implemented by *kube.Reconciler.
//
// It is called INLINE by every mutation, on whichever replica served the
// request: CR names are deterministic, so a non-leader writing its own
// projection is safe, and it is what makes a kill take effect in the time of
// one API call instead of by the next 60 s sweep.
type Projector interface {
	Project(ctx context.Context, b store.Ban) error
}

// Kicker asks the local reconciler for an immediate sweep. Best-effort: on a
// non-leader replica there is no loop to wake, and the leader's next sweep
// heals anything this request could not finish.
type Kicker interface {
	Kick()
}

// Fleet is the relay enumeration this package reads. Implemented by
// *relayscan.Scanner.
type Fleet interface {
	Snapshot(ctx context.Context) (relayscan.Snapshot, error)
	Invalidate()
}

// Recorder persists a moderation event together with its webhook fan-out
// (AP7) — one transaction, so a crash cannot record an event whose deliveries
// were never queued (the AppendEvent → EnqueueDeliveries window, PR #280
// round-2 review).
//
// The dispatcher — not this package — decides WHICH webhooks an event fans out
// to: it is the component that knows both the config-sourced and UI-created
// sets and holds the signing secrets. Handlers only guarantee that every event
// they produce is offered exactly once.
type Recorder interface {
	Record(ctx context.Context, ev store.Event) (store.Event, error)
}

// TestResult is the outcome of POST /webhooks/{name}/test.
type TestResult struct {
	OK         bool   `json:"ok"`
	Status     int    `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
	DeliveryID string `json:"deliveryId,omitempty"`
}

// Tester performs a synthetic signed delivery. Implemented by AP7's
// dispatcher; this package never signs or sends anything itself.
type Tester interface {
	TestWebhook(ctx context.Context, name string) (TestResult, error)
}

// ReadyCheck is one additional readiness gate. main.go folds internal/auth's
// Ready() in through this, so /readyz answers for the whole process while this
// package owns only the Postgres half.
type ReadyCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// Options configure an API.
type Options struct {
	// Store is the system of record. Required.
	Store *store.Store
	// Projector writes Ban CRs. Required in production; a nil Projector makes
	// mutations record-only, which is useful in tests and useless in a cluster.
	Projector Projector
	// Reconciler, when set, is kicked after every mutation.
	Reconciler Kicker
	// Fleet enumerates relay pods. Required for /broadcasts and /relays.
	Fleet Fleet
	// Rooms manages Room CRs (R42). nil means rooms are OFF: no /rooms route
	// is registered (the catch-all answers 404) and /me reports the feature
	// absent, so the SPA shows no rooms view. main.go sets it only under
	// -rooms, the value the chart derives from the same rooms.enabled that
	// grants the Role its Room and Secret verbs.
	Rooms Rooms
	// Config carries the deep-link bases, the kill cooldown, the operator role
	// and the chart-defined webhooks.
	Config config.Config

	// Authn wraps a handler so it runs only for a request carrying a valid
	// token whose identity has been placed on the context. Supplied by
	// internal/auth; api never validates a token itself.
	Authn func(http.Handler) http.Handler
	// RequireRole wraps a handler so it runs only when the context identity
	// carries role. Supplied by internal/auth.
	RequireRole func(role string) func(http.Handler) http.Handler

	// Recorder and Tester are AP7's dispatcher. Both have safe defaults so
	// this package is testable without internal/notify.
	Recorder Recorder
	Tester   Tester

	// ReadyChecks run alongside the Postgres check in /readyz.
	ReadyChecks []ReadyCheck

	Log *slog.Logger
	// Clock is the time source; nil means time.Now.
	Clock func() time.Time
}

// API serves /api/v1 plus the two probe endpoints.
type API struct {
	opts Options
	log  *slog.Logger

	// readyMu guards the last readiness verdict, so the "refusing to serve"
	// line is logged on transitions rather than once per probe — a kubelet
	// polls /readyz every few seconds and an unbootable pod must not drown its
	// own explanation.
	readyMu   sync.Mutex
	lastReady *bool
}

// New builds an API.
func New(opts Options) (*API, error) {
	if opts.Store == nil {
		return nil, errors.New("api: Options.Store is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Recorder == nil {
		opts.Recorder = appendOnlyRecorder{opts.Store}
	}
	if opts.Tester == nil {
		opts.Tester = unavailableTester{}
	}
	return &API{opts: opts, log: opts.Log}, nil
}

// Routes returns the /api/v1 subtree with absolute patterns, so main.go can
// mount it on the root mux alongside the portal SPA:
//
//	root.Handle("/api/v1/", api.Routes())
//	root.HandleFunc("/healthz", api.Healthz)
//	root.HandleFunc("/readyz", api.Readyz)
//	root.Handle("/", portal)
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /api/v1/me", a.protect(a.handleMe))

	mux.Handle("GET /api/v1/broadcasts", a.protect(a.handleListBroadcasts))
	mux.Handle("POST /api/v1/broadcasts/{id}/kill", a.protect(a.handleKill))

	mux.Handle("GET /api/v1/bans", a.protect(a.handleListBans))
	mux.Handle("POST /api/v1/bans", a.protect(a.handleCreateBan))
	mux.Handle("DELETE /api/v1/bans/{id}", a.protect(a.handleDeleteBan))

	mux.Handle("GET /api/v1/events", a.protect(a.handleListEvents))
	mux.Handle("GET /api/v1/relays", a.protect(a.handleListRelays))

	mux.Handle("GET /api/v1/webhooks", a.protect(a.handleListWebhooks))
	mux.Handle("POST /api/v1/webhooks", a.protect(a.handleCreateWebhook))
	mux.Handle("PUT /api/v1/webhooks/{id}", a.protect(a.handleUpdateWebhook))
	mux.Handle("DELETE /api/v1/webhooks/{id}", a.protect(a.handleDeleteWebhook))
	mux.Handle("POST /api/v1/webhooks/{name}/test", a.protect(a.handleTestWebhook))

	// R42 rooms (docs/44 D20), only with the feature on: with it off the
	// paths fall through to the catch-all's 404, exactly like R40's reserved
	// route, so nothing can be reached that the ServiceAccount could not act
	// on anyway.
	if a.opts.Rooms != nil {
		mux.Handle("GET /api/v1/rooms", a.protect(a.handleListRooms))
		mux.Handle("POST /api/v1/rooms", a.protect(a.handleCreateRoom))
		mux.Handle("POST /api/v1/rooms/{name}/rotate-secret", a.protect(a.handleRotateRoomSecret))
		mux.Handle("POST /api/v1/rooms/{name}/end", a.protect(a.handleEndRoom))
		mux.Handle("DELETE /api/v1/rooms/{name}", a.protect(a.handleDeleteRoom))
	}

	// The catch-all keeps unknown /api/v1 paths answering the documented error
	// envelope instead of net/http's plain text — and it is how
	// POST /api/v1/content-flags returns 404 in R39. That route is R40's
	// (docs/42 §4.11, D11): the path is frozen and deliberately NOT registered
	// here, so nothing can squat it in the meantime.
	//
	// It is unauthenticated on purpose: "this path does not exist" is not a
	// secret, and requiring a token to learn it would only make the reserved
	// route harder to verify.
	mux.Handle("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint")
	}))
	return mux
}

// protect wraps a handler in the injected authentication and role check. Both
// are optional so the package is testable standalone; in production main.go
// supplies them and every route below is behind a valid token carrying the
// operator role (D17).
func (a *API) protect(h http.HandlerFunc) http.Handler {
	var handler http.Handler = h
	if a.opts.RequireRole != nil {
		handler = a.opts.RequireRole(a.opts.Config.OperatorRole)(handler)
	}
	if a.opts.Authn != nil {
		handler = a.opts.Authn(handler)
	}
	return handler
}

// Healthz is liveness: the process is running. It deliberately does NOT touch
// Postgres — a database outage must not get every replica restarted.
func (a *API) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Readyz is readiness: Postgres reachable, schema version compatible, plus
// every injected check (internal/auth's JWKS resolution, via main.go).
//
// The schema check is the serving process's whole relationship with
// migrations (§4.15/D18): it READS the version and refuses to serve when the
// database is older than this build requires. It never applies DDL.
func (a *API) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type result struct {
		Name  string `json:"name"`
		Error string `json:"error,omitempty"`
	}
	out := struct {
		Ready  bool     `json:"ready"`
		Checks []result `json:"checks"`
	}{Ready: true}

	var firstErr error
	if err := a.opts.Store.Ready(ctx); err != nil {
		out.Ready = false
		firstErr = err
		out.Checks = append(out.Checks, result{Name: "postgres", Error: err.Error()})
	} else {
		out.Checks = append(out.Checks, result{Name: "postgres"})
	}
	for _, c := range a.opts.ReadyChecks {
		if c.Check == nil {
			continue
		}
		if err := c.Check(ctx); err != nil {
			out.Ready = false
			if firstErr == nil {
				firstErr = err
			}
			out.Checks = append(out.Checks, result{Name: c.Name, Error: err.Error()})
			continue
		}
		out.Checks = append(out.Checks, result{Name: c.Name})
	}

	a.noteReadiness(out.Ready, firstErr)

	status := http.StatusOK
	if !out.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, out)
}

// noteReadiness logs the one clear line the acceptance criteria ask for, on
// each transition rather than on every probe.
func (a *API) noteReadiness(ready bool, err error) {
	a.readyMu.Lock()
	changed := a.lastReady == nil || *a.lastReady != ready
	a.lastReady = &ready
	a.readyMu.Unlock()
	if !changed {
		return
	}
	if ready {
		a.log.Info("readiness restored: serving")
		return
	}
	switch {
	case errors.Is(err, store.ErrSchemaTooOld):
		a.log.Error("refusing to serve: the database schema is older than this build requires; run the gawk-admin migrate step", "err", err)
	case errors.Is(err, store.ErrSchemaDirty):
		a.log.Error("refusing to serve: the database schema is marked dirty by a failed migration", "err", err)
	default:
		a.log.Error("refusing to serve: a readiness check failed", "err", err)
	}
}

// StoreReady exposes the Postgres half of readiness on its own, for a caller
// that composes the whole answer itself.
func (a *API) StoreReady(ctx context.Context) error { return a.opts.Store.Ready(ctx) }

func (a *API) now() time.Time { return a.opts.Clock() }

// caller returns the authenticated identity. Its absence on a protected route
// is a programming error — the middleware refuses the request long before a
// handler runs — so it is a 500, never an anonymous caller.
func (a *API) caller(w http.ResponseWriter, r *http.Request) (identity.Identity, bool) {
	id, ok := identity.FromContext(r.Context())
	if !ok {
		a.log.Error("no identity on an authenticated route: authentication middleware is not wired", "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, CodeInternal, "authentication is not configured")
		return identity.Identity{}, false
	}
	return id, true
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The portal must never cache an actuator's answers: a stale broadcast
	// list is a stale kill button.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// decodeJSON reads a request body into v, rejecting unknown fields so a
// mistyped field name is an error rather than a silently-ignored intent — on
// an enforcement API, "cooldownSecs" quietly defaulting to 10 minutes is the
// wrong failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// storeStatus maps a store error onto its deliberate HTTP status. Every
// sentinel gets one; the catch-all is 500 and is logged by the caller.
func storeStatus(err error) (int, string, bool) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, CodeNotFound, true
	case errors.Is(err, store.ErrDuplicateActive):
		return http.StatusConflict, CodeDuplicateActive, true
	case errors.Is(err, store.ErrDuplicateName):
		return http.StatusConflict, CodeDuplicateName, true
	case errors.Is(err, store.ErrNotActive):
		return http.StatusConflict, CodeNotActive, true
	default:
		return 0, "", false
	}
}

// fail is the single unhandled-error exit: 503 when Postgres is the problem
// (the operator should retry, §6 "portal mutations 503"), 500 otherwise.
func (a *API) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if status, code, ok := storeStatus(err); ok {
		writeError(w, status, code, err.Error())
		return
	}
	a.log.Error("request failed", "path", r.URL.Path, "what", what, "err", err)
	if a.opts.Store.Ping(r.Context()) != nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"the moderation database is unreachable; enforcement of existing bans is unaffected")
		return
	}
	writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
}

// project writes the ban's CR inline and reports whether it succeeded. A
// failure is NOT hidden: the row is committed and the reconciler will heal the
// CR within its sweep, but the operator is told — through a 202 and its
// `enforcement` object — because "the kill worked" when the enforcement object
// was never written is the one lie this surface must not tell.
func (a *API) project(ctx context.Context, b store.Ban) error {
	if a.opts.Projector == nil {
		return nil
	}
	ctx, cancel := postCommit(ctx)
	defer cancel()
	return a.opts.Projector.Project(ctx, b)
}

// postCommitTimeout bounds work that no longer has a client waiting on it.
const postCommitTimeout = 10 * time.Second

// postCommit detaches a request context for the bookkeeping that follows a
// committed row.
//
// The row is the commitment point: once it is written the enforcement has
// happened, and the CR projection and the event write are obligations, not
// parts of the answer. Running them on the request context means an operator's
// browser aborting in that window takes them with it — and losing the event
// loses every webhook page permanently, because deliveries are only ever
// enqueued from the event row. There is no retry for a page that was never
// created; internal/auth's keyset refresh detaches for the same reason.
//
// It keeps a deadline: detached is not unbounded, and a wedged API server must
// not accumulate handler goroutines.
func postCommit(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), postCommitTimeout)
}

// enforcementState grades a projection outcome for the event that records it.
//
// The same verdict that decides 201-vs-202 rides into the event's payload and
// into its summary, so the webhook says what the HTTP response says. A webhook
// is a statement that something happened; "broadcast X was terminated" while
// the relay is not enforcing anything is the same lie a 201 would have been —
// and it is the one the operator reads on a phone, away from the portal that
// would have shown them the 202.
func enforcementState(projErr error) store.EnforcementState {
	if projErr != nil {
		return store.EnforcementPending
	}
	return store.EnforcementInSync
}

// expireLapsed clears a lapsed ban out of the way of a new one on the same
// target, recording the ban.expired the janitor's sweep would have.
//
// The two duplicate gates disagree by construction and this reconciles them:
// ActiveBanForTarget evaluates expiry the way a relay does, but the partial
// unique index behind ErrDuplicateActive can only know `state = 'active'`. So
// a kill whose cooldown has lapsed would 409 against its OWN predecessor —
// until the leader's next 60 s sweep, and indefinitely whenever no replica
// holds the Lease. Doing it inline makes the re-kill independent of the
// janitor entirely.
//
// Best-effort: a failure here just means CreateBan answers the 409 it would
// have anyway, so it is logged rather than failing a mutation that has not
// happened yet. The actor is "system" for the same reason the janitor's is —
// nobody lifted this ban, its clock ran out.
func (a *API) expireLapsed(ctx context.Context, target moderation.Target) {
	lapsed, err := a.opts.Store.ExpireLapsedBansForTarget(ctx, target, a.now())
	if err != nil {
		a.log.Warn("expiring a lapsed ban before re-banning its target failed", "err", err)
		return
	}
	for _, b := range lapsed {
		a.record(ctx, store.Event{
			Type:        store.EventBanExpired,
			OccurredAt:  a.now(),
			Actor:       "system",
			BroadcastID: sourceBroadcastID(b),
			Payload: banPayload(b, b.Reason,
				store.Summarize(store.EventBanExpired, b.Target.Type, "", ""), store.EnforcementInSync),
		})
		a.log.Info("lapsed ban expired inline before re-banning its target", "banId", b.ID,
			"targetType", b.Target.Type)
	}
}

// sourceBroadcastID is the event's raw-ID column: the broadcast the ban was
// taken against. Portal and Postgres only — the dispatcher never copies it
// into a webhook (D8).
func sourceBroadcastID(b store.Ban) string {
	if b.SourceBroadcastID != "" {
		return b.SourceBroadcastID
	}
	if b.Target.Type == moderation.TargetBroadcastID {
		return b.Target.Value
	}
	return ""
}

func (a *API) kick() {
	if a.opts.Reconciler != nil {
		a.opts.Reconciler.Kick()
	}
}

// record persists an event and its webhook fan-out — one Recorder call, one
// transaction. A failure is logged, never fatal to the mutation that caused
// it: the enforcement action has already happened, and losing its
// notification must not un-happen it. What can no longer happen is the HALF
// failure — an event recorded whose fan-out silently never queued.
//
// It runs detached from the request (see postCommit): the one caller who must
// never be able to cancel this is the client whose action it records.
func (a *API) record(ctx context.Context, ev store.Event) {
	ctx, cancel := postCommit(ctx)
	defer cancel()
	if _, err := a.opts.Recorder.Record(ctx, ev); err != nil {
		a.log.Error("recording a moderation event failed", "type", ev.Type, "err", err)
	}
}

// appendOnlyRecorder is the no-dispatcher default: the event lands in Postgres
// and the portal feed with no fan-out queued. Deliberately NOT
// AppendEventAndEnqueue — with no dispatcher running, queued rows would sit
// "pending" in the feed forever, claiming a page is on its way that nothing
// will ever send.
type appendOnlyRecorder struct{ store *store.Store }

func (r appendOnlyRecorder) Record(ctx context.Context, ev store.Event) (store.Event, error) {
	return r.store.AppendEvent(ctx, ev)
}

type unavailableTester struct{}

func (unavailableTester) TestWebhook(context.Context, string) (TestResult, error) {
	return TestResult{}, errors.New("the webhook dispatcher is not configured")
}
