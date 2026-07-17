// R17 W4 acceptance tests (docs/22): the TimeSync estimator port + per-hop
// ClockMapping rewrite math, the edge session lifecycle against fake
// upstreams (prime invalidation on upstream loss — a stale prime can never
// be served alongside post-re-home deltas), the internal route's loop
// guards, and edge teardown on lease deletion.
package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func TestTimeSyncEstimatorLowestRTTWins(t *testing.T) {
	e := &timeSyncEstimator{}
	if _, _, ok := e.best(); ok {
		t.Fatal("empty estimator returned a sample")
	}

	// A slow, asymmetric exchange first (offset error large)...
	e.record(1000, 60_000, 21_000) // rtt 20000, offset = 60000-11000 = 49000
	// ...then a fast one (the truthful sample).
	e.record(30_000, 81_000, 32_000) // rtt 2000, offset = 81000-31000 = 50000
	off, rtt, ok := e.best()
	if !ok || off != 50_000 || rtt != 2000 {
		t.Fatalf("best = (%d, %d, %v), want (50000, 2000, true)", off, rtt, ok)
	}

	// Bogus exchange (t1 < t0) is ignored.
	e.record(5000, 1, 4000)
	if off2, _, _ := e.best(); off2 != 50_000 {
		t.Fatalf("bogus exchange changed the estimate: %d", off2)
	}

	// The window keeps only the last 8 samples: flood with slow samples and
	// the old fast one ages out.
	for i := range 8 {
		base := uint64(100_000 + i*10_000)
		e.record(base, base+500_000, base+9000) // rtt 9000
	}
	_, rtt, _ = e.best()
	if rtt != 9000 {
		t.Fatalf("windowed best rtt = %d, want 9000 (old fast sample aged out)", rtt)
	}
}

// The per-hop rewrite composes broadcaster↔origin + origin↔edge into
// broadcaster↔edge (docs/22 Decision 12): originUs = tsUs + X and
// originUs = edgeUs + est ⇒ edgeUs = tsUs + (X − est).
func TestClockMappingRewriteComposition(t *testing.T) {
	est := &timeSyncEstimator{}
	mapping := wire.AppendClockMapping(nil, 7_000_000) // X: broadcaster→origin

	// No estimator sample yet: withhold — an arbitrary inter-pod epoch gap
	// must never be served as latency truth.
	if _, ok := rewriteClockMapping(mapping, est); ok {
		t.Fatal("mapping served before the estimator had a sample")
	}

	// origin ≈ edge + 2_000_000 (symmetric exchange: offset exact).
	e0 := uint64(10_000_000)
	est.record(e0, 12_000_500, e0+1000)
	rewritten, ok := rewriteClockMapping(mapping, est)
	if !ok {
		t.Fatal("rewrite failed with a sample present")
	}
	got, err := wire.ParseClockMapping(rewritten)
	if err != nil {
		t.Fatalf("rewritten mapping unparseable: %v", err)
	}
	if got != 7_000_000-2_000_000 {
		t.Fatalf("rewritten offset = %d, want %d", got, 5_000_000)
	}

	// Malformed input: dropped.
	if _, ok := rewriteClockMapping([]byte{0x01, 0x06, 0x00}, est); ok {
		t.Fatal("malformed mapping rewritten")
	}
}

// ---- edge session lifecycle against fake upstreams ------------------------

type fakeUpstream struct {
	mu      sync.Mutex
	sent    [][]byte // datagrams the edge sent (TimeSync pings)
	dgrams  chan []byte
	streams chan io.Reader
	closed  chan struct{}
	once    sync.Once
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		dgrams:  make(chan []byte, 64),
		streams: make(chan io.Reader, 8),
		closed:  make(chan struct{}),
	}
}

func (f *fakeUpstream) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-f.dgrams:
		return d, nil
	case <-f.closed:
		return nil, errors.New("upstream closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeUpstream) SendDatagram(p []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, append([]byte(nil), p...))
	f.mu.Unlock()
	return nil
}

