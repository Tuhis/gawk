package murmurice

// Integration tests against REAL murmur (docs/89 MI1 criteria 4–6). They run
// when GAWK_TEST_MURMUR_ICE is set; testdata/murmur.sh starts murmur with
// Ice and prints the environment:
//
//	eval "$(testdata/murmur.sh up 1.6)"   # or 1.5, 1.6.870
//	go test ./internal/murmurice -run Murmur

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
)

type iceEnv struct {
	ice, secret, readSecret, cbHost, murmur, supw, version, container string
}

func needIce(t *testing.T) iceEnv {
	t.Helper()
	e := iceEnv{
		ice:        os.Getenv("GAWK_TEST_MURMUR_ICE"),
		secret:     os.Getenv("GAWK_TEST_MURMUR_ICE_SECRET"),
		readSecret: os.Getenv("GAWK_TEST_MURMUR_ICE_READ_SECRET"),
		cbHost:     os.Getenv("GAWK_TEST_MURMUR_CALLBACK_HOST"),
		murmur:     os.Getenv("GAWK_TEST_MURMUR"),
		supw:       os.Getenv("GAWK_TEST_MURMUR_SUPERUSER_PASSWORD"),
		version:    os.Getenv("GAWK_TEST_MURMUR_VERSION"),
		container:  os.Getenv("GAWK_TEST_MURMUR_CONTAINER"),
	}
	if e.ice == "" {
		t.Skip("GAWK_TEST_MURMUR_ICE unset: see testdata/murmur.sh")
	}
	if e.secret == "" || e.cbHost == "" || e.murmur == "" {
		t.Fatal("GAWK_TEST_MURMUR_ICE_SECRET, _CALLBACK_HOST and GAWK_TEST_MURMUR are required with GAWK_TEST_MURMUR_ICE")
	}
	return e
}

// needCallbacks skips on 1.6.870, where registering a callback is what the
// bridge must never do (D16).
func (e iceEnv) needCallbacks(t *testing.T) {
	t.Helper()
	if e.version == "1.6.870" {
		t.Skip("murmur 1.6.870: callbacks are unsafe (D16)")
	}
}

