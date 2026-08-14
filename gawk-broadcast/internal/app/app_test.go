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

	"github.com/Tuhis/gawk/gawk-broadcast/internal/appaudio"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/config"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/gst"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/notify"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/version"
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

// A dump pasted into a bug report has to say which build produced it, and it
// has to say so before a broadcast has ever started — half the reports are
// "it won't start". The full build string, not the bare release: every
// broadcaster binary in existence is a CI artifact or a local build.
func TestDiagnosticsCarriesTheBuildVersion(t *testing.T) {
	a, _ := testApp(t, &fakeSession{}, notify.Discard{})

	var d map[string]any
	if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
		t.Fatalf("diagnostics is not valid JSON: %v", err)
	}
	got, _ := d["appVersion"].(string)
	if got != version.String() {
		t.Errorf("appVersion = %q, want %q", got, version.String())
	}
	if !strings.HasPrefix(got, version.Release) {
		t.Errorf("appVersion = %q, want it to lead with the release %q", got, version.Release)
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

// R37 SP8: the selected server profile is what Start dials — URL and its own
// per-server secret together.
func TestSelectedServerProfileReachesTheDial(t *testing.T) {
	fs := &fakeSession{}
	cfg := &config.Config{
		Servers: []config.ServerProfile{
			{Name: "Homelab", URL: "https://relay.home.example:4433", PublishSecret: "home-secret"},
		},
		SelectedServer: "Homelab",
	}
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

	if fs.cfg.RelayURL != "https://relay.home.example:4433" {
		t.Errorf("engine got RelayURL %q, want the selected profile's", fs.cfg.RelayURL)
	}
	if fs.cfg.PublishSecret != "home-secret" {
		t.Errorf("engine got PublishSecret %q, want the selected profile's own", fs.cfg.PublishSecret)
	}
	// The §4.10 guard: a foreign profile with nothing configured reports
	// nowhere (until the relay advertises, below).
	if a.telemetry.Enabled() {
		t.Error("telemetry enabled against a foreign profile with no endpoint configured or advertised")
	}
}

// R37 (docs/40 §4.10): a relay-advertised 0x12 endpoint turns reporting on for
// a foreign-relay session — and an explicit "off" is not overruled by it.
func TestAdvertisedTelemetryEndpointIsHonoredUnlessOff(t *testing.T) {
	start := func(t *testing.T, telemetryURL string) (*App, *fakeSession) {
		t.Helper()
		fs := &fakeSession{}
		cfg := &config.Config{
			Servers:        []config.ServerProfile{{Name: "Homelab", URL: "https://relay.home.example:4433"}},
			SelectedServer: "Homelab",
			TelemetryURL:   telemetryURL,
		}
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
		return a, fs
	}

	t.Run("advertised endpoint enables reporting", func(t *testing.T) {
		a, fs := start(t, "")
		fs.cb.OnTelemetryEndpoint("https://gawk.home.example/api/telemetry/v1/ingest")
		if !a.telemetry.Enabled() {
			t.Error("an advertised endpoint did not reach the reporter")
		}
	})
	t.Run("off means off, advertised or not", func(t *testing.T) {
		a, fs := start(t, config.Off)
		fs.cb.OnTelemetryEndpoint("https://gawk.home.example/api/telemetry/v1/ingest")
		if a.telemetry.Enabled() {
			t.Errorf("an advertised endpoint overruled an explicit %q", config.Off)
		}
	})
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

// A transport-only interruption keeps the broadcast live. Presenting it as
// ended — which is what the window did before auto-resume, because the engine
// fired OnEnded on any relay loss — sends the user to hunt for a Start button
// while the engine is already reclaiming the code they still hold.
func TestResumingKeepsTheBroadcastLive(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "K7M2QP")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	fs.cb.OnEncoderChosen("vah264enc")
	live, liveStatus := a.State()

	fs.cb.OnResuming(2, errors.New("connection refused"))
	state, status := a.State()
	if state != live {
		t.Errorf("state = %v during a resume, want it to stay %v", state, live)
	}
	if !a.Resuming() {
		t.Error("Resuming() = false during a resume: the heartbeat would still read green")
	}
	if !strings.Contains(status, "Reconnecting") {
		t.Errorf("status = %q during a resume, want it to say so", status)
	}

	fs.cb.OnResumed()
	if a.Resuming() {
		t.Error("Resuming() stayed true after the reclaim succeeded")
	}
	if _, got := a.State(); got != liveStatus {
		t.Errorf("status = %q after resuming, want the encoder line %q back", got, liveStatus)
	}
}

// Giving up must be loud. The engine reports the failure through OnError
// before OnEnded, and that ordering is what makes the ending notification
// critical — KDE's portal swallows normal-urgency notifications while a screen
// cast is running, which is exactly when this fires.
func TestUnresumableLossEndsCritically(t *testing.T) {
	fs := &fakeSession{}
	n := &recordingNotifier{}
	a, _ := testApp(t, fs, n)
	a.Start(context.Background(), "K7M2QP")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")

	fs.cb.OnError(errors.New("lost the connection to the relay and could not resume"))
	fs.cb.OnEnded()

	waitFor(t, func() bool { s, _ := a.State(); return s == StateIdle }, "idle")
	if a.Resuming() {
		t.Error("Resuming() stayed true after the session ended")
	}
	sent := n.all()
	if len(sent) == 0 {
		t.Fatal("an unresumable loss notified nothing at all")
	}
	if last := sent[len(sent)-1]; last.urgency != notify.UrgencyCritical {
		t.Errorf("ending notification urgency = %d, want critical (%d)", last.urgency, notify.UrgencyCritical)
	}
}

// R25: the diagnostics dump carries the audio lane, and carries it even when
// there is no audio — a silent stream has to be diagnosable from the dump
// alone. An absent key would read as zero, which is the whole reason
// audioState is a word rather than a boolean.
func TestDiagnosticsCarriesTheAudioLane(t *testing.T) {
	fs := &fakeSession{stats: engine.Stats{
		AudioState: engine.AudioActive, AudioSource: "pipewire-monitor",
		AudioCodec: "opus", AudioSampleRate: 48000, AudioChannels: 2, AudioBitrateBps: 128_000,
		AudioPacketsSent: 250, AudioBytesSent: 80_000, AudioConfigsSent: 5, AudioPacketsDropped: 1,
	}}
	a, _ := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
	fs.cb.OnStats(fs.stats)
	waitFor(t, func() bool { return a.Stats().AudioPacketsSent == 250 }, "stats")

	var d map[string]any
	if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
		t.Fatalf("diagnostics is not valid JSON: %v", err)
	}
	for k, want := range map[string]any{
		"audioState":          "active",
		"audioSource":         "pipewire-monitor",
		"audioCodec":          "opus",
		"audioSampleRate":     48000.0,
		"audioChannels":       2.0,
		"audioPacketsSent":    250.0,
		"audioConfigsSent":    5.0,
		"audioPacketsDropped": 1.0,
	} {
		if d[k] != want {
			t.Errorf("%s = %v, want %v", k, d[k], want)
		}
	}
}

