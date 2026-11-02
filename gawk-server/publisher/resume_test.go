package publisher

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Auto-resume (R17 W2's broadcaster half, ported to the native engine).
//
// The incident these tests exist for: on 2026-07-27 a native broadcaster's
// publisher session died 78 minutes in ("publisher session ended", reason
// "context canceled"), the engine fired OnEnded and *kept capturing and
// encoding into the dead session*, and 60 s later the relay garbage-collected
// a broadcast whose broadcaster was still running. Nothing reclaimed the ID
// inside the grace window, and nothing stopped the screen cast.

type dialResult struct {
	sess   RelaySession
	status int
	err    error
}

// recordingDialer hands out results in order and records the URLs it was
// asked for — which is how the reclaim assertions read the broadcast ID and
// resume token off the wire rather than off the Session's private state.
type recordingDialer struct {
	mu      sync.Mutex
	results []dialResult
	urls    []string
	calls   int
	// blockAfter, when non-nil, makes every call past len(results) park until
	// the dial context is cancelled (a relay that never answers).
	blockAfter chan struct{}
}

func (d *recordingDialer) fn() DialFunc {
	return func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
		d.mu.Lock()
		d.urls = append(d.urls, rawURL)
		i := d.calls
		d.calls++
		var (
			r     dialResult
			known = i < len(d.results)
		)
		if known {
			r = d.results[i]
		}
		block := d.blockAfter
		d.mu.Unlock()

		if known {
			return r.sess, r.status, r.err
		}
		if block != nil {
			<-ctx.Done()
			return nil, 0, ctx.Err()
		}
		// Past the script: the broadcast is gone. Terminal, so a test that
		// forgets a result fails fast instead of spinning.
		return nil, http.StatusNotFound, errors.New("fake: broadcast not found")
	}
}

func (d *recordingDialer) dialedURLs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.urls...)
}

func resumeOpts(d *recordingDialer, media *fakeMedia) Options {
	return Options{
		Clock:         &FakeClock{},
		Dial:          d.fn(),
		MediaFactory:  media.factory(),
		Log:           testLog,
		StatsInterval: time.Hour,
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("%s never happened", what)
	}
}

// The headline criterion: a mid-session transport loss reconnects on its own,
// with no user action, and does not end the broadcast.
func TestRelayLossResumesWithoutUserAction(t *testing.T) {
	first, second := newFakeSession(), newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{results: []dialResult{{first, http.StatusOK, nil}, {second, http.StatusOK, nil}}}

	resumed := make(chan struct{}, 4)
	ended := make(chan struct{}, 4)
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
		Callbacks{
			OnResumed: func() { resumed <- struct{}{} },
			OnEnded:   func() { ended <- struct{}{} },
		},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	first.cancel() // the relay went away mid-broadcast
	waitSignal(t, resumed, 3*time.Second, "auto-resume")

	// The pipeline survived: capture was never stopped, and frames now travel
	// on the new session.
	if media.wasStopped() {
		t.Error("capture was stopped by a recoverable transport loss")
	}
	media.frames <- AccessUnit{Data: []byte{0x00, 0x00, 0x01, 0x41, 0x99}, TimestampUs: 1}

	deadline := time.Now().Add(2 * time.Second)
	for len(second.sentDatagrams()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no frame reached the resumed session")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(ended) != 0 {
		t.Errorf("OnEnded fired on a recoverable disconnect (%d times)", len(ended))
	}
	if got := s.Stats().Resumes; got != 1 {
		t.Errorf("Stats().Resumes = %d, want 1", got)
	}
}

// The reclaim must carry the ID and the token, or the relay refuses it (R17:
// every /publish/{id} claim needs a resume token) and the viewers are orphaned
// exactly as they were before.
func TestResumeReclaimsTheSameBroadcastCodeWithItsToken(t *testing.T) {
	first, second := newFakeSession(), newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{results: []dialResult{{first, http.StatusOK, nil}, {second, http.StatusOK, nil}}}

	gotID := make(chan struct{}, 4)
	gotToken := make(chan struct{}, 4)
	resumed := make(chan struct{}, 4)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{
			OnBroadcastID: func(string) { gotID <- struct{}{} },
			OnResumeToken: func(string) { gotToken <- struct{}{} },
			OnResumed:     func() { resumed <- struct{}{} },
		},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	announce, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatal(err)
	}
	token, err := wire.AppendResumeToken(nil, bytes.Repeat([]byte{0xab}, 16))
	if err != nil {
		t.Fatal(err)
	}
	first.incoming <- announceStream(announce)
	first.incoming <- announceStream(token)
	waitSignal(t, gotID, 2*time.Second, "announce")
	waitSignal(t, gotToken, 2*time.Second, "resume token")

	first.cancel()
	waitSignal(t, resumed, 3*time.Second, "auto-resume")

	urls := d.dialedURLs()
	if len(urls) < 2 {
		t.Fatalf("want a reclaim dial, got %v", urls)
	}
	reclaim := urls[1]
	if !strings.Contains(reclaim, "/publish/K7M2QP") {
		t.Errorf("reclaim dialed %q, want /publish/K7M2QP", reclaim)
	}
	if !strings.Contains(reclaim, "resume="+strings.Repeat("ab", 16)) {
		t.Errorf("reclaim dialed %q, want the resume token", reclaim)
	}
}

