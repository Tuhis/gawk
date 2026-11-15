package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble/mumbletest"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/murmurice"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/murmurice/icetest"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const testIceSecret = "ice-write-secret"

// iceOptions is Ice mode against a fake murmur's Ice, with short timers.
func iceOptions(ic *mumbletest.Ice) IceOptions {
	return IceOptions{
		Addr:            ic.Addr(),
		Secret:          testIceSecret,
		ServerID:        -1,
		Timeout:         time.Second,
		CallbackListen:  "127.0.0.1:0",
		CallbackHost:    "127.0.0.1",
		CallbackTimeout: 100 * time.Millisecond,
		Reconcile:       300 * time.Millisecond,
		CallbackProbe:   time.Hour,
	}
}

// iceStack is fakeStack in Ice mode.
func iceStack(t *testing.T, mod ...func(*Options)) (*testBridge, *fakeRelay, *mumbletest.Server, *mumbletest.Ice) {
	t.Helper()
	m := mumbletest.Start(t)
	m.AddChannel(0, "Gawk")
	ic := m.StartIce(t, testIceSecret)
	fr := startRelay(t)
	opts := baseOptions(t, fr.addr, m.Addr())
	opts.Mumble.Ice = iceOptions(ic)
	for _, f := range mod {
		f(&opts)
	}
	return startBridge(t, opts), fr, m, ic
}

func waitIce(t *testing.T, tb *testBridge, what string, cond func(*IceStatus) bool) *IceStatus {
	t.Helper()
	var st *IceStatus
	eventually(t, what, func() bool {
		st = tb.Status().Ice
		return st != nil && cond(st)
	})
	return st
}

func noObserverUser(t *testing.T, m *mumbletest.Server) {
	t.Helper()
	for _, u := range m.Users() {
		if strings.HasPrefix(u.Name, "gawkBridgeBot") {
			t.Fatalf("Ice mode created the observer user %q (D13)", u.Name)
		}
	}
}

// D3, D5, D13: a dynamic room's watcher reports an empty roster before
// anyone joins, with no channel and no observer user; the first voice user
// creates the channel; the roster follows; murmur removes the channel once
// it is empty.
func TestIceDynamicRoomFirstUserCreatesTheChannel(t *testing.T) {
	tb, fr, m, ic := iceStack(t)
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", DisplayCode: "ROOM-A", Temporary: true, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	r := ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	if len(r.Users) != 0 {
		t.Fatalf("roster %+v", r)
	}
	parent, _ := m.Channel(0, "Gawk")
	name := tb.tempChannel("room-a", "ROOM-A")
	if _, ok := m.Channel(parent, name); ok {
		t.Fatal("the channel exists before anyone joined voice")
	}
	noObserverUser(t, m)
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	ch, ok := m.Channel(parent, name)
	if !ok {
		t.Fatal("the first voice user did not create the channel")
	}
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, "alice [web]") })
	m.SetMute(st.Session, true)
	ol.waitRoster(t, func(m voice.Message) bool {
		return len(m.Users) == 1 && m.Users[0].Flags&wire.RoomVoiceUserMute != 0
	})
	s := tb.Status()
	if s.Roster != "ice" || s.Ice == nil || !s.Ice.Available || s.Ice.Callbacks != cbRegistered || s.Observers != 1 || s.TempChannels != 0 {
		t.Fatalf("status %+v %+v", s, s.Ice)
	}
	if s.Ice.ServerID == nil || *s.Ice.ServerID != 0 || s.Ice.MurmurVersion != "1.6.1001" || s.Ice.LastEventAgeMs == nil {
		t.Fatalf("ice status %+v", s.Ice)
	}
	if adds, _ := ic.Calls(); adds != 1 {
		t.Fatalf("%d addCallback calls", adds)
	}
	_ = b.sess.CloseWithError(0, "")
	eventually(t, "temp channel gone", func() bool { _, ok := m.Channel(parent, name); return !ok })
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available && len(m.Users) == 0 })
	_ = ch
	noObserverUser(t, m)
}

