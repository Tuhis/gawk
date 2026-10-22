package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The PoP side of an R74 uplink leg (docs/76 item 18): a dialed
// /pop/publish/{id} session fed from the local hub as one more subscriber —
// hub.SubscribeLeg, so it starts from the hub's join-prime (the cached
// keyframe with its config, and the clock mapping), forwards keyframe
// streams byte-identical and, against a home advertising CapUplinkCarriers,
// writes deltas and parity as R73 carrier records under R73's sender rules
// (the leg writer); everything else, and everything without the bit or with
// -pop-leg-delivery=datagram, goes as datagrams verbatim. The one rewrite is
// the ClockMapping, backward per hop (rewriteClockMappingBack, item 26),
// over the TimeSync estimator this leg runs against home. Home's viewer
// count G arrives down the leg and is recorded as the leg's downstream
// count, so the PoP's broadcaster and viewers see local + G_home (item 25).

// legCapsTimeout bounds the wait for home's RelayCapabilities on a leg
// (sent at once, flags zero included, by every home that serves the route).
const legCapsTimeout = 5 * time.Second

// legCloseGrace bounds the wait, once a leg's reads end, for home's close
// capsule to end the session, so its code (4013) is the one read.
const legCloseGrace = time.Second

// popLegSession is the slice of a WebTransport session a leg needs. A
// poplink.Session from poplink.WrapSession has it (the promoted methods of
// the wrapped *webtransport.Session); the narrow interface keeps the dialer
// a test seam.
type popLegSession interface {
	SendDatagram([]byte) error
	ReceiveDatagram(context.Context) ([]byte, error)
	OpenUniStream() (*webtransport.SendStream, error)
	AcceptUniStream(context.Context) (*webtransport.ReceiveStream, error)
	CloseWithError(code uint32, msg string) error
	Context() context.Context
}

// PopLegOptions configures ServePopUplinkLeg.
type PopLegOptions struct {
	// Carrier is the PoP's -pop-leg-delivery=carrier. The leg uses carriers
	// only when home also advertises CapUplinkCarriers on the leg.
	Carrier bool
	// Bytes, when set, counts every byte the leg writes (datagrams and
	// stream writes): the "zero bytes without demand" counter.
	Bytes *atomic.Uint64
	// OnStart is told the delivery the leg settled on, once subscribed.
	OnStart func(carrier bool)
}

// ErrNotLegSession: ServePopUplinkLeg was handed a session it cannot run a
// leg on (a fake without the WebTransport methods).
var ErrNotLegSession = errors.New("transport: not a WebTransport leg session")

// ServePopUplinkLeg runs broadcast id's uplink leg on an established
// /pop/publish/{id} session until the session ends, ctx ends, or the local
// hub drops the leg. It returns why the session ended — a
// *webtransport.SessionError carrying home's code (4013 PopNoDemand) when
// home closed it — and closes the session on its way out.
func (s *Server) ServePopUplinkLeg(ctx context.Context, leg poplink.Session, id string, o PopLegOptions) error {
	ls, ok := leg.(popLegSession)
	if !ok {
		_ = leg.CloseWithError(0, "not a leg session")
		return ErrNotLegSession
	}
	parent := ctx
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// The leg closes its own end only once home's reason has had its chance
	// to land (below), or at once when the caller gives the leg up.
	closed := make(chan struct{})
	var closeOnce sync.Once
	closeLeg := func() {
		closeOnce.Do(func() {
			close(closed)
			_ = ls.CloseWithError(0, "leg ended")
		})
	}
	defer closeLeg()
	go func() {
		select {
		case <-parent.Done():
			closeLeg()
		case <-closed:
		}
	}()

	caps, _ := readLegCapabilities(ctx, ls)
	carrier := o.Carrier && caps.Flags&wire.CapUplinkCarriers != 0
	conn := &popLegConn{sess: ls, est: &timeSyncEstimator{}, bytes: o.Bytes}
	sub, err := s.registry.SubscribeLeg(id, conn, hub.LegOptions{Carrier: carrier})
	if err != nil {
		return fmt.Errorf("transport: leg subscribe %s: %w", id, err)
	}
	defer sub.Close()
	if o.OnStart != nil {
		o.OnStart(carrier)
	}

	var endMu sync.Mutex
	var endErr error
	var wg sync.WaitGroup
	wg.Add(3)
	// Home's datagrams: TimeSync replies feed the estimator (and release a
	// held mapping); ViewerCount is G_home.
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			d, err := ls.ReceiveDatagram(ctx)
			if err != nil {
				endMu.Lock()
				endErr = err
				endMu.Unlock()
				return
			}
			if len(d) < 2 {
				continue
			}
			switch d[1] {
			case wire.TypeTimeSync:
				if t0, server, err := wire.ParseTimeSync(d); err == nil {
					conn.est.record(t0, server, relayNowUs())
					conn.flushPending()
				}
			case wire.TypeViewerCount:
				if n, err := wire.ParseViewerCount(d); err == nil {
					sub.RecordDownstreamViewers(n)
				}
			}
		}
	}()
	// Home opens no other streams on a leg; refuse any.
	go func() {
		defer wg.Done()
		for {
			st, err := ls.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			st.CancelRead(0)
		}
	}()
	// TimeSync pings, the first at once so a primed mapping is held briefly.
	go func() {
		defer wg.Done()
		t := time.NewTicker(edgePingInterval)
		defer t.Stop()
		for {
			_ = conn.sendRaw(wire.AppendTimeSync(nil, relayNowUs(), 0))
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-ls.Context().Done():
	}
	if parent.Err() == nil {
		// A read loop ended first. The datagram queue reports the CONNECT
		// stream's end (a bare EOF) before the session has parsed home's
		// WT_CLOSE_SESSION capsule, so wait for the session to end: closing
		// our end now would record our own code 0 over home's 4013, and a
		// leg that misses its 4013 redials.
		t := time.NewTimer(legCloseGrace)
		select {
		case <-ls.Context().Done():
		case <-parent.Done():
		case <-t.C:
		}
		t.Stop()
	}
	peerEnded := ls.Context().Err() != nil
	cancel()
	wg.Wait()
	// Home's close code: a closed session answers every call with its
	// SessionError (the context's cause is a bare cancellation).
	var se *webtransport.SessionError
	endMu.Lock()
	defer endMu.Unlock()
	if errors.As(endErr, &se) {
		return endErr
	}
	if peerEnded {
		if _, err := ls.AcceptUniStream(context.Background()); errors.As(err, &se) {
			return err
		}
	}
	if endErr != nil {
		return endErr
	}
	return ctx.Err()
}