// A dial that fails for a transient reason is retried — that is the whole
// point of a grace window.
func TestResumeRetriesATransientDialFailure(t *testing.T) {
	t.Parallel()
	first, second := newFakeSession(), newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{results: []dialResult{
		{first, http.StatusOK, nil},
		{nil, 0, errors.New("connection refused")},
		{nil, http.StatusTooManyRequests, errors.New("at capacity")},
		{second, http.StatusOK, nil},
	}}

	resumed := make(chan struct{}, 4)
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
		Callbacks{OnResumed: func() { resumed <- struct{}{} }},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	first.cancel()
	waitSignal(t, resumed, 5*time.Second, "auto-resume after transient failures")
	if media.wasStopped() {
		t.Error("capture was stopped while the reclaim was still being retried")
	}
}

// 404 means the relay's grace expired and the broadcast is gone: retrying can
// only ever fail, so the session ends — and ending must stop the screen cast.
func TestResumeStopsCaptureWhenTheBroadcastIsGone(t *testing.T) {
	first := newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{results: []dialResult{{first, http.StatusOK, nil}}}

	ended := make(chan struct{}, 4)
	failed := make(chan error, 4)
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
		Callbacks{
			OnEnded: func() { ended <- struct{}{} },
			OnError: func(err error) { failed <- err },
		},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	first.cancel()
	waitSignal(t, ended, 5*time.Second, "OnEnded after an unrecoverable loss")

	if !media.wasStopped() {
		t.Error("capture kept running after the broadcast ended: the screen is still being shared")
	}
	if len(failed) == 0 {
		t.Error("giving up on the reclaim reported no error, so the shell cannot say why")
	}
	if calls := len(d.dialedURLs()); calls != 2 {
		t.Errorf("dialed %d times, want 2 (the original plus one terminal reclaim)", calls)
	}
}

// Capture dying is session-fatal, and must release the publisher slot rather
// than leaving the relay holding the ID until its grace expires.
func TestCaptureEndTearsDownTheRelaySession(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{results: []dialResult{{sess, http.StatusOK, nil}}}

	ended := make(chan struct{}, 4)
	s := New(Config{RelayURL: "https://relay.example"},
		Callbacks{OnEnded: func() { ended <- struct{}{} }},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	close(media.frames) // the GStreamer child died
	waitSignal(t, ended, 2*time.Second, "OnEnded after capture ended")

	if !sess.isClosed() {
		t.Error("the relay session was left open, holding the broadcast ID hostage")
	}
	if !media.wasStopped() {
		t.Error("the media source was never stopped")
	}
}

// "Newest publisher wins" only converges because the deposed one stays down.
// An engine that auto-resumed here would reclaim the code straight back and
// the two broadcasters would depose each other forever.
//
// 4006 (R39, docs/42 §4.4) joins the same set for a different reason: the
// operator killed the broadcast and banned the ID for at least the kill
// cooldown, so a reclaim can only ever collect a 451. Each code must also
// surface a sentence saying which of these happened — "the relay closed this
// session (code 4006)" would tell the broadcaster nothing.
func TestSupersededPublisherDoesNotResume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    uint32
		wantMsg string
	}{
		{"superseded", wire.CloseCodePublisherSuperseded, "superseded"},
		{"broadcast ended", wire.CloseCodeBroadcastEnded, "the relay ended this broadcast"},
		{"terminated by operator", wire.CloseCodeTerminatedByOperator, "terminated by the server operator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := newFakeSession()
			media := newFakeMedia()
			d := &recordingDialer{results: []dialResult{
				{first, http.StatusOK, nil},
				{newFakeSession(), http.StatusOK, nil}, // available, and must go unused
			}}

			ended := make(chan struct{}, 4)
			failed := make(chan error, 4)
			s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
				Callbacks{
					OnEnded: func() { ended <- struct{}{} },
					OnError: func(err error) { failed <- err },
				},
				resumeOpts(d, media))
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Stop()

			first.closedByRelay(tc.code)
			waitSignal(t, ended, 3*time.Second, "OnEnded after a terminal close code")

			if calls := len(d.dialedURLs()); calls != 1 {
				t.Errorf("dialed %d times, want 1 — a terminal close code must not be reclaimed", calls)
			}
			if !media.wasStopped() {
				t.Error("capture kept running after a terminal close code")
			}
			select {
			case err := <-failed:
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("OnError = %q, want it to mention %q", err, tc.wantMsg)
				}
			default:
				t.Error("a terminal close code reported no error, so the shell cannot say why")
			}
		})
	}
}

