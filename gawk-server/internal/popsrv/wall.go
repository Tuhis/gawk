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
	m   *Manager
	o   WallOptions
	log *slog.Logger
	ctx context.Context

	// Ops are driven per broadcast, latest wins: one broadcast waiting for
	// its reservation never holds up another (a relink resyncs them all at
	// once), and a Hide that arrives during the wait replaces the attach.
	mu      sync.Mutex
	pending map[wallKey]*poplink.Wall
	running map[wallKey]bool
}

type wallKey struct{ pop, id string }

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
	w := &Wall{m: m, o: o, log: o.Log.With("subsystem", "wall"), ctx: ctx,
		pending: map[wallKey]*poplink.Wall{}, running: map[wallKey]bool{}}
	m.OnLinkRecord(poplink.TWall, w.onRecord)
	m.setLinkStreams(w.serveStream)
	return w
}

// onRecord records a wall record as its broadcast's latest op and makes
// sure a driver applies it; it never blocks the link's reader.
func (w *Wall) onRecord(pop string, rec any) {
	r, ok := rec.(*poplink.Wall)
	if !ok || r.ID == "" {
		return
	}
	k := wallKey{pop: pop, id: r.ID}
	w.mu.Lock()
	w.pending[k] = r
	start := !w.running[k]
	w.running[k] = true
	w.mu.Unlock()
	if start {
		go w.drive(k)
	}
}

// drive applies one broadcast's ops in order until none is pending.
func (w *Wall) drive(k wallKey) {
	for {
		w.mu.Lock()
		r := w.pending[k]
		delete(w.pending, k)
		if r == nil || w.ctx.Err() != nil {
			delete(w.running, k)
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		w.apply(k, r)
	}
}

// superseded reports a newer op for k arrived while one was in hand.
func (w *Wall) superseded(k wallKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending[k] != nil
}

// apply turns one wall record into a vouched op. An attach is vouched only
// for an ID this PoP reserved: the link authenticates the PoP, the
// reservation ties the broadcast to it. A detach needs no reservation (it
// may be the broadcast's end, after its release), and the registry only
// lets a PoP detach from its own wall.
func (w *Wall) apply(k wallKey, r *poplink.Wall) {
	pop := k.pop
	p, ok := w.m.pop(pop)
	if !ok || p.Spec.Wall == nil || p.Spec.Wall.RoomCode == "" {
		w.log.Debug("pop wall op ignored: the PoP has no wall", "pop", pop)
		return
	}
	code := p.Spec.Wall.RoomCode
	switch r.Op {
	case poplink.WallAttach:
		switch w.waitOwner(k) {
		case ownerSuperseded:
			return // a newer op (a Hide) replaces this attach
		case ownerMissing:
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

type ownerVerdict int

const (
	ownerOK ownerVerdict = iota
	ownerMissing
	ownerSuperseded
)

// waitOwner waits up to OwnerWait for k's ID to read as k's PoP's
// reservation, giving up early when a newer op for it arrives.
func (w *Wall) waitOwner(k wallKey) ownerVerdict {
	if w.o.Owner == nil {
		return ownerMissing
	}
	deadline := time.Now().Add(w.o.OwnerWait)
	for {
		if w.superseded(k) {
			return ownerSuperseded
		}
		if owner, ok := w.o.Owner.Owner(k.id); ok && owner == k.pop {
			return ownerOK
		}
		if time.Now().After(deadline) {
			return ownerMissing
		}
		select {
		case <-w.ctx.Done():
			return ownerMissing
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