func TestDiagnosticsSaysWhyThereIsNoAudio(t *testing.T) {
	for _, tc := range []struct {
		state engine.AudioState
		want  string
	}{
		{engine.AudioUnavailable, "unavailable"},
		{engine.AudioOff, "off"},
		{engine.AudioError, "error"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			fs := &fakeSession{stats: engine.Stats{AudioState: tc.state}}
			a, _ := testApp(t, fs, notify.Discard{})
			a.Start(context.Background(), "")
			waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "live")
			fs.cb.OnStats(fs.stats)
			waitFor(t, func() bool { return a.Stats().AudioState == tc.state }, "stats")

			var d map[string]any
			if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
				t.Fatal(err)
			}
			if d["audioState"] != tc.want {
				t.Errorf("audioState = %v, want %q", d["audioState"], tc.want)
			}
			// The counters stay present at zero rather than vanishing: "no
			// packets were sent" and "the field is missing" are different
			// claims, and only one of them is true here.
			if d["audioPacketsSent"] != 0.0 {
				t.Errorf("audioPacketsSent = %v, want 0", d["audioPacketsSent"])
			}
		})
	}
}

// The window shows a sentence, not a state name: "unavailable" on its own
// invites a bug report, where a machine with no usable audio source is working
// exactly as docs/28 Decision 6 designs it to.
func TestAudioStatusReadsAsASentence(t *testing.T) {
	for _, tc := range []struct {
		stats engine.Stats
		want  string
	}{
		{engine.Stats{AudioState: engine.AudioActive, AudioSource: "pipewire-monitor"}, "System audio · pipewire-monitor"},
		{engine.Stats{AudioState: engine.AudioOff}, "No audio (turned off)"},
		{engine.Stats{AudioState: engine.AudioUnavailable}, "No audio — this machine has no usable audio source"},
		{engine.Stats{AudioState: engine.AudioError}, "No audio — the encoder produced a stream we could not publish"},
		{engine.Stats{}, "Audio n/a"},
	} {
		if got := AudioStatus(tc.stats); got != tc.want {
			t.Errorf("AudioStatus(%q) = %q, want %q", tc.stats.AudioState, got, tc.want)
		}
	}
}

