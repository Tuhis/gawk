package gst

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// audioProbeBudget bounds the *whole* audio pre-flight, not each candidate.
//
// A per-candidate timeout multiplies: two candidates that each open and then
// never produce would push the share picker eight seconds further away for a
// broadcaster who is waiting to start a game. One budget for the cascade keeps
// the worst case flat as candidates are added, and running out of it is the
// correct answer anyway — a source that has not produced a buffer in this long
// is not one to publish with.
const audioProbeBudget = 8 * time.Second

// audioQueueDepth is the bounded channel between the demuxer and the sender:
// ~32 packets ≈ 640 ms at 20 ms per packet (docs/28 Decision 9).
const audioQueueDepth = 32

// Options configure the capture factory.
type Options struct {
	// LastGoodEncoder is the cached cascade winner, re-verified before use.
	LastGoodEncoder string
	// OnEncoderChosen persists the winner.
	OnEncoderChosen func(string)
	// LastGoodAudioSource is the cached audio cascade winner, re-verified
	// before use exactly like LastGoodEncoder.
	LastGoodAudioSource string
	// OnAudioSourceChosen persists the audio winner.
	OnAudioSourceChosen func(string)

	// Binary overrides gst-launch-1.0 (tests, unusual installs).
	Binary string
	// Trial overrides the trial-encode runner (tests).
	Trial TrialFunc
	// AudioTrial overrides the audio trial runner (tests).
	AudioTrial AudioTrialFunc
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
		if s.opts.AudioTrial == nil {
			s.opts.AudioTrial = s.realAudioTrial
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
	// captureMode is the ladder rung the live start settled on, valid once
	// captureModeSet — feeds Stats.CapturePath so a broadcaster can see
	// whether zero-copy actually won without reading child logs.
	captureMode    CaptureMode
	captureModeSet bool
	err            error
	stream         *portal.Stream
	// tgt is the granted node plus the fitted encode geometry (R35 D2),
	// computed once when the portal returns and reused by every cascade
	// attempt — the fit is taken at start, never per rung.
	tgt Target
	// sourceType is what the picker returned; it is the mode switch for the
	// whole milestone (D1) and what the shells report.
	sourceType portal.SourceType
	kid        *child
	stopped    bool

	frames chan engine.AccessUnit
	// audio carries Opus packets when a source won the pre-flight cascade;
	// nil means this session is video-only and always was (R25). Its depth is
	// the drop-oldest queue of Decision 9.
	audio chan engine.AudioPacket
	// audioCand is the winning candidate, audioOK whether audio is still
	// expected to flow (a live failure can revoke it mid-start), and
	// audioState what the shells report.
	audioCand  *AudioCandidate
	audioOK    bool
	audioState engine.AudioState

	wg sync.WaitGroup

	// dump tees every child's MPEG-TS output to disk when GAWK_DUMP_TS is
	// set — the ground-truth instrument for "is the picture black at the
	// source or broken on the viewer?". Written only by the (sequential)
	// pumps; opened in Start, closed in Stop.
	dump *os.File
	// dumpES tees the demuxed Annex-B elementary stream (GAWK_DUMP_H264):
	// every access unit exactly as the MPEG-TS demuxer reconstructed it,
	// before any drop policy. Against GAWK_DUMP_TS it isolates the demuxer —
	// identical quality convicts the encoder, damage only here convicts AU
	// reconstruction, damage only on the viewer convicts drops/wire/decode.
	// Same lifecycle as dump.
	dumpES *os.File
}

// pumpHandle tracks one child's demux goroutine. Every child gets a pump the
// moment it starts: a stdout pipe nobody reads fills at ~64 kB, fdsink
// blocks, and the whole encode pipeline stalls for the probe window — then
// bursts the stale backlog when the tap finally opens (field finding,
// 2026-07-17). Until the child is adopted its frames still enter the channel
// (nobody is listening yet, and the bounded channel simply fills), but drops
// stay silent and the pump neither reports an error nor closes the channel —
// a cascade attempt that dies in its probe window is the cascade's failure to
// report, not the session's.
type pumpHandle struct {
	done    chan struct{}
	adopted atomic.Bool

	// anchor maps this child's PES PTS onto the engine clock (see ptsAnchor).
	// Pump-goroutine only; a fresh attempt gets a fresh handle, so a fresh
	// PTS timeline never inherits a stale offset.
	anchor ptsAnchor

	// droppingGOP is the drop-until-keyframe gate, touched only by the pump
	// goroutine. Drops at the frame channel happen *before* the sender
	// assigns frameIds, so the wire shows contiguous ids and the viewer's
	// freeze-on-gap cannot see them — a single dropped delta silently turns
	// every later delta in its GOP into reference-broken garbage (field
	// finding, 2026-07-17: a viewer full of artifacts, flashing recognizable
	// at each IDR). Once one delta is dropped, the rest of the GOP is
	// dropped too: freeze over corruption, enforced on the one edge the
	// viewer cannot police.
	droppingGOP bool
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

	// Audio's pre-flight runs here, before the picker (R25, docs/28 Decision
	// 2). An audio trial opens no dialog, needs no permission and touches no
	// GPU, so it belongs alongside EnsureBinary under the same ordering rule
	// that keeps a machine without GStreamer from being asked to share its
	// screen first: a broadcaster learns "no audio on this machine" before
	// they pick a window, not after. It never returns an error — audio is
	// subordinate and cannot fail a broadcast (Decision 6).
	s.selectAudio(ctx)

	// The picker appears on every Start, by decision (docs/19): we ask what to
	// share each time rather than persisting the choice, so no restore token is
	// passed and none is kept.
	stream, err := s.opts.OpenPortal(ctx, portal.Options{})
	if err != nil {
		return nil, err
	}
	// The geometry is settled here, between the picker and the first pipeline:
	// the portal's reported size is the fit input, and it arrives before any
	// element is launched (R35, docs/39 D2). Per the standing trust-the-frame
	// rule the negotiated caps in the child remain the runtime truth — this is
	// the *request*, and a portal that reported no size falls back to today's
	// exact caps.
	tgt := TargetFor(s.cfg, stream.NodeID, stream.Width, stream.Height)
	s.mu.Lock()
	s.stream = stream
	s.tgt = tgt
	s.sourceType = stream.SourceType
	s.mu.Unlock()
	s.log.Info("capturing a portal stream",
		"source", stream.SourceType.String(),
		"source_width", stream.Width, "source_height", stream.Height,
		"encode_width", tgt.Width, "encode_height", tgt.Height,
		"box_width", s.cfg.Width, "box_height", s.cfg.Height)

	cand, err := SelectEncoder(ctx, s.cfg, s.opts.LastGoodEncoder, s.opts.Trial)
	if err != nil {
		stream.Close()
		return nil, err
	}

	s.dump = s.openDump("GAWK_DUMP_TS", "the raw MPEG-TS stream")
	s.dumpES = s.openDump("GAWK_DUMP_H264", "the demuxed H.264 elementary stream")

	s.frames = make(chan engine.AccessUnit, 8)
	if err := s.startWithCascade(ctx, stream, cand); err != nil {
		stream.Close()
		s.closeDumps()
		return nil, err
	}
	return s.frames, nil
}

// selectAudio runs the audio cascade and records the outcome. It never fails
// the start: every path here ends in a usable state, and three of the four are
// "publish video and say so".
func (s *Source) selectAudio(ctx context.Context) {
	if s.cfg.DisableAudio {
		s.setAudioState(engine.AudioOff)
		s.log.Info("system audio disabled by configuration")
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, audioProbeBudget)
	defer cancel()
	cand, err := SelectAudioSource(probeCtx, s.cfg.AudioDevice, s.opts.LastGoodAudioSource, s.opts.AudioTrial)
	if err != nil {
		// Not an error the user has to act on: a machine with no usable audio
		// source publishes video exactly as it did before R25.
		s.log.Warn("no system-audio source on this machine; publishing video only", "err", err)
		s.setAudioState(engine.AudioUnavailable)
		return
	}

	s.mu.Lock()
	s.audioCand = &cand
	s.audioOK = true
	s.audioState = engine.AudioActive
	s.audio = make(chan engine.AudioPacket, audioQueueDepth)
	s.mu.Unlock()
	if s.opts.OnAudioSourceChosen != nil {
		s.opts.OnAudioSourceChosen(cand.Name)
	}
	s.log.Info("capturing system audio", "source", cand.Name, "detail", cand.Description)
}

func (s *Source) setAudioState(state engine.AudioState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioState = state
	s.audioOK = state == engine.AudioActive
}

// dropAudio revokes audio for the rest of this start. Called when the live
// pipeline implicates an audio element (Decision 6), never on a video failure.
func (s *Source) dropAudio(why string) {
	s.mu.Lock()
	s.audioCand = nil
	s.audioOK = false
	s.audioState = engine.AudioUnavailable
	s.mu.Unlock()
	s.log.Warn("dropping system audio for this broadcast; video continues", "reason", why)
}

// audioCandidate returns the winning candidate, or nil for a video-only start.
func (s *Source) audioCandidate() *AudioCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioCand
}

