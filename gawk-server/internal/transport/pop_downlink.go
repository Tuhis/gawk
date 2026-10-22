package transport

// pop_downlink.go: the PoP side of the R74 downlink (docs/76 §6.6, item
// 23). A party viewer of a home broadcast misses locally on the PoP; while
// the PoP is linked, a second EdgeManager pulls the broadcast from home's
// public endpoint over CONNECT /pop/subscribe/{id}, presenting the link
// credential, into an ordinary edge hub. One leg per broadcast, whatever
// the number of party viewers; the edge rules (join-prime, prime
// invalidation on upstream loss, the 15 s linger, the per-hop ClockMapping
// rewrite, the upward ViewerCount) come with it.
//
// What differs from a fleet pod's edge pull:
//   - the upstream is always home (a resolver with one answer), dialed by
//     name with the system roots (or the PoP's pinned pool), never a pod IP;
//   - the dialer uses the link's QUIC timers (poplink.LinkIdleTimeout /
//     LinkKeepAlive), not edgeIdleTimeout / edgeKeepAlive, which would end
//     the leg and invalidate the prime on any outage over 4 s;
//   - it sizes its receive windows with R73's uplinkReceiveWindows, or a
//     1440p GOP's carrier blocks on flow control (docs/75 D10);
//   - home's refusals are legible statuses (PopLegStatusError): a 404 is the
//     viewer's 404, a 429 (home's per-PoP leg cap) the viewer's 429;
//   - the downlink budget (docs/76 item 24) admits each new pull.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
)

// ErrPopDownlinkFull refuses a party viewer's pull: the PoP's downlink
// budget is spent, or home's per-PoP leg cap refused the leg. The viewer
// gets 429, as for any full broadcast.
var ErrPopDownlinkFull = errors.New("transport: the PoP's downlink is full")

// errPopUnlinked: no link, so no home to pull from (a local miss stays 404).
var errPopUnlinked = errors.New("transport: the PoP is not linked")

// popHomeHolder is the Origin.Holder the PoP's resolver reports for home.
const popHomeHolder = "home"

// PopBudget is a downlink budget; a zero field is unlimited.
type PopBudget struct{ MaxMbps, MaxStreams int }

// PopDownlinkTarget is the PoP's home as the downlink dials it.
type PopDownlinkTarget struct {
	// Endpoint is home's public host:port (from the pairing string).
	Endpoint string
	// Credential is the link credential (Authorization: Bearer).
	Credential string
	// Pop is the PoP's name (?pop=), "" until home has named it.
	Pop string
	// Carrier asks for the carrier leg (?delivery=carrier, docs/76 OD16);
	// false is -pop-leg-delivery=datagram.
	Carrier bool
	// Budget is the effective downlink budget (home's spec.downlink lowered
	// by the host's -downlink-budget).
	Budget PopBudget
	// RootCAs verifies home's certificate (nil: the system roots).
	RootCAs *x509.CertPool
	// Dial replaces the leg dialer (tests); nil dials with the link timers
	// and R73's receive windows.
	Dial PopLegDial
}

// PopDownlinkSource is what the downlink needs from the PoP's link:
// popclient.Client implements it. ok false means not linked.
type PopDownlinkSource interface {
	PopDownlink() (PopDownlinkTarget, bool)
}

// PopLegSession is a dialed leg session: a *webtransport.Session behind
// poplink's narrow CloseWithError.
type PopLegSession interface {
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	SendDatagram(b []byte) error
	AcceptUniStream(ctx context.Context) (*webtransport.ReceiveStream, error)
	Context() context.Context
	CloseWithError(code uint32, msg string) error
}

// PopLegDial dials one leg. A refusal returns home's HTTP response with the
// error.
type PopLegDial func(ctx context.Context, target string, h http.Header) (PopLegSession, *http.Response, error)

