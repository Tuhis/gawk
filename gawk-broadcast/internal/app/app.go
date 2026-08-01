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
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/telemetry"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/version"
	"github.com/Tuhis/gawk/gawk-server/wire"
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
	// resuming is true while the engine is reclaiming the broadcast after a
	// transport drop. The state stays StateLive — capture, the encoder and the
	// broadcast code are all still ours — but the window must not present a
	// steady green "Live" while nothing is reaching viewers.
	resuming bool
	// liveStatus is the status line to return to once a resume succeeds (the
	// encoder line, or plain "Live" before one is chosen).
	liveStatus string
	// canMint records whether a failed Resume may fall back to minting. It is
	// set only for connect-phase failures (Decision 10).
	canMint bool
	// firstViewerSeen latches the R18 first-viewer notification: exactly one
	// ring per broadcast, on the count's first 0 → ≥1 transition. Reset on
	// every Start.
	firstViewerSeen bool

	// R28 (docs/33 TM2): the telemetry reporter. Owned here rather than by the
	// engine so a session with none configured is byte-identical to pre-R28,
	// and so it survives across Start/Stop cycles like the config does.
	telemetry *telemetry.Reporter

	// R35, the whose-audio step (docs/39 D5). prompt is non-nil exactly while
	// the card is on screen; answer carries the click back to the engine
	// goroutine that is blocked inside Start.
	prompt *AudioPrompt
	answer chan engine.AudioTarget
	// audioLinks is the helper's live link count and audioSilentSince when it
	// last went to zero — the two things the silence hint is derived from. A
	// link count rather than a level meter (D6).
	audioLinks      int
	audioLinksKnown bool
	audioSilentAt   time.Time
	// media is the live capture source, kept only so the mid-session switch
	// has something to call. Nil outside a broadcast.
	media any
}

// AudioPrompt is the whose-audio card's state: what to list, and what is
// already selected.
//
// It exists because Linux structurally cannot have the Windows sibling's
// one-picker flow — the portal never says which application owns the window
// that was picked (docs/39 §1) — so gawk asks, once, in its own words.
type AudioPrompt struct {
	// Apps is the live list of applications currently emitting audio.
	Apps []pwproto.App
	// Err is set when per-application audio is unavailable on this machine.
	// The card then says so and offers the two rows that still work.
	Err error
	// Preselect is the binary to highlight: the last-used application, but
	// only when it is present and actually emitting (AD3). Empty means nothing
	// is preselected, which is the honest state when the remembered
	// application is not running.
	Preselect string
}

// AudioPromptState returns the whose-audio card's state, or nil when the card
// is not open.
func (a *App) AudioPromptState() *AudioPrompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prompt == nil {
		return nil
	}
	p := *a.prompt
	p.Apps = append([]pwproto.App(nil), a.prompt.Apps...)
	return &p
}

// AnswerAudioPrompt delivers the broadcaster's choice.
//
// Called from the UI thread; the engine goroutine is blocked in
// chooseAudioTarget waiting for exactly one of these. A second call, or one
// with no prompt open, is a no-op rather than a panic — a double click on a
// card that has already closed is a normal thing for a user to do.
func (a *App) AnswerAudioPrompt(target engine.AudioTarget) {
	a.mu.Lock()
	ch := a.answer
	if ch == nil {
		a.mu.Unlock()
		return
	}
	a.answer = nil
	a.prompt = nil
	// AD3: remember the choice as a *preselection* for next time. It never
	// silently reuses itself — the card always appears — so this can only ever
	// save a click, not change what a broadcast publishes.
	if target.Mode == engine.AudioTargetApp {
		a.cfg.AudioApp = target.Binary
	}
	a.mu.Unlock()

	ch <- target
	if err := a.cfg.Save(); err != nil {
		a.log.Warn("could not save config", "err", err)
	}
	a.invalidate()
}

