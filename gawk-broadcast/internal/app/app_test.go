package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/config"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/gst"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/notify"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
)

// recordingNotifier captures what the user would actually have been shown.
type recordingNotifier struct {
	mu   sync.Mutex
	sent []sentNotification
}

type sentNotification struct {
	summary string
	body    string
	urgency notify.Urgency
}

func (n *recordingNotifier) Notify(summary, body string, u notify.Urgency) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, sentNotification{summary, body, u})
}

func (n *recordingNotifier) Close() {}

func (n *recordingNotifier) all() []sentNotification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]sentNotification(nil), n.sent...)
}

// fakeSession stands in for the engine.
type fakeSession struct {
	startErr error
	cb       engine.Callbacks
	cfg      engine.Config
	stats    engine.Stats
	stopped  bool
}

func testApp(t *testing.T, fs *fakeSession, n notify.Notifier) (*App, *config.Config) {
	t.Helper()
	cfg := &config.Config{RelayURL: "https://relay.example", AppURL: "https://gawk.example"}
	cfg.SetPath(filepath.Join(t.TempDir(), "broadcast.json"))
	a := New(Options{
		Config:   cfg,
		Notifier: n,
		NewSession: func(ec engine.Config, cb engine.Callbacks, _ engine.Options) Session {
			fs.cb = cb
			fs.cfg = ec
			return fs
		},
	})
	return a, cfg
}

func (f *fakeSession) Start(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	return nil
}
func (f *fakeSession) Stop() error {
	f.stopped = true
	f.cb.OnEnded()
	return nil
}
func (f *fakeSession) Stats() engine.Stats { return f.stats }
func (f *fakeSession) BroadcastID() string { return "" }

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// Decision 10, and the bug it exists to prevent: R1's reclaim fallback treated
// "the relay rejected the dial" and "capture failed" identically, and silently
// minted a new broadcast out from under a live publisher session.
func TestResumeMayMintOnlyOnConnectPhaseFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		canMint bool
	}{
		{
			name:    "connect phase (relay said 404) may mint",
			err:     &engine.StartError{Phase: engine.PhaseConnect, Status: http.StatusNotFound, Err: errors.New("received status 404")},
			canMint: true,
		},
		{
			name:    "connect phase (409, someone else publishing) may mint",
			err:     &engine.StartError{Phase: engine.PhaseConnect, Status: http.StatusConflict, Err: errors.New("received status 409")},
			canMint: true,
		},
		{
			name:    "capture phase must NOT mint: the ID was live and ours",
			err:     &engine.StartError{Phase: engine.PhaseCapture, Err: portal.ErrCancelled},
			canMint: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSession{startErr: tc.err}
			a, _ := testApp(t, fs, notify.Discard{})
			a.Start(context.Background(), "K7M2QP")
			waitFor(t, func() bool { s, _ := a.State(); return s == StateIdle }, "the start to fail")

			if got := a.CanMint(); got != tc.canMint {
				t.Errorf("CanMint() = %v, want %v", got, tc.canMint)
			}
			if a.LastError() == "" {
				t.Error("no error surfaced to the user")
			}
		})
	}
}

// Decision 17's whole point: KDE's portal inhibits normal notifications *while
// screen casting*, so anything that must reach a fullscreen broadcaster has to
// be critical. Failures are exactly that class.
func TestFailuresNotifyAtCriticalUrgency(t *testing.T) {
	n := &recordingNotifier{}
	fs := &fakeSession{startErr: &engine.StartError{Phase: engine.PhaseConnect, Err: errors.New("nope")}}
	a, _ := testApp(t, fs, n)
	a.Start(context.Background(), "")
	waitFor(t, func() bool { return len(n.all()) > 0 }, "a notification")

	got := n.all()[0]
	if got.urgency != notify.UrgencyCritical {
		t.Errorf("a failed start notified at urgency %d, want critical (%d) — KDE would swallow it while casting",
			got.urgency, notify.UrgencyCritical)
	}
}

