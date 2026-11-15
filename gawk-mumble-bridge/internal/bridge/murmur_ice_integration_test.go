package bridge

// Ice mode against REAL murmur (docs/89 MI2 criteria 3–5 and 7–9), run as
// {1.5.939, 1.6.1001} × {observer, ice} by the CI matrix, plus the 1.6.870
// version gate. testdata/murmur.sh starts murmur with Ice on its main server
// and prints the environment.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/murmurice"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

type murmurIceEnv struct {
	murmurEnv
	ice, secret, cbHost, version, container string
}

func needMurmurIce(t *testing.T) murmurIceEnv {
	t.Helper()
	e := murmurIceEnv{murmurEnv: needMurmur(t),
		ice:       os.Getenv("GAWK_TEST_MURMUR_ICE"),
		secret:    os.Getenv("GAWK_TEST_MURMUR_ICE_SECRET"),
		cbHost:    os.Getenv("GAWK_TEST_MURMUR_CALLBACK_HOST"),
		version:   os.Getenv("GAWK_TEST_MURMUR_VERSION"),
		container: os.Getenv("GAWK_TEST_MURMUR_CONTAINER"),
	}
	if e.ice == "" {
		t.Skip("GAWK_TEST_MURMUR_ICE unset: see testdata/murmur.sh")
	}
	return e
}

// callbacksSafe skips Ice-mode timing tests on 1.6.870, where the bridge
// runs on reconciliation only (D16; TestMurmurIceVersionGate covers it).
func (e murmurIceEnv) callbacksSafe(t *testing.T) {
	t.Helper()
	if e.version == "1.6.870" {
		t.Skip("murmur 1.6.870: Ice mode never registers callbacks (D16)")
	}
}

// withIce puts a murmur bridge in Ice mode, reaching Ice through addr.
func (e murmurIceEnv) withIce(addr string, reconcile time.Duration) func(*Options) {
	return func(o *Options) {
		o.Mumble.Ice = IceOptions{
			Addr: addr, Secret: e.secret, ServerID: -1, Timeout: 2 * time.Second,
			CallbackListen: net.JoinHostPort(e.cbHost, "0"), CallbackHost: e.cbHost,
			CallbackTimeout: 100 * time.Millisecond, Reconcile: reconcile, CallbackProbe: time.Hour,
		}
	}
}

func (e murmurIceEnv) log(t *testing.T) string {
	t.Helper()
	if e.container == "" {
		t.Fatal("GAWK_TEST_MURMUR_CONTAINER unset")
	}
	out, err := exec.Command("docker", "logs", e.container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	return string(out)
}

// observerNameSeen fails if any user named like the observer exists.
func observerNameSeen(t *testing.T, su *mumble.Client) {
	t.Helper()
	for _, u := range su.Users() {
		if strings.HasPrefix(u.Name, "gawkBridgeBot") {
			t.Fatalf("a user named %q exists in Ice mode (D13)", u.Name)
		}
	}
}

func bothModes(t *testing.T, f func(t *testing.T, mode string)) {
	for _, mode := range []string{"observer", "ice"} {
		t.Run(mode, func(t *testing.T) { f(t, mode) })
	}
}

// Criterion 3: a static room's roster before voice in both modes; in Ice
// mode a native user's join, leave, self-mute and server-mute reach it
// within 200 ms.
func TestMurmurModesStaticRoster(t *testing.T) {
	e := needMurmurIce(t)
	bothModes(t, func(t *testing.T, mode string) {
		if mode == "ice" {
			e.callbacksSafe(t)
		}
		su, _, _ := superuser(t, e.addr, e.supw)
		static := uniq("Static-")
		sid, err := su.CreateChannel(context.Background(), 0, static, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = su.RemoveChannel(sid) })
		native := murmurDial(t, e.addr, uniq("native-"), "")
		if err := native.JoinChannel(context.Background(), sid); err != nil {
			t.Fatal(err)
		}
		fr := startRelay(t)
		var mods []func(*Options)
		if mode == "ice" {
			mods = append(mods, e.withIce(e.ice, 5*time.Second))
		}
		tb := murmurBridge(t, fr, e.addr, "", mods...)
		ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: uniq("room-"), Channel: static})
		if err != nil {
			t.Fatal(err)
		}
		ol.waitRoster(t, func(m voice.Message) bool { return m.Available && rosterHas(m, native.Self().Name) })
		if mode != "ice" {
			return
		}
		observerNameSeen(t, su)
		timed := func(what string, act func(), cond func(voice.Message) bool) {
			t.Helper()
			start := time.Now()
			act()
			ol.waitRoster(t, cond)
			if d := time.Since(start); d > 200*time.Millisecond {
				t.Errorf("%s reached the roster in %s, want under 200 ms", what, d)
			}
		}
		flagsOf := func(m voice.Message, name string) (uint8, bool) {
			for _, u := range m.Users {
				if u.Name == name {
					return u.Flags, true
				}
			}
			return 0, false
		}
		name := native.Self().Name
		timed("a self-mute", func() { _ = native.SetSelf(true, false) }, func(m voice.Message) bool {
			f, ok := flagsOf(m, name)
			return ok && f&wire.RoomVoiceUserSelfMute != 0
		})
		timed("a server mute", func() { _ = su.SetServerMute(native.Session(), true) }, func(m voice.Message) bool {
			f, ok := flagsOf(m, name)
			return ok && f&wire.RoomVoiceUserMute != 0
		})
		other := uniq("joiner-")
		var oc *mumble.Client
		timed("a join", func() {
			oc = murmurDial(t, e.addr, other, "")
			_ = oc.JoinChannel(context.Background(), sid)
		}, func(m voice.Message) bool { return rosterHas(m, other) })
		_ = oc
		timed("a leave", func() { native.Close() }, func(m voice.Message) bool { return !rosterHas(m, name) })
		observerNameSeen(t, su)
	})
}

