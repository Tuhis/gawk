// Package hub implements the relay's pub/sub core: a registry of broadcast
// sessions, where each broadcast has a publisher fanning encoded video out to a
// small set of subscribers.
//
// The hub is a byte forwarder on two channels (docs/12):
//   - Delta frames travel as datagrams, forwarded verbatim; every subscriber
//     owns a bounded queue drained by its own goroutine, and a full queue drops
//     the datagram for that subscriber so a slow peer never blocks others.
//   - Keyframes travel as reliable unidirectional streams. Ingest reads each
//     one into a bounded buffer (which doubles as the cached keyframe), then
//     fan-out writes it to one uni stream per subscriber on a per-subscriber
//     goroutine with a write deadline. A stalled subscriber is cancelled and
//     recovers at the next keyframe; ingest never touches a subscriber stream.
package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/clientinfo"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/internal/mediaprobe"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Sentinel errors. Check with errors.Is.
var (
	// ErrPublisherActive is returned by StartPublish while another publisher
	// holds the slot.
	ErrPublisherActive = errors.New("hub: a publisher is already active")
	// ErrFull is returned by Subscribe when MaxSubscribers is reached.
	ErrFull = errors.New("hub: subscriber limit reached")
	// ErrNotFound is returned when the requested broadcast ID does not exist.
	ErrNotFound = errors.New("hub: broadcast not found")
	// ErrMaxBroadcasts is returned by StartPublish when MaxBroadcasts limit is reached.
	ErrMaxBroadcasts = errors.New("hub: max concurrent broadcasts reached")
	// ErrTotalSubscribers is returned by Subscribe when MaxTotalSubscribers limit is reached.
	ErrTotalSubscribers = errors.New("hub: total subscriber limit reached")
)

// KeyframeOpenFailEvictThreshold is the number of *consecutive* keyframe
// stream-open failures after which a subscriber is evicted with
// wire.CloseCodeSubscriberUnresponsive. Persistent open failure means the
// peer's uni-stream credit is exhausted (docs/14): it never recovers, and
// without eviction /statusz counts a ghost viewer forever. 10 misses ≈ 5 s at
// the default GOP; the code is non-terminal, so a live client just
// reconnects. A constant, not a knob: this is leak cleanup.
const KeyframeOpenFailEvictThreshold = 10

// KeyframeSlowEvictThreshold is the same rule for opens that *succeed* while
// every write stalls out: streams are flow-controlled but datagrams are not,
// so a wedged stream path keeps taking deltas while no keyframe can land, and
// the viewer freezes on "awaiting keyframe" with no signal to either end.
const KeyframeSlowEvictThreshold = 10

// CarrierOpenFailEvictThreshold evicts a reliable subscriber whose carrier
// stream keeps failing to open. It needs its own streak: the keyframe opens
// first and the carrier lazily, so under scarce stream credit the carrier
// loses, and a shared streak zeroed by each keyframe open would leave the
// viewer at keyframe-only playback (2 fps) forever. A failed carrier *write*
// is not counted: dropping a stalled GOP's tail is the mode working.
const CarrierOpenFailEvictThreshold = 10

// CarrierWriteTimeout bounds how long ONE record write to a reliable carrier
// may block on flow control before the carrier is abandoned (docs/24). Not
// KeyframeWriteTimeout: a carrier record is written by the drain goroutine
// that owns the subscriber's entire delta path, so this deadline is the
// freeze every delta behind it inherits. One GOP (500 ms) is the natural
// bound — the next keyframe rotates the carrier anyway.
const CarrierWriteTimeout = 500 * time.Millisecond

// AudioSidebandQueueDepth bounds the reliable subscriber's audio lane: ~1.3 s
// of 50/s Opus packets, enough to absorb a scheduling hiccup and one
// CarrierWriteTimeout. Deliberately not QueueDepth, which is sized for video
// chunks and would let audio queue 20 s deep.
const AudioSidebandQueueDepth = 64

// DefaultDVRWindow and DefaultDVRMaxBytes bound the DVR ring (docs/26). The
// window is how long a stall the mode can hide and must exceed the viewer's
// playout buffer, which must strictly exceed the stall it covers. The byte cap
// is what protects the pod: 3 s of a 50 Mbps broadcaster is 18 MB.
const (
	DefaultDVRWindow   = 3 * time.Second
	DefaultDVRMaxBytes = 24 << 20
)

// MinDVRBufferMs is the smallest viewer buffer worth serving from a ring.
// Below it every replayed record would arrive late, so the subscriber is
// *downgraded* to plain carrier delivery — a query param must never reject a
// session.
const MinDVRBufferMs = 1000

// DefaultDVRProgressTimeout is how long a DVR subscriber may write nothing at
// all before the relay calls it unreachable (docs/26). Lag is this mode's
// point, so health is progress. The timer runs only while the ring holds data
// the cursor cannot get out (dvrNoteIdle), and 6 s of nothing against a 3 s
// ring has already lost every frame in it; a longer value only prolongs a
// wedged viewer's freeze. Matches the viewer's MEDIA_STALL_MS so the two
// backstops fire together.
const DefaultDVRProgressTimeout = 6 * time.Second

// DefaultDVRMaxCatchup is how much faster than live a recovering DVR
// subscriber may send. Covering a stall S with buffer B needs B/(B−S) times
// the bitrate, so 4x carries a 2 s stall on a 2.67 s buffer while still
// bounding a herd recovering from one shared network event. Negative
// disables the ceiling.
const DefaultDVRMaxCatchup = 4.0

// ViewerCountInterval paces the viewer-count pump (docs/23): one
// recompute-and-emit pass per tick, so a reconnect storm can never emit more
// than 1/s/broadcast. ViewerCountKeepalive bounds how long an *unchanged*
// count goes unrepeated: datagrams are lossy, and the re-emit repairs a
// dropped update (new joiners are covered by the join-prime cache). Exported
// because the transport's edge pump reports upstream on the same discipline.
const (
	ViewerCountInterval  = time.Second
	ViewerCountKeepalive = 5 * time.Second
)

// Conn is the connection interface required by subscribers.
// *webtransport.Session is wrapped in an adapter by the transport layer to
// satisfy this.
type Conn interface {
	// SendDatagram delivers one delta datagram (unreliable, may be dropped).
	SendDatagram(payload []byte) error
	// OpenKeyframeStream opens a fresh server-initiated unidirectional stream
	// to this subscriber for one keyframe (reliable).
	OpenKeyframeStream() (KeyframeStream, error)
	// OpenCarrierStream opens a fresh server-initiated unidirectional stream
	// used as a reliable delta carrier (docs/24). A distinct method only to
	// keep the two stream kinds visible in fakes and stats.
	OpenCarrierStream() (KeyframeStream, error)
	CloseWithError(code uint32, reason string) error
}

// SessionCloser is the slice of a publisher's session the hub needs to depose
// it when a newer publisher session takes over the broadcast (docs/06),
// bound after the session upgrade via Publisher.BindConn.
type SessionCloser interface {
	CloseWithError(code uint32, reason string) error
}

// KeyframeStream is the minimal write side of a unidirectional stream the hub
// uses to deliver one keyframe, or to carry a resilient subscriber's delta
// records.
type KeyframeStream interface {
	SetWriteDeadline(t time.Time) error
	Write(p []byte) (int, error)
	Close() error
	// CancelWrite aborts the stream with a reset (used to supersede a stale
	// in-flight keyframe or abandon a stalled subscriber).
	CancelWrite()
}

// Options configures a Registry.
type Options struct {
	// MaxSubscribers caps concurrent subscribers per broadcast; Subscribe returns ErrFull
	// beyond it. Defaults to 15.
	MaxSubscribers int
	// QueueDepth is the per-subscriber datagram queue capacity. Defaults to
	// 1024.
	QueueDepth int

	// DVR bounds the per-broadcast DVR ring (docs/26). Only allocated for
	// broadcasts that actually have a DVR subscriber.
	DVR DVROptions

	// DVRAudio puts audio in the ring too. Off leaves audio on the live-edge
	// sideband.
	DVRAudio bool

	// LiveEdgeAudioOnReliableStream gives plain live-edge viewers the audio carrier that
	// reliable and DVR ones always get. Off by default on purpose: a
	// resilient viewer buffers 150–2000 ms, so a retransmit is free, while a
	// live-edge viewer holds only ~90–150 ms of audio (docs/20) and a
	// retransmit costs a stall once the RTT exceeds it, where the same loss is
	// otherwise a concealed 20 ms gap. Video deltas stay datagrams either way.
	LiveEdgeAudioOnReliableStream bool
	// ParityDefault is the fleet's forward-parity level (docs/34 §5.3): how
	// many parity symbols producers are asked to emit per delta frame, and the
	// ceiling on what any subscriber can be served. 0 disables the feature
	// fleet-wide: no capability is advertised, so producers emit nothing.
	ParityDefault int

	// StripedDelivery enables stripe legs (docs/35). The transport owns the
	// dial gate and capability bit; this mirror keeps registryOptions complete
	// (guarded by the carry-all-limits test).
	StripedDelivery bool

	// StripeLegLease is how long a stripe leg may go without any inbound
	// datagram before it is reaped as orphaned. Defaults to
	// DefaultStripeLegLease; for tests only, deliberately no flag.
	StripeLegLease time.Duration

	// DVRMaxCatchup caps how much faster than live a recovering DVR subscriber
	// may send, as a multiple of the broadcast's own bitrate. Negative
	// disables the ceiling.
	DVRMaxCatchup float64

	// DVRProgressTimeout is how long a DVR subscriber may make no progress at
	// all before it is evicted (see DefaultDVRProgressTimeout).
	DVRProgressTimeout time.Duration
	// BroadcastGrace is the amount of time a broadcast ID survives after its
	// publisher disconnects, allowing it to be reclaimed. Defaults to 5 minutes.
	BroadcastGrace time.Duration

	// PublisherStallTimeout is how long a connected publisher may send NO
	// datagram at all before the broadcast is reported STALLED (not live in
	// BroadcastState, publisherStalled in /statusz). Any datagram counts:
	// every broadcaster pings TimeSync every 2 s while its page runs, while a
	// frozen page — whose QUIC keepalives the browser still answers — sends
	// nothing. Chrome wakes a tab hidden > 5 min once a minute, so a value
	// under ~90 s makes such a tab flap. 0 disables.
	PublisherStallTimeout time.Duration
	// PublisherStallEnds ends a broadcast stalled for BroadcastGrace with
	// the terminal 4000 (everyone, the publisher included) and frees its
	// slot. Off by default: the default relay only reports a stall; ending is
	// the operator's opt-in.
	PublisherStallEnds bool

	MaxBroadcasts       int
	MaxTotalSubscribers int
	MaxBandwidthBytes   int64

	// MaxKeyframeBytes caps a single keyframe stream message (header + config +
	// payload); a publisher stream exceeding it is cancelled and not cached.
	// Defaults to wire.MaxKeyframeBytes.
	MaxKeyframeBytes int
	// KeyframeWriteTimeout bounds how long a single keyframe write to one
	// subscriber may block on flow control before the stream is cancelled and
	// the subscriber recovers at the next keyframe. Defaults to 1s.
	KeyframeWriteTimeout time.Duration

	// Cluster-mode lifecycle hooks (docs/22), nil in single-pod mode, invoked
	// OUTSIDE the registry lock (they do Kubernetes API I/O):
	// OnPublisherClosed when the grace timer starts (the Lease stops renewing);
	// OnBroadcastExpired when grace-GC deletes the hub (the Lease is deleted).
	OnPublisherClosed  func(broadcastID string)
	OnBroadcastExpired func(broadcastID string)
	// OnPublisherStalled reports a stall transition, fired by
	// SweepStalledPublishers outside the lock, once per transition, ORIGIN
	// hubs only, so a room homed on another pod can show the tile away
	// (docs/44 §4.9). Nil in single-pod mode.
	OnPublisherStalled func(broadcastID string, stalled bool)
	// OnOriginEnded reports an ORIGIN hub's removal (grace expiry, end, or
	// termination) with its lifetime and peak audience, for the usage
	// histograms (docs/61). Invoked outside the lock; nil disables it. A hub
	// that never had a publisher reports nothing.
	OnOriginEnded func(BroadcastEnd)
	// OnEvent receives one bus event per broadcast lifecycle transition
	// (docs/51): started, ended with its reason, the away/back pair and
	// the coalesced viewer counts. It MUST NOT block — most call sites hold
	// the registry lock, and the publisher's contract is a non-blocking send.
	// Nil is the off switch.
	OnEvent func(eventbus.Event)

	// IDReserved reports whether a freshly minted broadcast ID names a live
	// room (docs/44 §4.2): /publish never mints an ID that a room owns, which
	// is the hub's half of keeping the two code namespaces disjoint. Nil (the
	// -rooms-off shape) reserves nothing.
	IDReserved func(id string) bool

	// StatsKey keys ObfuscateID (docs/22): 32 bytes, shared across the fleet
	// so one broadcast keeps ONE obfuscated identity in every pod's /statusz
	// and gawk_broadcast_* series. Empty falls back to a fresh per-process key.
	StatsKey []byte
}

// carrierWriteTimeout is the deadline budget for placing ONE record on a
// carrier (the open's prologue and the record write share it):
// CarrierWriteTimeout, or KeyframeWriteTimeout when that is tighter.
func (o Options) carrierWriteTimeout() time.Duration {
	if o.KeyframeWriteTimeout > 0 && o.KeyframeWriteTimeout < CarrierWriteTimeout {
		return o.KeyframeWriteTimeout
	}
	return CarrierWriteTimeout
}

// BroadcastEnd is one origin broadcast's lifetime, reported through
// Options.OnOriginEnded when its hub is removed. Duration runs from the hub's
// creation to its publisher's last disconnect, so the grace period is not
// counted; PeakViewers is the largest global viewer count the origin
// computed (stripe legs excluded).
type BroadcastEnd struct {
	Duration    time.Duration
	PeakViewers uint32
}

// KeyframeDrops breaks keyframe-stream drops down by cause. The split is what
// makes the counter diagnostic: "superseded" is benign (a newer keyframe
// replaced an in-flight one), "slow" is a stalling subscriber, "bandwidth" is
// the configured egress cap, "open_failed" is a session-level open failure.
type KeyframeDrops struct {
	Superseded uint64 `json:"superseded"`
	Slow       uint64 `json:"slow"`
	Bandwidth  uint64 `json:"bandwidth"`
	OpenFailed uint64 `json:"openFailed"`
}

// Total is the sum across all causes.
func (k KeyframeDrops) Total() uint64 {
	return k.Superseded + k.Slow + k.Bandwidth + k.OpenFailed
}

func (k *KeyframeDrops) add(o KeyframeDrops) {
	k.Superseded += o.Superseded
	k.Slow += o.Slow
	k.Bandwidth += o.Bandwidth
	k.OpenFailed += o.OpenFailed
}

// SubscriberStats is the per-subscriber breakdown inside a broadcast's Stats,
// keyed by a random per-session key (never anything joinable or identifying).
// Deliberately JSON-only: per-subscriber Prometheus labels are series churn.
type SubscriberStats struct {
	Key string `json:"key"` // random per-session key, stable across /statusz polls
	// SessionID is the telemetry join key (docs/33): the same handle this
	// viewer's own reports carry. Omitted when telemetry is disabled.
	SessionID        string `json:"sessionId,omitempty"`
	QueueDepth       int    `json:"queueDepth"` // current datagram queue occupancy
	Dropped          uint64 `json:"dropped"`
	SendErrors       uint64 `json:"sendErrors"`
	KeyframesSent    uint64 `json:"keyframesSent"`
	KeyframesDropped uint64 `json:"keyframesDropped"`
	// Internal marks a downstream edge session — plumbing, not a
	// viewer; excluded from the Subscribers counts.
	Internal bool `json:"internal,omitempty"`
	// Reliable marks a resilient subscriber (docs/24); the carrier counters
	// below are zero (and omitted) for datagram subscribers.
	Reliable bool `json:"reliable,omitempty"`
	// Parity served vs. asked for (docs/34): two numbers so a fleet ceiling
	// or delivery-mode suppression is distinguishable from an opt-down.
	ParityK         int `json:"parityK,omitempty"`
	ParityRequested int `json:"parityRequested,omitempty"`
	// Striped delivery (docs/35). StripeLeg marks a leg session, with its
	// (member, n) filter; Striped marks a primary whose deltas its legs
	// currently carry, with the width the viewer last reported.
	StripeLeg             bool   `json:"stripeLeg,omitempty"`
	StripeN               int    `json:"stripeN,omitempty"`
	StripeMember          int    `json:"stripeMember,omitempty"`
	Striped               bool   `json:"striped,omitempty"`
	CarrierStreams        uint64 `json:"carrierStreams,omitempty"`
	CarrierRecords        uint64 `json:"carrierRecords,omitempty"`
	CarrierRecordsDropped uint64 `json:"carrierRecordsDropped,omitempty"`
	CarrierQueueOverflow  uint64 `json:"carrierQueueOverflow,omitempty"`

	// DVR (docs/26). Lag is NOT a health signal in this mode: a large
	// DVRLagMs with DVRResyncs flat and DVRGopSeq climbing is a viewer riding
	// out a bad link as designed. Resyncs are the mode's only frame loss.
	DVR         bool   `json:"dvr,omitempty"`
	DVRBufferMs int    `json:"dvrBufferMs,omitempty"`
	DVRLagMs    int64  `json:"dvrLagMs,omitempty"`
	DVRGopSeq   int64  `json:"dvrGopSeq,omitempty"`
	DVRResyncs  uint64 `json:"dvrResyncs,omitempty"`
}

