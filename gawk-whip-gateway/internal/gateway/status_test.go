package gateway

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

// WG7: "returns the D9 fields for the key's session, including viewers
// from the engine's viewer count" — camelCase, warnings always present.
func TestWG7StatusFieldsForALiveSession(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	relay.cur(key.ID).dgrams <- wire.AppendViewerCount(nil, 3)
	stop := make(chan struct{})
	go func() { // a 1 s GOP at 60 fps, paced
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%60 == 0 {
				c.sendAU(testmedia.IDR(c.sps, 3000))
			} else {
				c.sendAU(testmedia.P(1000, byte(i)))
			}
			time.Sleep(time.Second / 60)
		}
	}()
	defer close(stop)
	eventually(t, "full status", 8*time.Second, func() bool {
		st := g.status(key)
		return st.State == status.StateLive && st.Viewers != nil && *st.Viewers == 3 && st.FPS > 30 &&
			st.BitrateKbps > 0 && st.LossPct != nil && st.GOPMs == 1000 && st.Resolution != nil
	})
	r := do(t, http.MethodGet, g.public.URL+"/whip/status", key.String(), "", "", nil)
	for _, field := range []string{`"state":"live"`, `"codec":"H.264"`, `"resolution":{"width":1280,"height":720}`,
		`"fps":`, `"gopMs":1000`, `"bitrateKbps":`, `"lossPct":0`, `"viewers":3`, `"warnings":[]`} {
		if !strings.Contains(r.body, field) {
			t.Errorf("status %s lacks %s", r.body, field)
		}
	}
	for _, absent := range []string{"lastError", "retryAfterMs"} {
		if strings.Contains(r.body, absent) {
			t.Errorf("status %s carries %s", r.body, absent)
		}
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Error("status must not be cached")
	}
}

// WG7: "a malformed key gets 401 and an unknown well-formed key gets idle".
func TestWG7MalformedKey401UnknownKeyIdle(t *testing.T) {
	g := startGW(t, newFakeRelay(), nil)
	if r := do(t, http.MethodGet, g.public.URL+"/whip/status", "gawk1_nope", "", "", nil); r.code != http.StatusUnauthorized {
		t.Fatalf("malformed: %d", r.code)
	}
	r := do(t, http.MethodGet, g.public.URL+"/whip/status", newKey(t).String(), "", "", nil)
	if r.code != http.StatusOK || strings.TrimSpace(r.body) != `{"state":"idle","warnings":[]}` {
		t.Fatalf("unknown key: %d %s", r.code, r.body)
	}
	if r := do(t, http.MethodDelete, g.public.URL+"/whip/status", newKey(t).String(), "", "", nil); r.code != http.StatusNoContent {
		t.Fatalf("DELETE /whip/status on an unknown key: %d", r.code)
	}
}

