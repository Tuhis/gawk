package gst

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/mpegts"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// liveProbeWindow is how long a freshly started child must survive before we
// believe its encoder works.
//
// This is the "live start is the final probe" rule (Decision 4): a videotestsrc
// trial cannot prove the real path's dmabuf import, so the first seconds of the
// real pipeline are the last probe. A child that dies inside this window
// advances the cascade; one that dies later is a genuine failure, because it
// was working.
const liveProbeWindow = 3 * time.Second

// Options configure the capture factory.
type Options struct {
	// LastGoodEncoder is the cached cascade winner, re-verified before use.
	LastGoodEncoder string
	// OnEncoderChosen persists the winner.
	OnEncoderChosen func(string)

	// Binary overrides gst-launch-1.0 (tests, unusual installs).
	Binary string
	// Trial overrides the trial-encode runner (tests).
	Trial TrialFunc
	// OpenPortal overrides the portal handshake (tests).
	OpenPortal func(ctx context.Context, opts portal.Options) (*portal.Stream, error)
	// LiveProbeWindow overrides how long a child must survive to be believed
	// (tests; production uses liveProbeWindow).
	LiveProbeWindow time.Duration
}

// NewFactory returns an engine.MediaSourceFactory that captures via the portal
// and encodes in a GStreamer child.
//
// The engine deliberately does not import this package: it knows about frames
// and the relay, not about D-Bus and subprocesses. The shells compose the two.
func NewFactory(opts Options) engine.MediaSourceFactory {
	return func(cfg engine.MediaConfig, clock engine.Clock, log *slog.Logger) (engine.MediaSource, error) {
		s := &Source{cfg: cfg, clock: clock, log: log, opts: opts}
		if s.opts.Binary == "" {
			s.opts.Binary = LaunchBinary
		}
		if s.opts.Trial == nil {
			s.opts.Trial = s.realTrial
		}
		if s.opts.OpenPortal == nil {
			s.opts.OpenPortal = portal.Open
		}
		if s.opts.LiveProbeWindow <= 0 {
			s.opts.LiveProbeWindow = liveProbeWindow
		}
		return s, nil
	}
}

// Source captures the screen and emits access units.
type Source struct {
	cfg   engine.MediaConfig
	clock engine.Clock
	log   *slog.Logger
	opts  Options

	mu      sync.Mutex
	encoder string
	err     error
	stream  *portal.Stream
	kid     *child
	stopped bool

	frames chan engine.AccessUnit
	wg     sync.WaitGroup
}

// Start runs the portal handshake, picks an encoder, and starts the child.
func (s *Source) Start(ctx context.Context) (<-chan engine.AccessUnit, error) {
	// Before anything user-visible: no GStreamer means no capture, and asking
	// someone to share their screen and *then* telling them a package is
	// missing is the same mistake as popping the picker before dialling the
	// relay.
	if err := EnsureBinary(s.opts.Binary); err != nil {
		return nil, err
	}

	// The picker appears on every Start, by decision (docs/19): we ask what to
	// share each time rather than persisting the choice, so no restore token is
	// passed and none is kept.
	stream, err := s.opts.OpenPortal(ctx, portal.Options{})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.stream = stream
	s.mu.Unlock()

	cand, err := SelectEncoder(ctx, s.cfg, s.opts.LastGoodEncoder, s.opts.Trial)
	if err != nil {
		stream.Close()
		return nil, err
	}

	s.frames = make(chan engine.AccessUnit, 8)
	if err := s.startWithCascade(ctx, stream, cand); err != nil {
		stream.Close()
		return nil, err
	}
	return s.frames, nil
}

// startWithCascade starts the child, and on an immediate death retries —
// first with the capture pinned to system memory (a pipewiresrc negotiation
// death is not the encoder's fault, and the same encoder deserves a second
// try with CPU-visible frames), then by advancing the cascade. Every retry
// reuses the already-granted portal session, so it costs seconds rather than
// another share dialog.
func (s *Source) startWithCascade(ctx context.Context, stream *portal.Stream, first Candidate) error {
	order := candidatesFrom(first, s.cfg.Encoder != "")
	var failures []string

	for _, cand := range order {
		for _, mode := range CaptureModes {
			kid, err := startChild(ctx, s.opts.Binary, BuildPipeline(cand, s.cfg, stream.NodeID, mode), stream.FD, s.log)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s (capture %s): %v", cand.Element, mode, err))
				continue
			}

			// The live probe: did it survive long enough to believe?
			if err := liveProbe(kid, s.opts.LiveProbeWindow); err != nil {
				failures = append(failures, fmt.Sprintf("%s (capture %s): %v", cand.Element, mode, err))
				s.log.Warn("live pipeline died inside its probe window, trying the next rung",
					"encoder", cand.Element, "capture", mode, "err", err)
				continue
			}

			s.mu.Lock()
			s.kid = kid
			s.encoder = cand.Name
			s.mu.Unlock()
			if s.opts.OnEncoderChosen != nil {
				s.opts.OnEncoderChosen(cand.Element)
			}
			s.log.Info("encoding in hardware",
				"encoder", cand.Element, "capture", mode.String(),
				"width", s.cfg.Width, "height", s.cfg.Height,
				"fps", s.cfg.Fps, "bitrate_bps", s.cfg.BitrateBps, "gop_ms", s.cfg.GOPMs)

			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.pump(kid)
			}()
			return nil
		}
	}

	if allFailuresInsidePipeWireSrc(failures) {
		return fmt.Errorf("%w: every pipeline died inside pipewiresrc before frames reached an encoder\n  %s",
			engine.ErrCaptureFormat, strings.Join(failures, "\n  "))
	}
	if len(order) == 1 {
		return fmt.Errorf("encoder %s failed to start:\n  %s", order[0].Element, strings.Join(failures, "\n  "))
	}
	return fmt.Errorf("%w\n  tried: %v", engine.ErrNoHardwareEncoder, failures)
}

