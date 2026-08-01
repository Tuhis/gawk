package engine

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/opus"
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
	// relay is swapped by setRelay when auto-resume reclaims the broadcast on
	// a fresh session (engine.go). Guarded because the swap happens on the
	// supervisor goroutine while the pump is mid-frame.
	relayMu sync.RWMutex
	relay   RelaySession

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

	// parityLevel is how many R29 parity symbols this producer emits per
	// delta frame. It is set only by applyCapabilities — the relay decides,
	// because the relay is what filters symbols per subscriber — and stays 0
	// against a relay predating R29 or configured off. Guarded by mu: the
	// capabilities stream lands on the reader goroutine while the pump is
	// mid-frame.
	parityLevel int

	// configDatagram is the DecoderConfig, embedded in every keyframe stream
	// so a delivered keyframe is self-sufficient to decode. It is never sent
	// as a standalone datagram: that mirrors the browser broadcaster since R8
	// (broadcaster.ts embeds it in packetizeStreamKeyframe and sends no config
	// datagram at all), and it is what the relay's priming expects — the hub
	// caches the whole StreamFrame message and tracks whether it carried a
	// config, rather than keeping a separate config cache.
	configDatagram []byte
	codec          string

	// inflight is the single keyframe stream allowed to be open. closed is set
	// by wait() to stop new writes being spawned behind it; both guard kfWG,
	// whose Add must happen-before Wait (sync.WaitGroup's contract).
	kfMu     sync.Mutex
	inflight SendStream
	closed   bool
	kfWG     sync.WaitGroup

	// The audio lane (R25, docs/28 Decision 9) — the Go mirror of
	// gawk-app/src/media/audio-lane.ts's AudioPacketizer, deliberately so
	// that the two broadcasters stay legible to each other. Written by the
	// audio pump goroutine after setAudioFormat, which runs before it starts;
	// the counters go through mu like the video ones, and the one field a
	// second goroutine touches is called out below.
	audioFormat AudioFormat
	// audioConfigDatagram is the AudioConfig, re-sent at 1 Hz on the packet
	// flow. Unlike video's DecoderConfig it *is* a standalone datagram: audio
	// has no keyframe to embed it in, and repetition is the whole
	// lossy-tolerance story (docs/20 Decision 5).
	audioConfigDatagram []byte
	// nextAudioSeq is audio's own uint32 sequence space, independent of video
	// frameIDs and advanced with the same wrap-aware rule.
	nextAudioSeq      uint32
	audioConfigSentAt uint64 // ms on the engine clock; valid once audioConfigSent
	// audioConfigSent is atomic because it is the one piece of this state a
	// second goroutine touches: setRelay clears it from the resume supervisor
	// while the audio pump is reading it. Everything else here stays
	// pump-only, which is why nothing else needs guarding.
	audioConfigSent atomic.Bool
	audioChecked    bool

	mu sync.Mutex
	st Stats
	// audioErrored latches Decision 10's refusal: a bitstream that disagrees
	// with the config we are advertising is not shipped.
	audioErrored bool
	// lastKeyframeUs and the EMA behind Stats.KeyframeIntervalMs (guarded by
	// mu). Measured on AU arrival stamps — the same clock TimeSync reads.
	lastKeyframeUs uint64
	kfIntervalMs   float64
	kfIntervalOK   bool
}

func newSender(relay RelaySession, clock Clock, log *slog.Logger) *sender {
	return &sender{
		relay:        relay,
		clock:        clock,
		log:          log,
		chunkPayload: wire.MaxChunkPayload,
	}
}

func (s *sender) currentRelay() RelaySession {
	s.relayMu.RLock()
	defer s.relayMu.RUnlock()
	return s.relay
}