// Stats is a point-in-time snapshot of hub state, for logging and the
// GET /statusz endpoint (the json tags are its response shape).
type Stats struct {
	PublisherActive bool `json:"publisherActive"`
	// PublisherSessionID is the telemetry session handle of the CURRENT
	// publisher session (docs/33), cleared when it goes away. Omitted when
	// telemetry is disabled.
	PublisherSessionID string `json:"publisherSessionId,omitempty"`
	// Role of this hub in the federation: "origin" (hosts the real publisher;
	// the only role in single-pod mode) or "edge" (derived state fed by an
	// upstream pull, docs/22).
	Role string `json:"role"`
	// Subscribers counts local viewers only; internal edge sessions are
	// accounted separately (EdgeSessions).
	Subscribers int `json:"subscribers"`
	// EdgeSessions counts downstream edge pods attached via the internal
	// subscribe route.
	EdgeSessions int `json:"edgeSessions"`
	// ViewersGlobal is the global viewer count this pod computes as origin
	// (local viewers + Σ edge downstream reports); always 0 on edge hubs,
	// which receive the number from upstream instead (docs/23).
	ViewersGlobal             uint32 `json:"viewersGlobal"`
	FramesRelayed             uint64 `json:"framesRelayed"`    // deltas (datagram, chunk 0) + keyframes (stream)
	DatagramsRelayed          uint64 `json:"datagramsRelayed"` // delta datagrams fanned out (before per-sub drops)
	DatagramsDropped          uint64 `json:"datagramsDropped"` // per-subscriber datagram drops: queue overflows + bandwidth-limit drops
	BadDatagrams              uint64 `json:"badDatagrams"`     // unparseable/unknown datagrams dropped
	BandwidthDroppedDatagrams uint64 `json:"bandwidthDroppedDatagrams"`
	BandwidthDroppedBytes     uint64 `json:"bandwidthDroppedBytes"`
	HasConfig                 bool   `json:"hasConfig"`        // cached keyframe embeds a decoder config
	CachedKeyframeID          uint32 `json:"cachedKeyframeId"` // frameID of the cached keyframe (stream)
	CachedKeyframeBytes       int    `json:"cachedKeyframeBytes"`
	KeyframeStreamsIn         uint64 `json:"keyframeStreamsIn"`       // keyframes ingested from the publisher
	KeyframeBytesIn           uint64 `json:"keyframeBytesIn"`         // total bytes of ingested keyframes
	KeyframeStreamsSent       uint64 `json:"keyframeStreamsSent"`     // keyframe streams fully delivered to subscribers
	KeyframeStreamsDropped    uint64 `json:"keyframeStreamsDropped"`  // sum of KeyframeDrops causes
	KeyframeStreamsOversize   uint64 `json:"keyframeStreamsOversize"` // publisher streams rejected over MaxKeyframeBytes
	GraceRemainingSeconds     int    `json:"graceRemainingSeconds"`   // 0 while publisher is active
	// PublisherStalled: connected, but no datagram for PublisherStallTimeout;
	// StalledSeconds is how long it has been silent (0 unless stalled).
	PublisherStalled bool `json:"publisherStalled"`
	StalledSeconds   int  `json:"stalledSeconds"`

	// Ingress = publisher→relay, egress = relay→subscribers (docs/13); the
	// lost counters come from the ingress window (ingress.go).
	KeyframeDrops        KeyframeDrops     `json:"keyframeDrops"`
	SendErrors           uint64            `json:"sendErrors"`           // datagram write failures to subscribers
	IngressDatagramBytes uint64            `json:"ingressDatagramBytes"` // valid delta/config datagram bytes from the publisher
	EgressDatagramBytes  uint64            `json:"egressDatagramBytes"`  // datagram bytes actually written to subscribers
	EgressKeyframeBytes  uint64            `json:"egressKeyframeBytes"`  // keyframe stream bytes fully delivered
	IngressFramesLost    uint64            `json:"ingressFramesLost"`    // frames the publisher sent that never arrived
	IngressChunksLost    uint64            `json:"ingressChunksLost"`    // missing chunks of frames that did arrive
	SubscriberDetails    []SubscriberStats `json:"subscriberDetails"`

	// Forward parity (docs/34): symbols forwarded vs. suppressed for this
	// broadcast. Omitted when zero.
	ParityDatagramsForwarded uint64 `json:"parityDatagramsForwarded,omitempty"`
	ParitySuppressed         uint64 `json:"paritySuppressed,omitempty"`
	// EgressParityBytes is parity's share of EgressDatagramBytes — a SLICE of
	// that total, not a sibling; summing the two would double-count.
	EgressParityBytes uint64 `json:"egressParityBytes,omitempty"`

	// Striped delivery. StripeLegs (also counted in Subscribers) are live leg
	// sessions; StripedPrimaries are viewers whose primary is currently
	// suppressed. Omitted when zero.
	StripeLegs                int    `json:"stripeLegs,omitempty"`
	StripedPrimaries          int    `json:"stripedPrimaries,omitempty"`
	StripeSuppressedDatagrams uint64 `json:"stripeSuppressedDatagrams,omitempty"`
	StripeTransitions         uint64 `json:"stripeTransitions,omitempty"`
	// StripeLegsReaped counts leg sessions the relay ended as orphaned (owner
	// closed, or lease expired). Non-zero says viewers are losing primaries
	// without tearing their legs down.
	StripeLegsReaped uint64 `json:"stripeLegsReaped,omitempty"`

	// Reliable delivery (docs/24).
	ReliableSubscribers   int    `json:"reliableSubscribers"`   // live local subscribers in reliable mode
	CarrierStreams        uint64 `json:"carrierStreams"`        // carrier streams opened
	CarrierRecords        uint64 `json:"carrierRecords"`        // records fully written to carriers
	CarrierRecordsDropped uint64 `json:"carrierRecordsDropped"` // records dropped: dead carrier, open/write failure
	// CarrierQueueOverflow counts deltas dropped at the queue because a
	// reliable subscriber's drain fell behind — a slice of DatagramsDropped,
	// not a separate budget.
	CarrierQueueOverflow uint64 `json:"carrierQueueOverflow"`
	EgressCarrierBytes   uint64 `json:"egressCarrierBytes"` // carrier bytes written (prologues + records)

	// DVR. DVRRingBytes/DVRRingGops is what the window costs right now — the
	// number to watch against -dvr-max-bytes. DVRResyncs counts viewers whose
	// stall outlived the ring.
	DVRSubscribers int    `json:"dvrSubscribers,omitempty"`
	DVRRingBytes   int    `json:"dvrRingBytes,omitempty"`
	DVRRingGops    int    `json:"dvrRingGops,omitempty"`
	DVRResyncs     uint64 `json:"dvrResyncs,omitempty"`

	// Usage labels (docs/61), metrics-only: never on /statusz.
	// Client is the publisher's self-description; Codec is the codec family
	// and CodedHeight the coded height read from its media (0 = not probed).
	Client      clientinfo.Info `json:"-"`
	Codec       string          `json:"-"`
	CodedHeight int             `json:"-"`
}

// TotalStats aggregates stats across all active and past broadcasts.
type TotalStats struct {
	Broadcasts                int    `json:"broadcasts"`
	Subscribers               int    `json:"subscribers"`
	FramesRelayed             uint64 `json:"framesRelayed"`
	DatagramsRelayed          uint64 `json:"datagramsRelayed"`
	DatagramsDropped          uint64 `json:"datagramsDropped"`
	BadDatagrams              uint64 `json:"badDatagrams"`
	BandwidthDroppedDatagrams uint64 `json:"bandwidthDroppedDatagrams"`
	BandwidthDroppedBytes     uint64 `json:"bandwidthDroppedBytes"`
	KeyframeStreamsIn         uint64 `json:"keyframeStreamsIn"`
	KeyframeBytesIn           uint64 `json:"keyframeBytesIn"`
	KeyframeStreamsSent       uint64 `json:"keyframeStreamsSent"`
	KeyframeStreamsDropped    uint64 `json:"keyframeStreamsDropped"`
	KeyframeStreamsOversize   uint64 `json:"keyframeStreamsOversize"`

	// See the Stats field comments.
	KeyframeDrops        KeyframeDrops `json:"keyframeDrops"`
	SendErrors           uint64        `json:"sendErrors"`
	IngressDatagramBytes uint64        `json:"ingressDatagramBytes"`
	EgressDatagramBytes  uint64        `json:"egressDatagramBytes"`
	EgressKeyframeBytes  uint64        `json:"egressKeyframeBytes"`
	IngressFramesLost    uint64        `json:"ingressFramesLost"`
	IngressChunksLost    uint64        `json:"ingressChunksLost"`

	// The edge-leg (origin→edge) loss windows, from EDGE hubs only — never
	// mixed with the broadcaster-leg numbers above: the two legs have
	// different owners and different fixes.
	EdgeIngressFramesLost uint64 `json:"edgeIngressFramesLost"`
	EdgeIngressChunksLost uint64 `json:"edgeIngressChunksLost"`

	// Reliable delivery — see the Stats field comments.
	ReliableSubscribers   int    `json:"reliableSubscribers"`
	CarrierStreams        uint64 `json:"carrierStreams"`
	CarrierRecords        uint64 `json:"carrierRecords"`
	CarrierRecordsDropped uint64 `json:"carrierRecordsDropped"`
	CarrierQueueOverflow  uint64 `json:"carrierQueueOverflow"`
	EgressCarrierBytes    uint64 `json:"egressCarrierBytes"`
	// Fleet-wide parity forwarding, omitted when zero. EgressParityBytes is a
	// SLICE of EgressDatagramBytes (see Stats).
	ParityDatagramsForwarded uint64 `json:"parityDatagramsForwarded,omitempty"`
	ParitySuppressed         uint64 `json:"paritySuppressed,omitempty"`
	EgressParityBytes        uint64 `json:"egressParityBytes,omitempty"`
	// Fleet-wide stripe accounting. Omitted when zero.
	StripeLegs                int    `json:"stripeLegs,omitempty"`
	StripedPrimaries          int    `json:"stripedPrimaries,omitempty"`
	StripeSuppressedDatagrams uint64 `json:"stripeSuppressedDatagrams,omitempty"`
	StripeTransitions         uint64 `json:"stripeTransitions,omitempty"`
	StripeLegsReaped          uint64 `json:"stripeLegsReaped,omitempty"`
	// DVR — see the Stats field comment. Omitted when zero.
	DVRResyncs uint64 `json:"dvrResyncs,omitempty"`
}

// RegistryStats is the full response structure of GET /statusz.
type RegistryStats struct {
	Totals     TotalStats       `json:"totals"`
	Broadcasts map[string]Stats `json:"broadcasts"` // keyed by obfuscated broadcast ID
}

// Registry owns the map of active broadcasts and cumulative statistics.
type Registry struct {
	log  *slog.Logger
	opts Options

	mu   sync.Mutex
	hubs map[string]*broadcastHub

	totalFramesRelayed             uint64
	totalDatagramsRelayed          uint64
	totalDatagramsDropped          uint64
	totalBadDatagrams              uint64
	totalBandwidthDroppedDatagrams uint64
	totalBandwidthDroppedBytes     uint64
	totalKeyframeStreamsIn         uint64
	totalKeyframeBytesIn           uint64
	totalKeyframeStreamsSent       uint64
	totalKeyframeDrops             KeyframeDrops
	totalKeyframeStreamsOversize   uint64
	totalSendErrors                uint64
	totalIngressDatagramBytes      uint64
	totalEgressDatagramBytes       uint64
	totalEgressKeyframeBytes       uint64
	totalIngressFramesLost         uint64
	totalIngressChunksLost         uint64
	totalEdgeIngressFramesLost     uint64
	totalEdgeIngressChunksLost     uint64
	totalCarrierStreams            uint64
	totalCarrierRecords            uint64
	totalCarrierRecordsDropped     uint64
	totalCarrierQueueOverflow      uint64
	totalEgressCarrierBytes        uint64
	totalDVRResyncs                uint64
	// The expiry fold must carry every per-hub counter, these included: one
	// it misses vanishes with the hub, and the fleet total (a Prometheus
	// counter) goes BACKWARDS.
	totalParityDatagramsForwarded  uint64
	totalParitySuppressed          uint64
	totalEgressParityBytes         uint64
	totalStripeSuppressedDatagrams uint64
	totalStripeTransitions         uint64
	totalStripeLegsReaped          uint64

	limiter *bandwidthLimiter

	// statsKey keys ObfuscateID so /statusz broadcast keys can't be
	// brute-forced back to joinable IDs (see Options.StatsKey).
	statsKey []byte
}

// broadcastHub is the per-broadcast session unit.
type broadcastHub struct {
	registry *Registry
	id       string
	log      *slog.Logger

	publisherActive bool
	// publisherSessionID is the telemetry handle of the session currently
	// holding the publisher slot. Cleared alongside publisherActive so a
	// graced broadcast never attributes observations to an ended session.
	publisherSessionID string
	// publisher is the handle currently holding the slot (nil when inactive).
	// TakeOverPublish needs it to depose the incumbent.
	publisher  *Publisher
	generation uint64
	graceTimer *time.Timer
	graceStart time.Time
	// stalledSince is set by the stall sweep at onset (so it is logged once)
	// and cleared when a datagram returns. stalledReported is what
	// OnPublisherStalled was last told; a reclaim resets stalledSince but not
	// this, so a recovery by reclaim is still reported.
	stalledSince    time.Time
	stalledReported bool
	// edge marks a hub as derived state (docs/22): its "publisher" is this
	// pod's upstream pull from the origin, it is exempt from MaxBroadcasts,
	// and the Lease, not grace, is its liveness truth. Flipped only through
	// setRoleLocked so ingress-loss counts never cross legs.
	edge bool

	subs map[*Subscriber]struct{}

	// cachedKeyframe holds the full StreamFrame message of the last complete
	// keyframe, replayed verbatim to prime late joiners; immutable once set.
	// keyframeSeq is a per-hub monotonic counter (never reset, even across
	// publisher restarts) so a late prime can't supersede a newer keyframe.
	cachedKeyframe          []byte
	cachedKeyframeID        uint32
	cachedKeyframeHasConfig bool
	keyframeSeq             uint64

	// cachedClockMapping holds the latest ClockMapping datagram verbatim
	// (docs/15): relayed live, replayed to prime late joiners, and
	// invalidated on a new publisher session (a new clock timeline).
	cachedClockMapping []byte

	// cachedAudioConfig holds the latest AudioConfig datagram, with the
	// ClockMapping lifecycle (docs/20). The broadcaster re-sends it at 1 Hz,
	// so a cleared cache heals within a second.
	cachedAudioConfig []byte

	// cachedViewerCount holds the latest global ViewerCount datagram verbatim,
	// with the ClockMapping lifecycle (docs/23). On an origin the count pump
	// produces it; on an edge it arrives from upstream and is forwarded
	// verbatim (counts are pod-independent, no per-hop rewrite).
	cachedViewerCount []byte

	// startedAt is when this hub was registered — the BROADCAST's age (it
	// survives grace and reclaims) for GET /internal/admin/broadcasts
	// (docs/42 §4.5). Never exposed on /statusz.
	startedAt time.Time

	// Usage labels and lifetime (docs/61). client is the publisher's
	// self-description; codec and codedHeight are probed from the media and
	// reset per session. publisherLeftAt is the last publisher disconnect.
	client          clientinfo.Info
	codec           string
	avccConfig      bool
	codedHeight     int
	publisherLeftAt time.Time
	peakViewers     uint32

	// dvr is this broadcast's DVR window, nil until a DVR subscriber joins
	// and shared by every one of them.
	dvr *DVRRing
	// dvrAudio is the same window for the audio lane, allocated beside it.
	dvrAudio *DVRAudioRing
	// Count-pump emit tracking (origin hubs only): the last count pushed and
	// when, making emits change-driven with a keepalive re-send.
	lastViewerCount        uint32
	lastViewerCountEmitAt  time.Time
	viewerCountEverEmitted bool

	framesRelayed             uint64
	datagramsRelayed          uint64
	datagramsDropped          uint64
	badDatagrams              uint64
	bandwidthDroppedDatagrams uint64
	bandwidthDroppedBytes     uint64
	keyframeStreamsIn         uint64
	keyframeBytesIn           uint64
	keyframeStreamsOversize   uint64
	ingressDatagramBytes      uint64
	// Ingress-loss window: cumulative counters live here — not on the
	// window — so a publisher-restart window reset can't lose them.
	ingress           ingressWindow
	ingressFramesLost uint64
	ingressChunksLost uint64
	// Folded from subscribers that closed while this hub was still alive
	// (mirrors datagramsDropped); live subscribers are summed on demand.
	keyframeStreamsSent uint64
	keyframeDrops       KeyframeDrops
	sendErrors          uint64
	egressDatagramBytes uint64
	egressKeyframeBytes uint64
	// Carrier counters, folded like the above.
	carrierStreams        uint64
	carrierRecords        uint64
	carrierRecordsDropped uint64
	carrierQueueOverflow  uint64
	// Closed DVR subscribers' resyncs, folded like the above so the
	// per-broadcast counter never goes backwards when a resynced viewer leaves.
	dvrResyncs uint64
	// Parity symbols actually forwarded, and symbols dropped because the
	// subscriber's k was lower.
	parityDatagramsForwarded uint64
	egressParityBytes        uint64
	paritySuppressed         uint64
	egressCarrierBytes       uint64
	// Delta datagrams withheld from striped primaries. A leg's non-matching
	// share is routing, not suppression, and is deliberately not counted.
	stripeSuppressedDatagrams uint64
	stripeTransitions         uint64
	// Leg sessions the relay reaped as orphaned.
	stripeLegsReaped uint64
}

type bandwidthLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBandwidthLimiter(rate float64) *bandwidthLimiter {
	return &bandwidthLimiter{
		rate:   rate,
		burst:  rate,
		tokens: rate,
		last:   time.Now(),
	}
}

func (l *bandwidthLimiter) consume(n int) bool {
	if l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.last).Seconds()
	l.last = now

	l.tokens += elapsed * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}

	if l.tokens >= float64(n) {
		l.tokens -= float64(n)
		return true
	}
	return false
}

// NewRegistry builds a Registry. Zero-valued Options fields get defaults.
func NewRegistry(log *slog.Logger, opts Options) *Registry {
	if opts.MaxSubscribers <= 0 {
		opts.MaxSubscribers = 15
	}
	if opts.DVRMaxCatchup == 0 {
		opts.DVRMaxCatchup = DefaultDVRMaxCatchup
	}
	if opts.DVRProgressTimeout <= 0 {
		opts.DVRProgressTimeout = DefaultDVRProgressTimeout
	}
	if opts.StripeLegLease <= 0 {
		opts.StripeLegLease = DefaultStripeLegLease
	}
	if opts.DVR.Window <= 0 {
		opts.DVR.Window = DefaultDVRWindow
	}
	if opts.DVR.MaxBytes <= 0 {
		opts.DVR.MaxBytes = DefaultDVRMaxBytes
	}
	if opts.QueueDepth <= 0 {
		// ~1024 delta chunks ≈ 2.6 s of 1080p video (~13 chunks per frame).
		// Sized for a reliable subscriber, whose carrier write can park on
		// flow control; at ~0.65 s the queue shed under ordinary stalls. Near
		// inert for datagram subscribers, whose drain never blocks, and their
		// drop-newest policy means depth cannot turn into replayed staleness.
		opts.QueueDepth = 1024
	}
	if opts.BroadcastGrace <= 0 {
		opts.BroadcastGrace = 5 * time.Minute
	}
	if opts.MaxBroadcasts <= 0 {
		opts.MaxBroadcasts = 5
	}
	if opts.MaxTotalSubscribers <= 0 {
		opts.MaxTotalSubscribers = 50
	}
	if opts.MaxKeyframeBytes <= 0 {
		opts.MaxKeyframeBytes = wire.MaxKeyframeBytes
	}
	if opts.KeyframeWriteTimeout <= 0 {
		opts.KeyframeWriteTimeout = time.Second
	}
	var limiter *bandwidthLimiter
	if opts.MaxBandwidthBytes > 0 {
		limiter = newBandwidthLimiter(float64(opts.MaxBandwidthBytes))
	}
	statsKey := opts.StatsKey
	if len(statsKey) != 32 {
		statsKey = make([]byte, 32)
		if _, err := rand.Read(statsKey); err != nil {
			panic("hub: crypto/rand unavailable: " + err.Error())
		}
	}
	return &Registry{
		log:      log,
		opts:     opts,
		hubs:     make(map[string]*broadcastHub),
		limiter:  limiter,
		statsKey: statsKey,
	}
}

// StartPublish claims a publisher slot.
// With an empty id, it mints a new broadcast ID.
// With a non-empty id, it attempts to reclaim the broadcast: ErrNotFound if it doesn't
// exist or has expired, and ErrPublisherActive if another publisher holds it.
func (r *Registry) StartPublish(id string) (string, *Publisher, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if id != "" {
		normID, err := broadcastid.Normalize(id)
		if err != nil {
			return "", nil, ErrNotFound
		}
		id = normID
	} else {
		if r.opts.MaxBroadcasts > 0 && len(r.hubs) >= r.opts.MaxBroadcasts {
			return "", nil, ErrMaxBroadcasts
		}
		var newID string
		var err error
		for range 10 {
			newID, err = broadcastid.Mint()
			if err != nil {
				return "", nil, err
			}
			if _, exists := r.hubs[newID]; !exists && !r.idReserved(newID) {
				break
			}
		}
		if _, exists := r.hubs[newID]; exists || r.idReserved(newID) {
			return "", nil, errors.New("hub: collision limits exceeded minting ID")
		}
		id = newID
		r.newHubLocked(id)
	}

	b, exists := r.hubs[id]
	if !exists {
		return "", nil, ErrNotFound
	}
	pub, err := r.claimPublisherLocked(b)
	if err != nil {
		return "", nil, err
	}
	// A real publisher claim makes (or re-makes) this hub the origin — a
	// prior demote-to-edge is over when the broadcaster comes home.
	r.setRoleLocked(b, false)
	r.busStarted(id)
	return id, pub, nil
}

// ResumePublish claims the publisher slot of a specific broadcast ID,
// creating the hub when this process doesn't know it (docs/22). The caller
// MUST have verified a resume token first — the proof of ownership that
// makes an unknown ID a legitimate resume rather than a 404. Creating counts
// against MaxBroadcasts like a mint.
func (r *Registry) ResumePublish(id string) (string, *Publisher, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return "", nil, ErrNotFound
	}
	b, exists := r.hubs[normID]
	if !exists {
		if r.opts.MaxBroadcasts > 0 && len(r.hubs) >= r.opts.MaxBroadcasts {
			return "", nil, ErrMaxBroadcasts
		}
		b = r.newHubLocked(normID)
	}
	pub, err := r.claimPublisherLocked(b)
	if err != nil {
		return "", nil, err
	}
	// The broadcaster re-homed onto this pod: origin again.
	r.setRoleLocked(b, false)
	r.busStarted(normID)
	return normID, pub, nil
}

// EdgePublish claims the publisher slot of an EDGE hub for a broadcast this
// pod serves via an upstream pull. Creating an edge hub is exempt from
// MaxBroadcasts — edge hubs are derived state, not broadcasts (docs/22).
// The claim resets the prime caches like a real publisher claim; with
// InvalidatePrimes on upstream loss, that makes a stale prime impossible.
func (r *Registry) EdgePublish(id string) (string, *Publisher, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return "", nil, ErrNotFound
	}
	b, exists := r.hubs[normID]
	if !exists {
		b = r.newHubLocked(normID)
	}
	pub, err := r.claimPublisherLocked(b)
	if err != nil {
		return "", nil, err
	}
	// The role flips only on a SUCCESSFUL claim: on ErrPublisherActive the
	// hub may belong to a live origin publisher and must keep its role (and
	// with it the lease lifecycle hooks).
	r.setRoleLocked(b, true)
	return normID, pub, nil
}

// InvalidatePrimes clears the cached primes NOW, when an edge's upstream
// session ends, so a viewer joining before the re-attach is never served
// origin A's prime alongside origin B's deltas. keyframeSeq is not reset.
func (r *Registry) InvalidatePrimes(id string) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, exists := r.hubs[normID]
	if !exists {
		return
	}
	b.cachedKeyframe = nil
	b.cachedKeyframeID = 0
	b.cachedKeyframeHasConfig = false
	b.cachedClockMapping = nil
	// Audio config follows the same rule; the 1 Hz re-send refills it.
	b.cachedAudioConfig = nil
	b.cachedViewerCount = nil
}

// CloseInternalSubscribers closes every downstream edge session of a
// broadcast with the given code (4003 on demote — the edge clients
// re-resolve the lease and re-attach at the new origin). Local
// viewers are untouched: nobody chases viewers across pods.
func (r *Registry) CloseInternalSubscribers(id string, code uint32, reason string) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return
	}
	r.mu.Lock()
	b, exists := r.hubs[normID]
	if !exists {
		r.mu.Unlock()
		return
	}
	var internal []*Subscriber
	for s := range b.subs {
		if s.internal {
			internal = append(internal, s)
		}
	}
	r.mu.Unlock()

	for _, s := range internal {
		_ = s.sender.CloseWithError(code, reason)
		s.Close()
	}
}

// ExternalSubscribers reports the local (non-internal) session count for a
// broadcast — the edge linger signal. Stripe legs COUNT here on purpose: an
// edge serving only another pod's legs is still serving media and must not
// linger out (docs/35 §5.8).
func (r *Registry) ExternalSubscribers(id string) int {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, exists := r.hubs[normID]
	if !exists {
		return 0
	}
	return b.externalSubsLocked()
}

// ViewerSubscribers reports the local WATCHING-HUMAN count for a broadcast —
// the number an edge reports upstream. Unlike ExternalSubscribers it
// excludes stripe legs: a leg keeps an edge alive but must never inflate the
// audience number.
func (r *Registry) ViewerSubscribers(id string) int {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, exists := r.hubs[normID]
	if !exists {
		return 0
	}
	return b.externalHumansLocked()
}

// idReserved is the mint-time room-code check (see Options.IDReserved).
func (r *Registry) idReserved(id string) bool {
	return r.opts.IDReserved != nil && r.opts.IDReserved(id)
}

// BroadcastState is the one-lock read a room needs about an attachment
// (docs/44 §4.7): known at all, publisher live (vs. away within the grace),
// and the fleet-global viewer count G — on an origin the aggregate
// globalViewersLocked computes, on an edge the G its origin last sent down
// (a pod-local count would show only the viewers sharing the home pod).
// Three separate calls would race each other across a grace expiry.
func (r *Registry) BroadcastState(id string) (live bool, viewers int, known bool) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return false, 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, exists := r.hubs[normID]
	if !exists {
		return false, 0, false
	}
	if b.edge {
		return b.publisherActive, b.edgeGlobalViewersLocked(), true
	}
	return b.publisherActive && !b.stalledLocked(time.Now()), int(b.globalViewersLocked()), true
}

// silentForLocked is how long the current publisher has sent no datagram
// (zero without an active publisher). Caller holds r.mu.
func (b *broadcastHub) silentForLocked(now time.Time) time.Duration {
	if !b.publisherActive || b.publisher == nil {
		return 0
	}
	return now.Sub(b.publisher.lastSeenTime())
}

// stalledLocked reports whether the publisher is connected but has sent no
// datagram for Options.PublisherStallTimeout (0 = never stalled). Caller
// holds r.mu. Edge hubs never stall: their "publisher" is an upstream pull
// whose liveness is the Lease.
func (b *broadcastHub) stalledLocked(now time.Time) bool {
	t := b.registry.opts.PublisherStallTimeout
	return t > 0 && !b.edge && b.silentForLocked(now) > t
}

// SweepStalledPublishers is the stall lifecycle, ticked with the viewer-count
// pump: logs a stall's onset once, reports each transition through
// OnPublisherStalled, and — only with Options.PublisherStallEnds — ends a
// broadcast stalled for BroadcastGrace with the terminal 4000, so the
// broadcaster does not resume-loop against a session that would stall again.
func (r *Registry) SweepStalledPublishers(now time.Time) {
	type kick struct {
		id     string
		silent time.Duration
	}
	type transition struct {
		id      string
		stalled bool
	}
	var kicks []kick
	var transitions []transition
	r.mu.Lock()
	for id, b := range r.hubs {
		stalled := b.stalledLocked(now)
		if stalled != b.stalledReported && !b.edge {
			b.stalledReported = stalled
			transitions = append(transitions, transition{id: id, stalled: stalled})
		}
		if !stalled {
			b.stalledSince = time.Time{}
			continue
		}
		silent := b.silentForLocked(now).Truncate(time.Second)
		if b.stalledSince.IsZero() {
			b.stalledSince = now
			if r.opts.PublisherStallEnds && r.opts.BroadcastGrace > 0 {
				b.log.Warn("publisher stalled: connected, no datagrams", "silent", silent, "ends_after", r.opts.BroadcastGrace)
			} else {
				b.log.Warn("publisher stalled: connected, no datagrams", "silent", silent)
			}
		}
		if r.opts.PublisherStallEnds && r.opts.BroadcastGrace > 0 && b.silentForLocked(now) >= r.opts.BroadcastGrace {
			kicks = append(kicks, kick{id: id, silent: silent})
		}
	}
	r.mu.Unlock()
	if hook := r.opts.OnPublisherStalled; hook != nil {
		for _, tr := range transitions {
			hook(tr.id, tr.stalled)
		}
	}
	// The same transitions onto the bus, so away/back cannot flap on the bus
	// without flapping in the fleet.
	for _, tr := range transitions {
		r.busStalled(tr.id, tr.stalled)
	}
	for _, k := range kicks {
		reason := fmt.Sprintf("no datagrams from the publisher for %s", k.silent)
		if r.TerminateBroadcast(k.id, uint32(wire.CloseCodeBroadcastEnded), reason) {
			r.log.Warn("stalled broadcast ended", "broadcast_id", k.id, "silent", k.silent)
		}
	}
}

// edgeGlobalViewersLocked is an edge hub's view of G: the cached ViewerCount
// datagram its origin last sent. Until one arrives the local human count
// stands in: an undercount that heals within a pump tick, never a 0 while
// someone here is watching. Caller holds r.mu.
func (b *broadcastHub) edgeGlobalViewersLocked() int {
	if b.cachedViewerCount != nil {
		if g, err := wire.ParseViewerCount(b.cachedViewerCount); err == nil {
			return int(g)
		}
	}
	return b.externalHumansLocked()
}

// externalHumansLocked counts the watching humans on this hub: external
// subscribers minus stripe legs. Caller holds r.mu.
func (b *broadcastHub) externalHumansLocked() int {
	n := 0
	for s := range b.subs {
		if !s.internal && !s.stripeLeg {
			n++
		}
	}
	return n
}

// newHubLocked creates and registers an empty hub. Caller holds r.mu.
func (r *Registry) newHubLocked(id string) *broadcastHub {
	b := &broadcastHub{
		registry:  r,
		id:        id,
		log:       r.log.With("broadcast_id", id),
		subs:      make(map[*Subscriber]struct{}),
		startedAt: time.Now(),
	}
	r.hubs[id] = b
	return b
}

// claimPublisherLocked takes the hub's publisher slot: cancels a running
// grace timer, bumps the generation, and resets the per-session caches.
// Caller holds r.mu.
func (r *Registry) claimPublisherLocked(b *broadcastHub) (*Publisher, error) {
	if b.publisherActive {
		return nil, ErrPublisherActive
	}

	if b.graceTimer != nil {
		b.graceTimer.Stop()
		b.graceTimer = nil
		b.graceStart = time.Time{}
	}

	b.publisherActive = true
	// The outgoing session's telemetry handle must not be attributed to the
	// new one, which gets its own after the upgrade.
	b.publisherSessionID = ""
	b.generation++
	// A new stall clock (starting at the claim, so a publisher that never
	// sends still stalls) and a fresh onset log.
	b.stalledSince = time.Time{}

	// Reset the keyframe cache on a new publisher session (frameIDs reset, the
	// codec may differ). keyframeSeq is intentionally NOT reset so a keyframe
	// from the new session always outranks any stale prime still in flight.
	b.cachedKeyframe = nil
	b.cachedKeyframeID = 0
	b.cachedKeyframeHasConfig = false
	// The codec may differ too; the new session's first config and keyframe
	// re-probe it. The client is re-stamped by the transport.
	b.codec = ""
	b.avccConfig = false
	b.codedHeight = 0
	// New session, new clock timeline: the old mapping is meaningless.
	b.cachedClockMapping = nil
	// New session, possibly new (or no) audio config; the 1 Hz re-send
	// refills it.
	b.cachedAudioConfig = nil
	// Not session-bound, but clearing keeps the cache lifecycle uniform, and
	// resetting the emit tracking guarantees a fresh emit on the next tick.
	b.cachedViewerCount = nil
	b.viewerCountEverEmitted = false
	// New session, new frameID space: reset the ingress-loss window (its
	// cumulative counters live on the hub and survive).
	b.ingress.reset()

	p := &Publisher{hub: b}
	p.lastSeen.Store(time.Now().UnixNano())
	b.publisher = p
	return p, nil
}

// setRoleLocked flips the hub's federation role. Per-hub ingress-loss
// counters are attributed to the hub's CURRENT role at scrape time, so counts
// accumulated under the old role are folded into that leg's totals before
// the flip. Caller holds r.mu.
func (r *Registry) setRoleLocked(b *broadcastHub, edge bool) {
	if b.edge == edge {
		return
	}
	if b.edge {
		r.totalEdgeIngressFramesLost += b.ingressFramesLost
		r.totalEdgeIngressChunksLost += b.ingressChunksLost
	} else {
		r.totalIngressFramesLost += b.ingressFramesLost
		r.totalIngressChunksLost += b.ingressChunksLost
	}
	b.ingressFramesLost = 0
	b.ingressChunksLost = 0
	b.edge = edge
}

// TakeOverPublish claims the publisher slot for id even when another
// publisher holds it, deposing the incumbent — newest publisher wins
// (docs/06). A silently-dead publisher otherwise holds the slot until the
// QUIC idle timeout, and refusing its own reclaim forces a mint that orphans
// every viewer. It is the same-pod counterpart of the lease force-take: the
// caller MUST have verified the resume token, and MUST call this only after
// the claiming session has completed its upgrade, so a malformed request
// can never depose a healthy publisher. A deposed session (when one is
// bound) is closed with wire.CloseCodePublisherSuperseded, outside the
// lock. The only error is ErrNotFound.
func (r *Registry) TakeOverPublish(id string) (string, *Publisher, error) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return "", nil, ErrNotFound
	}

	r.mu.Lock()
	b, exists := r.hubs[normID]
	if !exists {
		r.mu.Unlock()
		return "", nil, ErrNotFound
	}
	var deposed SessionCloser
	superseded := false
	if old := b.publisher; b.publisherActive && old != nil {
		// Mark the incumbent closed so its late datagrams drop and its
		// deferred Close is a no-op (no grace timer, no lease release — both
		// now belong to the new publisher).
		old.closed = true
		deposed = old.conn
		// Tracked apart from deposed: an incumbent without a bound conn
		// still ended as replaced.
		superseded = true
		b.publisherActive = false
		b.publisher = nil
	}
	pub, err := r.claimPublisherLocked(b)
	if err != nil {
		// Unreachable after the depose above; guards a future refactor.
		r.mu.Unlock()
		return "", nil, err
	}
	// A real publisher claim makes (or re-makes) this hub the origin, same
	// as StartPublish/ResumePublish.
	r.setRoleLocked(b, false)
	r.mu.Unlock()

	// Two bus facts in order — ended (replaced), then started — so a
	// consumer can tell a takeover from a duplicate "started".
	if superseded {
		r.busEnded(normID, events.BroadcastEndedReplaced)
	}
	r.busStarted(normID)

	if deposed != nil {
		b.log.Info("active publisher superseded by token-bearing claim")
		_ = deposed.CloseWithError(uint32(wire.CloseCodePublisherSuperseded), "superseded by a new publisher session")
	}
	return normID, pub, nil
}

