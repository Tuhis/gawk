package gateway

import (
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

// hashProxy is the Ingress of D10: it routes by the Authorization header
// (ingress-nginx upstream-hash-by "$http_authorization"). moveKey models a
// rescale or a rollout re-mapping one key; stateless OPTIONS go anywhere.
type hashProxy struct {
	mu       sync.Mutex
	targets  []*gw
	override map[string]int
	srv      *httptest.Server
	hits     []int
}

func newHashProxy(t *testing.T, targets ...*gw) *hashProxy {
	p := &hashProxy{targets: targets, override: map[string]int{}, hits: make([]int, len(targets))}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		i := p.route(r.In.Header.Get("Authorization"))
		u, _ := url.Parse(p.targets[i].public.URL)
		r.SetURL(u)
	}}
	p.srv = httptest.NewServer(rp)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *hashProxy) route(auth string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.override[auth]
	if !ok {
		h := fnv.New32a()
		h.Write([]byte(auth))
		i = int(h.Sum32() % uint32(len(p.targets)))
	}
	p.hits[i]++
	return i
}

func (p *hashProxy) pin(key streamkey.Key, i int) {
	p.mu.Lock()
	p.override["Bearer "+key.String()] = i
	p.mu.Unlock()
}

func (p *hashProxy) hitCounts() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.hits...)
}

func endWithBFrames(t *testing.T, g *gw, c *whipClient, key streamkey.Key) {
	t.Helper()
	c.sendGOP(2)
	c.sendAU(testmedia.B(500))
	eventually(t, "bframes end", 5*time.Second, func() bool {
		st := getStatus(t, g.public.URL, key)
		return st.RetryAfterMs > 0
	})
}

// WG5 D10: "a key's POST, DELETE and status reach the same instance".
func TestWG5D10KeyRequestsReachTheSameInstance(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, nil)
	p := newHashProxy(t, a, b)
	key := newKey(t)
	c := connect(t, p.srv.URL, key)
	home := p.route("Bearer " + key.String())
	waitLive(t, nil, c, func() status.Status { return getStatus(t, p.srv.URL, key) })
	if r := c.delete(); r.code != http.StatusOK {
		t.Fatalf("DELETE: %d", r.code)
	}
	hits := p.hitCounts()
	if hits[1-home] != 0 {
		t.Fatalf("requests for one key reached both instances: %v", hits)
	}
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code4014}) {
		t.Fatalf("close codes %v", got)
	}
}

// WG5 D10: "the same key's POST landing on the other instance (as after a
// rescale) reclaims the ID while the relay closes the first with 4004", and
// is exempt from that instance's cap.
func TestWG5D10PostOnTheOtherInstanceReclaimsAndSupersedes(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 2 })
	p := newHashProxy(t, a, b)
	key := newKey(t)
	p.pin(key, 0)
	c1 := connect(t, p.srv.URL, key)
	waitLive(t, a, c1, func() status.Status { return getStatus(t, p.srv.URL, key) })
	connect(t, b.public.URL, newKey(t)) // B at maxSessions
	p.pin(key, 1)                       // the rescale moves the key
	first := relay.cur(key.ID)
	c2 := connect(t, p.srv.URL, key) // exempt: live on A
	if first.remoteCode == nil || *first.remoteCode != webtransport.SessionErrorCode(wire.CloseCodePublisherSuperseded) {
		t.Fatal("relay did not close the first publisher with 4004")
	}
	eventually(t, "A torn down", 10*time.Second, func() bool { return sessions(a) == 0 })
	eventually(t, "A's client closed", 10*time.Second, func() bool { return c1.pc.ConnectionState() != webrtc.PeerConnectionStateConnected })
	waitLive(t, b, c2, func() status.Status { return getStatus(t, p.srv.URL, key) })
}