// WG7: "reports a 503/400/406/451 refusal of a POST that never made a
// session as lastError", and "a different key with the same ID neither sees
// nor sets that record".
func TestWG7RefusalsBeforeASessionAreRecordedPerKey(t *testing.T) {
	relay := newFakeRelay()
	full := startGW(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 1 })
	connect(t, full.public.URL, newKey(t))
	proxied := startGW(t, relay, func(o *Options) { o.TrustedProxyCIDRs = []string{"127.0.0.0/8"} })
	g := startGW(t, relay, nil)
	_, offer := newClientPC(t, clientOpts{})
	_, hevc := newClientPC(t, clientOpts{videoMime: "video/H265", fmtp: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST"})

	k503, k400, k406, k451 := newKey(t), newKey(t), newKey(t), newKey(t)
	relay.setRefuse(k451.ID, http.StatusUnavailableForLegalReasons)
	steps := []struct {
		g      *gw
		key    string
		offer  string
		want   int
		reason string
		check  *gw
	}{
		{full, k503.String(), offer, 503, status.ReasonGatewayFull, full},
		{proxied, k400.String(), offer, 400, status.ReasonClientIPUnknown, proxied},
		{g, k406.String(), hevc, 406, status.ReasonCodec, g},
		{g, k451.String(), offer, 451, status.ReasonBanned, g},
	}
	keys := map[string]string{k503.String(): k503.ID, k400.String(): k400.ID, k406.String(): k406.ID, k451.String(): k451.ID}
	for _, s := range steps {
		r := do(t, http.MethodPost, s.g.public.URL+"/whip", s.key, "application/sdp", s.offer, nil)
		if r.code != s.want {
			t.Fatalf("%s: POST %d, want %d", s.reason, r.code, s.want)
		}
		_ = keys
	}
	for _, s := range []struct {
		g      *gw
		k      func() string
		reason string
	}{
		{full, k503.String, status.ReasonGatewayFull},
		{proxied, k400.String, status.ReasonClientIPUnknown},
		{g, k406.String, status.ReasonCodec},
		{g, k451.String, status.ReasonBanned},
	} {
		r := do(t, http.MethodGet, s.g.public.URL+"/whip/status", s.k(), "", "", nil)
		st := decodeStatus(t, r.body)
		if st.LastError == nil || st.LastError.Reason != s.reason {
			t.Errorf("%s: status %s", s.reason, r.body)
		}
	}
	// Same ID, another token: its own (empty) record, and its refusal
	// doesn't touch the first key's.
	other := otherTokenKey(t, k406)
	if st := g.status(other); st.LastError != nil || st.State != status.StateIdle {
		t.Fatalf("a key with the same ID sees %+v", st)
	}
	r := do(t, http.MethodPost, g.public.URL+"/whip", other.String(), "application/sdp", offer, nil)
	if r.code != http.StatusCreated {
		t.Fatalf("other key POST: %d", r.code)
	}
	if st := g.status(k406); st.LastError == nil || st.LastError.Reason != status.ReasonCodec || st.State != status.StateIdle {
		t.Fatalf("the first key's record changed: %+v", st)
	}
}

// WG7: "a session that goes live clears an earlier lastError (a 503
// refusal, an earlier lost)" — lost is in the ICE-failure test.
func TestWG7GoingLiveClearsAnEarlierRefusal(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 1 })
	first := newKey(t)
	c1 := connect(t, g.public.URL, first)
	key := newKey(t)
	_, offer := newClientPC(t, clientOpts{})
	if r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", offer, nil); r.code != 503 {
		t.Fatalf("POST %d", r.code)
	}
	if st := g.status(key); st.LastError == nil || st.LastError.Reason != status.ReasonGatewayFull {
		t.Fatalf("%+v", st)
	}
	c1.delete()
	c := connect(t, g.public.URL, key)
	waitLive(t, g, c, func() status.Status { return g.status(key) })
	if st := g.status(key); st.LastError != nil {
		t.Fatalf("live but lastError %+v", st.LastError)
	}
}

// WG7: "a 422'd POST during a backoff leaves lastError as the backoff's own
// reason"; WG5: "During a backoff, a POST that would also be over the cap
// or refused 406 gets 422 with the backoff's reason"; then "DELETE clears
// the backoff and its lastError together, so the next POST is answered and
// the next GET is ended with no lastError"; retryAfterMs only while the
// backoff runs.
func TestWG7BackoffRetryNowAndRetryAfter(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 1 })
	key := newKey(t)
	c := connect(t, g.public.URL, key)
	c.sendGOP(2)
	c.sendAU(testmedia.B(500))
	eventually(t, "backoff", 5*time.Second, func() bool { st := g.status(key); return st.RetryAfterMs > 0 })

	connect(t, g.public.URL, newKey(t)) // the pod is now full
	_, offer := newClientPC(t, clientOpts{})
	_, hevc := newClientPC(t, clientOpts{videoMime: "video/H265", fmtp: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST"})
	for _, o := range []string{offer, hevc, offer} {
		r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", o, nil)
		if r.code != http.StatusUnprocessableEntity {
			t.Fatalf("POST during backoff (over the cap / HEVC): %d, want 422", r.code)
		}
		if st := g.status(key); st.LastError == nil || st.LastError.Reason != status.ReasonBFrames {
			t.Fatalf("422 replaced lastError: %+v", st.LastError)
		}
	}
	if r := do(t, http.MethodDelete, g.public.URL+"/whip/status", key.String(), "", "", nil); r.code != http.StatusNoContent {
		t.Fatalf("Retry now: %d", r.code)
	}
	st := g.status(key)
	if st.State != status.StateEnded || st.LastError != nil || st.RetryAfterMs != 0 {
		t.Fatalf("after Retry now: %+v, want ended, no lastError, no retryAfterMs", st)
	}
	// Answered again (the pod is full, so: 503, not 422 — the backoff is gone).
	if r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", offer, nil); r.code != http.StatusServiceUnavailable {
		t.Fatalf("POST after Retry now: %d, want an answer past the backoff (503 at the cap)", r.code)
	}

	// retryAfterMs disappears when the backoff runs out on its own.
	key2 := newKey(t)
	g2 := startGW(t, relay, nil)
	c2 := connect(t, g2.public.URL, key2)
	c2.sendGOP(2)
	c2.sendAU(testmedia.B(500))
	eventually(t, "backoff", 5*time.Second, func() bool { return g2.status(key2).RetryAfterMs > 0 })
	g2.clock.advance(31 * time.Second)
	if st := g2.status(key2); st.RetryAfterMs != 0 || st.LastError == nil || st.LastError.Reason != status.ReasonBFrames {
		t.Fatalf("after the backoff: %+v, want the reason without retryAfterMs", st)
	}
}