func (f *fakeUpstream) AcceptUniStream(ctx context.Context) (io.Reader, error) {
	select {
	case s := <-f.streams:
		return s, nil
	case <-f.closed:
		return nil, errors.New("upstream closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeUpstream) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// fakeResolver serves a switchable Origin.
type fakeResolver struct {
	mu     sync.Mutex
	origin cluster.Origin
	err    error
}

func (r *fakeResolver) Resolve(context.Context, string) (cluster.Origin, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.origin, r.err
}

func (r *fakeResolver) set(o cluster.Origin, err error) {
	r.mu.Lock()
	r.origin = o
	r.err = err
	r.mu.Unlock()
}

// edgeConn is a minimal hub.Conn recording keyframe deliveries.
type edgeConn struct {
	mu        sync.Mutex
	keyframes [][]byte
	dgrams    [][]byte
	closeCode uint32
	closed    bool
}

func (c *edgeConn) SendDatagram(d []byte) error {
	c.mu.Lock()
	c.dgrams = append(c.dgrams, append([]byte(nil), d...))
	c.mu.Unlock()
	return nil
}

func (c *edgeConn) OpenKeyframeStream() (hub.KeyframeStream, error) {
	return &edgeConnStream{conn: c}, nil
}

func (c *edgeConn) CloseWithError(code uint32, _ string) error {
	c.mu.Lock()
	c.closeCode = code
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *edgeConn) keyframeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.keyframes)
}

func (c *edgeConn) closeInfo() (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCode, c.closed
}

type edgeConnStream struct {
	conn *edgeConn
	buf  bytes.Buffer
}

func (s *edgeConnStream) SetWriteDeadline(time.Time) error { return nil }
func (s *edgeConnStream) Write(p []byte) (int, error)      { return s.buf.Write(p) }
func (s *edgeConnStream) Close() error {
	s.conn.mu.Lock()
	s.conn.keyframes = append(s.conn.keyframes, append([]byte(nil), s.buf.Bytes()...))
	s.conn.mu.Unlock()
	return nil
}
func (s *edgeConnStream) CancelWrite() {}

type edgeHarness struct {
	registry *hub.Registry
	resolver *fakeResolver
	manager  *EdgeManager
	mu       sync.Mutex
	queued   []*fakeUpstream
	dials    int
}

func newEdgeHarness(t *testing.T) *edgeHarness {
	t.Helper()
	h := &edgeHarness{
		registry: hub.NewRegistry(discardLog, hub.Options{}),
		resolver: &fakeResolver{},
	}
	h.resolver.set(cluster.Origin{Holder: "pod-origin", Addr: "10.0.0.9:4433", Generation: 3}, nil)
	dial := func(ctx context.Context, addr, path string) (edgeUpstream, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.dials++
		if len(h.queued) == 0 {
			return nil, errors.New("no upstream queued")
		}
		up := h.queued[0]
		h.queued = h.queued[1:]
		return up, nil
	}
	h.manager = newEdgeManager(h.registry, h.resolver, dial, "pod-self", discardLog)
	t.Cleanup(h.manager.Stop)
	return h
}

func (h *edgeHarness) queue(ups ...*fakeUpstream) {
	h.mu.Lock()
	h.queued = append(h.queued, ups...)
	h.mu.Unlock()
}

func (h *edgeHarness) dialCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dials
}

