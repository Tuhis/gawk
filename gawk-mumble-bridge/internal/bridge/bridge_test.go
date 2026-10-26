package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble/mumbletest"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// fakeStack is a bridge in front of a fake relay and a fake murmur with the
// temporary-channel parent "Gawk".
func fakeStack(t *testing.T, mod ...func(*Options)) (*testBridge, *fakeRelay, *mumbletest.Server) {
	t.Helper()
	m := mumbletest.Start(t)
	m.AddChannel(0, "Gawk")
	fr := startRelay(t)
	opts := baseOptions(t, fr.addr, m.Addr())
	for _, f := range mod {
		f(&opts)
	}
	return startBridge(t, opts), fr, m
}

func joinVoice(t *testing.T, tb *testBridge, code, nick string, flags uint8) *browser {
	t.Helper()
	b, err := dialBrowser(t, tb.public, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	b.hello(mintTicket(t, code, nick, time.Now().Add(voice.TicketTTL)), flags)
	return b
}

// defaultUsernameRule is murmur's default `username` setting.
var defaultUsernameRule = regexp.MustCompile(`^[-=\w\[\]\{\}\(\)\@\|\.]+$`)

func connected(st wire.VoiceState) bool { return st.Flags&wire.VoiceStateFlagConnected != 0 }

func TestRefusedBadTicketIs4015(t *testing.T) {
	tb, _, _ := fakeStack(t)
	b, err := dialBrowser(t, tb.public, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	forged := mintTicket(t, "room-a", "alice", time.Now().Add(time.Minute))
	forged[len(forged)-1] ^= 1
	b.hello(forged, 0)
	b.expectClose(wire.CloseCodeVoiceRemoved)

	// A record that is not a VoiceHello is refused the same way.
	b2, _ := dialBrowser(t, tb.public, testOrigin)
	str, _ := b2.sess.OpenStreamSync(context.Background())
	b2.str = str
	b2.writeRaw([]byte{wire.Version, wire.TypeVoiceControl, 0})
	b2.expectClose(wire.CloseCodeVoiceRemoved)
}

func TestExpiredTicketIs4016(t *testing.T) {
	tb, _, _ := fakeStack(t)
	b, err := dialBrowser(t, tb.public, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	b.hello(mintTicket(t, "room-a", "alice", time.Now().Add(-time.Second)), 0)
	b.expectClose(wire.CloseCodeVoiceUnavailable)
}

func TestNoHelloIs4016(t *testing.T) {
	tb, _, _ := fakeStack(t, func(o *Options) { o.Timings.HelloTimeout = 100 * time.Millisecond })
	b, err := dialBrowser(t, tb.public, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	b.expectClose(wire.CloseCodeVoiceUnavailable)
}

func TestOriginNotAllowedIsRefused(t *testing.T) {
	tb, _, _ := fakeStack(t)
	if _, err := dialBrowser(t, tb.public, "https://evil.test"); err == nil {
		t.Fatal("a foreign Origin opened a voice session")
	}
}

func TestPerPodCapIs4016(t *testing.T) {
	tb, fr, _ := fakeStack(t, func(o *Options) { o.MaxVoiceSessions = 1 })
	a := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	a.waitState(connected)
	b := joinVoice(t, tb, "room-a", "bob", 0)
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	if _, body := tb.get(t, "/metrics"); !strings.Contains(body, `gawk_bridge_sessions_refused_total{reason="bridge_full"} 1`) {
		t.Fatal("cap refusal not counted")
	}
}

// The happy path: Register carries the ticket and the client address, the
// user lands in the temporary channel under its D9 name, Joined and the
// VoiceState follow, and audio passes both ways byte-identical.
func TestSessionJoinsAndCarriesAudio(t *testing.T) {
	tb, fr, m := fakeStack(t)
	b := joinVoice(t, tb, "room-a", "alice", 0)
	rs := fr.nextLink(t)
	if rs.code != "room-a" || len(rs.reg.Ticket) == 0 || rs.reg.Type != voice.TypeRegister || rs.reg.Remote == "" {
		t.Fatalf("register %+v", rs.reg)
	}
	j := rs.next(t, voice.TypeJoined)
	if j.Name != "alice [web]" || j.Session == 0 {
		t.Fatalf("joined %+v", j)
	}
	st := b.waitState(connected)
	if st.Name != "alice [web]" || st.Session != j.Session {
		t.Fatalf("state %+v", st)
	}
	parent, _ := m.Channel(0, "Gawk")
	ch, ok := m.Channel(parent, tb.tempChannel("room-a", "ROOM-A"))
	if !ok {
		t.Fatal("temporary channel not created")
	}
	native, err := mumble.Dial(context.Background(), mumble.Config{Addr: m.Addr(), Username: "native", TLS: mumble.TLSOptions{InsecureSkipVerify: true}, AudioQueue: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if err := native.JoinChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	// browser → Mumble
	sent := [][]byte{{1, 2, 3}, {4, 5, 6, 7}}
	eventually(t, "native hears the browser", func() bool {
		b.sendFrame(41, sent[0], false)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		p, err := native.Audio().Pop(ctx)
		return err == nil && p.Seq == 41 && bytes.Equal(p.Opus, sent[0]) && p.Session == j.Session
	})
	b.sendFrame(42, sent[1], true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		p, err := native.Audio().Pop(ctx)
		if err != nil {
			t.Fatal("end frame not heard")
		}
		if p.Seq == 42 {
			if !p.End || !bytes.Equal(p.Opus, sent[1]) {
				t.Fatalf("end frame %+v", p)
			}
			break
		}
	}
	// Mumble → browser
	if _, err := native.WriteAudio(7, []byte{9, 9, 9}, true); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-b.frames:
		h, payload, err := wire.ParseVoiceFrame(d)
		if err != nil || h.Speaker != native.Session() || h.Seq != 7 || !h.End || !bytes.Equal(payload, []byte{9, 9, 9}) {
			t.Fatalf("frame %+v %x %v", h, payload, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("browser heard nothing")
	}
	// Self-mute from the browser reaches Mumble and gates the uplink.
	b.control(wire.VoiceControlFlagSelfMute)
	b.waitState(func(s wire.VoiceState) bool { return s.Flags&wire.VoiceStateFlagSelfMute != 0 })
	st2 := rs.next(t, voice.TypeState)
	if st2.Flags&wire.RoomVoiceUserSelfMute == 0 {
		t.Fatalf("state %+v", st2)
	}
	b.sendFrame(50, []byte{1}, false)
	_, body := tb.get(t, "/metrics")
	for _, want := range []string{`gawk_bridge_frames_forwarded_total{direction="to_mumble"}`, `gawk_bridge_frames_forwarded_total{direction="to_browser"} 1`, `gawk_bridge_voice_sessions 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	// The browser leaving disconnects the Mumble user and closes the link.
	_ = b.sess.CloseWithError(0, "")
	rs.waitClosed(t)
	eventually(t, "mumble user gone", func() bool { _, ok := m.UserByName("alice [web]"); return !ok })
}

func TestListenOnlyNeverSpeaks(t *testing.T) {
	tb, fr, m := fakeStack(t)
	b := joinVoice(t, tb, "room-a", "alice", wire.VoiceHelloFlagListenOnly)
	fr.nextLink(t)
	st := b.waitState(connected)
	if st.Flags&wire.VoiceStateFlagSelfMute == 0 {
		t.Fatalf("listen-only not self-muted: %+v", st)
	}
	u, _ := m.UserByName("alice [web]")
	if !u.SelfMute {
		t.Fatal("murmur does not see the mute")
	}
	b.control(0) // asking to unmute changes nothing
	b.sendFrame(1, []byte{1}, false)
	eventually(t, "drop counted", func() bool {
		_, body := tb.get(t, "/metrics")
		return strings.Contains(body, `gawk_bridge_frames_dropped_total{direction="to_mumble",reason="listen_only"} 1`)
	})
}

func TestStaticRoomAndNameClash(t *testing.T) {
	tb, fr, m := fakeStack(t)
	friends := m.AddChannel(0, "Friends")
	gaming := m.AddChannel(friends, "Gaming")
	fr.onRegister = func(rs *relaySide) {
		rs.send(t, voice.Message{Type: voice.TypeAccepted, Code: rs.code, Channel: "Friends/Gaming"})
	}
	a := joinVoice(t, tb, "static", "tuhis", 0)
	fr.nextLink(t)
	a.waitState(connected)
	b := joinVoice(t, tb, "static", "tuhis", 0)
	rs := fr.nextLink(t)
	if j := rs.next(t, voice.TypeJoined); j.Name != "tuhis [web] 2" {
		t.Fatalf("clash name %q", j.Name)
	}
	b.waitState(connected)
	for _, n := range []string{"tuhis [web]", "tuhis [web] 2"} {
		if u, ok := m.UserByName(n); !ok || u.Channel != gaming {
			t.Fatalf("%s not in Friends/Gaming: %+v", n, u)
		}
	}
}

func TestMissingChannelIs4016AndCounted(t *testing.T) {
	tb, fr, _ := fakeStack(t)
	fr.onRegister = func(rs *relaySide) {
		rs.send(t, voice.Message{Type: voice.TypeAccepted, Code: rs.code, Channel: "Nope"})
	}
	b := joinVoice(t, tb, "static", "alice", 0)
	rs := fr.nextLink(t)
	left := rs.next(t, voice.TypeLeft)
	if left.CloseCode != wire.CloseCodeVoiceUnavailable || left.Reason == "" {
		t.Fatalf("left %+v", left)
	}
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	var st Status
	_, body := tb.get(t, "/statusz")
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.MumbleFailures[ReasonChannelNotFound] != 1 || st.LastFailure == nil || st.LastFailure.Reason != ReasonChannelNotFound {
		t.Fatalf("statusz %s", body)
	}
	if strings.Contains(body, "static") {
		t.Fatal("/statusz exposes the raw room code")
	}
}

func TestMurmurDownIs4016(t *testing.T) {
	fr := startRelay(t)
	tb := startBridge(t, baseOptions(t, fr.addr, "127.0.0.1:1"))
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	if st := tb.Status(); st.MumbleFailures[ReasonUnreachable] != 1 {
		t.Fatalf("%+v", st.MumbleFailures)
	}
}

func TestAutobanIs4016WithReason(t *testing.T) {
	tb, fr, m := fakeStack(t)
	m.Refuse.Store(true)
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	st := tb.Status()
	if st.MumbleFailures[ReasonAutoban] != 1 || st.LastFailure.Text != failureText[ReasonAutoban] {
		t.Fatalf("%+v", st)
	}
}

func TestRelayCloseAtRegistrationAndLater(t *testing.T) {
	tb, fr, m := fakeStack(t)
	fr.onRegister = func(rs *relaySide) {
		if rs.code == "refused" {
			rs.send(t, voice.Message{Type: voice.TypeClose, CloseCode: wire.CloseCodeVoiceRemoved, Reason: "not in the room"})
			return
		}
		rs.send(t, voice.Message{Type: voice.TypeAccepted, Code: rs.code, Temporary: true})
	}
	b := joinVoice(t, tb, "refused", "alice", 0)
	fr.nextLink(t)
	b.expectClose(wire.CloseCodeVoiceRemoved)

	// A kill later: the relay's Close ends the session with its code and
	// disconnects the Mumble user.
	c := joinVoice(t, tb, "room-b", "carol", 0)
	rs := fr.nextLink(t)
	c.waitState(connected)
	rs.send(t, voice.Message{Type: voice.TypeClose, CloseCode: wire.CloseCodeVoiceRemoved, Reason: "killed"})
	c.expectClose(wire.CloseCodeVoiceRemoved)
	eventually(t, "mumble user gone", func() bool { _, ok := m.UserByName("carol [web]"); return !ok })

	// The link dropping without a Close is a 4002: rejoin.
	d := joinVoice(t, tb, "room-c", "dave", 0)
	rs = fr.nextLink(t)
	d.waitState(connected)
	rs.l.close("home restarting")
	d.expectClose(wire.CloseCodeServerDraining)
}

func TestRelayUnreachableIs4016(t *testing.T) {
	m := mumbletest.Start(t)
	tb := startBridge(t, baseOptions(t, "127.0.0.1:1", m.Addr()))
	b := joinVoice(t, tb, "room-a", "alice", 0)
	b.expectClose(wire.CloseCodeVoiceUnavailable)
}

func TestMumbleKickIs4015AndServerMuteReachesBrowser(t *testing.T) {
	tb, fr, m := fakeStack(t)
	b := joinVoice(t, tb, "room-a", "alice", 0)
	rs := fr.nextLink(t)
	st := b.waitState(connected)
	m.SetMute(st.Session, true)
	b.waitState(func(s wire.VoiceState) bool { return s.Flags&wire.VoiceStateFlagServerMute != 0 })
	if s := rs.next(t, voice.TypeState); s.Flags&wire.RoomVoiceUserMute == 0 {
		t.Fatalf("relay state %+v", s)
	}
	b.sendFrame(1, []byte{1}, false)
	m.Kick(st.Session, "behave", false)
	left := rs.next(t, voice.TypeLeft)
	if left.CloseCode != wire.CloseCodeVoiceRemoved {
		t.Fatalf("left %+v", left)
	}
	b.expectClose(wire.CloseCodeVoiceRemoved)

	// A ban is a removal too; murmur going away is 4016.
	b2 := joinVoice(t, tb, "room-a", "bob", 0)
	fr.nextLink(t)
	st2 := b2.waitState(connected)
	m.Kick(st2.Session, "", true)
	b2.expectClose(wire.CloseCodeVoiceRemoved)
	b3 := joinVoice(t, tb, "room-a", "carol", 0)
	fr.nextLink(t)
	st3 := b3.waitState(connected)
	m.Ghost(st3.Session)
	b3.expectClose(wire.CloseCodeVoiceUnavailable)
	b4 := joinVoice(t, tb, "room-a", "dave", 0)
	fr.nextLink(t)
	b4.waitState(connected)
	m.DropAll()
	b4.expectClose(wire.CloseCodeVoiceUnavailable)
	if _, body := tb.get(t, "/metrics"); !strings.Contains(body, `gawk_bridge_mumble_removals_total{kind="ban"} 1`) || !strings.Contains(body, `gawk_bridge_mumble_removals_total{kind="kick"} 1`) {
		t.Fatal("removals not counted")
	}
}

// D19 with the docs/22 lesson: the drain's 4002 reaches the browser while
// /readyz still answers ready; it goes unready only once the drain is done.
func TestDrainSends4002WhileReady(t *testing.T) {
	tb, fr, m := fakeStack(t, func(o *Options) { o.Timings.NoticeSettle = 300 * time.Millisecond })
	b := joinVoice(t, tb, "room-a", "alice", 0)
	rs := fr.nextLink(t)
	b.waitState(connected)
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available && rosterHas(m, "alice [web]") })
	go tb.cancel()
	select {
	case code := <-b.notices:
		if code != wire.CloseCodeServerDraining {
			t.Fatalf("notice %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no drain notice")
	}
	if code, _ := tb.get(t, "/readyz"); code != 200 {
		t.Fatalf("/readyz %d while the drain notice is out: must still be ready", code)
	}
	_, code := b.closed()
	if code != wire.CloseCodeServerDraining {
		t.Fatalf("close code %d", code)
	}
	if left := rs.next(t, voice.TypeLeft); left.CloseCode != wire.CloseCodeServerDraining {
		t.Fatalf("left %+v", left)
	}
	select {
	case <-ol.sess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("observer link not closed by the drain")
	}
	eventually(t, "drain done", func() bool { return !tb.Ready() })
	select {
	case err := <-tb.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	tb.done <- nil
	eventually(t, "mumble users gone", func() bool { return len(m.Users()) == 0 })
	// A voice dial while draining (simulated on a fresh pod) is a 4002.
	tb2, _, _ := fakeStack(t)
	tb2.draining.Store(true)
	b2, err := dialBrowser(t, tb2.public, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	b2.expectClose(wire.CloseCodeServerDraining)
	if _, _, err := dialObserver(t, tb2.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "x"}); err == nil {
		t.Fatal("observer accepted while draining")
	}
}

func TestObserverPSKAndRoster(t *testing.T) {
	tb, fr, m := fakeStack(t)
	_, rsp, err := dialObserver(t, tb.internal, "wrong", voice.Message{Type: voice.TypeObserve, Code: "room-a"})
	if err == nil || rsp == nil || rsp.StatusCode != 401 {
		t.Fatalf("bad psk: %v %+v", err, rsp)
	}
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", DisplayCode: "ROOM-A", Temporary: true, Epoch: 3})
	if err != nil {
		t.Fatal(err)
	}
	r := ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	if len(r.Users) != 0 {
		t.Fatalf("observer lists itself: %+v", r)
	}
	parent, _ := m.Channel(0, "Gawk")
	name := tb.tempChannel("room-a", "ROOM-A")
	ch, ok := m.Channel(parent, name)
	if !ok {
		t.Fatal("observer did not create the temporary channel")
	}
	if u, _ := m.UserByName("gawkBridgeBot"); u.Channel != ch || !u.SelfMute || !u.SelfDeaf {
		t.Fatalf("observer %+v", u)
	}
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	r = ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, "alice [web]") })
	m.SetMute(st.Session, true)
	ol.waitRoster(t, func(m voice.Message) bool {
		return len(m.Users) == 1 && m.Users[0].Flags&wire.RoomVoiceUserMute != 0
	})
	s := tb.Status()
	if s.Observers != 1 || s.TempChannels != 1 || len(s.Rooms) != 1 {
		t.Fatalf("status %+v", s)
	}
	for k, room := range s.Rooms {
		if k != RoomKey([]byte("stats-key"), "room-a") || room.Observer == nil || !room.Observer.Available || room.VoiceSessions != 1 {
			t.Fatalf("room %s %+v", k, room)
		}
	}
	// Murmur dropping the observer: unavailable, then back.
	m.DropAll()
	ol.waitRoster(t, func(m voice.Message) bool { return !m.Available && m.Reason != "" })
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	// The link closing removes the observer, and with everyone gone murmur
	// removes the temporary channel.
	_ = ol.sess.CloseWithError(0, "")
	eventually(t, "observer gone", func() bool { _, ok := m.UserByName("gawkBridgeBot"); return !ok && tb.Status().Observers == 0 })
	eventually(t, "temp channel gone", func() bool { _, ok := m.Channel(parent, name); return !ok })
}

func TestObserverTempCapAndStaticFailure(t *testing.T) {
	tb, _, m := fakeStack(t, func(o *Options) { o.MaxTempChannels = 0 })
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	r := ol.waitRoster(t, func(m voice.Message) bool { return !m.Available })
	if r.Reason != failureText[ReasonTempChannelCap] {
		t.Fatalf("roster %+v", r)
	}
	ol2, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-b", Channel: "Missing"})
	if err != nil {
		t.Fatal(err)
	}
	ol2.waitRoster(t, func(m voice.Message) bool { return !m.Available && m.Reason == failureText[ReasonChannelNotFound] })
	// Once the channel exists, the retry finds it.
	m.AddChannel(0, "Missing")
	ol2.waitRoster(t, func(m voice.Message) bool { return m.Available })
	// A bad first message closes the link.
	ol3, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeRoster})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ol3.sess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("bad observe accepted")
	}
}

// The observer and the users of one room agree on the temporary channel
// whichever pod hosts them; a user's pod that gets there first creates it.
func TestTempChannelSharedAcrossPods(t *testing.T) {
	m := mumbletest.Start(t)
	m.AddChannel(0, "Gawk")
	fr := startRelay(t)
	podA := startBridge(t, baseOptions(t, fr.addr, m.Addr()))
	podB := startBridge(t, baseOptions(t, fr.addr, m.Addr()))
	b := joinVoice(t, podA, "room-x", "alice", 0) // before any observer
	fr.nextLink(t)
	b.waitState(connected)
	parent, _ := m.Channel(0, "Gawk")
	ch, ok := m.Channel(parent, podA.tempChannel("room-x", "ROOM-X"))
	if !ok || podA.tempChannel("room-x", "") != podB.tempChannel("room-x", "") {
		t.Fatal("pods disagree on the channel")
	}
	ol, _, err := dialObserver(t, podB.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-x", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available && rosterHas(m, "alice [web]") })
	if u, _ := m.UserByName("gawkBridgeBot"); u.Channel != ch {
		t.Fatal("observer on pod B is in another channel")
	}
}

func TestNamingRules(t *testing.T) {
	if got := MumbleName("{nick} [web]", "  Al\x00ice\n "); got != "Alice [web]" {
		t.Fatalf("%q", got)
	}
	if got := MumbleName("{nick} [web]", ""); got != "guest [web]" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("é", 60)
	if got := MumbleName("{nick} [web]", long); len(got)+3 > wire.MaxVoiceNameLen {
		t.Fatalf("%d bytes", len(got))
	}
	if got := SanitizeUsername("Jörg [web] 2!"); got != "J_rg_[web]_2_" {
		t.Fatalf("%q", got)
	}
	if nameCandidate("a", " ", 0) != "a" || nameCandidate("a", "_", 2) != "a_3" {
		t.Fatal("candidates")
	}
}

// D9 on a server with murmur's default username rule: the pretty form is
// refused, the sanitized one joins; a clash then suffixes it.
func TestInvalidUsernameFallsBackToSanitized(t *testing.T) {
	tb, fr, m := fakeStack(t)
	m.UsernameRule = defaultUsernameRule
	a := joinVoice(t, tb, "room-a", "tuhis", 0)
	if j := fr.nextLink(t).next(t, voice.TypeJoined); j.Name != "tuhis_[web]" {
		t.Fatalf("name %q", j.Name)
	}
	a.waitState(connected)
	b := joinVoice(t, tb, "room-a", "tuhis", 0)
	if j := fr.nextLink(t).next(t, voice.TypeJoined); j.Name != "tuhis_[web]_2" {
		t.Fatalf("name %q", j.Name)
	}
	b.waitState(connected)
}

// Review finding (PR #595): every voice-live room has its own observer and
// they all share -mumble-observer-name, so D9's ten-name ladder meant a
// server could hold ten observers and the eleventh voice room never became
// available. Observers get a ladder as long as the rooms a fleet can hold.
func TestMoreThanTenVoiceRoomsAllGetAnObserver(t *testing.T) {
	tb, _, _ := fakeStack(t)
	const rooms = 14
	links := make([]*observerLink, 0, rooms)
	for i := 0; i < rooms; i++ {
		ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: fmt.Sprintf("room-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		links = append(links, ol)
	}
	for _, ol := range links {
		ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	}
}
