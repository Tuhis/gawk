package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

func testOpts(sess RelaySession, media *fakeMedia, clock Clock) Options {
	// One dial succeeds; every later one 404s. That second half matters now
	// that a lost transport is auto-resumed: without a terminal answer a test
	// that drops the session would reclaim the same dead fake forever. Tests
	// that exercise resuming supply their own dialer (resume_test.go).
	var dials atomic.Int32
	return Options{
		Clock: clock,
		Dial: func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
			if dials.Add(1) > 1 {
				return nil, http.StatusNotFound, errors.New("fake: broadcast not found")
			}
			return sess, http.StatusOK, nil
		},
		MediaFactory:  media.factory(),
		Log:           testLog,
		StatsInterval: time.Hour, // never fires on its own; tests call Stats()
	}
}

func TestPublishURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		relay  string
		id     string
		secret string
		resume string
		want   string
	}{
		{"mint", "https://relay.example:4433", "", "", "", "https://relay.example:4433/publish"},
		{"reclaim", "https://relay.example:4433", "K7M2QP", "", "", "https://relay.example:4433/publish/K7M2QP"},
		{"mint with secret", "https://relay.example:4433", "", "hunter2", "", "https://relay.example:4433/publish?secret=hunter2"},
		{"reclaim with secret", "https://relay.example:4433", "K7M2QP", "hunter2", "", "https://relay.example:4433/publish/K7M2QP?secret=hunter2"},
		// R17: every /publish/{id} claim needs the resume token minted at
		// first publish, as the `resume` query param (hex).
		{"reclaim with token", "https://relay.example:4433", "K7M2QP", "", "abab", "https://relay.example:4433/publish/K7M2QP?resume=abab"},
		{"reclaim with secret and token", "https://relay.example:4433", "K7M2QP", "hunter2", "abab", "https://relay.example:4433/publish/K7M2QP?resume=abab&secret=hunter2"},
		// A mint has no ID for the relay to verify a token against; the
		// param is meaningless there and must not leak into the URL.
		{"mint ignores stale token", "https://relay.example:4433", "", "", "abab", "https://relay.example:4433/publish"},
		// A relay URL carrying a path is a plausible reverse-proxy setup; the
		// join must not silently drop it.
		{"trailing slash", "https://relay.example/", "", "", "", "https://relay.example/publish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PublishURL(tc.relay, tc.id, tc.secret, tc.resume)
			if err != nil {
				t.Fatalf("PublishURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("PublishURL(%q, %q, %q, %q) = %q, want %q", tc.relay, tc.id, tc.secret, tc.resume, got, tc.want)
			}
		})
	}
}

// The native broadcaster sends an Origin header on the dial (the browser sends
// one automatically and can't change it; a native client sends none by default,
// which a relay with -allowed-origins rejects). Default when unset, and the
// override wins when set — so an operator can reuse an already-whitelisted
// origin instead of adding a relay entry.
func TestDialSendsOrigin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfgOrigin  string
		wantOrigin string
	}{
		{"default when unset", "", DefaultOrigin},
		{"override wins", "https://gawk.example", "https://gawk.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := newFakeSession()
			media := newFakeMedia()
			gotOrigin := make(chan string, 1)
			s := New(
				Config{RelayURL: "https://relay.example", Origin: tc.cfgOrigin},
				Callbacks{},
				Options{
					Clock: &FakeClock{},
					Dial: func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
						gotOrigin <- origin
						return sess, http.StatusOK, nil
					},
					MediaFactory:  media.factory(),
					Log:           testLog,
					StatsInterval: time.Hour,
				},
			)
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer s.Stop()
			select {
			case got := <-gotOrigin:
				if got != tc.wantOrigin {
					t.Errorf("dial Origin = %q, want %q", got, tc.wantOrigin)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("dial was never called")
			}
		})
	}
}

// The secret travels as a query param because the browser's WebTransport API
// cannot set headers (R2) and the relay reads it from the query. A native
// client could send a header — and would then authenticate differently from
// the browser, which is the point of pinning this.
func TestSecretIsAQueryParamNotAHeader(t *testing.T) {
	got, err := PublishURL("https://relay.example", "", "s3cret", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "secret=s3cret") {
		t.Errorf("URL = %q, want a secret query param", got)
	}
}

func TestStartConnectPhaseCarriesStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "secret"},
		{http.StatusNotFound, "no longer exists"},
		{http.StatusConflict, "already publishing"},
		{http.StatusTooManyRequests, "capacity"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, Options{
				Dial: func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
					return nil, tc.status, errors.New("received status")
				},
				Log: testLog,
			})
			err := s.Start(context.Background())
			se, ok := AsStartError(err)
			if !ok {
				t.Fatalf("Start error = %v, want *StartError", err)
			}
			if se.Phase != PhaseConnect {
				t.Errorf("phase = %q, want %q", se.Phase, PhaseConnect)
			}
			if se.Status != tc.status {
				t.Errorf("status = %d, want %d", se.Status, tc.status)
			}
			// Decision 10's payoff: the browser cannot tell these apart at
			// all, so each one must become a sentence a friend can act on.
			if msg := se.Message(); !strings.Contains(strings.ToLower(msg), tc.want) {
				t.Errorf("Message() = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

// A dial that never reached HTTP (DNS, TLS, refused) has no status — and the
// message must still be a sentence rather than a bare Go error.
func TestStartConnectPhaseWithoutStatus(t *testing.T) {
	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, Options{
		Dial: func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
			return nil, 0, errors.New("connection refused")
		},
		Log: testLog,
	})
	se, ok := AsStartError(s.Start(context.Background()))
	if !ok {
		t.Fatal("want *StartError")
	}
	if se.Status != 0 {
		t.Errorf("status = %d, want 0", se.Status)
	}
	if !strings.Contains(se.Message(), "Could not reach the relay") {
		t.Errorf("Message() = %q", se.Message())
	}
}

// A capture-phase failure had a live publisher session. It must tear that
// session down — a leaked one is a zombie publisher holding the broadcast ID
// hostage (CODE-REVIEW.md) — and it must be labelled `capture`, because the
// reclaim→mint fallback is only ever legal on `connect`.
func TestStartCapturePhaseTearsDownSession(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	media.startErr = errors.New("portal denied")

	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, testOpts(sess, media, &FakeClock{}))
	se, ok := AsStartError(s.Start(context.Background()))
	if !ok {
		t.Fatalf("want *StartError")
	}
	if se.Phase != PhaseCapture {
		t.Fatalf("phase = %q, want %q — a capture failure must never license minting a new ID", se.Phase, PhaseCapture)
	}
	if !sess.isClosed() {
		t.Error("relay session still open after a capture-phase failure: zombie publisher holding the broadcast ID")
	}
}

// The media source itself failing to build (no hardware encoder) is also the
// capture phase, and must also release the session.
func TestNoHardwareEncoderIsCapturePhaseAndReleases(t *testing.T) {
	sess := newFakeSession()
	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, Options{
		Dial: func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
			return sess, http.StatusOK, nil
		},
		MediaFactory: func(cfg MediaConfig, clock Clock, log *slog.Logger) (MediaSource, error) {
			return nil, ErrNoHardwareEncoder
		},
		Log: testLog,
	})
	se, ok := AsStartError(s.Start(context.Background()))
	if !ok {
		t.Fatal("want *StartError")
	}
	if se.Phase != PhaseCapture {
		t.Errorf("phase = %q, want %q", se.Phase, PhaseCapture)
	}
	if !errors.Is(se, ErrNoHardwareEncoder) {
		t.Errorf("error does not unwrap to ErrNoHardwareEncoder: %v", se)
	}
	if !sess.isClosed() {
		t.Error("relay session leaked when the encoder cascade refused")
	}
}

func TestStartAnnouncesBroadcastID(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	msg, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatal(err)
	}
	sess.incoming <- announceStream(msg)

	got := make(chan string, 1)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnBroadcastID: func(id string) { got <- id }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	select {
	case id := <-got:
		if id != "K7M2QP" {
			t.Errorf("id = %q, want K7M2QP", id)
		}
		if s.BroadcastID() != "K7M2QP" {
			t.Errorf("BroadcastID() = %q, want K7M2QP", s.BroadcastID())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no broadcast ID announced")
	}
}