// Criterion 3: a dynamic room's channel — in Ice mode created by the first
// voice user and joined by a second who raced it on another pod; it
// survives its creator leaving while others stay, and murmur removes it once
// it is empty. In observer mode the observer keeps it, as R79 left it.
func TestMurmurModesDynamicChannel(t *testing.T) {
	e := needMurmurIce(t)
	bothModes(t, func(t *testing.T, mode string) {
		if mode == "ice" {
			e.callbacksSafe(t)
		}
		su, parentName, parent := superuser(t, e.addr, e.supw)
		fr := startRelay(t)
		var mods []func(*Options)
		if mode == "ice" {
			mods = append(mods, e.withIce(e.ice, 5*time.Second))
		}
		podA := murmurBridge(t, fr, e.addr, parentName, mods...)
		podB := murmurBridge(t, fr, e.addr, parentName, mods...)
		code := uniq("room-")
		ol, _, err := dialObserver(t, podA.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: code, Temporary: true, Epoch: 1})
		if err != nil {
			t.Fatal(err)
		}
		ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
		name := podA.tempChannel(code, "")
		if mode == "ice" {
			time.Sleep(200 * time.Millisecond)
			if _, ok := su.FindChild(parent, name); ok {
				t.Fatal("the channel exists before anyone joined voice (D5)")
			}
			observerNameSeen(t, su)
		}
		// Two users on two pods race for the channel.
		a := joinVoice(t, podA, code, uniq("a"), 0)
		b := joinVoice(t, podB, code, uniq("b"), 0)
		fr.nextLink(t)
		fr.nextLink(t)
		sa, sb := a.waitState(connected), b.waitState(connected)
		ch := suChild(t, su, parent, name)
		suInChannel(t, su, sa.Session, ch)
		suInChannel(t, su, sb.Session, ch)
		if c, _ := su.Channel(ch); !c.Temporary {
			t.Fatal("not temporary")
		}
		ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, sa.Name) && rosterHas(m, sb.Name) })
		// The creator leaves; the other stays: the channel stays.
		_ = a.sess.CloseWithError(0, "")
		ol.waitRoster(t, func(m voice.Message) bool { return !rosterHas(m, sa.Name) && rosterHas(m, sb.Name) })
		if _, ok := su.Channel(ch); !ok {
			t.Fatal("the channel went while a user is still in it")
		}
		if mode == "ice" {
			observerNameSeen(t, su)
			_ = b.sess.CloseWithError(0, "")
			eventually(t, "the channel removed once empty", func() bool { _, ok := su.Channel(ch); return !ok })
			ol.waitRoster(t, func(m voice.Message) bool { return m.Available && len(m.Users) == 0 })
		} else {
			_ = ol.sess.CloseWithError(0, "")
			_ = b.sess.CloseWithError(0, "")
			eventually(t, "the channel removed once empty", func() bool { _, ok := su.Channel(ch); return !ok })
		}
	})
}