func uniqName(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func iceClient(t *testing.T, e iceEnv, secret string) *Client {
	t.Helper()
	c := NewClient(e.ice, secret, 3*time.Second)
	t.Cleanup(func() { c.Close() })
	return c
}

func booted(t *testing.T, c *Client) int {
	t.Helper()
	ids, err := c.BootedServers(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("booted servers %v %v", ids, err)
	}
	return ids[0]
}

func native(t *testing.T, e iceEnv, name, pw string) *mumble.Client {
	t.Helper()
	c, err := mumble.Dial(context.Background(), mumble.Config{Addr: e.murmur, Username: name, Password: pw, TLS: mumble.TLSOptions{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatalf("dial murmur as %s: %v", name, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// recorder is a callback endpoint on the docker gateway with its events.
type recorder struct {
	*Endpoint
	mu     sync.Mutex
	events []Event
	errs   []error
	ch     chan Event
}

func listenRecorder(t *testing.T, e iceEnv, addr string, id Identity) *recorder {
	t.Helper()
	r := &recorder{ch: make(chan Event, 4096)}
	if addr == "" {
		addr = net.JoinHostPort(e.cbHost, "0")
	}
	ep, err := Listen(addr, id, func(ev Event, err error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if err != nil {
			r.errs = append(r.errs, err)
			return
		}
		r.events = append(r.events, ev)
		r.ch <- ev
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Endpoint = ep
	t.Cleanup(func() { ep.Close() })
	return r
}

func (r *recorder) proxy(host string) Proxy {
	return Proxy{Object: r.Identity(), Host: host, Port: r.Addr().Port, Timeout: 100 * time.Millisecond}
}

// wait waits for an event matching cond.
func (r *recorder) wait(t *testing.T, what string, d time.Duration, cond func(Event) bool) Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev := <-r.ch:
			if cond(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event within %s", what, d)
			return Event{}
		}
	}
}

// quiet asserts no event matching cond arrives within d.
func (r *recorder) quiet(t *testing.T, what string, d time.Duration, cond func(Event) bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev := <-r.ch:
			if cond(ev) {
				t.Fatalf("unexpected %s event %+v", what, ev.Op)
			}
		case <-deadline:
			return
		}
	}
}

func userNamed(name string) func(Event) bool {
	return func(ev Event) bool { return strings.HasPrefix(ev.Op, "user") && ev.User.Name == name }
}

func TestMurmurDiscoveryAndVersion(t *testing.T) {
	e := needIce(t)
	c := iceClient(t, e, e.secret)
	sid := booted(t, c)
	want := map[string]int{"1.5": 1, "1.6": 0, "1.6.870": 0}[e.version]
	if sid != want {
		t.Fatalf("booted server %d on %s, want %d (F8)", sid, e.version, want)
	}
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantV := map[string]string{"1.5": "1.5.939", "1.6": "1.6.1001", "1.6.870": "1.6.870"}[e.version]
	if v.String() != wantV {
		t.Fatalf("version %s, want %s", v, wantV)
	}
	if v.CallbacksUnsafe() != (e.version == "1.6.870") {
		t.Fatalf("%s: CallbacksUnsafe %v", v, v.CallbacksUnsafe())
	}
}

func TestMurmurSecrets(t *testing.T) {
	e := needIce(t)
	for _, secret := range []string{"", "wrong-secret"} {
		c := iceClient(t, e, secret)
		if _, err := c.BootedServers(context.Background()); !IsUserException(err, ExInvalidSecret) {
			t.Fatalf("secret %q: getBootedServers %v", secret, err)
		}
	}
	if e.readSecret == "" {
		return
	}
	// The read secret reads, but cannot register a callback (F4).
	rc := iceClient(t, e, e.readSecret)
	sid := booted(t, rc)
	if _, err := rc.Users(context.Background(), sid); err != nil {
		t.Fatalf("getUsers with the read secret: %v", err)
	}
	if _, err := rc.Channels(context.Background(), sid); err != nil {
		t.Fatalf("getChannels with the read secret: %v", err)
	}
	if e.version == "1.6.870" {
		return // never register there, not even to be refused
	}
	rec := listenRecorder(t, e, "", NewIdentity())
	if err := rc.AddCallback(context.Background(), sid, rec.proxy(e.cbHost)); !IsUserException(err, ExInvalidSecret) {
		t.Fatalf("addCallback with the read secret: %v", err)
	}
}

// 50 users, each with a name, a mute, a deafen and a channel checked.
func TestMurmurUsersAndChannelsDecode(t *testing.T) {
	e := needIce(t)
	c := iceClient(t, e, e.secret)
	sid := booted(t, c)
	su := native(t, e, "SuperUser", e.supw)
	ctx := context.Background()
	parent, err := su.CreateChannel(ctx, 0, uniqName("Ice-"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = su.RemoveChannel(parent) })
	sub, err := su.CreateChannel(ctx, parent, "sub", false)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		ch         uint32
		mute, deaf bool
	}
	wants := map[string]want{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("%s-%02d", uniqName("u"), i)
			nc := native(t, e, name, "")
			w := want{ch: 0, mute: i%3 == 0, deaf: i%5 == 0}
			if i%2 == 0 {
				w.ch = sub
				if err := nc.JoinChannel(ctx, sub); err != nil {
					t.Error(err)
					return
				}
			}
			if w.mute || w.deaf {
				if err := nc.SetSelf(w.mute || w.deaf, w.deaf); err != nil {
					t.Error(err)
				}
				w.mute = w.mute || w.deaf // a self-deafen self-mutes too
			}
			mu.Lock()
			wants[name] = w
			mu.Unlock()
		}()
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond)
	users, err := c.Users(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, u := range users {
		w, ok := wants[u.Name]
		if !ok {
			continue
		}
		seen++
		if u.Channel != w.ch || u.SelfMute != w.mute || u.SelfDeaf != w.deaf || u.UserID != -1 {
			t.Errorf("user %s: %+v, want %+v", u.Name, u, w)
		}
	}
	if seen != 50 {
		t.Fatalf("saw %d of 50 users", seen)
	}
	chans, err := c.Channels(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ch := range chans {
		if ch.ID == sub {
			found = ch.Parent == parent && ch.Name == "sub" && !ch.Temporary
		}
	}
	if !found {
		t.Fatalf("channel sub not in %+v", chans)
	}
}

// Each of the seven events arrives and decodes.
func TestMurmurCallbackEvents(t *testing.T) {
	e := needIce(t)
	e.needCallbacks(t)
	c := iceClient(t, e, e.secret)
	sid := booted(t, c)
	rec := listenRecorder(t, e, "", NewIdentity())
	p := rec.proxy(e.cbHost)
	if err := c.AddCallback(context.Background(), sid, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.RemoveCallback(context.Background(), sid, p) })
	ctx := context.Background()
	su := native(t, e, "SuperUser", e.supw)
	rec.wait(t, "userConnected", 3*time.Second, func(ev Event) bool { return ev.Op == OpUserConnected && ev.User.Name == "SuperUser" })
	name := uniqName("Ev-")
	id, err := su.CreateChannel(ctx, 0, name, false)
	if err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "channelCreated", 3*time.Second, func(ev Event) bool {
		return ev.Op == OpChannelCreated && ev.Channel.ID == id && ev.Channel.Name == name
	})
	if err := su.JoinChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "userStateChanged", 3*time.Second, func(ev Event) bool { return ev.Op == OpUserStateChanged && ev.User.Channel == id })
	u := native(t, e, uniqName("tm-"), "")
	if err := u.JoinChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := u.SendText(id, "hello"); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "userTextMessage", 3*time.Second, func(ev Event) bool { return ev.Op == OpUserTextMessage && ev.User.Session == u.Session() })
	if err := su.SetChannelDescription(id, "renamed"); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "channelStateChanged", 3*time.Second, func(ev Event) bool { return ev.Op == OpChannelStateChanged && ev.Channel.ID == id })
	u.Close()
	rec.wait(t, "userDisconnected", 3*time.Second, func(ev Event) bool { return ev.Op == OpUserDisconnected && ev.User.Session != su.Session() })
	if err := su.RemoveChannel(id); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "channelRemoved", 3*time.Second, func(ev Event) bool { return ev.Op == OpChannelRemoved && ev.Channel.ID == id })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.errs) > 0 {
		t.Fatalf("decode errors: %v", rec.errs)
	}
}

