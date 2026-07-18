// Package hub implements the relay's pub/sub core: a registry of broadcast
// sessions, where each broadcast has a publisher fanning encoded video out to a
// small set of subscribers.
//
// The hub is a byte forwarder on two channels (R8, docs/12):
//   - Delta frames travel as datagrams. The hub parses their headers only to
//     observe (counting frames) and forwards them verbatim; every subscriber
//     owns a bounded queue drained by its own goroutine, and a full queue drops
//     the datagram for that subscriber so a slow peer never blocks others.
//   - Keyframes travel as reliable unidirectional streams. The publisher's
//     ingest goroutine reads each keyframe stream to completion into one
//     bounded buffer (which doubles as the broadcast's cached keyframe), then
//     fan-out opens one uni stream per subscriber and writes that buffer on a
//     per-subscriber goroutine with a write deadline. A subscriber that stalls
//     is cancelled and recovers at the next keyframe — the datagram drop
//     discipline, at stream granularity — while the publisher ingest is fully
//     decoupled and never touches a subscriber stream.
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

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
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
// stream-open failures after which a subscriber is evicted (session closed
// with wire.CloseCodeSubscriberUnresponsive and removed). Persistent open
// failure means the peer's uni-stream credit is exhausted — the signature of
// a session whose client stopped reading streams (R10 field finding,
// docs/14): it never recovers on its own, and without eviction the relay
// burns fan-out work on it forever while /statusz counts a ghost viewer.
// At the default 500 ms GOP, 10 misses ≈ 5 s of unreachability — far above
// any transient stream-limit blip, and a wrongly-evicted live client just
// reconnects (the code is non-terminal). A constant, not a knob: this is
// correctness (leak cleanup), not capacity tuning.
const KeyframeOpenFailEvictThreshold = 10

// ViewerCountInterval paces the R18 count pump (docs/23 Decision 4): one
// recompute-and-emit pass per tick, which is also what makes a reconnect
// storm structurally unable to spam clients — emits can never exceed
// 1/s/broadcast. ViewerCountKeepalive bounds how long an *unchanged* count
// goes unrepeated: datagrams are lossy, and the periodic re-emit repairs a
// dropped update for already-connected clients (new joiners are covered by
// the join-prime cache). Constants like timeSyncReplyRate, not knobs.
// Exported because the transport's edge pump reports its local count
// upstream on the same change-driven + keepalive discipline (Decision 5a).
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
	// used as a reliable delta carrier for a resilient subscriber (R19,
	// docs/24). Identical transport mechanics to a keyframe stream; the
	// distinct method keeps the two stream kinds visible in fakes and stats.
	OpenCarrierStream() (KeyframeStream, error)
	CloseWithError(code uint32, reason string) error
}

// SessionCloser is the slice of a publisher's session the hub needs to depose
// it when a newer publisher session takes over the broadcast (docs/06
// revision 2026-07-18). The transport layer binds it after the session
// upgrade via Publisher.BindConn.
type SessionCloser interface {
	CloseWithError(code uint32, reason string) error
}

// KeyframeStream is the minimal write side of a unidirectional stream the hub
// uses to deliver one keyframe — and, since R19, to carry a resilient
// subscriber's delta records (the write surface is identical). The transport
// layer's adapter maps it onto a webtransport SendStream.
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
	// QueueDepth is the per-subscriber datagram queue capacity. Deltas are
	// usually one datagram each, but a high-motion delta can span a few, so
	// this comfortably exceeds a burst. Defaults to 256.
	QueueDepth int
	// BroadcastGrace is the amount of time a broadcast ID survives after its
	// publisher disconnects, allowing it to be reclaimed. Defaults to 5 minutes.
	BroadcastGrace time.Duration

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

	// Cluster-mode lifecycle hooks (R17 W3, docs/22 Decision 8) — nil in
	// single-pod mode. Both are invoked OUTSIDE the registry lock (they do
	// Kubernetes API I/O): OnPublisherClosed when a publisher disconnects and
	// the grace timer starts (the origin stops renewing its Lease and stamps
	// the grace deadline); OnBroadcastExpired when grace-GC deletes the hub
	// (the Lease is deleted — cluster-wide "broadcast ended").
	OnPublisherClosed  func(broadcastID string)
	OnBroadcastExpired func(broadcastID string)

	// StatsKey keys ObfuscateID (R17 W6, docs/22 Decision 14): 32 bytes,
	// shared across the fleet so one broadcast keeps ONE obfuscated identity
	// in every pod's /statusz and gawk_broadcast_* series. Empty falls back
	// to a fresh per-process key — exactly the pre-R17 single-pod behavior.
	StatsKey []byte
}

// KeyframeDrops breaks keyframe-stream drops down by cause (R9 M2). The
// split is what makes the counter diagnostic: "superseded" is benign (a newer
// keyframe replaced an in-flight one), "slow" is a stalling subscriber,
// "bandwidth" is the configured egress cap, "open_failed" is a session-level
// stream-open failure.
type KeyframeDrops struct {
	Superseded uint64 `json:"superseded"`
	Slow       uint64 `json:"slow"`
	Bandwidth  uint64 `json:"bandwidth"`
	OpenFailed uint64 `json:"openFailed"`
}

// Total is the sum across all causes (the pre-R9 aggregate counter).
func (k KeyframeDrops) Total() uint64 {
	return k.Superseded + k.Slow + k.Bandwidth + k.OpenFailed
}

func (k *KeyframeDrops) add(o KeyframeDrops) {
	k.Superseded += o.Superseded
	k.Slow += o.Slow
	k.Bandwidth += o.Bandwidth
	k.OpenFailed += o.OpenFailed
}