// chooseAudioTarget is the engine's ChooseAudioTarget seam: open the card and
// block until the broadcaster answers.
//
// Cancelling (the context ending, i.e. the broadcast being stopped) answers
// "whole system", which is the pre-R35 behavior and the only answer that is
// never surprising.
func (a *App) chooseAudioTarget(ctx context.Context, offer gst.AppAudioOffer) engine.AudioTarget {
	ch := make(chan engine.AudioTarget, 1)
	a.mu.Lock()
	a.answer = ch
	a.prompt = &AudioPrompt{
		Apps:      offer.Apps,
		Err:       offer.Err,
		Preselect: preselect(a.cfg.AudioApp, offer.Apps),
	}
	a.status = "Choose whose audio to share…"
	a.mu.Unlock()
	a.invalidate()

	select {
	case target := <-ch:
		a.setStatus("Starting capture…")
		return target
	case <-ctx.Done():
		a.mu.Lock()
		a.answer, a.prompt = nil, nil
		a.mu.Unlock()
		a.invalidate()
		return engine.AudioTarget{Mode: engine.AudioTargetSystem}
	}
}

// preselect implements AD3: the last-used application is highlighted only when
// it is present and emitting. A remembered name that is not in the list is not
// a selection — it is a memory, and presenting it as a selection would invite a
// blind Enter that captures nothing.
func preselect(last string, apps []pwproto.App) string {
	if last == "" {
		return ""
	}
	for _, a := range apps {
		if a.Binary == last {
			return last
		}
	}
	return ""
}

// onAudioApps keeps the open card's list live, so an application that starts
// playing while the card is up appears without the user doing anything. That
// is what makes the card's one caveat — "apps appear here when they play
// sound" — a wait rather than a dead end.
func (a *App) onAudioApps(apps []pwproto.App) {
	a.mu.Lock()
	if a.prompt != nil {
		a.prompt.Apps = apps
		a.prompt.Preselect = preselect(a.cfg.AudioApp, apps)
	}
	a.mu.Unlock()
	a.invalidate()
}

// onAudioLinks records the helper's link count and the moment it went to zero,
// which is all the silence hint needs (D6).
func (a *App) onAudioLinks(n int) {
	a.mu.Lock()
	if n == 0 && (!a.audioLinksKnown || a.audioLinks != 0) {
		a.audioSilentAt = time.Now()
	}
	a.audioLinks, a.audioLinksKnown = n, true
	a.mu.Unlock()
	a.invalidate()
}

// AudioSilenceHint is the sentence to show when the chosen application has
// been quiet long enough to be worth mentioning, or "" when there is nothing
// to say.
//
// This is docs/38 D8's escape hatch ported to Linux, driven by a link count
// instead of a level meter: zero links means the application has no audio
// streams open at all, which is a stronger signal than a quiet meter and needs
// no metering to observe. The residual case it covers is Windows' V-3a — a game
// whose audio moved to a helper process with a different binary name.
func (a *App) AudioSilenceHint() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != StateLive || a.stats.AudioApp == "" {
		return ""
	}
	if !a.audioLinksKnown || a.audioLinks > 0 {
		return ""
	}
	if time.Since(a.audioSilentAt) < audioSilenceGrace {
		return ""
	}
	return fmt.Sprintf(
		"No audio from %s right now — it may have stopped playing sound, or another "+
			"process took over. Switch to whole-system audio?", a.stats.AudioApp)
}

// audioSilenceGrace is how long a captured application may be silent before the
// hint appears. Ten seconds, matching the Windows sibling: long enough that a
// loading screen or a menu transition does not trigger it, short enough that a
// broadcaster does not stream silence for a whole match.
const audioSilenceGrace = 10 * time.Second

// SwitchToSystemAudio is the card's one mid-session action (docs/39 D5).
//
// It is a re-link, not a renegotiation: same Opus stream, same sequence space,
// nothing re-emitted. Viewers lose the packets in the gap and notice nothing
// else.
func (a *App) SwitchToSystemAudio(ctx context.Context) {
	a.mu.Lock()
	media := a.media
	a.mu.Unlock()
	sw, ok := media.(interface {
		SwitchToSystemAudio(context.Context) error
	})
	if !ok {
		return
	}
	if err := sw.SwitchToSystemAudio(ctx); err != nil {
		a.log.Warn("could not switch to whole-system audio", "err", err)
		a.mu.Lock()
		a.lastErr = "Could not switch to whole-system audio: " + err.Error()
		a.mu.Unlock()
	} else {
		a.mu.Lock()
		a.audioLinks, a.audioLinksKnown = 0, false
		a.mu.Unlock()
	}
	a.invalidate()
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
	// On by default against the default fleet (Config.Telemetry applies that
	// rule), and still inert unless the relay's hello enables collection: a
	// relay with no telemetry key sends none, and then this process makes no
	// telemetry request at all.
	//
	// version.Release, not version.String(): the reporter's Version doubles as
	// the telemetry schema version (docs/33 D15) and gawk-telemetry groups
	// sessions by it, so the per-build "+g<sha>" suffix belongs in the window
	// and the diagnostics dump, not on the wire. Before this it was the
	// literal "dev" for every Linux broadcast ever reported.
	a.telemetry = telemetry.New(telemetry.Options{
		URL:     opts.Config.Telemetry(),
		Version: version.Release,
		Log:     a.log,
	})
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

// TermsLink builds the operator's terms-of-use URL from the app URL. Empty
// when no app URL is configured. R23 (docs/29 TC5): the native broadcaster
// publishes under the same operator's terms as the browser; it does not gate,
// it just points at them.
func (a *App) TermsLink() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return TermsLink(a.cfg.AppURL)
}

