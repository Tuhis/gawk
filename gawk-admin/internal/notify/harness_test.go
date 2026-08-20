package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
)

// deadDSN points at a port nothing listens on. pgxpool connects lazily, so
// store.Open succeeds and every query fails — which is what lets the send-path
// tests (redirects, timeouts, signing) run against a chart-defined webhook
// with no database in sight. Same trick internal/api's harness uses.
const deadDSN = "postgres://nobody:nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1"

// fakeClock is the seam the retry-schedule test drives. Safe for concurrent
// use because sends run on their own goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// capture is one request a webhook receiver saw, kept whole so a test can
// verify the signature exactly as a real receiver would.
type capture struct {
	path        string
	event       string
	delivery    string
	timestamp   string
	signature   string
	contentType string
	body        []byte
}

// payloadOf decodes a captured body into a generic map, so a test can assert
// over the KEY SET rather than over a struct that would silently accept a new
// field.
func (c capture) payloadOf(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.body, &m); err != nil {
		t.Fatalf("payload is not JSON: %v (%s)", err, c.body)
	}
	return m
}

// receiver is a webhook endpoint under test control.
type receiver struct {
	srv *httptest.Server

	mu     sync.Mutex
	got    []capture
	status int
	// hook, when set, runs before the response is written. It is how the
	// double-send test observes overlapping in-flight requests.
	hook func(capture)
	body string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		c := capture{
			path:        req.URL.Path,
			event:       req.Header.Get(HeaderEvent),
			delivery:    req.Header.Get(HeaderDelivery),
			timestamp:   req.Header.Get(HeaderTimestamp),
			signature:   req.Header.Get(HeaderSignature),
			contentType: req.Header.Get("Content-Type"),
			body:        body,
		}
		r.mu.Lock()
		r.got = append(r.got, c)
		status, hook, respBody := r.status, r.hook, r.body
		r.mu.Unlock()
		if hook != nil {
			hook(c)
		}
		w.WriteHeader(status)
		if respBody != "" {
			_, _ = io.WriteString(w, respBody)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) url(path string) string { return r.srv.URL + path }

func (r *receiver) setStatus(status int, body string) {
	r.mu.Lock()
	r.status, r.body = status, body
	r.mu.Unlock()
}

func (r *receiver) setHook(f func(capture)) {
	r.mu.Lock()
	r.hook = f
	r.mu.Unlock()
}

func (r *receiver) captures() []capture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capture(nil), r.got...)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// byPath indexes captures by request path, which is how each test tells one
// webhook's delivery from another's.
func (r *receiver) byPath() map[string][]capture {
	out := map[string][]capture{}
	for _, c := range r.captures() {
		out[c.path] = append(out[c.path], c)
	}
	return out
}

func enabled(v bool) *bool { return &v }

// newDispatcher builds a Dispatcher over a store with test-friendly defaults:
// one send at a time and a short timeout, so a hung receiver fails a test
// instead of hanging it.
func newDispatcher(t *testing.T, st *store.Store, cfg config.Config, tweak func(*Options)) *Dispatcher {
	t.Helper()
	opts := Options{
		Store:          st,
		Config:         cfg,
		Concurrency:    1,
		RequestTimeout: 5 * time.Second,
	}
	if tweak != nil {
		tweak(&opts)
	}
	d, err := New(opts)
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	return d
}

// deadStore opens a store whose every query fails. The chart-defined webhook
// path never touches it, so the send-path tests need no Postgres.
func deadStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), deadDSN)
	if err != nil {
		t.Fatalf("open dead store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// seedEvent appends one event, failing the test if it cannot.
func seedEvent(t *testing.T, st *store.Store, ev store.Event) store.Event {
	t.Helper()
	saved, err := st.AppendEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	return saved
}

// newStore is storetest.New, named locally so every Postgres-backed test in
// this file reads the same way and skips identically without the DSN.
func newStore(t *testing.T) *store.Store { return storetest.New(t) }

// deliveriesFor returns one event's delivery rows keyed by webhook name — the
// same query the portal's events view runs.
func deliveriesFor(t *testing.T, st *store.Store, eventID int64) map[string]store.Delivery {
	t.Helper()
	byEvent, err := st.ListDeliveriesForEvents(context.Background(), []int64{eventID})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	out := map[string]store.Delivery{}
	for _, d := range byEvent[eventID] {
		out[d.WebhookName] = d
	}
	return out
}