// SubscriberStats is the per-subscriber breakdown inside a broadcast's Stats
// (R9 M3): live-debugging detail for "which viewer is slow", keyed by a
// random per-session key (never anything joinable or identifying). It is
// deliberately JSON-only — per-subscriber Prometheus labels would be
// pointless series churn.
type SubscriberStats struct {
	Key              string `json:"key"`        // random per-session key, stable across /statusz polls
	QueueDepth       int    `json:"queueDepth"` // current datagram queue occupancy
	Dropped          uint64 `json:"dropped"`
	SendErrors       uint64 `json:"sendErrors"`
	KeyframesSent    uint64 `json:"keyframesSent"`
	KeyframesDropped uint64 `json:"keyframesDropped"`
	// Internal marks a downstream edge session (R17 W4) — plumbing, not a
	// viewer; excluded from the Subscribers counts.
	Internal bool `json:"internal,omitempty"`
	// Reliable marks an R19 resilient subscriber: deltas are delivered as
	// records on carrier streams instead of datagrams (docs/24). The carrier
	// counters below are zero (and omitted) for datagram subscribers.
	Reliable              bool   `json:"reliable,omitempty"`
	CarrierStreams        uint64 `json:"carrierStreams,omitempty"`
	CarrierRecords        uint64 `json:"carrierRecords,omitempty"`
	CarrierRecordsDropped uint64 `json:"carrierRecordsDropped,omitempty"`
}

// Stats is a point-in-time snapshot of hub state, for logging and the
// GET /statusz endpoint (the json tags are its response shape).
type Stats struct {
	PublisherActive bool `json:"publisherActive"`
	// Role of this hub in the R17 federation: "origin" (hosts the real
	// publisher — the only role in single-pod mode) or "edge" (derived state
	// fed by an upstream pull, docs/22 Decision 10).
	Role string `json:"role"`
	// Subscribers counts local viewers only; internal edge sessions are
	// accounted separately (EdgeSessions) — an edge is fan-out plumbing, not
	// an audience member (docs/22 Decision 10/14).
	Subscribers int `json:"subscribers"`
	// EdgeSessions counts downstream edge pods attached via the internal
	// subscribe route.
	EdgeSessions int `json:"edgeSessions"`
	// ViewersGlobal is the R18 global viewer count this pod computes as
	// origin (local viewers + Σ edge downstream reports — the G it pushes to
	// clients); always 0 on edge hubs, which receive the number from
	// upstream instead of computing it (docs/23 Decision 9).
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

	// R9 additions (docs/13). Ingress = publisher→relay, egress = relay→
	// subscribers; the lost counters come from the ingress window (ingress.go)
	// and attribute loss to the broadcaster→relay leg specifically.
	KeyframeDrops        KeyframeDrops     `json:"keyframeDrops"`
	SendErrors           uint64            `json:"sendErrors"`           // datagram write failures to subscribers
	IngressDatagramBytes uint64            `json:"ingressDatagramBytes"` // valid delta/config datagram bytes from the publisher
	EgressDatagramBytes  uint64            `json:"egressDatagramBytes"`  // datagram bytes actually written to subscribers
	EgressKeyframeBytes  uint64            `json:"egressKeyframeBytes"`  // keyframe stream bytes fully delivered
	IngressFramesLost    uint64            `json:"ingressFramesLost"`    // frames the publisher sent that never arrived
	IngressChunksLost    uint64            `json:"ingressChunksLost"`    // missing chunks of frames that did arrive
	SubscriberDetails    []SubscriberStats `json:"subscriberDetails"`

	// R19 reliable delivery (docs/24 Decision 10).
	ReliableSubscribers   int    `json:"reliableSubscribers"`   // live local subscribers in reliable mode
	CarrierStreams        uint64 `json:"carrierStreams"`        // carrier streams opened
	CarrierRecords        uint64 `json:"carrierRecords"`        // records fully written to carriers
	CarrierRecordsDropped uint64 `json:"carrierRecordsDropped"` // records dropped: dead carrier, open/write failure
	EgressCarrierBytes    uint64 `json:"egressCarrierBytes"`    // carrier bytes written (prologues + records)
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

	// R9 additions — see the Stats field comments.
	KeyframeDrops        KeyframeDrops `json:"keyframeDrops"`
	SendErrors           uint64        `json:"sendErrors"`
	IngressDatagramBytes uint64        `json:"ingressDatagramBytes"`
	EgressDatagramBytes  uint64        `json:"egressDatagramBytes"`
	EgressKeyframeBytes  uint64        `json:"egressKeyframeBytes"`
	IngressFramesLost    uint64        `json:"ingressFramesLost"`
	IngressChunksLost    uint64        `json:"ingressChunksLost"`

	// R17 W6 (docs/22 Decision 14): the edge-leg (origin→edge) loss windows,
	// accumulated from EDGE hubs only — kept apart from the broadcaster-leg
	// numbers above (origin hubs), never mixed: the two legs have different
	// owners and different fixes.
	EdgeIngressFramesLost uint64 `json:"edgeIngressFramesLost"`
	EdgeIngressChunksLost uint64 `json:"edgeIngressChunksLost"`

	// R19 reliable delivery — see the Stats field comments.
	ReliableSubscribers   int    `json:"reliableSubscribers"`
	CarrierStreams        uint64 `json:"carrierStreams"`
	CarrierRecords        uint64 `json:"carrierRecords"`
	CarrierRecordsDropped uint64 `json:"carrierRecordsDropped"`
	EgressCarrierBytes    uint64 `json:"egressCarrierBytes"`
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
	totalEgressCarrierBytes        uint64

	limiter *bandwidthLimiter

	// statsKey keys ObfuscateID so /statusz broadcast keys can't be
	// brute-forced back to joinable IDs. Fresh per process; stats keys are
	// only ever compared within one server run.
	statsKey []byte
}

