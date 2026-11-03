package gateway

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/publisher"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

// Review finding 1 (D8 row 2): OBS's Stop Streaming closes its
// PeerConnection (DTLS close_notify) first and only then sends the WHIP
// DELETE (WHIPOutput::StopThread). The DELETE must still find the session
// and end the broadcast with 4014, recording ended without lastError.
func TestWG5D8OBSStopClosesPeerConnectionThenDeletes(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	_ = c.pc.Close()
	time.Sleep(150 * time.Millisecond) // the close_notify lands first
	if r := c.delete(); r.code != http.StatusOK {
		t.Fatalf("DELETE after the close: %d, want 200", r.code)
	}
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code4014}) {
		t.Fatalf("relay close codes %v, want [4014]", got)
	}
	if st := g.status(key); st.State != status.StateEnded || st.LastError != nil {
		t.Fatalf("status %+v, want ended without lastError", st)
	}
}

// Review finding 1: a remote close with no DELETE is lost, after the grace.
func TestWG5D8RemoteCloseWithoutDeleteIsLostAfterTheGrace(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	_ = c.pc.Close()
	time.Sleep(300 * time.Millisecond)
	if sessions(g) != 1 {
		t.Fatal("the session ended before the DELETE grace ran out")
	}
	eventually(t, "lost", 5*time.Second, func() bool {
		st := g.status(key)
		return st.LastError != nil && st.LastError.Reason == status.ReasonLost
	})
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code0}) {
		t.Fatalf("relay close codes %v, want [0]", got)
	}
}

// Review finding 3: the engine refusing one delta must not cost the deltas
// after a keyframe that was pushed later in the same batch.
func TestWG6BackpressureDemandsAnIDROnlyUntilOneIsPushed(t *testing.T) {
	g := startGW(t, newFakeRelay(), nil)
	ss := newSession(g.srv, newKey(t), offer{}, "")
	var pushed []bool
	calls := 0
	ss.pushAU = func(au publisher.AccessUnit) bool {
		calls++
		if calls == 3 { // D1 below
			return false
		}
		pushed = append(pushed, au.Keyframe)
		return true
	}
	stream := testmedia.Stream{FPS: 60}
	sps := testmedia.SPS(testmedia.SPSOptions{})
	feed := func(pkts []*rtp.Packet) {
		for _, p := range pkts {
			ss.handle(input{kind: inVideo, pkt: p, at: time.Now()})
		}
	}
	feed(stream.Packets(testmedia.IDR(sps, 3000)))
	a := stream.Packets(testmedia.P(3000, 1)) // its middle packet arrives last
	feed(append([]*rtp.Packet{a[0]}, a[2:]...))
	feed(stream.Packets(testmedia.P(800, 2)))       // D1: the engine refuses it
	feed(stream.Packets(testmedia.IDR(sps, 3000)))  // K2
	feed(stream.Packets(testmedia.P(800, 3)))       // D3
	feed([]*rtp.Packet{a[1]})                       // releases A, D1, K2, D3 at once
	feed(stream.Packets(testmedia.P(800, 4)))       // D4
	want := []bool{true, false, true, false, false} // K0, A, K2, D3, D4
	if !slices.Equal(pushed, want) {
		t.Fatalf("pushed keyframe flags %v, want %v", pushed, want)
	}
}