// R17 (relay scale-out) sends BroadcastAnnounce (0x03) and ResumeToken (0x09)
// on two separate server-initiated uni streams — and webtransport-go delivers
// incoming streams in no guaranteed order (the PR #47 rebase measured the
// token stream beating the announce in roughly half of real dials). Server
// messages are dispatched by wire type, never by arrival order (the same rule
// as broadcaster.ts).
func TestServerStreamDispatchIsByTypeNotArrivalOrder(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	tokenMsg, err := wire.AppendResumeToken(nil, bytes.Repeat([]byte{0xab}, 16))
	if err != nil {
		t.Fatal(err)
	}
	announce, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatal(err)
	}
	// Token first: the arrival order that broke the single-accept read.
	sess.incoming <- announceStream(tokenMsg)
	sess.incoming <- announceStream(announce)

	got := make(chan string, 1)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnBroadcastID: func(id string) { got <- id }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	select {
	case id := <-got:
		if id != "K7M2QP" {
			t.Errorf("id = %q, want K7M2QP", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no broadcast ID: a resume-token stream arriving first must not eat the announce")
	}
}

// R28's TelemetryHello (0x0D) is the third and last server message the relay
// sends a publisher, and it is the only one whose loss is *silent*: the
// broadcast streams perfectly and simply never reports. It had no dispatch test
// at all, which is part of why a 25-minute session that reported nothing
// (2026-07-27) took relay pod logs to even localize. Ordered here as the relay
// actually writes them, hello last.
func TestTelemetryHelloIsDispatchedBehindTheOtherServerMessages(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	announce, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatal(err)
	}
	tokenMsg, err := wire.AppendResumeToken(nil, bytes.Repeat([]byte{0xab}, 16))
	if err != nil {
		t.Fatal(err)
	}
	helloMsg, err := wire.AppendTelemetryHello(nil, wire.TelemetryHello{
		Enabled:          true,
		ReportIntervalMs: 2000,
		Token:            bytes.Repeat([]byte{0x11}, wire.TelemetrySessionTokenSize),
		BroadcastKey:     bytes.Repeat([]byte{0x22}, wire.TelemetryBroadcastKeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess.incoming <- announceStream(announce)
	sess.incoming <- announceStream(tokenMsg)
	sess.incoming <- announceStream(helloMsg)

	got := make(chan wire.TelemetryHello, 1)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnTelemetryHello: func(h wire.TelemetryHello) { got <- h }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	select {
	case h := <-got:
		if !h.Enabled {
			t.Error("hello dispatched with Enabled=false; the fleet flag was not carried")
		}
		if h.ReportIntervalMs != 2000 {
			t.Errorf("ReportIntervalMs = %d, want 2000", h.ReportIntervalMs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the telemetry hello was never dispatched: this session would stream fine and report nothing")
	}
}

// R37's TelemetryEndpoint (0x12, docs/40 §4.10) rides its own uni stream like
// every server message. The URL here is deliberately longer than the old
// 258-byte announce read cap: the wire allows up to 512 URL bytes, and a read
// limit sized for the announce would silently truncate the message into a
// parse failure — a session that streams fine and reports to the wrong
// operator's collector, with nothing visible anywhere.
func TestTelemetryEndpointIsDispatched(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	longURL := "https://gawk.example.com/api/telemetry/v1/ingest?pad=" + strings.Repeat("x", 300)
	msg, err := wire.AppendTelemetryEndpoint(nil, longURL)
	if err != nil {
		t.Fatal(err)
	}
	sess.incoming <- announceStream(msg)

	got := make(chan string, 1)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnTelemetryEndpoint: func(u string) { got <- u }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	select {
	case u := <-got:
		if u != longURL {
			t.Errorf("advertised URL = %q, want %q", u, longURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the telemetry endpoint was never dispatched: a foreign relay's telemetry would silently go nowhere")
	}
}

// A malformed 0x12 degrades to "no advertised URL" (docs/40 F7): no callback,
// and the session keeps working — later server messages still dispatch.
func TestMalformedTelemetryEndpointIsIgnored(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	msg, err := wire.AppendTelemetryEndpoint(nil, "https://gawk.example.com/ingest")
	if err != nil {
		t.Fatal(err)
	}
	msg[2] = 0x01 // a reserved flag bit — strict, unlike trailing bytes
	sess.incoming <- announceStream(msg)
	announce, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatal(err)
	}
	sess.incoming <- announceStream(announce)

	gotID := make(chan string, 1)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{
			OnTelemetryEndpoint: func(u string) { t.Errorf("a malformed endpoint message was honored: %q", u) },
			OnBroadcastID:       func(id string) { gotID <- id },
		},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	select {
	case <-gotID:
	case <-time.After(2 * time.Second):
		t.Fatal("a malformed telemetry endpoint stopped later server messages from dispatching")
	}
}

// An unknown server-message type is skipped, not an error: a newer relay
// (version skew during a rolling upgrade) allocates types this binary has
// never heard of, and each rides its own stream — ignoring one must not cost
// the messages that follow. 0x13 is the next unallocated type today, so this
// is literally tomorrow's message arriving early.
func TestUnknownServerMessageTypeIsTolerated(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	sess.incoming <- announceStream([]byte{wire.Version, 0x13, 0xde, 0xad, 0xbe, 0xef})
	announce, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatal(err)
	}
	sess.incoming <- announceStream(announce)

	gotID := make(chan string, 1)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnBroadcastID: func(id string) { gotID <- id }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	select {
	case <-gotID:
	case <-time.After(2 * time.Second):
		t.Fatal("an unknown-type stream broke server-message dispatch")
	}
}

// The captured token is surfaced (hex, the relay's query-param encoding) so
// the app can persist it beside the broadcast ID — an R17 relay refuses every
// /publish/{id} claim without it.
func TestResumeTokenIsCapturedAndReported(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	raw := bytes.Repeat([]byte{0xcd}, 16)
	tokenMsg, err := wire.AppendResumeToken(nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	announce, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatal(err)
	}
	sess.incoming <- announceStream(announce)
	sess.incoming <- announceStream(tokenMsg)

	got := make(chan string, 1)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnResumeToken: func(token string) { got <- token }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	select {
	case token := <-got:
		if want := hex.EncodeToString(raw); token != want {
			t.Errorf("token = %q, want %q", token, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no resume token reported")
	}
}

// docs/06 says twice that the announce read must not gate media start, and
// R1's review found an implementation that awaited it and could hang forever.
// So: a relay that accepts the session and then never announces must still
// produce a live, sending broadcast — the code just never appears.
//
// This is a deliberate deviation from V1's acceptance criterion as worded ("a
// relay that never sends one fails Start cleanly"): failing Start there would
// reintroduce exactly the coupling docs/06 forbids, and would break Resume,
// where we already know the ID and the announce only confirms it.
func TestSilentRelayDoesNotHangOrFailStart(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	sess.incoming <- &fakeReceiveStream{never: true}

	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, testOpts(sess, media, &FakeClock{}))

	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start failed on a silent relay: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start hung waiting for an announce that never came")
	}
	defer s.Stop()

	// And media really is flowing despite the missing code.
	media.frames <- AccessUnit{Data: []byte{0, 0, 0, 1, 0x41, 0xaa}, TimestampUs: 1}
	waitFor(t, func() bool { return len(sess.sentDatagrams()) > 0 }, "delta datagram sent without an announce")
}

// Stop must not wait out the announce read deadline. A pending Read does not
// watch a context, so ending the session has to actively abort it; without
// that, closing the GUI window appears to hang for the whole announce timeout.
func TestStopIsPromptWhileAnnounceReadIsPending(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	sess.incoming <- &fakeReceiveStream{never: true}

	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() hung on the pending announce read (it must abort it, not wait it out)")
	}
}

// A hostile or broken relay must not be able to grow our heap through the
// announce stream: the message is ≤258 bytes by construction.
func TestAnnounceReadIsBounded(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	// A megabyte of plausible-looking announce.
	flood := make([]byte, 1<<20)
	flood[0] = wire.Version
	flood[1] = wire.TypeBroadcastAnnounce
	flood[2] = 255
	sess.incoming <- announceStream(flood)

	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnBroadcastID: func(id string) { t.Errorf("announced %q from a malformed flood", id) }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	// The read is capped at serverMessageReadLimit, so the parse sees 517
	// bytes (not a megabyte) and rejects them as a length mismatch.
	time.Sleep(50 * time.Millisecond)
	if s.BroadcastID() != "" {
		t.Errorf("BroadcastID = %q, want empty", s.BroadcastID())
	}
}

func TestAnnounceReadSetsDeadline(t *testing.T) {
	msg, _ := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	str := &fakeReceiveStream{r: strings.NewReader(string(msg))}
	sess := newFakeSession()
	media := newFakeMedia()
	sess.incoming <- str

	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return !str.readDeadline().IsZero() }, "read deadline set on the announce stream")
}

// Decision 6's correctness argument in one assertion: ClockMapping relates
// frame timestamps to the relay clock via the TimeSync offset, so if these two
// read different clocks the mapping is between unrelated timelines and every
// viewer's absolute latency is nonsense — silently.
func TestOneClockFeedsFramesAndTimeSync(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	clock := &FakeClock{Us: 12345}

	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, testOpts(sess, media, clock))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if got := media.gotClock(); got != Clock(clock) {
		t.Fatalf("media source was built with a different clock (%p) than the session's (%p)", got, clock)
	}

	// The TimeSync ping must be stamped from that same clock.
	waitFor(t, func() bool {
		for _, d := range sess.sentDatagrams() {
			if len(d) == wire.TimeSyncSize && d[1] == wire.TypeTimeSync {
				t0, _, err := wire.ParseTimeSync(d)
				return err == nil && t0 == clock.Us
			}
		}
		return false
	}, "a TimeSync ping stamped from the session clock")
}

// R18 (docs/23 Decision 7): the relay pushes the live viewer count as a
// datagram; the engine surfaces it in Stats and fires OnViewerCount, without
// disturbing the TimeSync replies sharing the read loop. A malformed push is
// dropped, keeping the last good count.
func TestViewerCountPushUpdatesStatsAndFiresCallback(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()

	var mu sync.Mutex
	var counts []uint32
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnViewerCount: func(count uint32) {
			mu.Lock()
			counts = append(counts, count)
			mu.Unlock()
		}},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if st := s.Stats(); st.ViewerCountAvailable {
		t.Error("ViewerCountAvailable = true before any push")
	}

	// Wait on the CALLBACK, not on Stats. setViewerCount publishes the stats
	// and only then fires the callback, so waiting on the stats and asserting
	// on the callback immediately after loses that window — a flake that
	// additionally read `counts` unlocked in the failure message, which is
	// what the race detector reported (CI run 30027598262). Stats are
	// guaranteed set by the time the callback has fired, so they are asserted
	// after rather than waited on.
	sess.receiveDatagrams <- wire.AppendViewerCount(nil, 2)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(counts) == 1 && counts[0] == 2
	}, "the first OnViewerCount call")
	if st := s.Stats(); !st.ViewerCountAvailable || st.ViewerCount != 2 {
		t.Fatalf("Stats = {available:%v count:%d}, want {true 2}", st.ViewerCountAvailable, st.ViewerCount)
	}

	// Malformed (bad version): dropped, count unchanged.
	bad := wire.AppendViewerCount(nil, 9)
	bad[0] = 0x02
	sess.receiveDatagrams <- bad
	// A subsequent valid push still lands.
	sess.receiveDatagrams <- wire.AppendViewerCount(nil, 3)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(counts) == 2
	}, "the second OnViewerCount call")
	mu.Lock()
	defer mu.Unlock()
	if counts[1] != 3 {
		t.Errorf("OnViewerCount calls = %v, want [2 3] — the malformed push was not dropped", counts)
	}
	if st := s.Stats(); st.ViewerCount != 3 {
		t.Errorf("Stats.ViewerCount = %d, want 3", st.ViewerCount)
	}
}

