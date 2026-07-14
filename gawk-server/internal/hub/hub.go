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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/wire"
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

// Conn is the connection interface required by subscribers.
// *webtransport.Session is wrapped in an adapter by the transport layer to
// satisfy this.
type Conn interface {
	// SendDatagram delivers one delta datagram (unreliable, may be dropped).
	SendDatagram(payload []byte) error
	// OpenKeyframeStream opens a fresh server-initiated unidirectional stream
	// to this subscriber for one keyframe (reliable).
	OpenKeyframeStream() (KeyframeStream, error)
	CloseWithError(code uint32, reason string) error
}

// KeyframeStream is the minimal write side of a unidirectional stream the hub
// uses to deliver one keyframe. The transport layer's adapter maps it onto a
// webtransport SendStream.
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
}

// Stats is a point-in-time snapshot of hub state, for logging and the
// GET /statusz endpoint (the json tags are its response shape).
type Stats struct {
	PublisherActive           bool   `json:"publisherActive"`
	Subscribers               int    `json:"subscribers"`
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
	KeyframeStreamsDropped    uint64 `json:"keyframeStreamsDropped"`  // superseded/slow/bandwidth/open-fail keyframe drops
	KeyframeStreamsOversize   uint64 `json:"keyframeStreamsOversize"` // publisher streams rejected over MaxKeyframeBytes
	GraceRemainingSeconds     int    `json:"graceRemainingSeconds"`   // 0 while publisher is active
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
	totalKeyframeStreamsDropped    uint64
	totalKeyframeStreamsOversize   uint64

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
	generation      uint64
	graceTimer      *time.Timer
	graceStart      time.Time

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

	framesRelayed             uint64
	datagramsRelayed          uint64
	datagramsDropped          uint64
	badDatagrams              uint64
	bandwidthDroppedDatagrams uint64
	bandwidthDroppedBytes     uint64
	keyframeStreamsIn         uint64
	keyframeBytesIn           uint64
	keyframeStreamsOversize   uint64
	// Folded from subscribers that closed while this hub was still alive
	// (mirrors datagramsDropped); live subscribers are summed on demand.
	keyframeStreamsSent    uint64
	keyframeStreamsDropped uint64
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
	statsKey := make([]byte, 32)
	if _, err := rand.Read(statsKey); err != nil {
		panic("hub: crypto/rand unavailable: " + err.Error())
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
		r.hubs[id] = &broadcastHub{
			registry: r,
			id:       id,
			log:      r.log.With("broadcast_id", id),
			subs:     make(map[*Subscriber]struct{}),
		}
	}

	b, exists := r.hubs[id]
	if !exists {
		return "", nil, ErrNotFound
	}
	if b.publisherActive {
		return "", nil, ErrPublisherActive
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
	b.cachedKeyframe = nil
	b.cachedKeyframeID = 0
	b.cachedKeyframeHasConfig = false

	return id, &Publisher{hub: b}, nil
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
	if len(b.subs) >= r.opts.MaxSubscribers {
		return ErrFull
	}
	if r.opts.MaxTotalSubscribers > 0 {
		totalSubs := 0
		for _, hub := range r.hubs {
			totalSubs += len(hub.subs)
		}
		if totalSubs >= r.opts.MaxTotalSubscribers {
			return ErrTotalSubscribers
		}
	}
	return nil
}

// Subscribe registers a subscriber, re-checking under lock. A newly registered
// subscriber is primed with the cached keyframe (over a stream) after the lock
// is released, so it can show a first picture without waiting for the next
// keyframe.
func (r *Registry) Subscribe(id string, conn Conn) (*Subscriber, error) {
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
	if len(b.subs) >= r.opts.MaxSubscribers {
		r.mu.Unlock()
		return nil, ErrFull
	}
	if r.opts.MaxTotalSubscribers > 0 {
		totalSubs := 0
		for _, hub := range r.hubs {
			totalSubs += len(hub.subs)
		}
		if totalSubs >= r.opts.MaxTotalSubscribers {
			r.mu.Unlock()
			return nil, ErrTotalSubscribers
		}
	}

	s := &Subscriber{
		hub:    b,
		sender: conn,
		queue:  make(chan []byte, r.opts.QueueDepth),
		done:   make(chan struct{}),
	}
	b.subs[s] = struct{}{}
	go s.drain()

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
		totals.Subscribers += len(b.subs)
		totals.FramesRelayed += b.framesRelayed
		totals.DatagramsRelayed += b.datagramsRelayed
		totals.BadDatagrams += b.badDatagrams
		totals.BandwidthDroppedDatagrams += b.bandwidthDroppedDatagrams
		totals.BandwidthDroppedBytes += b.bandwidthDroppedBytes
		totals.KeyframeStreamsIn += b.keyframeStreamsIn
		totals.KeyframeBytesIn += b.keyframeBytesIn
		totals.KeyframeStreamsOversize += b.keyframeStreamsOversize

		// Per-subscriber counters are folded into the hub only when the
		// subscriber closes; sum the live ones here for a current view.
		dropped := b.datagramsDropped
		kfSent := b.keyframeStreamsSent
		kfDropped := b.keyframeStreamsDropped
		for s := range b.subs {
			dropped += s.dropped.Load()
			kfSent += s.keyframesSent.Load()
			kfDropped += s.keyframesDropped.Load()
		}
		totals.DatagramsDropped += dropped
		totals.KeyframeStreamsSent += kfSent
		totals.KeyframeStreamsDropped += kfDropped

		var graceRemaining int
		if !b.publisherActive && !b.graceStart.IsZero() {
			rem := r.opts.BroadcastGrace - time.Since(b.graceStart)
			if rem > 0 {
				graceRemaining = int(rem.Seconds())
			}
		}

		obf := r.ObfuscateID(id)
		broadcasts[obf] = Stats{
			PublisherActive:           b.publisherActive,
			Subscribers:               len(b.subs),
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
			KeyframeStreamsDropped:    kfDropped,
			KeyframeStreamsOversize:   b.keyframeStreamsOversize,
			GraceRemainingSeconds:     graceRemaining,
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
	totals.KeyframeStreamsDropped += r.totalKeyframeStreamsDropped
	totals.KeyframeStreamsOversize += r.totalKeyframeStreamsOversize

	return RegistryStats{
		Totals:     totals,
		Broadcasts: broadcasts,
	}
}

// handleGraceExpiry deletes the hub, shuts down viewers and records metrics.
func (r *Registry) handleGraceExpiry(id string, gen uint64) {
	r.mu.Lock()
	b, exists := r.hubs[id]
	if !exists || b.publisherActive || b.generation != gen {
		r.mu.Unlock()
		return
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
	r.totalKeyframeStreamsDropped += b.keyframeStreamsDropped

	var subs []*Subscriber
	for s := range b.subs {
		// Still-live subscribers' drop counts are NOT folded here: their
		// drain loops may still be dropping. Subscriber.Close (called below)
		// folds each count once drain has finished, crediting the registry
		// totals because the hub is no longer registered.
		subs = append(subs, s)
	}
	r.mu.Unlock()

	r.log.Info("broadcast expired and garbage collected", "broadcast_id", id, "subscribers", len(subs))

	for _, s := range subs {
		_ = s.sender.CloseWithError(uint32(wire.CloseCodeBroadcastEnded), "broadcast ended")
		s.Close()
	}
}

// Publisher is the active publisher session's handle.
type Publisher struct {
	hub    *broadcastHub
	closed bool
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
	default:
		b.countBad()
	}
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
	subs := make([]*Subscriber, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	r.mu.Unlock()

	for _, s := range subs {
		s.sendKeyframe(msg, seq)
	}
}

// Close releases the publisher slot and schedules the GC grace timer.
func (p *Publisher) Close() {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true
	b.publisherActive = false

	if r.opts.BroadcastGrace > 0 {
		gen := b.generation
		id := b.id
		b.graceStart = time.Now()
		b.graceTimer = time.AfterFunc(r.opts.BroadcastGrace, func() {
			r.handleGraceExpiry(id, gen)
		})
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

	// closed is set (atomically) by Close before it cancels the in-flight
	// keyframe stream; sendKeyframe reads it without the registry lock to avoid
	// opening a doomed stream.
	closed atomic.Bool

	dropped    atomic.Uint64
	sendErrors atomic.Uint64

	// Keyframe stream fan-out (R8). kfMu guards kfCurrent + kfLastSeq; at most
	// one keyframe stream is ever in flight to a subscriber, and a stale send
	// (lower seq) is skipped so a late prime can't supersede a live keyframe.
	kfMu             sync.Mutex
	kfCurrent        KeyframeStream
	kfLastSeq        uint64
	kfWriters        sync.WaitGroup
	keyframesSent    atomic.Uint64
	keyframesDropped atomic.Uint64
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
	for dgram := range s.queue {
		if !s.hub.registry.consumeBandwidth(len(dgram)) {
			s.dropped.Add(1)
			s.hub.countBandwidthDrop(len(dgram))
			continue
		}
		if err := s.sender.SendDatagram(dgram); err != nil {
			s.sendErrors.Add(1)
		}
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
	if !s.hub.registry.consumeBandwidth(len(msg)) {
		s.keyframesDropped.Add(1)
		s.hub.countBandwidthDrop(len(msg))
		return
	}

	s.kfMu.Lock()
	if s.closed.Load() || seq < s.kfLastSeq {
		s.kfMu.Unlock()
		s.keyframesDropped.Add(1)
		return
	}
	s.kfLastSeq = seq
	if s.kfCurrent != nil {
		// Supersede the stale in-flight keyframe; its writer goroutine's Write
		// returns an error and accounts the drop.
		s.kfCurrent.CancelWrite()
		s.kfCurrent = nil
	}
	stream, err := s.sender.OpenKeyframeStream()
	if err != nil {
		s.kfMu.Unlock()
		s.keyframesDropped.Add(1)
		return
	}
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

	s.kfMu.Lock()
	if s.kfCurrent == stream {
		s.kfCurrent = nil
	}
	s.kfMu.Unlock()

	if err != nil {
		stream.CancelWrite()
		s.keyframesDropped.Add(1)
		return
	}
	s.keyframesSent.Add(1)
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
		b.keyframeStreamsDropped += s.keyframesDropped.Load()
	} else {
		r.totalDatagramsDropped += s.dropped.Load()
		r.totalKeyframeStreamsSent += s.keyframesSent.Load()
		r.totalKeyframeStreamsDropped += s.keyframesDropped.Load()
	}
}

// Dropped reports dropped datagrams count.
func (s *Subscriber) Dropped() uint64 { return s.dropped.Load() }

// KeyframesSent reports keyframe streams fully delivered to this subscriber.
func (s *Subscriber) KeyframesSent() uint64 { return s.keyframesSent.Load() }

// KeyframesDropped reports keyframe streams dropped for this subscriber
// (superseded, slow, bandwidth-limited, or open failures).
func (s *Subscriber) KeyframesDropped() uint64 { return s.keyframesDropped.Load() }

func (r *Registry) consumeBandwidth(n int) bool {
	if r.limiter == nil {
		return true
	}
	return r.limiter.consume(n)
}

// countBandwidthDrop records one bandwidth-limited drop. It runs on drain and
// keyframe-writer goroutines, which can outlive their broadcast: after
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