// Review finding 5: the cap check and the reservation are one step; two
// new keys racing at maxSessions-1 must not both get in.
func TestWG5CapCheckAndReservationAreAtomic(t *testing.T) {
	var arrived atomic.Int32
	both := make(chan struct{})
	g := startGW(t, newFakeRelay(), func(o *Options) {
		o.MaxSessions, o.MaxSessionsHard = 1, 1
		o.Hooks.capChecked = func() {
			if arrived.Add(1) == 2 {
				close(both)
			}
			select {
			case <-both:
			case <-time.After(time.Second):
			}
		}
	})
	_, o1 := newClientPC(t, clientOpts{})
	_, o2 := newClientPC(t, clientOpts{})
	var wg sync.WaitGroup
	var created atomic.Int32
	for _, o := range []string{o1, o2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := do(t, http.MethodPost, g.public.URL+"/whip", newKey(t).String(), "application/sdp", o, nil)
			t.Logf("POST %d %s", r.code, strings.TrimSpace(r.body))
			if r.code == http.StatusCreated {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := created.Load(); n != 1 {
		t.Fatalf("%d sessions admitted at maxSessions 1", n)
	}
}

// Review finding 6: a POST that fails after the relay dial must not record
// an end for a session that never existed (wiping an earlier lastError).
func TestWG7FailedPostAfterDialRecordsNothing(t *testing.T) {
	g := startGW(t, newFakeRelay(), nil)
	key := newKey(t)
	_, hevc := newClientPC(t, clientOpts{videoMime: "video/H265", fmtp: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST"})
	if r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", hevc, nil); r.code != 406 {
		t.Fatalf("406 expected, got %d", r.code)
	}
	g.srv.mu.Lock()
	g.srv.closed = true // the registration step now fails, after the dial
	g.srv.mu.Unlock()
	_, offer := newClientPC(t, clientOpts{})
	if r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", offer, nil); r.code != http.StatusInternalServerError {
		t.Fatalf("POST: %d, want 500", r.code)
	}
	time.Sleep(500 * time.Millisecond)
	if st := g.status(key); st.State != status.StateIdle || st.LastError == nil || st.LastError.Reason != status.ReasonCodec {
		t.Fatalf("status %+v, want the earlier codec refusal untouched", st)
	}
}

// Review finding 7: an end racing goLive must not leave the record live.
func TestWG7EndRacingGoLiveNeverLeavesALiveRecord(t *testing.T) {
	g := startGW(t, newFakeRelay(), func(o *Options) {
		o.Hooks.beforeLiveRecord = func(ss *session) {
			done := make(chan struct{})
			go func() { ss.end(endCause{kind: endLost, reason: status.ReasonLost}); close(done) }()
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
			}
		}
	})
	key := newKey(t)
	c, offer := newClientPC(t, clientOpts{})
	if r := c.post(g.public.URL, key, offer); r.code != http.StatusCreated {
		t.Fatalf("POST %d", r.code)
	}
	eventually(t, "session gone", 10*time.Second, func() bool { return sessions(g) == 0 })
	time.Sleep(500 * time.Millisecond)
	rec, ok := g.srv.table.Get(key.Hash, g.clock.now())
	if !ok || rec.State != status.StateEnded || rec.LastError == nil || rec.LastError.Reason != status.ReasonLost {
		t.Fatalf("record %+v, want ended+lost", rec)
	}
}

// Review finding 8 (D5): only profiles and levels the viewers decode.
func TestWG5OfferProfileLevelID(t *testing.T) {
	sdp := func(fmtps ...string) []byte {
		var b strings.Builder
		b.WriteString("v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF")
		for i := range fmtps {
			b.WriteString(" " + string(rune('0'+i)) + "6")
		}
		b.WriteString("\r\nc=IN IP4 0.0.0.0\r\na=mid:0\r\n")
		for i, f := range fmtps {
			pt := string(rune('0'+i)) + "6"
			b.WriteString("a=rtpmap:" + pt + " H264/90000\r\na=fmtp:" + pt + " " + f + "\r\n")
		}
		return []byte(b.String())
	}
	cases := []struct {
		fmtps  []string
		wantPT int // 0: refused with codec
	}{
		{[]string{"packetization-mode=1;profile-level-id=42e01f"}, 6},
		{[]string{"packetization-mode=1;profile-level-id=4d0034"}, 6},
		{[]string{"packetization-mode=1;profile-level-id=640034"}, 6},
		{[]string{"packetization-mode=1"}, 6}, // RFC 6184's default: 42000a
		{[]string{"packetization-mode=1;profile-level-id=640035"}, 0},
		{[]string{"packetization-mode=1;profile-level-id=7a001f"}, 0},
		{[]string{"packetization-mode=1;profile-level-id=f4001f"}, 0},
		{[]string{"packetization-mode=1;profile-level-id=zz"}, 0},
		{[]string{"packetization-mode=1;profile-level-id=7a001f", "packetization-mode=1;profile-level-id=42e01f"}, 16},
	}
	for _, c := range cases {
		o, ref, err := inspectOffer(sdp(c.fmtps...))
		if err != nil {
			t.Fatalf("%v: %v", c.fmtps, err)
		}
		switch {
		case c.wantPT == 0 && (ref == nil || ref.reason != status.ReasonCodec):
			t.Errorf("%v: accepted (PT %d), want 406 codec", c.fmtps, o.video.PayloadType)
		case c.wantPT != 0 && (ref != nil || int(o.video.PayloadType) != c.wantPT):
			t.Errorf("%v: PT %d refusal %+v, want PT %d", c.fmtps, o.video.PayloadType, ref, c.wantPT)
		}
	}
}

// Review finding 9: status reads are rate-limited per client IP.
func TestWG7StatusIsRateLimitedPerClientIP(t *testing.T) {
	g := startGW(t, newFakeRelay(), func(o *Options) { o.StatusRate, o.StatusBurst = 2, 10 })
	key := newKey(t)
	var limited resp
	for i := 0; i < 15; i++ {
		method := http.MethodGet
		if i%2 == 1 {
			method = http.MethodDelete
		}
		if r := do(t, method, g.public.URL+"/whip/status", key.String(), "", "", nil); r.code == http.StatusTooManyRequests {
			limited = r
			break
		}
	}
	if limited.code != http.StatusTooManyRequests || limited.hdr.Get("Retry-After") == "" {
		t.Fatalf("15 status requests at once: no 429 + Retry-After (got %d)", limited.code)
	}
}

// PR #688 review round 1 finding 4: a DELETE for a session this pod doesn't
// hold asks every peer, so it is rate-limited per client IP like a status
// read. A DELETE for a local session is never limited: it is OBS's Stop.
func TestWG7UnknownSessionDeletesAreRateLimitedPerClientIP(t *testing.T) {
	a, _ := pair(t, newFakeRelay(), func(o *Options) { o.StatusRate, o.StatusBurst = 2, 10 })
	var limited resp
	for i := 0; i < 15; i++ {
		r := do(t, http.MethodDelete, a.public.URL+"/whip/session/nosuchsession", newKey(t).String(), "", "", nil)
		if r.code == http.StatusTooManyRequests {
			limited = r
			break
		}
	}
	if limited.code != http.StatusTooManyRequests || limited.hdr.Get("Retry-After") == "" {
		t.Fatalf("15 unknown-session DELETEs at once: no 429 + Retry-After (got %d)", limited.code)
	}
}
