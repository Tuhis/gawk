package main

// The host console (R74 PP3, docs/76 §7.1): relayconsole's handler on the
// loopback -console-addr, fed by a Provider built from this binary's parts,
// with the embedded gawk-app at /app/ and its generated config.js.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/cmd/gawk-relay/webapp"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/relayconsole"
	"github.com/Tuhis/gawk/gawk-server/internal/serve"
)

func init() { registerPart(consolePart) }

const (
	// standaloneMarker in the state directory records the host's Run
	// standalone, so a restart does not ask again.
	standaloneMarker = "standalone"
	// openedMarker records that the console was opened in the browser on
	// the first interactive start (docs/76 §6.1 item 2), so it happens once.
	openedMarker = "console-opened"

	updateFirstCheck = 10 * time.Second
	updateEvery      = 6 * time.Hour
	firewallEvery    = time.Minute
)

// consoleLink is the PoP link's half of the console. Nil until a link part
// sets linkForConsole from its Part, which runs before serve.Run: until
// then the console shows an unpaired relay and Pair says pairing is not in
// this build.
type consoleLink interface {
	// Header is a relayconsole.Header* value, or "" while never paired.
	Header() string
	HomeOrigin() string
	NamedHost() string
	Notice() string
	Pair(ctx context.Context, s string) error
	// UplinkCeiling is the home fleet's uplink budget (zero: none known).
	UplinkCeiling() config.Budget
}

// linkForConsole is set by the PoP link's part (Wave 3 wiring).
var linkForConsole atomic.Pointer[consoleLink]

// console is the relayconsole.Provider for this binary.
type console struct {
	r     *Relay
	wired atomic.Pointer[serve.Wired]

	mu         sync.Mutex
	standalone bool
	uplink     config.Budget // zero: the configured budget
	downlink   config.Budget
	update     string
	firewallOK *bool // nil: not checked (or not Windows)
}

func newConsole(r *Relay) *console {
	c := &console{r: r}
	if _, err := os.Stat(filepath.Join(r.StateDir, standaloneMarker)); err == nil {
		c.standalone = true
	}
	return c
}

// consolePart serves the console beside the relay.
func consolePart(r *Relay) ([]func(ctx context.Context, w serve.Wired) error, []func(w serve.Wired) error, error) {
	c := newConsole(r)
	pre := func(w serve.Wired) error {
		c.wired.Store(&w)
		return nil
	}
	extra := []func(ctx context.Context, w serve.Wired) error{c.serve, c.watchUpdates}
	if relayconsole.FirewallSupported {
		extra = append(extra, c.watchFirewall)
	}
	return extra, []func(w serve.Wired) error{pre}, nil
}