// Audio implements engine.AudioSource. Nil when this session has no audio to
// give, which the engine reads as "video-only" and never pumps.
func (s *Source) Audio() <-chan engine.AudioPacket {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audio == nil {
		return nil
	}
	return s.audio
}

// AudioFormat implements engine.AudioSource. ok is false whenever audio is
// off, unavailable, or was revoked during the cascade.
func (s *Source) AudioFormat() (engine.AudioFormat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.audioOK || s.audioCand == nil {
		return engine.AudioFormat{}, false
	}
	return engine.DefaultAudioFormat(s.audioCand.Name), true
}

// AudioState reports what the shells show: off / unavailable / active. The
// engine derives its own from AudioFormat plus the config, so this is the
// source's own view, not the wire's.
func (s *Source) AudioState() engine.AudioState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audioState == "" {
		return engine.AudioOff
	}
	return s.audioState
}

// openDump opens one debug-dump file named by env, or nil when unset.
func (s *Source) openDump(env, what string) *os.File {
	path := os.Getenv(env)
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		s.log.Warn(env+" set but the file could not be created", "path", path, "err", err)
		return nil
	}
	s.log.Info("dumping "+what, "path", path)
	return f
}

func (s *Source) closeDumps() {
	if s.dump != nil {
		s.dump.Close()
		s.dump = nil
	}
	if s.dumpES != nil {
		s.dumpES.Close()
		s.dumpES = nil
	}
}

