package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/identity"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
)

// deadDSN points at a port nothing listens on. pgxpool connects lazily, so
// store.Open succeeds and every query fails — which is exactly the "Postgres
// is down" condition /readyz must report, testable without any database.
const deadDSN = "postgres://nobody:nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1"

// fakeFleet stands in for relayscan.Scanner.
type fakeFleet struct {
	mu          sync.Mutex
	snap        relayscan.Snapshot
	err         error
	invalidated int
	// hook runs inside Snapshot, with the caller's context. It is how a test
	// suspends a handler at a known point — the fleet lookup is the last thing
	// a mutation does on the REQUEST context — and observes what happens after.
	hook func(context.Context)
}

func (f *fakeFleet) Snapshot(ctx context.Context) (relayscan.Snapshot, error) {
	f.mu.Lock()
	hook, snap, err := f.hook, f.snap, f.err
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	return snap, err
}

func (f *fakeFleet) setHook(hook func(context.Context)) {
	f.mu.Lock()
	f.hook = hook
	f.mu.Unlock()
}

func (f *fakeFleet) Invalidate() {
	f.mu.Lock()
	f.invalidated++
	f.mu.Unlock()
}

func (f *fakeFleet) set(snap relayscan.Snapshot) {
	f.mu.Lock()
	f.snap, f.err = snap, nil
	f.mu.Unlock()
}

// fakeProjector records the Ban CR writes a mutation performed inline.
type fakeProjector struct {
	mu        sync.Mutex
	projected []store.Ban
	err       error
	// ctxErrs is each call's ctx.Err() at entry. A projection is post-commit
	// work: it must never arrive already cancelled because the operator's
	// browser hung up.
	ctxErrs []error
}

func (p *fakeProjector) Project(ctx context.Context, b store.Ban) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctxErrs = append(p.ctxErrs, ctx.Err())
	if p.err != nil {
		return p.err
	}
	p.projected = append(p.projected, b)
	return nil
}

func (p *fakeProjector) contexts() []error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]error(nil), p.ctxErrs...)
}

func (p *fakeProjector) last() (store.Ban, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.projected) == 0 {
		return store.Ban{}, false
	}
	return p.projected[len(p.projected)-1], true
}

func (p *fakeProjector) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.projected)
}

// breakFrom makes every SUBSEQUENT Project fail. It is how the unban tests get
// a ban that exists with a CR behind it and then lose only the delete.
func (p *fakeProjector) breakFrom(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// recordingEnqueuer captures what the API offered to AP7's dispatcher.
type recordingEnqueuer struct {
	mu     sync.Mutex
	events []store.Event
}

func (e *recordingEnqueuer) Enqueue(_ context.Context, ev store.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
	return nil
}

func (e *recordingEnqueuer) all() []store.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]store.Event(nil), e.events...)
}

type stubTester struct {
	result api.TestResult
	err    error
	mu     sync.Mutex
	names  []string
}

func (s *stubTester) TestWebhook(_ context.Context, name string) (api.TestResult, error) {
	s.mu.Lock()
	s.names = append(s.names, name)
	s.mu.Unlock()
	return s.result, s.err
}

type kickCounter struct {
	mu sync.Mutex
	n  int
}

func (k *kickCounter) Kick() {
	k.mu.Lock()
	k.n++
	k.mu.Unlock()
}

func (k *kickCounter) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n
}

// harness wires an API the way main.go will, with test doubles for everything
// outside this package's lane.
type harness struct {
	t     *testing.T
	api   *api.API
	srv   *httptest.Server
	store *store.Store
	fleet *fakeFleet
	proj  *fakeProjector
	enq   *recordingEnqueuer
	test  *stubTester
	kicks *kickCounter
	logs  *bytes.Buffer

	// identity is what the injected Authn puts on every request context.
	// Tests mutate it to change who is calling.
	identity identity.Identity
}

type harnessOption func(*api.Options, *harness)

func withConfig(mutate func(*config.Config)) harnessOption {
	return func(o *api.Options, _ *harness) { mutate(&o.Config) }
}