// F13 and F14 kept as tests.
func TestMurmurRegistrationLossAndRepair(t *testing.T) {
	e := needIce(t)
	e.needCallbacks(t)
	c := iceClient(t, e, e.secret)
	sid := booted(t, c)
	ctx := context.Background()
	rec := listenRecorder(t, e, "", NewIdentity())
	p := rec.proxy(e.cbHost)
	if err := c.AddCallback(ctx, sid, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.RemoveCallback(context.Background(), sid, p) })
	// Re-adding while registered does not duplicate deliveries.
	if err := c.AddCallback(ctx, sid, p); err != nil {
		t.Fatal(err)
	}
	n1 := uniqName("dup-")
	native(t, e, n1, "")
	rec.wait(t, "connect", 3*time.Second, func(ev Event) bool { return ev.Op == OpUserConnected && ev.User.Name == n1 })
	rec.quiet(t, "duplicate", 500*time.Millisecond, func(ev Event) bool { return ev.Op == OpUserConnected && ev.User.Name == n1 })

	// Two endpoints with one identity are independent (F14).
	twin := listenRecorder(t, e, "", rec.Identity())
	tp := twin.proxy(e.cbHost)
	if err := c.AddCallback(ctx, sid, tp); err != nil {
		t.Fatal(err)
	}
	twin.Close()
	n2 := uniqName("twin-")
	native(t, e, n2, "")
	rec.wait(t, "connect after the twin failed", 3*time.Second, func(ev Event) bool { return ev.Op == OpUserConnected && ev.User.Name == n2 })

	// One refused delivery removes the registration for good (F13).
	port := rec.Addr().Port
	rec.Close()
	n3 := uniqName("lost-")
	native(t, e, n3, "")
	time.Sleep(300 * time.Millisecond)
	back := listenRecorder(t, e, net.JoinHostPort(e.cbHost, fmt.Sprint(port)), rec.Identity())
	n4 := uniqName("after-")
	native(t, e, n4, "")
	back.quiet(t, "delivery to a dropped registration", time.Second, userNamed(n4))
	// Re-adding restores it.
	if err := c.AddCallback(ctx, sid, p); err != nil {
		t.Fatal(err)
	}
	n5 := uniqName("repaired-")
	native(t, e, n5, "")
	back.wait(t, "connect after re-adding", 3*time.Second, userNamed(n5))
}

// F15 kept as a test (G7): a registration pointing at an unroutable address,
// with the default 100 ms endpoint timeout, freezes murmur once, briefly.
func TestMurmurBlackholedCallbackFreezesOnceBriefly(t *testing.T) {
	e := needIce(t)
	e.needCallbacks(t)
	if raceEnabled {
		t.Skip("a freeze-timing test: run it without -race")
	}
	c := iceClient(t, e, e.secret)
	sid := booted(t, c)
	ctx := context.Background()
	watcher := native(t, e, uniqName("watch-"), "")
	actor := native(t, e, uniqName("act-"), "")
	toggle := func(mute bool) time.Duration {
		start := time.Now()
		if err := actor.SetSelf(mute, false); err != nil {
			t.Fatal(err)
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		for {
			ch := watcher.Changed()
			if u, ok := watcher.User(actor.Session()); ok && u.SelfMute == mute {
				return time.Since(start)
			}
			select {
			case <-ch:
			case <-wctx.Done():
				t.Fatal("the watcher never saw the toggle")
			}
		}
	}
	toggle(true) // warm up
	dead := Proxy{Object: NewIdentity(), Host: "192.0.2.1", Port: 9, Timeout: 100 * time.Millisecond}
	if err := c.AddCallback(ctx, sid, dead); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.RemoveCallback(context.Background(), sid, dead) })
	first := toggle(false)
	if first < 150*time.Millisecond {
		t.Fatalf("the first toggle took %s: 192.0.2.1 does not blackhole on this runner (it answered at once), so F15 cannot be measured here", first)
	}
	if first > 500*time.Millisecond {
		t.Fatalf("the first toggle took %s, want under 0.5 s (F15)", first)
	}
	if next := toggle(true); next > 50*time.Millisecond {
		t.Fatalf("the next toggle took %s, want under 50 ms: murmur did not drop the dead registration", next)
	}
}

// murmurLog is the container's log (for "Added/Removed Ice ServerCallback").
func murmurLog(t *testing.T, e iceEnv) string {
	t.Helper()
	if e.container == "" {
		t.Skip("GAWK_TEST_MURMUR_CONTAINER unset")
	}
	out, err := exec.Command("docker", "logs", e.container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	return string(out)
}

var _ = errors.New
