// Package hub implements the relay's pub/sub core: a registry of broadcast
// sessions, where each broadcast has a publisher fanning encoded-video
// datagrams out to a small set of subscribers.
//
// The hub is a byte forwarder. It parses datagram headers only to observe —
// caching the latest decoder config and the last complete keyframe so that
// late joiners can be primed — and forwards the publisher's datagrams
// verbatim. Every subscriber owns a bounded queue drained by its own
// goroutine; when the queue is full the datagram is dropped for that
// subscriber, so a slow peer never blocks the publisher or other viewers.
package hub

import (
	"errors"
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
// *webtransport.Session is wrapped in an adapter by the transport layer to satisfy this.
type Conn interface {
	SendDatagram(payload []byte) error
	CloseWithError(code uint32, reason string) error
}

// Options configures a Registry.
type Options struct {
	// MaxSubscribers caps concurrent subscribers per broadcast; Subscribe returns ErrFull
	// beyond it. Defaults to 15.
	MaxSubscribers int
	// QueueDepth is the per-subscriber datagram queue capacity. It must
	// comfortably exceed the chunk count of a keyframe (~130 at 1080p) or
	// priming itself will drop. Defaults to 256.
	QueueDepth int
	// BroadcastGrace is the amount of time a broadcast ID survives after its
	// publisher disconnects, allowing it to be reclaimed. Defaults to 5 minutes.
	BroadcastGrace time.Duration

	MaxBroadcasts       int
	MaxTotalSubscribers int
	MaxBandwidthBytes   int64
}

// Stats is a point-in-time snapshot of hub state, for logging and the
// GET /statusz endpoint (the json tags are its response shape).
type Stats struct {
	PublisherActive           bool   `json:"publisherActive"`
	Subscribers               int    `json:"subscribers"`
	FramesRelayed             uint64 `json:"framesRelayed"`          // counted at chunk 0 of each frame
	DatagramsRelayed          uint64 `json:"datagramsRelayed"`       // datagrams fanned out (before per-sub drops)
	DatagramsDropped          uint64 `json:"datagramsDropped"`       // enqueue failures summed over all subscribers
	BadDatagrams              uint64 `json:"badDatagrams"`           // unparseable/unknown datagrams dropped
	BandwidthDroppedDatagrams uint64 `json:"bandwidthDroppedDatagrams"`
	BandwidthDroppedBytes     uint64 `json:"bandwidthDroppedBytes"`
	HasConfig                 bool   `json:"hasConfig"`
	CachedKeyframeID          uint32 `json:"cachedKeyframeId"`
	CachedKeyframeChunks      int    `json:"cachedKeyframeChunks"`
	CachedKeyframeBytes   int    `json:"cachedKeyframeBytes"`
	GraceRemainingSeconds     int    `json:"graceRemainingSeconds"`  // 0 while publisher is active
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
}

// RegistryStats is the full response structure of GET /statusz.
type RegistryStats struct {
	Totals     TotalStats            `json:"totals"`
	Broadcasts map[string]Stats      `json:"broadcasts"` // keyed by obfuscated broadcast ID
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

	limiter *bandwidthLimiter
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

	cachedConfig []byte
	cachedKeyframe struct {
		frameID uint32
		chunks  [][]byte
		bytes   int
	}

	framesRelayed             uint64
	datagramsRelayed          uint64
	datagramsDropped          uint64
	badDatagrams              uint64
	bandwidthDroppedDatagrams uint64
	bandwidthDroppedBytes     uint64
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
	var limiter *bandwidthLimiter
	if opts.MaxBandwidthBytes > 0 {
		limiter = newBandwidthLimiter(float64(opts.MaxBandwidthBytes))
	}
	return &Registry{
		log:     log,
		opts:    opts,
		hubs:    make(map[string]*broadcastHub),
		limiter: limiter,
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

	// Reset caches on new publisher session
	b.cachedConfig = nil
	b.cachedKeyframe.frameID = 0
	b.cachedKeyframe.chunks = nil
	b.cachedKeyframe.bytes = 0

	return id, &Publisher{hub: b}, nil
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

// Subscribe registers a subscriberAuthoritative, re-checking under lock.
func (r *Registry) Subscribe(id string, conn Conn) (*Subscriber, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return nil, ErrNotFound
	}
	b, exists := r.hubs[normID]
	if !exists {
		return nil, ErrNotFound
	}
	if len(b.subs) >= r.opts.MaxSubscribers {
		return nil, ErrFull
	}
	if r.opts.MaxTotalSubscribers > 0 {
		totalSubs := 0
		for _, hub := range r.hubs {
			totalSubs += len(hub.subs)
		}
		if totalSubs >= r.opts.MaxTotalSubscribers {
			return nil, ErrTotalSubscribers
		}
	}

	s := &Subscriber{
		hub:    b,
		sender: conn,
		queue:  make(chan []byte, r.opts.QueueDepth),
		done:   make(chan struct{}),
	}

	if b.cachedConfig != nil {
		s.enqueueLocked(b.cachedConfig)
	}
	for _, chunk := range b.cachedKeyframe.chunks {
		s.enqueueLocked(chunk)
	}
	b.subs[s] = struct{}{}
	go s.drain()

	return s, nil
}

// Stats returns a point-in-time snapshot of registry state.
func (r *Registry) Stats() RegistryStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	broadcasts := make(map[string]Stats)
	var activeSubscribersCount int
	var activeFramesRelayed uint64
	var activeDatagramsRelayed uint64
	var activeDatagramsDropped uint64
	var activeBadDatagrams uint64
	var activeBandwidthDroppedDatagrams uint64
	var activeBandwidthDroppedBytes uint64

	for id, b := range r.hubs {
		activeSubscribersCount += len(b.subs)
		activeFramesRelayed += b.framesRelayed
		activeDatagramsRelayed += b.datagramsRelayed
		activeBadDatagrams += b.badDatagrams
		activeBandwidthDroppedDatagrams += b.bandwidthDroppedDatagrams
		activeBandwidthDroppedBytes += b.bandwidthDroppedBytes

		dropped := b.datagramsDropped
		for s := range b.subs {
			dropped += s.dropped.Load()
		}
		activeDatagramsDropped += dropped

		var graceRemaining int
		if !b.publisherActive && !b.graceStart.IsZero() {
			rem := r.opts.BroadcastGrace - time.Since(b.graceStart)
			if rem > 0 {
				graceRemaining = int(rem.Seconds())
			}
		}

		obf := broadcastid.Obfuscate(id)
		broadcasts[obf] = Stats{
			PublisherActive:           b.publisherActive,
			Subscribers:               len(b.subs),
			FramesRelayed:             b.framesRelayed,
			DatagramsRelayed:          b.datagramsRelayed,
			DatagramsDropped:          dropped,
			BadDatagrams:              b.badDatagrams,
			BandwidthDroppedDatagrams: b.bandwidthDroppedDatagrams,
			BandwidthDroppedBytes:     b.bandwidthDroppedBytes,
			HasConfig:                 b.cachedConfig != nil,
			CachedKeyframeID:          b.cachedKeyframe.frameID,
			CachedKeyframeChunks:      len(b.cachedKeyframe.chunks),
			CachedKeyframeBytes:       b.cachedKeyframe.bytes,
			GraceRemainingSeconds:     graceRemaining,
		}
	}

	totals := TotalStats{
		Broadcasts:                len(r.hubs),
		Subscribers:               activeSubscribersCount,
		FramesRelayed:             r.totalFramesRelayed + activeFramesRelayed,
		DatagramsRelayed:          r.totalDatagramsRelayed + activeDatagramsRelayed,
		DatagramsDropped:          r.totalDatagramsDropped + activeDatagramsDropped,
		BadDatagrams:              r.totalBadDatagrams + activeBadDatagrams,
		BandwidthDroppedDatagrams: r.totalBandwidthDroppedDatagrams + activeBandwidthDroppedDatagrams,
		BandwidthDroppedBytes:     r.totalBandwidthDroppedBytes + activeBandwidthDroppedBytes,
	}

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

	var subs []*Subscriber
	for s := range b.subs {
		// Fold still-live subscribers' drops into the totals here: their
		// Close below runs against the already-deleted hub, so this is the
		// last chance to count them.
		r.totalDatagramsDropped += s.dropped.Load()
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
	// assembly is the keyframe reassembly buffer.
	assembly *keyframeAssembly
}

type keyframeAssembly struct {
	frameID  uint32
	chunks   [][]byte
	received int
	bytes    int
}

// HandleDatagram processes and relays one publisher datagram.
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
		p.relayConfig(dgram)
	default:
		b.countBad()
	}
}