// The core staleness guarantee: an upstream drop invalidates the prime
// caches immediately, a viewer joining before the re-attach gets NO stale
// prime, and the re-attach re-primes through the fresh join-prime.
func TestEdgePrimeInvalidationAcrossReattach(t *testing.T) {
	h := newEdgeHarness(t)
	up1, up2 := newFakeUpstream(), newFakeUpstream()
	h.queue(up1, up2)
	ctx := context.Background()

	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("EnsureEdge: %v", err)
	}

	// Origin A's keyframe arrives (join-prime): cached + hub live.
	kfA := buildStreamKeyframe(t, 7, "avc1.42E02A", 900)
	up1.streams <- bytes.NewReader(kfA)
	waitFor(t, 5*time.Second, func() bool {
		return h.registry.Stats().Broadcasts[h.registry.ObfuscateID("K7XQ2M")].CachedKeyframeBytes == len(kfA)
	}, "keyframe cached from upstream 1")

	// A viewer joining now is primed with it.
	v1 := &edgeConn{}
	s1, err := h.registry.Subscribe("K7XQ2M", v1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s1.Close()
	waitFor(t, 5*time.Second, func() bool { return v1.keyframeCount() == 1 }, "viewer 1 primed")

	// Upstream dies (origin drained / moved): the caches must be gone BEFORE
	// the re-attach, so a viewer joining in the gap waits instead of getting
	// origin A's prime against origin B's future deltas.
	up1.Close()
	waitFor(t, 5*time.Second, func() bool {
		st := h.registry.Stats().Broadcasts[h.registry.ObfuscateID("K7XQ2M")]
		return st.CachedKeyframeBytes == 0 && !st.HasConfig
	}, "primes invalidated on upstream loss")

	v2 := &edgeConn{}
	s2, err := h.registry.Subscribe("K7XQ2M", v2)
	if err != nil {
		t.Fatalf("Subscribe (gap viewer): %v", err)
	}
	defer s2.Close()
	time.Sleep(100 * time.Millisecond)
	if v2.keyframeCount() != 0 {
		t.Fatal("gap viewer was served a stale prime")
	}

	// Re-attach (upstream 2 = origin B): a fresh keyframe re-primes and the
	// gap viewer receives THAT one, byte-identical.
	kfB := buildStreamKeyframe(t, 0, "vp09.00.40.08", 700)
	waitFor(t, 5*time.Second, func() bool { return h.dialCount() >= 2 }, "re-attach dial")
	up2.streams <- bytes.NewReader(kfB)
	waitFor(t, 5*time.Second, func() bool { return v2.keyframeCount() == 1 }, "gap viewer served the fresh keyframe")
	v2.mu.Lock()
	got := v2.keyframes[0]
	v2.mu.Unlock()
	if !bytes.Equal(got, kfB) {
		t.Fatal("re-primed keyframe not byte-identical to origin B's")
	}
}

// Datagrams re-ingest verbatim; ClockMappings are rewritten (or withheld
// until the estimator has a sample); TimeSync pings flow upstream.
func TestEdgePumpRewritesClockMappingAndForwardsVerbatim(t *testing.T) {
	h := newEdgeHarness(t)
	up := newFakeUpstream()
	h.queue(up)
	ctx := context.Background()

	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("EnsureEdge: %v", err)
	}
	v := &edgeConn{}
	sub, err := h.registry.Subscribe("K7XQ2M", v)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// The primed mapping arrives BEFORE any pong: withheld, then emitted
	// once the estimator can translate it.
	up.dgrams <- wire.AppendClockMapping(nil, 7_000_000)

	// Answer the edge's first ping with a known origin clock: offset ≈ 2s.
	waitFor(t, 5*time.Second, func() bool {
		up.mu.Lock()
		defer up.mu.Unlock()
		return len(up.sent) >= 1
	}, "edge ping sent")
	up.mu.Lock()
	t0, _, err := wire.ParseTimeSync(up.sent[0])
	up.mu.Unlock()
	if err != nil {
		t.Fatalf("edge ping unparseable: %v", err)
	}
	up.dgrams <- wire.AppendTimeSync(nil, t0, t0+2_000_000)

	// A delta datagram forwards byte-identical.
	delta := encodeFrame(t, 42, false, 3)[0]
	up.dgrams <- delta

	waitFor(t, 5*time.Second, func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return len(v.dgrams) >= 2
	}, "viewer received mapping + delta")

	v.mu.Lock()
	defer v.mu.Unlock()
	var sawDelta, sawMapping bool
	for _, d := range v.dgrams {
		if d[1] == wire.TypeVideoChunk {
			sawDelta = true
			if !bytes.Equal(d, delta) {
				t.Error("delta datagram not byte-identical across the hop")
			}
		}
		if d[1] == wire.TypeClockMapping {
			sawMapping = true
			off, err := wire.ParseClockMapping(d)
			if err != nil {
				t.Fatalf("forwarded mapping unparseable: %v", err)
			}
			// X − est: 7_000_000 − ~2_000_000. The estimator error is
			// bounded by the exchange's rtt/2 (the fake exchange is fast but
			// t1 is real wall-clock µs after t0) — allow a generous ±50 ms.
			if off < 4_900_000 || off > 5_100_000 {
				t.Errorf("rewritten mapping offset = %d, want ≈5_000_000", off)
			}
		}
	}
	if !sawDelta || !sawMapping {
		t.Fatalf("viewer datagrams missing kinds: delta=%v mapping=%v", sawDelta, sawMapping)
	}
}