// CheckPublishNew is the read-only pre-upgrade check for minting a new
// broadcast: ErrMaxBroadcasts when the registry is at capacity, nil
// otherwise. StartPublish re-checks authoritatively under the same lock.
func (r *Registry) CheckPublishNew() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.opts.MaxBroadcasts > 0 && len(r.hubs) >= r.opts.MaxBroadcasts {
		return ErrMaxBroadcasts
	}
	return nil
}

// externalSubsLocked counts local viewers (internal edge sessions excluded).
// Caller holds r.mu.
func (b *broadcastHub) externalSubsLocked() int {
	n := 0
	for s := range b.subs {
		if !s.internal {
			n++
		}
	}
	return n
}

// globalViewersLocked computes an origin hub's global viewer count G
// (docs/23): local real viewers plus each attached edge's last-reported
// downstream count (0 until it reports — a brief undercount). Edge sessions
// and stripe legs are never counted themselves. Caller holds r.mu.
func (b *broadcastHub) globalViewersLocked() uint32 {
	g := uint64(0)
	for s := range b.subs {
		if s.internal {
			g += s.downstreamViewers.Load()
		} else if !s.stripeLeg {
			g++
		}
	}
	if g > math.MaxUint32 {
		g = math.MaxUint32
	}
	return uint32(g)
}

// PumpViewerCounts runs one tick of the count pump (docs/23): for every
// ORIGIN hub it computes the global viewer count G and — when G changed
// since the last emit or the keepalive elapsed — builds the ViewerCount
// datagram once, caches it, fans it to every subscriber (local viewers and
// edge sessions alike) and pushes it to the publisher. Edge hubs only
// report to the bus: they receive G from upstream and forward it. Exported
// as the test seam; RunViewerCountPump drives it on the production cadence.
func (r *Registry) PumpViewerCounts(now time.Time) {
	type push struct {
		send  func([]byte)
		dgram []byte
	}
	var pushes []push
	r.mu.Lock()
	for _, b := range r.hubs {
		// An *away* publisher is deliberately NOT skipped: its viewers stay
		// attached through the grace period, and this count is then the only
		// app-layer traffic they get — without it a client cannot tell "my
		// session died silently" from "the broadcaster stepped away".
		// The bus hears from BOTH roles, unlike the datagram fan-out: only an
		// edge knows its local viewers, only the origin the fleet-wide number.
		// The bus publisher coalesces both, so this is cheap on every tick.
		local := b.externalHumansLocked()
		if b.edge {
			r.busViewers(b.id, local, 0, true)
			continue
		}
		g := b.globalViewersLocked()
		b.peakViewers = max(b.peakViewers, g)
		r.busViewers(b.id, local, int(g), false)
		if b.viewerCountEverEmitted && g == b.lastViewerCount &&
			now.Sub(b.lastViewerCountEmitAt) < ViewerCountKeepalive {
			continue
		}
		b.lastViewerCount = g
		b.lastViewerCountEmitAt = now
		b.viewerCountEverEmitted = true
		dgram := wire.AppendViewerCount(nil, g)
		b.cacheAndFanViewerCountLocked(dgram)
		if p := b.publisher; p != nil && p.send != nil {
			pushes = append(pushes, push{send: p.send, dgram: dgram})
		}
	}
	r.mu.Unlock()

	// The publisher pushes are network I/O, so off the registry lock.
	for _, p := range pushes {
		p.send(p.dgram)
	}
}

// RunViewerCountPump ticks PumpViewerCounts every ViewerCountInterval until
// ctx ends. Started explicitly from main, never from NewRegistry, so tests
// stay goroutine-free and drive ticks directly.
func (r *Registry) RunViewerCountPump(ctx context.Context) {
	t := time.NewTicker(ViewerCountInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.PumpViewerCounts(now)
			r.SweepStalledPublishers(now)
		}
	}
}

// totalExternalSubsLocked counts local viewers across all hubs. Caller holds r.mu.
func (r *Registry) totalExternalSubsLocked() int {
	n := 0
	for _, b := range r.hubs {
		n += b.externalSubsLocked()
	}
	return n
}

// CheckSubscribe is the read-only pre-upgrade check: ErrNotFound / ErrFull / nil.
func (r *Registry) CheckSubscribe(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return ErrNotFound
	}
	b, exists := r.hubs[normID]
	if !exists {
		return ErrNotFound
	}
	if b.externalSubsLocked() >= r.opts.MaxSubscribers {
		return ErrFull
	}
	if r.opts.MaxTotalSubscribers > 0 && r.totalExternalSubsLocked() >= r.opts.MaxTotalSubscribers {
		return ErrTotalSubscribers
	}
	return nil
}

// Subscribe registers a subscriber, re-checking under lock, and primes it
// with the cached keyframe so it can show a first picture immediately.
func (r *Registry) Subscribe(id string, conn Conn) (*Subscriber, error) {
	return r.subscribe(id, conn, false, false)
}

// SubscribeReliable registers a resilient subscriber (docs/24): its deltas
// are delivered as length-prefixed records on per-GOP reliable carrier
// streams instead of datagrams. Everything else is identical to Subscribe.
func (r *Registry) SubscribeReliable(id string, conn Conn) (*Subscriber, error) {
	return r.subscribe(id, conn, false, true)
}

// SubscribeInternal registers a downstream EDGE session (docs/22): exempt
// from the subscriber caps (they protect viewers) and excluded from the
// viewer counts. Never reliable: the in-cluster leg keeps datagrams.
func (r *Registry) SubscribeInternal(id string, conn Conn) (*Subscriber, error) {
	return r.subscribe(id, conn, true, false)
}

// SubscribeDVR registers a DVR subscriber (docs/26): reliable carrier
// delivery served from the broadcast's ring at this subscriber's own cursor,
// so a stalled link loses nothing until the stall outlives the ring.
// bufferMs is the viewer's *guaranteed minimum* playout offset (its profile
// floor) and bounds how far behind the cursor may fall.
func (r *Registry) SubscribeDVR(id string, conn Conn, bufferMs int) (*Subscriber, error) {
	return r.subscribeOpts(id, conn, subscribeOpts{reliable: true, dvr: true, bufferMs: bufferMs})
}

type subscribeOpts struct {
	internal bool
	reliable bool
	dvr      bool
	bufferMs int
	// Parity symbols served / asked for. Resolved by NegotiateParity at
	// the transport layer, so the hub never re-derives policy.
	parityK         int
	parityRequested int
	// This session is stripe leg stripeMember of width stripeN (docs/35).
	// Validated by NegotiateStripe at the transport layer.
	stripeLeg    bool
	stripeN      uint8
	stripeMember uint8
	// owner is the viewer-minted ?owner= token tying one viewer's sessions
	// together: required on legs, optional on primaries (empty bars
	// striping). Validated at the transport layer.
	owner string
}

// SubscribeParity subscribes a datagram-delivery viewer served parityK
// forward-parity symbols per delta frame (docs/34). parityK is the
// already-negotiated SERVED level (see NegotiateParity). owner is the
// viewer's validated ?owner= token, or empty: only the datagram path takes
// one, because only a datagram viewer can become a striping primary.
func (r *Registry) SubscribeParity(id string, conn Conn, parityK int, owner string) (*Subscriber, error) {
	return r.subscribeOpts(id, conn, subscribeOpts{parityK: parityK, parityRequested: parityK, owner: owner})
}

func (r *Registry) subscribe(id string, conn Conn, internal, reliable bool) (*Subscriber, error) {
	return r.subscribeOpts(id, conn, subscribeOpts{internal: internal, reliable: reliable})
}

func (r *Registry) subscribeOpts(id string, conn Conn, so subscribeOpts) (*Subscriber, error) {
	internal, reliable := so.internal, so.reliable
	r.mu.Lock()

	normID, err := broadcastid.Normalize(id)
	if err != nil {
		r.mu.Unlock()
		return nil, ErrNotFound
	}
	b, exists := r.hubs[normID]
	if !exists {
		r.mu.Unlock()
		return nil, ErrNotFound
	}
	if !internal {
		if b.externalSubsLocked() >= r.opts.MaxSubscribers {
			r.mu.Unlock()
			return nil, ErrFull
		}
		if r.opts.MaxTotalSubscribers > 0 && r.totalExternalSubsLocked() >= r.opts.MaxTotalSubscribers {
			r.mu.Unlock()
			return nil, ErrTotalSubscribers
		}
	}

	s := &Subscriber{
		hub:      b,
		sender:   conn,
		internal: internal,
		reliable: reliable,

		parityK:         so.parityK,
		parityRequested: so.parityRequested,
		stripeLeg:       so.stripeLeg,
		stripeN:         so.stripeN,
		stripeMember:    so.stripeMember,
		owner:           so.owner,
		queue:           make(chan []byte, r.opts.QueueDepth),
		done:            make(chan struct{}),
		statsKey:        newSubscriberStatsKey(),
	}
	// The audio lane: always for reliable delivery, for live-edge viewers
	// only when opted in. Never for an edge session (the second hop makes its
	// own delivery choice) or a stripe leg (audio rides the primary).
	if reliable || (r.opts.LiveEdgeAudioOnReliableStream && !internal && !so.stripeLeg) {
		s.audioQueue = make(chan []byte, AudioSidebandQueueDepth)
		s.audioDone = make(chan struct{})
	}
	if so.dvr {
		// Lazy allocation: a fleet with no DVR viewers pays nothing, and one
		// ring serves every later joiner.
		if b.dvr == nil {
			b.dvr = NewDVRRing(r.opts.DVR)
			if r.opts.DVRAudio {
				b.dvrAudio = NewDVRAudioRing(r.opts.DVR)
			}
			// Seed from the cached keyframe so a joiner has a decodable entry
			// point immediately.
			if b.cachedKeyframe != nil {
				b.dvr.AppendKeyframe(b.cachedKeyframe, time.Now())
			}
		}
		s.dvr = b.dvr
		s.dvrBufferMs = so.bufferMs
		s.dvrCursor = b.dvr.NewCursor()
		s.dvrStop = make(chan struct{})
		s.dvrPace = newDVRPacer(r.opts.DVRMaxCatchup, time.Now)
		if b.dvrAudio != nil {
			s.dvrAudio = b.dvrAudio
			s.dvrAudioCur = b.dvrAudio.Newest()
		}
	}
	b.subs[s] = struct{}{}
	go s.drain()
	if s.audioQueue != nil {
		go s.drainAudioSideband()
	}
	if s.stripeLeg {
		// Arm the liveness lease now: every admitted leg has promised the
		// 1 Hz heartbeat, so a leg that never sends anything IS an orphan.
		s.legLastInbound.Store(time.Now().UnixNano())
		go s.legLeaseWatch(r.opts.StripeLegLease)
	}

	// A stripe leg gets no join primes at all: they belong to the viewer's
	// PRIMARY session, which was primed when it subscribed.
	if !so.stripeLeg {
		// Join primes ride the normal datagram queue, so the joiner need not
		// wait for the next re-send of each.
		if b.cachedClockMapping != nil {
			s.enqueueLocked(b.cachedClockMapping)
		}

		if b.cachedViewerCount != nil {
			s.enqueueLocked(b.cachedViewerCount)
		}

		if b.cachedAudioConfig != nil {
			s.enqueueLocked(b.cachedAudioConfig)
		}
	}

	// Snapshot the cached keyframe under the lock; prime over a stream outside
	// it (stream I/O must never hold the registry lock). A live keyframe that
	// arrives meanwhile carries a higher keyframeSeq and supersedes this prime.
	primeMsg := b.cachedKeyframe
	primeSeq := b.keyframeSeq
	r.mu.Unlock()

	// A DVR subscriber is primed by the ring instead (seeded from this same
	// keyframe), so priming again would duplicate the first GOP.
	if primeMsg != nil && s.dvr == nil && !s.stripeLeg {
		s.sendKeyframe(primeMsg, primeSeq)
	}

	return s, nil
}

// ObfuscateID returns the key under which a broadcast appears in this
// registry's Stats. It is keyed: broadcast IDs are only ~31^6 strong, so any
// unkeyed hash of them can be brute-forced offline from a /statusz scrape.
func (r *Registry) ObfuscateID(id string) string {
	mac := hmac.New(sha256.New, r.statsKey)
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil)[:6])
}