func withProjectorError(err error) harnessOption {
	return func(_ *api.Options, h *harness) { h.proj.err = err }
}

// newHarness builds an API against a fresh migrated database. It SKIPS when no
// test Postgres is configured, so `go test ./...` stays green on a machine
// without one.
func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	return buildHarness(t, storetest.New(t), opts...)
}

// newHarnessWithoutPostgres builds an API whose store can never answer — the
// only way to exercise the "Postgres is unreachable" paths with no database
// present at all.
func newHarnessWithoutPostgres(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	s, err := store.Open(context.Background(), deadDSN)
	if err != nil {
		t.Fatalf("open a store on an unreachable DSN: %v", err)
	}
	t.Cleanup(s.Close)
	return buildHarness(t, s, opts...)
}

func buildHarness(t *testing.T, s *store.Store, opts ...harnessOption) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		store: s,
		fleet: &fakeFleet{},
		proj:  &fakeProjector{},
		enq:   &recordingEnqueuer{},
		test:  &stubTester{result: api.TestResult{OK: true, Status: 200, DeliveryID: "d-1"}},
		kicks: &kickCounter{},
		logs:  &bytes.Buffer{},
		identity: identity.Identity{
			Subject: "sub-1", Email: "op@example.com", Roles: []string{"operator"},
		},
	}
	o := api.Options{
		Store:      s,
		Projector:  h.proj,
		Reconciler: h.kicks,
		Fleet:      h.fleet,
		Enqueuer:   h.enq,
		Tester:     h.test,
		Config: config.Config{
			OperatorRole:     "operator",
			KillCooldown:     10 * time.Minute,
			AppBaseURL:       "https://gawk.example",
			TelemetryBaseURL: "https://telemetry.example",
		},
		Log: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		// The injected authentication seam: internal/auth supplies these in
		// production; here a passthrough that stamps the harness identity, so
		// this package's tests never depend on JWT machinery.
		Authn: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(identity.NewContext(r.Context(), h.identity)))
			})
		},
		RequireRole: func(role string) func(http.Handler) http.Handler {
			return func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					id, _ := identity.FromContext(r.Context())
					if !id.HasRole(role) {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					next.ServeHTTP(w, r)
				})
			}
		},
	}
	for _, opt := range opts {
		opt(&o, h)
	}

	a, err := api.New(o)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	h.api = a

	root := http.NewServeMux()
	root.Handle("/api/v1/", a.Routes())
	root.HandleFunc("/healthz", a.Healthz)
	root.HandleFunc("/readyz", a.Readyz)
	h.srv = httptest.NewServer(root)
	t.Cleanup(h.srv.Close)
	return h
}

// do issues a request and returns the response. Every response is checked for
// Set-Cookie: there are no cookies anywhere in this service (D17), and a
// stray one is a test failure by construction rather than by a separate test.
func (h *harness) do(method, path string, body any) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal request body: %v", err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, r)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		h.t.Fatalf("%s %s set a cookie (%v): gawk-admin has no sessions and no cookies (D17)", method, path, cookies)
	}
	return resp
}

