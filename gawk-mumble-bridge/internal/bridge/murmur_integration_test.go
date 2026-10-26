package bridge

// Integration tests against REAL murmur (docs/80 MV5 acceptance). They run
// when GAWK_TEST_MURMUR is set; testdata/murmur.sh starts the servers and
// prints the environment:
//
//	eval "$(testdata/murmur.sh up 1.5)"   # or 1.6
//	go test -race ./internal/bridge -run Murmur
//
// Without it they skip, so a plain `go test ./...` needs no docker.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const murmurToken = "gawk-test-token"

type murmurEnv struct {
	addr, strict, autoban, supw string
}

func needMurmur(t *testing.T) murmurEnv {
	t.Helper()
	e := murmurEnv{
		addr:    os.Getenv("GAWK_TEST_MURMUR"),
		strict:  os.Getenv("GAWK_TEST_MURMUR_STRICT"),
		autoban: os.Getenv("GAWK_TEST_MURMUR_AUTOBAN"),
		supw:    os.Getenv("GAWK_TEST_MURMUR_SUPERUSER_PASSWORD"),
	}
	if e.addr == "" {
		t.Skip("GAWK_TEST_MURMUR unset: see testdata/murmur.sh")
	}
	if e.supw == "" {
		t.Fatal("GAWK_TEST_MURMUR_SUPERUSER_PASSWORD is required with GAWK_TEST_MURMUR")
	}
	return e
}

