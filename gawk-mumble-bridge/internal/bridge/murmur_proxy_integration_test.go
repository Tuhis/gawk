package bridge

// R88 (docs/92 PX2 criterion 3): the bridge in proxy mode against REAL
// murmur behind the REAL gawk-mmproxy, in the pod layout of
// gawk-mmproxy/testdata/topology.sh. testdata/murmur.sh starts it as the
// `proxied` server and exports GAWK_TEST_MURMUR_PROXY*; without them these
// skip.
//
// Each test browser dials the bridge from its own loopback address
// (127.x.y.z), so the bridge sees distinct remotes, and through the proxy
// murmur does too: that is what makes a Mumble ban cover one person.

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

type proxiedEnv struct {
	proxy, murmur, container, supw string
}

func needProxiedMurmur(t *testing.T) proxiedEnv {
	t.Helper()
	e := proxiedEnv{
		proxy:     os.Getenv("GAWK_TEST_MURMUR_PROXY"),
		murmur:    os.Getenv("GAWK_TEST_MURMUR_PROXY_MURMUR"),
		container: os.Getenv("GAWK_TEST_MURMUR_PROXY_CONTAINER"),
		supw:      os.Getenv("GAWK_TEST_MURMUR_SUPERUSER_PASSWORD"),
	}
	if e.proxy == "" {
		t.Skip("GAWK_TEST_MURMUR_PROXY unset: see testdata/murmur.sh (the `proxied` server)")
	}
	return e
}

func (e proxiedEnv) sawConnection(t *testing.T, addr string) bool {
	t.Helper()
	out, _ := exec.Command("docker", "logs", e.container).CombinedOutput()
	return strings.Contains(string(out), "New connection: "+addr+":")
}

// loopback is a fresh 127.x.y.z per call: murmur keeps server bans in its
// database, so a fixed address banned by one run would stay banned.
func loopback() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("127.%d.%d.%d", b[0]|1, b[1], b[2]|1)
}

func joinFrom(t *testing.T, tb *testBridge, local, code, nick string) *browser {
	t.Helper()
	b, err := dialBrowserFrom(t, tb.public, testOrigin, local)
	if err != nil {
		t.Fatal(err)
	}
	b.hello(mintTicket(t, code, nick, time.Now().Add(voice.TicketTTL)), 0)
	return b
}

func TestMurmurProxiedBanCoversOnePerson(t *testing.T) {
	e := needProxiedMurmur(t)
	su, parentName, _ := superuser(t, e.murmur, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.murmur, parentName, func(o *Options) { o.Mumble.ProxyAddr = e.proxy })
	code := uniq("room-")
	aliceNick, bobNick := uniq("a"), uniq("b")
	aliceAddr, bobAddr := loopback(), loopback()

	alice := joinFrom(t, tb, aliceAddr, code, aliceNick)
	fr.nextLink(t).next(t, voice.TypeJoined)
	alice.waitState(connected)
	bob := joinFrom(t, tb, bobAddr, code, bobNick)
	fr.nextLink(t).next(t, voice.TypeJoined)
	bob.waitState(connected)

	// G1: murmur sees each browser at its own address, not the pod's.
	for _, a := range []string{aliceAddr, bobAddr} {
		if !e.sawConnection(t, a) {
			t.Errorf("murmur never logged a connection from %s", a)
		}
	}

	// G2: a kickban of alice ends her voice with 4015 and refuses her
	// rejoin; bob, on the same pod, stays and can rejoin.
	var aliceSess uint32
	eventually(t, "the SuperUser to see alice", func() bool {
		for _, u := range su.Users() {
			if u.Name == aliceNick+" [web]" {
				aliceSess = u.Session
				return true
			}
		}
		return false
	})
	if err := su.Kick(aliceSess, "R88 test", true); err != nil {
		t.Fatal(err)
	}
	alice.expectClose(wire.CloseCodeVoiceRemoved)
	time.Sleep(500 * time.Millisecond)
	bobThere := false
	for _, u := range su.Users() {
		bobThere = bobThere || u.Name == bobNick+" [web]"
	}
	if !bobThere {
		t.Fatal("bob lost his Mumble connection when alice was banned")
	}
	select {
	case code := <-bob.notices:
		t.Fatalf("bob got a close notice %d when alice was banned", code)
	default:
	}

	again := joinFrom(t, tb, aliceAddr, code, aliceNick)
	fr.nextLink(t)
	again.expectClose(wire.CloseCodeVoiceUnavailable)
	if st := tb.Status(); st.MumbleFailures[ReasonAutoban] < 1 {
		t.Errorf("alice's refused rejoin: failures %+v, want autoban (murmur's reset through the proxy)", st.MumbleFailures)
	}

	bob2 := joinFrom(t, tb, bobAddr, code, bobNick+"x")
	fr.nextLink(t).next(t, voice.TypeJoined)
	bob2.waitState(connected)
}