// broadcastHub is the per-broadcast session unit.
type broadcastHub struct {
	registry *Registry
	id       string
	log      *slog.Logger

	publisherActive bool
	// publisher is the handle currently holding the slot (nil when inactive).
	// TakeOverPublish needs it to depose the incumbent.
	publisher  *Publisher
	generation uint64
	graceTimer *time.Timer
	graceStart time.Time
	// edge marks a hub as derived state (R17 W4): its "publisher" is this
	// pod's upstream pull from the broadcast's origin, it is exempt from
	// MaxBroadcasts, and it never idles in grace — the Lease is the liveness
	// truth (docs/22 Decision 10): a lingered-out pull deletes the hub via
	// ExpireEdgeIfViewerless, and the grace timer a mid-stream upstream loss
	// arms is cancelled by the re-attach's claim. Flipped only through
	// setRoleLocked so ingress-loss counts never cross legs.
	edge bool

	subs map[*Subscriber]struct{}

	// cachedKeyframe holds the full StreamFrame message bytes (header + embedded
	// config + payload) of the last complete keyframe, replayed verbatim to
	// prime late joiners. Immutable once set; nil until the first keyframe of
	// the current publisher session. keyframeSeq is a per-hub monotonic counter
	// (never reset, even across publisher restarts) that orders keyframe sends
	// to a subscriber so a late prime can't supersede a newer live keyframe.
	cachedKeyframe          []byte
	cachedKeyframeID        uint32
	cachedKeyframeHasConfig bool
	keyframeSeq             uint64

	// cachedClockMapping holds the latest ClockMapping datagram verbatim
	// (R5 Q2, docs/15): relayed to live subscribers as it arrives, replayed to
	// prime late joiners, and invalidated on a new publisher session — frame
	// timestamps live on the publisher's clock timeline, so a new session's
	// mapping is a different mapping.
	cachedClockMapping []byte

	// cachedViewerCount holds the latest global ViewerCount datagram verbatim
	// (R18, docs/23 Decision 3 — the ClockMapping template): fanned out live,
	// replayed to prime late joiners, cleared alongside the other caches. On
	// an origin the count pump produces it; on an edge it arrives from
	// upstream and is forwarded verbatim (Decision 5c — counts are
	// pod-independent, no per-hop rewrite).
	cachedViewerCount []byte
	// Count-pump emit tracking (origin hubs only): the last count pushed and
	// when, making emits change-driven with a keepalive re-send (Decision 4).
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
	// Ingress-loss window (R9 M3): cumulative counters live here — not on the
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
	// R19 carrier counters, folded like the above.
	carrierStreams        uint64
	carrierRecords        uint64
	carrierRecordsDropped uint64
	egressCarrierBytes    uint64
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
	if opts.QueueDepth <= 0 {
		opts.QueueDepth = 256
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
		// Mint a new ID (with collision check)
		var newID string
		var err error
		for range 10 {
			newID, err = broadcastid.Mint()
			if err != nil {
				return "", nil, err
			}
			if _, exists := r.hubs[newID]; !exists {
				break
			}
		}
		if _, exists := r.hubs[newID]; exists {
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
	// prior demote-to-edge is over when the broadcaster comes home (W5).
	r.setRoleLocked(b, false)
	return id, pub, nil
}

// ResumePublish claims the publisher slot of a specific broadcast ID,
// creating the hub when this process doesn't know it (R17 W2, docs/22
// Decision 7). The caller MUST have verified a resume token first — the
// token is the proof of ownership that makes an unknown ID a legitimate
// resume (a relay restart, or a different pod) rather than a 404. Creating
// counts against MaxBroadcasts like a mint.
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
	// The broadcaster re-homed onto this pod: origin again (W5 un-demote).
	r.setRoleLocked(b, false)
	return normID, pub, nil
}

// EdgePublish claims the publisher slot of an EDGE hub for a broadcast this
// pod serves via an upstream pull (R17 W4). Creating an edge hub is exempt
// from MaxBroadcasts — edge hubs are derived state, not broadcasts (docs/22
// Decision 10). The claim resets the prime caches exactly like a real
// publisher claim, which is one half of the "stale prime impossible"
// guarantee (the other is InvalidatePrimes on upstream loss).
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
	// The role flips only on a SUCCESSFUL claim (post-review fix, PR #47):
	// on ErrPublisherActive the hub belongs to whoever holds the slot —
	// possibly a live origin publisher — and must keep its role, and with it
	// the origin's lease lifecycle hooks at close/expiry.
	r.setRoleLocked(b, true)
	return normID, pub, nil
}

// InvalidatePrimes clears the cached keyframe/config/clock-mapping NOW
// (R17 W4): called when an edge's upstream session ends, so a viewer joining
// between the drop and the re-attach waits for the fresh join-prime instead
// of being served origin A's prime alongside origin B's deltas (docs/22
// Decision 10). keyframeSeq is not reset — same supersede rule as a
// publisher restart.
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
	// A count from the lost origin may be stale by the time we re-attach;
	// the fresh origin's fan-out (or the next report round-trip) replaces it.
	b.cachedViewerCount = nil
}

// CloseInternalSubscribers closes every downstream edge session of a
// broadcast with the given code (R17 W5: 4003 on demote — the Go edge
// clients re-resolve the lease and re-attach at the new origin). Local
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

// ExternalSubscribers reports the local (non-internal) viewer count for a
// broadcast — the edge linger signal (R17 W4).
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