func uniq(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func murmurDial(t *testing.T, addr, name, password string, mod ...func(*mumble.Config)) *mumble.Client {
	t.Helper()
	cfg := mumble.Config{Addr: addr, Username: name, Password: password, TLS: mumble.TLSOptions{InsecureSkipVerify: true}, AudioQueue: 512}
	for _, m := range mod {
		m(&cfg)
	}
	c, err := mumble.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial %s as %s: %v", addr, name, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// superuser logs in as SuperUser, creates a fresh parent channel with the
// docs/80 §5 ACL (#token may make temporary channels and enter) and returns
// it with the parent's name.
func superuser(t *testing.T, addr, supw string) (*mumble.Client, string, uint32) {
	t.Helper()
	su := murmurDial(t, addr, "SuperUser", supw)
	name := uniq("Gawk-")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := su.CreateChannel(ctx, 0, name, false)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if err := su.SetACL(id, []mumble.ACLEntry{{Group: "#" + murmurToken, ApplyHere: true, ApplySubs: true,
		Grant: mumble.PermMakeTempChannel | mumble.PermEnter | mumble.PermTraverse | mumble.PermSpeak}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = su.RemoveChannel(id); time.Sleep(50 * time.Millisecond) })
	time.Sleep(100 * time.Millisecond)
	return su, name, id
}

func murmurBridge(t *testing.T, relay *fakeRelay, addr, parent string, mod ...func(*Options)) *testBridge {
	t.Helper()
	opts := baseOptions(t, relay.addr, addr)
	opts.Mumble.AccessToken = murmurToken
	opts.Mumble.TempParent = parent
	opts.Timings.MumbleDial = 5 * time.Second
	opts.Timings.ObserverBackoff = 200 * time.Millisecond
	opts.Timings.ObserverBackoffMax = time.Second
	opts.Timings.TempChannelWait = time.Second
	for _, m := range mod {
		m(&opts)
	}
	return startBridge(t, opts)
}

func TestMurmurNameClashSuffix(t *testing.T) {
	e := needMurmur(t)
	su, _, _ := superuser(t, e.addr, e.supw)
	ctx := context.Background()
	static := uniq("Static-")
	sid, err := su.CreateChannel(ctx, 0, static, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = su.RemoveChannel(sid) })
	fr := startRelay(t)
	fr.onRegister = func(rs *relaySide) {
		rs.send(t, voice.Message{Type: voice.TypeAccepted, Code: rs.code, Channel: static})
	}
	tb := murmurBridge(t, fr, e.addr, "")
	a := joinVoice(t, tb, "static-room", "tuhis", 0)
	if j := fr.nextLink(t).next(t, voice.TypeJoined); j.Name != "tuhis [web]" {
		t.Fatalf("first name %q", j.Name)
	}
	a.waitState(connected)
	b := joinVoice(t, tb, "static-room", "tuhis", 0)
	if j := fr.nextLink(t).next(t, voice.TypeJoined); j.Name != "tuhis [web] 2" {
		t.Fatalf("second name %q", j.Name)
	}
	st := b.waitState(connected)
	if st.Name != "tuhis [web] 2" {
		t.Fatalf("browser name %q", st.Name)
	}
	// Both stay connected: murmur did not ghost the first one.
	time.Sleep(300 * time.Millisecond)
	ch, _ := su.FindPath([]string{static})
	names := map[string]bool{}
	for _, u := range su.UsersIn(ch) {
		names[u.Name] = true
	}
	if !names["tuhis [web]"] || !names["tuhis [web] 2"] {
		t.Fatalf("users in %s: %v", static, names)
	}
}

func TestMurmurInvalidUsernameFallback(t *testing.T) {
	e := needMurmur(t)
	if e.strict == "" {
		t.Skip("GAWK_TEST_MURMUR_STRICT unset")
	}
	_, parent, _ := superuser(t, e.strict, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.strict, parent)
	b := joinVoice(t, tb, uniq("room-"), "tuhis", 0)
	if j := fr.nextLink(t).next(t, voice.TypeJoined); j.Name != "tuhis_[web]" {
		t.Fatalf("sanitized name %q", j.Name)
	}
	if st := b.waitState(connected); st.Name != "tuhis_[web]" {
		t.Fatalf("browser name %q", st.Name)
	}
	b2 := joinVoice(t, tb, uniq("room-"), "tuhis", 0)
	if j := fr.nextLink(t).next(t, voice.TypeJoined); j.Name != "tuhis_[web]_2" {
		t.Fatalf("sanitized clash %q", j.Name)
	}
	b2.waitState(connected)
}

// The temporary channel is created under the parent by the observer, holds
// the bridged and native users, and is gone after the room ends (the
// observer link closed and everyone gone).
func TestMurmurTemporaryChannelLifecycle(t *testing.T) {
	e := needMurmur(t)
	su, parentName, parent := superuser(t, e.addr, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, parentName)
	code := uniq("room-")
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: code, DisplayCode: strings.ToUpper(code), Temporary: true, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	name := tb.tempChannel(code, strings.ToUpper(code))
	ch := suChild(t, su, parent, name)
	if c, _ := su.Channel(ch); !c.Temporary {
		t.Fatal("channel is not temporary")
	}
	b := joinVoice(t, tb, code, uniq("u"), 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	native := murmurDial(t, e.addr, uniq("native-"), "")
	if err := native.JoinChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	r := ol.waitRoster(t, func(m voice.Message) bool { return len(m.Users) == 2 })
	for _, u := range r.Users {
		if u.Name == "gawk" {
			t.Fatal("the observer lists itself")
		}
	}
	suInChannel(t, su, st.Session, ch)
	// The room ends: observer link closed, the browser leaves, the native
	// user leaves.
	_ = ol.sess.CloseWithError(0, "")
	_ = b.sess.CloseWithError(0, "")
	time.Sleep(300 * time.Millisecond)
	if _, ok := su.Channel(ch); !ok {
		t.Fatal("channel went while a native user is still in it")
	}
	native.Close()
	eventually(t, "temporary channel removed", func() bool { _, ok := su.Channel(ch); return !ok })
}

// D11: the channel name is the same from two bridge instances for one room
// code, and a home move mid-call (the observer link closed on instance A and
// re-opened on instance B) keeps everyone — a native user included — in the
// same channel.
func TestMurmurHomeMoveKeepsEveryoneInTheChannel(t *testing.T) {
	e := needMurmur(t)
	su, parentName, parent := superuser(t, e.addr, e.supw)
	fr := startRelay(t)
	podA := murmurBridge(t, fr, e.addr, parentName, func(o *Options) { o.StatsKey = []byte("pod-a") })
	podB := murmurBridge(t, fr, e.addr, parentName, func(o *Options) { o.StatsKey = []byte("pod-b") })
	code := uniq("room-")
	if podA.tempChannel(code, "X") != podB.tempChannel(code, "X") {
		t.Fatal("two instances name one room's channel differently")
	}
	olA, _, err := dialObserver(t, podA.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: code, Temporary: true, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	olA.waitRoster(t, func(m voice.Message) bool { return m.Available })
	ch := suChild(t, su, parent, podA.tempChannel(code, ""))
	b := joinVoice(t, podA, code, uniq("u"), 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	native := murmurDial(t, e.addr, uniq("native-"), "")
	if err := native.JoinChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	// The home moves: A's observer link closes, B's opens.
	_ = olA.sess.CloseWithError(0, "home moved")
	olB, _, err := dialObserver(t, podB.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: code, Temporary: true, Epoch: 2})
	if err != nil {
		t.Fatal(err)
	}
	olB.waitRoster(t, func(m voice.Message) bool { return m.Available && len(m.Users) == 2 })
	if got := suChild(t, su, parent, podB.tempChannel(code, "")); got != ch {
		t.Fatalf("channel after the move %d, want %d", got, ch)
	}
	for _, s := range []uint32{st.Session, native.Session()} {
		suInChannel(t, su, s, ch)
	}
	if native.Self().Channel != ch {
		t.Fatal("native user left the channel")
	}
}

func TestMurmurKickIs4015(t *testing.T) {
	e := needMurmur(t)
	su, parentName, _ := superuser(t, e.addr, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, parentName)
	b := joinVoice(t, tb, uniq("room-"), uniq("k"), 0)
	rs := fr.nextLink(t)
	st := b.waitState(connected)
	if err := su.Kick(st.Session, "behave", false); err != nil {
		t.Fatal(err)
	}
	if left := rs.next(t, voice.TypeLeft); left.CloseCode != wire.CloseCodeVoiceRemoved {
		t.Fatalf("left %+v", left)
	}
	b.expectClose(wire.CloseCodeVoiceRemoved)
}

func TestMurmurServerMuteReachesBrowser(t *testing.T) {
	e := needMurmur(t)
	su, parentName, _ := superuser(t, e.addr, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, parentName)
	b := joinVoice(t, tb, uniq("room-"), uniq("m"), 0)
	rs := fr.nextLink(t)
	st := b.waitState(connected)
	if err := su.SetServerMute(st.Session, true); err != nil {
		t.Fatal(err)
	}
	got := b.waitState(func(s wire.VoiceState) bool { return s.Flags&wire.VoiceStateFlagServerMute != 0 })
	if got.Flags&wire.VoiceStateFlagConnected == 0 {
		t.Fatalf("state %+v", got)
	}
	if s := rs.next(t, voice.TypeState); s.Flags&wire.RoomVoiceUserMute == 0 {
		t.Fatalf("relay state %+v", s)
	}
	_ = su.SetServerMute(st.Session, false)
	b.waitState(func(s wire.VoiceState) bool { return s.Flags&wire.VoiceStateFlagServerMute == 0 })
}

// D18: an autobanned bridge reports 4016 with its reason in /statusz.
func TestMurmurAutobanIs4016InStatusz(t *testing.T) {
	e := needMurmur(t)
	if e.autoban == "" {
		t.Skip("GAWK_TEST_MURMUR_AUTOBAN unset")
	}
	// Exhaust the threshold (3 a minute in murmur.sh); a ban left by an
	// earlier run only makes these fail sooner.
	for i := 0; i < 4; i++ {
		c, err := mumble.Dial(context.Background(), mumble.Config{Addr: e.autoban, Username: uniq("x"), TLS: mumble.TLSOptions{InsecureSkipVerify: true}})
		if err == nil {
			c.Close()
		}
	}
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.autoban, "")
	b := joinVoice(t, tb, uniq("room-"), "banned", 0)
	rs := fr.nextLink(t)
	if left := rs.next(t, voice.TypeLeft); left.CloseCode != wire.CloseCodeVoiceUnavailable || left.Reason != failureText[ReasonAutoban] {
		t.Fatalf("left %+v", left)
	}
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	_, body := tb.get(t, "/statusz")
	if !strings.Contains(body, `"autoban": 1`) || !strings.Contains(body, failureText[ReasonAutoban]) {
		t.Fatalf("statusz %s", body)
	}
	if _, m := tb.get(t, "/metrics"); !strings.Contains(m, `gawk_bridge_mumble_failures_total{reason="autoban"} 1`) {
		t.Fatal("metric")
	}
}

// Audio passes both ways byte-identical: browser → bridge → murmur → native
// client, and back, with sequence numbers and the end bit. The bridge pins
// murmur's certificate here, the way an operator would.
func TestMurmurAudioBothWaysByteIdentical(t *testing.T) {
	e := needMurmur(t)
	_, parentName, parent := superuser(t, e.addr, e.supw)
	conn, err := tls.Dial("tcp", e.addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	conn.Close()
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, parentName, func(o *Options) {
		o.Mumble.TLS = mumble.TLSOptions{CertSHA256: hex.EncodeToString(sum[:])}
	})
	code := uniq("room-")
	b := joinVoice(t, tb, code, uniq("a"), 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	ch, ok := murmurDial(t, e.addr, uniq("look-"), "").FindChild(parent, tb.tempChannel(code, ""))
	if !ok {
		t.Fatal("no channel")
	}
	native := murmurDial(t, e.addr, uniq("native-"), "")
	if err := native.JoinChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// browser → native
	frames := make([][]byte, 20)
	for i := range frames {
		frames[i] = []byte(fmt.Sprintf("opus-frame-%02d-\xfc\x00\xff", i))
	}
	for i, f := range frames {
		b.sendFrame(uint32(1000+i), f, i == len(frames)-1)
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := 0
	for got < len(frames) {
		p, err := native.Audio().Pop(ctx)
		if err != nil {
			t.Fatalf("native heard %d of %d frames", got, len(frames))
		}
		i := int(p.Seq) - 1000
		if i < 0 || i >= len(frames) || p.Session != st.Session || !bytes.Equal(p.Opus, frames[i]) || p.End != (i == len(frames)-1) {
			t.Fatalf("native got %+v", p)
		}
		got++
	}

	// native → browser
	for i, f := range frames {
		if _, err := native.WriteAudio(int64(500+i), f, i == len(frames)-1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got = 0
	deadline := time.After(5 * time.Second)
	for got < len(frames) {
		select {
		case d := <-b.frames:
			h, payload, err := wire.ParseVoiceFrame(d)
			if err != nil {
				t.Fatal(err)
			}
			i := int(h.Seq) - 500
			if i < 0 || i >= len(frames) || h.Speaker != native.Session() || !bytes.Equal(payload, frames[i]) || h.End != (i == len(frames)-1) || h.Target != 0 {
				t.Fatalf("browser got %+v %q", h, payload)
			}
			got++
		case <-deadline:
			t.Fatalf("browser heard %d of %d frames", got, len(frames))
		}
	}
}

// suChild waits for the SuperUser connection to see a channel. The observer
// reports Available once IT is in the channel, but the SuperUser is a
// separate connection, and murmur's ChannelState for the new channel may
// reach it a moment later (a CI flake, run 40292314503).
func suChild(t *testing.T, su *mumble.Client, parent uint32, name string) uint32 {
	t.Helper()
	var ch uint32
	eventually(t, "the SuperUser to see channel "+name, func() bool {
		var ok bool
		ch, ok = su.FindChild(parent, name)
		return ok
	})
	return ch
}

// suInChannel waits for the SuperUser connection to see a user in a channel,
// for the same reason.
func suInChannel(t *testing.T, su *mumble.Client, session, ch uint32) {
	t.Helper()
	eventually(t, fmt.Sprintf("the SuperUser to see user %d in channel %d", session, ch), func() bool {
		u, ok := su.User(session)
		return ok && u.Channel == ch
	})
}