// TermsLink builds a terms-of-use URL.
func TermsLink(appURL string) string {
	if appURL == "" {
		return ""
	}
	return strings.TrimSuffix(appURL, "/") + "/#/terms"
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
	a.liveStatus = ""
	a.resuming = false
	a.lastErr = ""
	a.canMint = false
	a.firstViewerSeen = false
	a.audioLinks, a.audioLinksKnown = 0, false
	a.prompt, a.answer = nil, nil
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
	relayURL := a.cfg.Relay()
	ingestURL := a.cfg.Telemetry()
	a.mu.Unlock()

	// Settings are read per broadcast, not once at launch, so editing the
	// telemetry field and pressing Start means what it says. Repointing between
	// sessions is exactly the window SetURL documents.
	a.telemetry.SetURL(ingestURL)

	sess := a.newSess(
		engine.Config{
			RelayURL:      relayURL,
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
				a.setLiveStatus(fmt.Sprintf("Live — %s hardware encode (%s)", gst.EncoderAPI(enc), enc))
			},
			OnResuming: func(attempt int, lastErr error) {
				a.setResuming(attempt, lastErr)
			},
			OnResumed: func() {
				a.setResumed()
			},
			OnStats: func(s engine.Stats) {
				a.mu.Lock()
				a.stats = s
				a.mu.Unlock()
				a.telemetry.Report(s)
				a.invalidate()
			},
			OnError: func(err error) {
				a.telemetry.Event("error", err.Error())
				a.fail(err)
			},
			OnEnded: func() {
				// A clean end lets the service finalize this session rather
				// than waiting out an idle timeout.
				a.telemetry.Event("ended", "")
				a.telemetry.Finish()
				a.ended()
			},
			// R28: this session's telemetry identity (wire 0x0D). A relay
			// predating R28, or one with telemetry off, sends none — and the
			// reporter then stays inert.
			OnTelemetryHello: func(h wire.TelemetryHello) {
				a.telemetry.Begin(h)
			},
			OnViewerCount: func(count uint32) {
				a.onViewerCount(count)
			},
		},
		engine.Options{
			Log:          a.log,
			MediaFactory: a.mediaFactory(saveCfg),
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
	if a.liveStatus == "" {
		a.liveStatus = a.status
	}
	a.mu.Unlock()
	a.invalidate()
	// Normal urgency: nice to have, and fine if the desktop swallows it while
	// screen casting.
	a.notifier.Notify("Broadcast started", a.liveBody(), notify.UrgencyNormal)
}

// mediaFactory builds the capture factory and remembers the source it makes.
//
// The source is kept for exactly one reason: the mid-session switch to
// whole-system audio has to reach it, and the engine deliberately does not
// expose its media source (it knows about frames and the relay, not about
// PipeWire). Wrapping the factory is the smallest seam that gets there without
// widening engine.Session for one button.
func (a *App) mediaFactory(saveCfg func()) engine.MediaSourceFactory {
	inner := gst.NewFactory(gst.Options{
		LastGoodEncoder: a.cfg.LastGoodEncoder,
		OnEncoderChosen: func(enc string) {
			a.mu.Lock()
			a.cfg.LastGoodEncoder = enc
			a.mu.Unlock()
			saveCfg()
		},
		// R35: the whose-audio step. Only reached when the picker returned a
		// window, so a whole-screen broadcast never touches any of it.
		ChooseAudioTarget: a.chooseAudioTarget,
		OnAudioApps:       a.onAudioApps,
		OnAudioLinks:      a.onAudioLinks,
	})
	return func(cfg engine.MediaConfig, clock engine.Clock, log *slog.Logger) (engine.MediaSource, error) {
		src, err := inner(cfg, clock, log)
		if err == nil {
			a.mu.Lock()
			a.media = src
			a.mu.Unlock()
		}
		return src, err
	}
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
	a.media = nil
	a.prompt, a.answer = nil, nil
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
	a.media = nil
	a.prompt, a.answer = nil, nil
	a.status = "Ready"
	a.resuming = false
	hadErr := a.lastErr != ""
	a.mu.Unlock()
	a.invalidate()

	if wasLive && !hadErr {
		a.notifier.Notify("Broadcast ended", "Your screen is no longer being shared.", notify.UrgencyNormal)
	} else if wasLive && hadErr {
		a.notifier.Notify("Broadcast ended unexpectedly", "Your screen is no longer being shared.", notify.UrgencyCritical)
	}
}

// onViewerCount tracks the audience number (the stats card renders it from
// engine.Stats) and rings the first-viewer notification docs/19 asked for —
// R18 supplies the signal, this latch turns it into exactly one ring per
// broadcast on the 0 → ≥1 transition. Critical urgency on purpose: KDE's
// portal inhibits normal notifications while screen casting (Decision 17's
// trap), and "someone is now watching" is precisely what a fullscreen
// broadcaster wants to hear about.
func (a *App) onViewerCount(count uint32) {
	a.mu.Lock()
	first := count > 0 && !a.firstViewerSeen && a.state == StateLive
	if first {
		a.firstViewerSeen = true
	}
	a.mu.Unlock()
	a.invalidate()
	if first {
		a.notifier.Notify("First viewer joined", "Someone is watching your stream.", notify.UrgencyCritical)
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
	// R28: flush the session's last batch and stop the sender. Folded into the
	// one existing shutdown path rather than adding a second the GUI has to
	// remember — Decision 15 is that closing the window ends everything.
	a.telemetry.Close()
	a.notifier.Close()
}

func (a *App) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
	a.invalidate()
}

// setLiveStatus records the healthy status line and shows it, unless a resume
// is in flight — a late OnEncoderChosen must not overwrite "Reconnecting…".
func (a *App) setLiveStatus(s string) {
	a.mu.Lock()
	a.liveStatus = s
	if !a.resuming {
		a.status = s
	}
	a.mu.Unlock()
	a.invalidate()
}

// setResuming reflects a transport-only interruption. Deliberately no
// notification: a rollout blip reconnects in well under a second, and nagging
// through every one of those would train the user to ignore the notifications
// that do matter. The give-up path rings critically through fail()/ended().
func (a *App) setResuming(attempt int, lastErr error) {
	a.mu.Lock()
	a.resuming = true
	if attempt <= 1 {
		a.status = "Reconnecting to the relay…"
	} else {
		a.status = fmt.Sprintf("Reconnecting to the relay… (attempt %d)", attempt)
	}
	a.mu.Unlock()
	a.invalidate()
	if lastErr != nil {
		a.telemetry.Event("resuming", lastErr.Error())
	} else {
		a.telemetry.Event("resuming", "")
	}
}

func (a *App) setResumed() {
	a.mu.Lock()
	a.resuming = false
	a.status = a.liveStatus
	if a.status == "" {
		a.status = "Live"
	}
	a.mu.Unlock()
	a.invalidate()
	a.telemetry.Event("resumed", "")
}

// Resuming reports whether the broadcast is mid-reclaim. The window colours
// its heartbeat with it: green means frames are reaching the relay, and during
// a resume they are not.
func (a *App) Resuming() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resuming
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
	m.DisableAudio = a.cfg.DisableAudio
	m.AudioDevice = a.cfg.AudioDevice
	return m
}