// PopLegStatusError is home refusing a leg dial with an HTTP status: 404
// (no such broadcast), 401 / 403 (credential), 426 (proto), 429 (the leg
// cap), 503 (draining).
type PopLegStatusError struct {
	Status int
	Reason string
	Err    error
}

func (e *PopLegStatusError) Error() string {
	return fmt.Sprintf("pop subscribe: status %d %s: %v", e.Status, e.Reason, e.Err)
}

func (e *PopLegStatusError) Unwrap() error { return e.Err }

// PopDownlinkUse is the downlink as the PoP reports it (stats, console).
type PopDownlinkUse struct {
	// Streams is the number of home broadcasts pulled now.
	Streams int
	// Mbps is their measured ingress over the last 10 s.
	Mbps float64
	// LegsOpened counts the leg sessions opened since start.
	LegsOpened uint64
}

type popDownlinkBox struct{ src PopDownlinkSource }

// SetPopDownlink installs the downlink's link source. Without it the
// LocalRelayProvider (SetLocalRelay) is used when it implements
// PopDownlinkSource, as popclient.Client does, so a relay wires the
// downlink by installing its link. Safe while serving.
func (s *Server) SetPopDownlink(src PopDownlinkSource) {
	if src == nil {
		s.popDownSrc.Store(nil)
		return
	}
	s.popDownSrc.Store(&popDownlinkBox{src: src})
}

func (s *Server) popDownlinkSource() PopDownlinkSource {
	if b := s.popDownSrc.Load(); b != nil {
		return b.src
	}
	if b := s.localRelay.Load(); b != nil {
		if src, ok := b.p.(PopDownlinkSource); ok {
			return src
		}
	}
	return nil
}

func (s *Server) popTarget() (PopDownlinkTarget, bool) {
	if src := s.popDownlinkSource(); src != nil {
		return src.PopDownlink()
	}
	return PopDownlinkTarget{}, false
}

// popDownlink is the PoP's downlink: its edge manager and ingress meter.
type popDownlink struct {
	s     *Server
	em    *EdgeManager
	meter *rateMeter
	legs  atomic.Uint64
}

// popDownlinkManager returns the downlink, built on first use; nil when
// this relay has no link source (a home pod, or a plain server).
func (s *Server) popDownlinkManager() *popDownlink {
	if d := s.popDown.Load(); d != nil {
		return d
	}
	if s.popDownlinkSource() == nil {
		return nil
	}
	s.popDownMu.Lock()
	defer s.popDownMu.Unlock()
	if d := s.popDown.Load(); d != nil {
		return d
	}
	d := &popDownlink{s: s, meter: newRateMeter(time.Now)}
	em := newEdgeManager(s.registry, popHomeResolver{s}, d.dial, "pop", s.log.With("component", "pop-downlink"))
	em.upstreamPath = func(id string, _ cluster.Origin) string { return "/pop/subscribe/" + id }
	em.dialRefusal = popDialRefusal
	em.admit = d.admit
	em.standalone = true
	em.terminated = s.metrics.Termination
	d.em = em
	s.popDown.Store(d)
	return d
}

// ensurePopDownlink resolves a party viewer's local miss at home: nil once
// the edge hub exists, hub.ErrNotFound when unlinked or home has no such
// broadcast, ErrPopDownlinkFull over a budget or cap.
func (s *Server) ensurePopDownlink(ctx context.Context, id string) error {
	d := s.popDownlinkManager()
	if d == nil {
		return hub.ErrNotFound
	}
	if _, ok := s.popTarget(); !ok {
		return hub.ErrNotFound
	}
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return hub.ErrNotFound
	}
	return d.em.EnsureEdge(ctx, normID)
}

// PopDownlinkUse reports the downlink (zero without one).
func (s *Server) PopDownlinkUse() PopDownlinkUse {
	d := s.popDown.Load()
	if d == nil {
		return PopDownlinkUse{}
	}
	return PopDownlinkUse{Streams: d.em.Active(), Mbps: d.meter.mbps(), LegsOpened: d.legs.Load()}
}

