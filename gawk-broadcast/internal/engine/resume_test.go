package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
func TestSupersededPublisherDoesNotResume(t *testing.T) {
	for _, tc := range []struct {
		name string
		code uint32
	}{
		{"superseded", wire.CloseCodePublisherSuperseded},
		{"broadcast ended", wire.CloseCodeBroadcastEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := newFakeSession()
			media := newFakeMedia()
			d := &recordingDialer{results: []dialResult{
				{first, http.StatusOK, nil},
				{newFakeSession(), http.StatusOK, nil}, // available, and must go unused
			}}

			ended := make(chan struct{}, 4)
			s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP"},
				Callbacks{OnEnded: func() { ended <- struct{}{} }},
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
		})
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
