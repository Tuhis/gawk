package gateway

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

const (
	code0    = webtransport.SessionErrorCode(0)
	code4014 = webtransport.SessionErrorCode(wire.CloseCodeBroadcasterStopped)
)

func waitLive(t *testing.T, g *gw, c *whipClient, keyStatus func() status.Status) {
	t.Helper()
	c.sendGOP(5)
	eventually(t, "live", 5*time.Second, func() bool { return keyStatus().State == status.StateLive })
}

func sessions(g *gw) int {
	g.srv.mu.Lock()
	defer g.srv.mu.Unlock()
	return len(g.srv.sessions)
}

// D8 row 2 (WG5, WG7): DELETE → StopBroadcast → 4014; the status reads
// ended with no lastError.
func TestWG5D8DeleteEndsBroadcastWith4014(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	if r := c.delete(); r.code != http.StatusOK {
		t.Fatalf("DELETE: %d", r.code)
	}
	// DELETE answers after the close is sent.
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code4014}) {
		t.Fatalf("relay close codes %v, want [4014]", got)
	}
	st := g.status(key)
	if st.State != status.StateEnded || st.LastError != nil || st.RetryAfterMs != 0 {
		t.Fatalf("after DELETE: %+v, want ended without lastError", st)
	}
	if st.Viewers != nil || st.LossPct != nil {
		t.Fatalf("an ended session reports live-only fields: %+v", st)
	}
	if r := c.delete(); r.code != http.StatusNotFound {
		t.Fatalf("second DELETE: %d, want 404", r.code)
	}
	if sessions(g) != 0 {
		t.Fatal("session not removed")
	}
}

// A DELETE with another key for the same Location doesn't end the session.
func TestWG5DeleteNeedsTheSessionsKey(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	other := otherTokenKey(t, key)
	if r := do(t, http.MethodDelete, g.public.URL+c.location, other.String(), "", "", nil); r.code != http.StatusNotFound {
		t.Fatalf("DELETE with another key: %d", r.code)
	}
	if sessions(g) != 1 {
		t.Fatal("session ended by a foreign key")
	}
}

// D8 row 3 (WG5, WG7): ICE/consent failure → Stop (code 0: grace) and lost.
func TestWG5D8ICEFailureIsLostAndLeavesBroadcastInGrace(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	c.conn.cut.Store(true) // OBS vanishes without a goodbye
	eventually(t, "lost", 10*time.Second, func() bool {
		st := g.status(key)
		return st.State == status.StateEnded && st.LastError != nil
	})
	st := g.status(key)
	if st.LastError.Reason != status.ReasonLost || st.RetryAfterMs != 0 {
		t.Fatalf("after ICE failure: %+v, want ended+lost, no backoff", st)
	}
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code0}) {
		t.Fatalf("relay close codes %v, want [0] (grace)", got)
	}
	// OBS's auto-reconnect reclaims the same ID.
	c2 := connect(t, g.public.URL, key)
	waitLive(t, g, c2, func() status.Status { return g.status(key) })
	if st := g.status(key); st.LastError != nil {
		t.Fatalf("live again but lastError %+v (WG7: going live clears an earlier lost)", st.LastError)
	}
}

// D8 row 4: a second OBS with the same key reclaims; the relay sends 4004
// to the first and the gateway tears its PeerConnection down. Not counted
// against maxSessions.
func TestWG5D8SecondOBSSameKeySupersedes(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 1 })
	key := newKey(t)
	c1 := connect(t, g.public.URL, key)
	waitLive(t, g, c1, func() status.Status { return g.status(key) })
	first := relay.cur(key.ID)
	c2 := connect(t, g.public.URL, key) // over the cap of 1, but exempt
	if first.remoteCode == nil || *first.remoteCode != webtransport.SessionErrorCode(wire.CloseCodePublisherSuperseded) {
		t.Fatal("the relay did not supersede the first publisher")
	}
	eventually(t, "first PeerConnection torn down", 10*time.Second, func() bool {
		s := c1.pc.ConnectionState()
		return s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateDisconnected
	})
	eventually(t, "one session", 3*time.Second, func() bool { return sessions(g) == 1 })
	waitLive(t, g, c2, func() status.Status { return g.status(key) })
	if st := g.status(key); st.LastError != nil {
		t.Fatalf("supersede recorded %+v", st.LastError)
	}
}