// WG5 D10: "with the hash moved away from the instance holding a live
// session, a DELETE on its Location reaching the other instance still ends
// the broadcast with 4014", and "/whip/status there still reports it live".
func TestWG5D10DeleteAndStatusReachTheSessionThroughThePeer(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, nil)
	p := newHashProxy(t, a, b)
	key := newKey(t)
	p.pin(key, 0)
	c := connect(t, p.srv.URL, key)
	waitLive(t, a, c, func() status.Status { return getStatus(t, p.srv.URL, key) })
	p.pin(key, 1)
	if st := getStatus(t, p.srv.URL, key); st.State != status.StateLive || st.Codec != "H.264" {
		t.Fatalf("status on B: %+v, want A's live session", st)
	}
	if r := c.delete(); r.code != http.StatusOK {
		t.Fatalf("DELETE through B: %d", r.code)
	}
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code4014}) {
		t.Fatalf("close codes %v, want [4014]", got)
	}
	if st := getStatus(t, p.srv.URL, key); st.State != status.StateEnded || st.LastError != nil {
		t.Fatalf("after DELETE: %+v", st)
	}
	if sessions(a) != 0 {
		t.Fatal("A still holds the session")
	}
}

// WG5 D10: "when the instance holding the key goes away (a scale-down),
// OBS's reconnect to the remaining one is admitted as a new key within its
// cap and reclaims the same ID".
func TestWG5D10ScaleDownReconnectIsANewKeyWithinTheCap(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 1 })
	p := newHashProxy(t, a, b)
	key := newKey(t)
	p.pin(key, 0)
	c := connect(t, p.srv.URL, key)
	waitLive(t, a, c, func() status.Status { return getStatus(t, p.srv.URL, key) })
	a.intl.Close() // A goes away: its peer listener first, then its sessions
	a.srv.Close()
	if got := relay.closeCodes(key.ID); !slices.Equal(got, []webtransport.SessionErrorCode{code0}) {
		t.Fatalf("A's shutdown closed with %v, want [0] (grace)", got)
	}
	p.pin(key, 1)
	c2 := connect(t, p.srv.URL, key)
	u, _ := url.Parse(relay.lastDial())
	if u.Path != "/publish/"+key.ID {
		t.Fatalf("reconnect dialed %q", relay.lastDial())
	}
	waitLive(t, b, c2, func() status.Status { return getStatus(t, p.srv.URL, key) })
	// And it counted against B's cap: another new key is refused.
	_, offer := newClientPC(t, clientOpts{})
	if r := do(t, http.MethodPost, b.public.URL+"/whip", newKey(t).String(), "application/sdp", offer, nil); r.code != http.StatusServiceUnavailable {
		t.Fatalf("B over its cap: %d", r.code)
	}
}

// WG5 D10: "the peer lookup is refused without internalToken".
func TestWG5D10PeerLookupNeedsTheInternalToken(t *testing.T) {
	a, _ := pair(t, newFakeRelay(), nil)
	key := newKey(t)
	path := a.intl.URL + keyPath(key.Hash)
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + testToken + "x", testToken} {
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("auth %q: %d, want 401", auth, r.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("with the token: %d", r.StatusCode)
	}
}

