package engine

import (
	"errors"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// A real 20 ms stereo Opus packet's first bytes: TOC 0xFC is CELT fullband,
// 20 ms, stereo, one frame — exactly what the NA1 capture contains and what
// Decision 3's caps force. The rest is filler; nothing here decodes audio.
func opusPacket(n int) []byte {
	p := make([]byte, n)
	p[0] = 0xfc
	return p
}

func newAudioSender(sess RelaySession) *sender {
	s := newTestSender(sess)
	s.setAudioFormat(DefaultAudioFormat("pipewire-monitor"))
	return s
}

// audioDatagrams splits what the session received into configs and frames.
func audioDatagrams(t *testing.T, sess *fakeSession) (configs, frames [][]byte) {
	t.Helper()
	for _, d := range sess.sentDatagrams() {
		if len(d) < 2 {
			continue
		}
		switch d[1] {
		case wire.TypeAudioConfig:
			configs = append(configs, d)
		case wire.TypeAudioFrame:
			frames = append(frames, d)
		}
	}
	return configs, frames
}

// Audio has no keyframe to embed its config in, so the config has to lead the
// flow: a viewer that receives a frame first has nothing to configure its
// decoder with and throws the packet away.
func TestAudioConfigLeadsTheFirstPacket(t *testing.T) {
	sess := newFakeSession()
	s := newAudioSender(sess)

	s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: 1_000_000})

	sent := sess.sentDatagrams()
	if len(sent) != 2 {
		t.Fatalf("first packet produced %d datagrams, want config + frame", len(sent))
	}
	if sent[0][1] != wire.TypeAudioConfig {
		t.Errorf("first datagram type = %#02x, want AudioConfig 0x08", sent[0][1])
	}
	if sent[1][1] != wire.TypeAudioFrame {
		t.Errorf("second datagram type = %#02x, want AudioFrame 0x07", sent[1][1])
	}

	cfg, err := wire.ParseAudioConfig(sent[0])
	if err != nil {
		t.Fatalf("config does not parse: %v", err)
	}
	if cfg.Codec != AudioCodec || cfg.SampleRate != AudioSampleRate || cfg.Channels != AudioChannels {
		t.Errorf("config = %+v, want 48 kHz stereo opus", cfg)
	}
	// Empty on purpose, exactly as the browser lane sends it: WebCodecs
	// configures plain stereo Opus from the codec string alone.
	if len(cfg.Description) != 0 {
		t.Errorf("config carries a %d-byte description; docs/20 sends none", len(cfg.Description))
	}
}

// The config rides the packet flow at 1 Hz — no separate timer, because 50
// packets per second is already a scheduler (docs/20 Decision 5). Repetition
// is the whole lossy-tolerance story: a viewer that joins mid-broadcast, or
// loses the config datagram, is configured within a second.
func TestAudioConfigRepeatsAtOneHz(t *testing.T) {
	sess := newFakeSession()
	clock := &FakeClock{}
	s := newSender(sess, clock, testLog)
	s.setAudioFormat(DefaultAudioFormat("pipewire-monitor"))

	// A second of packets at the real 20 ms cadence.
	for i := range 50 {
		clock.Us = uint64(i) * 20_000
		s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: clock.Us})
	}
	configs, frames := audioDatagrams(t, sess)
	if len(frames) != 50 {
		t.Fatalf("sent %d frames, want 50", len(frames))
	}
	if len(configs) != 1 {
		t.Fatalf("sent %d configs in the first second, want exactly 1", len(configs))
	}

	// Cross the resend interval: exactly one more.
	clock.Us = AudioConfigResendMs * 1000
	s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: clock.Us})
	if configs, _ = audioDatagrams(t, sess); len(configs) != 2 {
		t.Fatalf("sent %d configs after %d ms, want 2", len(configs), AudioConfigResendMs)
	}
	if st := s.stats(); st.AudioConfigsSent != 2 {
		t.Errorf("AudioConfigsSent = %d, want 2", st.AudioConfigsSent)
	}
}

