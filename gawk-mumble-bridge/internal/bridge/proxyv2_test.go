package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble/mumbletest"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The bytes the bridge sends are pinned by the vectors gawk-mmproxy decodes
// (docs/92 D2, PX2 criterion 5).
func TestProxyV2HeaderMatchesTheVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/proxyv2-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Encode []struct{ Name, Source, Hex string } `json:"encode"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Encode) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Encode {
		got := hex.EncodeToString(proxyV2Header(netip.MustParseAddrPort(c.Source)))
		if got != c.Hex {
			t.Errorf("%s: %s, want %s", c.Name, got, c.Hex)
		}
	}
}

func TestProxySource(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7:40001":          "203.0.113.7:40001",
		"[::ffff:203.0.113.7]:40001": "203.0.113.7:40001",
		"[fe80::1%eth0]:1":           "[fe80::1]:1",
		"[2001:db8::7]:40020":        "[2001:db8::7]:40020",
		"127.0.0.5:51000":            "127.0.0.5:51000",
		"":                           "",
		"not an address":             "",
		"0.0.0.0:1":                  "",
		"[::]:1":                     "",
		"224.0.0.1:1":                "",
	} {
		got, err := proxySource(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q: accepted as %v", in, got)
			} else if strings.Contains(err.Error(), in) && in != "" {
				t.Errorf("%q: the error carries the address: %v", in, err)
			}
			continue
		}
		if err != nil || got != netip.MustParseAddrPort(want) {
			t.Errorf("%q: %v %v, want %s", in, got, err, want)
		}
	}
}

// fakeMMProxy stands in for gawk-mmproxy: it decodes the bridge's header,
// records the source, then forwards to target, or ends the connection the
// way gawk-mmproxy does when murmur is unreachable (a FIN after a drain) or
// when murmur refuses the address (a reset).
type fakeMMProxy struct {
	addr   string
	target string
	mode   string // "forward", "fin", "reset"
	mu     sync.Mutex
	srcs   []netip.AddrPort
}

func startFakeMMProxy(t *testing.T, target, mode string) *fakeMMProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &fakeMMProxy{addr: ln.Addr().String(), target: target, mode: mode}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(c.(*net.TCPConn))
		}
	}()
	return p
}

func (p *fakeMMProxy) sources() []netip.AddrPort {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]netip.AddrPort(nil), p.srcs...)
}

func (p *fakeMMProxy) handle(c *net.TCPConn) {
	fixed := make([]byte, 16)
	if _, err := io.ReadFull(c, fixed); err != nil || !bytes.Equal(fixed[:12], proxyV2Signature) {
		c.Close()
		return
	}
	body := make([]byte, binary.BigEndian.Uint16(fixed[14:16]))
	if _, err := io.ReadFull(c, body); err != nil {
		c.Close()
		return
	}
	var src netip.AddrPort
	if fixed[13] == 0x11 {
		src = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:10]))
	} else {
		src = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[0:16])), binary.BigEndian.Uint16(body[32:34]))
	}
	p.mu.Lock()
	p.srcs = append(p.srcs, src)
	p.mu.Unlock()
	switch p.mode {
	case "reset":
		buf := make([]byte, 5)
		io.ReadFull(c, buf)
		c.SetLinger(0)
		c.Close()
		return
	case "fin":
		c.CloseWrite()
		c.SetReadDeadline(time.Now().Add(time.Second))
		io.Copy(io.Discard, c)
		c.Close()
		return
	}
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		c.Close()
		return
	}
	go func() { io.Copy(u, c); u.(*net.TCPConn).CloseWrite() }()
	io.Copy(c, u)
	c.Close()
	u.Close()
}

func proxyStack(t *testing.T, mode string, mod ...func(*Options)) (*testBridge, *fakeRelay, *mumbletest.Server, *fakeMMProxy) {
	t.Helper()
	m := mumbletest.Start(t)
	m.AddChannel(0, "Gawk")
	p := startFakeMMProxy(t, m.Addr(), mode)
	fr := startRelay(t)
	opts := baseOptions(t, fr.addr, m.Addr())
	opts.Mumble.ProxyAddr = p.addr
	for _, f := range mod {
		f(&opts)
	}
	return startBridge(t, opts), fr, m, p
}

// docs/92 D8: the header carries the voice session's own address (the one
// the relay's IP bans see in Register), and the session works as before.
func TestProxyModeSendsTheBrowsersAddress(t *testing.T) {
	tb, fr, _, p := proxyStack(t, "forward")
	b := joinVoice(t, tb, "room-a", "alice", 0)
	rs := fr.nextLink(t)
	rs.next(t, voice.TypeJoined)
	b.waitState(connected)
	srcs := p.sources()
	if len(srcs) != 1 {
		t.Fatalf("the proxy saw %d headers, want 1", len(srcs))
	}
	want, err := proxySource(rs.reg.Remote)
	if err != nil {
		t.Fatal(err)
	}
	if srcs[0] != want {
		t.Errorf("header source %v, want the session's remote %v", srcs[0], want)
	}
}

// The observer and Ice dial murmur directly; only voice users go through
// the proxy (docs/92 D8). TLS keeps verifying murmur under -mumble-addr's
// host.
func TestMumbleConfigInProxyMode(t *testing.T) {
	s := &Server{opts: Options{Mumble: MumbleOptions{Addr: "murmur.example:64738", ProxyAddr: "mmproxy.example:64739"}}}
	obs := s.mumbleConfig("gawkBridgeBot", netip.AddrPort{})
	if obs.Addr != "murmur.example:64738" || obs.Proxied || obs.DialContext != nil {
		t.Errorf("observer config %+v: want a direct dial", obs)
	}
	user := s.mumbleConfig("alice [web]", netip.MustParseAddrPort("203.0.113.7:1"))
	if user.Addr != "mmproxy.example:64739" || !user.Proxied || user.DialContext == nil || user.TLS.ServerName != "murmur.example" {
		t.Errorf("voice user config %+v", user)
	}
	s.opts.Mumble.ProxyAddr = ""
	if c := s.mumbleConfig("alice [web]", netip.MustParseAddrPort("203.0.113.7:1")); c.Addr != "murmur.example:64738" || c.Proxied {
		t.Errorf("without the knob %+v", c)
	}
}

// Fail closed (OD4): through the proxy, murmur unreachable (a FIN) is 4016
// murmur_unreachable and murmur's refusal (a reset) is 4016 autoban; a
// stopped proxy is murmur_unreachable too, and murmur sees nothing.
func TestProxyModeFailsClosed(t *testing.T) {
	for _, c := range []struct{ mode, reason string }{
		{"fin", ReasonUnreachable},
		{"reset", ReasonAutoban},
	} {
		tb, fr, m, _ := proxyStack(t, c.mode)
		b := joinVoice(t, tb, "room-a", "alice", 0)
		fr.nextLink(t)
		b.expectClose(wire.CloseCodeVoiceUnavailable)
		if st := tb.Status(); st.MumbleFailures[c.reason] != 1 {
			t.Errorf("%s: failures %+v, want one %s", c.mode, st.MumbleFailures, c.reason)
		}
		if n := len(m.Users()); n != 0 {
			t.Errorf("%s: murmur has %d users", c.mode, n)
		}
	}
	// The proxy not listening at all.
	m := mumbletest.Start(t)
	fr := startRelay(t)
	opts := baseOptions(t, fr.addr, m.Addr())
	opts.Mumble.ProxyAddr = "127.0.0.1:1"
	tb := startBridge(t, opts)
	b := joinVoice(t, tb, "room-a", "alice", 0)
	fr.nextLink(t)
	b.expectClose(wire.CloseCodeVoiceUnavailable)
	if st := tb.Status(); st.MumbleFailures[ReasonUnreachable] != 1 || len(m.Users()) != 0 {
		t.Errorf("stopped proxy: %+v, murmur users %d", st.MumbleFailures, len(m.Users()))
	}
}

// An address the header can't carry fails the join with proxy_source and
// dials nothing: never the pod's own address instead (OD4).
func TestProxySourceRefusalDialsNothing(t *testing.T) {
	tb, _, m, p := proxyStack(t, "forward")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vs := &voiceSession{s: tb.Server, remote: "garbage", ctx: ctx, cancel: cancel, ending: make(chan struct{})}
	_, _, _, err := vs.connect("alice", false, false, "room-a", voice.Message{})
	if failureReason(err) != ReasonProxySource {
		t.Fatalf("reason %q (%v), want %s", failureReason(err), err, ReasonProxySource)
	}
	if len(p.sources()) != 0 || len(m.Users()) != 0 {
		t.Errorf("dialled: proxy %v, murmur users %d", p.sources(), len(m.Users()))
	}
}

// docs/92 D12, in proxy mode with Ice: murmur ghosts a second login of one
// name from one address, and through the proxy that is one person on two
// pods. A pod with no live Mumble connection must still see the name in its
// Ice mirror and suffix, so the first session isn't ghosted. With the knob
// unset the registry never reads the mirror (G7).
func TestProxyModeNameCheckUsesTheIceMirror(t *testing.T) {
	m := mumbletest.Start(t)
	m.AddChannel(0, "Gawk")
	m.GhostSameName.Store(true)
	ic := m.StartIce(t, testIceSecret)
	p := startFakeMMProxy(t, m.Addr(), "forward")
	fr := startRelay(t)
	pod := func(proxy bool) *testBridge {
		o := baseOptions(t, fr.addr, m.Addr())
		o.Mumble.Ice = iceOptions(ic)
		if proxy {
			o.Mumble.ProxyAddr = p.addr
		}
		return startBridge(t, o)
	}
	a, b := pod(true), pod(true)
	if b.names.onServer == nil {
		t.Fatal("proxy mode with Ice: the registry doesn't consult the mirror")
	}
	if plain := pod(false); plain.names.onServer != nil {
		t.Fatal("without -mumble-proxy-addr the registry reads the mirror (G7)")
	}
	first := joinVoice(t, a, "room-a", "alice", 0)
	fr.nextLink(t).next(t, voice.TypeJoined)
	first.waitState(connected)
	eventually(t, "pod b's mirror to list alice", func() bool { return b.ice.hasName("alice [web]") })
	second := joinVoice(t, b, "room-a", "alice", 0)
	j := fr.nextLink(t).next(t, voice.TypeJoined)
	if j.Name != "alice [web] 2" {
		t.Errorf("the second session is %q, want the suffixed name", j.Name)
	}
	second.waitState(connected)
	time.Sleep(200 * time.Millisecond)
	if _, ok := m.UserByName("alice [web]"); !ok {
		t.Error("the first session was ghosted")
	}
}
