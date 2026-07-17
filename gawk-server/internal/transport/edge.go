// Edge pull (R17 W4, docs/22 Decisions 9/10/12): when a viewer lands on a
// pod that is not a broadcast's origin, the pod subscribes upstream — dialing
// the origin's POD IP from the Lease (never the Service VIP: guard 1 against
// loops) over the same WebTransport wire protocol — and re-ingests everything
// into a local EDGE hub through the ordinary Publisher surface: datagrams
// verbatim, keyframe streams byte-identical, so store-and-forward + supersede
// compose per hop. Local viewers attach to that hub exactly as they would on
// the origin.
//
// The one thing that is NOT forwarded verbatim is the ClockMapping (Decision
// 12): each pod keeps its own monotonic clock, so the edge runs a Go port of
// the client TimeSync estimator over its upstream session and rewrites the
// mapping's offset from origin-clock terms into edge-clock terms before it
// reaches local viewers.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Internal-session QUIC timing (docs/22 Decision 10): in-cluster keepalives
// are cheap, and a dead origin must be detected fast even without the lease
// watch. Constants, not knobs.
const (
	edgeIdleTimeout   = 4 * time.Second
	edgeKeepAlive     = 1 * time.Second
	edgePingInterval  = 2 * time.Second
	edgeLingerDefault = 15 * time.Second
	// Re-attach backoff: base with full jitter, capped — the herd is bounded
	// by pod count (single digits), jitter just de-synchronizes it (W5).
	edgeRetryBase = 250 * time.Millisecond
	edgeRetryCap  = 2 * time.Second
	// How long EnsureEdge waits for the first upstream attach before the
	// viewer's subscribe is failed (the viewer retries on its ladder).
	edgeAttachTimeout = 5 * time.Second
)

// timeSyncEstimator is the Go port of the client TimeSyncEstimator
// (gawk-app/src/transport/time-sync.ts): NTP-style samples against the
// upstream origin's clock, lowest-RTT-of-8 wins (the fastest exchange is the
// most symmetric one; error ≈ that sample's rtt/2).
type timeSyncEstimator struct {
	mu      sync.Mutex
	samples []tsSample
}

type tsSample struct {
	offsetUs int64
	rttUs    uint64
}

const timeSyncSampleWindow = 8

// record ingests one echoed exchange: t0 = local send time, serverTimeUs =
// origin clock at reply, t1 = local receive time (all µs; t0/t1 on this
// pod's monotonic clock). originUs ≈ localUs + offsetUs.
func (e *timeSyncEstimator) record(t0, serverTimeUs, t1 uint64) {
	if t1 < t0 {
		return // impossible exchange (bogus/forged echo)
	}
	rtt := t1 - t0
	offset := int64(serverTimeUs) - int64(t0+rtt/2)
	e.mu.Lock()
	e.samples = append(e.samples, tsSample{offsetUs: offset, rttUs: rtt})
	if len(e.samples) > timeSyncSampleWindow {
		e.samples = e.samples[1:]
	}
	e.mu.Unlock()
}

// best returns the lowest-RTT sample's offset; ok is false before the first
// sample.
func (e *timeSyncEstimator) best() (offsetUs int64, rttUs uint64, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.samples) == 0 {
		return 0, 0, false
	}
	b := e.samples[0]
	for _, s := range e.samples[1:] {
		if s.rttUs < b.rttUs {
			b = s
		}
	}
	return b.offsetUs, b.rttUs, true
}

// rewriteClockMapping translates a ClockMapping from origin-clock terms into
// edge-clock terms (docs/22 Decision 12). Derivation of the sign: the
// incoming mapping says originUs = tsUs + X; the estimator says
// originUs ≈ edgeUs + est; so edgeUs = tsUs + (X − est) — the offsets
// compose broadcaster↔origin + origin↔edge = broadcaster↔edge. ok is false
// (mapping must be withheld, never served wrong by an arbitrary inter-pod
// epoch difference) until the estimator has a sample or when the datagram is
// malformed.
func rewriteClockMapping(dgram []byte, est *timeSyncEstimator) ([]byte, bool) {
	x, err := wire.ParseClockMapping(dgram)
	if err != nil {
		return nil, false
	}
	off, _, ok := est.best()
	if !ok {
		return nil, false
	}
	return wire.AppendClockMapping(nil, x-off), true
}