// The config's audio keys reach the engine's media config. A knob that parses
// and never arrives is the R2 finding CODE-REVIEW.md exists to prevent, and
// the *disable* spelling is what makes a pre-R25 config mean "audio on".
func TestAudioConfigKeysReachTheMediaConfig(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})
	if m := a.mediaConfig(); m.DisableAudio || m.AudioDevice != "" {
		t.Errorf("a fresh config gives %+v, want audio on with no pinned device", m)
	}

	cfg.DisableAudio = true
	cfg.AudioDevice = "my-sink.monitor"
	m := a.mediaConfig()
	if !m.DisableAudio {
		t.Error("disableAudio did not reach the media config")
	}
	if m.AudioDevice != "my-sink.monitor" {
		t.Errorf("AudioDevice = %q, want the configured device", m.AudioDevice)
	}
}

// R35 AS5: the whose-audio card's logic, without a window.
//
// The card is the one place a wrong answer streams the wrong application's
// sound, so its rules — when it appears, what it preselects, what an answer
// does — are worth pinning here rather than discovering on a desktop.

// AD3: the card always appears in app mode, and the last-used application is
// preselected *only* when it is present and emitting. A remembered name that
// is not running is a memory, not a selection; presenting it as one would
// invite a blind Enter that captures nothing.
func TestAudioPromptPreselectsOnlyARunningLastChoice(t *testing.T) {
	apps := []pwproto.App{
		{Binary: "supertuxkart", Name: "SuperTuxKart", Streams: 1},
		{Binary: "firefox", Name: "Firefox", Streams: 2},
	}
	for _, tc := range []struct {
		name string
		last string
		apps []pwproto.App
		want string
	}{
		{"nothing remembered", "", apps, ""},
		{"remembered and running", "firefox", apps, "firefox"},
		{"remembered but not running", "mpv", apps, ""},
		{"remembered but nothing is playing", "firefox", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preselect(tc.last, tc.apps); got != tc.want {
				t.Errorf("preselect(%q) = %q, want %q", tc.last, got, tc.want)
			}
		})
	}
}

// The card opens, blocks the engine, and the click is what unblocks it.
func TestAudioPromptBlocksUntilAnswered(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})
	cfg.AudioApp = "supertuxkart"

	answered := make(chan engine.AudioTarget, 1)
	go func() {
		answered <- a.chooseAudioTarget(context.Background(), gst.AppAudioOffer{
			Apps: []pwproto.App{{Binary: "supertuxkart", Name: "SuperTuxKart", Streams: 1}},
		})
	}()

	// The card appears, with the remembered application preselected.
	var prompt *AudioPrompt
	waitFor(t, func() bool {
		prompt = a.AudioPromptState()
		return prompt != nil
	}, "the whose-audio card to open")
	if len(prompt.Apps) != 1 || prompt.Apps[0].Binary != "supertuxkart" {
		t.Fatalf("the card lists %+v, want the one running application", prompt.Apps)
	}
	if prompt.Preselect != "supertuxkart" {
		t.Errorf("preselect = %q, want supertuxkart", prompt.Preselect)
	}

	// Nothing has been answered yet.
	select {
	case target := <-answered:
		t.Fatalf("the engine proceeded without an answer: %+v", target)
	default:
	}

	a.AnswerAudioPrompt(engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "firefox"})
	select {
	case target := <-answered:
		if target.Mode != engine.AudioTargetApp || target.Binary != "firefox" {
			t.Errorf("the engine got %+v, want the clicked application", target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never reached the engine")
	}

	// The card closes, and the choice is remembered as a preselection for
	// next time — never as something that reuses itself silently.
	if a.AudioPromptState() != nil {
		t.Error("the card is still open after being answered")
	}
	if cfg.AudioApp != "firefox" {
		t.Errorf("cfg.AudioApp = %q, want firefox remembered for next time", cfg.AudioApp)
	}
}

// Answering the card is the one place in this app where the UI goroutine and
// the engine goroutine touch the config at the same time — and it is not an
// accident of timing but the shape of the thing: the answer is what *releases*
// the engine into the encoder cascade, whose OnEncoderChosen writes
// LastGoodEncoder. Every other saveCfg call site is serialized on the engine
// goroutine, so this pair is the only cross-goroutine one, and persisting the
// preselection must not race it (docs/39 F9).
//
// Under -race this fails if the save happens after the answer is handed over.
func TestAnsweringTheCardDoesNotRaceTheEngineWritingConfig(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})

	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		// The engine goroutine: blocked here until the card is answered…
		_ = a.chooseAudioTarget(context.Background(), gst.AppAudioOffer{
			Apps: []pwproto.App{{Binary: "supertuxkart", Name: "SuperTuxKart", Streams: 1}},
		})
		// …and then straight into the cascade, whose encoder callback writes
		// the config exactly like this (mediaFactory's OnEncoderChosen).
		a.mu.Lock()
		a.cfg.LastGoodEncoder = "vah264enc"
		a.mu.Unlock()
	}()

	waitFor(t, func() bool { return a.AudioPromptState() != nil }, "the whose-audio card to open")
	a.AnswerAudioPrompt(engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "supertuxkart"})
	<-engineDone

	// The preselection is still persisted — the fix is about *when*, not
	// whether.
	if cfg.AudioApp != "supertuxkart" {
		t.Errorf("cfg.AudioApp = %q, want the answered application remembered", cfg.AudioApp)
	}
	reloaded, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatalf("reloading the saved config: %v", err)
	}
	if reloaded.AudioApp != "supertuxkart" {
		t.Errorf("saved AudioApp = %q, want it on disk for the next start", reloaded.AudioApp)
	}
}

