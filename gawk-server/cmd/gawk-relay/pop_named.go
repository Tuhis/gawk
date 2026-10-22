package main

// The named tier (R74 PP12, docs/76 §6.11 items 38-40) and the console's
// view of the PoP link. This file sorts after pop.go, so its pre-run hook
// runs after the one that builds popLink.

import (
	"context"
	"crypto/tls"
	"errors"
	"sync/atomic"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/cmd/gawk-relay/webapp"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/relayconsole"
	"github.com/Tuhis/gawk/gawk-server/internal/serve"
)

func init() { registerPart(namedPart) }

// namedTier is the relay's named tier, nil until the pre-run hook builds it.
var namedTier atomic.Pointer[popclient.Named]

// withNamedCert answers a handshake for the named host with the named
// certificate, and every other one (IP literals, pinned clients) with
// next: https://<named>:4433 is then one origin for the page and the relay.
func withNamedCert(next serve.CertFunc) serve.CertFunc {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if n := namedTier.Load(); n != nil {
			if c := n.Certificate(hello); c != nil {
				return c, nil
			}
		}
		return next(hello)
	}
}

// namedPart builds the named tier over the PoP link and gives the console
// its link view (the W2-console seam, linkForConsole).
func namedPart(r *Relay) ([]func(ctx context.Context, w serve.Wired) error, []func(w serve.Wired) error, error) {
	pre := func(serve.Wired) error {
		if popLink == nil {
			return errors.New("named tier: the PoP link is not built")
		}
		n := popclient.NewNamed(popLink, popclient.NamedOptions{
			StateDir:  r.StateDir,
			UIAddr:    r.Relay.UIAddr,
			RelayPort: addrPort(r.Cfg.Addr),
			App:       webapp.FS(),
			Log:       r.Log,
		})
		namedTier.Store(n)
		var cl consoleLink = popConsoleLink{c: popLink, n: n}
		linkForConsole.Store(&cl)
		return nil
	}
	run := func(ctx context.Context, _ serve.Wired) error {
		if n := namedTier.Load(); n != nil {
			return n.Run(ctx)
		}
		return nil
	}
	return []func(context.Context, serve.Wired) error{run}, []func(serve.Wired) error{pre}, nil
}

// popConsoleLink is the console's view of the PoP link and named tier.
type popConsoleLink struct {
	c *popclient.Client
	n *popclient.Named
}

// Header maps the link's state onto the console's. "No credential" is the
// console's own to show (waiting to pair, or standalone once chosen).
func (p popConsoleLink) Header() string {
	h := p.c.State().Header
	if h == popclient.HeaderStandalone {
		return ""
	}
	return h
}

func (p popConsoleLink) HomeOrigin() string {
	if cfg := p.c.State().Config; cfg != nil {
		return cfg.HomeOrigin
	}
	return ""
}

// NamedHost is the name guests are sent to: only once the DNS self-check
// says the venue resolves it (docs/76 item 40); otherwise the console's QR
// falls back to the home link (or the pinned link).
func (p popConsoleLink) NamedHost() string {
	if p.n == nil {
		return ""
	}
	return p.n.GuestHost()
}

func (p popConsoleLink) Notice() string {
	if p.n == nil {
		return ""
	}
	return p.n.Notice()
}

// Health is the named tier's certificate and DNS self-check lines for the
// console's Health section (docs/76 §7.1).
func (p popConsoleLink) Health() []adminapi.HealthLine {
	if p.n == nil {
		return nil
	}
	return p.n.Health()
}

func (p popConsoleLink) Pair(ctx context.Context, s string) error { return p.c.Pair(ctx, s) }

// LowerBudgets reaches the uplink's and the downlink's admission.
func (p popConsoleLink) LowerBudgets(up, down config.Budget) { p.c.LowerBudgets(up, down) }

func (p popConsoleLink) DisplayName() string {
	if cfg := p.c.State().Config; cfg != nil {
		return cfg.DisplayName
	}
	return ""
}

// Usage is the legs' use; the outside column only while linked.
func (p popConsoleLink) Usage() consoleUsage {
	up, down := p.c.UplinkStats(), p.c.DownlinkUse()
	u := consoleUsage{UplinkMbps: up.Mbps, UplinkStreams: up.Streams, DownlinkStreams: down.Streams}
	if p.c.State().Header != popclient.HeaderLinked {
		return u
	}
	u.Outside = map[string]consoleOutside{}
	for _, b := range p.c.UplinkBroadcasts() {
		u.Outside[b.ID] = outsideOf(b)
	}
	return u
}

// outsideOf is one broadcast's outside state (docs/76 §7.1, item 15: past
// the PoP's limit or in conflict a broadcast stays local-only).
func outsideOf(b popclient.BroadcastUplink) consoleOutside {
	switch {
	case b.Forwarded:
		return consoleOutside{State: relayconsole.OutsideForwarded, Mbps: b.Mbps}
	case b.BudgetFull, b.Reservation == poplink.ReservedLimit, b.Reservation == poplink.ReservedConflict:
		return consoleOutside{State: relayconsole.OutsidePartyOnly}
	}
	return consoleOutside{State: relayconsole.OutsideNotRequested}
}

func (p popConsoleLink) UplinkCeiling() config.Budget {
	if cfg := p.c.State().Config; cfg != nil {
		return config.Budget{Mbps: cfg.Uplink.MaxMbps, Streams: cfg.Uplink.MaxStreams}
	}
	return config.Budget{}
}
