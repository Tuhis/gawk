package engine

import (
	"errors"
	"log/slog"
)

// DefaultMediaFactory is the production capture path: the XDG ScreenCast
// portal handshake for a PipeWire fd, and a GStreamer child that encodes it in
// hardware and writes MPEG-TS to our pipe.
//
// It lands in V2/V3. Until then the engine is complete from the relay's point
// of view — it dials, announces, clock-syncs and sends whatever a MediaSource
// gives it — and every test injects its source through Options.MediaFactory.
func DefaultMediaFactory(cfg MediaConfig, clock Clock, log *slog.Logger) (MediaSource, error) {
	return nil, errors.New("engine: capture is not wired up yet (R14 V2)")
}
