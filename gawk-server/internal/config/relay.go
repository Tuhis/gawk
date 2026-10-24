package config

// R74 PP2 (docs/76 §6.1): the gawk-relay profile. One flag list, two
// binaries: the profile changes defaults, never mechanisms, so every GAWK_*
// env name is the same in both. Relay-only knobs live in RelayConfig, which
// only gawk-relay registers, so gawk-server neither accepts nor logs them.

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Profile is the set of defaults a binary substitutes into the shared flag
// list. Numeric defaults are strings because the flags parse them as such.
type Profile struct {
	Name                string // FlagSet name (usage output)
	Addr                string
	MaxSubscribers      string
	MaxBroadcasts       string
	MaxTotalSubscribers string
	MetricsAddr         string
	Rooms               bool
}

// ServerProfile is gawk-server's: the defaults the relay fleet has always had.
var ServerProfile = Profile{
	Name:                "gawk-server",
	Addr:                ":4433",
	MaxSubscribers:      "15",
	MaxBroadcasts:       "5",
	MaxTotalSubscribers: "50",
	MetricsAddr:         ":2112",
}

// RelayProfile is gawk-relay's LAN-shaped defaults (docs/76 §6.1 item 2): a
// party's whole roster can stream, a hall can watch, the ops surface stays on
// the host, and rooms are on (in memory).
var RelayProfile = Profile{
	Name:                "gawk-relay",
	Addr:                ":4433",
	MaxSubscribers:      "100",
	MaxBroadcasts:       "50",
	MaxTotalSubscribers: "500",
	MetricsAddr:         "127.0.0.1:2112",
	Rooms:               true,
}

// Budget is a PoP bandwidth budget as the host writes it: "Mbps[/streams]".
// Zero in either half means "the ceiling" (the home fleet's value); a host
// can only lower a budget, never raise it (docs/76 §6.8).
type Budget struct {
	Mbps    int
	Streams int
}

// String renders the flag form back; the zero Budget reads "ceiling".
func (b Budget) String() string {
	switch {
	case b == Budget{}:
		return "ceiling"
	case b.Streams == 0:
		return strconv.Itoa(b.Mbps)
	default:
		return strconv.Itoa(b.Mbps) + "/" + strconv.Itoa(b.Streams)
	}
}

// ParseBudget parses "Mbps[/streams]". Empty is the zero Budget.
func ParseBudget(s string) (Budget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Budget{}, nil
	}
	mbps, streams, hasStreams := strings.Cut(s, "/")
	var b Budget
	var err error
	if b.Mbps, err = strconv.Atoi(strings.TrimSpace(mbps)); err != nil || b.Mbps < 0 {
		return Budget{}, fmt.Errorf("invalid budget %q: want Mbps[/streams] as non-negative integers", s)
	}
	if hasStreams {
		if b.Streams, err = strconv.Atoi(strings.TrimSpace(streams)); err != nil || b.Streams < 0 {
			return Budget{}, fmt.Errorf("invalid budget %q: want Mbps[/streams] as non-negative integers", s)
		}
	}
	return b, nil
}

// Pop leg delivery modes (-pop-leg-delivery, docs/76 item 18 / OD15).
const (
	PopLegCarrier  = "carrier"
	PopLegDatagram = "datagram"
)

// RelayConfig is gawk-relay's own knobs (docs/76 §9).
type RelayConfig struct {
	// StateDir holds the persisted keys, certificates and the PoP
	// credential. Empty means the per-OS default (relaystate.DefaultDir),
	// which cmd/gawk-relay resolves; config cannot import relaystate.
	StateDir string
	// ConsoleAddr is the loopback-only host console (TCP).
	ConsoleAddr string
	// UIAddr is the named tier's HTTPS UI listener (TCP).
	UIAddr string
	// LANAddress is "auto" or an IP literal the host picked.
	LANAddress string
	// Pair is a pairing string to redeem at start. A one-time credential:
	// redacted in Sanitized.
	Pair string
	// PopLegDelivery is PopLegCarrier or PopLegDatagram.
	PopLegDelivery string
	// UplinkBudget and DownlinkBudget lower the home fleet's ceilings.
	UplinkBudget, DownlinkBudget Budget
	// DevMode records that -dev-cert was given. The relay always has a
	// certificate (the rolling pinned one), so it ignores the flag and says
	// so rather than dropping it silently.
	DevMode bool
}

