package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// edgeStreamResetter is the part of an upstream uni stream that resets it:
// *webtransport.ReceiveStream's CancelRead. edgeUpstream.AcceptUniStream
// returns a plain io.Reader (the fakes are readers), so it is asserted.
type edgeStreamResetter interface {
	CancelRead(webtransport.StreamErrorCode)
}

// The production upstream's streams are resettable.
var _ edgeStreamResetter = (*webtransport.ReceiveStream)(nil)

// resetEdgeStream resets r when it can be reset: a refused or failed stream
// must not sit unread holding the session's flow-control credit.
func resetEdgeStream(r io.Reader) {
	if c, ok := r.(edgeStreamResetter); ok {
		c.CancelRead(0)
	}
}

// acceptStreams is the edge pump's uni-stream loop: it accepts every stream
// the upstream opens and hands it to classifyUniStream, at most
// maxConcurrentKeyframeStreams keyframe streams at once. A stream refused for
// want of a slot is reset. It returns the accept error that ended the loop,
// after every in-flight ingest has finished.
func (es *edgeSession) acceptStreams(ctx context.Context, up edgeUpstream, pub *hub.Publisher) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	log := es.m.log.With("broadcast_id", es.id)
	sem := make(chan struct{}, maxConcurrentKeyframeStreams)
	for {
		stream, err := up.AcceptUniStream(ctx)
		if err != nil {
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			resetEdgeStream(stream)
			log.Warn("edge keyframe stream rejected: too many concurrent")
			continue
		}
		wg.Add(1)
		go func(st io.Reader) {
			defer wg.Done()
			// An edge reads what its upstream relays send, which never
			// includes a publisher's keyframe carrier (docs/88 D2).
			classifyUniStream(st, func() { resetEdgeStream(st) }, sem, pub, false, log)
		}(stream)
	}
}

// classifyUniStream runs one uni stream to its end (an edge pump's, or a
// publisher's with -uplink-carriers on), holding one of
// sem's slots (acquired by the caller) and resetting the stream through
// cancel when its ingest fails. It reads the stream kind from the prologue
// (docs/75 D2): Version‖TypeReliableCarrier is an R73 carrier, which the
// PoP's downlink leg writer sends (docs/76 item 18), read by
// Publisher.IngestUplinkCarrier after the slot is given back, since a carrier
// lives for a GOP and the hub bounds carriers itself (hub.MaxUplinkCarriers).
// With keyframeCarriers (a publisher's session, never an edge's),
// Version‖TypeKeyframeCarrier is an R84 keyframe carrier (docs/88 D2), read by
// Publisher.IngestKeyframeCarrier the same way. Anything else goes to the
// keyframe reader with the peeked bytes replayed, so it sees exactly what the
// upstream sent.
func classifyUniStream(r io.Reader, cancel func(), sem chan struct{}, pub *hub.Publisher, keyframeCarriers bool, log *slog.Logger) {
	release := sync.OnceFunc(func() { <-sem })
	defer release()
	var head [wire.CarrierPrologueSize]byte
	n, _ := io.ReadFull(r, head[:])
	if n == len(head) {
		var ingest func(io.Reader) error
		switch {
		case wire.ParseCarrierPrologue(head[:]) == nil:
			ingest = pub.IngestUplinkCarrier
		case keyframeCarriers && wire.ParseKeyframeCarrierPrologue(head[:]) == nil:
			ingest = pub.IngestKeyframeCarrier
		}
		if ingest != nil {
			release()
			if err := ingest(io.MultiReader(bytes.NewReader(head[:]), r)); err != nil {
				cancel()
				logCarrierEnd(log, err)
			}
			return
		}
	}
	if err := pub.IngestKeyframeStream(io.MultiReader(bytes.NewReader(head[:n]), r)); err != nil {
		cancel()
		log.Debug("keyframe stream ingest failed", "err", err)
	}
}

// logCarrierEnd logs why a carrier of either kind ended early.
func logCarrierEnd(log *slog.Logger, err error) {
	switch {
	case errors.Is(err, hub.ErrUplinkCarrierLimit):
		log.Warn("carrier rejected: too many concurrent", "max", hub.MaxUplinkCarriers)
	case errors.Is(err, hub.ErrBadUplinkRecord):
		log.Warn("carrier ended: malformed record", "err", err)
	case errors.Is(err, hub.ErrBadKeyframeCarrier):
		log.Warn("keyframe carrier ended: malformed keyframe", "err", err)
	default:
		log.Debug("carrier ended", "err", err)
	}
}