// Criterion 3: the D6 cap refuses a create but not a join, for a voice
// user on a pod that holds no observer link.
func TestMurmurIceTempCap(t *testing.T) {
	e := needMurmurIce(t)
	e.callbacksSafe(t) // the mirror the cap counts in is 5 s stale there
	_, parentName, _ := superuser(t, e.addr, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, parentName, e.withIce(e.ice, 5*time.Second), func(o *Options) { o.MaxTempChannels = 1 })
	code := uniq("room-")
	a := joinVoice(t, tb, code, uniq("a"), 0)
	fr.nextLink(t)
	a.waitState(connected)
	b := joinVoice(t, tb, uniq("room-"), uniq("b"), 0)
	if left := fr.nextLink(t).next(t, voice.TypeLeft); left.Reason != failureText[ReasonTempChannelCap] {
		t.Fatalf("left %+v", left)
	}
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	c := joinVoice(t, tb, code, uniq("c"), 0)
	fr.nextLink(t)
	c.waitState(connected)
}

// tcpProxy forwards to target until cut.
type tcpProxy struct {
	t      *testing.T
	addr   string
	target string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
}

func startProxy(t *testing.T, target string) *tcpProxy {
	p := &tcpProxy{t: t, target: target, conns: map[net.Conn]struct{}{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.cut)
	return p
}

func (p *tcpProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", p.target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns[c], p.conns[u] = struct{}{}, struct{}{}
			p.mu.Unlock()
			go func() { _, _ = io.Copy(u, c); u.Close(); c.Close() }()
			go func() { _, _ = io.Copy(c, u); u.Close(); c.Close() }()
		}
	}()
}

func (p *tcpProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

func (p *tcpProxy) restore() {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.serve(ln)
}

// Criterion 4: an Ice outage — observers ice_unavailable, an established
// voice session keeps sending and receiving, no Mumble user created, the
// roster back after recovery. Then a murmur restart, which ends the same
// way with the registration made again.
func TestMurmurIceOutage(t *testing.T) {
	e := needMurmurIce(t)
	e.callbacksSafe(t)
	su, parentName, parent := superuser(t, e.addr, e.supw)
	px := startProxy(t, e.ice)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, parentName, e.withIce(px.addr, 5*time.Second))
	code := uniq("room-")
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: code, Temporary: true})
	if err != nil {
		t.Fatal(err)
	}
	b := joinVoice(t, tb, code, uniq("a"), 0)
	fr.nextLink(t)
	st := b.waitState(connected)
	ch := suChild(t, su, parent, tb.tempChannel(code, ""))
	native := murmurDial(t, e.addr, uniq("native-"), "")
	if err := native.JoinChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, st.Name) && rosterHas(m, native.Self().Name) })
	usersBefore := len(su.Users())
	px.cut()
	ol.waitRoster(t, func(m voice.Message) bool { return !m.Available && m.Reason == failureText[ReasonIceUnavailable] })
	// Voice both ways during the outage.
	b.sendFrame(7, []byte("during-outage"), true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	p, err := native.Audio().Pop(ctx)
	cancel()
	if err != nil || string(p.Opus) != "during-outage" {
		t.Fatalf("native heard %+v %v during the outage", p, err)
	}
	if _, err := native.WriteAudio(9, []byte("back-at-you"), true); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-b.frames:
		if _, payload, err := wire.ParseVoiceFrame(d); err != nil || string(payload) != "back-at-you" {
			t.Fatalf("browser got %q %v", payload, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the browser heard nothing during the outage")
	}
	if n := len(su.Users()); n != usersBefore {
		t.Fatalf("users %d → %d during the outage: no Mumble user may appear (OD6)", usersBefore, n)
	}
	observerNameSeen(t, su)
	px.restore()
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available && rosterHas(m, native.Self().Name) })
	if tb.Status().MumbleFailures[ReasonIceUnavailable] == 0 {
		t.Fatal("the outage was not counted")
	}
	// A native change makes an event, which answers the reconnect's
	// registration (D15).
	_ = native.SetSelf(true, false)
	waitIce(t, tb, "an event after the outage", func(s *IceStatus) bool { return s.Unanswered == 1 && s.Callbacks == cbRegistered })
	if os.Getenv("GAWK_TEST_MURMUR_RESTART") == "" {
		t.Log("GAWK_TEST_MURMUR_RESTART unset: skipping the murmur restart half")
		return
	}
	id := tb.ice.proxy.Object.Name
	before := strings.Count(e.log(t), "Added Ice ServerCallback "+id)
	if out, err := exec.Command("docker", "restart", "-t", "1", e.container).CombinedOutput(); err != nil {
		t.Fatalf("docker restart: %v %s", err, out)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return !m.Available })
	eventually2(t, 30*time.Second, "the registration made again", func() bool {
		return strings.Count(e.log(t), "Added Ice ServerCallback "+id) > before
	})
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	su2 := murmurDial(t, e.addr, "SuperUser", e.supw)
	observerNameSeen(t, su2)
}

