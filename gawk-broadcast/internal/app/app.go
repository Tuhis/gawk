// Package app is the GUI's logic without the GUI (R14 V5/V6, docs/19).
//
// It exists so the parts worth testing — the reclaim→mint rule, the
// error-to-sentence mapping, the notification urgencies, the diagnostics dump —
// are testable without a window, a compositor or a GPU. cmd/gawk-broadcast-gui
// is then only layout and event wiring over this.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/config"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/gst"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/notify"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
)

// State is what the window renders.
type State int

const (
	// StateIdle: not broadcasting.
	StateIdle State = iota
	// StateStarting: dialling, then capturing.
	StateStarting
	// StateLive: publishing.
	StateLive
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "Starting…"
	case StateLive:
		return "Live"
	default:
		return "Not broadcasting"
	}
}

// SessionFactory builds a session. Injectable so the app's logic can be tested
// against a fake engine.
type SessionFactory func(cfg engine.Config, cb engine.Callbacks, opts engine.Options) Session

// Session is the slice of engine.Session the app uses — exactly
// BroadcastSessionLike, minus the ladder (Decision 9).
type Session interface {
	Start(ctx context.Context) error
	Stop() error
	Stats() engine.Stats
	BroadcastID() string
}

// App holds the GUI's state machine.
type App struct {
	cfg      *config.Config
	notifier notify.Notifier
	newSess  SessionFactory
	log      *slog.Logger
	// invalidate asks the window to redraw. The engine's callbacks arrive on
	// its own goroutines, so every state change has to poke the UI thread.
	invalidate func()

	mu      sync.Mutex
	state   State
	sess    Session
	id      string
	status  string
	lastErr string
	stats   engine.Stats
	// canMint records whether a failed Resume may fall back to minting. It is
	// set only for connect-phase failures (Decision 10).
	canMint bool
}

// Options configure the app.
type Options struct {
	Config   *config.Config
	Notifier notify.Notifier
	Log      *slog.Logger
	// NewSession defaults to the real engine.
	NewSession SessionFactory
	Invalidate func()
}

func New(opts Options) *App {
	a := &App{
		cfg:        opts.Config,
		notifier:   opts.Notifier,
		newSess:    opts.NewSession,
		log:        opts.Log,
		invalidate: opts.Invalidate,
		status:     "Ready",
	}
	if a.notifier == nil {
		a.notifier = notify.Discard{}
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if a.invalidate == nil {
		a.invalidate = func() {}
	}
	if a.newSess == nil {
		a.newSess = func(cfg engine.Config, cb engine.Callbacks, opts engine.Options) Session {
			return engine.New(cfg, cb, opts)
		}
	}
	return a
}

// State returns the current state and status line.
func (a *App) State() (State, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, a.status
}

// BroadcastID returns the live code, or "".
func (a *App) BroadcastID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id
}

// LastError returns the last error as a sentence, or "".
func (a *App) LastError() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// Stats returns the latest engine stats.
func (a *App) Stats() engine.Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

// CanMint reports whether a failed Resume may fall back to minting a new code.
func (a *App) CanMint() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.canMint
}

// JoinLink builds the viewer URL from the app URL. Empty when no app URL is
// configured — it is not derivable from the relay URL (Decision 19).
func (a *App) JoinLink() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return JoinLink(a.cfg.AppURL, a.id)
}

// JoinLink builds a viewer URL.
func JoinLink(appURL, id string) string {
	if appURL == "" || id == "" {
		return ""
	}
	return strings.TrimSuffix(appURL, "/") + "/#/view/" + id
}

// Start begins a broadcast. id empty mints a new code; non-empty reclaims.
func (a *App) Start(ctx context.Context, id string) {
	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return
	}
	a.state = StateStarting
	a.status = "Connecting to the relay…"
	a.lastErr = ""
	a.canMint = false
	a.id = id
	a.mu.Unlock()
	a.invalidate()

	go a.run(ctx, id)
}

