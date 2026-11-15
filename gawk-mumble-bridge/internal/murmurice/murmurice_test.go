package murmurice

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/murmurice/icetest"
)

const testSecret = "write-secret"

// fakeMurmur answers the six operations from fixed state.
type fakeMurmur struct {
	mu       sync.Mutex
	users    []icetest.User
	chans    []icetest.Channel
	sid      int32
	reqs     []icetest.Request
	override func(icetest.Request) (icetest.Reply, bool)
}

func (f *fakeMurmur) handle(r icetest.Request) icetest.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.override != nil {
		if rep, ok := f.override(r); ok {
			return rep
		}
	}
	if r.Context["secret"] != testSecret {
		return icetest.Reply{Status: 1, Result: icetest.Exception("::MumbleServer::InvalidSecretException")}
	}
	var w icetest.W
	switch r.Operation {
	case "getBootedServers":
		w.Size(1)
		w.Ident("s", strconv.Itoa(int(f.sid)))
		w.Size(0)
		w.U8(0)
		w.Bool(false)
		w.B = append(w.B, 1, 0, 1, 1)
		w.Size(1)
		w.I16(1)
		var ep icetest.W
		ep.Str("127.0.0.1")
		ep.I32(6502)
		ep.I32(-1)
		ep.Bool(false)
		w.Encaps(ep.B)
	case "getVersion":
		w.I32(1)
		w.I32(6)
		w.I32(1001)
		w.Str("1.6.1001")
	case "getUsers":
		w.B = icetest.UserMap(f.users)
	case "getChannels":
		w.B = icetest.ChannelMap(f.chans)
	case "addCallback", "removeCallback":
	default:
		return icetest.Reply{Status: 4}
	}
	return icetest.Reply{Result: w.B}
}

func (f *fakeMurmur) requests() []icetest.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]icetest.Request(nil), f.reqs...)
}

