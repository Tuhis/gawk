// Edge pull (docs/22): when a viewer lands on a pod that is not a
// broadcast's origin, the pod subscribes upstream — dialing the origin's pod
// IP from the Lease, never the Service VIP (which could loop back) — and
// re-ingests everything into a local edge hub through the ordinary Publisher
// surface, datagrams and keyframe streams verbatim, so store-and-forward and
// supersede compose per hop.
//
// Only the ClockMapping is rewritten: each pod has its own monotonic clock, so
// the edge runs a TimeSync estimator against the origin and translates the
// mapping's offset into edge-clock terms.
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
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// internalEdgeOrigin is the Origin header on pod-to-pod /internal/subscribe
// dials. CheckOrigin honors it only on the PSK-gated /internal/* routes
// (browsers can't send it anyway). Keep it stable across versions: during a
// rolling update, old pods dial new pods with it.
const internalEdgeOrigin = "gawk-server://native-internal-edge"

// Internal-session QUIC timing: in-cluster keepalives are cheap, and a dead
// origin must be detected fast even without the lease watch.
const (
	edgeIdleTimeout   = 4 * time.Second
	edgeKeepAlive     = 1 * time.Second
	edgePingInterval  = 2 * time.Second
	edgeLingerDefault = 15 * time.Second
	// Re-attach backoff: full jitter, capped (the herd is bounded by pod count).
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

// rewriteClockMapping translates a ClockMapping from origin-clock into
// edge-clock terms: the mapping says originUs = tsUs + X and the estimator
// originUs ≈ edgeUs + est, so edgeUs = tsUs + (X − est). ok is false — the
// mapping must be withheld, never served wrong by an arbitrary inter-pod
// epoch difference — until the estimator has a sample, or when malformed.
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
	// CloseError reports why the session ended (the origin's
	// *webtransport.SessionError when it closed with a code), nil while
	// open. The read loops can't be trusted to return it: the first to fail
	// cancels the other, and a datagram read can fail with a bare EOF.
	CloseError() error
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
// disappears (the Lease is the liveness truth — no grace).
type EdgeManager struct {
	registry *hub.Registry
	resolver originResolver
	dial     edgeDialer
	podName  string
	linger   time.Duration
	log      *slog.Logger
	// terminated counts kills this pod's edge pull received as its origin's
	// 4006 (they never pass through Server.terminate). Set by SetCluster;
	// nil in the unit harness.
	terminated func()

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
// (never dial ourselves).
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

// OnLeaseDeleted tears down the broadcast's edge pull (if any); the caller
// (main's dispatch) then ends local viewers with EndBroadcast's 4000.
func (m *EdgeManager) OnLeaseDeleted(broadcastID string) {
	// The origin closes edge sessions with the terminal code before it
	// deletes the Lease, but the informer event can arrive first; stopping
	// the pull at once would end local viewers with a reconstructed 4000
	// for a 4006. Give the pull a bounded chance to end with the origin's code.
	m.mu.Lock()
	es := m.edges[broadcastID]
	m.mu.Unlock()
	if es != nil {
		t := time.NewTimer(edgeEndWait)
		select {
		case <-es.doneCh:
		case <-t.C:
		}
		t.Stop()
	}
	m.StopEdge(broadcastID)
}

// edgeEndWait bounds OnLeaseDeleted's wait. It only elapses when the upstream
// is gone without a close (a dead origin's stale Lease was reaped), and it
// holds up the lease informer, so it stays short.
const edgeEndWait = 500 * time.Millisecond

// StopEdge synchronously stops the broadcast's edge pull, if any (lease
// deletion, or the real broadcaster claiming the hub's publisher slot).
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
			// We became the origin — this pull is obsolete.
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
				// The slot is briefly held (a demote racing the old publisher's
				// teardown); it frees once that handler returns.
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

		lingered, upErr := es.pump(up, pub)

		// Upstream ended or we lingered out. The prime caches die with the
		// session: a viewer joining before the re-attach must never get origin
		// A's keyframe against origin B's deltas.
		pub.Close()
		es.m.registry.InvalidatePrimes(es.id)
		up.Close()
		if lingered {
			// Linger-out: expire the hub atomically, only if still viewer-less.
			// Left in grace it would keep satisfying CheckSubscribe, and a viewer
			// joining then would attach with no pull behind it and end at a wrong
			// 4000. A viewer that raced the linger keeps it — re-attach instead.
			if !es.m.registry.ExpireEdgeIfViewerless(es.id) && es.ctx.Err() == nil {
				attempt = 0
				continue
			}
			break
		}
		// The origin ended the broadcast and said why: pass its code on.
		// Local state can't tell a kill (an IP ban names no ID here; an ID
		// ban may not have reached this pod yet) and would say 4000 for 4006.
		if code, ok := upstreamTerminalCode(upErr); ok && es.upstreamEndIsFinal(code, origin) {
			if code == wire.CloseCodeTerminatedByOperator {
				// Counted only if this removed the hub: a Ban event that got
				// here first went through Server.terminate, which counted it.
				if es.m.registry.TerminateBroadcast(es.id, code, terminationReason) && es.m.terminated != nil {
					es.m.terminated()
				}
			} else {
				es.m.registry.EndBroadcast(es.id)
			}
			es.m.log.Info("edge ended with the origin's close code", "broadcast_id", es.id, "close_code", code)
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
		// Lease deletion means "broadcast ended": close local viewers with
		// 4000 (EndBroadcast skips live hubs; ours is publisher-less now).
		es.m.registry.EndBroadcast(es.id)
	}
}

// upstreamEndIsFinal decides whether the origin's terminal close ends this
// pod's copy too. 4006 always does (banned fleet-wide). 4000 does unless the
// Lease now names another holder or a newer generation — a re-home, so the
// pull re-attaches instead of ending a broadcast that is still live.
func (es *edgeSession) upstreamEndIsFinal(code uint32, attached cluster.Origin) bool {
	if code == wire.CloseCodeTerminatedByOperator {
		return true
	}
	now, err := es.m.resolver.Resolve(es.ctx, es.id)
	if errors.Is(err, cluster.ErrNotFound) {
		return true
	}
	if err != nil {
		return false
	}
	return now.Holder == attached.Holder && now.Generation == attached.Generation
}

// edgeBackoffDuration is base·2^attempt with full jitter, capped. Pure, for
// the unit test; backoff() does the sleeping.
func edgeBackoffDuration(attempt int) time.Duration {
	d := edgeRetryBase << min(attempt, 3)
	if d > edgeRetryCap {
		d = edgeRetryCap
	}
	return time.Duration(rand.Int64N(int64(d))) + edgeRetryBase/2
}

// backoff sleeps one jittered retry delay; false means ctx is done.
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
func (es *edgeSession) pump(up edgeUpstream, pub *hub.Publisher) (lingered bool, upstreamErr error) {
	ctx, cancel := context.WithCancel(es.ctx)
	defer cancel()

	// How the upstream ended: both loops end together on a close, but only
	// one sees the *webtransport.SessionError (the other may get a bare
	// EOF), so a session error wins. Our own cancels are context errors.
	var endMu sync.Mutex
	recordEnd := func(err error) {
		endMu.Lock()
		defer endMu.Unlock()
		var se *webtransport.SessionError
		if upstreamErr == nil || errors.As(err, &se) {
			upstreamErr = err
		}
	}

	est := &timeSyncEstimator{}
	// The origin's cached ClockMapping is join-primed at attach, usually
	// before the first TimeSync pong: hold the newest one and emit it once
	// the estimator can translate it.
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
				recordEnd(err)
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
				recordEnd(err)
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

	// TimeSync pings (2 s, as the TS client) + linger check + viewer-count report.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ping := time.NewTicker(edgePingInterval)
		defer ping.Stop()
		lingerTick := time.NewTicker(time.Second)
		defer lingerTick.Stop()
		var viewerlessSince time.Time
		var reported bool
		var lastReport uint32
		var lastReportAt time.Time
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
				// Two counts on purpose: linger counts every external session (an
				// edge serving only stripe legs still serves media); the upstream
				// report counts watching humans, excluding legs.
				n := es.m.registry.ExternalSubscribers(es.id)
				// Change-driven plus keepalive, so a lost report heals on re-send.
				// The count is bounded by the subscriber caps, far below uint32.
				count := uint32(es.m.registry.ViewerSubscribers(es.id))
				if !reported || count != lastReport || time.Since(lastReportAt) >= hub.ViewerCountKeepalive {
					if up.SendDatagram(wire.AppendViewerCount(nil, count)) == nil {
						reported = true
						lastReport = count
						lastReportAt = time.Now()
					}
				}
				if n > 0 {
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
	endMu.Lock()
	defer endMu.Unlock()
	select {
	case <-lingerCh:
		return true, upstreamErr
	default:
	}
	var se *webtransport.SessionError
	if !errors.As(upstreamErr, &se) {
		if ce := up.CloseError(); ce != nil {
			upstreamErr = ce
		}
	}
	return false, upstreamErr
}

// upstreamTerminalCode reports the origin's close code when it means the
// broadcast is over — codes a downstream pod passes on to its viewers rather
// than re-attach through.
func upstreamTerminalCode(err error) (uint32, bool) {
	var se *webtransport.SessionError
	if !errors.As(err, &se) || !se.Remote {
		return 0, false
	}
	switch code := uint32(se.ErrorCode); code {
	case wire.CloseCodeBroadcastEnded, wire.CloseCodeTerminatedByOperator:
		return code, true
	}
	return 0, false
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
	dialer *webtransport.Transport
}

func (u *webtransportUpstream) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return u.sess.ReceiveDatagram(ctx)
}

func (u *webtransportUpstream) SendDatagram(p []byte) error { return u.sess.SendDatagram(p) }

func (u *webtransportUpstream) AcceptUniStream(ctx context.Context) (io.Reader, error) {
	return u.sess.AcceptUniStream(ctx)
}

// closeErrorWait bounds CloseError's wait for a session that is ending to
// finish closing (the loops can fail a moment before the close capsule is
// processed).
const closeErrorWait = 100 * time.Millisecond

func (u *webtransportUpstream) CloseError() error {
	t := time.NewTimer(closeErrorWait)
	defer t.Stop()
	select {
	case <-u.sess.Context().Done():
	case <-t.C:
		return nil // still open: our own cancel ended the pump
	}
	// A closed session's stream map holds its close error; any streams
	// still queued ahead of it are accepted and dropped.
	ctx, cancel := context.WithTimeout(context.Background(), closeErrorWait)
	defer cancel()
	for {
		if _, err := u.sess.AcceptUniStream(ctx); err != nil {
			return err
		}
	}
}

func (u *webtransportUpstream) Close() error {
	err := u.sess.CloseWithError(0, "edge detaching")
	_ = u.dialer.Close()
	return err
}

// newEdgeDialer builds the production dialer: TLS against the public cert
// hostname (the lease addr is a raw pod IP — no per-pod certs, no
// InsecureSkipVerify), tight in-cluster QUIC timers, PSK appended here.
func newEdgeDialer(serverName, psk string, rootCAs *x509.CertPool, log *slog.Logger) edgeDialer {
	return func(ctx context.Context, addr, path string) (edgeUpstream, error) {
		d := &webtransport.Transport{
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

		// CheckOrigin accepts internalEdgeOrigin on /internal/* only, so no
		// -allowed-origins entry is needed and the origin check stays live on
		// the internal route for anything else.
		rsp, sess, err := d.Dial(ctx, target, http.Header{"Origin": []string{internalEdgeOrigin}})
		if err != nil {
			_ = d.Close()
			// Unlike a browser, Go can read the HTTP status: surface it (404
			// not-origin / 409 stale generation / 401 bad PSK / 426 version skew).
			if rsp != nil {
				return nil, fmt.Errorf("internal subscribe to %s: status %d: %w", addr, rsp.StatusCode, err)
			}
			return nil, err
		}
		return &webtransportUpstream{sess: sess, dialer: d}, nil
	}
}