// edgeUpstream is the slice of an upstream WebTransport session the pump
// needs; narrowed so tests can drive the whole edge lifecycle with fakes.
type edgeUpstream interface {
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	SendDatagram(payload []byte) error
	AcceptUniStream(ctx context.Context) (io.Reader, error)
	Close() error
}

// edgeDialer establishes one upstream session to an origin pod. addr is the
// lease's pod address; path carries the internal route + auth/fencing params.
type edgeDialer func(ctx context.Context, addr, path string) (edgeUpstream, error)

// originResolver is the EdgeManager's slice of the cluster coordinator.
type originResolver interface {
	Resolve(ctx context.Context, broadcastID string) (cluster.Origin, error)
}

// EdgeManager owns this pod's edge pulls: at most one upstream session per
// broadcast, demand-created when a viewer asks for a hub we don't have,
// lingering ~15 s past the last local viewer, and torn down when the lease
// disappears (the Lease is the liveness truth — no grace, Decision 10).
type EdgeManager struct {
	registry *hub.Registry
	resolver originResolver
	dial     edgeDialer
	podName  string
	linger   time.Duration
	log      *slog.Logger

	baseCtx    context.Context
	cancelBase context.CancelFunc

	mu    sync.Mutex
	edges map[string]*edgeSession
}

func newEdgeManager(registry *hub.Registry, resolver originResolver, dial edgeDialer, podName string, log *slog.Logger) *EdgeManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &EdgeManager{
		registry:   registry,
		resolver:   resolver,
		dial:       dial,
		podName:    podName,
		linger:     edgeLingerDefault,
		log:        log,
		baseCtx:    ctx,
		cancelBase: cancel,
		edges:      make(map[string]*edgeSession),
	}
}

// Stop tears down every edge pull (server shutdown).
func (m *EdgeManager) Stop() {
	m.cancelBase()
}

// EnsureEdge makes sure an edge pull is running for the broadcast and its
// local hub exists, blocking (bounded) until the first upstream attach. A
// hub.ErrNotFound return maps to the viewer's 404: no lease, an origin in
// flux (empty holder mid-re-home), or a stale lease naming this very pod
// (guard 3: never dial ourselves).
func (m *EdgeManager) EnsureEdge(ctx context.Context, broadcastID string) error {
	origin, err := m.resolver.Resolve(ctx, broadcastID)
	if err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			return hub.ErrNotFound
		}
		return err
	}
	if origin.Holder == m.podName || origin.Holder == "" {
		return hub.ErrNotFound
	}

	m.mu.Lock()
	es := m.edges[broadcastID]
	if es == nil || es.done() {
		es = newEdgeSession(m, broadcastID)
		m.edges[broadcastID] = es
		go es.run()
	}
	m.mu.Unlock()

	return es.awaitAttached(ctx)
}

// OnLeaseDeleted tears down the broadcast's edge pull (if any) so its local
// viewers get the terminal 4000 from the registry's EndBroadcast — which the
// caller (main's OnLeaseDeleted dispatch) invokes right after this.
func (m *EdgeManager) OnLeaseDeleted(broadcastID string) {
	m.StopEdge(broadcastID)
}

// StopEdge synchronously stops the broadcast's edge pull, if any (lease
// deletion, or the W5 come-home: the real broadcaster claiming this pod
// needs the hub's publisher slot our upstream pull is holding).
func (m *EdgeManager) StopEdge(broadcastID string) {
	m.mu.Lock()
	es := m.edges[broadcastID]
	delete(m.edges, broadcastID)
	m.mu.Unlock()
	if es != nil {
		es.stop()
		<-es.doneCh
	}
}