// allFailuresInsidePipeWireSrc reports whether every live failure died inside
// the portal capture element. When that is true the encoders are innocent —
// no frame ever reached one — and "no working hardware encoder" would be the
// wrong diagnosis (see engine.ErrCaptureFormat).
func allFailuresInsidePipeWireSrc(failures []string) bool {
	if len(failures) == 0 {
		return false
	}
	for _, f := range failures {
		if !strings.Contains(strings.ToLower(f), "pipewiresrc") {
			return false
		}
	}
	return true
}

// liveProbe waits out the probe window, returning the child's error if it died
// inside it.
func liveProbe(kid *child, window time.Duration) error {
	died := make(chan error, 1)
	go func() { died <- kid.wait() }()

	select {
	case err := <-died:
		if err == nil {
			return errors.New("pipeline exited immediately without producing a stream")
		}
		return err
	case <-time.After(window):
		return nil // still alive: believe it
	}
}

// candidatesFrom returns the cascade starting at `first`. An explicit override
// gets exactly one candidate: the user asked for it, and silently running a
// different encoder than the one they named would be worse than failing.
func candidatesFrom(first Candidate, override bool) []Candidate {
	if override {
		return []Candidate{first}
	}
	out := []Candidate{first}
	seen := false
	for _, c := range Cascade {
		if c.Element == first.Element {
			seen = true
			continue
		}
		if seen {
			out = append(out, c)
		}
	}
	return out
}

// pump demuxes the child's stdout into access units.
func (s *Source) pump(kid *child) {
	defer close(s.frames)

	// The AU bound is the relay's: an access unit larger than it would accept
	// is useless to us anyway.
	demux := mpegts.NewDemuxer(wire.MaxKeyframeBytes, func(au mpegts.AU) error {
		// Stamped on arrival, from the engine's clock — the same one TimeSync
		// reads (Decision 6). The bias this introduces (capture + encode + mux)
		// is small and roughly constant because every candidate is pinned to
		// ≤1 frame of internal latency; V4 measures it against a physical
		// reference, and clock-anchored PES PTS is the fix if it fails.
		frame := engine.AccessUnit{
			Data:        au.Data,
			Keyframe:    engine.HasIDR(au.Data),
			TimestampUs: s.clock.NowUs(),
			PTSUs:       au.PTS,
			HasPTS:      au.HasPTS,
		}
		select {
		case s.frames <- frame:
			return nil
		default:
			// The sender is behind. Dropping here is consistent with the whole
			// project: frames over stalls. Blocking would stall the demuxer,
			// then the pipe, then the encoder — backpressure all the way into
			// the GPU.
			s.log.Debug("dropping access unit: sender is behind")
			return nil
		}
	})

	_, copyErr := io.Copy(demux, kid.stdout)
	if copyErr == nil {
		copyErr = demux.Close()
	}
	waitErr := kid.wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return // a clean stop: the child died because we killed it
	}
	switch {
	case waitErr != nil:
		s.err = waitErr
	case copyErr != nil:
		s.err = copyErr
	default:
		s.err = errors.New("capture ended: the GStreamer pipeline stopped unexpectedly")
	}
}

// Stop kills the child and releases the portal session.
func (s *Source) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	kid, stream := s.kid, s.stream
	s.mu.Unlock()

	if kid != nil {
		kid.stop()
	}
	s.wg.Wait()
	if stream != nil {
		return stream.Close()
	}
	return nil
}

// Encoder names the candidate in use.
func (s *Source) Encoder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoder
}

// Err returns why capture ended, or nil for a clean stop.
func (s *Source) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// realTrial encodes videotestsrc — never the portal (Decision 4).
func (s *Source) realTrial(ctx context.Context, c Candidate, cfg engine.MediaConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return runToCompletion(ctx, s.opts.Binary, BuildTrialPipeline(c, cfg), s.log)
}