// G3: a native client and a proxied voice user in one channel hear each
// other, both ways.
func TestMurmurProxiedAudioBothWays(t *testing.T) {
	e := needProxiedMurmur(t)
	_, parentName, parent := superuser(t, e.murmur, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.murmur, parentName, func(o *Options) { o.Mumble.ProxyAddr = e.proxy })
	code := uniq("room-")
	b := joinFrom(t, tb, loopback(), code, uniq("a"))
	fr.nextLink(t)
	st := b.waitState(connected)
	ch, ok := murmurDial(t, e.murmur, uniq("look-"), "").FindChild(parent, tb.tempChannel(code, ""))
	if !ok {
		t.Fatal("no channel")
	}
	native := murmurDial(t, e.murmur, uniq("native-"), "")
	if err := native.JoinChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	frames := make([][]byte, 20)
	for i := range frames {
		frames[i] = []byte(fmt.Sprintf("opus-%02d-\xfc\x00\xff", i))
	}
	for i, f := range frames {
		b.sendFrame(uint32(1000+i), f, i == len(frames)-1)
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for got := 0; got < len(frames); got++ {
		p, err := native.Audio().Pop(ctx)
		i := int(p.Seq) - 1000
		if err != nil || i < 0 || i >= len(frames) || p.Session != st.Session || !bytes.Equal(p.Opus, frames[i]) {
			t.Fatalf("native heard %d of %d frames (%v)", got, len(frames), err)
		}
	}
	for i, f := range frames {
		if _, err := native.WriteAudio(int64(500+i), f, i == len(frames)-1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline := time.After(5 * time.Second)
	for got := 0; got < len(frames); {
		select {
		case d := <-b.frames:
			h, payload, err := wire.ParseVoiceFrame(d)
			i := int(h.Seq) - 500
			if err != nil || i < 0 || i >= len(frames) || h.Speaker != native.Session() || !bytes.Equal(payload, frames[i]) {
				t.Fatalf("browser got %+v %q %v", h, payload, err)
			}
			got++
		case <-deadline:
			t.Fatalf("browser heard %d of %d frames", got, len(frames))
		}
	}
}

// OD4: with murmur stopped behind a running proxy, a join fails with 4016
// murmur_unreachable, not autoban (D6).
func TestMurmurProxiedMurmurDownIsUnreachable(t *testing.T) {
	e := needProxiedMurmur(t)
	_, parentName, _ := superuser(t, e.murmur, e.supw)
	fr := startRelay(t)
	tb := murmurBridge(t, fr, e.murmur, parentName, func(o *Options) { o.Mumble.ProxyAddr = e.proxy })
	if out, err := exec.Command("docker", "stop", e.container).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v %s", err, out)
	}
	defer func() {
		_ = exec.Command("docker", "start", e.container).Run()
		eventually(t, "murmur back", func() bool {
			c, err := mumble.Dial(context.Background(), mumble.Config{Addr: e.murmur, Username: uniq("probe-"), TLS: mumble.TLSOptions{InsecureSkipVerify: true}})
			if err == nil {
				c.Close()
			}
			return err == nil
		})
		// The SuperUser password is in murmur's database: it survives.
	}()
	b := joinFrom(t, tb, loopback(), uniq("room-"), uniq("a"))
	fr.nextLink(t)
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	if st := tb.Status(); st.MumbleFailures[ReasonUnreachable] != 1 || st.MumbleFailures[ReasonAutoban] != 0 {
		t.Errorf("failures %+v, want one murmur_unreachable and no autoban", st.MumbleFailures)
	}
}