// A mid-session child death is the failure mode notifications exist for: an
// hour of not knowing your broadcast is dead.
func TestMidSessionErrorNotifiesCritical(t *testing.T) {
	n := &recordingNotifier{}
	fs := &fakeSession{}
	a, _ := testApp(t, fs, n)
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "the session to go live")

	fs.cb.OnError(errors.New("gst child died"))
	waitFor(t, func() bool {
		for _, s := range n.all() {
			if s.urgency == notify.UrgencyCritical {
				return true
			}
		}
		return false
	}, "a critical notification for the child's death")
}

// Going live is a nice-to-have: fine if the desktop swallows it mid-game.
func TestGoingLiveNotifiesAtNormalUrgency(t *testing.T) {
	n := &recordingNotifier{}
	fs := &fakeSession{}
	a, _ := testApp(t, fs, n)
	a.Start(context.Background(), "")
	waitFor(t, func() bool { return len(n.all()) > 0 }, "the went-live notification")

	got := n.all()[0]
	if got.urgency != notify.UrgencyNormal {
		t.Errorf("went-live urgency = %d, want normal (%d)", got.urgency, notify.UrgencyNormal)
	}
	if !strings.Contains(got.summary, "started") {
		t.Errorf("summary = %q", got.summary)
	}
}

func TestBroadcastIDAndJoinLink(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	fs.cb.OnBroadcastID("K7M2QP")
	waitFor(t, func() bool { return a.BroadcastID() == "K7M2QP" }, "the announced code")

	if got := a.JoinLink(); got != "https://gawk.example/#/view/K7M2QP" {
		t.Errorf("JoinLink = %q", got)
	}
	// The code is remembered for Resume, and persisted.
	if cfg.LastBroadcastID != "K7M2QP" {
		t.Errorf("LastBroadcastID = %q, want K7M2QP", cfg.LastBroadcastID)
	}
	reloaded, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LastBroadcastID != "K7M2QP" {
		t.Errorf("persisted LastBroadcastID = %q, want K7M2QP", reloaded.LastBroadcastID)
	}
}

// The app URL is not derivable from the relay URL — they are different hosts
// (Decision 19). With no app URL there is simply no link to offer.
func TestJoinLinkNeedsAnAppURL(t *testing.T) {
	if got := JoinLink("", "K7M2QP"); got != "" {
		t.Errorf("JoinLink with no app URL = %q, want empty", got)
	}
	if got := JoinLink("https://gawk.example", ""); got != "" {
		t.Errorf("JoinLink with no code = %q, want empty", got)
	}
	// A trailing slash must not produce a double slash.
	if got := JoinLink("https://gawk.example/", "ABC123"); got != "https://gawk.example/#/view/ABC123" {
		t.Errorf("JoinLink = %q", got)
	}
}

// R23 (docs/29 TC5): the terms link is derived from the app URL like the join
// link, and is empty without one.
func TestTermsLink(t *testing.T) {
	if got := TermsLink(""); got != "" {
		t.Errorf("TermsLink with no app URL = %q, want empty", got)
	}
	if got := TermsLink("https://gawk.example"); got != "https://gawk.example/#/terms" {
		t.Errorf("TermsLink = %q", got)
	}
	// A trailing slash must not produce a double slash.
	if got := TermsLink("https://gawk.example/"); got != "https://gawk.example/#/terms" {
		t.Errorf("TermsLink = %q", got)
	}
}