// Stats returns a point-in-time snapshot of registry state.
func (r *Registry) Stats() RegistryStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	broadcasts := make(map[string]Stats)
	var totals TotalStats
	totals.Broadcasts = len(r.hubs)

	for id, b := range r.hubs {
		totals.Subscribers += b.externalSubsLocked()
		totals.FramesRelayed += b.framesRelayed
		totals.DatagramsRelayed += b.datagramsRelayed
		totals.BadDatagrams += b.badDatagrams
		totals.BandwidthDroppedDatagrams += b.bandwidthDroppedDatagrams
		totals.BandwidthDroppedBytes += b.bandwidthDroppedBytes
		totals.KeyframeStreamsIn += b.keyframeStreamsIn
		totals.KeyframeBytesIn += b.keyframeBytesIn
		totals.KeyframeStreamsOversize += b.keyframeStreamsOversize
		totals.IngressDatagramBytes += b.ingressDatagramBytes
		// Loss attribution by leg: an edge hub's ingress window
		// measures origin→edge loss; an origin hub's measures
		// broadcaster→relay loss. Never mixed.
		if b.edge {
			totals.EdgeIngressFramesLost += b.ingressFramesLost
			totals.EdgeIngressChunksLost += b.ingressChunksLost
		} else {
			totals.IngressFramesLost += b.ingressFramesLost
			totals.IngressChunksLost += b.ingressChunksLost
		}

		// Per-subscriber counters are folded into the hub only when the
		// subscriber closes; sum the live ones here for a current view.
		dropped := b.datagramsDropped
		kfSent := b.keyframeStreamsSent
		kfDrops := b.keyframeDrops
		sendErrors := b.sendErrors
		egressDgram := b.egressDatagramBytes
		egressParity := b.egressParityBytes
		egressKf := b.egressKeyframeBytes
		carStreams := b.carrierStreams
		carRecords := b.carrierRecords
		carDropped := b.carrierRecordsDropped
		carOverflow := b.carrierQueueOverflow
		egressCar := b.egressCarrierBytes
		reliableSubs := 0
		dvrSubs := 0
		dvrResyncs := b.dvrResyncs // closed subscribers' fold
		details := make([]SubscriberStats, 0, len(b.subs))
		edgeSessions := 0
		stripeLegs, stripedPrimaries := 0, 0
		stripeTransitions := b.stripeTransitions // closed subscribers' fold
		statsNow := time.Now().UnixNano()
		for s := range b.subs {
			if s.internal {
				edgeSessions++
			}
			if s.reliable && !s.internal {
				reliableSubs++
			}
			if s.dvr != nil {
				dvrSubs++
				dvrResyncs += s.dvrResyncs.Load()
			}
			striped := s.stripeSuppressed(statsNow)
			if s.stripeLeg {
				stripeLegs++
			} else if striped {
				stripedPrimaries++
			}
			stripeTransitions += s.stripeTransitions.Load()
			subDrops := s.keyframeDrops()
			dropped += s.dropped.Load()
			kfSent += s.keyframesSent.Load()
			kfDrops.add(subDrops)
			sendErrors += s.sendErrors.Load()
			egressDgram += s.egressDatagramBytes.Load()
			egressParity += s.egressParityBytes.Load()
			egressKf += s.egressKeyframeBytes.Load()
			carStreams += s.carrierStreams.Load()
			carRecords += s.carrierRecords.Load()
			carDropped += s.carrierRecordsDropped.Load()
			carOverflow += s.carrierQueueOverflow.Load()
			egressCar += s.egressCarrierBytes.Load()
			details = append(details, SubscriberStats{
				Key:                   s.statsKey,
				SessionID:             s.sessionID,
				QueueDepth:            len(s.queue),
				Dropped:               s.dropped.Load(),
				SendErrors:            s.sendErrors.Load(),
				KeyframesSent:         s.keyframesSent.Load(),
				KeyframesDropped:      subDrops.Total(),
				Internal:              s.internal,
				Reliable:              s.reliable,
				CarrierStreams:        s.carrierStreams.Load(),
				CarrierRecords:        s.carrierRecords.Load(),
				CarrierRecordsDropped: s.carrierRecordsDropped.Load(),
				CarrierQueueOverflow:  s.carrierQueueOverflow.Load(),
				DVR:                   s.dvr != nil,
				DVRBufferMs:           s.dvrBufferMs,
				DVRLagMs:              s.dvrLagMs.Load(),
				DVRGopSeq:             s.dvrGopSeq.Load(),
				DVRResyncs:            s.dvrResyncs.Load(),
				ParityK:               s.parityK,
				ParityRequested:       s.parityRequested,
				StripeLeg:             s.stripeLeg,
				StripeN:               stripeStatN(s, striped),
				StripeMember:          int(s.stripeMember),
				Striped:               striped,
			})
		}
		totals.DatagramsDropped += dropped
		totals.KeyframeStreamsSent += kfSent
		totals.KeyframeDrops.add(kfDrops)
		totals.SendErrors += sendErrors
		totals.EgressDatagramBytes += egressDgram
		totals.EgressKeyframeBytes += egressKf
		totals.ReliableSubscribers += reliableSubs
		totals.CarrierStreams += carStreams
		totals.CarrierRecords += carRecords
		totals.CarrierRecordsDropped += carDropped
		totals.CarrierQueueOverflow += carOverflow
		totals.DVRResyncs += dvrResyncs
		totals.EgressCarrierBytes += egressCar
		totals.ParityDatagramsForwarded += b.parityDatagramsForwarded
		totals.EgressParityBytes += egressParity
		totals.ParitySuppressed += b.paritySuppressed
		totals.StripeLegs += stripeLegs
		totals.StripedPrimaries += stripedPrimaries
		totals.StripeSuppressedDatagrams += b.stripeSuppressedDatagrams
		totals.StripeTransitions += stripeTransitions
		totals.StripeLegsReaped += b.stripeLegsReaped

		stalled := b.stalledLocked(time.Now())
		stalledFor := 0
		if stalled {
			stalledFor = int(b.silentForLocked(time.Now()).Seconds())
		}
		var graceRemaining int
		if !b.publisherActive && !b.graceStart.IsZero() {
			rem := r.opts.BroadcastGrace - time.Since(b.graceStart)
			if rem > 0 {
				graceRemaining = int(rem.Seconds())
			}
		}

		role := "origin"
		var viewersGlobal uint32
		if b.edge {
			role = "edge"
		} else {
			viewersGlobal = b.globalViewersLocked()
		}
		ringBytes, ringGops := 0, 0
		if b.dvr != nil {
			ringBytes, ringGops = b.dvr.Bytes(), b.dvr.Gops()
		}
		obf := r.ObfuscateID(id)
		broadcasts[obf] = Stats{
			PublisherActive:           b.publisherActive,
			PublisherSessionID:        b.publisherSessionID,
			Role:                      role,
			Subscribers:               b.externalSubsLocked(),
			ViewersGlobal:             viewersGlobal,
			ReliableSubscribers:       reliableSubs,
			CarrierStreams:            carStreams,
			CarrierRecords:            carRecords,
			CarrierRecordsDropped:     carDropped,
			CarrierQueueOverflow:      carOverflow,
			ParityDatagramsForwarded:  b.parityDatagramsForwarded,
			ParitySuppressed:          b.paritySuppressed,
			StripeLegs:                stripeLegs,
			StripedPrimaries:          stripedPrimaries,
			StripeSuppressedDatagrams: b.stripeSuppressedDatagrams,
			StripeTransitions:         stripeTransitions,
			StripeLegsReaped:          b.stripeLegsReaped,
			DVRSubscribers:            dvrSubs,
			DVRRingBytes:              ringBytes,
			DVRRingGops:               ringGops,
			DVRResyncs:                dvrResyncs,
			EgressCarrierBytes:        egressCar,
			EdgeSessions:              edgeSessions,
			FramesRelayed:             b.framesRelayed,
			DatagramsRelayed:          b.datagramsRelayed,
			DatagramsDropped:          dropped,
			BadDatagrams:              b.badDatagrams,
			BandwidthDroppedDatagrams: b.bandwidthDroppedDatagrams,
			BandwidthDroppedBytes:     b.bandwidthDroppedBytes,
			HasConfig:                 b.cachedKeyframeHasConfig,
			Client:                    b.client,
			Codec:                     b.codec,
			CodedHeight:               b.codedHeight,
			CachedKeyframeID:          b.cachedKeyframeID,
			CachedKeyframeBytes:       len(b.cachedKeyframe),
			KeyframeStreamsIn:         b.keyframeStreamsIn,
			KeyframeBytesIn:           b.keyframeBytesIn,
			KeyframeStreamsSent:       kfSent,
			KeyframeStreamsDropped:    kfDrops.Total(),
			KeyframeStreamsOversize:   b.keyframeStreamsOversize,
			GraceRemainingSeconds:     graceRemaining,
			PublisherStalled:          stalled,
			StalledSeconds:            stalledFor,
			KeyframeDrops:             kfDrops,
			SendErrors:                sendErrors,
			IngressDatagramBytes:      b.ingressDatagramBytes,
			EgressDatagramBytes:       egressDgram,
			EgressParityBytes:         egressParity,
			EgressKeyframeBytes:       egressKf,
			IngressFramesLost:         b.ingressFramesLost,
			IngressChunksLost:         b.ingressChunksLost,
			SubscriberDetails:         details,
		}
	}

	// Add the counters folded from expired broadcasts / closed subscribers.
	totals.FramesRelayed += r.totalFramesRelayed
	totals.DatagramsRelayed += r.totalDatagramsRelayed
	totals.DatagramsDropped += r.totalDatagramsDropped
	totals.BadDatagrams += r.totalBadDatagrams
	totals.BandwidthDroppedDatagrams += r.totalBandwidthDroppedDatagrams
	totals.BandwidthDroppedBytes += r.totalBandwidthDroppedBytes
	totals.KeyframeStreamsIn += r.totalKeyframeStreamsIn
	totals.KeyframeBytesIn += r.totalKeyframeBytesIn
	totals.KeyframeStreamsSent += r.totalKeyframeStreamsSent
	totals.KeyframeDrops.add(r.totalKeyframeDrops)
	totals.KeyframeStreamsOversize += r.totalKeyframeStreamsOversize
	totals.SendErrors += r.totalSendErrors
	totals.IngressDatagramBytes += r.totalIngressDatagramBytes
	totals.EgressDatagramBytes += r.totalEgressDatagramBytes
	totals.EgressKeyframeBytes += r.totalEgressKeyframeBytes
	totals.IngressFramesLost += r.totalIngressFramesLost
	totals.IngressChunksLost += r.totalIngressChunksLost
	totals.EdgeIngressFramesLost += r.totalEdgeIngressFramesLost
	totals.EdgeIngressChunksLost += r.totalEdgeIngressChunksLost
	totals.CarrierStreams += r.totalCarrierStreams
	totals.CarrierRecords += r.totalCarrierRecords
	totals.CarrierRecordsDropped += r.totalCarrierRecordsDropped
	totals.CarrierQueueOverflow += r.totalCarrierQueueOverflow
	totals.DVRResyncs += r.totalDVRResyncs
	totals.EgressCarrierBytes += r.totalEgressCarrierBytes
	totals.ParityDatagramsForwarded += r.totalParityDatagramsForwarded
	totals.ParitySuppressed += r.totalParitySuppressed
	totals.EgressParityBytes += r.totalEgressParityBytes
	totals.StripeSuppressedDatagrams += r.totalStripeSuppressedDatagrams
	totals.StripeTransitions += r.totalStripeTransitions
	totals.StripeLegsReaped += r.totalStripeLegsReaped
	totals.KeyframeStreamsDropped = totals.KeyframeDrops.Total()

	return RegistryStats{
		Totals:     totals,
		Broadcasts: broadcasts,
	}
}

// handleGraceExpiry deletes the hub, shuts down viewers and records metrics.
func (r *Registry) handleGraceExpiry(id string, gen uint64) {
	r.expireBroadcast(id, func(b *broadcastHub) bool { return b.generation == gen })
}

// EndBroadcast force-expires a broadcast whose cluster Lease disappeared:
// local viewers get the terminal 4000 as if the local grace expired. A no-op
// when this pod has an ACTIVE publisher for the ID (a racing janitor must not
// kill a live broadcast) or doesn't know the ID.
func (r *Registry) EndBroadcast(id string) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return
	}
	r.expireBroadcast(normID, func(*broadcastHub) bool { return true })
}

// ExpireEdgeIfViewerless deletes an EDGE hub that has no local viewers — the
// linger-out path. Left in grace, the hub would keep satisfying
// CheckSubscribe, so a joiner would attach with no upstream pull behind it
// (EnsureEdge runs only on ErrNotFound) and later get a wrong terminal 4000.
// Atomic with Subscribe: returns true when the hub is gone, false when a
// viewer or publisher raced the linger window. Origin hubs are never expired
// here.
func (r *Registry) ExpireEdgeIfViewerless(id string) bool {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return true
	}
	if r.expireBroadcast(normID, func(b *broadcastHub) bool {
		return b.edge && b.externalSubsLocked() == 0
	}) {
		return true
	}
	r.mu.Lock()
	_, exists := r.hubs[normID]
	r.mu.Unlock()
	return !exists
}

// TerminateBroadcast force-expires a broadcast the operator has banned
// (docs/42 §4.3) — the ONLY entry point that may kill a LIVE broadcast, and
// the only one that closes subscribers with something other than 4000.
//
// It differs from every other expiry path only in ignoring the
// publisherActive guard (which stops a racing janitor killing a live
// broadcast); the rest is the shared expiry body, deliberately not
// duplicated. It closes, with code, the publisher and every subscriber. The
// transport closes its own publisher session too, on purpose: an EDGE hub's
// publisher is an upstream pull the transport's session map doesn't know.
//
// Idempotent: on a pod without the broadcast it returns false, so it is safe
// to run on every pod from one informer event.
func (r *Registry) TerminateBroadcast(id string, code uint32, reason string) bool {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return false
	}
	return r.removeBroadcast(normID, func(*broadcastHub) bool { return true },
		true, code, reason)
}

// expireBroadcast removes the hub when it exists, has no active publisher and
// passes ok, closing subscribers with the terminal 4000. Returns whether the
// hub was removed.
func (r *Registry) expireBroadcast(id string, ok func(*broadcastHub) bool) bool {
	return r.removeBroadcast(id, ok, false, uint32(wire.CloseCodeBroadcastEnded), "broadcast ended")
}

// removeBroadcast is the shared body of every hub removal. force skips the
// publisherActive guard (admin kill only — see TerminateBroadcast); code and
// reason are what every subscriber's session is closed with.
func (r *Registry) removeBroadcast(id string, ok func(*broadcastHub) bool, force bool, code uint32, reason string) bool {
	r.mu.Lock()
	b, exists := r.hubs[id]
	if !exists || (!force && b.publisherActive) || !ok(b) {
		r.mu.Unlock()
		return false
	}

	delete(r.hubs, id)

	// The broadcast's active life ends at its publisher's last disconnect, or
	// now if a forced removal deposes a live one.
	var end *BroadcastEnd
	if !b.edge && (b.publisherActive || !b.publisherLeftAt.IsZero()) {
		endAt := b.publisherLeftAt
		if b.publisherActive {
			endAt = time.Now()
		}
		end = &BroadcastEnd{
			Duration:    max(endAt.Sub(b.startedAt), 0),
			PeakViewers: max(b.peakViewers, b.globalViewersLocked()),
		}
	}

	// Depose a live publisher (force path only), as TakeOverPublish does, so
	// its deferred Close can't arm a grace timer on a deleted hub or stamp a
	// Lease this expiry is about to delete.
	var deposed SessionCloser
	if old := b.publisher; b.publisherActive && old != nil {
		old.closed = true
		deposed = old.conn
	}
	b.publisherActive = false
	b.publisherSessionID = ""
	b.publisher = nil

	r.totalFramesRelayed += b.framesRelayed
	r.totalDatagramsRelayed += b.datagramsRelayed
	r.totalDatagramsDropped += b.datagramsDropped
	r.totalBadDatagrams += b.badDatagrams
	r.totalBandwidthDroppedDatagrams += b.bandwidthDroppedDatagrams
	r.totalBandwidthDroppedBytes += b.bandwidthDroppedBytes
	r.totalKeyframeStreamsIn += b.keyframeStreamsIn
	r.totalKeyframeBytesIn += b.keyframeBytesIn
	r.totalKeyframeStreamsOversize += b.keyframeStreamsOversize
	r.totalKeyframeStreamsSent += b.keyframeStreamsSent
	r.totalKeyframeDrops.add(b.keyframeDrops)
	r.totalSendErrors += b.sendErrors
	r.totalIngressDatagramBytes += b.ingressDatagramBytes
	r.totalEgressDatagramBytes += b.egressDatagramBytes
	r.totalEgressKeyframeBytes += b.egressKeyframeBytes
	r.totalCarrierStreams += b.carrierStreams
	r.totalCarrierRecords += b.carrierRecords
	r.totalCarrierRecordsDropped += b.carrierRecordsDropped
	r.totalCarrierQueueOverflow += b.carrierQueueOverflow
	r.totalDVRResyncs += b.dvrResyncs
	r.totalEgressCarrierBytes += b.egressCarrierBytes
	r.totalParityDatagramsForwarded += b.parityDatagramsForwarded
	r.totalParitySuppressed += b.paritySuppressed
	r.totalEgressParityBytes += b.egressParityBytes
	r.totalStripeSuppressedDatagrams += b.stripeSuppressedDatagrams
	r.totalStripeTransitions += b.stripeTransitions
	r.totalStripeLegsReaped += b.stripeLegsReaped
	if b.edge {
		r.totalEdgeIngressFramesLost += b.ingressFramesLost
		r.totalEdgeIngressChunksLost += b.ingressChunksLost
	} else {
		r.totalIngressFramesLost += b.ingressFramesLost
		r.totalIngressChunksLost += b.ingressChunksLost
	}

	// A pending grace timer must not fire against the next hub registered
	// under this ID (EndBroadcast can expire ahead of the timer).
	if b.graceTimer != nil {
		b.graceTimer.Stop()
		b.graceTimer = nil
	}
	b.graceStart = time.Time{}

	// Purge everything that could replay this broadcast's media, under the
	// lock and before anyone is closed: a live publisher's ingest goroutine
	// still holds b, and DVR cursors still hold the ring, which would replay
	// banned content to a not-yet-closed subscriber.
	b.cachedKeyframe = nil
	b.cachedKeyframeID = 0
	b.cachedKeyframeHasConfig = false
	b.cachedClockMapping = nil
	b.cachedAudioConfig = nil
	b.cachedViewerCount = nil
	b.dvr = nil
	b.dvrAudio = nil

	var subs []*Subscriber
	for s := range b.subs {
		// Still-live subscribers' counts are NOT folded here: their drains may
		// still be dropping. Subscriber.Close folds them into the registry
		// totals once drain has finished.
		subs = append(subs, s)
	}
	edge := b.edge
	r.mu.Unlock()

	msg := "broadcast expired and garbage collected"
	if force {
		msg = "broadcast terminated"
	}
	r.log.Info(msg, "broadcast_id", id,
		"subscribers", len(subs), "publisher_closed", deposed != nil, "close_code", code)

	// The publisher first, so no new media arrives mid-teardown. Outside the
	// lock, like every session close in this package.
	if deposed != nil {
		_ = deposed.CloseWithError(code, reason)
	}

	// Then EVERY subscriber kind: viewers, internal edge sessions (so
	// downstream pods tear down too) and stripe legs. One loop, one code
	// — a second copy for a second code is how the two drift.
	for _, s := range subs {
		_ = s.sender.CloseWithError(code, reason)
		s.Close()
	}

	// ORIGIN hubs only: an edge's expiry deleting the origin's Lease would
	// kill the broadcast fleet-wide.
	if !edge && r.opts.OnBroadcastExpired != nil {
		r.opts.OnBroadcastExpired(id)
	}
	if end != nil && r.opts.OnOriginEnded != nil {
		r.opts.OnOriginEnded(*end)
	}
	// Origin only, like the lease delete above. The reason comes from the
	// CLOSE CODE, not from `force`: the stall sweep also removes forcefully,
	// and calling that a kill would report an automatic timeout as
	// enforcement — the one reason a consumer is expected to escalate. 4006
	// is the operator's, and only the operator's.
	if !edge {
		busReason := events.BroadcastEndedGC
		if code == uint32(wire.CloseCodeTerminatedByOperator) {
			busReason = events.BroadcastEndedKilled
		}
		r.busEnded(id, busReason)
	}
	return true
}

// Publisher is the active publisher session's handle.
type Publisher struct {
	hub    *broadcastHub
	closed bool
	// conn is the session close handle bound after the upgrade (BindConn);
	// nil until then. Guarded by the registry lock, like closed.
	conn SessionCloser
	// send pushes a relay-originated datagram (the viewer count) to this
	// publisher. Nil until BindSend, cleared on Close. Guarded by the
	// registry lock, like conn.
	send func([]byte)
	// lastSeen is the stall clock: when this session last delivered ANY
	// datagram or keyframe stream (unix nanos; the claim time until then).
	// Atomic because the transport stamps it for every datagram outside r.mu.
	// Per Publisher, so a deposed session never freshens its successor.
	lastSeen atomic.Int64
}

// NoteSeen stamps the stall clock. The transport calls it for every datagram,
// including TimeSync, which never reaches HandleDatagram.
func (p *Publisher) NoteSeen() { p.lastSeen.Store(time.Now().UnixNano()) }

func (p *Publisher) lastSeenTime() time.Time { return time.Unix(0, p.lastSeen.Load()) }

// BindConn attaches the publisher's session close handle so a later
// TakeOverPublish can depose this session. It reports false when the
// publisher was already deposed before this bind; the caller must then end
// its session.
func (p *Publisher) BindConn(c SessionCloser) bool {
	r := p.hub.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return false
	}
	p.conn = c
	return true
}

