package mumble_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble/mumbletest"
)

func dial(t *testing.T, srv *mumbletest.Server, name string, mod ...func(*mumble.Config)) (*mumble.Client, error) {
	t.Helper()
	cfg := mumble.Config{Addr: srv.Addr(), Username: name, TLS: mumble.TLSOptions{InsecureSkipVerify: true}, AudioQueue: 16, DialTimeout: 3 * time.Second}
	for _, m := range mod {
		m(&cfg)
	}
	c, err := mumble.Dial(context.Background(), cfg)
	if c != nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func mustDial(t *testing.T, srv *mumbletest.Server, name string, mod ...func(*mumble.Config)) *mumble.Client {
	t.Helper()
	c, err := dial(t, srv, name, mod...)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	return c
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLoginStateAndReportedVersion(t *testing.T) {
	srv := mumbletest.Start(t)
	gaming := srv.AddChannel(0, "Gaming")
	sub := srv.AddChannel(gaming, "Sub")
	a := mustDial(t, srv, "alice")
	if a.Session() == 0 || a.Self().Name != "alice" {
		t.Fatalf("self %+v", a.Self())
	}
	if id, ok := a.FindPath([]string{"Gaming", "Sub"}); !ok || id != sub {
		t.Fatalf("FindPath = %d %v, want %d", id, ok, sub)
	}
	if id, ok := a.FindPath(nil); !ok || id != 0 {
		t.Fatalf("root = %d %v", id, ok)
	}
	if _, ok := a.FindPath([]string{"Nope"}); ok {
		t.Fatal("found a missing channel")
	}
	if len(a.Channels()) != 3 {
		t.Fatalf("channels %+v", a.Channels())
	}
	b := mustDial(t, srv, "bob", func(c *mumble.Config) { c.SelfMute, c.SelfDeaf = true, true })
	eventually(t, "alice sees bob", func() bool { u, ok := a.User(b.Session()); return ok && u.SelfDeaf && u.SelfMute })
	if len(a.UsersIn(0)) != 2 {
		t.Fatalf("users in root %+v", a.UsersIn(0))
	}
	// The default reported version is 1.4.0 in both forms.
	want := mumble.DefaultVersion.V2()
	for _, v := range srv.Versions() {
		if v != want {
			t.Fatalf("reported version_v2 %x, want %x (1.4.0)", v, want)
		}
	}
	if mumble.DefaultVersion.V1() != 0x010400 {
		t.Fatalf("V1 = %x", mumble.DefaultVersion.V1())
	}
	// A configured version reaches the server.
	mustDial(t, srv, "carol", func(c *mumble.Config) { c.Version = mumble.Version{Major: 1, Minor: 3, Patch: 4} })
	vs := srv.Versions()
	if got := vs[len(vs)-1]; got != (mumble.Version{Major: 1, Minor: 3, Patch: 4}).V2() {
		t.Fatalf("configured version_v2 %x", got)
	}
	b.Close()
	eventually(t, "bob gone", func() bool { _, ok := a.User(b.Session()); return !ok })
}

func TestRejectsAreTyped(t *testing.T) {
	srv := mumbletest.Start(t)
	srv.UsernameRule = regexp.MustCompile(`^[-=\w\[\]\{\}\(\)\@\|\.]+$`)
	mustDial(t, srv, "alice")
	cases := []struct {
		name string
		mod  func(*mumble.Config)
		want mumble.RejectType
	}{
		{"alice", nil, mumble.RejectUsernameInUse},
		{"ALICE", nil, mumble.RejectUsernameInUse},
		{"alice [web]", nil, mumble.RejectInvalidUsername},
	}
	for _, c := range cases {
		var mods []func(*mumble.Config)
		if c.mod != nil {
			mods = append(mods, c.mod)
		}
		_, err := dial(t, srv, c.name, mods...)
		var rej *mumble.RejectError
		if !errors.As(err, &rej) || rej.Type != c.want {
			t.Errorf("%q: %v, want %s", c.name, err, c.want)
		}
		if rej != nil && rej.Error() == "" {
			t.Error("empty error text")
		}
	}
	srv.Password = "pw"
	_, err := dial(t, srv, "dave")
	var rej *mumble.RejectError
	if !errors.As(err, &rej) || rej.Type != mumble.RejectWrongServerPW {
		t.Errorf("password: %v", err)
	}
	mustDial(t, srv, "dave", func(c *mumble.Config) { c.Password = "pw" })
	srv.MaxUsers = 2
	_, err = dial(t, srv, "erin", func(c *mumble.Config) { c.Password = "pw" })
	if !errors.As(err, &rej) || rej.Type != mumble.RejectServerFull {
		t.Errorf("full: %v", err)
	}
	if mumble.RejectType(99).String() != "RejectType(99)" || mumble.RejectServerFull.String() != "ServerFull" {
		t.Error("RejectType names")
	}
}

// Murmur's autoban closes a banned address's connection without a word: the
// client must say so (ErrRefused), not "unreachable".
func TestSilentCloseIsRefused(t *testing.T) {
	srv := mumbletest.Start(t)
	srv.Refuse.Store(true)
	if _, err := dial(t, srv, "alice"); !errors.Is(err, mumble.ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}
	var de *mumble.DialError
	if _, err := dial(t, srv, "alice", func(c *mumble.Config) { c.Addr = "127.0.0.1:1" }); !errors.As(err, &de) {
		t.Fatalf("closed port: %v, want DialError", err)
	}
	if _, err := dial(t, srv, "alice", func(c *mumble.Config) { c.Addr = "no-port" }); !errors.As(err, &de) {
		t.Fatalf("bad address: %v, want DialError", err)
	}
}

func TestKickBanAndGhostAreTyped(t *testing.T) {
	srv := mumbletest.Start(t)
	for _, tc := range []struct {
		name  string
		do    func(session uint32)
		ban   bool
		ghost bool
	}{
		{"kicked", func(s uint32) { srv.Kick(s, "bye", false) }, false, false},
		{"banned", func(s uint32) { srv.Kick(s, "go", true) }, true, false},
		{"ghosted", func(s uint32) { srv.Ghost(s) }, false, true},
	} {
		c := mustDial(t, srv, tc.name)
		tc.do(c.Session())
		select {
		case <-c.Done():
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: not ended", tc.name)
		}
		var rem *mumble.RemovedError
		if !errors.As(c.Err(), &rem) || rem.Ban != tc.ban || rem.Ghost() != tc.ghost {
			t.Fatalf("%s: %#v", tc.name, c.Err())
		}
		if rem.Error() == "" {
			t.Fatal("empty error")
		}
	}
	// A connection dropped without a word is an I/O error, not a removal.
	c := mustDial(t, srv, "dropped")
	srv.DropAll()
	<-c.Done()
	var rem *mumble.RemovedError
	if errors.As(c.Err(), &rem) || c.Err() == nil {
		t.Fatalf("drop: %v", c.Err())
	}
}

func TestChannelsTemporaryAndDenied(t *testing.T) {
	srv := mumbletest.Start(t)
	parent := srv.AddChannel(0, "Gawk")
	srv.TempToken = "tok"
	ctx := context.Background()
	a := mustDial(t, srv, "a", func(c *mumble.Config) { c.Tokens = []string{"tok"} })
	id, err := a.CreateChannel(ctx, parent, "gawk ABC", true)
	if err != nil || a.Self().Channel != id {
		t.Fatalf("create temp: %d %v self %+v", id, err, a.Self())
	}
	ch, ok := a.Channel(id)
	if !ok || !ch.Temporary || ch.Parent != parent {
		t.Fatalf("channel %+v", ch)
	}
	// Without the token: PermissionDenied, typed.
	b := mustDial(t, srv, "b")
	_, err = b.CreateChannel(ctx, parent, "gawk DEF", true)
	var den *mumble.DeniedError
	if !errors.As(err, &den) || den.Type != mumble.DenyPermission {
		t.Fatalf("no token: %v", err)
	}
	// A duplicate name is DenyChannelName.
	c := mustDial(t, srv, "c", func(c *mumble.Config) { c.Tokens = []string{"tok"} })
	if err := c.JoinChannel(ctx, parent); err != nil {
		t.Fatal(err)
	}
	// An existing channel is returned, not re-created.
	if got, err := b.CreateChannel(ctx, 0, "Gawk", false); err != nil || got != parent {
		t.Fatalf("existing: %d %v", got, err)
	}
	if mumble.DenyType(77).String() != "DenyType(77)" {
		t.Fatal("deny names")
	}
	// Join, and a denied join.
	if err := b.JoinChannel(ctx, id); err != nil || b.Self().Channel != id {
		t.Fatalf("join: %v", err)
	}
	srv.DenyEnter = map[uint32]bool{parent: true}
	if err := b.JoinChannel(ctx, parent); !errors.As(err, &den) {
		t.Fatalf("denied join: %v", err)
	}
	// The temporary channel goes when the last user leaves.
	a.Close()
	if err := b.JoinChannel(ctx, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "temporary channel removed", func() bool { _, ok := c.Channel(id); return !ok })
	// A wait ends with ctx.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := b.JoinChannel(short, 9999); err == nil {
		t.Fatal("join to a missing channel succeeded")
	}
}

// D6: every Opus packet reaches the listener with sender session, sequence,
// target and end bit, byte-identical; a stalled listener does not stall the
// connection.
func TestAudioPassesThroughUndecoded(t *testing.T) {
	srv := mumbletest.Start(t)
	a := mustDial(t, srv, "a")
	b := mustDial(t, srv, "b")
	extra := a.AddAudioListener(2) // never read: must not stall a.Audio()
	payloads := [][]byte{{0xFC, 0xFF, 0xFE}, {1, 2, 3, 4, 5}, {}}
	for i, p := range payloads {
		if ok, err := b.WriteAudio(int64(10+i), p, i == len(payloads)-1); !ok || err != nil {
			t.Fatalf("write %d: %v %v", i, ok, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i, want := range payloads {
		p, err := a.Audio().Pop(ctx)
		if err != nil {
			t.Fatalf("pop %d: %v", i, err)
		}
		if p.Session != b.Session() || p.Seq != int64(10+i) || p.Target != 0 || p.End != (i == len(payloads)-1) || !bytes.Equal(p.Opus, want) {
			t.Fatalf("packet %d: %+v", i, p)
		}
	}
	if extra.Dropped() != 1 || extra.Len() != 2 {
		t.Fatalf("stalled listener: len %d dropped %d", extra.Len(), extra.Dropped())
	}
	if st := a.Stats(); st.AudioReceived != 3 {
		t.Fatalf("stats %+v", st)
	}
	if st := b.Stats(); st.AudioSent != 3 {
		t.Fatalf("stats %+v", st)
	}
	if _, err := b.WriteAudioTarget(32, 0, []byte{1}, false); err == nil {
		t.Fatal("target 32 accepted")
	}
	b.Close()
	if _, err := b.WriteAudio(0, []byte{1}, false); !errors.Is(err, mumble.ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
	if !errors.Is(b.Err(), mumble.ErrClosed) || b.SetSelf(true, true) == nil {
		t.Fatalf("closed client: %v", b.Err())
	}
	b.Close() // idempotent
}

func TestSelfAndAdminState(t *testing.T) {
	srv := mumbletest.Start(t)
	a := mustDial(t, srv, "a")
	b := mustDial(t, srv, "b")
	if err := a.SetSelf(true, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "self mute", func() bool { return a.Self().SelfMute })
	if err := a.SetServerMute(b.Session(), true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "server mute", func() bool { return b.Self().Mute })
	_ = a.SetServerDeaf(b.Session(), true)
	if err := a.Move(b.Session(), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.SetACL(0, []mumble.ACLEntry{{Group: "#tok", ApplyHere: true, Grant: mumble.PermMakeTempChannel}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Kick(b.Session(), "bye", false); err != nil {
		t.Fatal(err)
	}
	<-b.Done()
	var rem *mumble.RemovedError
	if !errors.As(b.Err(), &rem) || !rem.HasActor || rem.Actor != a.Session() {
		t.Fatalf("kick by a: %#v", b.Err())
	}
}

func TestCertificatePinAndCAFile(t *testing.T) {
	srv := mumbletest.Start(t)
	sum := sha256.Sum256(srv.CertDER())
	pin := hex.EncodeToString(sum[:])
	mustDial(t, srv, "pinned", func(c *mumble.Config) { c.TLS = mumble.TLSOptions{CertSHA256: pin} })
	wrong := make([]byte, 32)
	var de *mumble.DialError
	if _, err := dial(t, srv, "wrongpin", func(c *mumble.Config) { c.TLS = mumble.TLSOptions{CertSHA256: hex.EncodeToString(wrong)} }); !errors.As(err, &de) {
		t.Fatalf("wrong pin: %v", err)
	}
	if _, err := dial(t, srv, "badpin", func(c *mumble.Config) { c.TLS = mumble.TLSOptions{CertSHA256: "zz"} }); !errors.As(err, &de) {
		t.Fatalf("bad pin: %v", err)
	}
	// Colon-separated pins (openssl's form) parse.
	if _, err := mumble.ParseCertPin("AB:" + pin[2:]); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.CertDER()}), 0o600); err != nil {
		t.Fatal(err)
	}
	mustDial(t, srv, "ca", func(c *mumble.Config) { c.TLS = mumble.TLSOptions{CAFile: ca} })
	if _, err := dial(t, srv, "noca", func(c *mumble.Config) { c.TLS = mumble.TLSOptions{} }); !errors.As(err, &de) {
		t.Fatalf("system roots must refuse a self-signed murmur: %v", err)
	}
	if _, err := dial(t, srv, "missingca", func(c *mumble.Config) { c.TLS = mumble.TLSOptions{CAFile: ca + ".missing"} }); !errors.As(err, &de) {
		t.Fatalf("missing CA: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	_ = os.WriteFile(empty, []byte("nothing"), 0o600)
	if _, err := dial(t, srv, "emptyca", func(c *mumble.Config) { c.TLS = mumble.TLSOptions{CAFile: empty} }); !errors.As(err, &de) {
		t.Fatalf("empty CA: %v", err)
	}
}

func TestParseVersion(t *testing.T) {
	v, err := mumble.ParseVersion("1.4.0")
	if err != nil || v != mumble.DefaultVersion || v.String() != "1.4.0" {
		t.Fatalf("%v %v", v, err)
	}
	for _, bad := range []string{"", "1.4", "1.4.0.1", "a.b.c", "1.4.0 x"} {
		if _, err := mumble.ParseVersion(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if !(mumble.Version{Major: 1, Minor: 4, Patch: 300}).Less(mumble.LegacyAudioBefore) {
		t.Fatal("Less")
	}
	if (mumble.Version{Major: 1, Minor: 2, Patch: 300}).V1() != 0x0102ff {
		t.Fatal("V1 clamps the patch")
	}
}

func TestRemoveChannelSends(t *testing.T) {
	srv := mumbletest.Start(t)
	a := mustDial(t, srv, "a")
	if err := a.RemoveChannel(1); err != nil {
		t.Fatal(err)
	}
}