// edgeSession is one broadcast's edge pull: resolve → dial → attach → pump,
// re-attaching (jittered) on upstream loss for as long as local viewers and
// the lease exist.
type edgeSession struct {
	m  *EdgeManager
	id string

	ctx    context.Context
	cancel context.CancelFunc
	doneCh chan struct{}

	attachedOnce sync.Once
	attachedCh   chan struct{}
	attachErr    error // set before attachedCh closes on a failed FIRST attach
}

func newEdgeSession(m *EdgeManager, id string) *edgeSession {
	ctx, cancel := context.WithCancel(m.baseCtx)
	return &edgeSession{
		m:          m,
		id:         id,
		ctx:        ctx,
		cancel:     cancel,
		doneCh:     make(chan struct{}),
		attachedCh: make(chan struct{}),
	}
}

func (es *edgeSession) stop() { es.cancel() }

func (es *edgeSession) done() bool {
	select {
	case <-es.doneCh:
		return true
	default:
		return false
	}
}

// awaitAttached blocks until the first upstream attach (hub exists), the
// first attach failure, or the caller's context/timeout runs out.
func (es *edgeSession) awaitAttached(ctx context.Context) error {
	timer := time.NewTimer(edgeAttachTimeout)
	defer timer.Stop()
	select {
	case <-es.attachedCh:
		return es.attachErr
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return hub.ErrNotFound
	}
}

func (es *edgeSession) signalAttached(err error) {
	es.attachedOnce.Do(func() {
		es.attachErr = err
		close(es.attachedCh)
	})
}

func (es *edgeSession) run() {
	defer close(es.doneCh)
	defer func() {
		es.m.mu.Lock()
		if es.m.edges[es.id] == es {
			delete(es.m.edges, es.id)
		}
		es.m.mu.Unlock()
	}()

	leaseGone := false
	for attempt := 0; ; attempt++ {
		if es.ctx.Err() != nil {
			break
		}
		origin, err := es.m.resolver.Resolve(es.ctx, es.id)
		if errors.Is(err, cluster.ErrNotFound) {
			leaseGone = true
			es.signalAttached(hub.ErrNotFound)
			break
		}
		if err == nil && origin.Holder == es.m.podName {
			// We became the origin (W5 promote) — this pull is obsolete.
			es.signalAttached(nil)
			break
		}
		if err != nil || origin.Holder == "" || origin.Addr == "" {
			es.signalAttached(hub.ErrNotFound)
			if !es.backoff(attempt) {
				break
			}
			continue
		}

		up, err := es.m.dial(es.ctx, origin.Addr, internalSubscribePath(es.id, origin.Generation))
		if err != nil {
			es.m.log.Warn("edge upstream dial failed", "broadcast_id", es.id, "origin", origin.Addr, "err", err)
			es.signalAttached(hub.ErrNotFound)
			if !es.backoff(attempt) {
				break
			}
			continue
		}

		_, pub, err := es.m.registry.EdgePublish(es.id)
		if err != nil {
			up.Close()
			if errors.Is(err, hub.ErrPublisherActive) {
				// The hub's slot is briefly held (a demote racing the old
				// publisher's teardown, W5): back off and retry — the slot
				// frees as soon as that session's handler returns.
				es.m.log.Info("edge hub slot busy; retrying", "broadcast_id", es.id)
				if !es.backoff(attempt) {
					break
				}
				continue
			}
			es.m.log.Warn("edge hub claim failed", "broadcast_id", es.id, "err", err)
			es.signalAttached(err)
			break
		}
		es.signalAttached(nil)
		es.m.log.Info("edge attached", "broadcast_id", es.id, "origin", origin.Addr, "generation", origin.Generation)

		lingered := es.pump(up, pub)

		// Upstream ended (origin drain/crash/4003) or we lingered out. The
		// prime caches die with the session — a viewer joining before the
		// re-attach must wait for the fresh join-prime, never be served
		// origin A's keyframe against origin B's deltas (Decision 10).
		pub.Close()
		es.m.registry.InvalidatePrimes(es.id)
		up.Close()
		if lingered {
			// Linger-out: take the derived hub with us — atomically, and
			// only if it is still viewer-less (post-review fix, PR #47).
			// Left in the ordinary grace, the hub would keep satisfying
			// CheckSubscribe, so a viewer joining that window would attach
			// with no pull behind it and end at a wrong terminal 4000. A
			// viewer that raced the linger window keeps the hub — re-attach
			// for it instead.
			if !es.m.registry.ExpireEdgeIfViewerless(es.id) && es.ctx.Err() == nil {
				attempt = 0
				continue
			}
			break
		}
		if es.ctx.Err() != nil {
			break
		}
		attempt = 0 // a successful attach resets the backoff ladder
		if !es.backoff(attempt) {
			break
		}
	}

	if leaseGone {
		// Lease deletion = cluster-wide "broadcast ended": close any local
		// viewers with the terminal 4000 (EndBroadcast skips live hubs, and
		// ours is publisher-less now).
		es.m.registry.EndBroadcast(es.id)
	}
}