// SetTelemetrySession records the telemetry session handle this publisher
// session was told in its TelemetryHello (docs/33). A no-op with an empty id
// or on a closed or deposed publisher, which must never relabel the live one.
func (p *Publisher) SetTelemetrySession(sessionID string) {
	if sessionID == "" {
		return
	}
	r := p.hub.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed || p.hub.publisher != p {
		return
	}
	p.hub.publisherSessionID = sessionID
}

// BindSend attaches the relay→publisher datagram sender for the count pump.
// It must be goroutine-safe (quic-go's SendDatagram is): the pump calls it
// concurrently with the read loop's TimeSync replies. A no-op on a closed or
// deposed publisher.
func (p *Publisher) BindSend(send func([]byte)) {
	r := p.hub.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return
	}
	p.send = send
}

// HandleDatagram processes and relays one publisher delta datagram. Keyframes
// arrive via IngestKeyframeStream, so a keyframe-flagged VideoChunk here is
// forwarded verbatim but not cached.
func (p *Publisher) HandleDatagram(dgram []byte) {
	p.NoteSeen()
	b := p.hub
	if len(dgram) > wire.MaxDatagramSize {
		b.countBad()
		return
	}
	ver, typ, err := wire.PeekType(dgram)
	if err != nil || ver != wire.Version {
		b.countBad()
		return
	}
	switch typ {
	case wire.TypeVideoChunk:
		hdr, _, err := wire.ParseVideoChunk(dgram)
		if err != nil {
			b.countBad()
			return
		}
		p.relayVideoChunk(hdr, dgram)
	case wire.TypeDecoderConfig:
		if _, err := wire.ParseDecoderConfig(dgram); err != nil {
			b.countBad()
			return
		}
		p.noteConfig(dgram)
		p.relayDatagram(dgram)
	case wire.TypeParityChunk:
		// Deliberately NOT relayVideoChunk: a parity datagram would corrupt
		// its frame count and ingress-loss window.
		if _, _, err := wire.ParseParityChunk(dgram); err != nil {
			b.countBad()
			return
		}
		p.relayDatagram(dgram)
	case wire.TypeClockMapping:
		if _, err := wire.ParseClockMapping(dgram); err != nil {
			b.countBad()
			return
		}
		p.relayClockMapping(dgram)
	case wire.TypeViewerCount:
		if _, err := wire.ParseViewerCount(dgram); err != nil {
			b.countBad()
			return
		}
		p.relayViewerCount(dgram)
	case wire.TypeAudioFrame:
		if _, _, err := wire.ParseAudioFrame(dgram); err != nil {
			b.countBad()
			return
		}
		// No ingress-loss observation: the window assumes a single frameID
		// space, and audio loss is concealed (and counted) viewer-side.
		p.relayDatagram(dgram)
	case wire.TypeAudioConfig:
		if _, err := wire.ParseAudioConfig(dgram); err != nil {
			b.countBad()
			return
		}
		p.relayAudioConfig(dgram)
	default:
		b.countBad()
	}
}

// relayViewerCount forwards an upstream origin's global viewer count to this
// EDGE hub's local viewers (docs/23), verbatim. On an origin hub the
// "publisher" is a real broadcaster, which has no business announcing an
// audience number: dropped silently (spoof guard).
func (p *Publisher) relayViewerCount(dgram []byte) {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed || !b.edge {
		return
	}
	b.ingressDatagramBytes += uint64(len(dgram))
	b.cacheAndFanViewerCountLocked(dgram)
}

// cacheAndFanViewerCountLocked caches a copy of one ViewerCount datagram and
// fans it out to local viewers AND edge sessions (which forward it down).
// Caller holds r.mu.
func (b *broadcastHub) cacheAndFanViewerCountLocked(dgram []byte) {
	msg := make([]byte, len(dgram))
	copy(msg, dgram)
	b.cachedViewerCount = msg
	b.fanOutLocked(msg)
}

// relayClockMapping forwards a ClockMapping datagram to all subscribers and
// caches a copy for late-joiner priming.
func (p *Publisher) relayClockMapping(dgram []byte) {
	b := p.hub
	r := b.registry
	msg := make([]byte, len(dgram))
	copy(msg, dgram)
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return
	}
	b.cachedClockMapping = msg
	b.ingressDatagramBytes += uint64(len(msg))
	b.fanOutLocked(msg)
}

// relayAudioConfig forwards an AudioConfig datagram to all subscribers and
// caches a copy for late-joiner priming. Unlike ViewerCount it is accepted on
// origin and edge hubs alike: the publisher is exactly who should speak.
func (p *Publisher) relayAudioConfig(dgram []byte) {
	b := p.hub
	r := b.registry
	msg := make([]byte, len(dgram))
	copy(msg, dgram)
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return
	}
	b.cachedAudioConfig = msg
	b.ingressDatagramBytes += uint64(len(msg))
	b.fanOutLocked(msg)
}

// IngestKeyframeStream reads one complete keyframe StreamFrame message from a
// publisher-initiated unidirectional stream into a single bounded buffer, then
// caches it and fans it out to subscribers. It reads at most MaxKeyframeBytes;
// an oversize or malformed stream is rejected (the caller resets it) and the
// existing cache is left intact. Runs on its own goroutine per stream.
func (p *Publisher) IngestKeyframeStream(stream io.Reader) error {
	p.NoteSeen()
	b := p.hub

	header := make([]byte, wire.StreamFrameHeaderSize)
	if _, err := io.ReadFull(stream, header); err != nil {
		b.countBad()
		return err
	}
	hdr, err := wire.ParseStreamFrameHeader(header)
	if err != nil {
		b.countBad()
		return err
	}
	total := wire.StreamFrameHeaderSize + int(hdr.ConfigLen) + int(hdr.PayloadLen)
	if total > b.registry.opts.MaxKeyframeBytes {
		b.countKeyframeOversize()
		return fmt.Errorf("hub: keyframe %d bytes exceeds MaxKeyframeBytes %d", total, b.registry.opts.MaxKeyframeBytes)
	}

	msg := make([]byte, total)
	copy(msg, header)
	if _, err := io.ReadFull(stream, msg[wire.StreamFrameHeaderSize:]); err != nil {
		b.countBad()
		return err
	}
	// The message is exactly total bytes; a well-behaved publisher FINs here.
	// Trailing bytes mean a framing disagreement — reject rather than cache.
	var extra [1]byte
	if n, _ := stream.Read(extra[:]); n > 0 {
		b.countBad()
		return errors.New("hub: keyframe stream has trailing bytes past declared length")
	}

	p.onKeyframe(msg, hdr)
	return nil
}

// onKeyframe caches the keyframe and fans it out to every current subscriber.
// The cache swap and subscriber snapshot happen under the lock; the stream
// writes happen outside it, on per-subscriber goroutines.
func (p *Publisher) onKeyframe(msg []byte, hdr wire.StreamFrameHeader) {
	b := p.hub
	r := b.registry
	cfg, payload := splitKeyframe(msg, hdr)
	var cfgProbe configProbe
	if cfg != nil {
		cfgProbe = probeConfig(cfg)
	}
	kfHeight := mediaprobe.KeyframeHeight(payload)
	r.mu.Lock()
	if p.closed {
		r.mu.Unlock()
		return
	}
	if cfg != nil {
		b.applyConfigProbeLocked(cfgProbe)
	}
	// An AVCC H.264 keyframe carries no SPS; its height came from the
	// config's extradata, and a length prefix must not overwrite it.
	if kfHeight > 0 && !b.avccConfig {
		b.codedHeight = kfHeight
	}
	b.cachedKeyframe = msg
	if b.dvr != nil {
		b.dvr.AppendKeyframe(msg, time.Now())
	}
	b.cachedKeyframeID = hdr.FrameID
	b.cachedKeyframeHasConfig = hdr.ConfigLen > 0
	b.keyframeSeq++
	seq := b.keyframeSeq
	b.keyframeStreamsIn++
	b.keyframeBytesIn += uint64(len(msg))
	b.framesRelayed++
	fl, cl := b.ingress.observeFrame(hdr.FrameID)
	b.ingressFramesLost += fl
	b.ingressChunksLost += cl
	subs := make([]*Subscriber, 0, len(b.subs))
	for s := range b.subs {
		// A DVR subscriber's keyframes come from the ring at ITS cursor,
		// seconds behind live. Sending the live one too hands the viewer a
		// second, contradictory timeline and freezes its video while data
		// keeps arriving (docs/26).
		if s.dvr != nil {
			continue
		}
		// A stripe leg carries delta datagrams only — its viewer's
		// keyframes ride the primary session's streams.
		if s.stripeLeg {
			continue
		}
		subs = append(subs, s)
	}
	r.mu.Unlock()

	for _, s := range subs {
		s.sendKeyframe(msg, seq)
	}
}

// configProbe is what a DecoderConfig says about the media (docs/61).
type configProbe struct {
	codec  string
	avcc   bool
	height int
}

// probeConfig reads the codec family, and for AVCC H.264 the coded height,
// from a DecoderConfig datagram. Header-only; called outside the lock.
func probeConfig(dgram []byte) configProbe {
	dc, err := wire.ParseDecoderConfig(dgram)
	if err != nil {
		return configProbe{}
	}
	family := mediaprobe.CodecFamily(dc.Codec)
	return configProbe{
		codec:  family,
		avcc:   family == mediaprobe.CodecH264 && len(dc.Extradata) > 0,
		height: mediaprobe.ExtradataHeight(family, dc.Extradata),
	}
}

// applyConfigProbeLocked records a config's probe on the hub. Caller holds
// r.mu.
func (b *broadcastHub) applyConfigProbeLocked(c configProbe) {
	if c.codec == "" {
		return
	}
	b.codec = c.codec
	b.avccConfig = c.avcc
	if c.height > 0 {
		b.codedHeight = c.height
	}
}

// noteConfig probes a DecoderConfig datagram for the usage labels.
func (p *Publisher) noteConfig(dgram []byte) {
	c := probeConfig(dgram)
	r := p.hub.registry
	r.mu.Lock()
	if !p.closed {
		p.hub.applyConfigProbeLocked(c)
	}
	r.mu.Unlock()
}

// splitKeyframe returns a StreamFrame message's embedded DecoderConfig
// datagram (nil when ConfigLen is 0) and its encoded payload. The lengths
// were validated when the message was read.
func splitKeyframe(msg []byte, hdr wire.StreamFrameHeader) (cfg, payload []byte) {
	off := wire.StreamFrameHeaderSize
	end := off + int(hdr.ConfigLen)
	if end > len(msg) {
		return nil, nil
	}
	if hdr.ConfigLen > 0 {
		cfg = msg[off:end]
	}
	return cfg, msg[end:]
}

// SetClient records the publisher session's self-description (docs/61) for
// the per-broadcast info gauge. Ignored on a closed or deposed publisher.
func (p *Publisher) SetClient(info clientinfo.Info) {
	r := p.hub.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return
	}
	p.hub.client = info
}

// Close releases the publisher slot and schedules the GC grace timer. A no-op
// on a deposed publisher: arming the grace timer there would
// garbage-collect a live broadcast.
func (p *Publisher) Close() {
	b := p.hub
	r := b.registry
	r.mu.Lock()

	if p.closed {
		r.mu.Unlock()
		return
	}
	p.closed = true
	p.send = nil
	b.publisherActive = false
	b.publisherSessionID = ""
	b.publisher = nil
	b.publisherLeftAt = time.Now()

	if r.opts.BroadcastGrace > 0 {
		gen := b.generation
		id := b.id
		b.graceStart = time.Now()
		b.graceTimer = time.AfterFunc(r.opts.BroadcastGrace, func() {
			r.handleGraceExpiry(id, gen)
		})
	}
	id := b.id
	edge := b.edge
	r.mu.Unlock()

	// Outside the lock (k8s API I/O). ORIGIN hubs only: an edge's upstream
	// pull ending has no business touching the origin's lease.
	if !edge && r.opts.OnPublisherClosed != nil {
		r.opts.OnPublisherClosed(id)
	}
}

// cachedAudioConfigSnapshot returns the latest AudioConfig datagram for the
// audio DVR's resync re-emit. Under the registry lock: the drain reads it from
// its own goroutine.
func (b *broadcastHub) cachedAudioConfigSnapshot() []byte {
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	return b.cachedAudioConfig
}

func (b *broadcastHub) countBad() {
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	b.badDatagrams++
}

func (b *broadcastHub) countKeyframeOversize() {
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	b.keyframeStreamsOversize++
}

// relayDatagram forwards a datagram verbatim to all subscribers (no caching).
func (p *Publisher) relayDatagram(dgram []byte) {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return
	}
	b.ingressDatagramBytes += uint64(len(dgram))
	b.fanOutLocked(dgram)
}

func (p *Publisher) relayVideoChunk(hdr wire.VideoChunkHeader, dgram []byte) {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return
	}
	if hdr.ChunkIndex == 0 {
		b.framesRelayed++
	}
	b.ingressDatagramBytes += uint64(len(dgram))
	fl, cl := b.ingress.observeChunk(hdr.FrameID, int(hdr.ChunkIndex), int(hdr.ChunkCount))
	b.ingressFramesLost += fl
	b.ingressChunksLost += cl
	b.fanOutLocked(dgram)
}

func (b *broadcastHub) fanOutLocked(dgram []byte) {
	b.datagramsRelayed++
	// The DVR ring is fed ONCE per broadcast, not per subscriber (docs/26).
	// Only media is retained: control traffic is live-edge by nature and goes
	// straight out through the sideband, so a DVR subscriber's keepalive is
	// never delayed by the depth of its video cursor.
	if b.dvr != nil && isVideoChunkDatagram(dgram) {
		b.dvr.AppendRecord(dgram, time.Now())
	}
	if b.dvrAudio != nil && isAudioDatagram(dgram) {
		b.dvrAudio.Append(dgram, time.Now())
	}
	// Stripe routing (docs/35) is computed lazily, only when a
	// stripe-involved subscriber is present, so a fleet with no striping pays
	// nothing beyond two field reads per subscriber.
	var (
		stripeOrd        uint32
		stripeIsDelta    bool
		stripeClassified bool
		stripeNow        int64
	)
	classify := func() {
		if stripeClassified {
			return
		}
		stripeOrd, stripeIsDelta = deltaOrdinal(dgram)
		stripeNow = time.Now().UnixNano()
		stripeClassified = true
	}
	// Parity is served as a per-subscriber PREFIX of the producer's symbols;
	// the relay computes nothing, one comparison per subscriber (docs/34).
	if idx := parityIndexIfParity(dgram); idx >= 0 {
		for s := range b.subs {
			// An EDGE session is exempt: the prefix belongs at the pod that
			// SERVES the viewer. The origin cannot know an edge's subscribers'
			// k, and clamping here would silently strip parity from every
			// viewer behind the cascade.
			if s.internal || idx < s.parityK {
				// A leg forwards only its share of the prefix, a striped
				// primary none; only the primary's withheld symbols count
				// as suppressed (a leg's are routing).
				if s.stripeLeg || s.stripeUntil.Load() != 0 {
					classify()
					if stripeIsDelta && !s.wantsDeltaLocked(stripeOrd, stripeNow) {
						if !s.stripeLeg {
							b.stripeSuppressedDatagrams++
						}
						continue
					}
				}
				s.enqueueLocked(dgram)
				b.parityDatagramsForwarded++
			} else {
				b.paritySuppressed++
			}
		}
		return
	}
	for s := range b.subs {
		// A leg receives only its share of delta datagrams and nothing else;
		// a striped primary skips deltas but keeps everything else.
		if s.stripeLeg || s.stripeUntil.Load() != 0 {
			classify()
			if s.stripeLeg && !stripeIsDelta {
				continue
			}
			if stripeIsDelta && !s.wantsDeltaLocked(stripeOrd, stripeNow) {
				if !s.stripeLeg {
					b.stripeSuppressedDatagrams++
				}
				continue
			}
		}
		s.enqueueLocked(dgram)
	}
}

// parityIndexIfParity returns the symbol index when dgram is a parity chunk,
// else -1, so fanOutLocked's hot path pays a single type peek.
func parityIndexIfParity(dgram []byte) int {
	if !isParityDatagram(dgram) {
		return -1
	}
	return parityIndexOf(dgram)
}