// setRelay points the sender at a reclaimed session.
//
// The frameId space, the derived DecoderConfig and the shrunken chunk budget
// all carry over deliberately: continuous frameIds are what tell the relay
// this is a *resume* rather than a restart (docs/22 — there is no server-side
// epoch), the relay dropped its config cache when the new publisher session
// claimed the hub, and a path MTU learned the hard way is a property of the
// uplink, not of the session.
//
// The in-flight keyframe does not carry over: it belongs to the session that
// just died. Cancelling it lets its writer goroutine finish now instead of
// waiting on a dead stream, and clearing the slot stops the next keyframe
// being counted as having superseded it.
//
// Neither does the audio config's 1 Hz cadence (R25). The same dropped cache
// that video re-primes through its next keyframe leaves audio with nothing to
// re-prime through — audio has no keyframe, so on the ordinary cadence the
// lane would be undecodable for up to a second after every resume, and the
// relay would have nothing to join-prime a new viewer with either. Resetting
// here makes the *next* packet carry the config, which is what the browser
// lane does whenever its config changes.
func (s *sender) setRelay(r RelaySession) {
	s.relayMu.Lock()
	s.relay = r
	s.relayMu.Unlock()

	s.audioConfigSent.Store(false)

	s.kfMu.Lock()
	old := s.inflight
	s.inflight = nil
	s.kfMu.Unlock()
	if old != nil {
		old.CancelWrite(keyframeSupersededCode)
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
		if s.lastKeyframeUs != 0 && au.TimestampUs > s.lastKeyframeUs {
			gap := float64(au.TimestampUs-s.lastKeyframeUs) / 1000
			if s.kfIntervalOK {
				// EMA, α=0.3: settles within a few GOPs, still smooths the
				// jitter damage-driven capture puts on individual gaps.
				s.kfIntervalMs = 0.7*s.kfIntervalMs + 0.3*gap
			} else {
				s.kfIntervalMs, s.kfIntervalOK = gap, true
			}
		}
		s.lastKeyframeUs = au.TimestampUs
	}
	s.mu.Unlock()

	if au.EncoderRestarted {
		// A rebuilt capture pipeline is a new SPS lineage on a session the
		// viewer is already decoding (AccessUnit.EncoderRestarted). Drop the
		// cached config so this frame's own SPS derives it: the frameId space
		// and the relay session carry on untouched, but what we *say* about
		// the bitstream has to describe the pipeline now producing it.
		s.configDatagram, s.codec = nil, ""
	}
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

// setAudioFormat prepares the AudioConfig this lane will advertise. Called
// once, before the audio pump starts.
func (s *sender) setAudioFormat(f AudioFormat) {
	dgram, err := wire.AppendAudioConfig(nil, wire.AudioConfig{
		Codec:      f.Codec,
		SampleRate: uint32(f.SampleRate),
		Channels:   uint8(f.Channels),
		// Description stays empty, exactly as the browser lane sends it
		// (docs/20): WebCodecs configures plain stereo Opus from the codec
		// string alone, and an OpusHead here would be the only thing that
		// could make a multistream layout decodable — which is precisely the
		// layout the capture caps exist to prevent.
	})
	if err != nil {
		s.log.Warn("failed to build audio config", "codec", f.Codec, "err", err)
		return
	}
	s.audioFormat = f
	s.audioConfigDatagram = dgram
	s.mu.Lock()
	s.st.AudioCodec = f.Codec
	s.st.AudioSampleRate = f.SampleRate
	s.st.AudioChannels = f.Channels
	s.st.AudioBitrateBps = f.BitrateBps
	s.st.AudioSource = f.Source
	s.mu.Unlock()
}

// sendAudio routes one Opus packet (docs/28 Decision 9).
//
// Three properties distinguish it from the video path, and each is deliberate:
//
//   - **One datagram per packet. Never chunked.** A 320 B packet has no
//     chunking story and the wire has no reassembly for one, so a packet that
//     somehow exceeds MaxAudioPayload is dropped and counted, not split.
//   - **The config is piggybacked at 1 Hz on the packet flow** — no separate
//     timer, because 50 packets per second is already a scheduler.
//   - **A failure here touches no video counter.** Audio never triggers the
//     video frame-drop path, never affects FramesDroppedAtSend, and never
//     shrinks the video chunk budget.
func (s *sender) sendAudio(p AudioPacket) {
	if s.audioConfigDatagram == nil {
		return // no format: nothing on the wire could describe this packet
	}
	if !s.audioChecked {
		s.audioChecked = true
		s.checkAudioBitstream(p.Data)
	}
	if s.audioFailed() {
		return
	}
	if len(p.Data) == 0 || len(p.Data) > wire.MaxAudioPayload {
		s.countAudioDropped()
		return
	}

	// One read of the relay for this packet, through the accessor: auto-resume
	// swaps the session on the supervisor goroutine, and reading the field
	// directly would both race that write and risk splitting one packet's
	// config and frame across two sessions.
	relay := s.currentRelay()
	if relay == nil {
		s.countAudioDropped()
		return
	}

	// The config rides the flow: on the first packet, then at most once per
	// AudioConfigResendMs.
	nowMs := s.clock.NowUs() / 1000
	if !s.audioConfigSent.Load() || nowMs-s.audioConfigSentAt >= AudioConfigResendMs {
		if err := relay.SendDatagram(s.audioConfigDatagram); err != nil {
			s.log.Debug("audio config send failed", "err", err)
		} else {
			s.audioConfigSentAt = nowMs
			s.audioConfigSent.Store(true)
			s.mu.Lock()
			s.st.AudioConfigsSent++
			s.mu.Unlock()
		}
	}

	dgram, err := wire.AppendAudioFrame(nil, wire.AudioFrameHeader{
		Seq:         s.nextAudioSeq,
		TimestampUs: p.TimestampUs,
	}, p.Data)
	// The sequence advances whether or not the send succeeds: a viewer seeing
	// a gap is seeing the truth, where reusing the number would hide a lost
	// packet behind a duplicate.
	s.nextAudioSeq++ // wraps at uint32 by construction
	if err != nil {
		s.log.Debug("audio frame encode failed", "bytes", len(p.Data), "err", err)
		s.countAudioDropped()
		return
	}
	if err := relay.SendDatagram(dgram); err != nil {
		s.log.Debug("audio datagram send failed", "err", err)
		s.countAudioDropped()
		return
	}
	s.mu.Lock()
	s.st.AudioPacketsSent++
	s.st.AudioBytesSent += uint64(len(dgram))
	s.st.BytesSent += uint64(len(dgram))
	s.mu.Unlock()
}

// checkAudioBitstream verifies the first packet against the config we are
// about to advertise (docs/28 Decision 10) — the audio counterpart of parsing
// the codec string out of the SPS rather than assuming it.
//
// A disagreement means the caps filter did not do what we told it to. The
// answer is to stop and say so loudly, not to ship a config that lies about
// the stream: a viewer configured for stereo that receives mono produces a
// confusing bug report three layers away from its cause.
//
// The sample rate is deliberately not checked here, and that is not an
// oversight: Opus always operates at 48 kHz internally, so the bitstream
// cannot disagree about it. Channels and frame duration are what the TOC
// actually states, and they are what the caps filter can get wrong.
func (s *sender) checkAudioBitstream(pkt []byte) {
	toc, ok := opus.ParseTOC(pkt)
	if !ok {
		s.log.Error("audio: first packet has no readable Opus TOC; dropping the audio lane",
			"bytes", len(pkt))
		s.failAudio()
		return
	}
	if toc.Channels() != s.audioFormat.Channels || toc.FrameDurationUs != AudioFrameMs*1000 {
		s.log.Error("audio: the encoder produced a stream the advertised config does not describe; dropping the audio lane",
			"bitstream_channels", toc.Channels(), "config_channels", s.audioFormat.Channels,
			"bitstream_frame_us", toc.FrameDurationUs, "config_frame_us", AudioFrameMs*1000)
		s.failAudio()
		return
	}
	s.log.Info("audio lane verified against the bitstream",
		"codec", s.audioFormat.Codec, "channels", toc.Channels(),
		"frame_ms", toc.FrameDurationUs/1000, "source", s.audioFormat.Source)
}

func (s *sender) failAudio() {
	s.mu.Lock()
	s.audioErrored = true
	s.mu.Unlock()
}

func (s *sender) audioFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioErrored
}

