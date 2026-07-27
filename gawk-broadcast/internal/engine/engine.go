// Package engine is the native Linux broadcaster's engine (R14, docs/19): it
// captures the screen through the XDG ScreenCast portal, encodes it in
// hardware via a GStreamer child, and publishes the result to the relay using
// the same publisher protocol the browser broadcaster speaks — byte for byte,
// via the shared gawk-server/wire package.
//
// The surface deliberately mirrors the TypeScript BroadcastSessionLike
// (start/stop) and BroadcastCallbacks (onBroadcastId/onStats/onError/onEnded)
// from gawk-app/src/transport/broadcaster.ts. Same idea, same names, other
// language: a second broadcaster only stays survivable if the two remain
// legible to each other.
//
// It is a package rather than a main package because the GUI requirement
// determines the shape — a main full of flags and globals would have to be
// dismantled to grow a window. Both shells (cmd/gawk-broadcast,
// cmd/gawk-broadcast-gui) are thin consumers of what follows. Accordingly:
// no package globals, no os.Exit, no direct stdio anywhere in this package.
package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// announceReadLimit bounds each server-message read. Both messages the relay
// sends a publisher — BroadcastAnnounce and ResumeToken — are at most 3 + 255
// bytes by construction (wire.AppendBroadcastAnnounce/AppendResumeToken), so
// anything larger is a misbehaving or hostile relay and must not be able to
// grow our heap.
const announceReadLimit = 258

// announceReadTimeout bounds each server-message read. A relay that opens a
// stream and then says nothing must not pin the reader forever (Decision 10).
const announceReadTimeout = 10 * time.Second

// Config is a session's fixed configuration. Everything here is decided before
// Start and never changes during a session (Decision 9: no ladder, no
// mid-session rung changes in v1).
type Config struct {
	// RelayURL is the relay's https:// origin.
	RelayURL string
	// BroadcastID empty mints a new broadcast; non-empty reclaims that ID
	// (the Resume path).
	BroadcastID string
	// PublishSecret is R2's pre-shared publish secret, sent as a query param.
	PublishSecret string
	// ResumeToken is the hex token the relay minted at first publish (R17
	// W2, wire 0x09), sent as the `resume` query param. An R17 relay refuses
	// every /publish/{id} claim without it, so Resume needs the token
	// persisted beside the broadcast ID. Ignored when BroadcastID is empty.
	ResumeToken string
	// Origin is the Origin header sent on the CONNECT dial. Empty uses
	// DefaultOrigin. A relay configured with -allowed-origins must whitelist
	// whichever value is sent.
	Origin string
	// Insecure skips TLS verification (the dev cert only).
	Insecure bool
	// Media is the capture/encode configuration.
	Media MediaConfig
}

// Callbacks mirror the browser's BroadcastCallbacks. All are optional; all are
// invoked from engine goroutines, so a GUI shell must marshal them onto its
// own loop.
type Callbacks struct {
	// OnBroadcastID fires once, when the relay announces the code.
	OnBroadcastID func(id string)
	// OnResumeToken fires when the relay hands over the broadcast's resume
	// token (hex — the relay's own query-param encoding). The shell must
	// persist it beside the broadcast ID: without it an R17 relay refuses
	// the reclaim. Pre-R17 relays never send one.
	OnResumeToken func(token string)
	// OnStats fires on the stats cadence while live.
	OnStats func(Stats)
	// OnError reports a session-fatal problem. Always followed by OnEnded.
	OnError func(error)
	// OnEnded fires exactly once per successful Start, when the session is
	// over — whether by Stop, an unrecoverable relay loss, or child death.
	//
	// A *recoverable* relay loss does not reach here: auto-resume reclaims the
	// broadcast and the session continues (OnResuming → OnResumed).
	OnEnded func()
	// OnResuming fires before each reclaim attempt after the transport
	// dropped. attempt counts from 1; lastErr is why the previous attempt
	// failed, nil on the first. Capture and encode are still running — this is
	// a transport-only interruption, and the shell should say so rather than
	// present the broadcast as ended.
	OnResuming func(attempt int, lastErr error)
	// OnResumed fires when a reclaim succeeds and frames are flowing again.
	OnResumed func()
	// OnEncoderChosen fires once the cascade settles on a candidate
	// (Decision 4: the chosen encoder is reported at startup and in stats).
	OnEncoderChosen func(encoder string)
	// OnTelemetryHello fires once per relay session with this session's
	// telemetry identity (R28, wire 0x0D): the ingest token, the obfuscated
	// broadcast key, and whether the fleet collects at all. Absent on a relay
	// predating R28 or with telemetry off — which is exactly the same thing
	// as a shell that installs no callback: nothing is collected or sent.
	OnTelemetryHello func(hello wire.TelemetryHello)
	// OnViewerCount fires whenever the relay pushes the live "N watching"
	// number (R18, docs/23 — the SubscriberCount R14 deferred as Decision
	// 18; the wire message exists now, TypeViewerCount 0x0B). The count is
	// fleet-global in cluster mode; ~1 s cadence, change-driven. The "first
	// viewer joined" moment is the 0 → ≥1 transition, derived by the shell.
	OnViewerCount func(count uint32)
}