// Every error a user can hit becomes a sentence, never a Go error string.
func TestMessageProducesSentences(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"bad secret", &engine.StartError{Phase: engine.PhaseConnect, Status: http.StatusUnauthorized}, "secret"},
		{"unknown code", &engine.StartError{Phase: engine.PhaseConnect, Status: http.StatusNotFound}, "no longer exists"},
		{"already publishing", &engine.StartError{Phase: engine.PhaseConnect, Status: http.StatusConflict}, "already publishing"},
		{"relay full", &engine.StartError{Phase: engine.PhaseConnect, Status: http.StatusTooManyRequests}, "capacity"},
		{"no hardware", engine.ErrNoHardwareEncoder, "browser"},
		{"capture format", engine.ErrCaptureFormat, "pipewiresrc"},
		{"cancelled", portal.ErrCancelled, "cancelled"},
		{"no portal", portal.ErrUnavailable, "xdg-desktop-portal"},
		{"no gstreamer", gst.ErrNoLaunchBinary, "gstreamer1.0-tools"},
		// The production shape: the engine wraps every capture-phase failure
		// in a StartError, and the curated sentences must survive the wrapper
		// rather than degrade to "Could not start capture: <Go error chain>".
		{"no hardware, wrapped", &engine.StartError{Phase: engine.PhaseCapture, Err: engine.ErrNoHardwareEncoder}, "browser"},
		{"capture format, wrapped", &engine.StartError{Phase: engine.PhaseCapture, Err: engine.ErrCaptureFormat}, "pipewiresrc"},
		{"cancelled, wrapped", &engine.StartError{Phase: engine.PhaseCapture, Err: portal.ErrCancelled}, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Message(tc.err)
			if !strings.Contains(strings.ToLower(got), strings.ToLower(tc.want)) {
				t.Errorf("Message = %q, want it to mention %q", got, tc.want)
			}
			// A Go error string leaking through is the failure this guards.
			if strings.HasPrefix(got, "engine:") || strings.HasPrefix(got, "portal:") {
				t.Errorf("Message = %q — that is a Go error string, not a sentence", got)
			}
		})
	}
	if Message(nil) != "" {
		t.Error("Message(nil) should be empty")
	}
}

// Decision 20: the child owns capture, so this stage is unobservable. "n/a" is
// the honest answer; a number derived from the requested rate would answer the
// question wrongly while looking plausible.
func TestDiagnosticsMarksCaptureFpsUnavailable(t *testing.T) {
	fs := &fakeSession{stats: engine.Stats{
		Encoder: "nvh264enc", Codec: "avc1.42E02A",
		Width: 1920, Height: 1080, Fps: 60, BitrateBps: 8_000_000,
		EncoderFps: 59.4, SentFps: 59.1, EncodedFrames: 600,
	}}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	fs.cb.OnStats(fs.stats)
	waitFor(t, func() bool { return a.Stats().Encoder == "nvh264enc" }, "stats")

	var d map[string]any
	if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
		t.Fatalf("diagnostics is not valid JSON: %v", err)
	}
	if d["captureFps"] != "n/a" {
		t.Errorf("captureFps = %v, want the string \"n/a\" — never a fabricated number", d["captureFps"])
	}
	if d["kind"] != "gawk-broadcast" {
		t.Errorf("kind = %v; a diagnostics dump must say what produced it", d["kind"])
	}
	if d["encoder"] != "nvh264enc" {
		t.Errorf("encoder = %v", d["encoder"])
	}
	// Absent TimeSync is null, not 0: zero RTT is a claim, null is a gap.
	if d["timeSyncRttMs"] != nil {
		t.Errorf("timeSyncRttMs = %v, want null when no sample exists", d["timeSyncRttMs"])
	}
}

func TestDiagnosticsIncludesTimeSyncWhenAvailable(t *testing.T) {
	fs := &fakeSession{stats: engine.Stats{TimeSyncAvailable: true, TimeSyncRttMs: 3.5, TimeSyncOffsetUs: -1234}}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	fs.cb.OnStats(fs.stats)
	waitFor(t, func() bool { return a.Stats().TimeSyncAvailable }, "stats")

	var d map[string]any
	if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
		t.Fatal(err)
	}
	if d["timeSyncRttMs"] != 3.5 {
		t.Errorf("timeSyncRttMs = %v, want 3.5", d["timeSyncRttMs"])
	}
}

