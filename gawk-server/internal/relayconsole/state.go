// Package relayconsole is gawk-relay's host console (R74 PP3, docs/76
// §7.1): a small web page on a loopback-only listener, by default
// http://127.0.0.1:4480, that shows the host how guests join, what is live,
// what the party's internet carries and what needs fixing, and takes the
// pairing string. It also serves the embedded gawk-app at /app/ so the
// host's own machine has a broadcaster and a projector view with no
// internet at all (§6.1 item 6).
//
// The console can pair the relay, lower its budgets and change its LAN
// address, and it shows raw broadcast codes. So it is loopback-only twice
// over: Listen refuses any non-loopback bind, and Handler refuses any
// request whose peer or Host header is not loopback (the second check is
// what stops a DNS-rebinding page from driving it through the host's own
// browser). State-changing requests also need a same-origin JSON POST.
//
// The package holds the console's mechanism only. What it shows comes from
// a Provider, which cmd/gawk-relay builds from the relay's parts.
package relayconsole

import (
	"context"
	"net/netip"
	"time"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
)

// Header values: the one state the console's header shows (docs/76 §7.1).
const (
	HeaderLinked     = "linked"
	HeaderStandalone = "standalone"
	HeaderOffline    = "offline"
	HeaderWaiting    = "waiting"
	HeaderRevoked    = "revoked"
	HeaderRepair     = "repair"
)

// Outside states of a live broadcast (docs/76 §7.1 "Live here").
const (
	OutsideForwarded    = "forwarded"
	OutsideNotRequested = "not-requested"
	OutsidePartyOnly    = "party-only"
)

// DefaultRelayPort is the relay's and the named UI's port when State
// leaves them zero.
const DefaultRelayPort = 4433

// DefaultAppOrigin is the gawk UI a standalone join link opens when the
// relay knows no home fleet: the official deployment (CLAUDE.md, "the
// official deployment is the default target"), which allows custom relays
// (docs/40 D6).
const DefaultAppOrigin = "https://gawk.ioio.fi"

// Budget is a PoP bandwidth budget, "Mbps[/streams]"; zero is the ceiling.
type Budget = config.Budget

// State is everything the console page shows, as one snapshot.
type State struct {
	// Header is one of the Header* constants.
	Header string `json:"header"`
	// DisplayName is the relay's name ("Assembly 2026").
	DisplayName string `json:"displayName"`
	// HomeOrigin is the linked home fleet's app origin, "" when unlinked.
	HomeOrigin string `json:"homeOrigin,omitempty"`
	// NamedHost is the named tier's host (p-<label>.<zone>), "" without one.
	// Set it only when guests can use it: the DNS self-check (§6.11 item
	// 40) clears it when the venue's resolver hides the name.
	NamedHost string `json:"namedHost,omitempty"`
	// LanAddr is the advertised LAN address; LanCandidates the others the
	// host may pick.
	LanAddr       netip.Addr   `json:"lanAddr"`
	LanCandidates []netip.Addr `json:"lanCandidates"`
	// RelayPort is the relay's UDP port, UIPort the named tier's TCP port;
	// zero means DefaultRelayPort.
	RelayPort int `json:"relayPort,omitempty"`
	UIPort    int `json:"uiPort,omitempty"`
	// Hashes are the pinned certificate's serving and next hashes; empty
	// with an explicit -cert-file.
	Hashes [2]string `json:"hashes"`
	// CertRotatesIn is how long the serving certificate has left; zero
	// when unknown.
	CertRotatesIn time.Duration `json:"certRotatesIn"`
	Live          []LiveRow     `json:"live"`
	// Uplink is the effective uplink budget, UplinkCeiling the home
	// fleet's; Downlink the effective downlink budget.
	Uplink          Budget  `json:"uplink"`
	UplinkCeiling   Budget  `json:"uplinkCeiling"`
	Downlink        Budget  `json:"downlink"`
	UplinkMbps      float64 `json:"uplinkMbps"`
	UplinkStreams   int     `json:"uplinkStreams"`
	DownlinkStreams int     `json:"downlinkStreams"`
	// Health is the self-check, one line per item, each with its fix.
	Health []adminapi.HealthLine `json:"health"`
	// Version is this binary's; UpdateAvailable the newer release, if any.
	Version         string `json:"version"`
	UpdateAvailable string `json:"updateAvailable,omitempty"`
	// Notice is one line the console shows above everything ("Re-pair
	// needed", "Update gawk-relay", a pairing failure).
	Notice string `json:"notice,omitempty"`
}

// LiveRow is one broadcast live on this relay. Code is the RAW broadcast
// code: the console is loopback-only, which is what lets it show one
// (docs/76 §8).
type LiveRow struct {
	Code           string `json:"code"`
	Label          string `json:"label,omitempty"`
	LocalViewers   int    `json:"localViewers"`
	OutsideViewers int    `json:"outsideViewers"`
	// Outside is one of the Outside* constants, "" when unlinked.
	Outside     string  `json:"outside,omitempty"`
	OutsideMbps float64 `json:"outsideMbps,omitempty"`
	// Wall is "on", "hidden", or "" with no wall.
	Wall string `json:"wall,omitempty"`
}

// Provider is what the console reads and drives. Every method is called on
// a request goroutine and must be safe for concurrent use.
type Provider interface {
	State() State
	// Pair redeems a pairing string (gawk-pair:…).
	Pair(ctx context.Context, s string) error
	// RunStandalone leaves the "waiting to pair" state.
	RunStandalone() error
	// SetLANOverride advertises a. The invalid Addr returns to detection.
	SetLANOverride(a netip.Addr) error
	// LowerUplink and LowerDownlink lower a budget; raising one past the
	// ceiling is the provider's to refuse (docs/76 §6.8).
	LowerUplink(Budget) error
	LowerDownlink(Budget) error
}

// FirewallAllower is a Provider that can add the Windows Firewall rule
// (docs/76 §7.1 Health). The console offers the button only when the
// provider implements it and FirewallSupported is true.
type FirewallAllower interface {
	AllowFirewall(ctx context.Context) error
}