// AudioStatus is the one line the window shows about sound. It is a sentence
// rather than a state name because "unavailable" on its own invites a bug
// report: a machine with no usable audio source is working as designed
// (docs/28 Decision 6), and the text has to say so.
func AudioStatus(s engine.Stats) string {
	switch s.AudioState {
	case engine.AudioActive:
		// R35: naming the application is the whole point of the feature, and
		// the line a broadcaster glances at to confirm the right sound is
		// going out. The source name stays for system audio, where *which*
		// candidate won is the first thing to know when audio is wrong.
		if s.AudioApp != "" {
			return fmt.Sprintf("Audio from %s only", s.AudioApp)
		}
		if s.AudioSource != "" {
			return fmt.Sprintf("System audio · %s", s.AudioSource)
		}
		return "System audio"
	case engine.AudioOff:
		return "No audio (turned off)"
	case engine.AudioUnavailable:
		return "No audio — this machine has no usable audio source"
	case engine.AudioError:
		return "No audio — the encoder produced a stream we could not publish"
	default:
		return "Audio n/a"
	}
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
	Kind string `json:"kind"`
	// AppVersion is the full build string ("1.9.0+g1a2b3c4"), not the bare
	// release the telemetry wire carries: a pasted dump should say which
	// *build* produced it, since every broadcaster binary in existence is a CI
	// artifact or a local build rather than a tagged release.
	AppVersion string    `json:"appVersion"`
	Timestamp  time.Time `json:"timestamp"`
	Broadcast  string    `json:"broadcastId,omitempty"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`

	Encoder     string `json:"encoder,omitempty"`
	EncoderAPI  string `json:"encoderApi,omitempty"`
	CapturePath string `json:"capturePath,omitempty"`
	Codec       string `json:"codec,omitempty"`
	Rung        string `json:"rung"`

	// R35: what was shared and whose sound went out. "screen" or "window";
	// AudioApp is the captured application's binary in app mode and empty for
	// system audio. Rung above carries the *fitted* dimensions, which for a
	// window is the aspect-preserving fit inside the configured box (docs/39
	// D2) — so a pasted dump answers "why is this 1542 wide?" by itself.
	ShareMode string `json:"shareMode,omitempty"`
	AudioApp  string `json:"audioApp,omitempty"`

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

	// ViewerCount is the relay's R18 "N watching" push; null until the first
	// push lands (an old relay never sends one).
	ViewerCount *uint32 `json:"viewerCount"`

	// The R25 audio lane. AudioState is always present — "off",
	// "unavailable", "active", "error" — so a silent stream is diagnosable
	// from the dump alone rather than from an absent key.
	AudioState      string `json:"audioState"`
	AudioSource     string `json:"audioSource,omitempty"`
	AudioCodec      string `json:"audioCodec,omitempty"`
	AudioSampleRate int    `json:"audioSampleRate,omitempty"`
	AudioChannels   int    `json:"audioChannels,omitempty"`
	AudioBitrateBps int    `json:"audioBitrateBps,omitempty"`

	AudioPacketsSent    uint64 `json:"audioPacketsSent"`
	AudioBytesSent      uint64 `json:"audioBytesSent"`
	AudioConfigsSent    uint64 `json:"audioConfigsSent"`
	AudioPacketsDropped uint64 `json:"audioPacketsDropped"`
}