// R18 (docs/23 Decision 7): the "first viewer joined" ring docs/19 asked for —
// derived from the pushed count's 0 → ≥1 transition, exactly once per
// broadcast, at critical urgency (KDE inhibits normal notifications while
// screen casting). Steady-state changes never ring again.
func TestFirstViewerNotifiesOnceAtCriticalUrgency(t *testing.T) {
	n := &recordingNotifier{}
	fs := &fakeSession{}
	a, _ := testApp(t, fs, n)
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	rings := func() []sentNotification {
		var out []sentNotification
		for _, s := range n.all() {
			if s.summary == "First viewer joined" {
				out = append(out, s)
			}
		}
		return out
	}

	// A zero count (nobody yet) never rings; 0 → 1 rings exactly once.
	fs.cb.OnViewerCount(0)
	fs.cb.OnViewerCount(1)
	waitFor(t, func() bool { return len(rings()) == 1 }, "the first-viewer ring")
	if got := rings()[0]; got.urgency != notify.UrgencyCritical {
		t.Errorf("first-viewer urgency = %d, want critical (%d) — KDE would swallow it while casting",
			got.urgency, notify.UrgencyCritical)
	}

	// ≥1 → ≥1 changes don't ring, and neither does a dip through zero within
	// the same broadcast — one ring per broadcast, not per join.
	fs.cb.OnViewerCount(2)
	fs.cb.OnViewerCount(0)
	fs.cb.OnViewerCount(3)
	time.Sleep(20 * time.Millisecond)
	if got := len(rings()); got != 1 {
		t.Errorf("first-viewer rings = %d, want exactly 1", got)
	}

	// A new broadcast re-arms the latch.
	fs.cb.OnEnded()
	waitFor(t, func() bool { s, _ := a.State(); return s == StateIdle }, "idle")
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live again")
	fs.cb.OnViewerCount(1)
	waitFor(t, func() bool { return len(rings()) == 2 }, "the second broadcast's ring")
}

func TestDiagnosticsIncludesViewerCountWhenAvailable(t *testing.T) {
	fs := &fakeSession{stats: engine.Stats{ViewerCountAvailable: true, ViewerCount: 4}}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	fs.cb.OnStats(fs.stats)
	waitFor(t, func() bool { return a.Stats().ViewerCountAvailable }, "stats")

	var d map[string]any
	if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
		t.Fatal(err)
	}
	if d["viewerCount"] != 4.0 {
		t.Errorf("viewerCount = %v, want 4", d["viewerCount"])
	}

	// Absent (old relay): null, not 0 — zero viewers is a claim, null a gap.
	fs.cb.OnStats(engine.Stats{})
	waitFor(t, func() bool { return !a.Stats().ViewerCountAvailable }, "stats reset")
	if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
		t.Fatal(err)
	}
	if d["viewerCount"] != nil {
		t.Errorf("viewerCount = %v, want null when no push has landed", d["viewerCount"])
	}
}

// Closing the window ends the broadcast (Decision 15): if the window is gone,
// nothing is publishing. No tray, no background presence, no hidden state.
func TestQuitStopsTheBroadcast(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	a.Quit()
	if !fs.stopped {
		t.Error("Quit did not stop the session: the screen would still be shared with no window to stop it")
	}
	waitFor(t, func() bool { s, _ := a.State(); return s == StateIdle }, "idle after quit")
}

// Starting twice must not open two sessions.
func TestStartIsIgnoredWhileLive(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	sessions := 0
	a.newSess = func(engine.Config, engine.Callbacks, engine.Options) Session {
		sessions++
		return &fakeSession{}
	}
	a.Start(context.Background(), "")
	time.Sleep(20 * time.Millisecond)
	if sessions != 0 {
		t.Errorf("started %d extra sessions while live", sessions)
	}
}