// D8 row 5 (WG5, WG7): at maxSessions a new key gets 503 + Retry-After: 30
// and no relay dial, recorded as gateway_full; the same key's reconnect
// succeeds.
func TestWG5D8AtMaxSessionsNewKey503SameKeyReconnects(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 1 })
	key := newKey(t)
	connect(t, g.public.URL, key)
	dials := relay.dialCount()
	other := newKey(t)
	_, offer := newClientPC(t, clientOpts{})
	r := do(t, http.MethodPost, g.public.URL+"/whip", other.String(), "application/sdp", offer, nil)
	if r.code != http.StatusServiceUnavailable || r.hdr.Get("Retry-After") != "30" {
		t.Fatalf("new key at the cap: %d Retry-After %q", r.code, r.hdr.Get("Retry-After"))
	}
	if relay.dialCount() != dials {
		t.Fatal("a 503 dialed the relay")
	}
	if st := g.status(other); st.LastError == nil || st.LastError.Reason != status.ReasonGatewayFull {
		t.Fatalf("503 not recorded: %+v", st)
	}
	connect(t, g.public.URL, key) // the same key: exempt
}

// D8 row 6 / WG6: a relay drain (4002, here the transport just going away)
// makes the engine resume on its own, and the gateway sends a PLI — on
// start and on resume only.
func TestWG6RelayDrainResumesAndPLIOnlyOnStartAndResume(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	if got := g.plis.list(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("PLIs %v, want [start]", got)
	}
	// Loss: a delta loses a packet for good. Drops never trigger a PLI.
	for i := 0; i < 3; i++ {
		pk := c.stream.Packets(testmedia.P(4000, byte(i)))
		for j, p := range pk {
			if j == 1 {
				continue
			}
			_ = c.video.WriteRTP(p)
		}
	}
	c.sendGOP(3)
	eventually(t, "NACK at the sender", 3*time.Second, func() bool { return c.nacks.Load() > 0 })
	eventually(t, "a window drop", 3*time.Second, func() bool {
		return testutilCounter(g.srv.m.framesDropped.WithLabelValues(dropReorderWindow)) > 0
	})
	relay.cur(key.ID).drop()
	eventually(t, "engine resumed", 5*time.Second, func() bool { return relay.dialCount() == 2 && relay.cur(key.ID) != nil })
	eventually(t, "resume PLI", 3*time.Second, func() bool { return len(g.plis.list()) == 2 })
	time.Sleep(1500 * time.Millisecond) // and nothing periodic
	if got := g.plis.list(); !slices.Equal(got, []string{"start", "resume"}) {
		t.Fatalf("PLIs %v, want [start resume]", got)
	}
	if n := c.plis.Load(); n != 2 {
		t.Fatalf("sender saw %d PLIs, want 2", n)
	}
	if st := g.status(key); st.State != status.StateLive {
		t.Fatalf("a drain reached the status: %+v", st)
	}
}

// D8 row 7 / WG6: the gateway ends a session itself (B-frames) with
// StopBroadcast (4014), records the reason and starts the backoff; the
// next POST gets 422 + Retry-After.
func TestWG6BFramesEndWith4014AndStartBackoff(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	c.sendAU(testmedia.B(800))
	eventually(t, "bframes end", 5*time.Second, func() bool {
		st := g.status(key)
		return st.State == status.StateEnded && st.LastError != nil
	})
	st := g.status(key)
	if st.LastError.Reason != status.ReasonBFrames || st.RetryAfterMs <= 29_000 || st.RetryAfterMs > 30_000 {
		t.Fatalf("after B-frames: %+v, want bframes with ~30 s retryAfterMs", st)
	}
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code4014}) {
		t.Fatalf("close codes %v, want [4014]", got)
	}
	_, offer := newClientPC(t, clientOpts{})
	r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", offer, nil)
	if r.code != http.StatusUnprocessableEntity || r.hdr.Get("Retry-After") != "30" {
		t.Fatalf("POST during backoff: %d Retry-After %q", r.code, r.hdr.Get("Retry-After"))
	}
	if st := g.status(key); st.LastError == nil || st.LastError.Reason != status.ReasonBFrames {
		t.Fatalf("a 422 changed lastError: %+v", st.LastError)
	}
}