// G2: a static room's roster shows its native users before anyone joins
// voice; a missing channel is channel_not_found until it exists.
func TestIceStaticRoomRosterBeforeVoice(t *testing.T) {
	tb, _, m, _ := iceStack(t)
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-s", Channel: "Lobby/Talk"})
	if err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return !m.Available && m.Reason == failureText[ReasonChannelNotFound] })
	lobby := m.AddChannel(0, "Lobby")
	talk := m.AddChannel(lobby, "Talk")
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available && len(m.Users) == 0 })
	native, err := mumble.Dial(context.Background(), mumble.Config{Addr: m.Addr(), Username: "bob", TLS: mumble.TLSOptions{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if err := native.JoinChannel(context.Background(), talk); err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, "bob") })
	if err := native.SetSelf(true, false); err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool {
		return len(m.Users) == 1 && m.Users[0].Flags&wire.RoomVoiceUserSelfMute != 0
	})
	native.Close()
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available && len(m.Users) == 0 })
	if tb.Status().MumbleFailures[ReasonChannelNotFound] != 1 {
		t.Fatalf("failures %+v", tb.Status().MumbleFailures)
	}
}

// D6: the cap refuses a create, never a join, counted in the voice user's
// own tree, on a pod that holds no observer link.
func TestIceTempCapRefusesCreateNotJoin(t *testing.T) {
	tb, fr, m, _ := iceStack(t, func(o *Options) { o.MaxTempChannels = 1 })
	a := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	a.waitState(connected)
	b := joinVoice(t, tb, "room-b", "bob", 0)
	if left := fr.nextLink(t).next(t, voice.TypeLeft); left.Reason != failureText[ReasonTempChannelCap] {
		t.Fatalf("left %+v", left)
	}
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	c := joinVoice(t, tb, "room-a", "carol", 0)
	fr.nextLink(t)
	c.waitState(connected)
	parent, _ := m.Channel(0, "Gawk")
	ch, _ := m.Channel(parent, tb.tempChannel("room-a", ""))
	for _, n := range []string{"alice [web]", "carol [web]"} {
		if u, ok := m.UserByName(n); !ok || u.Channel != ch {
			t.Fatalf("%s %+v", n, u)
		}
	}
	if tb.Status().MumbleFailures[ReasonTempChannelCap] != 1 {
		t.Fatalf("failures %+v", tb.Status().MumbleFailures)
	}
}

// Two pods race for one room's channel and end up in one.
func TestIceTempChannelRaceAcrossPods(t *testing.T) {
	m := mumbletest.Start(t)
	m.AddChannel(0, "Gawk")
	ic := m.StartIce(t, testIceSecret)
	fr := startRelay(t)
	mk := func() *testBridge {
		o := baseOptions(t, fr.addr, m.Addr())
		o.Mumble.Ice = iceOptions(ic)
		return startBridge(t, o)
	}
	podA, podB := mk(), mk()
	a := joinVoice(t, podA, "room-x", "alice", 0)
	b := joinVoice(t, podB, "room-x", "bob", 0)
	fr.nextLink(t)
	fr.nextLink(t)
	sa, sb := a.waitState(connected), b.waitState(connected)
	ua, _ := m.UserByName("alice [web]")
	ub, _ := m.UserByName("bob [web]")
	if ua.Channel != ub.Channel || sa.Session == sb.Session {
		t.Fatalf("alice in %d, bob in %d", ua.Channel, ub.Channel)
	}
	if ic.Registrations() != 2 {
		t.Fatalf("%d registrations, want one per pod", ic.Registrations())
	}
	noObserverUser(t, m)
}

// G4, D11: an Ice outage reports unavailable, keeps voice up, creates no
// Mumble user, and recovers; a murmur restart re-registers.
func TestIceOutageAndRestart(t *testing.T) {
	tb, fr, m, ic := iceStack(t)
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, "alice [web]") })
	users := len(m.Users())
	ic.Down.Store(true)
	ic.CloseConns()
	ol.waitRoster(t, func(m voice.Message) bool { return !m.Available && m.Reason == failureText[ReasonIceUnavailable] })
	// Voice keeps working: the browser's audio still reaches murmur.
	before := tb.Status().VoiceSessions
	b.sendFrame(1, []byte("opus"), true)
	time.Sleep(300 * time.Millisecond)
	if len(m.Users()) != users || tb.Status().VoiceSessions != before {
		t.Fatalf("users %d → %d during the outage", users, len(m.Users()))
	}
	noObserverUser(t, m)
	ic.Down.Store(false)
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available && rosterHas(m, "alice [web]") })
	if tb.Status().MumbleFailures[ReasonIceUnavailable] == 0 {
		t.Fatal("the outage was not counted")
	}
	// An event answers the registration the reconnect made (D15: two
	// reconnects with no event between them would hit the bound).
	m.SetMute(st.Session, true)
	ol.waitRoster(t, func(m voice.Message) bool { return len(m.Users) == 1 && m.Users[0].Flags&wire.RoomVoiceUserMute != 0 })
	waitIce(t, tb, "answered", func(s *IceStatus) bool { return s.Unanswered == 1 })
	// A murmur restart loses the registration; the reconnect makes it
	// again and events flow at callback pace.
	adds, _ := ic.Calls()
	ic.Restart()
	eventually(t, "re-registered", func() bool { a, _ := ic.Calls(); return a > adds && ic.Registrations() == 1 })
	waitIce(t, tb, "available after the restart", func(s *IceStatus) bool { return s.Available })
	m.SetMute(st.Session, false)
	start := time.Now()
	ol.waitRoster(t, func(m voice.Message) bool { return len(m.Users) == 1 && m.Users[0].Flags&wire.RoomVoiceUserMute == 0 })
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("the mute after the restart took %s", d)
	}
}