// One datagram per packet, never chunked: a 320 B packet has no chunking story
// and the wire has no reassembly for one. A packet that somehow exceeds the
// payload bound is dropped and counted, not split — a split one would arrive
// as two undecodable halves.
func TestAudioIsNeverChunked(t *testing.T) {
	sess := newFakeSession()
	s := newAudioSender(sess)

	s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: 1000})
	_, frames := audioDatagrams(t, sess)
	if len(frames) != 1 {
		t.Fatalf("one packet produced %d frames", len(frames))
	}

	// Over MaxAudioPayload: dropped whole.
	s.sendAudio(AudioPacket{Data: opusPacket(4000), TimestampUs: 2000})
	if _, frames = audioDatagrams(t, sess); len(frames) != 1 {
		t.Errorf("an oversize packet produced %d frames, want it dropped whole", len(frames)-1)
	}
	st := s.stats()
	if st.AudioPacketsDropped != 1 {
		t.Errorf("AudioPacketsDropped = %d, want 1", st.AudioPacketsDropped)
	}
	if st.AudioPacketsSent != 1 {
		t.Errorf("AudioPacketsSent = %d, want 1", st.AudioPacketsSent)
	}
}

// Audio's sequence space is its own: independent of video frameIDs, advanced
// with the same wrap-aware rule, and advanced even when a send fails — a
// viewer seeing a gap is seeing the truth, where reusing a number would hide a
// lost packet behind a duplicate.
func TestAudioSeqIsItsOwnSpace(t *testing.T) {
	sess := newFakeSession()
	s := newAudioSender(sess)

	// Interleave video and audio; neither counter may disturb the other.
	s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 1000})
	for i := range 3 {
		s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: uint64(i) * 20_000})
		s.send(AccessUnit{Data: testP, TimestampUs: uint64(i) * 33_000})
	}

	_, frames := audioDatagrams(t, sess)
	if len(frames) != 3 {
		t.Fatalf("sent %d audio frames, want 3", len(frames))
	}
	for i, f := range frames {
		h, _, err := wire.ParseAudioFrame(f)
		if err != nil {
			t.Fatalf("frame %d does not parse: %v", i, err)
		}
		if h.Seq != uint32(i) {
			t.Errorf("frame %d seq = %d, want %d — audio must not share the video frameID space", i, h.Seq, i)
		}
	}

	// A failed send still burns its sequence number.
	sess.sendErr = errors.New("queue full")
	s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: 60_000})
	sess.sendErr = nil
	s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: 80_000})

	_, frames = audioDatagrams(t, sess)
	h, _, err := wire.ParseAudioFrame(frames[len(frames)-1])
	if err != nil {
		t.Fatal(err)
	}
	if h.Seq != 4 {
		t.Errorf("seq after a failed send = %d, want 4 — the gap is the honest signal", h.Seq)
	}
}

// An audio failure never touches a video counter. Audio must not trigger the
// video frame-drop path, must not affect FramesDroppedAtSend, and must not
// shrink the video chunk budget — otherwise a sound-card problem reads as a
// saturated uplink.
func TestAudioFailuresTouchNoVideoCounter(t *testing.T) {
	sess := newFakeSession()
	s := newAudioSender(sess)
	before := s.chunkPayload

	sess.sendErr = errors.New("queue full")
	for range 5 {
		s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: 1000})
	}

	st := s.stats()
	if st.AudioPacketsDropped != 5 {
		t.Errorf("AudioPacketsDropped = %d, want 5", st.AudioPacketsDropped)
	}
	if st.FramesDroppedAtSend != 0 {
		t.Errorf("FramesDroppedAtSend = %d — audio drops leaked into the video funnel", st.FramesDroppedAtSend)
	}
	if st.EncodedFrames != 0 || st.SentFrames != 0 {
		t.Errorf("audio moved the video frame counters: encoded %d, sent %d", st.EncodedFrames, st.SentFrames)
	}
	if s.chunkPayload != before {
		t.Errorf("chunkPayload = %d, was %d — audio must not shrink the video chunk budget", s.chunkPayload, before)
	}
}