// WG6: "GOP thresholds warn and end as specified (the session ends with
// 4014 through StopBroadcast()), the backoff runs 30 s → 60 s → … → 10 min
// on repeats and a B-frame refusal starts it too".
func TestWG6GOPEndsWith4014AndBackoffLadder(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)

	// A 2 s GOP warns.
	c := connect(t, g.public.URL, key)
	for i := 0; i < 7; i++ {
		c.sendAU(testmedia.IDR(c.sps, 2000))
		c.stream.TS += 2*90000 - 90000/60
	}
	eventually(t, "gop_long warning", 5*time.Second, func() bool {
		st := g.status(key)
		return len(st.Warnings) == 1 && st.Warnings[0].Reason == status.WarnGOPLong && st.Warnings[0].GOPMs == 2000
	})
	// A 3 s GOP ends it (RTP timestamps are what's measured, so the test
	// needn't wait 15 s of wall clock).
	for i := 0; i < 7; i++ {
		c.sendAU(testmedia.IDR(c.sps, 2000))
		c.stream.TS += 3*90000 - 90000/60
	}
	eventually(t, "gop_too_long", 5*time.Second, func() bool {
		st := g.status(key)
		return st.LastError != nil && st.LastError.Reason == status.ReasonGOPTooLong
	})
	st := g.status(key)
	if st.LastError.GOPMs != 3000 || st.State != status.StateEnded {
		t.Fatalf("gop end: %+v", st)
	}
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code4014}) {
		t.Fatalf("close codes %v, want [4014]", got)
	}

	// The ladder, with B-frame ends for the repeats.
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second,
		480 * time.Second, 600 * time.Second, 600 * time.Second}
	for i, d := range want {
		st := g.status(key)
		if got := time.Duration(st.RetryAfterMs) * time.Millisecond; got > d || got < d-2*time.Second {
			t.Fatalf("repeat %d: retryAfter %v, want %v", i, got, d)
		}
		if i == len(want)-1 {
			break
		}
		g.clock.advance(d + time.Second)
		c := connect(t, g.public.URL, key)
		c.sendGOP(2)
		c.sendAU(testmedia.B(500))
		eventually(t, "bframes end", 5*time.Second, func() bool {
			st := g.status(key)
			return st.State == status.StateEnded && st.LastError != nil && st.LastError.Reason == status.ReasonBFrames
		})
	}
}

// D8 row 8 (WG7): a relay 4006 (operator kill) ends the session, recorded
// as killed, no backoff.
func TestWG5D8OperatorKill4006IsKilled(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	relay.cur(key.ID).remoteClose(wire.CloseCodeTerminatedByOperator)
	eventually(t, "killed", 5*time.Second, func() bool {
		st := g.status(key)
		return st.State == status.StateEnded && st.LastError != nil && st.LastError.Reason == status.ReasonKilled
	})
	if st := g.status(key); st.RetryAfterMs != 0 {
		t.Fatal("a kill must not start D7's backoff")
	}
	eventually(t, "PeerConnection torn down", 10*time.Second, func() bool {
		s := c.pc.ConnectionState()
		return s != webrtc.PeerConnectionStateConnected
	})
	if relay.dialCount() != 1 {
		t.Fatal("4006 is terminal: no resume")
	}
}

// D8 rows 9–12 (WG5, WG7): relay refusals map onto OBS's answer and D9's
// lastError; no PeerConnection survives.
func TestWG5D8RelayRefusals(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	cases := []struct {
		relay, want int
		reason      string
		retryAfter  string
	}{
		{451, 451, status.ReasonBanned, ""},
		{409, 409, status.ReasonReserved, ""},
		{429, 429, status.ReasonRelayBusy, "30"},
		{401, 401, status.ReasonRejected, ""},
		{403, 403, status.ReasonRejected, ""},
		{500, 503, status.ReasonRelayBusy, "30"},
	}
	for _, c := range cases {
		key := newKey(t)
		relay.setRefuse(key.ID, c.relay)
		_, offer := newClientPC(t, clientOpts{})
		r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", offer, nil)
		if r.code != c.want || r.hdr.Get("Retry-After") != c.retryAfter {
			t.Errorf("relay %d: answered %d Retry-After %q, want %d %q", c.relay, r.code, r.hdr.Get("Retry-After"), c.want, c.retryAfter)
		}
		if st := g.status(key); st.LastError == nil || st.LastError.Reason != c.reason || st.State != status.StateIdle {
			t.Errorf("relay %d: status %+v, want idle with %s", c.relay, st, c.reason)
		}
	}
	if sessions(g) != 0 {
		t.Fatal("a refused publish left a session")
	}
}

func testutilCounter(c prometheus.Collector) float64 { return testutil.ToFloat64(c) }
