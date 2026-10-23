package poplink

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// Record type names: the "t" of every frame. A name is the protocol; never
// rename one, add a new type instead.
const (
	// PoP → home.
	THello   = "hello"
	TReserve = "reserve"
	TRelease = "release"
	TState   = "state"
	TWall    = "wall"
	TRefuse  = "refuse"
	TCSR     = "csr"
	TStats   = "stats"
	THealth  = "health"
	// Home → PoP.
	TCredential  = "credential"
	TConfig      = "config"
	TReserved    = "reserved"
	TWant        = "want"
	TBan         = "ban"
	TCertificate = "certificate"
	TBye         = "bye"
	// The first record on a room-proxy bidi stream (W3-wall).
	TRoomOpen = "roomOpen"
)

// Enumerated string values.
const (
	WallAttach = "attach"
	WallDetach = "detach"

	RefuseBudget  = "budget"
	RefuseNotLive = "not-live"
	RefuseUnknown = "unknown"
	RefuseBanned  = "banned"

	ReservedOK       = "ok"
	ReservedConflict = "conflict"
	ReservedLimit    = "limit"

	// Bye reasons (docs/76 item 11): the console tells them apart, which
	// close code 4006 alone cannot.
	ByeRepair  = "repair"
	ByeRevoked = "revoked"
	ByeExpired = "expired"
)

// DefaultRelayPort is the port a PoP relay serves WebTransport on unless
// gawk-relay -addr says otherwise.
const DefaultRelayPort = 4433

// Hello is the PoP's first record on a link, and is sent again whenever
// what it carries changes (a certificate rotation, a new LAN address).
// NamedHost is the name the PoP already holds a certificate for, so home
// can tell a fresh install from a restart. LanAddr is an IP literal, with
// ":port" when the relay listens elsewhere than DefaultRelayPort.
type Hello struct {
	Version       string                `json:"version,omitempty"`
	Proto         int                   `json:"proto,omitempty"`
	LanAddr       string                `json:"lanAddr,omitempty"`
	LanCandidates []string              `json:"lanCandidates,omitempty"`
	PinnedHashes  []string              `json:"pinnedHashes,omitempty"`
	NamedHost     string                `json:"namedHost,omitempty"`
	Health        []adminapi.HealthLine `json:"health,omitempty"`
}

// Reserve asks home to reserve broadcast IDs minted at the PoP; home
// answers one Reserved per ID.
type Reserve struct {
	IDs []string `json:"ids,omitempty"`
}

// Release gives reserved IDs back.
type Release struct {
	IDs []string `json:"ids,omitempty"`
}

// State is a broadcast's live/away state at the PoP.
type State struct {
	ID               string `json:"id,omitempty"`
	Live             bool   `json:"live,omitempty"`
	Label            string `json:"label,omitempty"`
	PublisherLanAddr string `json:"publisherLanAddr,omitempty"`
}

// Wall attaches a broadcast to, or detaches it from, the party wall.
type Wall struct {
	Op    string `json:"op,omitempty"`
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
}

