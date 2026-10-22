package popclient

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/lanaddr"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// A pairing string is redeemed on a ?pair= dial: home's credential record
// is stored (it survives a restart, with the token's hash so -pair is not
// redeemed twice), Pair returns nil, and the same link carries on: the
// config links the PoP, names the Pop for later dials and reports the
// other address family.
func TestPairingLinksAndStoresTheCredential(t *testing.T) {
	f := newFakeHome(t)
	o := testOptions(t, f)
	c := New(o)
	configs := make(chan string, 8)
	c.OnRecord(poplink.TConfig, func(rec any) { configs <- rec.(*poplink.Config).PopName })
	run(t, c)

	tok, err := pops.NewPairingToken()
	if err != nil {
		t.Fatal(err)
	}
	pairing := pops.FormatPairing("home.example:4433", tok)
	paired := make(chan error, 1)
	go func() { paired <- c.Pair(t.Context(), pairing) }()

	l := f.nextLink(t)
	if got := l.url.Query().Get("pair"); got != tok {
		t.Fatalf("pairing dial ?pair=%q, want the token", got)
	}
	if l.hdr.Get("Authorization") != "" {
		t.Fatal("a pairing dial sent a credential")
	}
	if l.hdr.Get("Origin") != poplink.Origin || l.url.Query().Get("proto") != poplink.ProtoQuery() {
		t.Fatalf("dial headers %v query %v", l.hdr, l.url.Query())
	}
	if l.hello.Version != "test" || l.hello.Proto != poplink.ProtoMajor {
		t.Fatalf("hello = %+v", l.hello)
	}
	l.send(poplink.Credential{Credential: "the-credential"})
	select {
	case err := <-paired:
		if err != nil {
			t.Fatalf("Pair = %v", err)
		}
	case <-time.After(waitFor):
		t.Fatal("Pair did not return after the credential")
	}
	if st := c.State(); st.Header != HeaderWaiting || st.Endpoint != "home.example:4433" {
		t.Fatalf("paired, before config: %+v", st)
	}
	// A second credential record on the same link is ignored (only the
	// first record of a pairing dial).
	l.send(poplink.Credential{Credential: "other"})
	l.send(poplink.Config{PopName: "assembly", DisplayName: "Assembly"})
	st := waitState(t, c, headerIs(HeaderLinked))
	if st.Config == nil || st.Config.PopName != "assembly" {
		t.Fatalf("linked state %+v", st)
	}
	stored, err := loadCredential(o.StateDir)
	if err != nil || stored == nil {
		t.Fatalf("stored credential %+v, %v", stored, err)
	}
	if stored.Credential != "the-credential" || stored.Endpoint != "home.example:4433" ||
		stored.Pop != "assembly" || stored.PairTokenSHA != pops.TokenSHA256(tok) {
		t.Fatalf("stored = %+v", stored)
	}
	// The other family: the link came over IPv4, so /pop/addr goes udp6
	// with the credential and the Pop's name.
	deadline := time.Now().Add(waitFor)
	for len(f.dialsTo("/pop/addr")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no /pop/addr dial")
		}
		time.Sleep(5 * time.Millisecond)
	}
	a := f.dialsTo("/pop/addr")[0]
	if a.network != "udp6" || a.hdr.Get("Authorization") != "Bearer the-credential" || a.url.Query().Get("pop") != "assembly" {
		t.Fatalf("/pop/addr dial %s %v %v", a.network, a.hdr, a.url.Query())
	}
	if got := <-configs; got != "assembly" {
		t.Fatalf("OnRecord(config) saw %q", got)
	}

	// A draining home pod (4002): the PoP relinks at once, now with the
	// stored credential and ?pop=.
	l.close(wire.CloseCodeServerDraining, "draining")
	l2 := f.nextLink(t)
	if got := l2.hdr.Get("Authorization"); got != "Bearer the-credential" {
		t.Fatalf("relink Authorization %q", got)
	}
	if l2.url.Query().Get("pop") != "assembly" || l2.url.Query().Has("pair") {
		t.Fatalf("relink query %v", l2.url.Query())
	}
	l2.send(poplink.Config{PopName: "assembly"})
	waitState(t, c, headerIs(HeaderLinked))

	// A restart with -pair still set does not redeem the string again.
	o2 := o
	o2.Pair = pairing
	again := New(o2)
	run(t, again)
	l3 := f.nextLink(t)
	if l3.url.Query().Has("pair") {
		t.Fatal("a restart redeemed -pair again")
	}
}