// MI2 criterion 7 on the fake: a silently dropped registration is repaired
// within one reconciliation, counted as drift and as a re-add.
func TestIceSilentLossRepaired(t *testing.T) {
	tb, fr, m, ic := iceStack(t)
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, "alice [web]") })
	ic.DropRegistrations()
	m.SetMute(st.Session, true)
	ol.waitRoster(t, func(m voice.Message) bool { return len(m.Users) == 1 && m.Users[0].Flags&wire.RoomVoiceUserMute != 0 })
	_, metrics := tb.get(t, "/metrics")
	for _, want := range []string{"gawk_bridge_ice_reconcile_drift_total 1", `gawk_bridge_ice_registrations_total{result="ok"} 2`, "gawk_bridge_ice_registered 1"} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics lack %q", want)
		}
	}
	m.SetMute(st.Session, false)
	start := time.Now()
	ol.waitRoster(t, func(m voice.Message) bool { return len(m.Users) == 1 && m.Users[0].Flags&wire.RoomVoiceUserMute == 0 })
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("the next mute took %s, want callback time", d)
	}
	waitIce(t, tb, "registered", func(s *IceStatus) bool { return s.Callbacks == cbRegistered && s.Unanswered == 1 })
}

// D15: a callback host murmur cannot reach ends in unreachable after two
// registrations, the roster keeps following at reconcile pace, and the
// probe registers once more.
func TestIceUnreachableCallbackHost(t *testing.T) {
	tb, fr, m, ic := iceStack(t, func(o *Options) {
		o.Mumble.Ice.CallbackHost = "127.0.0.2" // the endpoint listens on 127.0.0.1 only: refused
		o.Mumble.Ice.CallbackProbe = 2 * time.Second
	})
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, "alice [web]") })
	// Each reconcile must drift: keep changing something.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		mute := false
		for {
			select {
			case <-stop:
				return
			case <-time.After(130 * time.Millisecond):
				mute = !mute
				m.SetMute(st.Session, mute)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	s := waitIce(t, tb, "unreachable", func(s *IceStatus) bool { return s.Callbacks == cbUnreachable })
	if s.NextProbeInMs == nil || *s.NextProbeInMs > 2000 {
		t.Fatalf("next probe %v", s.NextProbeInMs)
	}
	adds, removes := ic.Calls()
	if adds != 2 || removes != 1 {
		t.Fatalf("adds %d removes %d before the probe, want 2 and 1", adds, removes)
	}
	// The probe: exactly one more.
	eventually(t, "the probe", func() bool { a, _ := ic.Calls(); return a == 3 })
	waitIce(t, tb, "unreachable again", func(s *IceStatus) bool { return s.Callbacks == cbUnreachable })
	if a, _ := ic.Calls(); a != 3 {
		t.Fatalf("%d adds after the failed probe", a)
	}
	if !strings.Contains(func() string { _, b := tb.get(t, "/statusz"); return b }(), `"callbacks": "unreachable"`) {
		t.Fatal("statusz")
	}
}

// D16: on 1.6.870 the bridge sends no callback operation at all, through
// start, a reconnect and a drain, and its roster follows at reconcile pace.
func TestIceUnsafeVersionNeverRegisters(t *testing.T) {
	m2 := mumbletest.Start(t)
	m2.AddChannel(0, "Gawk")
	ic2 := m2.StartIce(t, testIceSecret)
	ic2.Patch = 870
	fr2 := startRelay(t)
	o := baseOptions(t, fr2.addr, m2.Addr())
	o.Mumble.Ice = iceOptions(ic2)
	tb2 := startBridge(t, o)
	ol, _, err := dialObserver(t, tb2.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	b := joinVoice(t, tb2, "room-a", "alice", 0)
	fr2.nextLink(t)
	b.waitState(connected)
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, "alice [web]") })
	waitIce(t, tb2, "unsafe", func(s *IceStatus) bool { return s.Callbacks == cbUnsafe && s.MurmurVersion == "1.6.870" })
	ic2.CloseConns()
	waitIce(t, tb2, "back after the reconnect", func(s *IceStatus) bool { return s.Available && s.Callbacks == cbUnsafe })
	tb2.Drain()
	if adds, removes := ic2.Calls(); adds != 0 || removes != 0 {
		t.Fatalf("callback operations on 1.6.870: %d adds, %d removes", adds, removes)
	}
}