// A cancelled start (the user pressing Stop while the card is up) must not
// hang the engine: it answers whole-system audio, which is the pre-R35
// behavior and the only answer that is never surprising.
func TestAudioPromptCancelsToSystemAudio(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	ctx, cancel := context.WithCancel(context.Background())

	answered := make(chan engine.AudioTarget, 1)
	go func() { answered <- a.chooseAudioTarget(ctx, gst.AppAudioOffer{}) }()
	waitFor(t, func() bool { return a.AudioPromptState() != nil }, "the card to open")

	cancel()
	select {
	case target := <-answered:
		if target.Mode != engine.AudioTargetSystem {
			t.Errorf("a cancelled card answered %+v, want system audio", target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the card hung the engine")
	}
	if a.AudioPromptState() != nil {
		t.Error("the card is still open after cancellation")
	}
}

// D6's first row: with no helper, the card still appears — the shell is what
// tells the user — carrying the error so it can say a sentence instead of
// showing an empty list.
func TestAudioPromptCarriesTheUnavailableReason(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	go a.chooseAudioTarget(context.Background(), gst.AppAudioOffer{Err: appaudio.ErrHelperMissing})

	var prompt *AudioPrompt
	waitFor(t, func() bool {
		prompt = a.AudioPromptState()
		return prompt != nil
	}, "the card to open")
	if prompt.Err == nil {
		t.Fatal("the card was not told that per-application audio is unavailable")
	}
	if msg := Message(prompt.Err); !strings.Contains(msg, "gawk-pw-helper") {
		t.Errorf("the sentence shown is %q; it should name the missing program", msg)
	}
	a.AnswerAudioPrompt(engine.AudioTarget{Mode: engine.AudioTargetSystem})
}

// The list stays live while the card is open, so an application that starts
// playing after the card appears can be picked without restarting anything.
func TestAudioPromptListUpdatesWhileOpen(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	go a.chooseAudioTarget(context.Background(), gst.AppAudioOffer{})
	waitFor(t, func() bool { return a.AudioPromptState() != nil }, "the card to open")

	a.onAudioApps([]pwproto.App{{Binary: "late-starter", Name: "Late Starter", Streams: 1}})
	waitFor(t, func() bool {
		p := a.AudioPromptState()
		return p != nil && len(p.Apps) == 1 && p.Apps[0].Binary == "late-starter"
	}, "the list to update")
	a.AnswerAudioPrompt(engine.AudioTarget{Mode: engine.AudioTargetSystem})
}

// The silence hint (D6, ported from docs/38 D8) fires only when a *chosen
// application* has had no audio streams for the grace period, and only while
// live. Everything else — system audio, a live application, a broadcast that
// has not started — says nothing.
func TestSilenceHintFiresOnlyForASilentChosenApplication(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})

	// Not live: nothing to say.
	a.onAudioLinks(0)
	if hint := a.AudioSilenceHint(); hint != "" {
		t.Errorf("hint before going live: %q", hint)
	}

	a.mu.Lock()
	a.state = StateLive
	a.stats = engine.Stats{ShareMode: "window", AudioApp: "supertuxkart"}
	a.mu.Unlock()

	// Links present: the application is audible, so no hint.
	a.onAudioLinks(2)
	if hint := a.AudioSilenceHint(); hint != "" {
		t.Errorf("hint while the application is audible: %q", hint)
	}

	// Links gone, but only just: the grace period exists so a menu
	// transition or a loading screen does not trigger it.
	a.onAudioLinks(0)
	if hint := a.AudioSilenceHint(); hint != "" {
		t.Errorf("hint before the grace period elapsed: %q", hint)
	}

	// Silent for long enough.
	a.mu.Lock()
	a.audioSilentAt = time.Now().Add(-audioSilenceGrace - time.Second)
	a.mu.Unlock()
	hint := a.AudioSilenceHint()
	if !strings.Contains(hint, "supertuxkart") {
		t.Errorf("hint = %q, want it to name the silent application", hint)
	}
	if !strings.Contains(hint, "whole-system audio") {
		t.Errorf("hint = %q, want it to offer the escape hatch", hint)
	}

	// System audio never gets a hint: there is no application to blame and
	// nothing to switch to.
	a.mu.Lock()
	a.stats.AudioApp = ""
	a.mu.Unlock()
	if hint := a.AudioSilenceHint(); hint != "" {
		t.Errorf("hint for a system-audio broadcast: %q", hint)
	}
}