// Resume dials /publish/{id} with the remembered code.
func TestResumeUsesTheRememberedCode(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "K7M2QP")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	if fs.cfg.BroadcastID != "K7M2QP" {
		t.Errorf("engine got BroadcastID %q, want K7M2QP (Resume must reclaim, not mint)", fs.cfg.BroadcastID)
	}
}

// R17: a reclaim must carry the token the relay minted for that ID — the
// relay refuses it otherwise — and must NOT carry a token minted for some
// other ID (it would only turn "unknown ID" into "403").
func TestResumeCarriesThePersistedTokenOnlyForItsID(t *testing.T) {
	const token = "abab1212abab1212abab1212abab1212"

	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})
	cfg.LastBroadcastID, cfg.LastResumeToken = "K7M2QP", token
	a.Start(context.Background(), "K7M2QP")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	if fs.cfg.ResumeToken != token {
		t.Errorf("engine got ResumeToken %q, want %q (reclaim of the token's own ID)", fs.cfg.ResumeToken, token)
	}

	fs2 := &fakeSession{}
	a2, cfg2 := testApp(t, fs2, notify.Discard{})
	cfg2.LastBroadcastID, cfg2.LastResumeToken = "K7M2QP", token
	a2.Start(context.Background(), "OTHERID")
	waitFor(t, func() bool { s, _ := a2.State(); return s == StateLive }, "live")
	if fs2.cfg.ResumeToken != "" {
		t.Errorf("engine got ResumeToken %q for a different ID, want none", fs2.cfg.ResumeToken)
	}
}

// A never-configured app must still reach the default fleet: pressing Start on
// a fresh install is the whole reason the default exists, and resolution has to
// happen where the session is built, not at Save time.
func TestUnconfiguredStartUsesTheDefaultRelay(t *testing.T) {
	fs := &fakeSession{}
	cfg := &config.Config{}
	cfg.SetPath(filepath.Join(t.TempDir(), "broadcast.json"))
	a := New(Options{
		Config: cfg,
		NewSession: func(ec engine.Config, cb engine.Callbacks, _ engine.Options) Session {
			fs.cb, fs.cfg = cb, ec
			return fs
		},
	})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	if fs.cfg.RelayURL != config.DefaultRelayURL {
		t.Errorf("engine got RelayURL %q, want the default %q", fs.cfg.RelayURL, config.DefaultRelayURL)
	}
	if !a.telemetry.Enabled() {
		t.Error("telemetry inert on the default fleet; reporting on by default is the point")
	}
}

// The settings field has to mean something without an app restart: the
// reporter outlives a broadcast, so Start re-reads it.
func TestTelemetryFieldTakesEffectOnTheNextStart(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})
	// testApp's relay is not the default one, so nothing reports until asked.
	if a.telemetry.Enabled() {
		t.Fatal("telemetry enabled against a non-default relay with no endpoint set")
	}

	cfg.TelemetryURL = "https://telemetry.example/api/telemetry/v1/ingest"
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	if !a.telemetry.Enabled() {
		t.Error("telemetry still inert after the field was set and Start pressed")
	}

	a.Stop()
	waitFor(t, func() bool { s, _ := a.State(); return s == StateIdle }, "idle")
	cfg.TelemetryURL = config.Off
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	if a.telemetry.Enabled() {
		t.Errorf("telemetry still enabled after %q; off must mean off", config.Off)
	}
}

// The relay-minted token must survive an app restart, or the very next
// Resume is refused: OnResumeToken persists it to the config file.
func TestResumeTokenIsPersistedOnReceipt(t *testing.T) {
	const token = "cdcd3434cdcd3434cdcd3434cdcd3434"

	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	fs.cb.OnResumeToken(token)
	waitFor(t, func() bool {
		saved, err := config.Load(cfg.Path())
		return err == nil && saved.LastResumeToken == token
	}, "resume token persisted to the config file")
}