func eventually2(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Criterion 5: a drain hands the room's watcher to the surviving pod, and
// the draining pod's removeCallback is in murmur's log before its listener
// closes.
func TestMurmurIceDrainHandover(t *testing.T) {
	e := needMurmurIce(t)
	e.callbacksSafe(t)
	su, _, _ := superuser(t, e.addr, e.supw)
	static := uniq("Static-")
	sid, err := su.CreateChannel(context.Background(), 0, static, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = su.RemoveChannel(sid) })
	fr := startRelay(t)
	podA := murmurBridge(t, fr, e.addr, "", e.withIce(e.ice, 5*time.Second))
	podB := murmurBridge(t, fr, e.addr, "", e.withIce(e.ice, 5*time.Second))
	code := uniq("room-")
	olA, _, err := dialObserver(t, podA.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: code, Channel: static})
	if err != nil {
		t.Fatal(err)
	}
	olA.waitRoster(t, func(m voice.Message) bool { return m.Available })
	idA := podA.ice.proxy.Object.Name
	if !strings.Contains(e.log(t), "Added Ice ServerCallback "+idA) {
		t.Fatal("pod A never registered")
	}
	podA.Drain()
	// Drain returned with the listener still bound: removeCallback first.
	if c, err := net.DialTimeout("tcp", podA.ice.callbackAddr().String(), time.Second); err != nil {
		t.Fatalf("the callback listener closed before the shutdown: %v", err)
	} else {
		c.Close()
	}
	if !strings.Contains(e.log(t), "Removed Ice ServerCallback "+idA) {
		t.Fatal("no removeCallback in murmur's log after the drain")
	}
	olB, _, err := dialObserver(t, podB.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: code, Channel: static})
	if err != nil {
		t.Fatal(err)
	}
	olB.waitRoster(t, func(m voice.Message) bool { return m.Available })
	n := murmurDial(t, e.addr, uniq("native-"), "")
	if err := n.JoinChannel(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	olB.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, n.Self().Name) })
}

func metricValue(t *testing.T, tb *testBridge, name string) string {
	t.Helper()
	_, body := tb.get(t, "/metrics")
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimPrefix(line, name+" ")
		}
	}
	return ""
}

// Criterion 7: silent loss repaired. One refused delivery makes murmur drop
// the registration (F13); the next mute reaches the roster within one
// reconciliation, counted; the one after arrives in callback time.
func TestMurmurIceSilentLossRepaired(t *testing.T) {
	e := needMurmurIce(t)
	e.callbacksSafe(t)
	su, _, _ := superuser(t, e.addr, e.supw)
	static := uniq("Static-")
	sid, err := su.CreateChannel(context.Background(), 0, static, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = su.RemoveChannel(sid) })
	native := murmurDial(t, e.addr, uniq("native-"), "")
	if err := native.JoinChannel(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, "", e.withIce(e.ice, 5*time.Second))
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: uniq("room-"), Channel: static})
	if err != nil {
		t.Fatal(err)
	}
	name := native.Self().Name
	muted := func(want bool) func(voice.Message) bool {
		return func(m voice.Message) bool {
			for _, u := range m.Users {
				if u.Name == name {
					return (u.Flags&wire.RoomVoiceUserSelfMute != 0) == want
				}
			}
			return false
		}
	}
	ol.waitRoster(t, muted(false))
	_ = native.SetSelf(true, false)
	ol.waitRoster(t, muted(true)) // events flow
	// Close the listener and murmur's connection to it for one event.
	tb.ice.closeEndpointForTest()
	other := murmurDial(t, e.addr, uniq("bump-"), "") // the refused event
	_ = other
	time.Sleep(500 * time.Millisecond)
	if err := tb.ice.reopenEndpointForTest(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = native.SetSelf(false, false)
	ol.waitRoster(t, muted(false))
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("the mute arrived in %s: the registration was not dropped", d)
	} else if d > 6*time.Second {
		t.Fatalf("the mute took %s, want within one 5 s reconciliation", d)
	}
	// The roster goes out as the mirror is replaced; the re-add follows.
	eventually(t, "drift counted", func() bool { return metricValue(t, tb, "gawk_bridge_ice_reconcile_drift_total") != "0" })
	eventually(t, "the re-add counted", func() bool {
		return metricValue(t, tb, `gawk_bridge_ice_registrations_total{result="ok"}`) == "2"
	})
	time.Sleep(200 * time.Millisecond)
	start = time.Now()
	_ = native.SetSelf(true, false)
	ol.waitRoster(t, muted(true))
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("the next mute took %s, want callback time", d)
	}
	waitIce(t, tb, "registered", func(s *IceStatus) bool { return s.Callbacks == cbRegistered })
}