// Subscriber is one viewer's handle.
type Subscriber struct {
	hub    *broadcastHub
	sender Conn
	queue  chan []byte
	done   chan struct{}
	// The audio lane, so a parked video write can't freeze audio. Nil unless
	// the subscriber gets an audio carrier (see subscribeOpts).
	audioQueue chan []byte
	audioDone  chan struct{}
	// audioCar is that lane's carrier (see "the audio carrier" below). Owned
	// by drainAudioSideband alone, hence no lock.
	audioCar KeyframeStream

	// statsKey names this subscriber in /statusz subscriberDetails: random
	// per-session, never anything identifying or joinable.
	statsKey string
	// sessionID is the telemetry session handle (docs/33): the nonce of the
	// token this session was handed in its TelemetryHello. Empty when
	// telemetry is off or this is an edge session. Deliberately NOT derived
	// from statsKey (or vice versa): that would leak part of a bearer
	// credential into a public-ish JSON endpoint. Guarded by the registry lock.
	sessionID string
	// internal marks a downstream edge session: exempt from viewer
	// caps and counted as an edge, not an audience member.
	internal bool
	// reliable marks a resilient subscriber: drain writes records on per-GOP
	// carrier streams and never calls SendDatagram. Fixed at subscribe time.
	reliable bool
	// parityK is how many parity symbols this subscriber is SERVED: 0 for
	// reliable/DVR subscribers (QUIC retransmits) and when the fleet is off.
	// parityRequested is what the viewer ASKED for, for /statusz. Both stay 0
	// on an INTERNAL subscriber, which fanOutLocked exempts from the prefix.
	parityK         int
	parityRequested int

	// Striped delivery (docs/35 §5.1). stripeLeg marks one leg of a viewer's
	// stripe: it receives only the delta datagrams whose ordinal matches
	// stripeMember mod stripeN. Fixed at subscribe time. A leg counts against
	// the subscriber caps (the bound against a counterfeit leg flood) but
	// never in the viewer count.
	stripeLeg    bool
	stripeN      uint8
	stripeMember uint8
	// stripeUntil is the primary-suppression deadline (UnixNano; 0 = not
	// suppressed). Written by ApplyStripeState off the registry lock, read by
	// the fan-out under it — hence atomic. The deadline IS the TTL fail-open:
	// without a refresh, deltas resume by comparison alone. stripeNInfo is for
	// /statusz only.
	stripeUntil       atomic.Int64
	stripeNInfo       atomic.Uint32
	stripeTransitions atomic.Uint64
	// owner is the viewer-minted session-group token, empty on unowned
	// sessions. Read-only after subscribe — no lock.
	owner string
	// legLastInbound is a stripe leg's liveness-lease stamp (UnixNano),
	// renewed by NoteLegAlive and read by legLeaseWatch — hence atomic.
	legLastInbound atomic.Int64

	// downstreamViewers is the local viewer count this INTERNAL (edge)
	// subscriber last reported up. Written lock-free by the read loop, summed
	// by the count pump under the registry lock — hence atomic.
	downstreamViewers atomic.Uint64

	// closed is set (atomically) by Close before it cancels the in-flight
	// keyframe stream; sendKeyframe reads it without the registry lock to avoid
	// opening a doomed stream.
	closed atomic.Bool

	dropped             atomic.Uint64
	sendErrors          atomic.Uint64
	egressDatagramBytes atomic.Uint64
	// Parity's share of egressDatagramBytes above — a slice of that total,
	// never a sibling of it.
	egressParityBytes   atomic.Uint64
	egressKeyframeBytes atomic.Uint64

	// Keyframe stream fan-out. kfMu guards kfCurrent + kfLastSeq; at most
	// one keyframe stream is ever in flight to a subscriber, and a stale send
	// (lower seq) is skipped so a late prime can't supersede a live keyframe.
	kfMu          sync.Mutex
	kfCurrent     KeyframeStream
	kfLastSeq     uint64
	kfWriters     sync.WaitGroup
	keyframesSent atomic.Uint64
	// Consecutive OpenKeyframeStream failures (guarded by kfMu); reset on any
	// successful open. Crossing KeyframeOpenFailEvictThreshold evicts the
	// subscriber; evicting latches so it fires exactly once.
	kfConsecOpenFailed int
	// Consecutive keyframes whose stream opened but whose write stalled out
	// (guarded by kfMu); reset on any delivered keyframe, untouched by a
	// superseded one. Crossing KeyframeSlowEvictThreshold evicts the
	// subscriber.
	kfConsecSlow int
	// Consecutive OpenCarrierStream failures (guarded by kfMu); reset on any
	// successful carrier open, deliberately NOT by a keyframe open (see
	// CarrierOpenFailEvictThreshold). Grows at GOP cadence, not record cadence.
	carConsecOpenFailed int
	evicting            bool
	// Keyframe drops by cause; keyframeDrops() snapshots them.
	kfDroppedSuperseded atomic.Uint64
	kfDroppedSlow       atomic.Uint64
	kfDroppedBandwidth  atomic.Uint64
	kfDroppedOpenFailed atomic.Uint64

	// Reliable carrier fan-out. carRotations is bumped per fanned-out
	// keyframe; the drain rotates its carrier when it sees a new value. carMu
	// guards carCurrent, otherwise owned by the drain; Close cancels it under
	// carMu to unblock a stalled write (CancelWrite is safe alongside Write).
	carRotations atomic.Uint64
	carMu        sync.Mutex
	carCurrent   KeyframeStream
	// The rotation whose GOP a queue overflow declared dead, stored as
	// generation+1 so zero means "none". Used under registry.mu; a rotation
	// moves past it, so it needs no reset.
	carDeadRotation atomic.Uint64

	// DVR state (docs/26). dvr non-nil selects the cursor drain over the
	// queue drain; dvrCursor is owned by that goroutine alone after subscribe.
	dvr           *DVRRing
	dvrCursor     DVRCursor
	dvrBufferMs   int
	dvrStop       chan struct{}
	dvrResyncs    atomic.Uint64
	dvrLagMs      atomic.Int64
	dvrProgressAt atomic.Int64
	dvrCursorAtMs atomic.Int64
	dvrPace       *dvrPacer
	dvrAudio      *DVRAudioRing
	dvrAudioCur   DVRAudioCursor
	dvrAudioCar   KeyframeStream
	dvrGopSeq     atomic.Int64

	carrierStreams        atomic.Uint64
	carrierRecords        atomic.Uint64
	carrierRecordsDropped atomic.Uint64
	carrierQueueOverflow  atomic.Uint64
	egressCarrierBytes    atomic.Uint64
}

// keyframeDrops snapshots the per-cause drop atomics.
func (s *Subscriber) keyframeDrops() KeyframeDrops {
	return KeyframeDrops{
		Superseded: s.kfDroppedSuperseded.Load(),
		Slow:       s.kfDroppedSlow.Load(),
		Bandwidth:  s.kfDroppedBandwidth.Load(),
		OpenFailed: s.kfDroppedOpenFailed.Load(),
	}
}

func (s *Subscriber) enqueueLocked(dgram []byte) {
	// Audio takes its own lane on a reliable subscriber: sharing the video
	// queue would freeze every audio packet behind a parked carrier write.
	// A DVR subscriber reads its video from the ring at its own cursor, so the
	// queue would only duplicate it. Everything else still rides the queue.
	if s.dvr != nil && isVideoChunkDatagram(dgram) {
		return
	}
	if s.dvrAudio != nil && isAudioDatagram(dgram) {
		return
	}
	if s.audioQueue != nil && isAudioDatagram(dgram) {
		select {
		case s.audioQueue <- dgram:
		default:
			// Drop-newest, exactly like the datagram drain's queue: this lane
			// ends in SendDatagram, which never parks, so a full queue means a
			// wedged session rather than a slow write.
			s.dropped.Add(1)
		}
		return
	}
	// A holed GOP is beyond saving, so shed its tail at the door until the
	// next rotation (bumped by the fan-out, so this clears even while the
	// drain is parked). Video-only: control traffic must still get through.
	if s.reliable && isVideoChunkDatagram(dgram) {
		if s.carDeadRotation.Load() == s.carRotations.Load()+1 {
			s.carrierRecordsDropped.Add(1)
			return
		}
	}
	select {
	case s.queue <- dgram:
		return
	default:
	}
	// The queue is full; which datagram to shed depends on the sink.
	if !s.reliable {
		// Datagram viewer: drop-newest; it is loss-tolerant.
		s.dropped.Add(1)
		return
	}
	// Reliable carrier: this overflow already costs the viewer a freeze to
	// the next keyframe, so mark the GOP dead and drop the newcomer. Not
	// drop-oldest (docs/24): the GOP-level mark leaves the drain idle for the
	// next keyframe instead of writing records nobody will decode.
	s.carDeadRotation.Store(s.carRotations.Load() + 1)
	s.carrierRecordsDropped.Add(uint64(s.purgeQueuedDeltasLocked()))
	// One overflow event per dead GOP, not one per shed packet. It gets its
	// own reason bucket because it holes a stream the viewer is told is
	// reliable, a different failure from a slow datagram viewer's. It still
	// counts in `dropped` (a slice of that budget, so queue_full stays
	// derivable by subtraction); the shed tail counts only in
	// carrierRecordsDropped, which keeps that subtraction honest.
	s.dropped.Add(1)
	s.carrierQueueOverflow.Add(1)
}

// purgeQueuedDeltasLocked empties the already-queued video deltas of a GOP that
// has just been declared dead, returning how many it discarded, so the drain
// doesn't work through a full queue of records the viewer will discard.
// Control datagrams are put back in order: above all the ViewerCount
// keepalive must survive, since a viewer's dead-session watchdog reads it as
// proof its session is alive.
//
// Caller holds registry.mu: enqueueLocked is the sole sender and the drain
// only removes, so re-enqueuing always fits. Sends stay non-blocking anyway —
// fan-out must never block under the lock.
func (s *Subscriber) purgeQueuedDeltasLocked() int {
	queued := len(s.queue)
	if queued == 0 {
		return 0
	}
	purged := 0
	keep := make([][]byte, 0, queued)
drain:
	for range queued {
		select {
		case d := <-s.queue:
			if isVideoChunkDatagram(d) {
				purged++
				continue
			}
			keep = append(keep, d)
		default:
			break drain
		}
	}
	for _, d := range keep {
		select {
		case s.queue <- d:
		default:
		}
	}
	return purged
}

func (s *Subscriber) drain() {
	defer close(s.done)
	if s.dvr != nil {
		// The queue still carries this subscriber's control traffic; a
		// separate goroutine drains it so neither can stall the other.
		go s.drainControlSideband()
		if s.dvrAudio != nil {
			go s.drainDVRAudio()
		}
		s.drainDVR()
		return
	}
	if s.reliable {
		s.drainReliable()
		return
	}
	for dgram := range s.queue {
		if !s.hub.registry.consumeBandwidth(len(dgram)) {
			s.dropped.Add(1)
			s.hub.countBandwidthDrop(len(dgram))
			continue
		}
		if err := s.sender.SendDatagram(dgram); err != nil {
			s.sendErrors.Add(1)
			continue
		}
		s.egressDatagramBytes.Add(uint64(len(dgram)))
		// Parity's share, counted where the bytes actually left. A SLICE of
		// egressDatagramBytes: exposing it as another egress_bytes_total{kind}
		// would look like a partition and double-count on sum.
		if isParityDatagram(dgram) {
			s.egressParityBytes.Add(uint64(len(dgram)))
		}
	}
}

// drainReliable is the reliable-delivery drain (docs/24): every dequeued
// datagram is written as a length-prefixed record onto the current carrier.
// The keyframe fan-out rotates the carrier per GOP; a carrier whose write
// fails or times out is cancelled and its GOP's remaining records dropped —
// drops-over-stalls at GOP granularity.
func (s *Subscriber) drainReliable() {
	defer s.retireCarrier()
	var carSeen uint64
	carDead := false
	var scratch []byte
	for dgram := range s.queue {
		// Audio never rides the VIDEO carrier; enqueueLocked diverts it to
		// its own lane, so this branch is an unreachable safety net. It must
		// stay a *datagram* one: the audio carrier belongs to
		// drainAudioSideband's goroutine, and writing it here would race.
		if isAudioDatagram(dgram) {
			s.sendSidebandDatagram(dgram)
			continue
		}
		if rot := s.carRotations.Load(); rot != carSeen {
			// A keyframe fanned out since the last record: this GOP's carrier
			// is done. Retire it and start the next one lazily below.
			s.retireCarrier()
			carSeen = rot
			carDead = false
		}
		if carDead || s.closed.Load() {
			s.carrierRecordsDropped.Add(1)
			continue
		}
		var err error
		scratch, err = wire.AppendCarrierRecord(scratch[:0], dgram)
		if err != nil {
			// Unreachable for queue datagrams (all ≤ MaxDatagramSize); count
			// rather than crash the drain if the invariant ever breaks.
			s.carrierRecordsDropped.Add(1)
			continue
		}
		// Charge the egress cap for exactly the bytes about to go out (plus
		// the prologue on a lazy open), after the drop decisions above: the
		// bucket is pod-wide, so charging an unwritten record throttles
		// somebody else's viewers. Over-cap records count as bandwidth
		// datagram drops, so queue_full stays derivable by subtraction.
		needsOpen := s.currentCarrier() == nil
		n := len(scratch)
		if needsOpen {
			n += wire.CarrierPrologueSize
		}
		if !s.hub.registry.consumeBandwidth(n) {
			s.dropped.Add(1)
			s.hub.countBandwidthDrop(n)
			continue
		}
		// One deadline shared by the lazy open's prologue and the record.
		deadline := time.Now().Add(s.hub.registry.opts.carrierWriteTimeout())
		if needsOpen && !s.openCarrier(deadline) {
			carDead = true
			s.carrierRecordsDropped.Add(1)
			continue
		}
		if !s.writeCarrier(scratch, deadline) {
			carDead = true
			s.carrierRecordsDropped.Add(1)
			continue
		}
		s.carrierRecords.Add(1)
		s.egressCarrierBytes.Add(uint64(len(scratch)))
	}
}

// drainAudioSideband is the second drain: audio only, onto an audio carrier
// of its own, so audio never inherits drainReliable's write stalls.
func (s *Subscriber) drainAudioSideband() {
	defer close(s.audioDone)
	defer s.retireAudioCarrier(&s.audioCar)
	var scratch []byte
	for dgram := range s.audioQueue {
		if s.closed.Load() {
			continue // drain the channel without writing to a dead session
		}
		framed, n, ok := frameAudioRecord(dgram, &scratch, s.audioCar == nil)
		if !ok {
			s.carrierRecordsDropped.Add(1)
			continue
		}
		if !s.hub.registry.consumeBandwidth(n) {
			s.dropped.Add(1)
			s.hub.countBandwidthDrop(n)
			continue
		}
		if !s.emitAudioRecord(&s.audioCar, framed) {
			s.carrierRecordsDropped.Add(1)
		}
	}
}

// --- the audio carrier -----------------------------------------------------
//
// Audio rides a reliable stream of its OWN on every subscriber that gets
// reliable delivery at all — resilient and DVR alike (docs/20). Sharing the
// VIDEO carrier would make audio inherit the deltas' head-of-line blocking
// and the per-GOP tail drop; a separate QUIC stream has neither problem.
//
// There is no rotation: audio has no keyframes, and a resync is just a
// timestamp discontinuity the viewer's jitter buffer handles. The records
// are audio datagrams, which the viewer already routes by type.

// frameAudioRecord frames one audio datagram and reports the bytes it will put
// on the wire — the record, plus the prologue when this is the record whose
// lazy open starts the carrier. ok is false only for a datagram that cannot
// be framed, which is unreachable for queued datagrams.
func frameAudioRecord(dgram []byte, scratch *[]byte, needsOpen bool) (framed []byte, n int, ok bool) {
	framed, err := wire.AppendCarrierRecord((*scratch)[:0], dgram)
	if err != nil {
		return nil, 0, false
	}
	*scratch = framed
	n = len(framed)
	if needsOpen {
		n += wire.CarrierPrologueSize
	}
	return framed, n, true
}

// emitAudioRecord opens the audio carrier if it is not up and writes one framed
// record onto it, under the same per-record deadline the video carrier uses.
// *car belongs to the single goroutine draining that lane, so it needs no lock
// — unlike carCurrent, which Close cancels from elsewhere.
//
// A failure cancels the stream, nils *car and reports false: the caller drops
// that packet and the next one (20 ms later) reopens. Deliberately NOT fed
// into the 4001 eviction streak: a peer that cannot take streams already
// fails the keyframe and video-carrier opens.
func (s *Subscriber) emitAudioRecord(car *KeyframeStream, framed []byte) bool {
	deadline := time.Now().Add(s.hub.registry.opts.carrierWriteTimeout())
	if *car == nil {
		st, err := s.sender.OpenCarrierStream()
		if err != nil {
			return false
		}
		_ = st.SetWriteDeadline(deadline)
		if _, err := st.Write(wire.AppendCarrierPrologue(nil)); err != nil {
			st.CancelWrite()
			return false
		}
		*car = st
		s.egressCarrierBytes.Add(wire.CarrierPrologueSize)
	}
	_ = (*car).SetWriteDeadline(deadline)
	if _, err := (*car).Write(framed); err != nil {
		(*car).CancelWrite()
		*car = nil
		return false
	}
	s.carrierRecords.Add(1)
	s.egressCarrierBytes.Add(uint64(len(framed)))
	return true
}

// Note on accounting: the audio carrier deliberately does NOT increment
// carrierStreams ("how many GOPs got a carrier?"); its records and bytes
// still count in carrierRecords and egressCarrierBytes.

// retireAudioCarrier gracefully closes an audio carrier when its drain exits.
// Same per-record budget as a write: this runs on the drain goroutine, so a
// Close that blocks would hold the session's teardown.
func (s *Subscriber) retireAudioCarrier(car *KeyframeStream) {
	if *car == nil {
		return
	}
	_ = (*car).SetWriteDeadline(time.Now().Add(s.hub.registry.opts.carrierWriteTimeout()))
	if err := (*car).Close(); err != nil {
		(*car).CancelWrite()
	}
	*car = nil
}

// isVideoChunkDatagram reports whether a fanned-out datagram is a video delta
// chunk — the only kind the dead-GOP shed may drop (see
// purgeQueuedDeltasLocked for why control traffic must survive).
func isVideoChunkDatagram(dgram []byte) bool {
	_, typ, err := wire.PeekType(dgram)
	return err == nil && typ == wire.TypeVideoChunk
}

// isAudioDatagram reports whether a fanned-out datagram belongs to the audio
// lane (frame or config). A peek failure can't happen for queued datagrams,
// which were validated before relaying.
func isAudioDatagram(dgram []byte) bool {
	_, typ, err := wire.PeekType(dgram)
	return err == nil && (typ == wire.TypeAudioFrame || typ == wire.TypeAudioConfig)
}

