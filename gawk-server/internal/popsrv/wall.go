package popsrv

// wall.go: the party wall's home side (docs/76 §6.10, PP11). A PoP's wall
// records become vouched attaches and detaches on the wall's home pod, and
// the extra bidirectional streams of its link carry its guests' room joins
// home (transport's ServeRoomStream).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

// WallHost is the transport side the wall drives: *transport.Server.
type WallHost interface {
	// VouchWall applies one wall op on the wall's home pod.
	VouchWall(ctx context.Context, pop, op, code, id, label string) error
	// ServeRoomStream serves one room join that arrived on pop's link.
	ServeRoomStream(ctx context.Context, pop, path, rawQuery string, stream io.ReadWriteCloser)
}

// ReservationOwner names the PoP a broadcast ID is reserved for
// (Reservations.Owner).
type ReservationOwner interface {
	Owner(id string) (pop string, ok bool)
}

// WallOptions wires StartWall.
type WallOptions struct {
	Host  WallHost
	Owner ReservationOwner
	// OwnerWait bounds how long an attach waits for its reservation to show
	// in the Owner cache (the informer lags the PoP's reserve by a moment,
	// and a relink's re-reserve may still be in flight). Default 10 s.
	OwnerWait time.Duration
	// OwnerPoll is the wait's poll interval (default 100 ms).
	OwnerPoll time.Duration
	// OpenTimeout bounds the RoomOpen read on a new link stream (default
	// 10 s).
	OpenTimeout time.Duration
	Log         *slog.Logger
}

// Wall is the home side of every linked PoP's party wall on this pod.
type Wall struct {
	m    *Manager
	o    WallOptions
	log  *slog.Logger
	ctx  context.Context
	mu   sync.Mutex
	work map[string]chan *poplink.Wall // per PoP, so its ops stay in order
}

// wallQueue bounds one PoP's pending wall ops.
const wallQueue = 256

// StartWall registers the wall on m: wall records from any link this pod
// holds, and room streams on those links. It runs until ctx ends.
func StartWall(ctx context.Context, m *Manager, o WallOptions) *Wall {
	if o.OwnerWait <= 0 {
		o.OwnerWait = 10 * time.Second
	}
	if o.OwnerPoll <= 0 {
		o.OwnerPoll = 100 * time.Millisecond
	}
	if o.OpenTimeout <= 0 {
		o.OpenTimeout = 10 * time.Second
	}
	if o.Log == nil {
		o.Log = m.log
	}
	w := &Wall{m: m, o: o, log: o.Log.With("subsystem", "wall"), ctx: ctx, work: map[string]chan *poplink.Wall{}}
	m.OnLinkRecord(poplink.TWall, w.onRecord)
	m.setLinkStreams(w.serveStream)
	return w
}

// onRecord queues a wall record for its PoP's worker; it never blocks the
// link's reader.
func (w *Wall) onRecord(pop string, rec any) {
	r, ok := rec.(*poplink.Wall)
	if !ok || r.ID == "" {
		return
	}
	w.mu.Lock()
	q := w.work[pop]
	if q == nil {
		q = make(chan *poplink.Wall, wallQueue)
		w.work[pop] = q
		go w.worker(pop, q)
	}
	w.mu.Unlock()
	select {
	case q <- r:
	default:
		w.log.Warn("pop wall op dropped: queue full", "pop", pop, "op", r.Op)
	}
}

func (w *Wall) worker(pop string, q chan *poplink.Wall) {
	for {
		select {
		case <-w.ctx.Done():
			return
		case r := <-q:
			w.apply(pop, r)
		}
	}
}

// apply turns one wall record into a vouched op. An attach is vouched only
// for an ID this PoP reserved: the link authenticates the PoP, the
// reservation ties the broadcast to it. A detach needs no reservation (it
// may be the broadcast's end, after its release), and the registry only
// lets a PoP detach from its own wall.
func (w *Wall) apply(pop string, r *poplink.Wall) {
	p, ok := w.m.pop(pop)
	if !ok || p.Spec.Wall == nil || p.Spec.Wall.RoomCode == "" {
		w.log.Debug("pop wall op ignored: the PoP has no wall", "pop", pop)
		return
	}
	code := p.Spec.Wall.RoomCode
	switch r.Op {
	case poplink.WallAttach:
		if !w.waitOwner(pop, r.ID) {
			w.log.Info("pop wall attach refused: the broadcast is not this PoP's reservation", "pop", pop)
			return
		}
	case poplink.WallDetach:
	default:
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	err := w.o.Host.VouchWall(ctx, pop, r.Op, code, r.ID, r.Label)
	switch {
	case err == nil:
	case errors.Is(err, roomsrv.ErrNotFound) && r.Op == poplink.WallDetach:
		// Hidden twice, or ended after a Hide.
	default:
		w.log.Info("pop wall op refused", "pop", pop, "op", r.Op, "err", err)
	}
}

func (w *Wall) waitOwner(pop, id string) bool {
	if w.o.Owner == nil {
		return false
	}
	deadline := time.Now().Add(w.o.OwnerWait)
	for {
		if owner, ok := w.o.Owner.Owner(id); ok && owner == pop {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-w.ctx.Done():
			return false
		case <-time.After(w.o.OwnerPoll):
		}
	}
}

// serveStream reads a link stream's RoomOpen and hands the join to the
// transport. Anything else on a stream is closed.
func (w *Wall) serveStream(ctx context.Context, pop string, s io.ReadWriteCloser) {
	if d, ok := s.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now().Add(w.o.OpenTimeout))
	}
	rec, err := poplink.Read(s)
	if d, ok := s.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Time{})
	}
	open, ok := rec.(*poplink.RoomOpen)
	if err != nil || !ok {
		w.log.Debug("pop link stream without a RoomOpen closed", "pop", pop, "err", err)
		_ = s.Close()
		return
	}
	w.o.Host.ServeRoomStream(ctx, pop, open.Path, open.RawQuery, s)
}

// linkStreamHandler serves one extra bidirectional stream of a link.
type linkStreamHandler func(ctx context.Context, pop string, s io.ReadWriteCloser)

func (m *Manager) setLinkStreams(h linkStreamHandler) { m.streams.Store(&h) }

// ServesLinkStreams reports whether a wall serves the links' extra streams
// (wiring tests).
func (m *Manager) ServesLinkStreams() bool { return m.streams.Load() != nil }

// acceptStreams serves the link's extra bidirectional streams (the control
// stream was the first) until the session ends. With no handler a stream
// is closed at once.
func (l *link) acceptStreams(ctx context.Context) {
	for {
		s, err := l.sess.AcceptStream(ctx)
		if err != nil {
			return
		}
		h := l.m.streams.Load()
		if h == nil {
			_ = s.Close()
			continue
		}
		go (*h)(ctx, l.name, s)
	}
}