func (c Callbacks) broadcastID(id string) {
	if c.OnBroadcastID != nil {
		c.OnBroadcastID(id)
	}
}

func (c Callbacks) resumeToken(token string) {
	if c.OnResumeToken != nil {
		c.OnResumeToken(token)
	}
}

func (c Callbacks) telemetryHello(h wire.TelemetryHello) {
	if c.OnTelemetryHello != nil {
		c.OnTelemetryHello(h)
	}
}

func (c Callbacks) stats(s Stats) {
	if c.OnStats != nil {
		c.OnStats(s)
	}
}

func (c Callbacks) err(e error) {
	if c.OnError != nil {
		c.OnError(e)
	}
}

func (c Callbacks) ended() {
	if c.OnEnded != nil {
		c.OnEnded()
	}
}

func (c Callbacks) resuming(attempt int, lastErr error) {
	if c.OnResuming != nil {
		c.OnResuming(attempt, lastErr)
	}
}

func (c Callbacks) resumed() {
	if c.OnResumed != nil {
		c.OnResumed()
	}
}

func (c Callbacks) encoderChosen(name string) {
	if c.OnEncoderChosen != nil {
		c.OnEncoderChosen(name)
	}
}

func (c Callbacks) viewerCount(count uint32) {
	if c.OnViewerCount != nil {
		c.OnViewerCount(count)
	}
}

// Options are the injection points. Zero values give the production wiring.
type Options struct {
	// Clock defaults to NewClock(). Tests inject FakeClock.
	Clock Clock
	// Dial defaults to dialRelay. Tests inject a fake session.
	Dial DialFunc
	// MediaFactory defaults to the portal+GStreamer source. Tests inject a
	// fake. The engine always calls it with its own Clock (Decision 6).
	MediaFactory MediaSourceFactory
	// Log defaults to a discard logger — the engine never writes to stdio of
	// its own accord (a GUI has no stderr worth speaking of).
	Log *slog.Logger
	// StatsInterval defaults to 1s.
	StatsInterval time.Duration
}

// Session is one broadcast. Start/Stop mirror BroadcastSessionLike.
//
// Deliberately absent: SetLadder/SetEncoderSettings (Decision 9). A rung
// change means restarting the GStreamer child, and keeping a dead-code setter
// "for symmetry" is a footgun. The restore token makes a restart-based SetRung
// cheap to add later, as its own task.
type Session struct {
	cfg          Config
	cb           Callbacks
	clock        Clock
	dial         DialFunc
	mediaFactory MediaSourceFactory
	log          *slog.Logger

	statsInterval time.Duration

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	media   MediaSource

	// relayCur is the session frames are travelling on right now. It changes
	// under the supervisor when auto-resume reclaims the broadcast, so it has
	// its own lock rather than living under mu: the send path reads it per
	// frame, and mu is held across callbacks.
	relayMu  sync.RWMutex
	relayCur RelaySession

	wg      sync.WaitGroup
	endOnce sync.Once

	sender *sender
	ts     *TimeSyncClient

	// clockPub is re-armed on every resume, so it outlives one connection.
	clockPubMu sync.Mutex
	clockPub   *clockMappingPublisher

	// Auto-resume state (guarded by mu).
	resumes  uint64
	resuming bool

	// Windowing state for the fps rates in Stats.
	rateMu      sync.Mutex
	lastRateUs  uint64
	lastEncoded uint64
	lastSent    uint64

	// R18 viewer-count state, written by the datagram read loop (guarded by
	// mu; known stays false until the relay's first push lands).
	viewerCount      uint32
	viewerCountKnown bool
}