// Criterion 8: against 1.6.870 the bridge sends no callback operation at
// all — start, a reconnect, a drain — reports unsafe, and follows changes
// at reconciliation pace.
func TestMurmurIceVersionGate(t *testing.T) {
	e := needMurmurIce(t)
	if e.version != "1.6.870" {
		t.Skip("the version gate runs against murmur 1.6.870")
	}
	su, _, _ := superuser(t, e.addr, e.supw)
	static := uniq("Static-")
	sid, err := su.CreateChannel(context.Background(), 0, static, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = su.RemoveChannel(sid) })
	px := startProxy(t, e.ice)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, "", e.withIce(px.addr, 5*time.Second))
	ol, _, err := dialObserver(t, tb.internal, testPSK, voice.Message{Type: voice.TypeObserve, Code: uniq("room-"), Channel: static})
	if err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	waitIce(t, tb, "unsafe", func(s *IceStatus) bool { return s.Callbacks == cbUnsafe && s.MurmurVersion == "1.6.870" })
	n := murmurDial(t, e.addr, uniq("native-"), "")
	start := time.Now()
	if err := n.JoinChannel(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	ol.waitRoster(t, func(m voice.Message) bool { return rosterHas(m, n.Self().Name) })
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("the join took %s at reconciliation pace", d)
	}
	px.cut()
	ol.waitRoster(t, func(m voice.Message) bool { return !m.Available })
	px.restore()
	ol.waitRoster(t, func(m voice.Message) bool { return m.Available })
	tb.Drain()
	id := tb.ice.proxy.Object.Name
	if log := e.log(t); strings.Contains(log, id) {
		t.Fatal("murmur's log shows a callback operation from the bridge on 1.6.870")
	}
}

// murmurLogTime parses "<W>2026-11-15 09:11:23.124 0 => …" (UTC).
var murmurLogLine = regexp.MustCompile(`^<.>(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3}) `)

func murmurLogTimeOf(t *testing.T, log, needle string) (time.Time, bool) {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, needle) {
			m := murmurLogLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			at, err := time.ParseInLocation("2006-01-02 15:04:05.000", m[1], time.UTC)
			if err == nil {
				return at, true
			}
		}
	}
	return time.Time{}, false
}