func TestStopIsIdempotentAndEndsOnce(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	var mu sync.Mutex
	ended := 0

	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnEnded: func() { mu.Lock(); ended++; mu.Unlock() }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if !media.wasStopped() {
		t.Error("media source not stopped")
	}
	if !sess.isClosed() {
		t.Error("relay session not closed: publisher slot left held")
	}
	mu.Lock()
	defer mu.Unlock()
	if ended != 1 {
		t.Errorf("OnEnded fired %d times, want exactly 1", ended)
	}
}

// The relay closing the session (grace expiry, restart, eviction) is the same
// "session over" event as Stop, and must reach OnEnded exactly once.
func TestRelayCloseEndsSession(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	ended := make(chan struct{}, 4)

	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnEnded: func() { ended <- struct{}{} }},
		testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess.cancel() // the relay went away

	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("OnEnded never fired after the relay closed the session")
	}
	s.Stop()
	if len(ended) != 0 {
		t.Errorf("OnEnded fired again from Stop: %d extra", len(ended))
	}
}

func TestStartTwiceRejected(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	s := New(Config{RelayURL: "https://relay.example"}, Callbacks{}, testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.Start(context.Background()); err == nil {
		t.Error("second Start succeeded; want an error")
	}
}

// Decision 20: the child owns capture, so this stage is unobservable here.
// Reporting the requested rate would answer "is the source keeping up?"
// wrongly while looking entirely plausible.
func TestCaptureFpsIsNeverFabricated(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	cfg := Config{RelayURL: "https://relay.example", Media: DefaultMediaConfig()}
	s := New(cfg, Callbacks{}, testOpts(sess, media, &FakeClock{}))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	st := s.Stats()
	if st.CaptureFpsAvailable {
		t.Error("CaptureFpsAvailable = true; the GStreamer child owns capture, we cannot see it")
	}
	if st.Fps != 60 || st.Width != 1920 {
		t.Errorf("rung = %dx%d@%d, want 1920x1080@60", st.Width, st.Height, st.Fps)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