// New builds a Session. It performs no I/O; Start does.
func New(cfg Config, cb Callbacks, opts Options) *Session {
	s := &Session{
		cfg:           cfg,
		cb:            cb,
		clock:         opts.Clock,
		dial:          opts.Dial,
		mediaFactory:  opts.MediaFactory,
		log:           opts.Log,
		statsInterval: opts.StatsInterval,
	}
	if s.clock == nil {
		s.clock = NewClock()
	}
	if s.dial == nil {
		s.dial = dialRelay
	}
	if s.mediaFactory == nil {
		// The engine deliberately knows nothing about GStreamer, PipeWire or
		// D-Bus: the shells compose the platform half (gst.NewFactory) and
		// inject it here. That layering is what keeps everything above this
		// line testable without a GPU, a portal or a child process.
		s.mediaFactory = func(MediaConfig, Clock, *slog.Logger) (MediaSource, error) {
			return nil, errors.New("engine: no MediaFactory configured")
		}
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.statsInterval <= 0 {
		s.statsInterval = time.Second
	}
	return s
}

// Start connects, then captures. It returns a *StartError on failure.
//
// The order is not incidental (Decision 10 / connect-before-picker, mirroring
// the browser): dial first, so that if the relay refuses — wrong secret, ID
// taken, at capacity — no share dialog ever appears. Being asked to pick a
// screen and only then told "someone is already publishing" is the exact
// experience this ordering exists to prevent.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("engine: session already started")
	}
	s.started = true
	s.mu.Unlock()

	url, err := PublishURL(s.cfg.RelayURL, s.cfg.BroadcastID, s.cfg.PublishSecret, s.cfg.ResumeToken)
	if err != nil {
		return &StartError{Phase: PhaseConnect, Err: fmt.Errorf("bad relay URL: %w", err)}
	}

	origin := s.cfg.Origin
	if origin == "" {
		origin = DefaultOrigin
	}
	relay, status, err := s.dial(ctx, url, origin, s.cfg.Insecure)
	if err != nil {
		return &StartError{Phase: PhaseConnect, Status: status, Err: err}
	}

	// From here on every failure must tear down what we hold, or we leak a
	// zombie publisher holding the broadcast ID hostage (CODE-REVIEW.md:
	// "Error paths must release what they acquired"). One shared teardown,
	// reached by success and failure alike.
	sessCtx, cancel := context.WithCancel(context.Background())
	s.setRelay(relay)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	if err := s.startLive(sessCtx, relay); err != nil {
		s.teardown()
		var se *StartError
		if errors.As(err, &se) {
			return se
		}
		return &StartError{Phase: PhaseCapture, Err: err}
	}
	return nil
}