// startWithCascade starts the child, and on an immediate death retries —
// first with the capture pinned to system memory (a pipewiresrc negotiation
// death is not the encoder's fault, and the same encoder deserves a second
// try with CPU-visible frames), then by advancing the cascade. Every retry
// reuses the already-granted portal session, so it costs seconds rather than
// another share dialog.
// Audio is an *outer* dimension of the cascade, never a per-rung one (R25,
// docs/28 Decision 7). Three encoders × three capture modes × audio-on/off
// would be 18 attempts and a worst case near a minute of a broadcaster staring
// at nothing. So: run the cascade with audio; on the first live failure whose
// stderr implicates an audio element, drop audio for the rest of the pass; if
// everything still fails, one clean re-run without it. Worst case grows by one
// pass, not by a factor of two, and only on a machine already failing.
func (s *Source) startWithCascade(ctx context.Context, stream *portal.Stream, first Candidate) error {
	audio := s.audioCandidate()
	err := s.cascadePass(ctx, stream, first, audio)
	if err == nil || audio == nil {
		return err
	}
	// Everything failed with audio on. A machine that cannot encode video must
	// not be told the problem is its sound card, so the diagnosis the user
	// finally sees comes from a pass that had no audio in it at all.
	s.log.Warn("every pipeline failed with audio enabled, retrying without it", "err", err)
	s.dropAudio("every encoder and capture mode failed while audio was enabled")
	return s.cascadePass(ctx, stream, first, nil)
}