// Refuse answers a Want the PoP cannot meet.
type Refuse struct {
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// CSR carries the named tier's certificate request.
type CSR struct {
	PEM string `json:"pem,omitempty"`
}

// Stats is the PoP's periodic use report. Uplink and Downlink are the
// host-lowered ceilings (zero fields: not lowered).
type Stats struct {
	UplinkMbps      float64                 `json:"uplinkMbps,omitempty"`
	UplinkStreams   int                     `json:"uplinkStreams,omitempty"`
	Uplink          pops.Budget             `json:"uplink,omitempty"`
	Downlink        pops.Budget             `json:"downlink,omitempty"`
	DownlinkStreams int                     `json:"downlinkStreams,omitempty"`
	GuestsConnected int                     `json:"guestsConnected,omitempty"`
	Broadcasts      []adminapi.PopBroadcast `json:"broadcasts,omitempty"`
}

// Health is the PoP's self-check, sent when it changes.
type Health struct {
	Lines []adminapi.HealthLine `json:"lines,omitempty"`
}

// Credential is the first record home sends after a pairing redeem, once.
type Credential struct {
	Credential string `json:"credential,omitempty"`
}

// Config is what home tells a linked PoP, sent after its Hello and again
// whenever the Pop changes.
type Config struct {
	PopName         string      `json:"popName,omitempty"`
	UID             string      `json:"uid,omitempty"`
	DisplayName     string      `json:"displayName,omitempty"`
	HomeOrigin      string      `json:"homeOrigin,omitempty"`
	WallCode        string      `json:"wallCode,omitempty"`
	NamedHost       string      `json:"namedHost,omitempty"`
	Uplink          pops.Budget `json:"uplink,omitempty"`
	Downlink        pops.Budget `json:"downlink,omitempty"`
	MaxBroadcasts   int         `json:"maxBroadcasts,omitempty"`
	AutoSwitch      bool        `json:"autoSwitch,omitempty"`
	ScreeningNotice bool        `json:"screeningNotice,omitempty"`
	HomeRooms       bool        `json:"homeRooms,omitempty"`
	NotAfter        *time.Time  `json:"notAfter,omitempty"`
}

// Reserved answers one Reserve ID.
type Reserved struct {
	ID     string `json:"id,omitempty"`
	Result string `json:"result,omitempty"`
}

// Want asks the PoP to forward a broadcast home (an outside viewer).
type Want struct {
	ID string `json:"id,omitempty"`
}

// Ban forwards an ID ban for a broadcast the PoP owns. Until nil is
// permanent. An Until already past lifts a ban forwarded earlier on the
// link (an operator removed it, or ended a cooldown early): home sends
// BanLifted, which a PoP that predates lifting ignores as an expired ban.
type Ban struct {
	ID    string     `json:"id,omitempty"`
	Until *time.Time `json:"until,omitempty"`
}

// BanLifted is the Until of a lift record: any time in the past would do.
var BanLifted = time.Unix(1, 0).UTC()

// Certificate delivers the named tier's chain for one CSR.
type Certificate struct {
	ChainPEM     string `json:"chainPEM,omitempty"`
	ForCSRSHA256 string `json:"forCSRSHA256,omitempty"`
}

// Bye is the last record before home closes the link with 4006.
type Bye struct {
	Reason string `json:"reason,omitempty"`
}

// RoomOpen is the first record on a room-proxy stream; raw room-control
// bytes follow it.
type RoomOpen struct {
	Path     string `json:"path,omitempty"`
	RawQuery string `json:"rawQuery,omitempty"`
}

// factories maps a type name to a fresh record.
var factories = map[string]func() any{
	THello:       func() any { return new(Hello) },
	TReserve:     func() any { return new(Reserve) },
	TRelease:     func() any { return new(Release) },
	TState:       func() any { return new(State) },
	TWall:        func() any { return new(Wall) },
	TRefuse:      func() any { return new(Refuse) },
	TCSR:         func() any { return new(CSR) },
	TStats:       func() any { return new(Stats) },
	THealth:      func() any { return new(Health) },
	TCredential:  func() any { return new(Credential) },
	TConfig:      func() any { return new(Config) },
	TReserved:    func() any { return new(Reserved) },
	TWant:        func() any { return new(Want) },
	TBan:         func() any { return new(Ban) },
	TCertificate: func() any { return new(Certificate) },
	TBye:         func() any { return new(Bye) },
	TRoomOpen:    func() any { return new(RoomOpen) },
}

// TypeOf returns a record's type name, value or pointer, or "" for
// anything that is not a record.
func TypeOf(rec any) string {
	switch rec.(type) {
	case Hello, *Hello:
		return THello
	case Reserve, *Reserve:
		return TReserve
	case Release, *Release:
		return TRelease
	case State, *State:
		return TState
	case Wall, *Wall:
		return TWall
	case Refuse, *Refuse:
		return TRefuse
	case CSR, *CSR:
		return TCSR
	case Stats, *Stats:
		return TStats
	case Health, *Health:
		return THealth
	case Credential, *Credential:
		return TCredential
	case Config, *Config:
		return TConfig
	case Reserved, *Reserved:
		return TReserved
	case Want, *Want:
		return TWant
	case Ban, *Ban:
		return TBan
	case Certificate, *Certificate:
		return TCertificate
	case Bye, *Bye:
		return TBye
	case RoomOpen, *RoomOpen:
		return TRoomOpen
	}
	return ""
}