// stopPopDownlink ends every downlink pull (server shutdown).
func (s *Server) stopPopDownlink() {
	if d := s.popDown.Load(); d != nil {
		d.em.Stop()
	}
}

// admit is the downlink budget (docs/76 item 24): admission, not shaping. A
// new pull is admitted while the pulls running are below MaxStreams and
// their measured 10 s ingress is below MaxMbps. A home broadcast's rate is
// unknown before it is pulled, so the candidate is not added in.
func (d *popDownlink) admit(_ string, active int) error {
	tgt, ok := d.s.popTarget()
	if !ok {
		return hub.ErrNotFound
	}
	b := tgt.Budget
	if b.MaxStreams > 0 && active >= b.MaxStreams {
		return ErrPopDownlinkFull
	}
	if b.MaxMbps > 0 && d.meter.mbps() >= float64(b.MaxMbps) {
		return ErrPopDownlinkFull
	}
	return nil
}

// popHomeResolver is the downlink's only origin: home's endpoint while
// linked. Unlinked is an error, not cluster.ErrNotFound: a pull re-attaching
// through a link flap keeps its viewers rather than ending the broadcast.
type popHomeResolver struct{ s *Server }

func (r popHomeResolver) Resolve(context.Context, string) (cluster.Origin, error) {
	tgt, ok := r.s.popTarget()
	if !ok || tgt.Endpoint == "" {
		return cluster.Origin{}, errPopUnlinked
	}
	return cluster.Origin{Holder: popHomeHolder, Addr: tgt.Endpoint}, nil
}

// popDialRefusal: which of home's refusals end a pull rather than retry.
// 503 (a draining pod) and transport errors retry; every other status is
// final. A home that predates the leg answers 501, which is a 404 here.
func popDialRefusal(err error) (viewerErr error, final bool) {
	var se *PopLegStatusError
	if !errors.As(err, &se) {
		return nil, false
	}
	switch {
	case se.Status == http.StatusTooManyRequests:
		return ErrPopDownlinkFull, true
	case se.Status == http.StatusServiceUnavailable, se.Status >= 500 && se.Status != http.StatusNotImplemented:
		return nil, false
	}
	return hub.ErrNotFound, true
}

// dial is the edge manager's dialer: one leg to home's /pop/subscribe/{id}.
func (d *popDownlink) dial(ctx context.Context, addr, path string) (edgeUpstream, error) {
	tgt, ok := d.s.popTarget()
	if !ok {
		return nil, errPopUnlinked
	}
	q := url.Values{"proto": {poplink.ProtoQuery()}}
	if tgt.Carrier {
		q.Set("delivery", "carrier")
	}
	if tgt.Pop != "" {
		q.Set("pop", tgt.Pop)
	}
	target := "https://" + addr + path + "?" + q.Encode()
	h := http.Header{"Origin": {popOrigin}, "Authorization": {"Bearer " + tgt.Credential}}
	dial := tgt.Dial
	if dial == nil {
		dial = d.s.popLegDialer(tgt.RootCAs)
	}
	sess, rsp, err := dial(ctx, target, h)
	if err != nil {
		if rsp != nil && rsp.StatusCode >= 300 {
			return nil, &PopLegStatusError{Status: rsp.StatusCode, Reason: rsp.Header.Get(poplink.ReasonHeader), Err: err}
		}
		return nil, err
	}
	d.legs.Add(1)
	return &popLegUpstream{sess: sess, meter: d.meter}, nil
}