// cascadePass walks encoders × capture modes once, for one audio decision.
func (s *Source) cascadePass(ctx context.Context, stream *portal.Stream, first Candidate, audio *AudioCandidate) error {
	order := candidatesFrom(first, s.cfg.Encoder != "")
	var failures []string

	for _, cand := range order {
		for _, mode := range CaptureModes {
			// The inner loop runs twice at most: an audio-implicated failure
			// retries this same rung with audio dropped, and dropping it sets
			// `audio` to nil, so the retry cannot repeat.
			for {
				err := s.attempt(ctx, stream, cand, mode, audio)
				if err == nil {
					if s.opts.OnEncoderChosen != nil {
						s.opts.OnEncoderChosen(cand.Element)
					}
					s.log.Info("encoding in hardware",
						"encoder", cand.Element, "capture", mode.String(),
						"width", s.cfg.Width, "height", s.cfg.Height,
						"fps", s.cfg.Fps, "bitrate_bps", s.cfg.BitrateBps, "gop_ms", s.cfg.GOPMs,
						"audio", audio != nil)
					s.mu.Lock()
					s.encoder = cand.Name
					s.captureMode, s.captureModeSet = mode, true
					s.mu.Unlock()
					return nil
				}
				failures = append(failures, fmt.Sprintf("%s (capture %s): %v", cand.Element, mode, err))
				s.log.Warn("live pipeline died inside its probe window, trying the next rung",
					"encoder", cand.Element, "capture", mode, "err", err)

				if audio != nil && failureNamesAudioElement(err.Error(), *audio) {
					// This rung was never given a fair chance: the pipeline
					// died in the audio branch, so retry it exactly as it is,
					// video-only. Advancing the cascade here would burn
					// encoders over a sound card.
					s.dropAudio("the live pipeline died naming an audio element")
					audio = nil
					continue
				}
				break
			}
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

// attempt starts one child and runs the live probe, adopting it on success.
//
// On failure it waits out the child's pump and flushes both queues, so the
// next attempt starts clean: the next attempt may run a different encoder, and
// its viewers must never see two SPS lineages interleaved.
func (s *Source) attempt(ctx context.Context, stream *portal.Stream, cand Candidate, mode CaptureMode, audio *AudioCandidate) error {
	kid, err := startChild(ctx, s.opts.Binary, BuildPipeline(cand, s.cfg, s.encodeTarget(), mode, audio), stream.FD, s.log)
	if err != nil {
		return err
	}

	// The pump starts with the child, not after the probe: an unread stdout
	// pipe blocks fdsink at ~64 kB and stalls the encoder for the whole probe
	// window (see pumpHandle).
	h := &pumpHandle{done: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pump(kid, h)
	}()

	// The live probe: did it survive long enough to believe?
	if err := liveProbe(kid, s.opts.LiveProbeWindow); err != nil {
		<-h.done
		drain(s.frames)
		drain(s.audio)
		return err
	}

	h.adopted.Store(true)
	s.mu.Lock()
	s.kid = kid
	s.mu.Unlock()
	return nil
}

// drain empties a queue left behind by a dead attempt.
func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
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
func (s *Source) pump(kid *child, h *pumpHandle) {
	defer close(h.done)

	// The AU bound is the relay's: an access unit larger than it would accept
	// is useless to us anyway.
	demux := mpegts.NewDemuxer(wire.MaxKeyframeBytes, func(au mpegts.AU) error {
		s.emitAU(h, au)
		return nil
	})

	// Audio rides the same demuxer, the same goroutine and — the load-bearing
	// part — the *same* ptsAnchor (R25, docs/28 Decision 5). Registered only
	// when a source won the pre-flight: without it the demuxer never even
	// resolves the audio PID.
	if s.audioChan() != nil {
		demux.OnAudioPacket(func(p mpegts.AudioPacket) { s.emitAudio(h, p) })
	}

	var src io.Reader = kid.stdout
	if s.dump != nil {
		// Debug tee (GAWK_DUMP_TS). A write failure here fails the copy and
		// with it the capture — acceptable for a debugging knob.
		src = io.TeeReader(src, s.dump)
	}
	_, copyErr := io.Copy(demux, src)
	if copyErr == nil {
		copyErr = demux.Close()
	}
	waitErr := kid.wait()

	if !h.adopted.Load() {
		// A cascade attempt that lost its probe: its post-mortem is the
		// cascade's failure list, and the frame channel belongs to whoever
		// wins.
		return
	}
	defer close(s.frames)
	if ch := s.audioChan(); ch != nil {
		// The engine's audio pump ends the same way the frame pump does.
		defer close(ch)
	}

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

// emitAU stamps one access unit and offers it to the frame channel.
//
// Timestamps are clock-anchored PES PTS (Decision 6's upgrade path, taken
// 2026-07-17 — see ptsAnchor): capture cadence on the engine clock, the same
// one TimeSync reads. Arrival stamping clumped timestamps behind the
// encode/mux/pipe buffering and intermittently cratered viewer decode fps via
// the R12 pacing path.
func (s *Source) emitAU(h *pumpHandle, au mpegts.AU) {
	if s.dumpES != nil {
		// Debug tee (GAWK_DUMP_H264): pre-policy, so the file is the
		// demuxer's exact output. Best-effort — a dump problem must not take
		// the capture down.
		_, _ = s.dumpES.Write(au.Data)
	}
	s.offer(engine.AccessUnit{
		// Cloned because the demuxer reclaims au.Data the moment this
		// callback returns (mpegts.AU: "copy to retain"), while the frame
		// outlives it in the channel — an aliased slice gets rewritten by the
		// AUs demuxed behind it.
		Data:        bytes.Clone(au.Data),
		Keyframe:    engine.HasIDR(au.Data),
		TimestampUs: h.anchor.stamp(s.clock.NowUs(), au.PTS, au.HasPTS),
		PTSUs:       au.PTS,
		HasPTS:      au.HasPTS,
	}, h)
}

// emitAudio stamps one Opus packet and offers it to the audio queue.
//
// It reads **h.anchor** — the same instance emitAU reads, which is the whole
// of Decision 5. Both media are muxed by one mpegtsmux from one pipeline
// running time, so they share one PTS timeline; one anchor maps both with one
// affine function, and the relative A/V skew it introduces is exactly zero, by
// construction. Giving audio its own anchor would look tidier and would
// silently reintroduce a (video path latency − audio path latency) bias — a
// constant lip-sync error the viewer can neither detect nor remove.
// TestOneAnchorStampsBothMedia is what stops a later refactor splitting it.
func (s *Source) emitAudio(h *pumpHandle, p mpegts.AudioPacket) {
	s.offerAudio(engine.AudioPacket{
		// Cloned for the same reason video is: the demuxer reclaims the
		// buffer when this callback returns, while the packet outlives it in
		// the channel.
		Data:        bytes.Clone(p.Data),
		TimestampUs: h.anchor.stamp(s.clock.NowUs(), p.PTS, p.HasPTS),
	})
}

// offer applies the drop policy and hands one access unit to the frame
// channel. Dropping (never blocking) is the project's standing answer to a
// slow consumer — blocking would stall the demuxer, then the pipe, then the
// encoder, backpressure all the way into the GPU — but a drop here is
// invisible to the viewer (see pumpHandle.droppingGOP), so it must take the
// GOP remainder with it:
//
//   - a delta that does not fit ends its GOP: everything until the next
//     keyframe is dropped too, because it would decode against a reference
//     the sender never saw;
//   - a keyframe always wins: if the queue is full, the stale frames ahead
//     of it are flushed — they all predate it, the decoder resets at an IDR
//     anyway, and jumping to the fresh keyframe is a latency win.
//
// Single producer (the pump goroutine); the engine is the only consumer, so
// the flush loop can only ever race the queue *emptier*, never fuller.
func (s *Source) offer(frame engine.AccessUnit, h *pumpHandle) {
	if h.droppingGOP && !frame.Keyframe {
		return // poisoned GOP: the reference chain is already broken
	}
	if frame.Keyframe {
		for flushed := false; ; {
			select {
			case s.frames <- frame:
				h.droppingGOP = false
				return
			default:
			}
			if flushed {
				// Still no room after a flush: the consumer is wedged mid-
				// receive. Drop the keyframe; the gate stays up until the
				// next one.
				break
			}
			for drained := false; !drained; {
				select {
				case <-s.frames:
				default:
					drained = true
				}
			}
			flushed = true
		}
	} else {
		select {
		case s.frames <- frame:
			return
		default:
		}
	}
	h.droppingGOP = true
	// Before adoption the drop is expected (nobody is listening during the
	// probe window), so it stays silent.
	if h.adopted.Load() {
		s.log.Debug("sender is behind, dropping until the next keyframe", "keyframe", frame.Keyframe)
	}
}

// audioChan returns the packet channel, or nil for a video-only session.
func (s *Source) audioChan() chan engine.AudioPacket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audio
}

// offerAudio hands one Opus packet to the queue, evicting the **oldest** on
// overflow — deliberately the opposite of the video path's drop-newest
// (docs/28 Decision 9, following docs/24 finding 14).
//
// Audio is an in-order, live-edge stream with no GOP, so there is nothing to
// poison and nothing to resync to: shedding the backlog keeps the listener
// near live, where shedding the newcomer would strand them as far behind as
// the queue is deep. One dropped packet costs the viewer one 20 ms
// concealment, which its buffer already knows how to do.
//
// Single producer (the pump goroutine); the engine is the only consumer, so
// the eviction can only ever race the queue *emptier*, never fuller.
func (s *Source) offerAudio(p engine.AudioPacket) {
	ch := s.audioChan()
	if ch == nil {
		return
	}
	select {
	case ch <- p:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- p:
	default:
		// The consumer refilled the slot between the two operations. Shedding
		// the newcomer here is one 20 ms concealment, and the next packet is
		// 20 ms away.
	}
	s.log.Debug("audio queue full, dropped the oldest packet")
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
	s.closeDumps()
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

// CapturePath reports the ladder rung the live start settled on:
// "zero-copy (capped)" (DMA-BUF with the compositor throttled to the stream
// rate), "zero-copy" (DMA-BUF, compositor delivers at its own rate) or
// "system-memory" (one CPU copy per frame); "" until capture is live.
func (s *Source) CapturePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.captureModeSet {
		return ""
	}
	switch s.captureMode {
	case CaptureAutoCapped:
		return "zero-copy (capped)"
	case CaptureSystemMemory:
		return "system-memory"
	default:
		return "zero-copy"
	}
}

// encodeTarget returns the node + fitted geometry every attempt builds from.
func (s *Source) encodeTarget() Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tgt
}

// EncodeSize implements engine.GeometrySource: the dimensions actually asked
// of the encoder, which are the configured box only when the source's aspect
// matches it (R35 D2). ok is false before the portal has answered, where the
// engine keeps reporting the configured rung.
func (s *Source) EncodeSize() (int, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tgt.Width <= 0 || s.tgt.Height <= 0 {
		return 0, 0, false
	}
	return s.tgt.Width, s.tgt.Height, true
}

// ShareMode implements engine.ShareModeSource: "screen" or "window", or ""
// before the picker has answered. It is the user's own vocabulary rather than
// the portal's ("monitor"), because it is rendered in the GUI and pasted into
// diagnostics.
func (s *Source) ShareMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stream == nil:
		return ""
	case s.sourceType.IsWindow():
		return "window"
	default:
		return "screen"
	}
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

// realAudioTrial captures ~500 ms and encodes it. Bounded by the caller's
// audioProbeBudget rather than its own timeout: a source that opens and never
// produces is exactly as broken as one that fails to open, and neither may
// hang startup.
func (s *Source) realAudioTrial(ctx context.Context, c AudioCandidate, device string) error {
	return runToCompletion(ctx, s.opts.Binary, BuildAudioTrialPipeline(c, device), s.log)
}