// serve runs the console's HTTP server until ctx ends.
func (c *console) serve(ctx context.Context, _ serve.Wired) error {
	l, err := relayconsole.Listen(c.r.Relay.ConsoleAddr)
	if err != nil {
		return fmt.Errorf("console: %w (is another gawk-relay running? -console-addr picks another port)", err)
	}
	var p relayconsole.Provider = c
	if relayconsole.FirewallSupported {
		p = firewallConsole{c}
	}
	srv := &http.Server{
		Handler:           relayconsole.Handler(p, webapp.FS(), func() []byte { return relayconsole.ConfigJS(c.State()) }),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	url := "http://" + l.Addr().String() + "/"
	c.r.Log.Info("host console", "url", url)
	c.maybeOpen(url)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(l) }()
	select {
	case err := <-errCh:
		return fmt.Errorf("console: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}

// maybeOpen opens the console in the browser on the first interactive
// start: stdout is a terminal (a double-clicked binary gets one; a
// service or a container does not) and the marker is absent.
func (c *console) maybeOpen(url string) {
	marker := filepath.Join(c.r.StateDir, openedMarker)
	if _, err := os.Stat(marker); err == nil || !interactive() {
		return
	}
	if relayconsole.OpenBrowser(runtime.GOOS, os.Getenv, url) {
		_ = os.WriteFile(marker, nil, 0o600)
	}
}

func interactive() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// watchUpdates keeps the "update available" line current. A dev build
// never checks.
func (c *console) watchUpdates(ctx context.Context, _ serve.Wired) error {
	if !releaseVersion(c.r.Cfg.ReleaseVersion) {
		return nil
	}
	client := &http.Client{Timeout: 15 * time.Second}
	t := time.NewTimer(updateFirstCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		v, err := relayconsole.CheckUpdate(ctx, client, relayconsole.ReleaseManifestURL, c.r.Cfg.ReleaseVersion)
		if err != nil {
			// A party network without internet is normal, not an error.
			c.r.Log.Debug("update check failed", "err", err)
		} else {
			c.mu.Lock()
			c.update = v
			c.mu.Unlock()
		}
		t.Reset(updateEvery)
	}
}

func releaseVersion(v string) bool {
	var a, b, d int
	n, err := fmt.Sscanf(v, "%d.%d.%d", &a, &b, &d)
	return err == nil && n == 3 && fmt.Sprintf("%d.%d.%d", a, b, d) == v
}

// watchFirewall re-reads whether our rule exists (Windows only).
func (c *console) watchFirewall(ctx context.Context, _ serve.Wired) error {
	t := time.NewTicker(firewallEvery)
	defer t.Stop()
	for {
		ok, err := relayconsole.FirewallRulePresent(ctx)
		if err == nil {
			c.mu.Lock()
			c.firewallOK = &ok
			c.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (c *console) link() consoleLink {
	if p := linkForConsole.Load(); p != nil {
		return *p
	}
	return nil
}

// State is the console's snapshot.
func (c *console) State() relayconsole.State {
	s := relayconsole.State{
		Version:   c.r.Cfg.ReleaseVersion,
		RelayPort: addrPort(c.r.Cfg.Addr),
		UIPort:    addrPort(c.r.Relay.UIAddr),
	}
	link := c.link()
	c.mu.Lock()
	standalone, update, fwOK := c.standalone, c.update, c.firewallOK
	upLowered, downLowered := c.uplink, c.downlink
	c.mu.Unlock()

	s.Header = relayconsole.HeaderWaiting
	if standalone {
		s.Header = relayconsole.HeaderStandalone
	}
	if link != nil {
		if h := link.Header(); h != "" {
			s.Header = h
		}
		s.HomeOrigin, s.NamedHost, s.Notice = link.HomeOrigin(), link.NamedHost(), link.Notice()
		s.UplinkCeiling = link.UplinkCeiling()
	}
	s.Uplink = effective(minBudget(c.r.Relay.UplinkBudget, s.UplinkCeiling), upLowered)
	s.Downlink = effective(c.r.Relay.DownlinkBudget, downLowered)
	s.UpdateAvailable = update

	if c.r.LAN != nil {
		s.LanAddr = c.r.LAN.Current()
		for _, cand := range c.r.LAN.Candidates() {
			s.LanCandidates = append(s.LanCandidates, cand.Addr)
		}
	}
	if c.r.Rolling != nil {
		s.Hashes[0], s.Hashes[1] = c.r.Rolling.Hashes()
		if d := time.Until(c.r.Rolling.RotatesAt()); d > 0 {
			s.CertRotatesIn = d
		}
	}
	if w := c.wired.Load(); w != nil && w.Registry != nil {
		for _, b := range w.Registry.AdminStats() {
			if b.Role != "origin" || !b.PublisherActive && b.GraceRemainingSeconds == 0 {
				continue
			}
			row := relayconsole.LiveRow{Code: b.ID, LocalViewers: b.ViewersLocal}
			if popLink != nil {
				row.Wall, row.Label = popLink.WallRow(b.ID)
			}
			s.Live = append(s.Live, row)
		}
	}
	s.Health = c.health(s, fwOK)
	return s
}

// health is the self-check, one line per item with its fix (§7.1).
func (c *console) health(s relayconsole.State, fwOK *bool) []adminapi.HealthLine {
	var out []adminapi.HealthLine
	add := func(key string, ok bool, detail string) {
		out = append(out, adminapi.HealthLine{Key: key, OK: ok, Detail: detail})
	}
	if c.r.Rolling != nil {
		days := int(s.CertRotatesIn.Hours()/24 + 0.5)
		add("certificate", true, fmt.Sprintf("Certificate: pinned, rotates in %d %s", days, plural(days, "day", "days")))
	} else {
		add("certificate", true, "Certificate: your own (-cert-file); renew it yourself")
	}
	if s.LanAddr.IsValid() {
		add("lan", true, "LAN address: "+s.LanAddr.String())
	} else {
		add("lan", false, "No LAN address: connect this machine to the party network, or pick an address below")
	}
	if relayconsole.FirewallSupported {
		switch {
		case fwOK == nil:
		case *fwOK:
			add("firewall", true, "Windows Firewall: guests on this network are allowed")
		default:
			add("firewall", false, "Windows Firewall may block guests on a Public network: use Allow on this network")
		}
	}
	if webapp.Placeholder() {
		add("webapp", false, "This is a development build: /app/ has no web app")
	}
	switch {
	case s.UpdateAvailable != "":
		add("version", false, fmt.Sprintf("Version %s: update available (%s), from gawk.ioio.fi", s.Version, s.UpdateAvailable))
	default:
		add("version", true, "Version "+s.Version)
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Pair redeems a pairing string through the PoP link.
func (c *console) Pair(ctx context.Context, s string) error {
	if l := c.link(); l != nil {
		return l.Pair(ctx, s)
	}
	return errors.New("this gawk-relay cannot pair with a home fleet yet; it runs standalone")
}

// RunStandalone records the host's choice, across restarts.
func (c *console) RunStandalone() error {
	if err := os.WriteFile(filepath.Join(c.r.StateDir, standaloneMarker), nil, 0o600); err != nil {
		return fmt.Errorf("could not save the choice: %w", err)
	}
	c.mu.Lock()
	c.standalone = true
	c.mu.Unlock()
	return nil
}

// SetLANOverride advertises a (the zero Addr: detect again). It lasts until
// the relay restarts; -lan-address is the persistent form.
func (c *console) SetLANOverride(a netip.Addr) error {
	if a.IsValid() && (a.IsUnspecified() || a.IsMulticast()) {
		return fmt.Errorf("%s cannot be a LAN address", a)
	}
	if c.r.LAN == nil {
		return errors.New("no LAN watcher")
	}
	c.r.LAN.SetOverride(a)
	return nil
}

// LowerUplink lowers the uplink budget below the configured one (the
// -uplink-budget flag, then the home fleet's ceiling); zero goes back to it.
func (c *console) LowerUplink(b config.Budget) error {
	var ceiling config.Budget
	if l := c.link(); l != nil {
		ceiling = l.UplinkCeiling()
	}
	return c.lower(&c.uplink, minBudget(c.r.Relay.UplinkBudget, ceiling), b, "uplink")
}

// LowerDownlink lowers the downlink budget below -downlink-budget.
func (c *console) LowerDownlink(b config.Budget) error {
	return c.lower(&c.downlink, c.r.Relay.DownlinkBudget, b, "downlink")
}

func (c *console) lower(dst *config.Budget, ceiling, b config.Budget, what string) error {
	if over(b.Mbps, ceiling.Mbps) || over(b.Streams, ceiling.Streams) {
		return fmt.Errorf("the %s budget can only be lowered here (the limit is %s)", what, budgetString(ceiling))
	}
	c.mu.Lock()
	*dst = b
	c.mu.Unlock()
	return nil
}

// over: v exceeds a nonzero ceiling.
func over(v, ceiling int) bool { return ceiling > 0 && v > ceiling }

// effective is the budget in force: each lowered field, else the
// configured one.
func effective(configured, lowered config.Budget) config.Budget {
	out := configured
	if lowered.Mbps > 0 {
		out.Mbps = lowered.Mbps
	}
	if lowered.Streams > 0 {
		out.Streams = lowered.Streams
	}
	return out
}

// minBudget is the tighter of two budgets per field; zero is no limit.
func minBudget(a, b config.Budget) config.Budget {
	m := func(x, y int) int {
		if x == 0 || (y != 0 && y < x) {
			return y
		}
		return x
	}
	return config.Budget{Mbps: m(a.Mbps, b.Mbps), Streams: m(a.Streams, b.Streams)}
}

func budgetString(b config.Budget) string {
	if b.Mbps == 0 && b.Streams == 0 {
		return "none"
	}
	s := strconv.Itoa(b.Mbps) + " Mbps"
	if b.Streams > 0 {
		s += "/" + strconv.Itoa(b.Streams) + " streams"
	}
	return s
}

// addrPort is the port of a listen address, 0 when it has none.
func addrPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// firewallConsole adds the Windows Firewall button (docs/76 §7.1 Health).
type firewallConsole struct{ *console }

func (f firewallConsole) AllowFirewall(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	argv, err := relayconsole.FirewallCommand(exe, lanPrefix(f.r.LAN.Current()), addrPort(f.r.Cfg.Addr), addrPort(f.r.Relay.UIAddr))
	if err != nil {
		return err
	}
	if err := relayconsole.AllowFirewallRule(ctx, argv); err != nil {
		return err
	}
	ok, err := relayconsole.FirewallRulePresent(ctx)
	if err == nil {
		f.mu.Lock()
		f.firewallOK = &ok
		f.mu.Unlock()
	}
	f.r.Log.Info("windows firewall rule added", slog.String("rule", relayconsole.FirewallRuleName))
	return nil
}

// lanPrefix is the subnet of the interface holding a, or the invalid
// Prefix (netsh's LocalSubnet) when it cannot be found.
func lanPrefix(a netip.Addr) netip.Prefix {
	if !a.IsValid() {
		return netip.Prefix{}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Prefix{}
	}
	for _, ia := range addrs {
		ipn, ok := ia.(*net.IPNet)
		if !ok {
			continue
		}
		p, err := netip.ParsePrefix(ipn.String())
		if err == nil && p.Addr().Unmap() == a.Unmap() {
			return netip.PrefixFrom(a.Unmap(), p.Bits()).Masked()
		}
	}
	return netip.Prefix{}
}