// popLegDialer is the production leg dialer: home's certificate by name
// (roots, or the system pool), the link's QUIC timers and R73's receive
// windows from -uplink-max-bitrate (0 leaves quic-go's defaults).
func (s *Server) popLegDialer(roots *x509.CertPool) PopLegDial {
	return func(ctx context.Context, target string, h http.Header) (PopLegSession, *http.Response, error) {
		u, err := url.Parse(target)
		if err != nil {
			return nil, nil, err
		}
		streamWindow, connWindow := uplinkReceiveWindows(s.cfg.UplinkMaxBitrateBytes)
		tr := &webtransport.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: u.Hostname()},
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
				MaxIdleTimeout:                   poplink.LinkIdleTimeout,
				KeepAlivePeriod:                  poplink.LinkKeepAlive,
				InitialStreamReceiveWindow:       streamWindow,
				InitialConnectionReceiveWindow:   connWindow,
			},
		}
		if hook := s.testHookPopLegTransport.Load(); hook != nil {
			(*hook)(tr)
		}
		rsp, sess, err := tr.Dial(ctx, target, h)
		if err != nil {
			_ = tr.Close()
			return nil, rsp, err
		}
		return &popLegWT{Session: sess, tr: tr}, rsp, nil
	}
}

// popLegWT is a leg dialed by popLegDialer: closing it closes its QUIC
// transport too.
type popLegWT struct {
	*webtransport.Session
	tr *webtransport.Transport
}

func (p *popLegWT) CloseWithError(code uint32, msg string) error {
	err := p.Session.CloseWithError(webtransport.SessionErrorCode(code), msg)
	_ = p.tr.Close()
	return err
}

// popLegUpstream adapts a leg to the edge pump, metering what arrives.
type popLegUpstream struct {
	sess  PopLegSession
	meter *rateMeter
}

func (u *popLegUpstream) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	b, err := u.sess.ReceiveDatagram(ctx)
	u.meter.add(len(b))
	return b, err
}

func (u *popLegUpstream) SendDatagram(p []byte) error { return u.sess.SendDatagram(p) }

func (u *popLegUpstream) AcceptUniStream(ctx context.Context) (io.Reader, error) {
	st, err := u.sess.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return &popLegStream{ReceiveStream: st, meter: u.meter}, nil
}

func (u *popLegUpstream) Close() error { return u.sess.CloseWithError(0, "pop leg detaching") }

// CloseError is webtransportUpstream.CloseError for a leg.
func (u *popLegUpstream) CloseError() error {
	t := time.NewTimer(closeErrorWait)
	defer t.Stop()
	select {
	case <-u.sess.Context().Done():
	case <-t.C:
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeErrorWait)
	defer cancel()
	for {
		if _, err := u.sess.AcceptUniStream(ctx); err != nil {
			return err
		}
	}
}

// popLegStream is a leg's uni stream, metered; CancelRead stays reachable
// so the pump resets a stream it refuses (resetEdgeStream).
type popLegStream struct {
	*webtransport.ReceiveStream
	meter *rateMeter
}

var _ edgeStreamResetter = (*popLegStream)(nil)

func (s *popLegStream) Read(p []byte) (int, error) {
	n, err := s.ReceiveStream.Read(p)
	s.meter.add(n)
	return n, err
}

// rateMeter is a 10 s ingress rate over one-second buckets.
type rateMeter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets [rateWindowSeconds]uint64
	stamps  [rateWindowSeconds]int64
}

const rateWindowSeconds = 10

func newRateMeter(now func() time.Time) *rateMeter { return &rateMeter{now: now} }

func (m *rateMeter) add(n int) {
	if n <= 0 {
		return
	}
	sec := m.now().Unix()
	i := int(sec % rateWindowSeconds)
	m.mu.Lock()
	if m.stamps[i] != sec {
		m.stamps[i], m.buckets[i] = sec, 0
	}
	m.buckets[i] += uint64(n)
	m.mu.Unlock()
}

// mbps is the mean rate over the last rateWindowSeconds, the current
// second included.
func (m *rateMeter) mbps() float64 {
	sec := m.now().Unix()
	var sum uint64
	m.mu.Lock()
	for i, st := range m.stamps {
		if st > sec-rateWindowSeconds && st <= sec {
			sum += m.buckets[i]
		}
	}
	m.mu.Unlock()
	return float64(sum) * 8 / rateWindowSeconds / 1e6
}
