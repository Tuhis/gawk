package hub

// The read side of R40's keyframe route (docs/71 D1, §9.2): the one place the
// relay hands a cached frame to anything but a subscriber. It returns the
// cache verbatim and copies nothing — cachedKeyframe is immutable once set
// (onKeyframe swaps the slice, never writes into it) — so serving a sample
// costs the hub one lock round and no allocation.

import "github.com/Tuhis/gawk/gawk-server/internal/broadcastid"

// ScreeningFrame is one broadcast's cached keyframe as the screening route
// serves it.
type ScreeningFrame struct {
	// Msg is the full StreamFrame message — 24-byte header, embedded
	// DecoderConfig, payload — exactly as the publisher sent it. Read-only:
	// it is the hub's cache, shared with every late joiner's prime.
	Msg []byte
	// FrameID is the frame's StreamFrame frame ID.
	FrameID uint32
	// PublisherEpoch is the session the frame belongs to (docs/71 §9.1).
	PublisherEpoch string
	// Codec is the hub's probed codec family (mediaprobe.Codec*), empty when
	// no config has been probed for this session yet.
	Codec string
}

// ScreeningKeyframe returns the broadcast's cached keyframe, or false when
// there is none to screen: no such broadcast, no keyframe cached for the
// current session, or no active publisher. The last case is deliberate
// (docs/71 §9.2): a broadcast in its away grace still caches the frame its
// broadcaster last sent, and a frozen frame nobody is broadcasting must not
// be scored (D2).
func (r *Registry) ScreeningKeyframe(id string) (ScreeningFrame, bool) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return ScreeningFrame{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.hubs[normID]
	if !ok || !b.publisherActive || b.cachedKeyframe == nil {
		return ScreeningFrame{}, false
	}
	return ScreeningFrame{
		Msg:            b.cachedKeyframe,
		FrameID:        b.cachedKeyframeID,
		PublisherEpoch: b.publisherEpoch,
		Codec:          b.codec,
	}, true
}
