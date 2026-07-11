// Package hub implements the relay's pub/sub core: a single publisher fans
// encoded-video datagrams out to a small set of subscribers.
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

	"github.com/Tuhis/gawk/gawk-server/internal/wire"
)

// Sentinel errors. Check with errors.Is.
var (
	// ErrPublisherActive is returned by StartPublish while another publisher
	// holds the slot.
	ErrPublisherActive = errors.New("hub: a publisher is already active")
	// ErrFull is returned by Subscribe when MaxSubscribers is reached.
	ErrFull = errors.New("hub: subscriber limit reached")
)

// DatagramSender is all the hub needs from a network session.
// *webtransport.Session satisfies it structurally.
type DatagramSender interface {
	SendDatagram(payload []byte) error
}

// Options configures a Hub.
type Options struct {
	// MaxSubscribers caps concurrent subscribers; Subscribe returns ErrFull
	// beyond it. Defaults to 15.
	MaxSubscribers int
	// QueueDepth is the per-subscriber datagram queue capacity. It must
	// comfortably exceed the chunk count of a keyframe (~130 at 1080p) or
	// priming itself will drop. Defaults to 256.
	QueueDepth int
}

// Stats is a point-in-time snapshot of hub state, for logging and the
// GET /statusz endpoint (the json tags are its response shape).
type Stats struct {
	PublisherActive      bool   `json:"publisherActive"`
	Subscribers          int    `json:"subscribers"`
	FramesRelayed        uint64 `json:"framesRelayed"`     // counted at chunk 0 of each frame
	DatagramsRelayed     uint64 `json:"datagramsRelayed"`  // datagrams fanned out (before per-sub drops)
	DatagramsDropped     uint64 `json:"datagramsDropped"`  // enqueue failures summed over all subscribers
	BadDatagrams         uint64 `json:"badDatagrams"`      // unparseable/unknown datagrams dropped
	HasConfig            bool   `json:"hasConfig"`
	CachedKeyframeID     uint32 `json:"cachedKeyframeId"`
	CachedKeyframeChunks int    `json:"cachedKeyframeChunks"`
	CachedKeyframeBytes  int    `json:"cachedKeyframeBytes"`
}

// Hub owns the publisher slot, the subscriber set and the priming caches.
type Hub struct {
	log  *slog.Logger
	opts Options

	mu              sync.Mutex
	publisherActive bool
	subs            map[*Subscriber]struct{}

	// cachedConfig is the latest DecoderConfig datagram, verbatim.
	cachedConfig []byte
	// cachedKeyframe is the last completely-assembled keyframe: its chunk
	// datagrams verbatim, in chunk order.
	cachedKeyframe struct {
		frameID uint32
		chunks  [][]byte
		bytes   int
	}

	framesRelayed    uint64
	datagramsRelayed uint64
	datagramsDropped uint64
	badDatagrams     uint64
}

// New builds a Hub. Zero-valued Options fields get defaults.
func New(log *slog.Logger, opts Options) *Hub {
	if opts.MaxSubscribers <= 0 {
		opts.MaxSubscribers = 15
	}
	if opts.QueueDepth <= 0 {
		opts.QueueDepth = 256
	}
	return &Hub{
		log:  log,
		opts: opts,
		subs: make(map[*Subscriber]struct{}),
	}
}

// StartPublish claims the single publisher slot. The caller must Close the
// returned Publisher when its session ends. The caches persist after Close
// so viewers can still be primed while the broadcaster is away — but a new
// publisher session invalidates them: its frameIDs restart at 0 and its
// config may differ, so datagrams cached from an older session must never
// prime a joiner once a newer session exists.
func (h *Hub) StartPublish() (*Publisher, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.publisherActive {
		return nil, ErrPublisherActive
	}
	h.publisherActive = true
	h.cachedConfig = nil
	h.cachedKeyframe.frameID = 0
	h.cachedKeyframe.chunks = nil
	h.cachedKeyframe.bytes = 0
	return &Publisher{hub: h}, nil
}