// newHubLocked creates and registers an empty hub. Caller holds r.mu.
func (r *Registry) newHubLocked(id string) *broadcastHub {
	b := &broadcastHub{
		registry: r,
		id:       id,
		log:      r.log.With("broadcast_id", id),
		subs:     make(map[*Subscriber]struct{}),
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

	// Cancel grace timer if running
	if b.graceTimer != nil {
		b.graceTimer.Stop()
		b.graceTimer = nil
		b.graceStart = time.Time{}
	}

	b.publisherActive = true
	b.generation++

	// Reset the keyframe cache on a new publisher session (frameIDs reset, the
	// codec may differ). keyframeSeq is intentionally NOT reset so a keyframe
	// from the new session always outranks any stale prime still in flight.
	// (On a broadcaster auto-resume the frameIDs actually continue — R17 W2 —
	// but the reset stays correct: the forced keyframe refills within ~RTT.)
	b.cachedKeyframe = nil
	b.cachedKeyframeID = 0
	b.cachedKeyframeHasConfig = false
	// New session, new clock timeline: the old mapping is meaningless (R5 Q2).
	b.cachedClockMapping = nil
	// The count itself isn't tied to the session, but clearing it keeps the
	// cache lifecycle uniform (R18 Decision 3) — and resetting the emit
	// tracking guarantees the new session a fresh emit on the next tick.
	b.cachedViewerCount = nil
	b.viewerCountEverEmitted = false
	// New session, new frameID space: reset the ingress-loss window (its
	// cumulative counters live on the hub and survive).
	b.ingress.reset()

	p := &Publisher{hub: b}
	b.publisher = p
	return p, nil
}

// setRoleLocked flips the hub's federation role (post-review fix, PR #47).
// The per-hub ingress-loss counters are attributed to the hub's CURRENT role
// at scrape time (broadcaster leg for origin, edge leg for edge — never
// mixed, docs/22 Decision 14), so counts accumulated under the old role are
// folded into that leg's lifetime totals before the flip. Caller holds r.mu.
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
// (docs/06 revision 2026-07-18). It exists for the zombie lockout: a
// publisher whose session dies silently keeps the slot until the QUIC idle
// timeout notices, and 409ing its own reclaim in that window forced clients
// into a mint fallback that orphaned every viewer. It is the same-pod
// counterpart of R17 W3's lease force-take: the caller MUST have verified
// the resume token (the proof of ownership), and MUST call this only after
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
	if old := b.publisher; b.publisherActive && old != nil {
		// Depose the incumbent: mark it closed so its late datagrams and
		// keyframes drop and its handler's deferred Close is a no-op (no
		// grace timer, no OnPublisherClosed lease release — the slot and
		// the lease now belong to the new publisher).
		old.closed = true
		deposed = old.conn
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
	// as StartPublish/ResumePublish (W5 un-demote).
	r.setRoleLocked(b, false)
	r.mu.Unlock()

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

// globalViewersLocked computes an origin hub's global viewer count G (R18,
// docs/23 Decision 5): local real viewers, plus each attached edge's
// last-reported downstream count. An edge that hasn't reported yet
// contributes 0 — a brief undercount on fresh attach that heals within a
// tick or two. Edge sessions themselves are never counted (they are
// internal — fan-out plumbing, not audience). Caller holds r.mu.
func (b *broadcastHub) globalViewersLocked() uint32 {
	g := uint64(0)
	for s := range b.subs {
		if s.internal {
			g += s.downstreamViewers.Load()
		} else {
			g++
		}
	}
	if g > math.MaxUint32 {
		g = math.MaxUint32
	}
	return uint32(g)
}

// PumpViewerCounts runs one tick of the R18 count pump (docs/23 Decision 4):
// for every ORIGIN hub with an active publisher it computes the global
// viewer count G and — when G changed since the last emit or the keepalive
// elapsed — builds the ViewerCount datagram once, caches it, fans it to
// every subscriber (local viewers and edge sessions alike) and pushes it to
// the publisher. Edge hubs are skipped: they neither aggregate nor host a
// real broadcaster — they receive G from upstream and forward it (Decision
// 5c). Exported as the test seam; RunViewerCountPump drives it on the
// production cadence.
func (r *Registry) PumpViewerCounts(now time.Time) {
	type push struct {
		send  func([]byte)
		dgram []byte
	}
	var pushes []push
	r.mu.Lock()
	for _, b := range r.hubs {
		if b.edge || !b.publisherActive {
			continue
		}
		g := b.globalViewersLocked()
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

	// The publisher pushes are network I/O — done off the registry lock,
	// like keyframe writes.
	for _, p := range pushes {
		p.send(p.dgram)
	}
}

// RunViewerCountPump ticks PumpViewerCounts every ViewerCountInterval until
// ctx ends. One goroutine for the whole registry, started explicitly from
// main — never from NewRegistry — so existing tests (and the transport test
// helper) stay goroutine-free and drive ticks directly.
func (r *Registry) RunViewerCountPump(ctx context.Context) {
	t := time.NewTicker(ViewerCountInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.PumpViewerCounts(now)
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

// Subscribe registers a subscriber, re-checking under lock. A newly registered
// subscriber is primed with the cached keyframe (over a stream) after the lock
// is released, so it can show a first picture without waiting for the next
// keyframe.
func (r *Registry) Subscribe(id string, conn Conn) (*Subscriber, error) {
	return r.subscribe(id, conn, false, false)
}

// SubscribeReliable registers an R19 resilient subscriber (docs/24): its
// deltas are delivered as length-prefixed records on per-GOP reliable carrier
// streams instead of datagrams, and datagram sends to it are suppressed
// entirely. Everything else — queueing, keyframe streams, caps — is identical
// to Subscribe.
func (r *Registry) SubscribeReliable(id string, conn Conn) (*Subscriber, error) {
	return r.subscribe(id, conn, false, true)
}

// SubscribeInternal registers a downstream EDGE session (R17 W4): exempt
// from MaxSubscribers/MaxTotalSubscribers (capacity guards protect viewers;
// an edge is fan-out plumbing) and excluded from the viewer counts — the
// internal route is the edge marker (docs/22 Decision 10). Never reliable:
// the in-cluster leg keeps datagrams (docs/24 scale-out interop).
func (r *Registry) SubscribeInternal(id string, conn Conn) (*Subscriber, error) {
	return r.subscribe(id, conn, true, false)
}

func (r *Registry) subscribe(id string, conn Conn, internal, reliable bool) (*Subscriber, error) {
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
		queue:    make(chan []byte, r.opts.QueueDepth),
		done:     make(chan struct{}),
		statsKey: newSubscriberStatsKey(),
	}
	b.subs[s] = struct{}{}
	go s.drain()

	// Prime the joiner with the cached clock mapping (R5 Q2) so absolute
	// latency works without waiting for the broadcaster's next re-send. A
	// plain enqueue: it rides the normal datagram queue.
	if b.cachedClockMapping != nil {
		s.enqueueLocked(b.cachedClockMapping)
	}

	// Prime the cached viewer count the same way (R18 Decision 3): the badge
	// shows immediately on join instead of waiting for the next pump tick.
	if b.cachedViewerCount != nil {
		s.enqueueLocked(b.cachedViewerCount)
	}

	// Snapshot the cached keyframe under the lock; prime over a stream outside
	// it (stream I/O must never hold the registry lock). A live keyframe that
	// arrives meanwhile carries a higher keyframeSeq and supersedes this prime.
	primeMsg := b.cachedKeyframe
	primeSeq := b.keyframeSeq
	r.mu.Unlock()

	if primeMsg != nil {
		s.sendKeyframe(primeMsg, primeSeq)
	}

	return s, nil
}

// ObfuscateID returns the key under which a broadcast appears in this
// registry's Stats. It must not be computable by anyone who doesn't hold
// server state: broadcast IDs are only ~31^6 strong, so any unkeyed hash of
// them can be brute-forced offline from a /statusz scrape.
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
		// Loss attribution by leg (R17 W6): an edge hub's ingress window
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
		egressKf := b.egressKeyframeBytes
		carStreams := b.carrierStreams
		carRecords := b.carrierRecords
		carDropped := b.carrierRecordsDropped
		egressCar := b.egressCarrierBytes
		reliableSubs := 0
		details := make([]SubscriberStats, 0, len(b.subs))
		edgeSessions := 0
		for s := range b.subs {
			if s.internal {
				edgeSessions++
			}
			if s.reliable && !s.internal {
				reliableSubs++
			}
			subDrops := s.keyframeDrops()
			dropped += s.dropped.Load()
			kfSent += s.keyframesSent.Load()
			kfDrops.add(subDrops)
			sendErrors += s.sendErrors.Load()
			egressDgram += s.egressDatagramBytes.Load()
			egressKf += s.egressKeyframeBytes.Load()
			carStreams += s.carrierStreams.Load()
			carRecords += s.carrierRecords.Load()
			carDropped += s.carrierRecordsDropped.Load()
			egressCar += s.egressCarrierBytes.Load()
			details = append(details, SubscriberStats{
				Key:                   s.statsKey,
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
		totals.EgressCarrierBytes += egressCar

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
		obf := r.ObfuscateID(id)
		broadcasts[obf] = Stats{
			PublisherActive:           b.publisherActive,
			Role:                      role,
			Subscribers:               b.externalSubsLocked(),
			ViewersGlobal:             viewersGlobal,
			ReliableSubscribers:       reliableSubs,
			CarrierStreams:            carStreams,
			CarrierRecords:            carRecords,
			CarrierRecordsDropped:     carDropped,
			EgressCarrierBytes:        egressCar,
			EdgeSessions:              edgeSessions,
			FramesRelayed:             b.framesRelayed,
			DatagramsRelayed:          b.datagramsRelayed,
			DatagramsDropped:          dropped,
			BadDatagrams:              b.badDatagrams,
			BandwidthDroppedDatagrams: b.bandwidthDroppedDatagrams,
			BandwidthDroppedBytes:     b.bandwidthDroppedBytes,
			HasConfig:                 b.cachedKeyframeHasConfig,
			CachedKeyframeID:          b.cachedKeyframeID,
			CachedKeyframeBytes:       len(b.cachedKeyframe),
			KeyframeStreamsIn:         b.keyframeStreamsIn,
			KeyframeBytesIn:           b.keyframeBytesIn,
			KeyframeStreamsSent:       kfSent,
			KeyframeStreamsDropped:    kfDrops.Total(),
			KeyframeStreamsOversize:   b.keyframeStreamsOversize,
			GraceRemainingSeconds:     graceRemaining,
			KeyframeDrops:             kfDrops,
			SendErrors:                sendErrors,
			IngressDatagramBytes:      b.ingressDatagramBytes,
			EgressDatagramBytes:       egressDgram,
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
	totals.EgressCarrierBytes += r.totalEgressCarrierBytes
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

// EndBroadcast force-expires a broadcast immediately (R17 W3): its cluster
// Lease disappeared, meaning the broadcast ended somewhere in the fleet —
// local viewers get the terminal 4000 exactly as if the local grace expired.
// A no-op when this pod has an ACTIVE publisher for the ID (we are origin;
// a racing janitor must not kill a live broadcast) or doesn't know the ID.
func (r *Registry) EndBroadcast(id string) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return
	}
	r.expireBroadcast(normID, func(*broadcastHub) bool { return true })
}

// ExpireEdgeIfViewerless deletes an EDGE hub that has no local viewers — the
// linger-out path (post-review fix, PR #47). A lingered-out pull must take
// its derived hub with it: left in the ordinary grace, the hub would keep
// satisfying CheckSubscribe, so a viewer joining inside that window would
// attach with no upstream pull behind it (handleSubscribe runs EnsureEdge
// only on ErrNotFound) and eventually receive a wrong terminal 4000 while
// the broadcast is still live at the origin. Atomic with Subscribe under the
// registry lock: returns true when the hub is gone (deleted now, or already
// absent) and false when it must be kept — a viewer raced the linger window,
// or a publisher claimed the slot. Origin hubs are never expired here; their
// lifecycle belongs to the grace timer.
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

// expireBroadcast removes the hub (when it exists, has no active publisher,
// and passes ok), folds its counters, closes its subscribers with the
// terminal code, and fires OnBroadcastExpired. Returns whether the hub was
// actually removed.
func (r *Registry) expireBroadcast(id string, ok func(*broadcastHub) bool) bool {
	r.mu.Lock()
	b, exists := r.hubs[id]
	if !exists || b.publisherActive || !ok(b) {
		r.mu.Unlock()
		return false
	}

	delete(r.hubs, id)

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
	r.totalEgressCarrierBytes += b.egressCarrierBytes
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

	var subs []*Subscriber
	for s := range b.subs {
		// Still-live subscribers' drop counts are NOT folded here: their
		// drain loops may still be dropping. Subscriber.Close (called below)
		// folds each count once drain has finished, crediting the registry
		// totals because the hub is no longer registered.
		subs = append(subs, s)
	}
	edge := b.edge
	r.mu.Unlock()

	r.log.Info("broadcast expired and garbage collected", "broadcast_id", id, "subscribers", len(subs))

	for _, s := range subs {
		_ = s.sender.CloseWithError(uint32(wire.CloseCodeBroadcastEnded), "broadcast ended")
		s.Close()
	}

	// Outside the lock: cluster mode deletes the broadcast's Lease here —
	// ORIGIN hubs only. An edge hub is derived state that never owns the
	// lease; its expiry deleting the origin's lease would kill the broadcast
	// fleet-wide (R17 W4).
	if !edge && r.opts.OnBroadcastExpired != nil {
		r.opts.OnBroadcastExpired(id)
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
	// send pushes a relay-originated datagram to this publisher's session —
	// the R18 viewer-count push (docs/23 Decision 4), the one place the relay
	// *initiates* a datagram to a broadcaster. Nil until BindSend, cleared on
	// Close; a deposed publisher is unreachable via b.publisher anyway.
	// Guarded by the registry lock, like conn.
	send func([]byte)
}

// BindConn attaches the publisher's session close handle so a later
// TakeOverPublish can depose this session. It reports false when the
// publisher has already been superseded (deposed between its pre-upgrade
// claim and this bind) — the caller must then end its session rather than
// continue publishing into a deposed broadcast.
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

// BindSend attaches the relay→publisher datagram sender (R18): the count
// pump pushes ViewerCount datagrams through it. Registered by the transport
// after the session upgrade; sess.SendDatagram is goroutine-safe in quic-go,
// so the pump may call it concurrently with the read loop's TimeSync
// replies. A no-op on a publisher that was already closed or deposed.
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
// no longer travel as datagrams (R8) — they arrive via IngestKeyframeStream —
// so a keyframe-flagged VideoChunk here is forwarded verbatim but not cached.
func (p *Publisher) HandleDatagram(dgram []byte) {
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
	default:
		b.countBad()
	}
}

// relayViewerCount forwards an upstream origin's global viewer count to this
// EDGE hub's local viewers (R18, docs/23 Decision 5c) — a count is
// pod-independent, so unlike ClockMapping there is no per-hop rewrite. On an
// origin hub the "publisher" is a real broadcaster, which has no business
// announcing an audience number: dropped silently (Decision 6 spoof guard).
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

// cacheAndFanViewerCountLocked caches one ViewerCount datagram (copied — the
// caller's buffer may be reused) and fans it out. The fan-out reaches local
// viewers AND edge sessions — which is how edges receive the global total
// they forward down (R18 Decision 3). Caller holds r.mu.
func (b *broadcastHub) cacheAndFanViewerCountLocked(dgram []byte) {
	msg := make([]byte, len(dgram))
	copy(msg, dgram)
	b.cachedViewerCount = msg
	b.fanOutLocked(msg)
}

// relayClockMapping forwards a ClockMapping datagram to all subscribers and
// caches it for late-joiner priming (R5 Q2). The cache holds a copy: the
// mapping outlives the datagram buffer's turn.
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

// IngestKeyframeStream reads one complete keyframe StreamFrame message from a
// publisher-initiated unidirectional stream into a single bounded buffer, then
// caches it and fans it out to subscribers. It reads at most MaxKeyframeBytes;
// an oversize or malformed stream is rejected (the caller resets it) and the
// existing cache is left intact. Runs on its own goroutine per stream.
func (p *Publisher) IngestKeyframeStream(stream io.Reader) error {
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
	r.mu.Lock()
	if p.closed {
		r.mu.Unlock()
		return
	}
	b.cachedKeyframe = msg
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
		subs = append(subs, s)
	}
	r.mu.Unlock()

	for _, s := range subs {
		s.sendKeyframe(msg, seq)
	}
}

// Close releases the publisher slot and schedules the GC grace timer. On a
// publisher that was deposed by TakeOverPublish it is a no-op — the slot
// belongs to the new publisher, and arming the grace timer here is exactly
// the bug that garbage-collected live broadcasts.
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
	b.publisher = nil

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

	// Outside the lock (k8s API I/O): cluster mode stops renewing the Lease
	// and stamps its grace deadline here. ORIGIN hubs only — an edge's
	// upstream pull ending has no business touching the origin's lease.
	if !edge && r.opts.OnPublisherClosed != nil {
		r.opts.OnPublisherClosed(id)
	}
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
	for s := range b.subs {
		s.enqueueLocked(dgram)
	}
}

// Subscriber is one viewer's handle.
type Subscriber struct {
	hub    *broadcastHub
	sender Conn
	queue  chan []byte
	done   chan struct{}

	// statsKey names this subscriber in /statusz subscriberDetails: random
	// per-session, so a slow viewer can be watched across polls without
	// exposing anything identifying or joinable.
	statsKey string
	// internal marks a downstream edge session (R17 W4): exempt from viewer
	// caps and counted as an edge, not an audience member.
	internal bool
	// reliable marks an R19 resilient subscriber (docs/24): drain writes the
	// queued datagrams as records on per-GOP carrier streams and never calls
	// SendDatagram. Fixed at subscribe time — changing mode is a reconnect.
	reliable bool

	// downstreamViewers is the local viewer count this INTERNAL (edge)
	// subscriber last reported up (R18, docs/23 Decision 5b); always 0 for
	// real viewers. Written by the origin's internal-subscribe read loop
	// (which holds no lock), summed by the count pump under the registry
	// lock — hence atomic.
	downstreamViewers atomic.Uint64

	// closed is set (atomically) by Close before it cancels the in-flight
	// keyframe stream; sendKeyframe reads it without the registry lock to avoid
	// opening a doomed stream.
	closed atomic.Bool

	dropped             atomic.Uint64
	sendErrors          atomic.Uint64
	egressDatagramBytes atomic.Uint64
	egressKeyframeBytes atomic.Uint64

	// Keyframe stream fan-out (R8). kfMu guards kfCurrent + kfLastSeq; at most
	// one keyframe stream is ever in flight to a subscriber, and a stale send
	// (lower seq) is skipped so a late prime can't supersede a live keyframe.
	kfMu          sync.Mutex
	kfCurrent     KeyframeStream
	kfLastSeq     uint64
	kfWriters     sync.WaitGroup
	keyframesSent atomic.Uint64
	// Consecutive OpenKeyframeStream failures (guarded by kfMu); reset on any
	// successful open. Crossing KeyframeOpenFailEvictThreshold evicts the
	// subscriber (R10, docs/14); evicting latches so it fires exactly once.
	kfConsecOpenFailed int
	evicting           bool
	// Keyframe drops by cause (R9 M2); KeyframesDropped() sums them.
	kfDroppedSuperseded atomic.Uint64
	kfDroppedSlow       atomic.Uint64
	kfDroppedBandwidth  atomic.Uint64
	kfDroppedOpenFailed atomic.Uint64

	// Reliable carrier fan-out (R19, docs/24 Decisions 4/5). carRotations is
	// bumped when a keyframe is fanned out to this subscriber; the drain
	// goroutine retires its current carrier and opens a fresh one when it
	// sees a new value — per-GOP rotation, with the rotation as the drop
	// point. carMu guards carCurrent, which is otherwise owned by the drain
	// goroutine; Close cancels it under carMu to unblock a stalled record
	// write (CancelWrite is safe concurrently with Write).
	carRotations atomic.Uint64
	carMu        sync.Mutex
	carCurrent   KeyframeStream

	carrierStreams        atomic.Uint64
	carrierRecords        atomic.Uint64
	carrierRecordsDropped atomic.Uint64
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
	select {
	case s.queue <- dgram:
	default:
		s.dropped.Add(1)
	}
}

func (s *Subscriber) drain() {
	defer close(s.done)
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
	}
}

// drainReliable is the R19 drain (docs/24 Decision 5): same bounded queue,
// different sink — every dequeued datagram is written verbatim as a
// length-prefixed record onto the current carrier stream, and SendDatagram is
// never called. The keyframe fan-out rotates the carrier per GOP (a new
// atomic value); a carrier whose write fails or times out is cancelled and
// its GOP's remaining records are dropped until the next rotation —
// drops-over-stalls at GOP granularity.
func (s *Subscriber) drainReliable() {
	defer s.retireCarrier()
	var carSeen uint64
	carDead := false
	var scratch []byte
	for dgram := range s.queue {
		// The egress cap is charged per record as written (prefix + datagram)
		// — reliable delivery must not become a cap bypass. Over-cap records
		// count as bandwidth datagram drops exactly like the datagram path,
		// so queue_full stays derivable by subtraction (R9).
		n := wire.CarrierRecordHeaderSize + len(dgram)
		if !s.hub.registry.consumeBandwidth(n) {
			s.dropped.Add(1)
			s.hub.countBandwidthDrop(n)
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
		if s.currentCarrier() == nil && !s.openCarrier() {
			carDead = true
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
		if !s.writeCarrier(scratch) {
			carDead = true
			s.carrierRecordsDropped.Add(1)
			continue
		}
		s.carrierRecords.Add(1)
		s.egressCarrierBytes.Add(uint64(len(scratch)))
	}
}

func (s *Subscriber) currentCarrier() KeyframeStream {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	return s.carCurrent
}

// openCarrier opens a fresh carrier stream and writes its prologue. Open
// failures feed the same consecutive-failure eviction streak as keyframe
// stream opens (docs/24 Decision 5): a zombie subscriber with exhausted
// stream credit fails both kinds and is evicted with 4001. At most one open
// is attempted per rotation (openCarrier failing marks the GOP dead), so the
// streak grows at GOP cadence, not record cadence.
func (s *Subscriber) openCarrier() bool {
	st, err := s.sender.OpenCarrierStream()
	if err != nil {
		s.kfMu.Lock()
		s.kfConsecOpenFailed++
		evict := !s.evicting && s.kfConsecOpenFailed >= KeyframeOpenFailEvictThreshold
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
	s.kfConsecOpenFailed = 0
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

	if !s.writeCarrier(wire.AppendCarrierPrologue(nil)) {
		return false
	}
	s.egressCarrierBytes.Add(wire.CarrierPrologueSize)
	return true
}

// writeCarrier writes buf to the current carrier under the keyframe write
// deadline. A failed or timed-out write leaves a half-written record on the
// stream — unrecoverable framing for a length-prefixed protocol — so the
// carrier is cancelled; the viewer loses the tail of this GOP and resyncs at
// the keyframe it already reliably has.
func (s *Subscriber) writeCarrier(buf []byte) bool {
	s.carMu.Lock()
	st := s.carCurrent
	s.carMu.Unlock()
	if st == nil {
		return false // cancelled by Close between records
	}
	_ = st.SetWriteDeadline(time.Now().Add(s.hub.registry.opts.KeyframeWriteTimeout))
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
	_ = st.SetWriteDeadline(time.Now().Add(s.hub.registry.opts.KeyframeWriteTimeout))
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
	// counter but not the *datagram* drop counter (R9: the reasons must not
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
		// Per-GOP carrier rotation (R19, docs/24 Decision 4): the keyframe
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
		// Track the failure streak under kfMu; a subscriber that can't accept
		// streams for KeyframeOpenFailEvictThreshold consecutive keyframes is
		// unreachable for good (exhausted stream credit) — evict it. The evict
		// itself runs off-goroutine: Close takes the registry lock and
		// CloseWithError is a network op, neither belongs under kfMu.
		s.kfConsecOpenFailed++
		evict := !s.evicting && s.kfConsecOpenFailed >= KeyframeOpenFailEvictThreshold
		if evict {
			s.evicting = true
		}
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
	s.kfMu.Lock()
	superseded := s.kfCurrent != stream
	if !superseded {
		s.kfCurrent = nil
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
		return
	}
	s.keyframesSent.Add(1)
	s.egressKeyframeBytes.Add(uint64(len(msg)))
}

// evict closes an unreachable subscriber's session and removes it from the
// hub (R10, docs/14). The close code is non-terminal: a live client's
// reconnect gets a fresh session (and fresh stream credit); a zombie simply
// stops costing fan-out work. Fired at most once per subscriber (the
// `evicting` latch) from its own goroutine.
func (s *Subscriber) evict() {
	s.hub.log.Warn("evicting unreachable subscriber: keyframe stream opens failing persistently",
		"subscriber", s.statsKey, "consecutive_open_failures", KeyframeOpenFailEvictThreshold)
	_ = s.sender.CloseWithError(uint32(wire.CloseCodeSubscriberUnresponsive), "subscriber unresponsive")
	s.Close()
}

// Close removes the subscriber and stops its drain loop.
func (s *Subscriber) Close() {
	b := s.hub
	r := b.registry
	r.mu.Lock()
	if s.closed.Load() {
		r.mu.Unlock()
		<-s.done
		return
	}
	s.closed.Store(true)
	delete(b.subs, s)
	close(s.queue)
	r.mu.Unlock()

	// Cancel any in-flight keyframe stream and wait for its writer to finish,
	// outside the registry lock (stream ops must never hold it).
	s.kfMu.Lock()
	if s.kfCurrent != nil {
		s.kfCurrent.CancelWrite()
		s.kfCurrent = nil
	}
	s.kfMu.Unlock()
	s.kfWriters.Wait()

	// Cancel the current carrier (R19) so a drain blocked in a record write
	// unblocks now instead of waiting out its write deadline; the backlog it
	// then consumes is dropped (drain checks closed before opening anew).
	s.carMu.Lock()
	if s.carCurrent != nil {
		s.carCurrent.CancelWrite()
		s.carCurrent = nil
	}
	s.carMu.Unlock()

	<-s.done

	// Fold the final counters only after drain has finished: drain keeps
	// consuming (and bandwidth-dropping) the queued backlog after the queue
	// closes, so folding any earlier loses those drops. The hub may itself
	// have been GC'd by now — credit the registry totals then.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[b.id] == b {
		b.datagramsDropped += s.dropped.Load()
		b.keyframeStreamsSent += s.keyframesSent.Load()
		b.keyframeDrops.add(s.keyframeDrops())
		b.sendErrors += s.sendErrors.Load()
		b.egressDatagramBytes += s.egressDatagramBytes.Load()
		b.egressKeyframeBytes += s.egressKeyframeBytes.Load()
		b.carrierStreams += s.carrierStreams.Load()
		b.carrierRecords += s.carrierRecords.Load()
		b.carrierRecordsDropped += s.carrierRecordsDropped.Load()
		b.egressCarrierBytes += s.egressCarrierBytes.Load()
	} else {
		r.totalDatagramsDropped += s.dropped.Load()
		r.totalKeyframeStreamsSent += s.keyframesSent.Load()
		r.totalKeyframeDrops.add(s.keyframeDrops())
		r.totalSendErrors += s.sendErrors.Load()
		r.totalEgressDatagramBytes += s.egressDatagramBytes.Load()
		r.totalEgressKeyframeBytes += s.egressKeyframeBytes.Load()
		r.totalCarrierStreams += s.carrierStreams.Load()
		r.totalCarrierRecords += s.carrierRecords.Load()
		r.totalCarrierRecordsDropped += s.carrierRecordsDropped.Load()
		r.totalEgressCarrierBytes += s.egressCarrierBytes.Load()
	}
}

// RecordDownstreamViewers stores an edge's reported local viewer count
// (R18): called from the origin's internal-subscribe read loop — the ONLY
// place a client-sent ViewerCount is trusted, because the peer there is a
// PSK-authenticated, generation-fenced edge (docs/23 Decision 6).
func (s *Subscriber) RecordDownstreamViewers(count uint32) {
	s.downstreamViewers.Store(uint64(count))
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
// counters — bandwidthDroppedDatagrams must stay datagram-only (R9) so
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