// D17: a drain removes the registration before the listener closes, and
// hands the room's watcher to the surviving pod.
func TestIceDrainRemovesTheRegistration(t *testing.T) {
	m := mumbletest.Start(t)
	m.AddChannel(0, "Gawk")
	m.AddChannel(0, "Talk")
	ic := m.StartIce(t, testIceSecret)
	fr := startRelay(t)
	mk := func() *testBridge {
		o := baseOptions(t, fr.addr, m.Addr())
		o.Mumble.Ice = iceOptions(ic)
		return startBridge(t, o)
	}
	podA, podB := mk(), mk()
	waitIce(t, podA, "A registered", func(s *IceStatus) bool { return s.Available })
	waitIce(t, podB, "B registered", func(s *IceStatus) bool { return s.Available })
	if ic.Registrations() != 2 {
		t.Fatalf("%d registrations", ic.Registrations())
	}
	olA, _, err := dialObserver(t, podA.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-s", Channel: "Talk"})
	if err != nil {
		t.Fatal(err)
	}
	olA.waitRoster(t, func(m voice.Message) bool { return m.Available })
	addr := podA.ice.callbackAddr()
	podA.Drain()
	if ic.Registrations() != 1 {
		t.Fatalf("%d registrations after the drain", ic.Registrations())
	}
	// The listener is still up until shutdown: removeCallback came first.
	if k, err := icetest.Dial(addr.String(), time.Second); err != nil {
		t.Fatalf("the listener closed before the shutdown: %v", err)
	} else {
		k.Close()
	}
	select {
	case <-olA.sess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the drain left the observer link open")
	}
	olB, _, err := dialObserver(t, podB.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-s", Channel: "Talk"})
	if err != nil {
		t.Fatal(err)
	}
	olB.waitRoster(t, func(m voice.Message) bool { return m.Available })
	if _, removes := ic.Calls(); removes != 1 {
		t.Fatalf("%d removeCallback calls", removes)
	}
}

// D8, D9, D11: the configuration errors report their own reasons.
func TestIceConfigurationErrors(t *testing.T) {
	t.Run("wrong secret", func(t *testing.T) {
		tb, _, _, _ := iceStack(t, func(o *Options) { o.Mumble.Ice.Secret = "nope" })
		ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "r", Temporary: true})
		if err != nil {
			t.Fatal(err)
		}
		ol.waitRoster(t, func(m voice.Message) bool { return !m.Available && m.Reason == failureText[ReasonIceSecret] })
	})
	t.Run("read secret", func(t *testing.T) {
		m := mumbletest.Start(t)
		ic := m.StartIce(t, testIceSecret)
		ic.ReadSecret = "read-only"
		fr := startRelay(t)
		o := baseOptions(t, fr.addr, m.Addr())
		o.Mumble.Ice = iceOptions(ic)
		o.Mumble.Ice.Secret = "read-only"
		tb := startBridge(t, o)
		s := waitIce(t, tb, "ice_secret", func(s *IceStatus) bool { return s.Reason == ReasonIceSecret })
		if s.Available {
			t.Fatal("available with a read secret")
		}
		// Refusals register nothing: they never reach D15's bound.
		time.Sleep(500 * time.Millisecond)
		if s := tb.Status().Ice; s.Unanswered != 0 {
			t.Fatalf("unanswered %d after refusals", s.Unanswered)
		}
	})
	t.Run("ambiguous server", func(t *testing.T) {
		m := mumbletest.Start(t)
		ic := m.StartIce(t, testIceSecret)
		ic.ServerIDs = []int32{1, 2}
		fr := startRelay(t)
		o := baseOptions(t, fr.addr, m.Addr())
		o.Mumble.Ice = iceOptions(ic)
		tb := startBridge(t, o)
		waitIce(t, tb, "ambiguous", func(s *IceStatus) bool { return s.Reason == ReasonIceServerAmbiguous })
		o2 := o
		o2.Mumble.Ice.ServerID = 2
		tb2 := startBridge(t, o2)
		s := waitIce(t, tb2, "server 2", func(s *IceStatus) bool { return s.Available })
		if *s.ServerID != 2 {
			t.Fatalf("server %d", *s.ServerID)
		}
	})
}