// startLive brings up everything that runs for the session's lifetime.
func (s *Session) startLive(ctx context.Context, relay RelaySession) error {
	// Relay clock sync (R5 Q2). Pings ride the ordinary datagram path; the
	// read loop exists solely to catch replies, since the relay sends a
	// publisher nothing else as datagrams.
	//
	// Both of these outlive any one connection — the sender keeps its frameId
	// space and derived config across a resume, and the TimeSync client keeps
	// its cadence — so they take the *current* relay through s rather than
	// capturing the one they were built with.
	s.ts = NewTimeSyncClient(s.sendDatagram, s.clock)
	s.sender = newSender(relay, s.clock, s.log)
	s.clockPub = newClockMappingPublisher()

	// The server-message read (announce + resume token) is detached on
	// purpose: media must never wait on it (docs/06 says so twice, and R1's
	// review found an implementation that awaited it before capture and
	// could hang forever). Only the UI consumes the code and the token.
	s.attach(ctx, relay)

	// Capture + encode. Everything past this point is the capture phase: a
	// failure here had a live publisher session, so the caller must not treat
	// it as licence to mint a different ID.
	//
	// The source is built with the session's own clock — the mechanism behind
	// Decision 6's shared-clock requirement.
	media, err := s.mediaFactory(s.cfg.Media, s.clock, s.log)
	if err != nil {
		return &StartError{Phase: PhaseCapture, Err: err}
	}
	s.mu.Lock()
	s.media = media
	s.mu.Unlock()

	frames, err := media.Start(ctx)
	if err != nil {
		return &StartError{Phase: PhaseCapture, Err: err}
	}
	if name := media.Encoder(); name != "" {
		s.cb.encoderChosen(name)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pump(ctx, frames)
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.periodic(ctx)
	}()

	// The transport ending is one event with one authoritative signal
	// (CODE-REVIEW.md): the relay session's context. What it means is now the
	// supervisor's decision, not "the broadcast is over" — a lost transport is
	// recoverable while the relay still holds the ID.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.supervise(ctx, relay)
	}()
	return nil
}

// attach wires the per-connection read loops and makes relay the one frames
// travel on. Called once at start and once per successful resume.
//
// The wg.Add here happens on the supervisor goroutine, which is itself
// inside wg — so the counter is never zero when Add runs, and Stop's Wait
// cannot race it (sync.WaitGroup's contract).
func (s *Session) attach(ctx context.Context, relay RelaySession) {
	s.setRelay(relay)
	s.sender.setRelay(relay)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.readServerMessages(ctx, relay)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.readDatagrams(ctx, relay)
	}()
}

func (s *Session) currentRelay() RelaySession {
	s.relayMu.RLock()
	defer s.relayMu.RUnlock()
	return s.relayCur
}

func (s *Session) setRelay(relay RelaySession) {
	s.relayMu.Lock()
	s.relayCur = relay
	s.relayMu.Unlock()
}

// sendDatagram sends on whichever session is current — the indirection that
// lets TimeSync and ClockMapping survive a resume without being rebuilt.
func (s *Session) sendDatagram(b []byte) error {
	relay := s.currentRelay()
	if relay == nil {
		return errors.New("engine: no relay session")
	}
	return relay.SendDatagram(b)
}

// readServerMessages reads the relay's server-initiated uni streams for the
// session's life. An R17 relay sends two — BroadcastAnnounce (0x03) and
// ResumeToken (0x09), each on its own stream — and the client sees them in
// NO guaranteed order: webtransport-go demultiplexes incoming streams
// concurrently, so accept order is not the server's open order (the PR #47
// rebase measured the token beating the announce in about half of real
// dials). Every stream is therefore read bounded and dispatched by wire
// type, exactly like broadcaster.ts; unknown types are ignored so a newer
// relay (version skew during a rolling upgrade) cannot break us.
func (s *Session) readServerMessages(ctx context.Context, relay RelaySession) {
	for {
		str, err := relay.AcceptUniStream(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("server message stream not accepted", "err", err)
			}
			return
		}
		s.readServerMessage(ctx, str)
	}
}