func (a *App) run(ctx context.Context, id string) {
	saveCfg := func() {
		if err := a.cfg.Save(); err != nil {
			a.log.Warn("could not save config", "err", err)
		}
	}

	// The persisted resume token is only valid for the ID it was minted for
	// (R17): on a reclaim of that ID it must travel, on any other claim it
	// would just be refused.
	a.mu.Lock()
	resumeToken := ""
	if id != "" && id == a.cfg.LastBroadcastID {
		resumeToken = a.cfg.LastResumeToken
	}
	a.mu.Unlock()

	sess := a.newSess(
		engine.Config{
			RelayURL:      a.cfg.RelayURL,
			BroadcastID:   id,
			PublishSecret: a.cfg.PublishSecret,
			ResumeToken:   resumeToken,
			Origin:        a.cfg.Origin,
			Media:         a.mediaConfig(),
		},
		engine.Callbacks{
			OnBroadcastID: func(bid string) {
				a.mu.Lock()
				a.id = bid
				a.cfg.LastBroadcastID = bid
				a.mu.Unlock()
				saveCfg()
				a.invalidate()
			},
			OnResumeToken: func(token string) {
				a.mu.Lock()
				a.cfg.LastResumeToken = token
				a.mu.Unlock()
				saveCfg()
			},
			OnEncoderChosen: func(enc string) {
				// The API name first: "VA-API" answers "which driver stack am
				// I on?" where the element name answers only "which element".
				a.setStatus(fmt.Sprintf("Live — %s hardware encode (%s)", gst.EncoderAPI(enc), enc))
			},
			OnStats: func(s engine.Stats) {
				a.mu.Lock()
				a.stats = s
				a.mu.Unlock()
				a.invalidate()
			},
			OnError: func(err error) {
				a.fail(err)
			},
			OnEnded: func() {
				a.ended()
			},
		},
		engine.Options{
			Log: a.log,
			MediaFactory: gst.NewFactory(gst.Options{
				LastGoodEncoder: a.cfg.LastGoodEncoder,
				OnEncoderChosen: func(enc string) {
					a.mu.Lock()
					a.cfg.LastGoodEncoder = enc
					a.mu.Unlock()
					saveCfg()
				},
			}),
		},
	)

	a.mu.Lock()
	a.sess = sess
	a.mu.Unlock()

	if err := sess.Start(ctx); err != nil {
		a.startFailed(err)
		return
	}

	a.mu.Lock()
	a.state = StateLive
	if a.status == "Connecting to the relay…" {
		a.status = "Live"
	}
	a.mu.Unlock()
	a.invalidate()
	// Normal urgency: nice to have, and fine if the desktop swallows it while
	// screen casting.
	a.notifier.Notify("Broadcast started", a.liveBody(), notify.UrgencyNormal)
}

// startFailed applies Decision 10's rule.
func (a *App) startFailed(err error) {
	msg := Message(err)
	canMint := false
	if se, ok := engine.AsStartError(err); ok {
		// Only a connect-phase failure may fall back to minting. A
		// capture-phase failure had a live publisher session on this ID, and
		// silently moving to a different broadcast would be exactly R1's bug.
		canMint = se.Phase == engine.PhaseConnect
	}

	a.mu.Lock()
	a.state = StateIdle
	a.sess = nil
	a.status = "Ready"
	a.lastErr = msg
	a.canMint = canMint
	a.mu.Unlock()
	a.invalidate()
	// Critical: the broadcast did not start, and the user may well be looking
	// at a game rather than at this window.
	a.notifier.Notify("Broadcast failed to start", firstLine(msg), notify.UrgencyCritical)
}

// fail reports a mid-session failure.
func (a *App) fail(err error) {
	msg := Message(err)
	a.mu.Lock()
	a.lastErr = msg
	a.mu.Unlock()
	a.invalidate()
	// Critical: this is the case Decision 17 exists for — a child-process
	// death you do not learn about for an hour.
	a.notifier.Notify("Broadcast error", firstLine(msg), notify.UrgencyCritical)
}

// ended handles the session ending for any reason.
func (a *App) ended() {
	a.mu.Lock()
	wasLive := a.state != StateIdle
	a.state = StateIdle
	a.sess = nil
	a.status = "Ready"
	hadErr := a.lastErr != ""
	a.mu.Unlock()
	a.invalidate()

	if wasLive && !hadErr {
		a.notifier.Notify("Broadcast ended", "Your screen is no longer being shared.", notify.UrgencyNormal)
	} else if wasLive && hadErr {
		a.notifier.Notify("Broadcast ended unexpectedly", "Your screen is no longer being shared.", notify.UrgencyCritical)
	}
}

// Stop ends the broadcast.
func (a *App) Stop() {
	a.mu.Lock()
	sess := a.sess
	a.mu.Unlock()
	if sess == nil {
		return
	}
	_ = sess.Stop()
}

// Quit stops everything. Closing the window ends the broadcast (Decision 15):
// no tray, no background presence, no hidden state — if the window is gone,
// nothing is publishing.
func (a *App) Quit() {
	a.Stop()
	a.notifier.Close()
}

func (a *App) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
	a.invalidate()
}

func (a *App) liveBody() string {
	a.mu.Lock()
	id := a.id
	link := JoinLink(a.cfg.AppURL, a.id)
	a.mu.Unlock()
	if link != "" {
		return "Code " + id + " · " + link
	}
	if id != "" {
		return "Code " + id
	}
	return "Your screen is being shared."
}

func (a *App) mediaConfig() engine.MediaConfig {
	m := engine.DefaultMediaConfig()
	if a.cfg.Width > 0 && a.cfg.Height > 0 {
		m.Width, m.Height = a.cfg.Width, a.cfg.Height
	}
	if a.cfg.Fps > 0 {
		m.Fps = a.cfg.Fps
	}
	if a.cfg.BitrateBps > 0 {
		m.BitrateBps = a.cfg.BitrateBps
	}
	m.Encoder = a.cfg.Encoder
	return m
}

