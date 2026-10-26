// Package bridge is gawk-mumble-bridge's service (docs/80 D1, D4, D5, D8,
// D9, D10, D11, D17, D18, D19, D23, D24 and §11.3–11.5): the public voice
// listener, the internal observer listener, the session links to the relay,
// the per-user and observer Mumble connections, and the ops endpoints.
//
// Nothing in this package writes, logs or exposes voice audio, a ticket, a
// raw room code or a client address (D24); rooms appear in logs and
// /statusz only as RoomKey, the keyed hash. TestNoVoiceBytesWrittenOrLogged
// (cmd/gawk-mumble-bridge) enforces the audio half statically.
package bridge

import (
	"log/slog"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
)

// Options is everything the service consumes; config.Config.Options builds
// it from the flags.
type Options struct {
	Listen         string
	InternalListen string
	OpsListen      string
	CertFile       string
	KeyFile        string
	DevCert        bool
	AllowedOrigins []string
	PSK            string
	TicketSecret   []byte
	Relay          RelayOptions
	Mumble         MumbleOptions

	MaxVoiceSessions int
	MaxTempChannels  int
	DrainTimeout     time.Duration
	StatsKey         []byte

	Version string
	Logger  *slog.Logger

	// Timings: zero picks the default. Not flags; tests shorten them.
	Timings Timings
}

// RelayOptions configures the session-link dial (bridge → relay).
type RelayOptions struct {
	// URL is the relay's https://host:port.
	URL string
	// ServerName is the TLS name verified on the dial.
	ServerName string
	// CAFile adds roots; empty uses the system roots.
	CAFile string
}

// MumbleOptions configures every Mumble connection.
type MumbleOptions struct {
	Addr                string
	Password            string
	AccessToken         string
	TLS                 mumble.TLSOptions
	ClientVersion       mumble.Version
	NameTemplate        string
	ObserverName        string
	TempParent          string
	TempChannelTemplate string
}

// Timings are the service's internal timers.
type Timings struct {
	// HelloTimeout bounds the browser's first stream and VoiceHello.
	HelloTimeout time.Duration
	// LinkTimeout bounds the session-link dial and the relay's answer.
	LinkTimeout time.Duration
	// NoticeSettle is the gap between the in-band SessionClosing and the
	// close (docs/59).
	NoticeSettle time.Duration
	// TempChannelWait is how long a user's pod waits for the observer's
	// temporary channel before creating it itself.
	TempChannelWait time.Duration
	// RosterCoalesce coalesces bursts of Mumble changes into one Roster.
	RosterCoalesce time.Duration
	// ObserverBackoff and ObserverBackoffMax pace the observer's retries.
	ObserverBackoff    time.Duration
	ObserverBackoffMax time.Duration
	// MumblePing is the Mumble keepalive.
	MumblePing time.Duration
	// MumbleDial bounds one Mumble connect and login.
	MumbleDial time.Duration
	// ChannelEnter bounds joining or creating the room's channel.
	ChannelEnter time.Duration
}

func (t Timings) withDefaults() Timings {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&t.HelloTimeout, 10*time.Second)
	def(&t.LinkTimeout, 10*time.Second)
	def(&t.NoticeSettle, 250*time.Millisecond)
	def(&t.TempChannelWait, 3*time.Second)
	def(&t.RosterCoalesce, 50*time.Millisecond)
	def(&t.ObserverBackoff, time.Second)
	def(&t.ObserverBackoffMax, 30*time.Second)
	def(&t.MumblePing, mumble.DefaultPingInterval)
	def(&t.MumbleDial, mumble.DefaultDialTimeout)
	def(&t.ChannelEnter, 10*time.Second)
	return t
}

// audioQueueLen is each bridged connection's receive queue: 2.5 s of one
// speaker's 10 ms frames, or a few speakers' worth of a scheduling stall.
const audioQueueLen = 256