func (s *sender) countAudioDropped() {
	s.mu.Lock()
	s.st.AudioPacketsDropped++
	s.mu.Unlock()
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

	str, err := s.currentRelay().OpenUniStream()
	if err != nil {
		s.log.Debug("keyframe stream open failed", "err", err)
		s.countKeyframeFailed()
		return
	}

	// Supersede any keyframe still writing: newest wins, ≤1 in flight.
	s.kfMu.Lock()
	if s.closed {
		// teardown is waiting for in-flight writes before closing the session.
		// Spawning another here would write to a session about to close, and
		// its Add would race the Wait that is already parked.
		s.kfMu.Unlock()
		str.CancelWrite(keyframeSupersededCode)
		s.countKeyframeFailed()
		return
	}
	if old := s.inflight; old != nil {
		old.CancelWrite(keyframeSupersededCode)
		s.mu.Lock()
		s.st.KeyframeStreamsSuperseded++
		s.mu.Unlock()
	}
	s.inflight = str
	// Under kfMu, so it is ordered against wait()'s close: a positive Add from
	// a zero counter must happen-before Wait, or the two race.
	s.kfWG.Add(1)
	s.kfMu.Unlock()

	// Write off the frame path: a stalled uplink must not block the next
	// frame's arrival from the child.
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
		dgrams, parity, err := s.chunkWithParity(frameID, au)
		if err != nil {
			s.log.Warn("chunking failed", "err", err)
			s.countFrameDropped()
			return
		}
		sent := 0
		var sendErr error
		relay := s.currentRelay()
		for _, d := range dgrams {
			if err := relay.SendDatagram(d); err != nil {
				sendErr = err
				break
			}
			sent++
			s.mu.Lock()
			s.st.DatagramsSent++
			s.st.BytesSent += uint64(len(d))
			s.mu.Unlock()
		}
		// R29: parity trails the data chunks — it is useless before the loss
		// it repairs is known. A parity send failure is NOT a frame failure:
		// the data chunks are already out, and a viewer without parity is
		// exactly a pre-R29 viewer, so it must not trigger the re-chunk or
		// drop path below.
		if sendErr == nil {
			for _, d := range parity {
				if err := relay.SendDatagram(d); err != nil {
					break
				}
				s.mu.Lock()
				s.st.ParityChunksSent++
				s.st.ParityBytesSent += uint64(len(d))
				s.mu.Unlock()
			}
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

// chunkWithParity splits an AU into VideoChunk datagrams and, when the fleet
// has asked for parity, computes up to parityLevel RAID-6 P/Q symbols over the
// chunk PAYLOADS (R29, docs/34) — the same rule packetizer.ts follows, and the
// reason the two producers' bytes are asserted identical in parity_test.go.
//
// Parity covers payloads rather than whole datagrams because payloads are what
// the viewer reassembles. Deltas only: keyframes ride reliable uni streams.
//
// A frame needing more than wire.MaxParityDataChunks chunks degrades to plain
// datagrams instead of erroring — past that bound the Q coefficients wrap and
// the code stops being MDS, and a ~300 KB delta is not worth failing a
// broadcast over.
func (s *sender) chunkWithParity(frameID uint32, au AccessUnit) (dgrams, parity [][]byte, err error) {
	dgrams, err = s.chunk(frameID, au)
	if err != nil {
		return nil, nil, err
	}
	level := s.parityLevelNow()
	if level <= 0 || len(dgrams) > wire.MaxParityDataChunks {
		return dgrams, nil, nil
	}
	payloads := make([][]byte, len(dgrams))
	for i, d := range dgrams {
		_, p, err := wire.ParseVideoChunk(d)
		if err != nil {
			return nil, nil, err
		}
		payloads[i] = p
	}
	symbols, err := wire.ComputeParity(payloads, level)
	if err != nil {
		// Parity is an enhancement: a frame it cannot cover still ships.
		return dgrams, nil, nil
	}
	parity = make([][]byte, 0, len(symbols))
	for i, sym := range symbols {
		d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
			FrameID:     frameID,
			ParityIndex: uint8(i),
			ChunkCount:  uint16(len(dgrams)),
			FrameBytes:  uint32(len(au.Data)),
		}, sym)
		if err != nil {
			return nil, nil, err
		}
		parity = append(parity, d)
	}
	return dgrams, parity, nil
}