// Diagnostics builds the JSON dump.
func (a *App) Diagnostics() string {
	a.mu.Lock()
	s := a.stats
	d := Diagnostics{
		Kind:       "gawk-broadcast",
		AppVersion: version.String(),
		Timestamp:  time.Now().UTC(),
		Broadcast:  a.id,
		State:      a.state.String(),
		Error:      a.lastErr,
	}
	a.mu.Unlock()

	d.Encoder = s.Encoder
	if s.Encoder != "" {
		d.EncoderAPI = gst.EncoderAPI(s.Encoder)
	}
	d.CapturePath = s.CapturePath
	d.Codec = s.Codec
	d.Rung = fmt.Sprintf("%dx%d@%d %.1fMbps", s.Width, s.Height, s.Fps, float64(s.BitrateBps)/1e6)
	d.ShareMode = s.ShareMode
	d.AudioApp = s.AudioApp
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
	if s.ViewerCountAvailable {
		count := s.ViewerCount
		d.ViewerCount = &count
	}
	d.AudioState = string(s.AudioState)
	d.AudioSource = s.AudioSource
	d.AudioCodec = s.AudioCodec
	d.AudioSampleRate = s.AudioSampleRate
	d.AudioChannels = s.AudioChannels
	d.AudioBitrateBps = s.AudioBitrateBps
	d.AudioPacketsSent = s.AudioPacketsSent
	d.AudioBytesSent = s.AudioBytesSent
	d.AudioConfigsSent = s.AudioConfigsSent
	d.AudioPacketsDropped = s.AudioPacketsDropped

	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
