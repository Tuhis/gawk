// Package pubsim is the synthetic fixture publisher behind cmd/gawk-pubsim
// (R20, docs/25 Decision 3): the committed H.264 fixture demuxed into access
// units once, then looped through the real engine at a live pace. It exists
// so CI (and the manual W6 scale drill) has a deterministic broadcaster that
// is not a gaming PC — same engine, same wire bytes, no GPU, no portal.
package pubsim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/mpegts"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Demux turns a raw MPEG-TS stream into engine access units, exactly as the
// relay integration test's fixtureSource does: bytes cloned per AU, keyframes
// flagged by the IDR sniff.
func Demux(ts []byte) ([]engine.AccessUnit, error) {
	var aus []engine.AccessUnit
	d := mpegts.NewDemuxer(wire.MaxKeyframeBytes, func(au mpegts.AU) error {
		data := make([]byte, len(au.Data))
		copy(data, au.Data)
		aus = append(aus, engine.AccessUnit{
			Data:     data,
			Keyframe: engine.HasIDR(data),
		})
		return nil
	})
	if _, err := d.Write(ts); err != nil {
		return nil, fmt.Errorf("demux: %w", err)
	}
	if err := d.Close(); err != nil {
		return nil, fmt.Errorf("demux: %w", err)
	}
	if len(aus) == 0 {
		return nil, errors.New("demux: no access units in stream")
	}
	if !aus[0].Keyframe {
		// The loop restarts decoding from AU 0 on every wrap; a non-IDR head
		// would make every wrap a decode gap for late joiners.
		return nil, errors.New("demux: stream does not start with a keyframe")
	}
	return aus, nil
}

// Factory adapts a demuxed AU list into the engine's media-source seam. The
// engine calls it with its own Clock, which is what makes the frame stamps
// and TimeSync read one timeline (docs/19 Decision 6) — pubsim must not
// weaken that invariant just because its frames are canned.
func Factory(aus []engine.AccessUnit, fps int) engine.MediaSourceFactory {
	return func(_ engine.MediaConfig, clock engine.Clock, _ *slog.Logger) (engine.MediaSource, error) {
		return NewSource(aus, fps, clock)
	}
}

// Source is an engine.MediaSource that replays a fixed AU sequence in a loop
// at a steady rate, forever, stamping each AU with the live clock at emit
// time. The fixture's own timeline never leaks: live-edge drift and the
// ClockMapping math on the viewer side read TimestampUs, and replayed stamps
// from a 2-second file would wrap those instruments around nonsense.
type Source struct {
	aus      []engine.AccessUnit
	clock    engine.Clock
	interval time.Duration

	// tick, when non-nil, replaces the real ticker — the test seam that
	// makes the loop/timestamp logic deterministic.
	tick <-chan time.Time

	frames  chan engine.AccessUnit
	stop    chan struct{}
	done    chan struct{}
	started bool
	mu      sync.Mutex
}

// NewSource builds a Source pacing aus at fps. The keyframe cadence follows
// the fixture (every 15 AUs → 500 ms at 30 fps; other rates scale it).
func NewSource(aus []engine.AccessUnit, fps int, clock engine.Clock) (*Source, error) {
	if len(aus) == 0 {
		return nil, errors.New("pubsim: no access units")
	}
	if fps <= 0 {
		return nil, fmt.Errorf("pubsim: fps must be positive, got %d", fps)
	}
	return &Source{
		aus:      aus,
		clock:    clock,
		interval: time.Second / time.Duration(fps),
		frames:   make(chan engine.AccessUnit, 4),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

func (s *Source) Start(ctx context.Context) (<-chan engine.AccessUnit, error) {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	tick := s.tick
	var ticker *time.Ticker
	if tick == nil {
		ticker = time.NewTicker(s.interval)
		tick = ticker.C
	}
	go func() {
		defer close(s.done)
		defer close(s.frames)
		if ticker != nil {
			defer ticker.Stop()
		}
		for i := 0; ; i = (i + 1) % len(s.aus) {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-tick:
			}
			au := s.aus[i]
			au.TimestampUs = s.clock.NowUs()
			select {
			case s.frames <- au:
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return s.frames, nil
}

func (s *Source) Stop() error {
	s.mu.Lock()
	started := s.started
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.mu.Unlock()
	if started {
		<-s.done
	}
	return nil
}

func (s *Source) Encoder() string     { return "fixture" }
func (s *Source) CapturePath() string { return "fixture" }
func (s *Source) Err() error          { return nil }