// WG5 D10: "At maxSessions, a new key gets 503 + Retry-After while the
// same key's reconnect succeeds … when its session is live on the other
// instance; at maxSessionsHard even that reconnect gets 503 when its session
// is on the other instance". The receiving instance also holds an old
// refusal for the key, which must not cost the reconnect its exemption.
func TestWG5D10CapExemptionForASessionLiveOnThePeer(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, func(o *Options) { o.MaxSessions, o.MaxSessionsHard = 1, 2 })
	key := newKey(t)
	_, hevc := newClientPC(t, clientOpts{videoMime: "video/H265", fmtp: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST"})
	if r := do(t, http.MethodPost, b.public.URL+"/whip", key.String(), "application/sdp", hevc, nil); r.code != 406 {
		t.Fatalf("old refusal: %d", r.code)
	}
	c := connect(t, a.public.URL, key)
	waitLive(t, a, c, func() status.Status { return a.status(key) })
	connect(t, b.public.URL, newKey(t)) // B at maxSessions
	_, offer := newClientPC(t, clientOpts{})
	r := do(t, http.MethodPost, b.public.URL+"/whip", newKey(t).String(), "application/sdp", offer, nil)
	if r.code != http.StatusServiceUnavailable || r.hdr.Get("Retry-After") != "30" {
		t.Fatalf("new key on full B: %d %q", r.code, r.hdr.Get("Retry-After"))
	}
	connect(t, b.public.URL, key) // exempt: live on A; B now at 2 = hard

	key2 := newKey(t)
	c2 := connect(t, a.public.URL, key2)
	waitLive(t, a, c2, func() status.Status { return a.status(key2) })
	_, offer2 := newClientPC(t, clientOpts{})
	r = do(t, http.MethodPost, b.public.URL+"/whip", key2.String(), "application/sdp", offer2, nil)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("exempt reconnect at maxSessionsHard: %d, want 503", r.code)
	}
}

// WG5 D10: "A key whose session ended on the other instance with a running
// D7 backoff gets 422 on this one, which then holds everything the other
// instance held for the key (the backoff, its lastError and the ended
// session), and the other instance holds nothing for it (a status read
// there is forwarded or idle); a Retry now (DELETE /whip/status) on this
// instance then lets the next POST through on either." WG7: "… the next GET
// is ended with no lastError, also after a D10 takeover".
func TestWG5D10TakeoverOfAPeersBackoff(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, nil)
	key := newKey(t)
	c := connect(t, a.public.URL, key)
	waitLive(t, a, c, func() status.Status { return a.status(key) })
	endWithBFrames(t, a, c, key)

	_, offer := newClientPC(t, clientOpts{})
	r := do(t, http.MethodPost, b.public.URL+"/whip", key.String(), "application/sdp", offer, nil)
	if r.code != http.StatusUnprocessableEntity || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("POST on B during A's backoff: %d", r.code)
	}
	rec, ok := b.srv.table.Get(key.Hash, b.clock.now())
	if !ok || !rec.BackoffRunning(b.clock.now()) || rec.LastError == nil || rec.LastError.Reason != status.ReasonBFrames ||
		rec.State != status.StateEnded || rec.Session == nil || rec.Session.Codec != "H.264" {
		t.Fatalf("B did not take everything over: %+v", rec)
	}
	if _, ok := a.srv.table.Get(key.Hash, a.clock.now()); ok {
		t.Fatal("A still holds the key's records after an acknowledged takeover")
	}
	if st := a.status(key); st.LastError == nil || st.LastError.Reason != status.ReasonBFrames || st.RetryAfterMs == 0 {
		t.Fatalf("status read on A (forwarded to B): %+v", st)
	}
	if r := do(t, http.MethodDelete, b.public.URL+"/whip/status", key.String(), "", "", nil); r.code != http.StatusNoContent {
		t.Fatalf("Retry now on B: %d", r.code)
	}
	for _, g := range []*gw{a, b} {
		if st := g.status(key); st.State != status.StateEnded || st.LastError != nil || st.RetryAfterMs != 0 {
			t.Fatalf("after Retry now: %+v, want ended without lastError", st)
		}
	}
	connect(t, a.public.URL, key) // the next POST, on either
}

// WG5 D10: "A takeover the receiving instance doesn't confirm leaves the
// other instance's records in place."
func TestWG5D10UnconfirmedTakeoverLeavesThePeersRecords(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, func(o *Options) { o.Hooks.SkipTakeoverAck = true })
	key := newKey(t)
	c := connect(t, a.public.URL, key)
	waitLive(t, a, c, func() status.Status { return a.status(key) })
	endWithBFrames(t, a, c, key)
	_, offer := newClientPC(t, clientOpts{})
	if r := do(t, http.MethodPost, b.public.URL+"/whip", key.String(), "application/sdp", offer, nil); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("POST on B: %d", r.code)
	}
	if rec, ok := a.srv.table.Get(key.Hash, a.clock.now()); !ok || !rec.BackoffRunning(a.clock.now()) {
		t.Fatal("A dropped its records without an acknowledgement")
	}
}

