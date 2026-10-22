package main

import (
	"context"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/relayconsole"
	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
	"github.com/Tuhis/gawk/gawk-server/internal/serve"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

// noLink clears the process-wide console link and PoP link for one test:
// an in-process run() installs both and nothing uninstalls them.
func noLink(t *testing.T) {
	t.Helper()
	prevLink, prevPop := linkForConsole.Load(), popLink
	linkForConsole.Store(nil)
	popLink = nil
	t.Cleanup(func() { linkForConsole.Store(prevLink); popLink = prevPop })
}

// A stored credential that has not connected yet is the link's "waiting",
// not the console's own standalone header.
func TestPopConsoleLinkWithAStoredCredential(t *testing.T) {
	dir := t.TempDir()
	if err := relaystate.SaveSecret(dir, popclient.CredentialSecret,
		[]byte(`{"endpoint":"home.example:4433","credential":"c"}`)); err != nil {
		t.Fatal(err)
	}
	cl := popConsoleLink{c: popclient.New(popclient.Options{StateDir: dir, Log: slog.New(slog.DiscardHandler)})}
	if h := cl.Header(); h != relayconsole.HeaderWaiting {
		t.Errorf("Header with a stored credential = %q, want %q", h, relayconsole.HeaderWaiting)
	}
}

func healthLine(lines []adminapi.HealthLine, key string) (adminapi.HealthLine, bool) {
	for _, l := range lines {
		if l.Key == key {
			return l, true
		}
	}
	return adminapi.HealthLine{}, false
}

// The pinned certificate's Health line counts the days to its rotation
// (singular for one), and its hashes are the console's to show.
func TestConsoleHealthNamesThePinnedCertificate(t *testing.T) {
	noLink(t)
	c := testConsole(t)
	rolling, err := tlsutil.NewRolling(tlsutil.RollingOptions{
		Dir:   filepath.Join(c.r.StateDir, certsSubdir),
		Hosts: func() []string { return []string{"127.0.0.1"} },
		Log:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.r.Rolling = rolling
	s := c.State()
	cur, next := rolling.Hashes()
	if s.Hashes != [2]string{cur, next} || cur == "" {
		t.Errorf("Hashes = %v, want the rolling pair %q %q", s.Hashes, cur, next)
	}
	if s.CertRotatesIn <= 0 {
		t.Errorf("CertRotatesIn = %v, want the time to the next rotation", s.CertRotatesIn)
	}
	l, ok := healthLine(s.Health, "certificate")
	if !ok || !l.OK || !strings.HasPrefix(l.Detail, "Certificate: pinned, rotates in ") {
		t.Errorf("certificate line = %+v", l)
	}
	if got, want := plural(1, "day", "days"), "day"; got != want {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(0, "day", "days"); got != "days" {
		t.Errorf("plural(0) = %q", got)
	}
}

// An explicit certificate is the host's own to renew; with no LAN address
// the line says how to fix it; an available update flips the version line.
func TestConsoleHealthLines(t *testing.T) {
	noLink(t)
	r := &Relay{StateDir: t.TempDir()}
	r.Cfg.ReleaseVersion = "1.2.3"
	c := newConsole(r)
	s := c.State()
	if l, _ := healthLine(s.Health, "certificate"); !l.OK || !strings.Contains(l.Detail, "-cert-file") {
		t.Errorf("explicit certificate line = %+v", l)
	}
	if l, _ := healthLine(s.Health, "lan"); l.OK || !strings.Contains(l.Detail, "No LAN address") {
		t.Errorf("no-LAN line = %+v, want a failing line", l)
	}
	if l, _ := healthLine(s.Health, "version"); !l.OK || l.Detail != "Version 1.2.3" {
		t.Errorf("version line = %+v", l)
	}

	c.mu.Lock()
	c.update = "1.3.0"
	c.mu.Unlock()
	s = c.State()
	if s.UpdateAvailable != "1.3.0" {
		t.Errorf("UpdateAvailable = %q", s.UpdateAvailable)
	}
	if l, _ := healthLine(s.Health, "version"); l.OK || !strings.Contains(l.Detail, "update available (1.3.0)") {
		t.Errorf("version line with an update = %+v", l)
	}
	if err := c.SetLANOverride(netip.MustParseAddr("10.0.0.2")); err == nil {
		t.Error("SetLANOverride with no watcher succeeded")
	}
}

// Live rows are the origin broadcasts being published or in grace; an edge
// hub (a broadcast pulled from elsewhere) is not this host's to list.
func TestConsoleLiveRowsAreLocalOrigins(t *testing.T) {
	noLink(t)
	c := testConsole(t)
	r := hub.NewRegistry(slog.New(slog.DiscardHandler), hub.Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	edgeID, edge, err := r.EdgePublish("ZZZZZ2")
	if err != nil {
		t.Fatal(err)
	}
	defer edge.Close()
	c.wired.Store(&serve.Wired{Registry: r})

	live := c.State().Live
	if len(live) != 1 || live[0].Code != id {
		t.Fatalf("Live = %+v, want only the origin %s (not the edge %s)", live, id, edgeID)
	}
}

// Pairing goes through the link when there is one.
func TestConsolePairUsesTheLink(t *testing.T) {
	noLink(t)
	c := testConsole(t)
	var l consoleLink = &budgetLink{}
	linkForConsole.Store(&l)
	t.Cleanup(func() { linkForConsole.Store(nil) })
	if err := c.Pair(context.Background(), "gawk-pair:x"); err != nil {
		t.Fatalf("Pair through the link = %v, want its nil", err)
	}
}

// A choice that cannot be saved is reported, and not half-applied.
func TestConsoleRunStandaloneReportsASaveFailure(t *testing.T) {
	noLink(t)
	c := newConsole(&Relay{StateDir: filepath.Join(t.TempDir(), "missing")})
	if err := c.RunStandalone(); err == nil || !strings.Contains(err.Error(), "could not save") {
		t.Fatalf("RunStandalone = %v", err)
	}
	if c.State().Header == relayconsole.HeaderStandalone {
		t.Error("an unsaved Run standalone still flipped the header")
	}
}

func TestBudgetStringAndAddrPort(t *testing.T) {
	for b, want := range map[config.Budget]string{
		{}:                     "none",
		{Mbps: 20}:             "20 Mbps",
		{Mbps: 20, Streams: 3}: "20 Mbps/3 streams",
	} {
		if got := budgetString(b); got != want {
			t.Errorf("budgetString(%+v) = %q, want %q", b, got, want)
		}
	}
	for addr, want := range map[string]int{":4433": 4433, "127.0.0.1:80": 80, "no-port": 0, "": 0} {
		if got := addrPort(addr); got != want {
			t.Errorf("addrPort(%q) = %d, want %d", addr, got, want)
		}
	}
}

// A dev build never checks for updates; a release build stops checking
// when the relay does, without a request before the first-check delay.
func TestWatchUpdatesStopsOnCancel(t *testing.T) {
	noLink(t)
	for _, v := range []string{"dev", "1.2.3"} {
		r := &Relay{StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
		r.Cfg.ReleaseVersion = v
		c := newConsole(r)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.watchUpdates(ctx, serve.Wired{}); err != nil {
			t.Errorf("watchUpdates(%s) = %v", v, err)
		}
		if c.State().UpdateAvailable != "" {
			t.Errorf("watchUpdates(%s) set an update without checking", v)
		}
	}
}

// Off Windows the firewall rule cannot be read or added: the watcher leaves
// the state unknown (no Health line), and AllowFirewall refuses rather than
// claiming a rule.
func TestFirewallOffWindows(t *testing.T) {
	noLink(t)
	if relayconsole.FirewallSupported {
		t.Skip("this build adds real Windows Firewall rules")
	}
	c := testConsole(t)
	c.r.Log = slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.watchFirewall(ctx, serve.Wired{}); err != nil {
		t.Fatalf("watchFirewall = %v", err)
	}
	c.mu.Lock()
	fw := c.firewallOK
	c.mu.Unlock()
	if fw != nil {
		t.Errorf("firewallOK = %v after an unsupported check, want unknown", *fw)
	}
	if err := (firewallConsole{c}).AllowFirewall(context.Background()); err == nil {
		t.Fatal("AllowFirewall succeeded off Windows")
	}
	if _, ok := healthLine(c.State().Health, "firewall"); ok {
		t.Error("a firewall Health line off Windows")
	}
}

// lanPrefix finds the subnet of the interface holding the address, and
// falls back to netsh's LocalSubnet (the invalid Prefix) otherwise.
func TestLANPrefix(t *testing.T) {
	if p := lanPrefix(netip.Addr{}); p.IsValid() {
		t.Errorf("lanPrefix(invalid) = %v", p)
	}
	if p := lanPrefix(netip.MustParseAddr("192.0.2.77")); p.IsValid() {
		t.Errorf("lanPrefix(an address on no interface) = %v", p)
	}
	p := lanPrefix(netip.MustParseAddr("127.0.0.1"))
	if !p.IsValid() {
		t.Skip("no 127.0.0.1 on this host's loopback")
	}
	if !p.Contains(netip.MustParseAddr("127.0.0.1")) || p != p.Masked() {
		t.Errorf("lanPrefix(127.0.0.1) = %v, want the masked loopback subnet", p)
	}
	if mapped := lanPrefix(netip.MustParseAddr("::ffff:127.0.0.1")); mapped != p {
		t.Errorf("lanPrefix(v4-mapped) = %v, want %v", mapped, p)
	}
}

// Before the named tier exists (or without one) the console's link view
// shows no named host, no notice and no Health lines, and an unpaired
// link's state maps onto the console's own header.
func TestPopConsoleLinkWithoutANamedTier(t *testing.T) {
	cl := popConsoleLink{c: popclient.New(popclient.Options{StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})}
	if h := cl.Header(); h != "" {
		t.Errorf("unpaired Header = %q, want the console's own", h)
	}
	if cl.NamedHost() != "" || cl.Notice() != "" || cl.Health() != nil {
		t.Errorf("no named tier: host %q notice %q health %v", cl.NamedHost(), cl.Notice(), cl.Health())
	}
	if cl.HomeOrigin() != "" || cl.DisplayName() != "" || cl.UplinkCeiling() != (config.Budget{}) {
		t.Error("an unlinked PoP reports home's config")
	}
	if u := cl.Usage(); u.Outside != nil {
		t.Errorf("unlinked Usage has an outside column: %+v", u)
	}
}

// The named part refuses to start without the PoP link it builds on, and
// its goroutine is a no-op while there is no named tier.
func TestNamedPartNeedsThePopLink(t *testing.T) {
	prevLink, prevNamed := popLink, namedTier.Load()
	t.Cleanup(func() { popLink = prevLink; namedTier.Store(prevNamed) })
	popLink = nil
	namedTier.Store(nil)
	extra, pre, err := namedPart(&Relay{StateDir: t.TempDir()})
	if err != nil || len(extra) != 1 || len(pre) != 1 {
		t.Fatalf("namedPart = %d %d %v", len(extra), len(pre), err)
	}
	if err := pre[0](serve.Wired{}); err == nil || !strings.Contains(err.Error(), "PoP link is not built") {
		t.Errorf("pre-run without the link = %v", err)
	}
	if err := extra[0](context.Background(), serve.Wired{}); err != nil {
		t.Errorf("run without a named tier = %v, want nil", err)
	}

	extra, _, err = popPart(&Relay{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := extra[0](context.Background(), serve.Wired{}); err != nil {
		t.Errorf("the PoP link's goroutine with no link = %v, want nil", err)
	}
}