// edgeBackoffDuration: base·2^attempt with full jitter, capped — the W5
// herd de-synchronizer (bounded by pod count, so jitter is all it needs).
// Pure, for the unit test; backoff() below does the sleeping.
func edgeBackoffDuration(attempt int) time.Duration {
	d := edgeRetryBase << min(attempt, 3)
	if d > edgeRetryCap {
		d = edgeRetryCap
	}
	return time.Duration(rand.Int64N(int64(d))) + edgeRetryBase/2
}

// backoff sleeps one jittered retry delay; false = ctx done.
func (es *edgeSession) backoff(attempt int) bool {
	t := time.NewTimer(edgeBackoffDuration(attempt))
	defer t.Stop()
	select {
	case <-es.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pump runs the upstream session's read loops until it dies or the edge
// lingers out (no local viewers for m.linger). Returns true when it stopped
// because of the linger (no re-attach wanted).
func (es *edgeSession) pump(up edgeUpstream, pub *hub.Publisher) (lingered bool) {
	ctx, cancel := context.WithCancel(es.ctx)
	defer cancel()

	est := &timeSyncEstimator{}
	// The origin's cached ClockMapping is join-primed at attach — usually
	// before the first TimeSync pong. Hold the newest un-rewritten mapping
	// and emit it as soon as the estimator can translate it, instead of
	// losing it until the broadcaster's next re-send.
	var pendingMu sync.Mutex
	var pendingMapping []byte

	var wg sync.WaitGroup
	lingerCh := make(chan struct{})

	// Datagram loop: TimeSync replies feed the estimator; ClockMappings are
	// rewritten per hop; everything else re-ingests verbatim.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			dgram, err := up.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if len(dgram) >= 2 && dgram[1] == wire.TypeTimeSync {
				if t0, server, err := wire.ParseTimeSync(dgram); err == nil {
					est.record(t0, server, relayNowUs())
					pendingMu.Lock()
					pm := pendingMapping
					pendingMapping = nil
					pendingMu.Unlock()
					if pm != nil {
						if rewritten, ok := rewriteClockMapping(pm, est); ok {
							pub.HandleDatagram(rewritten)
						}
					}
				}
				continue
			}
			if len(dgram) >= 2 && dgram[1] == wire.TypeClockMapping {
				if rewritten, ok := rewriteClockMapping(dgram, est); ok {
					pub.HandleDatagram(rewritten)
				} else if _, err := wire.ParseClockMapping(dgram); err == nil {
					pendingMu.Lock()
					pendingMapping = append([]byte(nil), dgram...)
					pendingMu.Unlock()
				}
				continue
			}
			pub.HandleDatagram(dgram)
		}
	}()

	// Keyframe streams: read + re-ingest byte-identical (the hub caches and
	// re-fans the exact message bytes).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		sem := make(chan struct{}, maxConcurrentKeyframeStreams)
		for {
			stream, err := up.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			select {
			case sem <- struct{}{}:
			default:
				es.m.log.Warn("edge keyframe stream rejected: too many concurrent", "broadcast_id", es.id)
				continue
			}
			wg.Add(1)
			go func(st io.Reader) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := pub.IngestKeyframeStream(st); err != nil {
					es.m.log.Debug("edge keyframe ingest failed", "broadcast_id", es.id, "err", err)
				}
			}(stream)
		}
	}()

	// TimeSync pings (2 s cadence, mirror of the TS client) + linger check.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ping := time.NewTicker(edgePingInterval)
		defer ping.Stop()
		lingerTick := time.NewTicker(time.Second)
		defer lingerTick.Stop()
		var viewerlessSince time.Time
		// First ping immediately: the sooner the estimator has a sample, the
		// sooner the primed ClockMapping can be served.
		_ = up.SendDatagram(wire.AppendTimeSync(nil, relayNowUs(), 0))
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				_ = up.SendDatagram(wire.AppendTimeSync(nil, relayNowUs(), 0))
			case <-lingerTick.C:
				if es.m.registry.ExternalSubscribers(es.id) > 0 {
					viewerlessSince = time.Time{}
					continue
				}
				if viewerlessSince.IsZero() {
					viewerlessSince = time.Now()
				} else if time.Since(viewerlessSince) >= es.m.linger {
					close(lingerCh)
					cancel()
					return
				}
			}
		}
	}()

	wg.Wait()
	select {
	case <-lingerCh:
		return true
	default:
		return false
	}
}