// Lease deletion tears the edge down and 4000-closes its local viewers.
func TestEdgeLeaseDeletionEndsViewers(t *testing.T) {
	h := newEdgeHarness(t)
	up := newFakeUpstream()
	h.queue(up)
	ctx := context.Background()

	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("EnsureEdge: %v", err)
	}
	v := &edgeConn{}
	if _, err := h.registry.Subscribe("K7XQ2M", v); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	h.resolver.set(cluster.Origin{}, cluster.ErrNotFound)
	h.manager.OnLeaseDeleted("K7XQ2M")
	h.registry.EndBroadcast("K7XQ2M")

	code, closed := v.closeInfo()
	if !closed || code != uint32(wire.CloseCodeBroadcastEnded) {
		t.Fatalf("viewer close = (%d, %v), want (4000, true)", code, closed)
	}
	if err := h.registry.CheckSubscribe("K7XQ2M"); !errors.Is(err, hub.ErrNotFound) {
		t.Errorf("edge hub survived lease deletion: %v", err)
	}
}

// Self-resolve short-circuit (guard 3): a lease naming this pod, or an
// origin in flux (empty holder), never dials.
func TestEnsureEdgeSelfAndFluxShortCircuit(t *testing.T) {
	h := newEdgeHarness(t)
	ctx := context.Background()

	h.resolver.set(cluster.Origin{Holder: "pod-self", Addr: "10.0.0.1:4433", Generation: 1}, nil)
	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); !errors.Is(err, hub.ErrNotFound) {
		t.Fatalf("self-resolve EnsureEdge = %v, want ErrNotFound", err)
	}
	h.resolver.set(cluster.Origin{Holder: "", Addr: "10.0.0.2:4433"}, nil)
	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); !errors.Is(err, hub.ErrNotFound) {
		t.Fatalf("empty-holder EnsureEdge = %v, want ErrNotFound", err)
	}
	h.resolver.set(cluster.Origin{}, cluster.ErrNotFound)
	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); !errors.Is(err, hub.ErrNotFound) {
		t.Fatalf("no-lease EnsureEdge = %v, want ErrNotFound", err)
	}
	if h.dialCount() != 0 {
		t.Fatalf("short-circuited resolves still dialed %d times", h.dialCount())
	}
}

// ---- internal route guards (pre-upgrade, plain statuses) -------------------

// fakeCoordinator drives handleInternalSubscribe's fencing.
type fakeCoordinator struct {
	heldID  string
	heldGen int64
}

func (f *fakeCoordinator) Claim(context.Context, string, bool) (int64, error) { return 0, nil }
func (f *fakeCoordinator) ReleaseAll(context.Context)                         {}
func (f *fakeCoordinator) Resolve(context.Context, string) (cluster.Origin, error) {
	return cluster.Origin{}, cluster.ErrNotFound
}
func (f *fakeCoordinator) OriginGeneration(id string) (int64, bool) {
	if id == f.heldID {
		return f.heldGen, true
	}
	return 0, false
}