// readServerMessage reads one server message to completion and dispatches it
// by type. Bounded and deadlined: a relay that opens a stream and never
// speaks, or speaks a megabyte, must not hang or grow us.
func (s *Session) readServerMessage(ctx context.Context, str ReceiveStream) {
	// A pending Read does not watch ctx, so ending the session must actively
	// unblock it: without this, Stop() waits out the full read timeout
	// before returning — closing the GUI window would appear to hang.
	stopWatch := context.AfterFunc(ctx, func() {
		_ = str.SetReadDeadline(time.Now().Add(-time.Second))
	})
	defer stopWatch()
	_ = str.SetReadDeadline(time.Now().Add(announceReadTimeout))
	buf, err := io.ReadAll(io.LimitReader(str, announceReadLimit))
	if err != nil {
		s.log.Warn("server message read failed", "err", err)
		return
	}
	if len(buf) < 2 {
		s.log.Warn("server message ignored: truncated", "len", len(buf))
		return
	}
	switch buf[1] {
	case wire.TypeBroadcastAnnounce:
		id, err := wire.ParseBroadcastAnnounce(buf)
		if err != nil {
			s.log.Warn("announce parse failed", "err", err)
			return
		}
		s.mu.Lock()
		s.cfg.BroadcastID = id
		s.mu.Unlock()
		s.cb.broadcastID(id)
	case wire.TypeResumeToken:
		raw, err := wire.ParseResumeToken(buf)
		if err != nil {
			s.log.Warn("resume token parse failed", "err", err)
			return
		}
		token := hex.EncodeToString(raw)
		s.mu.Lock()
		s.cfg.ResumeToken = token
		s.mu.Unlock()
		s.cb.resumeToken(token)
	case wire.TypeTelemetryHello:
		// R28 (docs/33 D2): this session's telemetry identity. Parsed and
		// handed out; the engine itself never collects or sends anything —
		// the reporter lives in the shell, beside the notification and stats
		// wiring, so a session with no reporter attached is byte-identical to
		// one before R28.
		hello, err := wire.ParseTelemetryHello(buf)
		if err != nil {
			s.log.Warn("telemetry hello parse failed", "err", err)
			return
		}
		s.cb.telemetryHello(hello)
	default:
		s.log.Debug("server message ignored: unknown type", "type", buf[1])
	}
}

// readDatagrams drains the session's incoming datagrams: the R18 viewer-count
// push is dispatched by wire type, TimeSync replies go to the sync client, and
// anything else is drained and ignored rather than left to fill a queue.
func (s *Session) readDatagrams(ctx context.Context, relay RelaySession) {
	for {
		dgram, err := relay.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		// R18 (docs/23 Decision 7): the relay pushes the live "N watching"
		// number on this session (~1 s cadence, change-driven).
		if len(dgram) == wire.ViewerCountSize && dgram[1] == wire.TypeViewerCount {
			if count, err := wire.ParseViewerCount(dgram); err == nil {
				s.setViewerCount(count)
			}
			continue
		}
		s.ts.HandleDatagram(dgram)
	}
}

func (s *Session) setViewerCount(count uint32) {
	s.mu.Lock()
	s.viewerCount = count
	s.viewerCountKnown = true
	s.mu.Unlock()
	s.cb.viewerCount(count)
}

// pump moves access units from the media source to the relay.
func (s *Session) pump(ctx context.Context, frames <-chan AccessUnit) {
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-frames:
			if !ok {
				// The media source ended: the child died or capture stopped.
				// That is session-fatal — there is no software fallback to
				// degrade to (Decision 4).
				if err := s.mediaSource().Err(); err != nil && ctx.Err() == nil {
					s.cb.err(fmt.Errorf("capture ended: %w", err))
				}
				s.finish()
				return
			}
			s.sender.send(au)
		}
	}
}

// periodic drives the two cadences that are not frame-driven: TimeSync pings
// and the ClockMapping publication, plus the stats callback.
func (s *Session) periodic(ctx context.Context) {
	pinger := time.NewTicker(TimeSyncInterval)
	defer pinger.Stop()
	// The mapping check runs faster than the mapping cadence so the first
	// mapping goes out promptly after the first pong rather than up to a full
	// interval later; clockMappingPublisher owns the actual timing.
	mapper := time.NewTicker(time.Second)
	defer mapper.Stop()
	stats := time.NewTicker(s.statsInterval)
	defer stats.Stop()

	s.ts.Ping()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pinger.C:
			s.ts.Ping()
		case <-mapper.C:
			sample, ok := s.ts.Sample()
			if s.clockMappingDue(s.clock.NowUs(), ok) {
				if err := s.sendDatagram(wire.AppendClockMapping(nil, sample.OffsetUs)); err != nil {
					s.log.Debug("clock mapping send failed", "err", err)
				}
			}
		case <-stats.C:
			s.cb.stats(s.Stats())
		}
	}
}

