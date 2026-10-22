package popclient

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// 0x18 (docs/76 item 45): linked and the home origin only while linked;
// the name is home's display name when linked, else -server-name, else
// the hostname, cut to the wire's limit on a rune boundary.
func TestLocalRelayInfo(t *testing.T) {
	c := New(Options{Cfg: config.Config{}, Hostname: func() (string, error) { return "party-pc", nil }})
	if got := c.LocalRelayInfo(""); got.Linked || got.HomeOrigin != "" || got.Name != "party-pc" {
		t.Fatalf("standalone, no name = %+v", got)
	}
	c.opts.Cfg.ServerName = "Assembly relay"
	if got := c.LocalRelayInfo(""); got.Name != "Assembly relay" {
		t.Fatalf("standalone with -server-name = %+v", got)
	}
	cfg := &poplink.Config{DisplayName: "Assembly 2026", HomeOrigin: "https://gawk.ioio.fi"}
	c.setState(func(s *LinkState) { s.Header, s.Config = HeaderLinked, cfg })
	got := c.LocalRelayInfo("ABC234")
	if !got.Linked || got.HomeOrigin != "https://gawk.ioio.fi" || got.Name != "Assembly 2026" {
		t.Fatalf("linked = %+v", got)
	}
	if _, err := wire.AppendLocalRelayInfo(nil, got); err != nil {
		t.Fatal(err)
	}
	// Linked to a home whose origin the wire refuses: still linked.
	c.setState(func(s *LinkState) {
		s.Config = &poplink.Config{DisplayName: "x", HomeOrigin: "https://gawk.ioio.fi/app"}
	})
	if got := c.LocalRelayInfo(""); !got.Linked || got.HomeOrigin != "" {
		t.Fatalf("bad origin = %+v", got)
	}
	// Offline (and every other header) clears linked and the origin.
	for _, h := range []string{HeaderOffline, HeaderRevoked, HeaderRepair, HeaderWaiting} {
		c.setState(func(s *LinkState) { s.Header, s.Config = h, cfg })
		if got := c.LocalRelayInfo(""); got.Linked || got.HomeOrigin != "" || got.Name != "Assembly relay" {
			t.Errorf("%s = %+v", h, got)
		}
	}
	long := strings.Repeat("ä", 40) // 80 bytes
	c.opts.Cfg.ServerName = long
	got = c.LocalRelayInfo("")
	if len(got.Name) > wire.MaxRelayIdentityNameLen || !strings.HasPrefix(long, got.Name) {
		t.Fatalf("long name = %q", got.Name)
	}
	if _, err := wire.AppendLocalRelayInfo(nil, got); err != nil {
		t.Fatal(err)
	}
}

func TestOtherFamily(t *testing.T) {
	for in, want := range map[string]string{"203.0.113.7": "udp6", "::ffff:203.0.113.7": "udp6", "2001:db8::1": "udp4"} {
		if got := otherFamily(netip.MustParseAddr(in)); got != want {
			t.Errorf("otherFamily(%s) = %q, want %q", in, got, want)
		}
	}
	if got := otherFamily(netip.Addr{}); got != "" {
		t.Errorf("unknown remote = %q", got)
	}
}

// The credential survives a restart; -pair is not redeemed again when the
// stored credential came from it; a malformed string is refused up front.
func TestStoredCredentialAndPairStrings(t *testing.T) {
	dir := t.TempDir()
	dials := 0
	dial := func(context.Context, string, string, http.Header) (Session, *http.Response, error) {
		dials++
		return nil, &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}, errors.New("down")
	}
	c := New(Options{StateDir: dir, Dial: dial})
	if h := c.State().Header; h != HeaderStandalone {
		t.Fatalf("fresh relay = %q", h)
	}
	ctx := t.Context()
	for _, bad := range []string{"gawk-pair:", "nonsense", CredentialPrefix + "no-slash", CredentialPrefix + "/cred"} {
		if err := c.Pair(ctx, bad); err == nil {
			t.Errorf("Pair(%q) accepted", bad)
		}
	}
	if err := c.Pair(ctx, CredentialPrefix+"home.example:4433/the-credential"); err != nil {
		t.Fatal(err)
	}
	if st := c.State(); st.Header != HeaderWaiting || st.Endpoint != "home.example:4433" {
		t.Fatalf("after a static credential: %+v", st)
	}
	c.rememberPop("lan-party")
	again := New(Options{StateDir: dir, Dial: dial})
	if st := again.State(); st.Header != HeaderWaiting || st.Endpoint != "home.example:4433" {
		t.Fatalf("restarted: %+v", st)
	}
	if again.stored.Pop != "lan-party" || again.stored.Credential != "the-credential" {
		t.Fatalf("stored = %+v", again.stored)
	}

	// pairAtStart skips a string the credential came from.
	tok, _ := pops.NewPairingToken()
	pair := pops.FormatPairing("home.example:4433", tok)
	again.stored.PairTokenSHA = tokenSHAOf(t, pair)
	done := make(chan struct{})
	go func() { again.pairAtStart(ctx, pair); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pairAtStart redeemed a string it already paired with")
	}
	if again.pending != nil {
		t.Fatal("a pairing was queued")
	}
}

// A refused link dial is read into the console's state by its status.
func TestRefusalStatuses(t *testing.T) {
	for _, tc := range []struct {
		status         int
		reason, header string
		terminal       bool
	}{
		{http.StatusUnauthorized, poplink.ByeRepair, HeaderRepair, true},
		{http.StatusForbidden, poplink.ByeExpired, HeaderRevoked, true},
		{http.StatusForbidden, "", HeaderRevoked, true},
		{http.StatusUpgradeRequired, poplink.ReasonProto, HeaderOffline, true},
		{http.StatusServiceUnavailable, "", HeaderOffline, false},
	} {
		c := New(Options{})
		c.setState(func(s *LinkState) { s.Header = HeaderLinked })
		rsp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
		rsp.Header.Set(poplink.ReasonHeader, tc.reason)
		res := c.refused(t.Context(), nil, rsp, errors.New("refused"))
		st := c.State()
		if st.Header != tc.header || st.LastStatus != tc.status || res.terminal != tc.terminal {
			t.Errorf("%d %q: state %+v result %+v", tc.status, tc.reason, st, res)
		}
		if tc.status == http.StatusForbidden && tc.reason == "" && st.ByeReason != poplink.ByeRevoked {
			t.Errorf("a bare 403 reads as %q, want revoked", st.ByeReason)
		}
	}
}

func tokenSHAOf(t *testing.T, s string) string {
	t.Helper()
	_, tok, err := pops.ParsePairing(s)
	if err != nil {
		t.Fatal(err)
	}
	return pops.TokenSHA256(tok)
}
