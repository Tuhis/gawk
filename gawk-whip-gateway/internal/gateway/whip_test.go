package gateway

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/Tuhis/gawk/gawk-server/publisher"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

// WG5: "H.264+Opus accepted (201 + Location)" — a pion WHIP client, ICE
// over the pod's UDP mux, media through the adapter into the engine. Also
// D2/D4's contract: the publish is a reclaim of the key's ID with its token
// and the secret, carrying the proxy token and the client's address.
func TestWG5AcceptsH264AndOpus(t *testing.T) {
	relay := newFakeRelay()
	var cfgs []publisher.Config
	g := startGW(t, relay, func(o *Options) {
		o.Hooks.EngineConfig = func(c publisher.Config) { cfgs = append(cfgs, c) }
	})
	key := newKey(t)
	c, offer := newClientPC(t, clientOpts{})
	r := c.post(g.public.URL, key, offer)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	if ct := r.hdr.Get("Content-Type"); ct != "application/sdp" {
		t.Fatalf("Content-Type %q", ct)
	}
	if !regexp.MustCompile(`^/whip/session/[0-9a-f]{32}$`).MatchString(c.location) {
		t.Fatalf("Location %q", c.location)
	}
	if !strings.Contains(r.body, "a=ice-lite") {
		t.Fatal("answer is not ICE-lite")
	}
	u, _ := url.Parse(relay.lastDial())
	if u.Path != "/publish/"+key.ID || u.Query().Get("resume") != key.Token || u.Query().Get("secret") != "s3cret" {
		t.Fatalf("publish dial %q is not D2's reclaim", relay.lastDial())
	}
	if len(cfgs) != 1 {
		t.Fatalf("engine configs: %d", len(cfgs))
	}
	if h := cfgs[0].Header; h.Get("Authorization") != "Bearer proxy-token" || h.Get("Gawk-Publisher-Ip") != "127.0.0.1" {
		t.Fatalf("D4 headers: %v", h)
	}
	c.waitConnected()
	c.sendGOP(10)
	c.sendAudio(20)
	eventually(t, "frames at the relay", 5*time.Second, func() bool {
		s := relay.cur(key.ID)
		if s == nil {
			return false
		}
		dg, kf := s.counts()
		return kf >= 1 && dg >= 10
	})
	eventually(t, "status live", 3*time.Second, func() bool { return g.status(key).State == status.StateLive })
	eventually(t, "status measured", 3*time.Second, func() bool {
		st := g.status(key)
		return st.Resolution != nil && st.Resolution.Width == 1280 && st.Resolution.Height == 720 && st.Codec == "H.264"
	})
	if c.plis.Load() != 1 {
		t.Fatalf("PLIs at session start: %d, want 1", c.plis.Load())
	}
}

func mungeOffer(t *testing.T, offer string, fn func(video string) string) string {
	t.Helper()
	i := strings.Index(offer, "m=video")
	if i < 0 {
		t.Fatal("no video section")
	}
	j := strings.Index(offer[i+1:], "\nm=")
	end := len(offer)
	if j >= 0 {
		end = i + 1 + j + 1
	}
	return offer[:i] + fn(offer[i:end]) + offer[end:]
}