// sendSidebandDatagram delivers a datagram to a reliable subscriber over the
// unreliable datagram path — byte-for-byte the normal drain's per-datagram
// handling (bandwidth charge, send, egress + error counters). It is the
// unreachable-safety-net sink for audio that somehow reaches the video drain;
// the audio lane's real sink is its own carrier stream.
func (s *Subscriber) sendSidebandDatagram(dgram []byte) {
	if !s.hub.registry.consumeBandwidth(len(dgram)) {
		s.dropped.Add(1)
		s.hub.countBandwidthDrop(len(dgram))
		return
	}
	if err := s.sender.SendDatagram(dgram); err != nil {
		s.sendErrors.Add(1)
		return
	}
	s.egressDatagramBytes.Add(uint64(len(dgram)))
}

// noteKeyframeOpenFailedLocked advances the consecutive-open-failure streak and
// reports whether this failure is the one that evicts. Requires kfMu: the live
// path maintains the streak under a lock it already holds across the supersede
// decision, so this is the shared definition rather than a second one.
func (s *Subscriber) noteKeyframeOpenFailedLocked() bool {
	s.kfConsecOpenFailed++
	evict := !s.evicting && s.kfConsecOpenFailed >= KeyframeOpenFailEvictThreshold
	if evict {
		s.evicting = true
	}
	return evict
}

// noteKeyframeOpenFailed is the same streak for a caller that holds no lock —
// the DVR drain, which opens its keyframe streams itself.
func (s *Subscriber) noteKeyframeOpenFailed() {
	s.kfMu.Lock()
	evict := s.noteKeyframeOpenFailedLocked()
	s.kfMu.Unlock()
	if evict {
		go s.evict()
	}
}

// noteKeyframeOpenSucceeded clears the streak; only consecutive failures mean
// a peer that cannot take streams at all.
func (s *Subscriber) noteKeyframeOpenSucceeded() {
	s.kfMu.Lock()
	s.kfConsecOpenFailed = 0
	s.kfMu.Unlock()
}

func (s *Subscriber) currentCarrier() KeyframeStream {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	return s.carCurrent
}

// openCarrier opens a fresh carrier stream and writes its prologue. Open
// failures feed the same consecutive-failure eviction streak as keyframe
// stream opens: a zombie subscriber with exhausted
// stream credit fails both kinds and is evicted with 4001. At most one open
// is attempted per rotation (openCarrier failing marks the GOP dead), so the
// streak grows at GOP cadence, not record cadence. deadline is the caller's
// per-record budget, shared with the record write that follows; so is the
// prologue's egress-cap charge, which the caller has already taken.
func (s *Subscriber) openCarrier(deadline time.Time) bool {
	st, err := s.sender.OpenCarrierStream()
	if err != nil {
		s.kfMu.Lock()
		s.carConsecOpenFailed++
		evict := !s.evicting && s.carConsecOpenFailed >= CarrierOpenFailEvictThreshold
		if evict {
			s.evicting = true
		}
		s.kfMu.Unlock()
		if evict {
			go s.evict()
		}
		return false
	}
	s.kfMu.Lock()
	s.carConsecOpenFailed = 0
	s.kfMu.Unlock()

	s.carMu.Lock()
	if s.closed.Load() {
		s.carMu.Unlock()
		st.CancelWrite()
		return false
	}
	s.carCurrent = st
	s.carMu.Unlock()
	s.carrierStreams.Add(1)

	if !s.writeCarrier(wire.AppendCarrierPrologue(nil), deadline) {
		return false
	}
	s.egressCarrierBytes.Add(wire.CarrierPrologueSize)
	return true
}

// writeCarrier writes buf to the current carrier under the caller's deadline.
// A failed or timed-out write leaves a half-written record on the stream —
// unrecoverable framing for a length-prefixed protocol — so the carrier is
// cancelled; the viewer loses the tail of this GOP and resyncs at the keyframe
// it already reliably has.
func (s *Subscriber) writeCarrier(buf []byte, deadline time.Time) bool {
	s.carMu.Lock()
	st := s.carCurrent
	s.carMu.Unlock()
	if st == nil {
		return false // cancelled by Close between records
	}
	_ = st.SetWriteDeadline(deadline)
	if _, err := st.Write(buf); err != nil {
		st.CancelWrite()
		s.carMu.Lock()
		if s.carCurrent == st {
			s.carCurrent = nil
		}
		s.carMu.Unlock()
		return false
	}
	return true
}

// retireCarrier gracefully closes the current carrier (its GOP is over). A
// Close error means the peer reset it or the stream is wedged — cancel.
func (s *Subscriber) retireCarrier() {
	s.carMu.Lock()
	st := s.carCurrent
	s.carCurrent = nil
	s.carMu.Unlock()
	if st == nil {
		return
	}
	// Same per-record budget as a write: retiring runs on the drain goroutine
	// too, so a Close that blocks stalls the next GOP's records.
	_ = st.SetWriteDeadline(time.Now().Add(s.hub.registry.opts.carrierWriteTimeout()))
	if err := st.Close(); err != nil {
		st.CancelWrite()
	}
}

// sendKeyframe delivers one keyframe (the full StreamFrame message bytes) to
// this subscriber over a fresh unidirectional stream, superseding any stale
// in-flight one. seq orders sends: a lower seq than the last accepted is a
// stale prime and is skipped. The write itself runs on a separate goroutine so
// a stall never blocks the fan-out loop or the publisher.
func (s *Subscriber) sendKeyframe(msg []byte, seq uint64) {
	if s.closed.Load() {
		return
	}
	// The reliable keyframe counts against the global egress budget; checked
	// before opening the stream because a stream can't be dropped mid-flight.
	// Bandwidth-dropped keyframes count bytes against the shared dropped-bytes
	// counter but not the *datagram* drop counter (the reasons must not
	// bleed across kinds or queue_full can't be derived by subtraction).
	if !s.hub.registry.consumeBandwidth(len(msg)) {
		s.kfDroppedBandwidth.Add(1)
		s.hub.countBandwidthDropBytes(len(msg))
		return
	}

	s.kfMu.Lock()
	if s.closed.Load() || seq < s.kfLastSeq {
		s.kfMu.Unlock()
		s.kfDroppedSuperseded.Add(1)
		return
	}
	s.kfLastSeq = seq
	if s.reliable {
		// Per-GOP carrier rotation: the keyframe
		// that starts a GOP also rotates this subscriber's carrier. The drain
		// goroutine picks the new value up before its next record. Keyed on
		// accepted keyframes only (a superseded stale prime doesn't rotate)
		// and deliberately best-effort: a delta racing the keyframe may land
		// on the predecessor carrier — the viewer's reorder buffer sorts by
		// frameId regardless.
		s.carRotations.Add(1)
	}
	if s.kfCurrent != nil {
		// Supersede the stale in-flight keyframe; its writer goroutine's Write
		// returns an error and accounts the drop.
		s.kfCurrent.CancelWrite()
		s.kfCurrent = nil
	}
	stream, err := s.sender.OpenKeyframeStream()
	if err != nil {
		// Track the failure streak under kfMu. The evict itself runs
		// off-goroutine: Close takes the registry lock and CloseWithError is
		// a network op, neither belongs under kfMu.
		evict := s.noteKeyframeOpenFailedLocked()
		s.kfMu.Unlock()
		s.kfDroppedOpenFailed.Add(1)
		if evict {
			go s.evict()
		}
		return
	}
	s.kfConsecOpenFailed = 0
	s.kfCurrent = stream
	s.kfWriters.Add(1)
	s.kfMu.Unlock()

	go s.writeKeyframe(stream, msg)
}

func (s *Subscriber) writeKeyframe(stream KeyframeStream, msg []byte) {
	defer s.kfWriters.Done()

	_ = stream.SetWriteDeadline(time.Now().Add(s.hub.registry.opts.KeyframeWriteTimeout))
	_, err := stream.Write(msg)
	if err == nil {
		err = stream.Close()
	}

	// If kfCurrent no longer points at this stream, someone (a newer keyframe
	// or Close) cancelled it under kfMu — that classifies a failed write as
	// "superseded" rather than "slow". Checked under the same lock that does
	// the superseding, so the classification can't race the cause.
	//
	// The consecutive-slow streak is maintained under the same lock so it
	// cannot interleave with the supersede that decides whether this write
	// counts as a stall. A superseded write leaves the streak untouched; only
	// a genuine stall increments it, and any delivered keyframe clears it.
	s.kfMu.Lock()
	superseded := s.kfCurrent != stream
	if !superseded {
		s.kfCurrent = nil
	}
	evict := false
	switch {
	case err == nil:
		s.kfConsecSlow = 0
	case !superseded:
		s.kfConsecSlow++
		evict = !s.evicting && s.kfConsecSlow >= KeyframeSlowEvictThreshold
		if evict {
			s.evicting = true
		}
	}
	s.kfMu.Unlock()

	if err != nil {
		stream.CancelWrite()
		if superseded {
			s.kfDroppedSuperseded.Add(1)
		} else {
			// Deadline exceeded (stalled flow control) or a write/close error
			// to this peer — either way, this subscriber couldn't take it.
			s.kfDroppedSlow.Add(1)
		}
		// Off-goroutine for the same reason as the open-failure path: Close
		// takes the registry lock and CloseWithError is a network op.
		if evict {
			go s.evictStalled()
		}
		return
	}
	s.keyframesSent.Add(1)
	s.egressKeyframeBytes.Add(uint64(len(msg)))
}

// evict closes an unreachable subscriber's session and removes it from the
// hub (docs/14). The close code is non-terminal: a live client's
// reconnect gets a fresh session (and fresh stream credit); a zombie simply
// stops costing fan-out work. Fired at most once per subscriber (the
// `evicting` latch) from its own goroutine.
func (s *Subscriber) evict() {
	s.hub.log.Warn("evicting unreachable subscriber: keyframe stream opens failing persistently",
		"subscriber", s.statsKey, "consecutive_open_failures", KeyframeOpenFailEvictThreshold)
	s.closeUnresponsive()
}

// evictStalled is the same eviction for a subscriber whose keyframe streams
// open fine but never drain. Logged
// separately from evict(): the two causes want different first questions of an
// operator — exhausted stream credit vs. a wedged stream/flow-control path on a
// session that is still happily taking datagrams.
func (s *Subscriber) evictStalled() {
	s.hub.log.Warn("evicting unreachable subscriber: keyframe stream writes stalling persistently",
		"subscriber", s.statsKey, "consecutive_slow_keyframes", KeyframeSlowEvictThreshold)
	s.closeUnresponsive()
}

func (s *Subscriber) closeUnresponsive() {
	_ = s.sender.CloseWithError(uint32(wire.CloseCodeSubscriberUnresponsive), "subscriber unresponsive")
	s.Close()
}

// waitAudioDrain blocks until the audio sideband drain has exited. A no-op on
// datagram subscribers, which have no second lane.
func (s *Subscriber) waitAudioDrain() {
	if s.audioDone != nil {
		<-s.audioDone
	}
}

// Close removes the subscriber and stops its drain loop.
func (s *Subscriber) Close() {
	b := s.hub
	r := b.registry
	r.mu.Lock()
	if s.closed.Load() {
		r.mu.Unlock()
		<-s.done
		s.waitAudioDrain()
		return
	}
	s.closed.Store(true)
	delete(b.subs, s)
	// A closing primary takes its stripe legs with it (docs/35 §14): this is
	// the single funnel every primary end passes
	// through — eviction, clean session end, abrupt death — so collecting
	// here is what makes the reap hold regardless of the cause. Collected
	// under the lock, closed outside it below.
	orphanedLegs := s.orphanedLegsLocked()
	close(s.queue)
	if s.audioQueue != nil {
		close(s.audioQueue)
	}
	if s.dvrStop != nil {
		// The DVR drain blocks on the ring rather than on the queue, so
		// closing the queue does not release it — this does.
		close(s.dvrStop)
	}
	r.mu.Unlock()

	s.reapLegs(orphanedLegs)

	// Cancel any in-flight keyframe stream and wait for its writer to finish,
	// outside the registry lock (stream ops must never hold it).
	s.kfMu.Lock()
	if s.kfCurrent != nil {
		s.kfCurrent.CancelWrite()
		s.kfCurrent = nil
	}
	s.kfMu.Unlock()
	s.kfWriters.Wait()

	// Cancel the current carrier so a drain blocked in a record write
	// unblocks now instead of waiting out its write deadline; the backlog it
	// then consumes is dropped (drain checks closed before opening anew).
	s.carMu.Lock()
	if s.carCurrent != nil {
		s.carCurrent.CancelWrite()
		s.carCurrent = nil
	}
	s.carMu.Unlock()

	<-s.done
	// Both drains must be finished before the fold below: the audio lane keeps
	// its own drop/send-error/egress counters, and folding while it still runs
	// loses whatever it does next.
	s.waitAudioDrain()

	// Fold the final counters only after drain has finished: drain keeps
	// consuming (and bandwidth-dropping) the queued backlog after the queue
	// closes, so folding any earlier loses those drops. The hub may itself
	// have been GC'd by now — credit the registry totals then.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[b.id] == b {
		b.stripeTransitions += s.stripeTransitions.Load()
		b.datagramsDropped += s.dropped.Load()
		b.keyframeStreamsSent += s.keyframesSent.Load()
		b.keyframeDrops.add(s.keyframeDrops())
		b.sendErrors += s.sendErrors.Load()
		b.egressDatagramBytes += s.egressDatagramBytes.Load()
		b.egressParityBytes += s.egressParityBytes.Load()
		b.egressKeyframeBytes += s.egressKeyframeBytes.Load()
		b.carrierStreams += s.carrierStreams.Load()
		b.carrierRecords += s.carrierRecords.Load()
		b.carrierRecordsDropped += s.carrierRecordsDropped.Load()
		b.carrierQueueOverflow += s.carrierQueueOverflow.Load()
		b.egressCarrierBytes += s.egressCarrierBytes.Load()
		b.dvrResyncs += s.dvrResyncs.Load()
	} else {
		r.totalDatagramsDropped += s.dropped.Load()
		r.totalKeyframeStreamsSent += s.keyframesSent.Load()
		r.totalKeyframeDrops.add(s.keyframeDrops())
		r.totalSendErrors += s.sendErrors.Load()
		r.totalEgressDatagramBytes += s.egressDatagramBytes.Load()
		r.totalEgressParityBytes += s.egressParityBytes.Load()
		r.totalEgressKeyframeBytes += s.egressKeyframeBytes.Load()
		r.totalCarrierStreams += s.carrierStreams.Load()
		r.totalCarrierRecords += s.carrierRecords.Load()
		r.totalCarrierRecordsDropped += s.carrierRecordsDropped.Load()
		r.totalCarrierQueueOverflow += s.carrierQueueOverflow.Load()
		r.totalEgressCarrierBytes += s.egressCarrierBytes.Load()
		r.totalDVRResyncs += s.dvrResyncs.Load()
		r.totalStripeTransitions += s.stripeTransitions.Load()
	}
}

// RecordDownstreamViewers stores an edge's reported local viewer count,
// from the origin's internal-subscribe read loop — the ONLY place a
// client-sent ViewerCount is trusted, because the peer there is a
// PSK-authenticated, generation-fenced edge (docs/23).
func (s *Subscriber) RecordDownstreamViewers(count uint32) {
	s.downstreamViewers.Store(uint64(count))
}

// SetTelemetrySession records the telemetry session handle this
// subscriber was told in its TelemetryHello (docs/33), so /statusz can
// carry it and the two sides of one viewer become joinable. Called by the
// transport right after the hello is sent; a no-op with an empty id, which is
// the telemetry-disabled path.
func (s *Subscriber) SetTelemetrySession(sessionID string) {
	if sessionID == "" {
		return
	}
	r := s.hub.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	s.sessionID = sessionID
}

// Dropped reports dropped datagrams count.
func (s *Subscriber) Dropped() uint64 { return s.dropped.Load() }

// KeyframesSent reports keyframe streams fully delivered to this subscriber.
func (s *Subscriber) KeyframesSent() uint64 { return s.keyframesSent.Load() }

// KeyframesDropped reports keyframe streams dropped for this subscriber
// (superseded, slow, bandwidth-limited, or open failures).
func (s *Subscriber) KeyframesDropped() uint64 { return s.keyframeDrops().Total() }

func (r *Registry) consumeBandwidth(n int) bool {
	if r.limiter == nil {
		return true
	}
	return r.limiter.consume(n)
}

// countBandwidthDrop records one bandwidth-limited *datagram* drop. It runs
// on drain goroutines, which can outlive their broadcast: after
// handleGraceExpiry has folded the hub's counters into the totals and deleted
// it, incrementing the orphaned hub struct would lose the count — credit the
// totals directly then.
func (b *broadcastHub) countBandwidthDrop(n int) {
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[b.id] == b {
		b.bandwidthDroppedDatagrams++
		b.bandwidthDroppedBytes += uint64(n)
	} else {
		r.totalBandwidthDroppedDatagrams++
		r.totalBandwidthDroppedBytes += uint64(n)
	}
}

// countBandwidthDropBytes records the bytes of a bandwidth-limited *keyframe*
// drop. The count itself lives in the subscriber's per-cause keyframe drop
// counters — bandwidthDroppedDatagrams must stay datagram-only so
// queue-overflow drops can be derived by subtraction without going negative.
func (b *broadcastHub) countBandwidthDropBytes(n int) {
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[b.id] == b {
		b.bandwidthDroppedBytes += uint64(n)
	} else {
		r.totalBandwidthDroppedBytes += uint64(n)
	}
}

// newSubscriberStatsKey mints the random per-session key naming a subscriber
// in /statusz subscriberDetails.
func newSubscriberStatsKey() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}