// internalSubscribePath builds the internal route path WITHOUT the PSK — the
// production dialer appends it (the PSK never travels through logs).
func internalSubscribePath(broadcastID string, generation int64) string {
	return fmt.Sprintf("/internal/subscribe/%s?gen=%d&proto=%d", broadcastID, generation, wire.Version)
}

// webtransportUpstream adapts a dialed *webtransport.Session (plus its
// Dialer, which owns the QUIC transport) to edgeUpstream.
type webtransportUpstream struct {
	sess   *webtransport.Session
	dialer *webtransport.Dialer
}

func (u *webtransportUpstream) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return u.sess.ReceiveDatagram(ctx)
}

func (u *webtransportUpstream) SendDatagram(p []byte) error { return u.sess.SendDatagram(p) }

func (u *webtransportUpstream) AcceptUniStream(ctx context.Context) (io.Reader, error) {
	return u.sess.AcceptUniStream(ctx)
}

func (u *webtransportUpstream) Close() error {
	err := u.sess.CloseWithError(0, "edge detaching")
	_ = u.dialer.Close()
	return err
}

// newEdgeDialer builds the production dialer: TLS against the public cert
// hostname (the lease addr is a raw pod IP — Decision 9: no per-pod certs,
// no InsecureSkipVerify), tight in-cluster QUIC timers, PSK appended here.
func newEdgeDialer(serverName, psk string, rootCAs *x509.CertPool, log *slog.Logger) edgeDialer {
	return func(ctx context.Context, addr, path string) (edgeUpstream, error) {
		d := &webtransport.Dialer{
			TLSClientConfig: &tls.Config{ServerName: serverName, RootCAs: rootCAs},
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
				MaxIdleTimeout:                   edgeIdleTimeout,
				KeepAlivePeriod:                  edgeKeepAlive,
			},
		}
		// QueryEscape so an arbitrary PSK can never break (or smuggle params
		// into) the query string; the origin's Query().Get decodes it back.
		target := "https://" + addr + path + "&psk=" + url.QueryEscape(psk)
		rsp, sess, err := d.Dial(ctx, target, nil)
		if err != nil {
			_ = d.Close()
			// The dialer is Go: HTTP statuses ARE readable here (unlike the
			// browser) — surface them for the ops story (404 not-origin /
			// 409 stale-generation / 401 bad PSK / 426 version skew).
			if rsp != nil {
				return nil, fmt.Errorf("internal subscribe to %s: status %d: %w", addr, rsp.StatusCode, err)
			}
			return nil, err
		}
		return &webtransportUpstream{sess: sess, dialer: d}, nil
	}
}