func TestInternalSubscribeGuards(t *testing.T) {
	cfg := config.Config{Addr: "127.0.0.1:0", InternalPSK: "fleet-psk", InternalServerName: "relay.example"}
	srv := New(cfg, hub.NewRegistry(discardLog, hub.Options{}), nil, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))

	req := func(id, query string) *http.Request {
		r := httptest.NewRequest(http.MethodConnect, "https://relay/internal/subscribe/"+id+"?"+query, nil)
		r.URL = &url.URL{Path: "/internal/subscribe/" + id, RawQuery: query}
		r.SetPathValue("id", id)
		return r
	}

	// Cluster mode off: the route does not exist (404), PSK or not.
	w := httptest.NewRecorder()
	srv.handleInternalSubscribe(w, req("K7XQ2M", "psk=fleet-psk&gen=1&proto=1"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("cluster-off status = %d, want 404", w.Code)
	}

	srv.SetCluster(&fakeCoordinator{heldID: "K7XQ2M", heldGen: 3}, "pod-self")

	cases := []struct {
		name  string
		id    string
		query string
		want  int
	}{
		{"bad psk", "K7XQ2M", "psk=wrong&gen=3&proto=1", http.StatusUnauthorized},
		{"missing psk", "K7XQ2M", "gen=3&proto=1", http.StatusUnauthorized},
		{"protocol skew", "K7XQ2M", "psk=fleet-psk&gen=3&proto=2", http.StatusUpgradeRequired},
		{"not origin", "ABC234", "psk=fleet-psk&gen=3&proto=1", http.StatusNotFound},
		{"stale generation", "K7XQ2M", "psk=fleet-psk&gen=2&proto=1", http.StatusConflict},
		{"garbage generation", "K7XQ2M", "psk=fleet-psk&gen=x&proto=1", http.StatusConflict},
		{"malformed id", "!!!", "psk=fleet-psk&gen=3&proto=1", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.handleInternalSubscribe(w, req(tc.id, tc.query))
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

// R17 post-review fix (PR #47): a lingered-out edge pull must DELETE its
// derived hub, not leave it idling through the ordinary 5-minute grace — a
// viewer joining that window would attach to a hub with no upstream pull
// behind it (handleSubscribe runs EnsureEdge only on ErrNotFound) and
// eventually receive a wrong terminal 4000 while the broadcast is still live
// at the origin. Docs/22 Decision 10: the Lease is the liveness truth; edge
// hubs never idle in grace.
func TestEdgeLingerOutDeletesDerivedHub(t *testing.T) {
	h := newEdgeHarness(t)
	h.manager.linger = time.Millisecond // linger out at the first viewerless check
	up := newFakeUpstream()
	h.queue(up)
	ctx := context.Background()

	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("EnsureEdge: %v", err)
	}
	if err := h.registry.CheckSubscribe("K7XQ2M"); err != nil {
		t.Fatalf("edge hub not live after attach: %v", err)
	}

	// No viewer ever joins: the pull lingers out (the viewerless check ticks
	// at 1 Hz) and the derived hub must be gone WITH it — ErrNotFound is what
	// routes the next viewer through EnsureEdge for a fresh pull.
	waitFor(t, 10*time.Second, func() bool {
		return errors.Is(h.registry.CheckSubscribe("K7XQ2M"), hub.ErrNotFound)
	}, "derived hub deleted on linger-out")

	// And the next viewer demand-creates a fresh pull as usual.
	h.queue(newFakeUpstream())
	if err := h.manager.EnsureEdge(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("EnsureEdge after linger-out: %v", err)
	}
	if h.dialCount() != 2 {
		t.Errorf("dials = %d, want 2 (fresh pull after linger-out)", h.dialCount())
	}
}