// D2: events arriving during the initial read are buffered and applied after
// it; during a reconcile they are not drift.
func TestIceEventsDuringAReadAreBuffered(t *testing.T) {
	var mu sync.Mutex
	users := []icetest.User{{Session: 1, UserID: -1, Name: "bob", Channel: 0}}
	delay := time.Duration(0)
	srv, err := icetest.NewServer(func(r icetest.Request) icetest.Reply {
		mu.Lock()
		defer mu.Unlock()
		switch r.Operation {
		case "getUsers":
			return icetest.Reply{Result: icetest.UserMap(users), Delay: delay}
		case "getChannels":
			return icetest.Reply{Result: icetest.ChannelMap([]icetest.Channel{{ID: 0, Parent: -1, Name: "Root"}})}
		}
		return icetest.Reply{}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	s := &Server{opts: Options{Mumble: MumbleOptions{Ice: IceOptions{Addr: srv.Addr()}}}, metrics: newMetrics("t"), log: testLogger(t), now: time.Now}
	s.timings = s.opts.Timings.withDefaults()
	m := newIceMirror(s)
	m.sid = 0
	defer m.cli.Close()
	ctx := context.Background()
	if _, err := m.read(ctx, false); err != nil {
		t.Fatal(err)
	}
	// The read in flight sees bob unmuted; the event that lands meanwhile
	// says muted, and is newer.
	mu.Lock()
	delay = 200 * time.Millisecond
	mu.Unlock()
	done := make(chan bool)
	go func() {
		drift, err := m.read(ctx, true)
		if err != nil {
			t.Error(err)
		}
		done <- drift
	}()
	time.Sleep(50 * time.Millisecond)
	m.onEvent(murmurice.Event{Op: murmurice.OpUserStateChanged, User: murmurice.User{Session: 1, UserID: -1, Name: "bob", SelfMute: true}}, nil)
	m.mu.Lock()
	if m.users[1].SelfMute {
		t.Fatal("the event was applied during the read")
	}
	m.mu.Unlock()
	if <-done {
		t.Fatal("an event during the read counted as drift")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.users[1].SelfMute {
		t.Fatal("the buffered event was lost")
	}
}

// D9, D10: no secret, user name, comment, identity or address reaches a
// log line, through events, outages and malformed callbacks.
func TestIceLogsNoPayloadOrSecret(t *testing.T) {
	var buf bytes.Buffer
	var bmu sync.Mutex
	lw := writerFunc(func(p []byte) (int, error) { bmu.Lock(); defer bmu.Unlock(); return buf.Write(p) })
	tb, fr, m, ic := iceStack(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(lw, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: "room-a", Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	b := joinVoice(t, tb, "room-a", "zelda-secret-name", 0)
	fr.nextLink(t)
	b.waitState(connected)
	ol.waitRoster(t, func(m voice.Message) bool { return len(m.Users) == 1 })
	// A malformed event.
	k, err := icetest.Dial(tb.ice.callbackAddr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var w icetest.W
	w.WriteUser(icetest.User{Session: 99, Name: "malformed-name", Comment: "a comment", Addr: []byte{1, 2, 3, 4}})
	_ = k.Oneway("", tb.ice.ep.Identity().Name, "userConnected", append(w.B, 0xff))
	k.Close()
	ic.Down.Store(true)
	ic.CloseConns()
	waitIce(t, tb, "down", func(s *IceStatus) bool { return !s.Available })
	ic.Down.Store(false)
	waitIce(t, tb, "up", func(s *IceStatus) bool { return s.Available })
	tb.Drain()
	_ = m
	bmu.Lock()
	out := buf.String()
	bmu.Unlock()
	for _, leak := range []string{testIceSecret, "zelda-secret-name", "malformed-name", "a comment", "an identity", "a context", "192.0.2.33", tb.ice.ep.Identity().Name} {
		if strings.Contains(out, leak) {
			t.Errorf("the log carries %q", leak)
		}
	}
	if !strings.Contains(out, ReasonIceProtocol) {
		t.Error("the malformed event was not logged as ice_protocol")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// D12: /statusz in both modes.
func TestStatuszRosterMode(t *testing.T) {
	tb, _, _ := fakeStack(t)
	_, body := tb.get(t, "/statusz")
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st["roster"] != "observer" || st["ice"] != nil {
		t.Fatalf("observer mode statusz %s", body)
	}
	ti, _, _, _ := iceStack(t)
	waitIce(t, ti, "available", func(s *IceStatus) bool { return s.Available })
	_, body = ti.get(t, "/statusz")
	st = nil
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	ice, _ := st["ice"].(map[string]any)
	if st["roster"] != "ice" || ice == nil || ice["callbacks"] != "registered" || ice["available"] != true {
		t.Fatalf("ice mode statusz %s", body)
	}
	for _, k := range []string{"serverId", "murmurVersion", "unanswered"} {
		if _, ok := ice[k]; !ok {
			t.Errorf("statusz ice lacks %s: %s", k, body)
		}
	}
	if strings.Contains(body, testIceSecret) {
		t.Fatal("statusz carries the secret")
	}
	_ = fmt.Sprint
}

// Round-1 review finding 1: a probe whose registration works must not fail
// on a change that happened before its addCallback. The probe fires between
// two reconciles; the next reconcile must judge drift from the probe's own
// baseline, not from the reconcile before it.
func TestIceProbeIsJudgedFromItsOwnBaseline(t *testing.T) {
	tb, fr, m, ic := iceStack(t, func(o *Options) {
		o.Mumble.Ice.Reconcile = 4 * time.Second
		o.Mumble.Ice.CallbackProbe = 6 * time.Second
	})
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	waitIce(t, tb, "registered", func(s *IceStatus) bool { return s.Available && s.Callbacks == cbRegistered })
	// Every delivery fails: two drifting reconciles give up.
	ic.Refuse.Store(true)
	mute := false
	stop := make(chan struct{})
	toggled := make(chan struct{})
	go func() {
		defer close(toggled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(700 * time.Millisecond): // never divides the 4 s interval evenly
				mute = !mute
				m.SetMute(st.Session, mute)
			}
		}
	}()
	eventually2(t, 20*time.Second, "unreachable", func() bool { return tb.Status().Ice.Callbacks == cbUnreachable })
	close(stop)
	<-toggled
	gaveUp := time.Now() // a reconcile tick, within the 10 ms poll
	ic.Refuse.Store(false)
	// A change after the next reconcile and before the probe.
	time.Sleep(time.Until(gaveUp.Add(5 * time.Second))) // after the +4 s reconcile, before the +6 s probe
	m.SetMute(st.Session, !mute)
	eventually2(t, 10*time.Second, "the probe", func() bool { return tb.Status().Ice.Callbacks == cbProbing })
	// The reconcile after the probe (at +8 s) sees nothing new since it.
	time.Sleep(time.Until(gaveUp.Add(9 * time.Second)))
	if s := tb.Status().Ice; s.Callbacks != cbProbing {
		t.Fatalf("callbacks %s after a quiet interval: the probe failed on a change from before it", s.Callbacks)
	}
	// And the next change arrives by callback: the probe passes.
	m.SetMute(st.Session, mute)
	waitIce(t, tb, "registered", func(s *IceStatus) bool { return s.Callbacks == cbRegistered })
}

// Round-1 review finding 6: the cap refuses a create only when the mirror
// does not already hold the channel (another pod's), which would end as a
// join.
func TestIceTempCapLetsAJoinThrough(t *testing.T) {
	s := &Server{opts: Options{MaxTempChannels: 1}}
	s.ice = &iceMirror{s: s, chans: map[uint32]murmurice.Channel{
		0: {ID: 0, Name: "Root"},
		1: {ID: 1, Parent: 0, Name: "Gawk"},
		2: {ID: 2, Parent: 1, Name: "gawk abc", Temporary: true},
	}}
	if err := s.iceTempCap("gawk abc")(1); err != nil {
		t.Fatalf("a channel the mirror holds was refused: %v", err)
	}
	if err := s.iceTempCap("gawk def")(1); failureReason(err) != ReasonTempChannelCap {
		t.Fatalf("a new channel past the cap: %v", err)
	}
}
