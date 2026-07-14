package hub

// Ingress-loss tracking (R9 M3, docs/13). The relay historically counted only
// what *arrived* from the publisher, so broadcaster→relay loss was invisible —
// indistinguishable from "the broadcaster never sent it". This window closes
// that gap: it watches the frameID sequence across both ingest paths (delta
// datagram chunks and keyframe streams) and counts a frame as lost only when
// it ages out of the window without ever being seen. Waiting for age-out (an
// RTP-receiver-style discipline) is what makes the counter robust to QUIC
// datagram reordering — a naive "gap on arrival" count would tally every
// reorder as a loss and then have to take it back.
//
// Per-frame chunk tracking (distinct chunkIndexes seen vs the header's
// chunkCount) additionally measures partial frame loss: frames that arrived
// but incomplete, which a viewer would drop as unreassemblable.
//
// All methods are called with the registry lock held (from the Publisher
// relay paths); the window has no locking of its own.

// ingressWindowFrames is how many consecutive frameIDs the window tracks
// before an unseen ID is declared lost. 1024 frames ≈ 17–34 s at 30–60 fps —
// far beyond any reordering horizon; a frame arriving later than that would
// be useless for playback anyway. Mirrors the spirit of the client
// reassembler's bounded in-flight window.
const ingressWindowFrames = 1024

// maxIngressChunkWords bounds the per-frame chunk bitmap allocation
// (wire.MaxChunkCount is 1000 → 16 words); a malformed chunkCount beyond it
// is ignored rather than allocated for.
const maxIngressChunkWords = 16

type ingressFrame struct {
	frameID    uint32
	active     bool // slot holds a tracked frameID (zero-value slots are junk)
	seen       bool
	chunkCount int
	chunksSeen int
	chunkBits  []uint64 // lazily allocated, ceil(chunkCount/64) words
}

// ingressWindow is a ring of the last ingressWindowFrames frameIDs, keyed by
// frameID % size. It reports losses as *deltas* from each observe call so the
// cumulative counters can live on the broadcastHub (surviving window resets
// on publisher restart, per the "counters survive their owner" rule).
type ingressWindow struct {
	slots   [ingressWindowFrames]ingressFrame
	started bool
	maxSeen uint32
}

// reset clears all tracking (new publisher session: frameIDs restart, pending
// unknowns from the old session are ambiguous and deliberately not counted).
func (w *ingressWindow) reset() {
	for i := range w.slots {
		w.slots[i] = ingressFrame{}
	}
	w.started = false
	w.maxSeen = 0
}

// observeFrame records a whole frame arriving in one piece (keyframe stream).
func (w *ingressWindow) observeFrame(frameID uint32) (framesLost, chunksLost uint64) {
	return w.observeChunk(frameID, 0, 1)
}

// observeChunk records one chunk of a frame. Returns the losses *finalized*
// by this observation (frames/chunks that just slid out of the window).
func (w *ingressWindow) observeChunk(frameID uint32, chunkIndex, chunkCount int) (framesLost, chunksLost uint64) {
	if !w.started {
		w.started = true
		w.maxSeen = frameID
		slot := &w.slots[frameID%ingressWindowFrames]
		*slot = ingressFrame{frameID: frameID, active: true}
		slot.markChunk(chunkIndex, chunkCount)
		return 0, 0
	}

	// Serial arithmetic: frameIDs are uint32 and may wrap.
	d := int32(frameID - w.maxSeen)
	switch {
	case d > 0:
		if d >= ingressWindowFrames {
			// A jump beyond the whole window: finalize everything tracked and
			// restart at the new position. IDs between the window edge and the
			// jump target were never tracked; counting that unbounded span as
			// lost from a single (possibly corrupt) header would be noise.
			framesLost, chunksLost = w.finalizeAll()
			w.maxSeen = frameID
			slot := &w.slots[frameID%ingressWindowFrames]
			*slot = ingressFrame{frameID: frameID, active: true}
			slot.markChunk(chunkIndex, chunkCount)
			return framesLost, chunksLost
		}
		// Advance: each new ID evicts the slot of (id - window size); an
		// evicted frame that was expected but never seen is a loss.
		for id := w.maxSeen + 1; ; id++ {
			slot := &w.slots[id%ingressWindowFrames]
			fl, cl := slot.finalize()
			framesLost += fl
			chunksLost += cl
			*slot = ingressFrame{frameID: id, active: true}
			if id == frameID {
				break
			}
		}
		w.maxSeen = frameID
	case d <= -ingressWindowFrames:
		// Older than the window: already finalized (and possibly counted
		// lost). Too late to matter for playback; ignore.
		return 0, 0
	}

	slot := &w.slots[frameID%ingressWindowFrames]
	if !slot.active || slot.frameID != frameID {
		// A frame older than the first one seen this session (never advanced
		// through): start tracking it now, without inventing history for the
		// IDs between it and the session start.
		*slot = ingressFrame{frameID: frameID, active: true}
	}
	slot.markChunk(chunkIndex, chunkCount)
	return framesLost, chunksLost
}

// finalizeAll finalizes every tracked slot (window jump or teardown).
func (w *ingressWindow) finalizeAll() (framesLost, chunksLost uint64) {
	for i := range w.slots {
		fl, cl := w.slots[i].finalize()
		framesLost += fl
		chunksLost += cl
		w.slots[i] = ingressFrame{}
	}
	return framesLost, chunksLost
}

// finalize scores a slot leaving the window: an expected-but-unseen frame is
// one lost frame; a seen-but-incomplete frame contributes its missing chunks.
func (f *ingressFrame) finalize() (framesLost, chunksLost uint64) {
	if !f.active {
		return 0, 0
	}
	if !f.seen {
		return 1, 0
	}
	if f.chunkCount > 0 && f.chunksSeen < f.chunkCount {
		return 0, uint64(f.chunkCount - f.chunksSeen)
	}
	return 0, 0
}

func (f *ingressFrame) markChunk(chunkIndex, chunkCount int) {
	f.seen = true
	if chunkCount <= 0 || chunkIndex < 0 || chunkIndex >= chunkCount {
		return
	}
	words := (chunkCount + 63) / 64
	if words > maxIngressChunkWords {
		return
	}
	if f.chunkBits == nil || f.chunkCount != chunkCount {
		// First chunk of this frame (or a malformed mid-frame chunkCount
		// change — retrack rather than index out of bounds).
		f.chunkBits = make([]uint64, words)
		f.chunkCount = chunkCount
		f.chunksSeen = 0
	}
	word, bit := chunkIndex/64, uint(chunkIndex%64)
	if f.chunkBits[word]&(1<<bit) == 0 {
		f.chunkBits[word] |= 1 << bit
		f.chunksSeen++
	}
}