// R39 (docs/42 D15 + §4.4): a ban is enforced pre-upgrade, so a reclaim gets
// an HTTP 451 with no close code to carry the reason. The native broadcasters
// can read that status where the browser cannot, and must both stop at once
// (the cap on repeated 403/451 is one attempt — see resumeTerminal) and say
// "banned" rather than "could not reach the relay".
func TestBannedReclaimStopsWithABannedMessage(t *testing.T) {
	first := newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{results: []dialResult{
		{first, http.StatusOK, nil},
		{nil, http.StatusUnavailableForLegalReasons, errors.New("received status")},
		{newFakeSession(), http.StatusOK, nil}, // available, and must go unused
	}}

	ended := make(chan struct{}, 4)
	failed := make(chan error, 4)
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
		Callbacks{
			OnEnded: func() { ended <- struct{}{} },
			OnError: func(err error) { failed <- err },
		},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	first.cancel() // an abrupt death: exactly the case auto-resume exists for
	waitSignal(t, ended, 5*time.Second, "OnEnded after a banned reclaim")

	if calls := len(d.dialedURLs()); calls != 2 {
		t.Errorf("dialed %d times, want 2 (the original plus ONE 451 reclaim)", calls)
	}
	if !media.wasStopped() {
		t.Error("capture kept running after the relay banned this broadcast")
	}
	select {
	case err := <-failed:
		se, ok := AsStartError(err)
		if !ok {
			t.Fatalf("OnError = %v, want a wrapped *StartError", err)
		}
		if se.Status != http.StatusUnavailableForLegalReasons {
			t.Errorf("status = %d, want 451", se.Status)
		}
		if msg := se.Message(); !strings.Contains(strings.ToLower(msg), "banned") {
			t.Errorf("Message() = %q, want it to say the broadcast is banned", msg)
		}
	default:
		t.Error("a banned reclaim reported no error, so the shell cannot say why")
	}
}

// A drain (4002) is exactly what auto-resume is for: the pod is going away,
// the broadcast is not.
func TestDrainingRelayIsResumed(t *testing.T) {
	first, second := newFakeSession(), newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{results: []dialResult{{first, http.StatusOK, nil}, {second, http.StatusOK, nil}}}

	resumed := make(chan struct{}, 4)
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
		Callbacks{OnResumed: func() { resumed <- struct{}{} }},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	first.closedByRelay(wire.CloseCodeServerDraining)
	waitSignal(t, resumed, 3*time.Second, "auto-resume after a rollout drain")
}

// Stop during a reclaim must not wait out the resume window.
func TestStopDuringResumeReturnsPromptly(t *testing.T) {
	first := newFakeSession()
	media := newFakeMedia()
	d := &recordingDialer{
		results:    []dialResult{{first, http.StatusOK, nil}},
		blockAfter: make(chan struct{}),
	}

	resuming := make(chan struct{}, 4)
	ended := make(chan struct{}, 4)
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
		Callbacks{
			OnResuming: func(int, error) { resuming <- struct{}{} },
			OnEnded:    func() { ended <- struct{}{} },
		},
		resumeOpts(d, media))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	first.cancel()
	waitSignal(t, resuming, 3*time.Second, "the first reclaim attempt")

	done := make(chan struct{})
	go func() { _ = s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() hung while a reclaim was in flight")
	}
	waitSignal(t, ended, time.Second, "OnEnded after Stop")
	if !media.wasStopped() {
		t.Error("Stop() left capture running")
	}
}

// A terminal close reaches OnError typed, with the relay's code, so a shell
// (gawk-whip-gateway, docs/83 D8) never has to match the message.
func TestCloseCodeErrorIsTypedWithItsCode(t *testing.T) {
	for _, code := range []uint32{wire.CloseCodeBroadcastEnded, wire.CloseCodePublisherSuperseded, wire.CloseCodeTerminatedByOperator, 4999} {
		var ce *CloseError
		if err := closeCodeError(code); !errors.As(err, &ce) || ce.Code != code || ce.Error() == "" {
			t.Errorf("closeCodeError(%d) = %v, want a *CloseError with that code", code, err)
		}
	}
}