// WG5: "HEVC, AV1 and simulcast refused 406" (D5), recorded for D9 with
// codec/simulcast and the codec's name, with no relay dial.
func TestWG5RefusesHEVCAV1AndSimulcast406(t *testing.T) {
	relay := newFakeRelay()
	g := startGW(t, relay, nil)
	_, h264 := newClientPC(t, clientOpts{})
	_, hevc := newClientPC(t, clientOpts{videoMime: webrtc.MimeTypeH265, fmtp: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST"})
	_, av1 := newClientPC(t, clientOpts{videoMime: webrtc.MimeTypeAV1, fmtp: "level-idx=5;profile=0;tier=0"})
	ridSimulcast := mungeOffer(t, h264, func(v string) string {
		return strings.TrimRight(v, "\r\n") + "\r\na=rid:h send\r\na=rid:l send\r\na=simulcast:send h;l\r\n"
	})
	twoVideo := mungeOffer(t, h264, func(v string) string { return v + strings.Replace(v, "a=mid:0", "a=mid:9", 1) })
	pm0 := strings.ReplaceAll(h264, "packetization-mode=1", "packetization-mode=0")
	cases := []struct {
		name, offer, reason, codec string
	}{
		{"HEVC", hevc, status.ReasonCodec, status.CodecHEVC},
		{"AV1", av1, status.ReasonCodec, status.CodecAV1},
		{"simulcast RIDs", ridSimulcast, status.ReasonSimulcast, ""},
		{"two video layers", twoVideo, status.ReasonSimulcast, ""},
		{"packetization-mode 0", pm0, status.ReasonCodec, ""},
	}
	for _, c := range cases {
		key := newKey(t)
		r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", c.offer, nil)
		if r.code != http.StatusNotAcceptable {
			t.Errorf("%s: %d %s, want 406", c.name, r.code, r.body)
			continue
		}
		st := g.status(key)
		if st.LastError == nil || st.LastError.Reason != c.reason || st.LastError.Codec != c.codec {
			t.Errorf("%s: lastError %+v, want %s/%s", c.name, st.LastError, c.reason, c.codec)
		}
	}
	if n := relay.dialCount(); n != 0 {
		t.Fatalf("a refused offer dialed the relay %d times", n)
	}
}

// WG5: "bad key 401" (D2), for POST, DELETE and status alike.
func TestWG5BadKey401(t *testing.T) {
	g := startGW(t, newFakeRelay(), nil)
	_, offer := newClientPC(t, clientOpts{})
	for _, bad := range []string{
		"", "gawk1_", "gawk2_K7Q2XM_0123456789abcdef0123456789abcdef",
		"gawk1_K7Q2X_0123456789abcdef0123456789abcdef",   // short ID
		"gawk1_K7Q2X0_0123456789abcdef0123456789abcdef",  // 0 isn't in the alphabet
		"gawk1_K7Q2XM_0123456789ABCDEF0123456789ABCDEF",  // uppercase token
		"gawk1_K7Q2XM_0123456789abcdef0123456789abcde",   // 31 hex
		"gawk1_K7Q2XM_0123456789abcdef0123456789abcdef0", // 33 hex
		"gawk1_K7Q2XM_0123456789abcdef0123456789abcdeg",  // not hex
		"gawk1_K7Q2XM-0123456789abcdef0123456789abcdef",
	} {
		for _, m := range []struct{ method, path, body string }{
			{http.MethodPost, "/whip", offer},
			{http.MethodDelete, "/whip/session/00", ""},
			{http.MethodGet, "/whip/status", ""},
			{http.MethodDelete, "/whip/status", ""},
		} {
			r := do(t, m.method, g.public.URL+m.path, bad, "application/sdp", m.body, nil)
			if r.code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: %d, want 401", m.method, m.path, bad, r.code)
			}
		}
	}
	// A lowercase code is the relay's own normalisation, not a bad key.
	if r := do(t, http.MethodGet, g.public.URL+"/whip/status", "gawk1_k7q2xm_0123456789abcdef0123456789abcdef", "", "", nil); r.code != http.StatusOK {
		t.Fatalf("lowercase ID: %d", r.code)
	}
}

// WG5 / D10: "Each pod answers with a candidate on its own basePort +
// ordinal", UDP and ICE-TCP, on the shared public address.
func TestWG5EachPodAnswersWithCandidateOnItsOwnPort(t *testing.T) {
	relay := newFakeRelay()
	base := freeUDPTCPPort(t)
	var answers []string
	for ordinal := 0; ordinal < 2; ordinal++ {
		g := startGW(t, relay, func(o *Options) {
			o.MediaPort = base + ordinal
			o.MediaBind = "127.0.0.1"
			o.PublicAddress = "203.0.113.10"
		})
		if g.srv.MediaPort() != base+ordinal {
			t.Fatalf("pod %d bound %d", ordinal, g.srv.MediaPort())
		}
		c, offer := newClientPC(t, clientOpts{})
		r := c.post(g.public.URL, newKey(t), offer)
		if r.code != http.StatusCreated {
			t.Fatalf("POST: %d", r.code)
		}
		answers = append(answers, r.body)
	}
	cand := regexp.MustCompile(`a=candidate:\S+ \d+ (udp|tcp) \d+ (\S+) (\d+) typ host( tcptype (\w+))?`)
	for ordinal, ans := range answers {
		found := map[string]bool{}
		for _, m := range cand.FindAllStringSubmatch(ans, -1) {
			port, _ := strconv.Atoi(m[3])
			if m[2] != "203.0.113.10" || port != base+ordinal {
				t.Errorf("pod %d candidate %q: want 203.0.113.10:%d", ordinal, m[0], base+ordinal)
			}
			if m[1] == "tcp" && m[5] != "passive" {
				t.Errorf("pod %d TCP candidate not passive: %q", ordinal, m[0])
			}
			found[m[1]] = true
		}
		if !found["udp"] || !found["tcp"] {
			t.Errorf("pod %d: candidates %v, want UDP and ICE-TCP", ordinal, found)
		}
	}
}

// OPTIONS for both WHIP routes and the status route, with CORS for an
// allowed origin, Authorization among the allowed headers (D9).
func TestWG5OptionsAndCORS(t *testing.T) {
	g := startGW(t, newFakeRelay(), func(o *Options) { o.AllowedOrigins = []string{"https://gawk.example"} })
	for _, path := range []string{"/whip", "/whip/session/abc", "/whip/status"} {
		r := do(t, http.MethodOptions, g.public.URL+path, "", "", "", http.Header{
			"Origin": {"https://gawk.example"}, "Access-Control-Request-Method": {"GET"},
			"Access-Control-Request-Headers": {"authorization"},
		})
		if r.code != http.StatusNoContent {
			t.Fatalf("OPTIONS %s: %d", path, r.code)
		}
		if r.hdr.Get("Access-Control-Allow-Origin") != "https://gawk.example" ||
			!strings.Contains(r.hdr.Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Fatalf("OPTIONS %s CORS: %v", path, r.hdr)
		}
		bad := do(t, http.MethodOptions, g.public.URL+path, "", "", "", http.Header{"Origin": {"https://evil.example"}})
		if bad.hdr.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("OPTIONS %s allowed a foreign origin", path)
		}
	}
	key := newKey(t)
	r := do(t, http.MethodGet, g.public.URL+"/whip/status", key.String(), "", "", http.Header{"Origin": {"https://gawk.example"}})
	if r.code != http.StatusOK || r.hdr.Get("Access-Control-Allow-Origin") != "https://gawk.example" {
		t.Fatalf("GET /whip/status CORS: %d %v", r.code, r.hdr)
	}
	if r := do(t, http.MethodPatch, g.public.URL+"/whip/session/abc", key.String(), "application/trickle-ice-sdpfrag", "", nil); r.code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH (trickle) %d, want 405", r.code)
	}
}

// freeUDPTCPPort finds a port free for both UDP and TCP, and the next one.
func freeUDPTCPPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		srv, err := New(Options{MaxSessions: 1, Backoff: status.BackoffPolicy{Initial: time.Second, Max: time.Second}, MediaBind: "127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		p := srv.MediaPort()
		srv.Close()
		if p+1 > 65535 {
			continue
		}
		probe, err := New(Options{MaxSessions: 1, Backoff: status.BackoffPolicy{Initial: time.Second, Max: time.Second}, MediaBind: "127.0.0.1", MediaPort: p + 1})
		if err == nil {
			probe.Close()
			return p
		}
	}
	t.Fatal("no free port pair")
	return 0
}

var _ = testmedia.P