// readLegCapabilities reads home's RelayCapabilities, the leg's first uni
// stream. Without one inside legCapsTimeout the leg is a datagram leg.
func readLegCapabilities(ctx context.Context, ls popLegSession) (wire.RelayCapabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, legCapsTimeout)
	defer cancel()
	st, err := ls.AcceptUniStream(ctx)
	if err != nil {
		return wire.RelayCapabilities{}, err
	}
	b, err := io.ReadAll(io.LimitReader(st, wire.RelayCapabilitiesSize+1))
	if err != nil {
		return wire.RelayCapabilities{}, err
	}
	return wire.ParseRelayCapabilities(b)
}

// popLegConn is the leg's hub.Conn: the session, with the ClockMapping
// rewritten backward and held until the estimator can translate it.
type popLegConn struct {
	sess  popLegSession
	est   *timeSyncEstimator
	bytes *atomic.Uint64

	mu      sync.Mutex
	pending []byte
}

func (c *popLegConn) count(n int) {
	if c.bytes != nil {
		c.bytes.Add(uint64(n))
	}
}

func (c *popLegConn) sendRaw(b []byte) error {
	c.count(len(b))
	return c.sess.SendDatagram(b)
}

func (c *popLegConn) SendDatagram(d []byte) error {
	if len(d) >= 2 && d[1] == wire.TypeClockMapping {
		out, ok := rewriteClockMappingBack(d, c.est)
		if !ok {
			if _, err := wire.ParseClockMapping(d); err == nil {
				c.mu.Lock()
				c.pending = append(c.pending[:0], d...)
				c.mu.Unlock()
			}
			return nil
		}
		return c.sendRaw(out)
	}
	return c.sendRaw(d)
}

// flushPending sends a held mapping once a TimeSync sample exists.
func (c *popLegConn) flushPending() {
	c.mu.Lock()
	pm := c.pending
	c.pending = nil
	c.mu.Unlock()
	if pm == nil {
		return
	}
	if out, ok := rewriteClockMappingBack(pm, c.est); ok {
		_ = c.sendRaw(out)
	}
}

func (c *popLegConn) OpenKeyframeStream() (hub.KeyframeStream, error) {
	st, err := c.sess.OpenUniStream()
	if err != nil {
		return nil, err
	}
	return countingStream{keyframeSendStream{st}, c}, nil
}

func (c *popLegConn) OpenCarrierStream() (hub.KeyframeStream, error) { return c.OpenKeyframeStream() }

func (c *popLegConn) CloseWithError(code uint32, reason string) error {
	return c.sess.CloseWithError(code, reason)
}

// countingStream counts a leg stream's writes.
type countingStream struct {
	keyframeSendStream
	c *popLegConn
}

func (s countingStream) Write(p []byte) (int, error) {
	n, err := s.keyframeSendStream.Write(p)
	s.c.count(n)
	return n, err
}