// A refused pairing returns home's status and reason; with no previous
// credential the relay stays standalone and does not redial.
func TestPairingRefused(t *testing.T) {
	f := newFakeHome(t)
	f.setRefuse(func(d dialRecord) (Session, *http.Response, error) {
		rsp := &http.Response{StatusCode: http.StatusGone, Header: http.Header{}}
		rsp.Header.Set(poplink.ReasonHeader, "used")
		return nil, rsp, errors.New("refused")
	})
	c := New(testOptions(t, f))
	run(t, c)
	tok, _ := pops.NewPairingToken()
	err := c.Pair(t.Context(), pops.FormatPairing("home.example:4433", tok))
	var pe *PairError
	if !errors.As(err, &pe) || pe.Status != http.StatusGone || pe.Reason != "used" {
		t.Fatalf("Pair = %v, want a 410 used PairError", err)
	}
	if !strings.Contains(pe.Error(), "410 used") {
		t.Fatalf("PairError text %q", pe.Error())
	}
	st := c.State()
	if st.Header != HeaderStandalone || st.LastStatus != http.StatusGone {
		t.Fatalf("after a refused pairing: %+v", st)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.dialsTo("/pop/link")); n != 1 {
		t.Fatalf("%d link dials after a refused pairing, want 1", n)
	}

	// A dial error without a response is returned as is.
	f.setRefuse(func(dialRecord) (Session, *http.Response, error) { return nil, nil, errors.New("no route") })
	if err := c.Pair(t.Context(), pops.FormatPairing("home.example:4433", tok)); err == nil || errors.As(err, &pe) {
		t.Fatalf("Pair over a dead network = %v", err)
	}
}

// A pairing link that upgraded but lost the redeem ends with the status as
// its session code: Pair reads it as the refusal. One that ends without a
// code is an error too. A stored credential relinks at once afterwards.
func TestPairingLinkEndsBeforeACredential(t *testing.T) {
	f := newFakeHome(t)
	o := testOptions(t, f)
	withCredential(t, o, "old-pop")
	c := New(o)
	run(t, c)
	l := f.nextLink(t)
	if l.hdr.Get("Authorization") != "Bearer cred-1" || l.url.Query().Get("pop") != "old-pop" {
		t.Fatalf("stored-credential dial %v %v", l.hdr, l.url.Query())
	}
	l.send(poplink.Config{PopName: "old-pop"})
	waitState(t, c, headerIs(HeaderLinked))

	tok, _ := pops.NewPairingToken()
	paired := make(chan error, 1)
	go func() { paired <- c.Pair(t.Context(), pops.FormatPairing("home.example:4433", tok)) }()
	pl := f.nextLink(t) // the live link was closed for the pairing dial
	if pl.url.Query().Get("pair") != tok {
		t.Fatalf("expected the pairing dial, got %v", pl.url)
	}
	pl.close(http.StatusGone, "used")
	var pe *PairError
	select {
	case err := <-paired:
		if !errors.As(err, &pe) || pe.Status != http.StatusGone || pe.Reason != "used" {
			t.Fatalf("Pair = %v, want 410 used", err)
		}
	case <-time.After(waitFor):
		t.Fatal("Pair did not return")
	}
	// The previous credential is back in use at once.
	l2 := f.nextLink(t)
	if l2.hdr.Get("Authorization") != "Bearer cred-1" {
		t.Fatalf("after the lost redeem: %v", l2.hdr)
	}

	go func() { paired <- c.Pair(t.Context(), pops.FormatPairing("home.example:4433", tok)) }()
	pl = f.nextLink(t)
	pl.close(0, "")
	select {
	case err := <-paired:
		if err == nil || errors.As(err, &pe) {
			t.Fatalf("Pair after a bare close = %v", err)
		}
	case <-time.After(waitFor):
		t.Fatal("Pair did not return")
	}
}