// A stop that lands while a reclaim dial is in flight must close the session
// that dial returns, with the stop's code. Before, supervise attached it
// after teardown had already run, and nothing ever closed it: a publisher
// with no media held the slot for the life of the process, and a WHIP
// DELETE's 4014 never reached the relay (docs/83 D8).
func TestStopBroadcastDuringAReclaimDialClosesTheResumedSession(t *testing.T) {
	first, second := newFakeSession(), newFakeSession()
	media := newFakeMedia()
	dialing, release := make(chan struct{}), make(chan struct{})
	calls := 0
	var mu sync.Mutex
	dial := func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return first, http.StatusOK, nil
		}
		if n == 2 {
			close(dialing)
			<-release // the dial completes regardless of the stop
			return second, http.StatusOK, nil
		}
		return nil, http.StatusNotFound, errors.New("fake: no more dials")
	}
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP", ResumeToken: "00"},
		Callbacks{},
		Options{Clock: &FakeClock{}, Dial: dial, MediaFactory: media.factory(), Log: testLog, StatsInterval: time.Hour})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.cancel()
	waitSignal(t, dialing, 3*time.Second, "the reclaim dial")

	done := make(chan struct{})
	go func() { _ = s.StopBroadcast(); close(done) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("StopBroadcast hung")
	}
	if !second.isClosed() {
		t.Fatal("the session the in-flight reclaim returned was never closed")
	}
	if got := second.lastCloseCode(); got != webtransport.SessionErrorCode(wire.CloseCodeBroadcasterStopped) {
		t.Errorf("resumed session closed with %d, want 4014", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("%d dials, want 2: the stop was delivered on the resumed session, no extra reclaim", calls)
	}
}

// A stop while the engine is between reclaim attempts (the relay drained it,
// say) has no live session to send 4014 on. It makes one bounded reclaim and
// closes that with 4014 at once, so the broadcast still ends instead of
// waiting out the grace — what the desktop engine's end() does when paused.
func TestStopBroadcastWhileResumingDeliversTheStopOnOneReclaim(t *testing.T) {
	first, stopLeg := newFakeSession(), newFakeSession()
	media := newFakeMedia()
	resuming := make(chan struct{}, 8)
	var mu sync.Mutex
	var urls []string
	stopping := false
	dial := func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
		mu.Lock()
		defer mu.Unlock()
		urls = append(urls, rawURL)
		if len(urls) == 1 {
			return first, http.StatusOK, nil
		}
		if stopping {
			return stopLeg, http.StatusOK, nil
		}
		return nil, http.StatusServiceUnavailable, errors.New("fake: relay draining")
	}
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP", ResumeToken: "ab"},
		Callbacks{OnResuming: func(int, error) { resuming <- struct{}{} }},
		Options{Clock: &FakeClock{}, Dial: dial, MediaFactory: media.factory(), Log: testLog, StatsInterval: time.Hour})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.closedByRelay(wire.CloseCodeServerDraining)
	waitSignal(t, resuming, 3*time.Second, "a reclaim attempt")

	mu.Lock()
	stopping = true
	mu.Unlock()
	if err := s.StopBroadcast(); err != nil {
		t.Fatal(err)
	}
	if !stopLeg.isClosed() || stopLeg.lastCloseCode() != webtransport.SessionErrorCode(wire.CloseCodeBroadcasterStopped) {
		t.Fatalf("stop leg closed=%v code=%d, want closed with 4014", stopLeg.isClosed(), stopLeg.lastCloseCode())
	}
	mu.Lock()
	last := urls[len(urls)-1]
	mu.Unlock()
	if !strings.Contains(last, "/publish/K7M2QP") || !strings.Contains(last, "resume=ab") {
		t.Errorf("stop reclaim dialled %q, want the broadcast's own token reclaim", last)
	}
}

// A StopBroadcast that lands after the relay deposed this engine (4004, a
// newer publisher reclaimed with the same token) must not reclaim to deliver
// its 4014: that reclaim would supersede the newer publisher and end the
// broadcast for every viewer (PR #688 review round 1). The supervisor is held
// inside OnError, after it has seen the 4004 and before it finishes, so the
// stop lands in exactly that window.
func TestStopBroadcastAfterASupersedeDoesNotReclaim(t *testing.T) {
	first := newFakeSession()
	media := newFakeMedia()
	var mu sync.Mutex
	dials := 0
	dial := func(ctx context.Context, rawURL, origin string, insecure bool) (RelaySession, int, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return first, http.StatusOK, nil
		}
		return newFakeSession(), http.StatusOK, nil
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP", ResumeToken: "ab"},
		Callbacks{OnError: func(error) {
			once.Do(func() { close(reached) })
			<-release
		}},
		Options{Clock: &FakeClock{}, Dial: dial, MediaFactory: media.factory(), Log: testLog, StatsInterval: time.Hour})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.closedByRelay(wire.CloseCodePublisherSuperseded)
	waitSignal(t, reached, 3*time.Second, "the supervisor seeing the 4004")

	done := make(chan struct{})
	go func() { _ = s.StopBroadcast(); close(done) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("StopBroadcast hung")
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Errorf("%d dials, want 1: a deposed engine's stop must not reclaim over the newer publisher", dials)
	}
}
