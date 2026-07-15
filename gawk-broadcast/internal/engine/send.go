package engine

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The send policy (docs/19 Decision 12). Friends broadcast over home uplinks,
// where saturation is a normal condition rather than an edge case, so every
// failure here has a stated answer:
//
//   - Keyframes go whole, on one reliable uni stream — too big and too
//     important to lose to a single dropped packet (R8).
//   - Deltas are sliced into datagrams: fast and lossy, and losing one costs a
//     frame rather than a GOP.
//   - A mid-frame datagram failure drops the frame's remaining chunks. A
//     partial frame is dead weight the viewer's reassembler discards anyway,
//     so pushing the rest is spending uplink to deliver nothing.
//   - DatagramTooLargeError lowers the chunk size and re-chunks rather than
//     dropping the frame — the Go-side analogue of docs/11's Firefox path-MTU
//     fix. Never assume 1200 is reachable.
//   - At most one keyframe stream is in flight; a newer keyframe cancels the
//     older. The browser fire-and-forgets its keyframe streams with no bound,
//     which is a known-weak spot we do not reproduce in fresh code: with a
//     500 ms GOP a stalled uplink would otherwise accumulate open streams
//     toward stream-credit exhaustion — the publisher-side mirror of R10's
//     zombie-subscriber finding.

// keyframeSupersededCode is the stream error code for a keyframe we abandon
// because a newer one is ready. Application-defined; the relay treats any
// reset stream the same way (it discards the partial keyframe), so the value
// is for our own logs.
const keyframeSupersededCode webtransport.StreamErrorCode = 1

// sender turns access units into wire messages and applies the send policy.
type sender struct {
	relay RelaySession
	clock Clock
	log   *slog.Logger

	// nextFrameID is the shared frameId space for datagrams and streams. It
	// starts at 0 and wraps at uint32 like the browser's nextFrameId — the
	// relay's R9 ingress-loss window depends on that wrap being serial.
	nextFrameID uint32

	// chunkPayload is the current per-datagram payload budget. It starts at
	// the wire maximum and only ever shrinks, in response to a real
	// DatagramTooLargeError from this path.
	chunkPayload int

	// configDatagram is the DecoderConfig, embedded in every keyframe stream
	// so a delivered keyframe is self-sufficient to decode. It is never sent
	// as a standalone datagram: that mirrors the browser broadcaster since R8
	// (broadcaster.ts embeds it in packetizeStreamKeyframe and sends no config
	// datagram at all), and it is what the relay's priming expects — the hub
	// caches the whole StreamFrame message and tracks whether it carried a
	// config, rather than keeping a separate config cache.
	configDatagram []byte
	codec          string

	// inflight is the single keyframe stream allowed to be open.
	kfMu     sync.Mutex
	inflight SendStream
	kfWG     sync.WaitGroup

	mu sync.Mutex
	st Stats
}

func newSender(relay RelaySession, clock Clock, log *slog.Logger) *sender {
	return &sender{
		relay:        relay,
		clock:        clock,
		log:          log,
		chunkPayload: wire.MaxChunkPayload,
	}
}

// send routes one access unit.
func (s *sender) send(au AccessUnit) {
	frameID := s.nextFrameID
	s.nextFrameID++ // wraps at uint32 by construction

	s.mu.Lock()
	s.st.EncodedFrames++
	if au.Keyframe {
		s.st.Keyframes++
	}
	s.mu.Unlock()

	if au.Keyframe {
		s.ensureConfig(au.Data)
		s.sendKeyframe(frameID, au)
		return
	}
	s.sendDelta(frameID, au)
}

// ensureConfig derives the DecoderConfig from the first SPS we see.
//
// The codec string is parsed from the bitstream, never assumed (Decision 8):
// the encoder may pick a different level than we asked for, and on the Annex-B
// path this string is the *only* thing telling the viewer's decoder what it is
// about to get — the viewer's extradata-derived correction only runs for AVCC.
func (s *sender) ensureConfig(au []byte) {
	if s.configDatagram != nil {
		return
	}
	codec, ok := ParseCodecString(au)
	if !ok {
		// No SPS in hand yet. Without a config the viewer cannot decode, but
		// h264parse config-interval=-1 guarantees SPS/PPS in every keyframe
		// AU, so this is a "not yet" rather than a "never".
		return
	}
	// Empty extradata on purpose: this is the Annex-B path (media.go).
	dgram, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: codec})
	if err != nil {
		s.log.Warn("failed to build decoder config", "codec", codec, "err", err)
		return
	}
	s.configDatagram = dgram
	s.codec = codec
	s.log.Info("decoder config derived from SPS", "codec", codec)
}