func startFake(t *testing.T, f *fakeMurmur) *icetest.Server {
	t.Helper()
	s, err := icetest.NewServer(f.handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func fullUser(session int32, name string) icetest.User {
	return icetest.User{Session: session, UserID: -1, Name: name, Channel: 3, Mute: true, SelfDeaf: true,
		OnlineSecs: 10, Version2: 1 << 48, Release: "1.5", OS: "Linux", OSVersion: "6",
		Ident: "the-identity", Ctx: "the-context", Comment: "the-comment", Addr: net.ParseIP("198.51.100.7").To16(),
		IdleSecs: 3, UDPPing: 1.5, TCPPing: 2.5}
}

func TestClientDecodesUsersChannelsVersion(t *testing.T) {
	long := strings.Repeat("ä", 200) // 400 bytes: a size of 255 and more
	f := &fakeMurmur{sid: 0,
		users: []icetest.User{fullUser(7, "alice [web]"), fullUser(3, long)},
		chans: []icetest.Channel{{ID: 0, Name: "Root", Parent: -1}, {ID: 3, Name: "Talk", Parent: 0, Links: []int32{0}, Description: strings.Repeat("d", 300), Temporary: true, Position: 2}},
	}
	// 300 users: the dictionary's own size goes past 254 too.
	for i := int32(100); i < 400; i++ {
		f.users = append(f.users, fullUser(i, "u"+strconv.Itoa(int(i))))
	}
	s := startFake(t, f)
	c := NewClient(s.Addr(), testSecret, time.Second)
	defer c.Close()
	ctx := context.Background()
	ids, err := c.BootedServers(ctx)
	if err != nil || len(ids) != 1 || ids[0] != 0 {
		t.Fatalf("booted %v %v", ids, err)
	}
	v, err := c.Version(ctx)
	if err != nil || v != (Version{1, 6, 1001}) || v.CallbacksUnsafe() {
		t.Fatalf("version %v %v", v, err)
	}
	users, err := c.Users(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 302 || users[0].Session != 3 || users[0].Name != long || users[1].Session != 7 {
		t.Fatalf("users %d %+v", len(users), users[:2])
	}
	want := User{Session: 7, UserID: -1, Name: "alice [web]", Channel: 3, Mute: true, SelfDeaf: true}
	if users[1] != want {
		t.Fatalf("user %+v, want %+v", users[1], want)
	}
	chans, err := c.Channels(ctx, 0)
	if err != nil || len(chans) != 2 || chans[1] != (Channel{ID: 3, Parent: 0, Name: "Talk", Temporary: true}) {
		t.Fatalf("channels %+v %v", chans, err)
	}
	// The secret rides in the request context of every call.
	for _, r := range f.requests() {
		if r.Context["secret"] != testSecret {
			t.Fatalf("%s without the secret", r.Operation)
		}
	}
}

func TestVersionGate(t *testing.T) {
	for v, unsafe := range map[string]bool{"1.5.939": false, "1.6.0": true, "1.6.870": true, "1.6.871": false, "1.6.1001": false, "1.7.0": false} {
		pv, err := ParseVersion(v)
		if err != nil || pv.CallbacksUnsafe() != unsafe {
			t.Errorf("%s: unsafe %v, want %v (%v)", v, pv.CallbacksUnsafe(), unsafe, err)
		}
	}
}

// F9: 1.5 derives InvalidSecretException from MurmurException, 1.6 from
// ServerException. The most derived type ID is what matches.
func TestUserExceptionsBothHierarchies(t *testing.T) {
	for _, base := range []string{"::MumbleServer::MurmurException", "::MumbleServer::ServerException"} {
		f := &fakeMurmur{override: func(r icetest.Request) (icetest.Reply, bool) {
			var w icetest.W
			w.U8(0x01) // first slice: type ID string, not last
			w.Str(ExInvalidSecret)
			w.U8(0x01 | 0x20)
			w.Str(base)
			return icetest.Reply{Status: 1, Result: w.B}, true
		}}
		s := startFake(t, f)
		c := NewClient(s.Addr(), "", time.Second)
		_, err := c.Version(context.Background())
		if !IsUserException(err, ExInvalidSecret) || IsUserException(err, base) {
			t.Fatalf("%s: %v", base, err)
		}
		c.Close()
	}
}

func TestRepliesThatEndEarlyOrRunLong(t *testing.T) {
	good := icetest.UserMap([]icetest.User{fullUser(1, "a")})
	for name, result := range map[string][]byte{
		"short":    good[:len(good)-3],
		"long":     append(append([]byte(nil), good...), 0),
		"bad size": {200},
	} {
		f := &fakeMurmur{override: func(r icetest.Request) (icetest.Reply, bool) {
			return icetest.Reply{Result: result}, r.Operation == "getUsers"
		}}
		s := startFake(t, f)
		c := NewClient(s.Addr(), testSecret, time.Second)
		_, err := c.Users(context.Background(), 1)
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: %v, want a ProtocolError", name, err)
		}
		// The connection stays usable: the framing was fine.
		if _, err := c.Version(context.Background()); err != nil {
			t.Fatalf("%s: next call %v", name, err)
		}
		c.Close()
	}
	// An encapsulation that lies about its size.
	f := &fakeMurmur{override: func(r icetest.Request) (icetest.Reply, bool) {
		return icetest.Reply{Raw: []byte{100, 0, 0, 0, 1, 1, 0}}, true
	}}
	s := startFake(t, f)
	c := NewClient(s.Addr(), testSecret, time.Second)
	defer c.Close()
	var pe *ProtocolError
	if _, err := c.Version(context.Background()); !errors.As(err, &pe) {
		t.Fatalf("lying encapsulation: %v", err)
	}
}

func TestRequestFailedAndUnknown(t *testing.T) {
	f := &fakeMurmur{override: func(r icetest.Request) (icetest.Reply, bool) {
		if r.Operation == "getVersion" {
			return icetest.Reply{Status: 2}, true
		}
		return icetest.Reply{Status: 5}, true
	}}
	s := startFake(t, f)
	c := NewClient(s.Addr(), testSecret, time.Second)
	defer c.Close()
	if _, err := c.Version(context.Background()); !errors.Is(err, ErrObjectNotExist) {
		t.Fatalf("object not exist: %v", err)
	}
	var ue *UnknownError
	if _, err := c.Users(context.Background(), 1); !errors.As(err, &ue) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestReconnectAfterServerCloses(t *testing.T) {
	f := &fakeMurmur{sid: 1}
	s := startFake(t, f)
	c := NewClient(s.Addr(), testSecret, time.Second)
	defer c.Close()
	ctx := context.Background()
	gen, err := c.Connect(ctx)
	if err != nil || gen != 1 {
		t.Fatalf("connect %d %v", gen, err)
	}
	lost := c.Lost()
	s.CloseConns()
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("Lost not closed")
	}
	if _, err := c.Version(ctx); err != nil {
		t.Fatalf("after the close: %v", err)
	}
	if c.Generation() != 2 {
		t.Fatalf("generation %d", c.Generation())
	}
}

// An idempotent call whose connection dies under it is retried once on a
// new one; a non-idempotent one is not.
func TestRetryOnlyIdempotentCalls(t *testing.T) {
	var s *icetest.Server
	var mu sync.Mutex
	drops := map[string]int{}
	f := &fakeMurmur{}
	f.override = func(r icetest.Request) (icetest.Reply, bool) {
		mu.Lock()
		defer mu.Unlock()
		if drops[r.Operation] == 0 {
			drops[r.Operation]++
			go s.CloseConns()
			return icetest.Reply{Drop: true}, true
		}
		return icetest.Reply{}, false
	}
	s = startFake(t, f)
	c := NewClient(s.Addr(), testSecret, time.Second)
	defer c.Close()
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("idempotent call not retried: %v", err)
	}
	err := c.AddCallback(context.Background(), 0, Proxy{Object: NewIdentity(), Host: "h", Port: 1, Timeout: 100 * time.Millisecond})
	if !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("addCallback: %v, want ErrConnectionLost (no retry)", err)
	}
	n := 0
	for _, r := range f.requests() {
		if r.Operation == "addCallback" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("addCallback sent %d times", n)
	}
}