// Subscribe registers a new subscriber. Before the subscriber joins the
// live fan-out it is primed, in order, with the cached decoder config and
// the chunks of the last complete keyframe, so a late joiner can render
// without waiting for the next keyframe.
func (h *Hub) Subscribe(sender DatagramSender) (*Subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.opts.MaxSubscribers {
		return nil, ErrFull
	}
	s := &Subscriber{
		hub:    h,
		sender: sender,
		queue:  make(chan []byte, h.opts.QueueDepth),
		done:   make(chan struct{}),
	}
	if h.cachedConfig != nil {
		s.enqueueLocked(h.cachedConfig)
	}
	for _, chunk := range h.cachedKeyframe.chunks {
		s.enqueueLocked(chunk)
	}
	h.subs[s] = struct{}{}
	go s.drain()
	return s, nil
}

// Full reports whether the subscriber limit is currently reached. It is a
// convenience for rejecting sessions before the WebTransport upgrade;
// Subscribe remains the authoritative check.
func (h *Hub) Full() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) >= h.opts.MaxSubscribers
}

// Stats returns a snapshot of hub state.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	dropped := h.datagramsDropped
	for s := range h.subs {
		dropped += s.dropped.Load()
	}
	return Stats{
		PublisherActive:      h.publisherActive,
		Subscribers:          len(h.subs),
		FramesRelayed:        h.framesRelayed,
		DatagramsRelayed:     h.datagramsRelayed,
		DatagramsDropped:     dropped,
		BadDatagrams:         h.badDatagrams,
		HasConfig:            h.cachedConfig != nil,
		CachedKeyframeID:     h.cachedKeyframe.frameID,
		CachedKeyframeChunks: len(h.cachedKeyframe.chunks),
		CachedKeyframeBytes:  h.cachedKeyframe.bytes,
	}
}

// Publisher is the active publisher session's handle into the hub.
type Publisher struct {
	hub *Hub

	// closed is guarded by hub.mu.
	closed bool
	// assembly is the in-progress keyframe reassembly for this session.
	// Guarded by hub.mu. Abandoned when a keyframe with a different frameID
	// starts; the previously cached complete keyframe is unaffected.
	assembly *keyframeAssembly
}

type keyframeAssembly struct {
	frameID  uint32
	chunks   [][]byte // len == chunkCount; nil entries not yet received
	received int
	bytes    int
}

// HandleDatagram observes and relays one publisher datagram. The hub takes
// ownership of dgram: it must not be modified or reused by the caller, as
// the same slice is shared across subscriber queues and the caches.
// Malformed or unknown datagrams are dropped and counted, never forwarded.
func (p *Publisher) HandleDatagram(dgram []byte) {
	ver, typ, err := wire.PeekType(dgram)
	if err != nil || ver != wire.Version {
		p.hub.countBad()
		return
	}
	switch typ {
	case wire.TypeVideoChunk:
		hdr, _, err := wire.ParseVideoChunk(dgram)
		if err != nil {
			p.hub.countBad()
			return
		}
		p.relayVideoChunk(hdr, dgram)
	case wire.TypeDecoderConfig:
		if _, err := wire.ParseDecoderConfig(dgram); err != nil {
			p.hub.countBad()
			return
		}
		p.relayConfig(dgram)
	default:
		p.hub.countBad()
	}
}

// Close releases the publisher slot. The caches persist until the next
// publisher session starts. Idempotent.
func (p *Publisher) Close() {
	h := p.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.assembly = nil
	h.publisherActive = false
}

func (p *Publisher) relayConfig(dgram []byte) {
	h := p.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.closed {
		return
	}
	h.cachedConfig = dgram
	h.fanOutLocked(dgram)
}