// Stats snapshots the session. The fps figures are windowed over the interval
// since the previous call, exactly as the browser derives encoderFps/sentFps
// from encodedSinceStats/dt — so a native diagnostics dump and a browser one
// mean the same thing when read side by side.
func (s *Session) Stats() Stats {
	st := Stats{}
	if s.sender != nil {
		st = s.sender.stats()
	}
	s.rateMu.Lock()
	nowUs := s.clock.NowUs()
	if s.lastRateUs != 0 {
		if dt := float64(nowUs-s.lastRateUs) / 1e6; dt > 0 {
			st.EncoderFps = float64(st.EncodedFrames-s.lastEncoded) / dt
			st.SentFps = float64(st.SentFrames-s.lastSent) / dt
		}
	}
	s.lastRateUs = nowUs
	s.lastEncoded = st.EncodedFrames
	s.lastSent = st.SentFrames
	s.rateMu.Unlock()
	if s.ts != nil {
		if sample, ok := s.ts.Sample(); ok {
			st.TimeSyncAvailable = true
			st.TimeSyncRttMs = sample.RttMs
			st.TimeSyncOffsetUs = sample.OffsetUs
		}
	}
	if m := s.mediaSource(); m != nil {
		st.Encoder = m.Encoder()
		st.CapturePath = m.CapturePath()
	}
	s.mu.Lock()
	st.ViewerCountAvailable = s.viewerCountKnown
	st.ViewerCount = s.viewerCount
	st.Resumes = s.resumes
	st.Resuming = s.resuming
	s.mu.Unlock()
	st.Width = s.cfg.Media.Width
	st.Height = s.cfg.Media.Height
	st.Fps = s.cfg.Media.Fps
	st.BitrateBps = s.cfg.Media.BitrateBps
	// Decision 20: the child owns capture, so this stage is genuinely
	// unobservable from here. Never fabricated from the requested rate.
	st.CaptureFpsAvailable = false
	return st
}

// mediaSource returns the live source, or nil before the capture phase.
func (s *Session) mediaSource() MediaSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.media
}

// BroadcastID returns the session's code once announced.
func (s *Session) BroadcastID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.BroadcastID
}

// Stop ends the session. Idempotent, and safe to call before Start.
func (s *Session) Stop() error {
	s.teardown()
	s.wg.Wait()
	s.finish()
	return nil
}

// teardown releases everything the session holds. Reached by Stop and by every
// failure path in Start — the single shared teardown the browser's
// BroadcastPipeline.teardown() is, for the same reason.
func (s *Session) teardown() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cancel, media := s.cancel, s.media
	s.mu.Unlock()
	relay := s.currentRelay()

	if cancel != nil {
		cancel()
	}
	if media != nil {
		if err := media.Stop(); err != nil {
			s.log.Warn("media stop failed", "err", err)
		}
	}
	if s.sender != nil {
		// Let in-flight keyframe writes settle before closing the session out
		// from under them.
		s.sender.wait()
	}
	if relay != nil {
		// Closing the session releases the publisher slot server-side. Without
		// it the relay holds the broadcast ID until its grace period expires.
		if err := relay.CloseWithError(webtransport.SessionErrorCode(0), ""); err != nil {
			s.log.Debug("relay close failed", "err", err)
		}
	}
}

// finish ends the session: it releases everything the session holds, then
// fires OnEnded exactly once.
//
// The teardown is not optional here, and its absence was a real bug. Before
// this, the relay-loss and capture-death paths called finish() alone, so the
// session context was never cancelled — the GStreamer child kept encoding, the
// portal ScreenCast session kept the screen-sharing indicator lit, and the
// pump kept feeding frames into a dead transport, indefinitely. The GUI shell
// then dropped its Session reference on OnEnded, so nothing could ever stop
// them short of killing the process. One event, one teardown.
func (s *Session) finish() {
	s.teardown()
	s.endOnce.Do(func() { s.cb.ended() })
}