// raw issues a request and returns status plus the body as a string.
func (h *harness) raw(method, path string, body any) (int, string) {
	h.t.Helper()
	resp := h.do(method, path, body)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// decode issues a request, asserts the status, and decodes the JSON body.
func (h *harness) decode(method, path string, body any, wantStatus int, out any) {
	h.t.Helper()
	status, raw := h.raw(method, path, body)
	if status != wantStatus {
		h.t.Fatalf("%s %s = %d, want %d; body: %s", method, path, status, wantStatus, raw)
	}
	if out == nil || strings.TrimSpace(raw) == "" {
		return
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		h.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
	}
}

// errorCode asserts a failing call's status and returns the envelope's code.
func (h *harness) errorCode(method, path string, body any, wantStatus int) string {
	h.t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	h.decode(method, path, body, wantStatus, &env)
	if env.Error.Code == "" {
		h.t.Fatalf("%s %s returned %d with no error code", method, path, wantStatus)
	}
	return env.Error.Code
}

func (h *harness) logText() string { return h.logs.String() }

// wireBroadcast is the test's view of the /broadcasts wire shape. It is
// declared here, independently of the handler's own structs, so a rename in
// the response contract fails a test rather than silently following along.
type wireBroadcast struct {
	ID                string `json:"id"`
	Key               string `json:"key"`
	PublisherActive   bool   `json:"publisherActive"`
	PublisherRemoteIP string `json:"publisherRemoteIp"`
	StartedAt         string `json:"startedAt"`
	ViewersGlobal     int    `json:"viewersGlobal"`
	Pods              []struct {
		Pod          string `json:"pod"`
		Role         string `json:"role"`
		ViewersLocal int    `json:"viewersLocal"`
	} `json:"pods"`
	Links *struct {
		Watch     string `json:"watch"`
		Telemetry string `json:"telemetry"`
	} `json:"links"`
	BanState *struct {
		Banned bool     `json:"banned"`
		Ban    *wireBan `json:"ban"`
	} `json:"banState"`
}

type wireBan struct {
	ID     string `json:"id"`
	Target struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"target"`
	State             string  `json:"state"`
	Reason            string  `json:"reason"`
	CreatedAt         string  `json:"createdAt"`
	CreatedBy         string  `json:"createdBy"`
	ExpiresAt         *string `json:"expiresAt"`
	RemovedAt         *string `json:"removedAt"`
	RemovedBy         string  `json:"removedBy"`
	SourceBroadcastID string  `json:"sourceBroadcastId"`
	CRName            string  `json:"crName"`
	// Enforcement rides only on a 202 — the mutation whose row landed and
	// whose CR did not. A pointer so "absent" and "present and false" are
	// distinguishable, which is the whole assertion on a 201.
	Enforcement *wireEnforcement `json:"enforcement"`
}

type wireEnforcement struct {
	InSync bool   `json:"inSync"`
	Detail string `json:"detail"`
}

type wireEvent struct {
	ID           int64  `json:"id"`
	Type         string `json:"type"`
	OccurredAt   string `json:"occurredAt"`
	Actor        string `json:"actor"`
	BroadcastKey string `json:"broadcastKey"`
	BroadcastID  string `json:"broadcastId"`
	Reason       string `json:"reason"`
	Summary      string `json:"summary"`
	Deliveries   []struct {
		WebhookName string `json:"webhookName"`
		State       string `json:"state"`
		Attempts    int    `json:"attempts"`
		LastError   string `json:"lastError"`
	} `json:"deliveries"`
}

type wireWebhook struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
}

// liveSnapshot builds a one-broadcast, two-pod fleet view.
func liveSnapshot(id, key, publisherIP string) relayscan.Snapshot {
	return relayscan.Snapshot{
		At:           time.Now(),
		PodsResolved: 2,
		PodsAnswered: 2,
		Pods: []relayscan.Pod{
			{Name: "gawk-server-0", Addr: "10.42.0.7:2112", Reachable: true, Version: "1.42.0",
				Config: map[string]any{"addr": ":4433", "publishSecret": "<set>"}},
			{Name: "gawk-server-1", Addr: "10.42.0.8:2112", Reachable: false, Err: "dial timeout"},
		},
		Broadcasts: []relayscan.Aggregate{{
			ID: id, Key: key, PublisherActive: true, PublisherRemoteIP: publisherIP,
			StartedAt: time.Date(2026, 8, 20, 14, 0, 11, 0, time.UTC), ViewersGlobal: 340,
			Pods: []relayscan.Placement{
				{Pod: "gawk-server-0", Role: "origin", ViewersLocal: 12},
				{Pod: "gawk-server-1", Role: "edge", ViewersLocal: 328},
			},
		}},
	}
}

// testClock is a manual clock for both halves of a mutation: api.Options.Clock
// decides a kill's cooldown expiry, and store.Store.Now decides whether a row
// is still in force. They must move together or the test is measuring the
// disagreement rather than the behaviour.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func withClock(c *testClock) harnessOption {
	return func(o *api.Options, h *harness) {
		o.Clock = c.Now
		h.store.Now = c.Now
	}
}