// applyCapabilities records what the relay says this fleet supports (R29,
// docs/34 §4.4). A relay predating R29 sends no capabilities message, so
// parityLevel stays 0 and nothing is ever emitted; a relay that has the
// feature but is configured off sends level 0 for the same effect.
//
// The CapParityChunks flag is checked separately from the level because the
// flag is what says the relay FILTERS parity per subscriber. Without it,
// emitting would spray parity chunks at viewers that cannot parse them.
func (s *sender) applyCapabilities(c wire.RelayCapabilities) {
	level := 0
	if c.Flags&wire.CapParityChunks != 0 {
		level = int(c.ParityLevel)
	}
	s.mu.Lock()
	s.parityLevel = level
	s.st.ParityLevel = level
	s.mu.Unlock()
}

func (s *sender) parityLevelNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parityLevel
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
	st.KeyframeIntervalAvailable = s.kfIntervalOK
	st.KeyframeIntervalMs = s.kfIntervalMs
	return st
}

// wait closes the sender to new keyframe writes, then blocks until the
// in-flight ones finish, so teardown does not race them.
//
// Closing first is what makes the wait meaningful: the pump goroutine can
// still be inside send() when teardown starts (cancelling the context does not
// stop it synchronously), and a keyframe spawned after Wait had already
// returned would be writing to a session teardown is about to close — the
// exact thing waiting is for.
func (s *sender) wait() {
	s.kfMu.Lock()
	s.closed = true
	s.kfMu.Unlock()
	s.kfWG.Wait()
}