// A newer Pair supersedes one still waiting for Run.
func TestPairSuperseded(t *testing.T) {
	c := New(Options{Log: discard()})
	tok, _ := pops.NewPairingToken()
	s := pops.FormatPairing("home.example:4433", tok)
	first := make(chan error, 1)
	go func() { first <- c.Pair(t.Context(), s) }()
	deadline := time.Now().Add(waitFor)
	for {
		c.mu.Lock()
		p := c.pending
		c.mu.Unlock()
		if p != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first Pair never queued")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Pair(ctx, s); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Pair with no Run = %v", err)
	}
	if err := <-first; !errors.Is(err, ErrPairSuperseded) {
		t.Fatalf("first Pair = %v, want ErrPairSuperseded", err)
	}
}

// Bye: home's reason is the console's header, and the relay retries only
// at the terminal pace; repair and revoked stay through later failures.
func TestByeReadsIntoTheState(t *testing.T) {
	for _, tc := range []struct{ reason, header string }{
		{poplink.ByeRepair, HeaderRepair},
		{poplink.ByeRevoked, HeaderRevoked},
		{poplink.ByeExpired, HeaderRevoked},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			f := newFakeHome(t)
			c, l := linkUp(t, f, testOptions(t, f), poplink.Config{PopName: "p"})
			f.setRefuse(func(dialRecord) (Session, *http.Response, error) { return nil, nil, errors.New("down") })
			l.send(poplink.Bye{Reason: tc.reason})
			st := waitState(t, c, headerIs(tc.header))
			if st.ByeReason != tc.reason {
				t.Fatalf("bye %q: state %+v", tc.reason, st)
			}
			// The retry fails without a status: the header holds.
			time.Sleep(150 * time.Millisecond)
			if st := c.State(); st.Header != tc.header {
				t.Fatalf("after a failed retry: %+v", st)
			}
		})
	}
}

// An ordinary link end goes offline and redials with backoff; a dial that
// fails to open the control stream does too.
func TestLinkLossGoesOfflineAndRedials(t *testing.T) {
	f := newFakeHome(t)
	c, l := linkUp(t, f, testOptions(t, f), poplink.Config{PopName: "p"})
	if err := c.SendHealth([]adminapi.HealthLine{{Key: "dns", OK: true}}); err != nil {
		t.Fatal(err)
	}
	if h := expect[*poplink.Health](t, l); len(h.Lines) != 1 || h.Lines[0].Key != "dns" {
		t.Fatalf("health = %+v", h)
	}
	// A session that is already closed: OpenStream fails.
	f.setRefuse(func(d dialRecord) (Session, *http.Response, error) {
		pop, _ := poplink.Pipe(popAddr, homeAddr)
		_ = pop.CloseWithError(0, "gone")
		return pop, nil, nil
	})
	l.close(4999, "something else")
	waitState(t, c, headerIs(HeaderOffline))
	if !errors.Is(c.Send(poplink.Stats{}), ErrNotLinked) {
		t.Fatal("Send with the link down did not say ErrNotLinked")
	}
	deadline := time.Now().Add(waitFor)
	for len(f.dialsTo("/pop/link")) < 4 {
		if time.Now().After(deadline) {
			t.Fatal("no redials")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.setRefuse(nil)
	l2 := f.nextLink(t)
	l2.send(poplink.Config{PopName: "p"})
	waitState(t, c, headerIs(HeaderLinked))
}

// A home that ends the link cleanly (code 0, superseded) gets a relink at
// once; a 401 then reads as repair.
func TestCleanEndRelinksAndUnauthorizedIsRepair(t *testing.T) {
	f := newFakeHome(t)
	c, l := linkUp(t, f, testOptions(t, f), poplink.Config{PopName: "p"})
	f.setRefuse(func(dialRecord) (Session, *http.Response, error) {
		return nil, &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}}, errors.New("401")
	})
	l.close(0, "superseded")
	st := waitState(t, c, headerIs(HeaderRepair))
	if st.LastStatus != http.StatusUnauthorized || st.ByeReason != poplink.ByeRepair {
		t.Fatalf("after 401: %+v", st)
	}
}