// WG5 D10: "A GET /whip/status reaching an instance without the key's
// records reports the other instance's lastError, and one reaching an
// instance that holds an old refusal reports the key's session live on the
// other instance."
func TestWG5D10StatusMergesAcrossInstances(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, nil)
	key := newKey(t)
	_, hevc := newClientPC(t, clientOpts{videoMime: "video/H265", fmtp: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST"})
	do(t, http.MethodPost, a.public.URL+"/whip", key.String(), "application/sdp", hevc, nil)
	if st := b.status(key); st.LastError == nil || st.LastError.Reason != status.ReasonCodec || st.LastError.Codec != status.CodecHEVC {
		t.Fatalf("B without records: %+v, want A's codec refusal", st)
	}
	// Now the key goes live on B, while A holds the old refusal.
	c := connect(t, b.public.URL, key)
	waitLive(t, b, c, func() status.Status { return b.status(key) })
	if st := a.status(key); st.State != status.StateLive || st.LastError != nil {
		t.Fatalf("A with an old refusal: %+v, want B's live session", st)
	}
}

// WG5 D10: "A POST reaching an instance that holds an old refusal still gets
// 422 from the other instance's running backoff … a Retry now on that
// instance clears the other instance's backoff."
func TestWG5D10OldRefusalDoesNotHideAPeersBackoff(t *testing.T) {
	relay := newFakeRelay()
	a, b := pair(t, relay, nil)
	key := newKey(t)
	_, hevc := newClientPC(t, clientOpts{videoMime: "video/H265", fmtp: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST"})
	do(t, http.MethodPost, b.public.URL+"/whip", key.String(), "application/sdp", hevc, nil) // B: old refusal
	c := connect(t, a.public.URL, key)
	waitLive(t, a, c, func() status.Status { return a.status(key) })
	endWithBFrames(t, a, c, key)

	// Retry now on B clears A's backoff (forwarded to the winner).
	if r := do(t, http.MethodDelete, b.public.URL+"/whip/status", key.String(), "", "", nil); r.code != http.StatusNoContent {
		t.Fatalf("Retry now on B: %d", r.code)
	}
	if rec, _ := a.srv.table.Get(key.Hash, a.clock.now()); rec.BackoffRunning(a.clock.now()) {
		t.Fatal("A's backoff survived a Retry now on B")
	}
	// A second end on A, and a POST on B gets A's 422.
	c2 := connect(t, a.public.URL, key)
	waitLive(t, a, c2, func() status.Status { return a.status(key) })
	endWithBFrames(t, a, c2, key)
	_, offer := newClientPC(t, clientOpts{})
	if r := do(t, http.MethodPost, b.public.URL+"/whip", key.String(), "application/sdp", offer, nil); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("POST on B with an old refusal during A's backoff: %d, want 422", r.code)
	}
}

// WG5 D10: "A peer that doesn't answer within 200 ms is treated as having
// nothing."
func TestWG5D10SlowPeerCountsAsHoldingNothing(t *testing.T) {
	relay := newFakeRelay()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		writeJSON(w, snapshot{Found: true, Backoff: true, Record: &status.Record{BackoffUntil: time.Now().Add(time.Hour)}})
	}))
	t.Cleanup(slow.Close)
	g := startGW(t, relay, func(o *Options) { o.InternalToken = testToken; o.Peers = []string{slow.URL} })
	key := newKey(t)
	start := time.Now()
	st := g.status(key)
	if took := time.Since(start); took > time.Second || st.State != status.StateIdle {
		t.Fatalf("status with a slow peer: %+v after %v", st, took)
	}
	start = time.Now()
	connect(t, g.public.URL, key) // not 422: the slow peer's backoff never arrived
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("POST took %v", took)
	}
}