// WG7: "the table evicts beyond its bound" through the gateway.
func TestWG7RecordTableEvictsBeyondItsBound(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, func(o *Options) { o.MaxRecords = 3; o.MaxSessions, o.MaxSessionsHard = 1, 1 })
	connect(t, g.public.URL, newKey(t))
	_, offer := newClientPC(t, clientOpts{})
	var keys []string
	for i := 0; i < 5; i++ {
		k := newKey(t)
		keys = append(keys, k.String())
		do(t, http.MethodPost, g.public.URL+"/whip", k.String(), "application/sdp", offer, nil)
	}
	if n := g.srv.table.Len(); n > 3 {
		t.Fatalf("table holds %d records, bound 3", n)
	}
	first := do(t, http.MethodGet, g.public.URL+"/whip/status", keys[0], "", "", nil)
	if !strings.Contains(first.body, `"state":"idle"`) || strings.Contains(first.body, "lastError") {
		t.Fatalf("oldest record not evicted: %s", first.body)
	}
}

// WG7: "every warning and refusal carries a reason from the fixed set";
// WG7 "nothing in a response contains an IP" — across the refusal and
// status paths, with a proxied client address.
func TestWG7ReasonsFixedSetAndNoIPsInResponses(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, func(o *Options) {
		o.TrustedProxyCIDRs = []string{"127.0.0.1/32"}
		o.MaxSessions, o.MaxSessionsHard = 2, 2
	})
	xff := http.Header{"X-Forwarded-For": {"198.51.100.23"}}
	_, offer := newClientPC(t, clientOpts{})
	_, av1 := newClientPC(t, clientOpts{videoMime: "video/AV1", fmtp: "level-idx=5;profile=0;tier=0"})
	var bodies []resp
	key := newKey(t)
	c, off := newClientPC(t, clientOpts{})
	r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", off, xff)
	bodies = append(bodies, r)
	_ = c.pc.SetRemoteDescription(sdpAnswer(r.body))
	for _, k := range []struct{ key, offer string }{{newKey(t).String(), av1}, {newKey(t).String(), offer}, {newKey(t).String(), offer}} {
		bodies = append(bodies, do(t, http.MethodPost, g.public.URL+"/whip", k.key, "application/sdp", k.offer, xff))
		bodies = append(bodies, do(t, http.MethodGet, g.public.URL+"/whip/status", k.key, "", "", xff))
	}
	noXFF := do(t, http.MethodPost, g.public.URL+"/whip", newKey(t).String(), "application/sdp", offer, nil)
	if noXFF.code != http.StatusBadRequest {
		t.Fatalf("trusted peer without XFF: %d, want 400", noXFF.code)
	}
	bodies = append(bodies, noXFF, do(t, http.MethodGet, g.public.URL+"/whip/status", key.String(), "", "", xff))
	for _, b := range bodies {
		all := b.body
		for k, v := range b.hdr {
			all += k + ":" + strings.Join(v, ",")
		}
		if strings.Contains(all, "198.51.100") {
			t.Fatalf("a response carries the client's IP: %q", all)
		}
		// An SDP answer carries the gateway's own candidates (its public
		// address); nothing else may carry any address at all.
		if b.hdr.Get("Content-Type") != "application/sdp" && (strings.Contains(all, "127.0.0.1") || strings.Contains(all, "::1")) {
			t.Fatalf("a response carries an IP: %q", all)
		}
		if strings.HasPrefix(strings.TrimSpace(b.body), "{") {
			st := decodeStatus(t, b.body)
			if st.LastError != nil && !slices.Contains(status.ErrorReasons, st.LastError.Reason) {
				t.Fatalf("lastError reason %q outside the fixed set", st.LastError.Reason)
			}
			for _, w := range st.Warnings {
				if !slices.Contains(status.WarningReasons, w.Reason) {
					t.Fatalf("warning reason %q outside the fixed set", w.Reason)
				}
			}
		}
	}
}