func (p *Publisher) relayVideoChunk(hdr wire.VideoChunkHeader, dgram []byte) {
	h := p.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.closed {
		return
	}

	if hdr.Keyframe {
		p.assembleLocked(hdr, dgram)
	}
	if hdr.ChunkIndex == 0 {
		h.framesRelayed++
		// Re-emit the cached config ahead of every keyframe so a viewer
		// that missed the publisher's config datagram can still configure
		// its decoder. Duplicates are idempotent on the viewer side.
		if hdr.Keyframe && h.cachedConfig != nil {
			for s := range h.subs {
				s.enqueueLocked(h.cachedConfig)
			}
		}
	}
	h.fanOutLocked(dgram)
}

// assembleLocked feeds one keyframe chunk into the reassembly buffer and,
// on completion, promotes it to the hub's keyframe cache.
func (p *Publisher) assembleLocked(hdr wire.VideoChunkHeader, dgram []byte) {
	a := p.assembly
	if a == nil || a.frameID != hdr.FrameID {
		// A new keyframe starts; any incomplete assembly is abandoned.
		a = &keyframeAssembly{
			frameID: hdr.FrameID,
			chunks:  make([][]byte, hdr.ChunkCount),
		}
		p.assembly = a
	}
	if int(hdr.ChunkCount) != len(a.chunks) {
		// Chunks of one frame disagree on the count: corrupt. Skip assembly;
		// the datagram is still forwarded.
		p.hub.badDatagrams++
		return
	}
	if a.chunks[hdr.ChunkIndex] != nil {
		return // duplicate
	}
	a.chunks[hdr.ChunkIndex] = dgram
	a.received++
	a.bytes += len(dgram)
	if a.received == len(a.chunks) {
		h := p.hub
		h.cachedKeyframe.frameID = a.frameID
		h.cachedKeyframe.chunks = a.chunks
		h.cachedKeyframe.bytes = a.bytes
		p.assembly = nil
	}
}

func (h *Hub) fanOutLocked(dgram []byte) {
	h.datagramsRelayed++
	for s := range h.subs {
		s.enqueueLocked(dgram)
	}
}

func (h *Hub) countBad() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.badDatagrams++
}

// Subscriber is one viewer's handle into the hub. Datagrams are pushed into
// its bounded queue and sent to the session by a dedicated goroutine.
type Subscriber struct {
	hub    *Hub
	sender DatagramSender
	queue  chan []byte
	done   chan struct{} // closed when the drain goroutine exits

	// closed is guarded by hub.mu.
	closed bool

	dropped    atomic.Uint64
	sendErrors atomic.Uint64
}

// enqueueLocked enqueues without blocking; a full queue drops the datagram.
// Callers must hold hub.mu — that is what makes enqueue-after-Close
// impossible (Close removes the subscriber and closes the queue under the
// same lock).
func (s *Subscriber) enqueueLocked(dgram []byte) {
	select {
	case s.queue <- dgram:
	default:
		s.dropped.Add(1)
	}
}

// drain sends queued datagrams to the session until Close closes the queue.
// Send errors are counted, not fatal: a dying session is detected and
// cleaned up by the transport layer, which then calls Close.
func (s *Subscriber) drain() {
	defer close(s.done)
	for dgram := range s.queue {
		if err := s.sender.SendDatagram(dgram); err != nil {
			s.sendErrors.Add(1)
		}
	}
}

// Close removes the subscriber from the fan-out, stops its drain goroutine
// and waits for it to finish. Idempotent.
func (s *Subscriber) Close() {
	h := s.hub
	h.mu.Lock()
	if s.closed {
		h.mu.Unlock()
		<-s.done
		return
	}
	s.closed = true
	delete(h.subs, s)
	h.datagramsDropped += s.dropped.Load()
	close(s.queue)
	h.mu.Unlock()
	<-s.done
}

// Dropped reports how many datagrams were dropped for this subscriber
// because its queue was full.
func (s *Subscriber) Dropped() uint64 { return s.dropped.Load() }