// Decision 10: the config is checked against the bitstream, not assumed.
//
// A caps filter that did not do what it was told produces a stream the
// advertised config does not describe — and a viewer configured for stereo
// that receives mono produces a confusing bug report three layers from its
// cause. So the lane stops rather than shipping a config that lies.
func TestBitstreamDisagreementDropsTheLane(t *testing.T) {
	for _, tc := range []struct {
		name string
		toc  byte
	}{
		// Config 31 (CELT fullband 20 ms) with the stereo bit clear: mono,
		// where the config says two channels.
		{"mono where the config says stereo", 31 << 3},
		// Config 30 (CELT fullband 10 ms), stereo: half the frame duration
		// the packet-per-datagram design assumes.
		{"10 ms frames where the config says 20", 30<<3 | 0x04},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := newFakeSession()
			s := newAudioSender(sess)

			pkt := opusPacket(320)
			pkt[0] = tc.toc
			s.sendAudio(AudioPacket{Data: pkt, TimestampUs: 1000})

			if n := len(sess.sentDatagrams()); n != 0 {
				t.Errorf("sent %d datagrams for a bitstream the config does not describe", n)
			}
			if !s.audioFailed() {
				t.Error("the lane did not latch as failed")
			}

			// And it stays down: a later, well-formed packet is not a reason
			// to start trusting the caps filter again.
			s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: 21_000})
			if n := len(sess.sentDatagrams()); n != 0 {
				t.Errorf("the lane resumed after latching, sending %d datagrams", n)
			}
		})
	}
}

// The matching case: a bitstream that agrees is verified once and then left
// alone — the check must not cost a TOC parse per packet.
func TestMatchingBitstreamPassesTheCheckOnce(t *testing.T) {
	sess := newFakeSession()
	s := newAudioSender(sess)

	for i := range 10 {
		s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: uint64(i) * 20_000})
	}
	if s.audioFailed() {
		t.Fatal("a conforming stereo 20 ms stream was rejected")
	}
	if !s.audioChecked {
		t.Error("the bitstream was never checked")
	}
	if _, frames := audioDatagrams(t, sess); len(frames) != 10 {
		t.Errorf("sent %d frames, want 10", len(frames))
	}
}

// A lane with no format never sends: there would be nothing on the wire able
// to describe the packets.
func TestNoFormatMeansNoAudio(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess) // deliberately no setAudioFormat

	s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: 1000})
	if n := len(sess.sentDatagrams()); n != 0 {
		t.Errorf("sent %d datagrams with no audio format configured", n)
	}
}

// Auto-resume (docs/19, #170) reclaims the broadcast on a *fresh* relay
// session, and the relay drops its cached config when the new publisher
// session claims the hub. Video re-primes immediately, because its
// DecoderConfig rides every keyframe stream and a resume forces a keyframe.
//
// Audio has no keyframe. Left to the 1 Hz cadence it would re-prime up to a
// second late, and for that second every audio frame reaching a viewer would
// be undecodable — the relay having nothing to join-prime new viewers with
// either. So a resume resets the cadence: the next packet carries the config,
// which is the same thing the browser lane does when its config changes.
func TestResumeRepromptsTheAudioConfig(t *testing.T) {
	sess := newFakeSession()
	clock := &FakeClock{}
	s := newSender(sess, clock, testLog)
	s.setAudioFormat(DefaultAudioFormat("pipewire-monitor"))

	// A second of steady packets: one config, at the front.
	for i := range 10 {
		clock.Us = uint64(i) * 20_000
		s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: clock.Us})
	}
	if configs, _ := audioDatagrams(t, sess); len(configs) != 1 {
		t.Fatalf("sent %d configs before the resume, want 1", len(configs))
	}

	// The reclaim, well inside the resend interval — so an unreset cadence
	// would send nothing.
	resumed := newFakeSession()
	s.setRelay(resumed)
	clock.Us += 20_000
	s.sendAudio(AudioPacket{Data: opusPacket(320), TimestampUs: clock.Us})

	configs, frames := audioDatagrams(t, resumed)
	if len(configs) != 1 {
		t.Errorf("the first packet after a resume carried %d configs, want 1 — "+
			"the relay dropped its cache when the new publisher session claimed the hub", len(configs))
	}
	if len(frames) != 1 {
		t.Fatalf("sent %d audio frames after the resume, want 1", len(frames))
	}
	// The config leads the frame, for the same reason it does at session start.
	sent := resumed.sentDatagrams()
	if len(sent) < 2 || sent[0][1] != wire.TypeAudioConfig {
		t.Error("the config did not lead the first frame after the resume")
	}

	// And the sequence space carries over: continuity is what tells a viewer
	// this is the same lane, not a new one.
	h, _, err := wire.ParseAudioFrame(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if h.Seq != 10 {
		t.Errorf("audio seq after the resume = %d, want 10 — the lane restarted its numbering", h.Seq)
	}
}