// Close releases the publisher slot and schedules GC grace timer.
func (p *Publisher) Close() {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true
	p.assembly = nil
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

func (p *Publisher) relayConfig(dgram []byte) {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return
	}
	b.cachedConfig = dgram
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

	if hdr.Keyframe {
		p.assembleLocked(hdr, dgram)
	}
	if hdr.ChunkIndex == 0 {
		b.framesRelayed++
		if hdr.Keyframe && b.cachedConfig != nil {
			for s := range b.subs {
				s.enqueueLocked(b.cachedConfig)
			}
		}
	}
	b.fanOutLocked(dgram)
}

func (p *Publisher) assembleLocked(hdr wire.VideoChunkHeader, dgram []byte) {
	b := p.hub
	a := p.assembly
	if a == nil || a.frameID != hdr.FrameID {
		a = &keyframeAssembly{
			frameID: hdr.FrameID,
			chunks:  make([][]byte, hdr.ChunkCount),
		}
		p.assembly = a
	}
	if int(hdr.ChunkCount) != len(a.chunks) {
		b.badDatagrams++
		return
	}
	if a.chunks[hdr.ChunkIndex] != nil {
		return
	}
	a.chunks[hdr.ChunkIndex] = dgram
	a.received++
	a.bytes += len(dgram)
	if a.received == len(a.chunks) {
		b.cachedKeyframe.frameID = a.frameID
		b.cachedKeyframe.chunks = a.chunks
		b.cachedKeyframe.bytes = a.bytes
		p.assembly = nil
	}
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

	closed bool

	dropped    atomic.Uint64
	sendErrors atomic.Uint64
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

// Close removes the subscriber and stops its drain loop.
func (s *Subscriber) Close() {
	b := s.hub
	r := b.registry
	r.mu.Lock()
	if s.closed {
		r.mu.Unlock()
		<-s.done
		return
	}
	s.closed = true
	delete(b.subs, s)
	b.datagramsDropped += s.dropped.Load()
	close(s.queue)
	r.mu.Unlock()
	<-s.done
}

// Dropped reports dropped datagrams count.
func (s *Subscriber) Dropped() uint64 { return s.dropped.Load() }

func (r *Registry) consumeBandwidth(n int) bool {
	if r.limiter == nil {
		return true
	}
	return r.limiter.consume(n)
}

func (b *broadcastHub) countBandwidthDrop(n int) {
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	b.bandwidthDroppedDatagrams++
	b.bandwidthDroppedBytes += uint64(n)
}
