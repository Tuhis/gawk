package transport

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// MintGate is consulted on the mint path after the origin claim and before
// the broadcast is announced (R74, docs/76 items 14-15): a PoP relay reserves
// the fresh ID at its home fleet there. localOnly reports that the broadcast
// was admitted but stays local to this relay (unlinked, a collision, or over
// the PoP's limit); an error refuses the mint.
type MintGate interface {
	ReserveMint(ctx context.Context, id string) (localOnly bool, err error)
}

// mintGateBox wraps the interface so it fits an atomic.Pointer.
type mintGateBox struct{ g MintGate }

// SetMintGate installs the mint gate; nil removes it. Safe while serving.
func (s *Server) SetMintGate(g MintGate) {
	if g == nil {
		s.mintGate.Store(nil)
		return
	}
	s.mintGate.Store(&mintGateBox{g: g})
}

// gateMint runs the installed MintGate for a freshly minted broadcast. With
// no gate (every relay before R74, and every home) it admits at once. False
// means the gate refused: the hub claim was released, the session closed,
// and the handler must return.
func (s *Server) gateMint(ctx context.Context, sess *webtransport.Session, id string, pub *hub.Publisher, log *slog.Logger) bool {
	box := s.mintGate.Load()
	if box == nil {
		return true
	}
	localOnly, err := box.g.ReserveMint(ctx, id)
	if err != nil {
		pub.Close()
		s.metrics.Connection("publish", metrics.OutcomeError)
		log.Warn("minted broadcast refused by the mint gate", "broadcast_key", s.broadcastKey(id), "err", err)
		sess.CloseWithError(http.StatusServiceUnavailable, "failed to reserve broadcast")
		return false
	}
	if localOnly {
		log.Info("minted broadcast stays local-only", "broadcast_key", s.broadcastKey(id))
	}
	return true
}