// Sanitized is the relay knobs' startup-log view, every field enumerated by
// hand (the Config.Sanitized rule): the pairing string is redacted.
func (rc RelayConfig) Sanitized() map[string]any {
	return map[string]any{
		"stateDir":       rc.StateDir,
		"consoleAddr":    rc.ConsoleAddr,
		"uiAddr":         rc.UIAddr,
		"lanAddress":     rc.LANAddress,
		"pair":           setness(rc.Pair != ""),
		"popLegDelivery": rc.PopLegDelivery,
		"uplinkBudget":   rc.UplinkBudget.String(),
		"downlinkBudget": rc.DownlinkBudget.String(),
		"devMode":        rc.DevMode,
	}
}

// ParseRelayFlags parses gawk-relay's command line: the shared flag list
// under RelayProfile plus the relay-only knobs.
func ParseRelayFlags(args []string, getenv func(string) string) (Config, RelayConfig, error) {
	var rc RelayConfig
	register := func(fs *flag.FlagSet, getenv func(string) string) func() error {
		env, _ := envLookups(getenv)
		stateDir := fs.String("state-dir", env("GAWK_STATE_DIR", ""),
			"directory for persisted keys, certificates and the PoP credential; empty = the per-OS default")
		consoleAddr := fs.String("console-addr", env("GAWK_CONSOLE_ADDR", "127.0.0.1:4480"),
			"TCP address of the host console (loopback only)")
		uiAddr := fs.String("ui-addr", env("GAWK_UI_ADDR", ":4433"),
			"TCP address of the HTTPS web UI (named tier only)")
		lanAddress := fs.String("lan-address", env("GAWK_LAN_ADDRESS", "auto"),
			"LAN address to advertise: auto, or an IP literal")
		pair := fs.String("pair", env("GAWK_PAIR", ""),
			"pairing string from the home fleet's admin portal (gawk-pair:…)")
		legDelivery := fs.String("pop-leg-delivery", env("GAWK_POP_LEG_DELIVERY", PopLegCarrier),
			"how PoP legs carry media: carrier (R73 reliable carriers) or datagram")
		uplinkBudget := fs.String("uplink-budget", env("GAWK_UPLINK_BUDGET", ""),
			"lower the uplink budget: Mbps[/streams]; empty = the home fleet's ceiling")
		downlinkBudget := fs.String("downlink-budget", env("GAWK_DOWNLINK_BUDGET", ""),
			"lower the downlink budget: Mbps[/streams]; empty = the home fleet's ceiling")
		return func() error {
			rc.StateDir = strings.TrimSpace(*stateDir)
			rc.ConsoleAddr = strings.TrimSpace(*consoleAddr)
			rc.UIAddr = strings.TrimSpace(*uiAddr)
			rc.Pair = strings.TrimSpace(*pair)
			rc.PopLegDelivery = strings.ToLower(strings.TrimSpace(*legDelivery))
			if rc.PopLegDelivery != PopLegCarrier && rc.PopLegDelivery != PopLegDatagram {
				return fmt.Errorf("invalid pop-leg-delivery %q: want carrier or datagram", *legDelivery)
			}
			lan := strings.TrimSpace(*lanAddress)
			if strings.EqualFold(lan, "auto") || lan == "" {
				lan = "auto"
			} else if _, err := netip.ParseAddr(lan); err != nil {
				return fmt.Errorf("invalid lan-address %q: want auto or an IP literal", lan)
			}
			rc.LANAddress = lan
			for name, v := range map[string]string{"console-addr": rc.ConsoleAddr, "ui-addr": rc.UIAddr} {
				if _, _, err := net.SplitHostPort(v); err != nil {
					return fmt.Errorf("invalid %s %q: %w", name, v, err)
				}
			}
			var err error
			if rc.UplinkBudget, err = ParseBudget(*uplinkBudget); err != nil {
				return fmt.Errorf("uplink-budget: %w", err)
			}
			if rc.DownlinkBudget, err = ParseBudget(*downlinkBudget); err != nil {
				return fmt.Errorf("downlink-budget: %w", err)
			}
			return nil
		}
	}
	cfg, err := parse(args, getenv, RelayProfile, register)
	if err != nil {
		return Config{}, RelayConfig{}, err
	}
	// docs/78 D1: the fleet's telemetry key never reaches a PoP; home mints
	// a PoP's tokens over its link. A key here would mint tokens the fleet
	// never scoped or marked.
	if len(cfg.TelemetryKey) != 0 {
		return Config{}, RelayConfig{}, fmt.Errorf("-telemetry-key is a fleet secret and gawk-relay refuses it: home mints a PoP's telemetry tokens over its link (docs/78)")
	}
	rc.DevMode = cfg.DevCert
	return cfg, rc, nil
}