// The audio line names the application in app mode. It is the line a
// broadcaster glances at to confirm the right sound is going out, and "System
// audio" there while one game's audio is being captured would be a lie.
func TestAudioStatusNamesTheCapturedApplication(t *testing.T) {
	got := AudioStatus(engine.Stats{
		AudioState:  engine.AudioActive,
		AudioSource: "app-sink-monitor",
		AudioApp:    "supertuxkart",
	})
	if got != "Audio from supertuxkart only" {
		t.Errorf("AudioStatus = %q, want it to name the application", got)
	}
}

// The diagnostics dump carries both R35 fields, so "the wrong app's sound went
// out" is answerable from a pasted dump alone.
func TestDiagnosticsCarryTheShareModeAndApplication(t *testing.T) {
	fs := &fakeSession{}
	a, _ := testApp(t, fs, notify.Discard{})
	a.mu.Lock()
	a.stats = engine.Stats{ShareMode: "window", AudioApp: "supertuxkart", Width: 1542, Height: 1080, Fps: 60}
	a.mu.Unlock()

	var d Diagnostics
	if err := json.Unmarshal([]byte(a.Diagnostics()), &d); err != nil {
		t.Fatalf("diagnostics are not valid JSON: %v", err)
	}
	if d.ShareMode != "window" || d.AudioApp != "supertuxkart" {
		t.Errorf("diagnostics = %+v, want the share mode and application", d)
	}
	if !strings.HasPrefix(d.Rung, "1542x1080") {
		t.Errorf("rung = %q, want the fitted dimensions", d.Rung)
	}
}

// Pressing Stop while a start is in flight — during the share picker, the
// whose-audio card, or the encoder cascade — is the user's own action, not a
// fault. R35's card widened that window from a second to however long a person
// takes to choose, so it must not ring critically or leave an error on screen.
func TestCancelledStartIsNotReportedAsAFailure(t *testing.T) {
	n := &recordingNotifier{}
	fs := &fakeSession{startErr: &engine.StartError{
		Phase: engine.PhaseCapture,
		Err:   context.Canceled,
	}}
	a, _ := testApp(t, fs, n)
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateIdle }, "the start to end")

	if msg := a.LastError(); msg != "" {
		t.Errorf("a cancelled start left an error on screen: %q", msg)
	}
	for _, sent := range n.all() {
		if sent.urgency == notify.UrgencyCritical {
			t.Errorf("a cancelled start notified critically: %+v", sent)
		}
	}
	// And when it *is* rendered — in a diagnostics dump, say — it reads as a
	// sentence rather than a Go error.
	if got := Message(context.Canceled); !strings.Contains(got, "stopped before it started") {
		t.Errorf("Message(context.Canceled) = %q", got)
	}
}