func TestCallOutlivingItsDeadline(t *testing.T) {
	f := &fakeMurmur{override: func(r icetest.Request) (icetest.Reply, bool) {
		if r.Operation == "getUsers" {
			return icetest.Reply{Delay: 500 * time.Millisecond}, true
		}
		return icetest.Reply{}, false
	}}
	s := startFake(t, f)
	c := NewClient(s.Addr(), testSecret, 100*time.Millisecond)
	defer c.Close()
	start := time.Now()
	if _, err := c.Users(context.Background(), 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("took %s", d)
	}
	// The late reply is dropped; the connection still works.
	time.Sleep(500 * time.Millisecond)
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDialFailures(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c := NewClient(addr, testSecret, 200*time.Millisecond)
	var de *DialError
	if _, err := c.Version(context.Background()); !errors.As(err, &de) {
		t.Fatalf("refused: %v", err)
	}
	f := &fakeMurmur{}
	s, _ := icetest.NewServer(f.handle)
	s.NoValidate.Store(true)
	defer s.Close()
	c = NewClient(s.Addr(), testSecret, 200*time.Millisecond)
	if _, err := c.Version(context.Background()); !errors.As(err, &de) {
		t.Fatalf("no validate: %v", err)
	}
}

// The registered proxy: <identity> -o:tcp -h <host> -p <port> -t <ms>.
func TestProxyEncoding(t *testing.T) {
	var got icetest.Proxy
	f := &fakeMurmur{override: func(r icetest.Request) (icetest.Reply, bool) {
		if r.Operation == "addCallback" {
			p, err := icetest.ReadProxy(r.Params)
			if err != nil {
				return icetest.Reply{Status: 5}, true
			}
			got = p
			if r.Category != "s" || r.Name != "1" || r.Mode != 0 {
				return icetest.Reply{Status: 5}, true
			}
		}
		return icetest.Reply{}, false
	}}
	s := startFake(t, f)
	c := NewClient(s.Addr(), testSecret, time.Second)
	defer c.Close()
	id := NewIdentity()
	if !strings.HasPrefix(id.Name, "gawk-bridge-") || len(id.Name) != len("gawk-bridge-")+32 || id.Category != "" {
		t.Fatalf("identity %q", id)
	}
	if err := c.AddCallback(context.Background(), 1, Proxy{Object: id, Host: "10.1.2.3", Port: 6503, Timeout: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	want := icetest.Proxy{Name: id.Name, Mode: 1, Host: "10.1.2.3", Port: 6503, TimeoutMs: 100}
	if got != want {
		t.Fatalf("proxy %+v, want %+v", got, want)
	}
}

func listenTest(t *testing.T, id Identity) (*Endpoint, chan Event, chan error) {
	t.Helper()
	evs, errs := make(chan Event, 64), make(chan error, 64)
	ep, err := Listen("127.0.0.1:0", id, func(ev Event, err error) {
		if err != nil {
			errs <- err
			return
		}
		evs <- ev
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ep.Close() })
	return ep, evs, errs
}

func userParams(u icetest.User) []byte {
	var w icetest.W
	w.WriteUser(u)
	return w.B
}

func TestEndpointDispatch(t *testing.T) {
	id := NewIdentity()
	ep, evs, errs := listenTest(t, id)
	k, err := icetest.Dial(ep.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	// Oneway, as murmur sends them (F12).
	if err := k.Oneway("", id.Name, OpUserStateChanged, userParams(fullUser(9, "bob"))); err != nil {
		t.Fatal(err)
	}
	ev := <-evs
	if ev.Op != OpUserStateChanged || ev.User != (User{Session: 9, UserID: -1, Name: "bob", Channel: 3, Mute: true, SelfDeaf: true}) {
		t.Fatalf("event %+v", ev)
	}
	// userTextMessage carries a TextMessage too, dropped.
	var w icetest.W
	w.WriteUser(fullUser(9, "bob"))
	w.Size(1)
	w.I32(9)
	w.Size(0)
	w.Size(0)
	w.Str("a secret message")
	if st, _, err := k.Twoway("", id.Name, OpUserTextMessage, w.B); err != nil || st != 0 {
		t.Fatalf("text message: %d %v", st, err)
	}
	if ev := <-evs; ev.Op != OpUserTextMessage || ev.User.Session != 9 {
		t.Fatalf("event %+v", ev)
	}
	// Channels.
	var cw icetest.W
	cw.WriteChannel(icetest.Channel{ID: 4, Parent: 0, Name: "c", Temporary: true})
	if st, _, _ := k.Twoway("", id.Name, OpChannelCreated, cw.B); st != 0 {
		t.Fatalf("channelCreated status %d", st)
	}
	if ev := <-evs; ev.Channel != (Channel{ID: 4, Name: "c", Temporary: true}) {
		t.Fatalf("channel %+v", ev)
	}
	// ice_ping and ice_isA.
	if st, _, _ := k.Twoway("", id.Name, "ice_ping", nil); st != 0 {
		t.Fatalf("ice_ping %d", st)
	}
	var tw icetest.W
	tw.Str(ServerCallbackType)
	st, payload, _ := k.Twoway("", id.Name, "ice_isA", tw.B)
	if r := (&icetest.R{B: payload}); st != 0 || !(&icetest.R{B: r.Encaps()}).Bool() {
		t.Fatalf("ice_isA %d %x", st, payload)
	}
	// Another identity: object does not exist, and no event.
	if st, _, _ := k.Twoway("", "gawk-bridge-someone-else", OpUserConnected, userParams(fullUser(1, "x"))); st != 2 {
		t.Fatalf("foreign identity status %d", st)
	}
	// An unknown operation.
	if st, _, _ := k.Twoway("", id.Name, "getUsers", nil); st != 4 {
		t.Fatalf("unknown op status %d", st)
	}
	// An event that runs long is a protocol error, not an event.
	if err := k.Oneway("", id.Name, OpUserConnected, append(userParams(fullUser(1, "x")), 7)); err != nil {
		t.Fatal(err)
	}
	var pe *ProtocolError
	if err := <-errs; !errors.As(err, &pe) {
		t.Fatalf("err %v", err)
	}
	// ...and one that ends early.
	short := userParams(fullUser(1, "x"))
	_ = k.Oneway("", id.Name, OpUserConnected, short[:len(short)-2])
	if err := <-errs; !errors.As(err, &pe) {
		t.Fatalf("err %v", err)
	}
	select {
	case ev := <-evs:
		t.Fatalf("unexpected event %+v", ev)
	default:
	}
}

func TestEndpointCloseConnsKeepsListening(t *testing.T) {
	id := NewIdentity()
	ep, evs, _ := listenTest(t, id)
	k, err := icetest.Dial(ep.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ep.CloseConns()
	time.Sleep(50 * time.Millisecond)
	if _, _, err := k.Twoway("", id.Name, "ice_ping", nil); err == nil {
		t.Fatal("connection survived CloseConns")
	}
	k2, err := icetest.Dial(ep.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer k2.Close()
	_ = k2.Oneway("", id.Name, OpUserConnected, userParams(fullUser(2, "y")))
	if ev := <-evs; ev.User.Session != 2 {
		t.Fatalf("%+v", ev)
	}
}

// D10: the package keeps no IP, comment, identity or context from Ice.
func TestNoIdentifyingFields(t *testing.T) {
	forbidden := map[string]bool{"address": true, "ip": true, "comment": true, "identity": true, "context": true}
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				for _, name := range fld.Names {
					if forbidden[strings.ToLower(name.Name)] {
						t.Errorf("%s: struct field %s (D10: the bridge keeps no IP, comment, identity or context from Ice)", fset.Position(name.Pos()), name.Name)
					}
				}
			}
			return true
		})
	}
}

// D10, D9: nothing in the package logs or prints, so no payload or secret
// can reach a log line from here; the bridge logs only errors, which carry
// neither (TestIceLogsNoPayloadOrSecret in internal/bridge).
func TestPackageNeverLogs(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`"log"`, `"log/slog"`, "fmt.Print", "os.Stderr", "os.Stdout", "println("} {
			if bytes.Contains(src, []byte(bad)) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}

// Errors never carry the secret or payload bytes.
func TestErrorsCarryNoPayloadOrSecret(t *testing.T) {
	u := fullUser(1, "payload-name")
	f := &fakeMurmur{override: func(r icetest.Request) (icetest.Reply, bool) {
		return icetest.Reply{Result: append(icetest.UserMap([]icetest.User{u}), 1, 2)}, true
	}}
	s := startFake(t, f)
	c := NewClient(s.Addr(), testSecret, time.Second)
	defer c.Close()
	_, err := c.Users(context.Background(), 0)
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{testSecret, "payload-name", "the-comment", "the-identity", "198.51.100.7"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q carries %q", err, leak)
		}
	}
}