// Criterion 9: an unreachable pod stops costing murmur. A native client
// moves around a ring of 10 channels every 700 ms; a second client times
// each move.
func TestMurmurIceUnreachablePodStopsCostingMurmur(t *testing.T) {
	e := needMurmurIce(t)
	e.callbacksSafe(t)
	if testing.Short() {
		t.Skip("70 s")
	}
	if raceEnabled {
		t.Skip("a freeze-timing test: run it without -race")
	}
	su, _, _ := superuser(t, e.addr, e.supw)
	ctx := context.Background()
	ringParent, err := su.CreateChannel(ctx, 0, uniq("Ring-"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = su.RemoveChannel(ringParent) })
	var ring []uint32
	for i := range 10 {
		id, err := su.CreateChannel(ctx, ringParent, fmt.Sprintf("r%d", i), false)
		if err != nil {
			t.Fatal(err)
		}
		ring = append(ring, id)
	}
	mover := murmurDial(t, e.addr, uniq("mover-"), "")
	watcher := murmurDial(t, e.addr, uniq("timer-"), "")
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.addr, "", e.withIce(e.ice, 5*time.Second), func(o *Options) {
		o.Mumble.Ice.CallbackHost = "192.0.2.1" // TEST-NET-1: blackholes
		o.Mumble.Ice.CallbackProbe = 30 * time.Second
	})
	id := tb.ice.proxy.Object.Name

	type move struct {
		at time.Time
		d  time.Duration
	}
	var mu sync.Mutex
	var moves []move
	var gauge []string
	var unreachableAt time.Time
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the gauge and the state, sampled
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			v := metricValue(t, tb, "gawk_bridge_ice_registered")
			st := tb.Status().Ice
			mu.Lock()
			if len(gauge) == 0 || gauge[len(gauge)-1] != v {
				gauge = append(gauge, v)
			}
			if unreachableAt.IsZero() && st != nil && st.Callbacks == cbUnreachable {
				unreachableAt = time.Now()
			}
			mu.Unlock()
		}
	}()
	go func() { // the ring
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(700 * time.Millisecond):
			}
			target := ring[i%len(ring)]
			start := time.Now()
			if err := mover.Move(mover.Session(), target); err != nil {
				t.Error(err)
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			for {
				ch := watcher.Changed()
				if u, ok := watcher.User(mover.Session()); ok && u.Channel == target {
					break
				}
				select {
				case <-ch:
					continue
				case <-wctx.Done():
				}
				break
			}
			cancel()
			mu.Lock()
			moves = append(moves, move{at: start, d: time.Since(start)})
			mu.Unlock()
		}
	}()
	// Run at least 70 s, and until the probe has had a full interval.
	began := time.Now()
	deadline := began.Add(110 * time.Second)
	var probeAt time.Time
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		u := unreachableAt
		mu.Unlock()
		if u.IsZero() {
			continue
		}
		if probeAt.IsZero() {
			// The probe's "Added" is the third one.
			log := e.log(t)
			if strings.Count(log, "Added Ice ServerCallback "+id) >= 3 {
				var lines []string
				for _, l := range strings.Split(log, "\n") {
					if strings.Contains(l, "Added Ice ServerCallback "+id) {
						lines = append(lines, l)
					}
				}
				probeAt, _ = murmurLogTimeOf(t, lines[2], id)
			}
		}
		if !probeAt.IsZero() && time.Since(probeAt) > 12*time.Second && time.Since(began) > 70*time.Second {
			break
		}
	}
	close(stop)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if unreachableAt.IsZero() {
		t.Fatal("the pod never became unreachable")
	}
	if probeAt.IsZero() {
		t.Fatal("murmur's log shows no probe")
	}
	if gap := probeAt.Sub(unreachableAt); gap < 29*time.Second || gap > 37*time.Second {
		t.Fatalf("the probe came %s after unreachable, want 30–37 s", gap)
	}
	// A freeze is about twice the 100 ms callback timeout (F15: 0.22 s).
	// Moves are counted as freezes from 150 ms: the race detector and
	// murmur 1.6 put up to ~125 ms of noise on a move without any
	// registration (measured 2026-11-15), and 50 ms would count it.
	const freeze = 150 * time.Millisecond
	slowBefore, slowBetween, slowProbe, slowest := 0, 0, 0, time.Duration(0)
	for _, m := range moves {
		switch {
		case m.at.Before(unreachableAt):
			if m.d >= freeze {
				slowBefore++
			}
			slowest = max(slowest, m.d)
		case m.at.Before(probeAt.Add(-time.Second)):
			if m.d >= freeze {
				slowBetween++
			}
		default:
			if m.d >= freeze {
				slowProbe++
			}
		}
	}
	for _, m := range moves {
		if m.d > 50*time.Millisecond {
			t.Logf("slow move at %s (unreachable %s, probe %s): %s", m.at.UTC().Format("15:04:05.000"), unreachableAt.UTC().Format("15:04:05.000"), probeAt.UTC().Format("15:04:05.000"), m.d)
		}
	}
	if slowest < freeze {
		t.Fatalf("no move before unreachable took %s or more (slowest %s): 192.0.2.1 does not blackhole on this runner, so the test cannot measure F15 here", freeze, slowest)
	}
	if slowBefore > 2 || slowBetween > 0 || slowProbe > 1 {
		t.Fatalf("freezes: %d before unreachable (max 2), %d between it and the probe (max 0), %d in the probe's interval (max 1)", slowBefore, slowBetween, slowProbe)
	}
	if !strings.Contains(strings.Join(gauge, ""), "101") {
		t.Fatalf("gawk_bridge_ice_registered went %v, want 1 → 0 → 1", gauge)
	}
}

// closeEndpointForTest closes the callback listener and every connection
// murmur holds to it; reopenEndpointForTest binds the same port again, with
// the same identity (criterion 7).
func (m *iceMirror) closeEndpointForTest() { _ = m.ep.Close() }

func (m *iceMirror) reopenEndpointForTest() error {
	ep, err := murmurice.Listen(m.ep.Addr().String(), m.ep.Identity(), m.onEvent)
	if err != nil {
		return err
	}
	m.ep = ep
	return nil
}