// sendKeyframe writes one StreamFrame on its own reliable uni stream.
func (s *sender) sendKeyframe(frameID uint32, au AccessUnit) {
	msg, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe:    true,
		FrameID:     frameID,
		TimestampUs: au.TimestampUs,
		ConfigLen:   uint32(len(s.configDatagram)),
		PayloadLen:  uint32(len(au.Data)),
	})
	if err != nil {
		// Over MaxKeyframeBytes: the relay would reject it anyway.
		s.log.Warn("keyframe too large to send", "bytes", len(au.Data), "err", err)
		s.countKeyframeFailed()
		return
	}
	msg = append(msg, s.configDatagram...)
	msg = append(msg, au.Data...)
	if s.configDatagram != nil {
		s.mu.Lock()
		s.st.ConfigsSent++
		s.mu.Unlock()
	}

	str, err := s.relay.OpenUniStream()
	if err != nil {
		s.log.Debug("keyframe stream open failed", "err", err)
		s.countKeyframeFailed()
		return
	}

	// Supersede any keyframe still writing: newest wins, ≤1 in flight.
	s.kfMu.Lock()
	if old := s.inflight; old != nil {
		old.CancelWrite(keyframeSupersededCode)
		s.mu.Lock()
		s.st.KeyframeStreamsSuperseded++
		s.mu.Unlock()
	}
	s.inflight = str
	s.kfMu.Unlock()

	// Write off the frame path: a stalled uplink must not block the next
	// frame's arrival from the child.
	s.kfWG.Add(1)
	go func() {
		defer s.kfWG.Done()
		s.writeKeyframe(str, msg)
	}()
}

func (s *sender) writeKeyframe(str SendStream, msg []byte) {
	_, err := str.Write(msg)
	if err == nil {
		err = str.Close()
	}

	s.kfMu.Lock()
	if s.inflight == str {
		s.inflight = nil
	}
	s.kfMu.Unlock()

	if err != nil {
		// A superseded stream lands here too (CancelWrite fails the Write);
		// it is already counted as superseded, so counting it failed as well
		// would double-count one event.
		s.log.Debug("keyframe write failed", "err", err)
		s.countKeyframeFailed()
		return
	}
	s.mu.Lock()
	s.st.KeyframeStreamsSent++
	s.st.KeyframeBytesSent += uint64(len(msg))
	s.st.BytesSent += uint64(len(msg))
	s.st.SentFrames++
	s.mu.Unlock()
}

func (s *sender) countKeyframeFailed() {
	s.mu.Lock()
	s.st.KeyframeStreamsFailed++
	s.mu.Unlock()
}

// sendDelta chunks a non-keyframe AU into datagrams.
func (s *sender) sendDelta(frameID uint32, au AccessUnit) {
	for attempt := 0; attempt < 2; attempt++ {
		dgrams, err := s.chunk(frameID, au)
		if err != nil {
			s.log.Warn("chunking failed", "err", err)
			s.countFrameDropped()
			return
		}
		sent := 0
		var sendErr error
		for _, d := range dgrams {
			if err := s.relay.SendDatagram(d); err != nil {
				sendErr = err
				break
			}
			sent++
			s.mu.Lock()
			s.st.DatagramsSent++
			s.st.BytesSent += uint64(len(d))
			s.mu.Unlock()
		}
		if sendErr == nil {
			s.mu.Lock()
			s.st.SentFrames++
			s.mu.Unlock()
			return
		}

		// A too-large datagram is not this frame's fault — the path MTU is
		// smaller than we assumed. Shrink and re-chunk once; the new size
		// sticks for every later frame.
		var tooLarge *quic.DatagramTooLargeError
		if errors.As(sendErr, &tooLarge) && s.shrinkChunk(int(tooLarge.MaxDatagramPayloadSize)) {
			s.log.Info("path MTU below assumption, re-chunking",
				"max_datagram_payload", tooLarge.MaxDatagramPayloadSize, "chunk_payload", s.chunkPayload)
			continue
		}

		// Any other failure mid-frame: the rest of this frame is dead weight.
		s.log.Debug("datagram send failed, dropping frame remainder",
			"frame_id", frameID, "sent_chunks", sent, "total_chunks", len(dgrams), "err", sendErr)
		s.countFrameDropped()
		return
	}
	// Re-chunked and still failing: drop the frame rather than loop.
	s.countFrameDropped()
}

// shrinkChunk lowers the chunk budget to fit maxDatagram. Returns false when
// no smaller size is possible or the size would not actually change (which
// would make the retry an infinite loop).
func (s *sender) shrinkChunk(maxDatagram int) bool {
	payload := maxDatagram - wire.VideoChunkHeaderSize
	if payload < 1 || payload >= s.chunkPayload {
		return false
	}
	s.chunkPayload = payload
	return true
}

// chunk splits an AU into VideoChunk datagrams. A zero-length frame still
// produces one chunk so the frame exists on the wire (mirroring packetizer.ts).
func (s *sender) chunk(frameID uint32, au AccessUnit) ([][]byte, error) {
	maxPayload := s.chunkPayload
	count := (len(au.Data) + maxPayload - 1) / maxPayload
	if count < 1 {
		count = 1
	}
	if count > wire.MaxChunkCount {
		return nil, errors.New("frame needs more chunks than wire.MaxChunkCount permits")
	}
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		lo := i * maxPayload
		hi := min(lo+maxPayload, len(au.Data))
		d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
			Keyframe:    false,
			FrameID:     frameID,
			ChunkIndex:  uint16(i),
			ChunkCount:  uint16(count),
			TimestampUs: au.TimestampUs,
		}, au.Data[lo:hi])
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *sender) countFrameDropped() {
	s.mu.Lock()
	s.st.FramesDroppedAtSend++
	s.mu.Unlock()
}

func (s *sender) stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Codec = s.codec
	return st
}

// wait blocks until in-flight keyframe writes finish, so teardown does not
// race them.
func (s *sender) wait() { s.kfWG.Wait() }