// The stats record goes out at once on linking and then every StatsEvery.
func TestStatsLoop(t *testing.T) {
	f := newFakeHome(t)
	o := testOptions(t, f)
	o.StatsEvery = 20 * time.Millisecond
	_, l := linkUp(t, f, o, poplink.Config{PopName: "p", Uplink: pops.Budget{MaxMbps: 30}})
	for range 3 {
		s := expect[*poplink.Stats](t, l)
		if s.UplinkStreams != 0 || s.DownlinkStreams != 0 {
			t.Fatalf("idle stats = %+v", s)
		}
	}
}

// The hello carries the LAN address (with -addr's port when it is not the
// default), the candidates and both pinned hashes, and is sent again when
// the LAN address changes or the certificate rotates.
func TestHelloContentAndRefresh(t *testing.T) {
	f := newFakeHome(t)
	o := testOptions(t, f)
	o.LAN = lanaddr.NewWatcher("192.168.1.20", 0)
	o.Cfg.Addr = ":4500"
	rolling, err := tlsutil.NewRolling(tlsutil.RollingOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	o.Rolling = rolling
	_, l := linkUp(t, f, o, poplink.Config{PopName: "p"})
	if l.hello.LanAddr != "192.168.1.20:4500" {
		t.Fatalf("hello LAN %q", l.hello.LanAddr)
	}
	cur, next := rolling.Hashes()
	want := slices.DeleteFunc([]string{cur, next}, func(s string) bool { return s == "" })
	want = slices.Compact(want)
	if !slices.Equal(l.hello.PinnedHashes, want) {
		t.Fatalf("hello hashes %v, want %v", l.hello.PinnedHashes, want)
	}
	o.LAN.SetOverride(netip.MustParseAddr("192.168.1.77"))
	h := expect[*poplink.Hello](t, l)
	if h.LanAddr != "192.168.1.77:4500" {
		t.Fatalf("refreshed hello LAN %q", h.LanAddr)
	}
}

func TestLanAddrWithPort(t *testing.T) {
	a := netip.MustParseAddr("192.168.1.20")
	v6 := netip.MustParseAddr("fe80::1%eth0")
	for _, tc := range []struct {
		a    netip.Addr
		addr string
		want string
	}{
		{a, "", "192.168.1.20"},
		{a, ":4433", "192.168.1.20"},
		{a, "0.0.0.0:4433", "192.168.1.20"},
		{a, ":0", "192.168.1.20"},
		{a, ":http", "192.168.1.20"},
		{a, ":70000", "192.168.1.20"},
		{a, ":8443", "192.168.1.20:8443"},
		{v6, "[::]:8443", "[fe80::1]:8443"},
	} {
		if got := lanAddrWithPort(tc.a, tc.addr); got != tc.want {
			t.Errorf("lanAddrWithPort(%s, %q) = %q, want %q", tc.a, tc.addr, got, tc.want)
		}
	}
}

func TestCloseReasonAndAddrOf(t *testing.T) {
	if got := closeReason(&webtransport.SessionError{ErrorCode: 410, Message: "used"}); got != "used" {
		t.Errorf("closeReason = %q", got)
	}
	if got := closeReason(errors.New("plain")); got != "" {
		t.Errorf("closeReason(plain) = %q", got)
	}
	if got := addrOf(nil); got.IsValid() {
		t.Errorf("addrOf(nil) = %v", got)
	}
	if got := addrOf(&netAddr{}); got.IsValid() {
		t.Errorf("addrOf(non-UDP) = %v", got)
	}
	u := netip.MustParseAddrPort("[::ffff:203.0.113.7]:4433")
	if got := addrOf(udpAddr(u)); got != netip.MustParseAddr("203.0.113.7") {
		t.Errorf("addrOf(mapped) = %v", got)
	}
}

func TestOfflineHeader(t *testing.T) {
	for in, want := range map[string]string{
		HeaderRepair: HeaderRepair, HeaderRevoked: HeaderRevoked, HeaderStandalone: HeaderStandalone,
		HeaderLinked: HeaderOffline, HeaderWaiting: HeaderOffline, HeaderOffline: HeaderOffline,
	} {
		if got := offlineHeader(in); got != want {
			t.Errorf("offlineHeader(%s) = %s, want %s", in, got, want)
		}
	}
}

// Subscribers hear every state change until they cancel; State hands out
// a copy of the config.
func TestSubscribeAndStateCopy(t *testing.T) {
	c := New(Options{Log: discard()})
	var heard []string
	cancel := c.Subscribe(func(s LinkState) { heard = append(heard, s.Header) })
	c.setState(func(s *LinkState) { s.Header, s.Config = HeaderLinked, &poplink.Config{PopName: "a"} })
	cancel()
	c.setState(func(s *LinkState) { s.Header = HeaderOffline })
	if !slices.Equal(heard, []string{HeaderLinked}) {
		t.Fatalf("heard %v", heard)
	}
	st := c.State()
	st.Config.PopName = "mutated"
	if c.State().Config.PopName != "a" {
		t.Fatal("State shares the config with the caller")
	}
}

// An unreadable stored credential runs standalone.
func TestUnreadableCredentialIsStandalone(t *testing.T) {
	dir := t.TempDir()
	if err := saveCredential(dir, &storedCredential{Endpoint: "x:1"}); err != nil {
		t.Fatal(err)
	}
	if st := New(Options{StateDir: dir, Log: discard()}).State(); st.Header != HeaderStandalone {
		t.Fatalf("incomplete credential: %+v", st)
	}
	dir2 := t.TempDir()
	if err := saveCredentialRaw(dir2, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredential(dir2); err == nil {
		t.Fatal("malformed credential loaded")
	}
	if st := New(Options{StateDir: dir2, Log: discard()}).State(); st.Header != HeaderStandalone {
		t.Fatalf("malformed credential: %+v", st)
	}
	// No state directory: nothing stored, nothing loaded.
	if err := saveCredential("", &storedCredential{Endpoint: "x:1", Credential: "c"}); err != nil {
		t.Fatal(err)
	}
	if st, err := loadCredential(""); st != nil || err != nil {
		t.Fatalf("loadCredential(\"\") = %+v, %v", st, err)
	}
}

// A static credential replaces a live link: it is closed and the relay
// dials with the new credential.
func TestStaticCredentialReplacesTheLink(t *testing.T) {
	f := newFakeHome(t)
	c, _ := linkUp(t, f, testOptions(t, f), poplink.Config{PopName: "p"})
	if err := c.Pair(t.Context(), " "+CredentialPrefix+"other.example:4433/cred-2 "); err != nil {
		t.Fatal(err)
	}
	l := f.nextLink(t)
	if l.hdr.Get("Authorization") != "Bearer cred-2" || l.url.Host != "other.example:4433" {
		t.Fatalf("after a static credential: %s %v", l.url, l.hdr)
	}
}

// Run with no credential idles until Pair, and returns nil on cancel.
func TestRunIdlesWithoutACredential(t *testing.T) {
	f := newFakeHome(t)
	c := New(testOptions(t, f))
	run(t, c)
	f.noLink(t, 50*time.Millisecond)
	if err := c.Pair(t.Context(), CredentialPrefix+"home.example:4433/c"); err != nil {
		t.Fatal(err)
	}
	f.nextLink(t)
}

// The default dialer refuses a malformed URL and fails to reach a closed
// port within the context, for the link and for a family-pinned dial.
func TestDefaultDialerErrors(t *testing.T) {
	d := Dialer(nil)
	if _, _, err := d(t.Context(), "udp", "://bad", nil); err == nil {
		t.Fatal("a malformed URL dialled")
	}
	for _, network := range []string{"udp", "udp4"} {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, _, err := d(ctx, network, "https://127.0.0.1:1/pop/link", http.Header{})
		cancel()
		if err == nil {
			t.Fatalf("%s: a dial to a closed port succeeded", network)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := d(ctx, "udp6", "https://127.0.0.1:1/pop/addr", http.Header{}); err == nil {
		t.Fatal("udp6 to an IPv4 literal succeeded")
	}
}