// Message turns an error into a sentence a friend can act on (V5's criterion:
// "errors surface as sentences, not Go error strings").
func Message(err error) string {
	if err == nil {
		return ""
	}
	// The sentinel checks run before StartError rendering on purpose: the
	// engine wraps every capture-phase failure in a StartError, and rendering
	// the wrapper first would shadow each curated sentence below behind
	// "Could not start capture: <Go error chain>". errors.Is unwraps through
	// the StartError, and none of the sentinels collide with the HTTP-status
	// cases StartError.Message renders.
	switch {
	case errors.Is(err, engine.ErrNoHardwareEncoder):
		return gst.NoHardwareMessage
	case errors.Is(err, engine.ErrCaptureFormat):
		return gst.CaptureFormatMessage
	case errors.Is(err, portal.ErrCancelled):
		return "Screen sharing was cancelled."
	case errors.Is(err, portal.ErrUnavailable):
		return "No screen-sharing portal is available. Check that xdg-desktop-portal " +
			"and your desktop's backend are installed and running."
	case errors.Is(err, gst.ErrNoLaunchBinary):
		return "GStreamer is not installed. Install gst-launch-1.0 " +
			"(gstreamer1.0-tools on Debian/Ubuntu, gstreamer on Arch, gstreamer1-tools on Fedora)."
	}
	if se, ok := engine.AsStartError(err); ok {
		// Decision 10's payoff: the browser cannot tell 401 from 404 from 409
		// from a typo, because WebTransportError hides the status. We can.
		return se.Message()
	}
	return err.Error()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Diagnostics is the Copy diagnostics payload, shaped to be pasted next to a
// viewer's (R9's remote-troubleshooting story, docs/13).
type Diagnostics struct {
	Kind      string    `json:"kind"`
	Timestamp time.Time `json:"timestamp"`
	Broadcast string    `json:"broadcastId,omitempty"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`

	Encoder     string `json:"encoder,omitempty"`
	EncoderAPI  string `json:"encoderApi,omitempty"`
	CapturePath string `json:"capturePath,omitempty"`
	Codec       string `json:"codec,omitempty"`
	Rung        string `json:"rung"`

	// CaptureFps is deliberately a string: the GStreamer child owns that
	// stage, so it is "n/a" rather than a fabricated number (Decision 20).
	// A number here would answer "is the source keeping up?" wrongly while
	// looking entirely plausible.
	CaptureFps string  `json:"captureFps"`
	EncoderFps float64 `json:"encoderFps"`
	SentFps    float64 `json:"sentFps"`

	EncodedFrames             uint64 `json:"encodedFrames"`
	Keyframes                 uint64 `json:"keyframes"`
	DatagramsSent             uint64 `json:"datagramsSent"`
	BytesSent                 uint64 `json:"bytesSent"`
	ConfigsSent               uint64 `json:"configsSent"`
	KeyframeStreamsSent       uint64 `json:"keyframeStreamsSent"`
	KeyframeStreamsFailed     uint64 `json:"keyframeStreamsFailed"`
	KeyframeStreamsSuperseded uint64 `json:"keyframeStreamsSuperseded"`
	FramesDroppedAtSend       uint64 `json:"framesDroppedAtSend"`

	// KeyframeIntervalMs is the measured cadence (EMA); null until two
	// keyframes have left the encoder. The target is the rung's 500 ms GOP.
	KeyframeIntervalMs *float64 `json:"keyframeIntervalMs"`

	TimeSyncRttMs    *float64 `json:"timeSyncRttMs"`
	TimeSyncOffsetUs *int64   `json:"timeSyncOffsetUs"`
}

// Diagnostics builds the JSON dump.
func (a *App) Diagnostics() string {
	a.mu.Lock()
	s := a.stats
	d := Diagnostics{
		Kind:      "gawk-broadcast",
		Timestamp: time.Now().UTC(),
		Broadcast: a.id,
		State:     a.state.String(),
		Error:     a.lastErr,
	}
	a.mu.Unlock()

	d.Encoder = s.Encoder
	if s.Encoder != "" {
		d.EncoderAPI = gst.EncoderAPI(s.Encoder)
	}
	d.CapturePath = s.CapturePath
	d.Codec = s.Codec
	d.Rung = fmt.Sprintf("%dx%d@%d %.1fMbps", s.Width, s.Height, s.Fps, float64(s.BitrateBps)/1e6)
	d.CaptureFps = "n/a"
	d.EncoderFps = s.EncoderFps
	d.SentFps = s.SentFps
	d.EncodedFrames = s.EncodedFrames
	d.Keyframes = s.Keyframes
	d.DatagramsSent = s.DatagramsSent
	d.BytesSent = s.BytesSent
	d.ConfigsSent = s.ConfigsSent
	d.KeyframeStreamsSent = s.KeyframeStreamsSent
	d.KeyframeStreamsFailed = s.KeyframeStreamsFailed
	d.KeyframeStreamsSuperseded = s.KeyframeStreamsSuperseded
	d.FramesDroppedAtSend = s.FramesDroppedAtSend
	if s.KeyframeIntervalAvailable {
		kf := s.KeyframeIntervalMs
		d.KeyframeIntervalMs = &kf
	}
	if s.TimeSyncAvailable {
		rtt, off := s.TimeSyncRttMs, s.TimeSyncOffsetUs
		d.TimeSyncRttMs, d.TimeSyncOffsetUs = &rtt, &off
	}

	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
