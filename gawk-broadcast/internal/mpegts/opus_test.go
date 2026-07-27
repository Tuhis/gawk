package mpegts

import (
	"bytes"
	"os"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/fixture"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/opus"
)

// testdata/opus-h264-na1.ts is real mpegtsmux output from the R25 NA1 spike —
// H.264 and Opus in one program, which is exactly the pipeline BuildPipeline
// produces with audio on. See its README for provenance. Hand-rolled bytes
// would only prove this parser agrees with my reading of the spec; these bytes
// prove it agrees with GStreamer.
const (
	na1AudioPackets = 8
	// 90 kHz / 50 packets per second: the 20 ms frame Decision 3 forces.
	na1PTSStep = 1800
)

func loadNA1(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/opus-h264-na1.ts")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// collectBoth runs a stream through the demuxer with both callbacks attached.
func collectBoth(t *testing.T, ts []byte, writeSize int) ([]AU, []AudioPacket, *Demuxer) {
	t.Helper()
	var aus []AU
	var pkts []AudioPacket
	d := NewDemuxer(8<<20, func(au AU) error {
		aus = append(aus, AU{Data: bytes.Clone(au.Data), PTS: au.PTS, HasPTS: au.HasPTS})
		return nil
	})
	d.OnAudioPacket(func(p AudioPacket) {
		pkts = append(pkts, AudioPacket{Data: bytes.Clone(p.Data), PTS: p.PTS, HasPTS: p.HasPTS})
	})
	for off := 0; off < len(ts); off += writeSize {
		end := min(off+writeSize, len(ts))
		if _, err := d.Write(ts[off:end]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return aus, pkts, d
}

// The headline: real muxed bytes in, Opus packets out, on a 20 ms grid.
func TestOpusPacketsFromTheNA1Capture(t *testing.T) {
	aus, pkts, d := collectBoth(t, loadNA1(t), 64*1024)

	if len(pkts) != na1AudioPackets {
		t.Fatalf("recovered %d Opus packets, want %d", len(pkts), na1AudioPackets)
	}
	if len(aus) == 0 {
		t.Fatal("no video access units — audio must not cost the picture")
	}
	if d.AudioParseDrops() != 0 {
		t.Errorf("AudioParseDrops = %d on a healthy capture, want 0", d.AudioParseDrops())
	}

	for i, p := range pkts {
		if len(p.Data) == 0 {
			t.Errorf("packet %d is empty — there is no zero-byte Opus packet", i)
		}
		if !p.HasPTS {
			t.Errorf("packet %d carries no PTS", i)
		}
		// The control header must be gone: what the sender puts on the wire
		// has to start at the TOC, or the viewer's decoder gets container
		// framing instead of audio.
		if p.Data[0] == 0x7f {
			t.Errorf("packet %d still starts with a control header: % x", i, p.Data[:4])
		}
		toc, ok := opus.ParseTOC(p.Data)
		if !ok {
			t.Fatalf("packet %d has no readable TOC: % x", i, p.Data[:min(4, len(p.Data))])
		}
		// Decision 3's caps, as the encoder actually produced them.
		if !toc.Stereo || toc.FrameDurationUs != 20_000 {
			t.Errorf("packet %d TOC = %+v, want stereo 20 ms", i, toc)
		}
		if i > 0 {
			if delta := p.PTS - pkts[i-1].PTS; delta != na1PTSStep {
				t.Errorf("packet %d PTS delta = %d ticks, want %d (20 ms)", i, delta, na1PTSStep)
			}
		}
	}
}

// The regression pin for the whole milestone: video output must be
// byte-identical whether or not anyone is listening to the audio, and a
// video-only stream must produce no audio callbacks at all.
func TestAudioNeverDisturbsVideo(t *testing.T) {
	ts := loadNA1(t)

	withAudio, pkts, _ := collectBoth(t, ts, 64*1024)
	if len(pkts) == 0 {
		t.Fatal("the capture carries audio; the rest of this test is meaningless without it")
	}
	videoOnly := collect(t, ts, 64*1024) // no OnAudioPacket at all

	if len(videoOnly) != len(withAudio) {
		t.Fatalf("video AUs = %d without the audio callback, %d with it", len(videoOnly), len(withAudio))
	}
	for i := range videoOnly {
		if !bytes.Equal(videoOnly[i].Data, withAudio[i].Data) {
			t.Fatalf("video AU %d differs depending on whether audio is being read", i)
		}
		if videoOnly[i].PTS != withAudio[i].PTS {
			t.Fatalf("video AU %d PTS differs depending on whether audio is being read", i)
		}
	}

	// And the other direction: the committed video-only fixture has no audio
	// stream, so an attached callback must simply never fire.
	_, none, d := collectBoth(t, bytes.Clone(fixture.TS), 64*1024)
	if len(none) != 0 {
		t.Errorf("a video-only stream produced %d audio packets", len(none))
	}
	if d.AudioParseDrops() != 0 {
		t.Errorf("a video-only stream counted %d audio drops", d.AudioParseDrops())
	}
}

// The child's stdout is a pipe: TS packets straddle reads, and an audio PES
// spans several packets. Adversarial write sizes must produce identical audio.
func TestAudioSurvivesAdversarialReadSizes(t *testing.T) {
	ts := loadNA1(t)
	_, want, _ := collectBoth(t, ts, 64*1024)

	for _, size := range []int{1, 3, 7, 187, 188, 189, 376, 1000, 1188, 4096} {
		_, got, _ := collectBoth(t, ts, size)
		if len(got) != len(want) {
			t.Fatalf("write size %d: %d Opus packets, want %d", size, len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i].Data, want[i].Data) {
				t.Fatalf("write size %d: packet %d differs (%d vs %d bytes)", size, i, len(got[i].Data), len(want[i].Data))
			}
			if got[i].PTS != want[i].PTS {
				t.Fatalf("write size %d: packet %d PTS %d, want %d", size, i, got[i].PTS, want[i].PTS)
			}
		}
	}
}

// stream_type 0x06 says "private data" and nothing else. Our own capture's
// *video* stream carries an HDMV registration descriptor, so a demuxer that
// keyed on "there is a registration descriptor" rather than on its contents
// would bind the audio PID to the picture.
func TestOpusIsIdentifiedByItsRegistrationDescriptor(t *testing.T) {
	// Both blocks are verbatim from testdata/opus-h264-na1.ts's PMT.
	opusES := []byte{0x05, 0x04, 'O', 'p', 'u', 's', 0x7f, 0x02, 0x80, 0x02}
	hdmvES := []byte{0x05, 0x08, 'H', 'D', 'M', 'V', 0xff, 0x1b, 0x44, 0x3f}

	if !isOpusES(opusES) {
		t.Error("the capture's own Opus ES info was not recognised")
	}
	if isOpusES(hdmvES) {
		t.Error("an HDMV registration descriptor was read as Opus")
	}
	if isOpusES(nil) {
		t.Error("an empty ES info block was read as Opus")
	}
	// A descriptor whose length runs past the block must not be trusted or
	// panic — this is parsing bytes off a pipe.
	if isOpusES([]byte{0x05, 0x40, 'O', 'p'}) {
		t.Error("a descriptor overrunning its ES info block was accepted")
	}
}

// Without a callback the audio PID is never even resolved: a video-only
// consumer (internal/pubsim, the relay integration test) pays nothing.
func TestNoCallbackMeansNoAudioParsing(t *testing.T) {
	d := NewDemuxer(8<<20, func(AU) error { return nil })
	if _, err := d.Write(loadNA1(t)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if d.haveAudio {
		t.Error("the audio PID was bound with no audio callback attached")
	}
	if d.AudioParseDrops() != 0 {
		t.Errorf("AudioParseDrops = %d with no callback attached", d.AudioParseDrops())
	}
}

// The Opus-in-TS mapping permits several access units in one PES, and one
// muxer in the wild does it (ffmpeg batches five). Only the PES carries a
// timestamp, so the packets behind the first take theirs from the TOC.
func TestBatchedPESDerivesTimestampsFromTheTOC(t *testing.T) {
	// Build a batched PES payload out of real packets from the capture, so
	// the control headers, sizes and TOCs are all genuine.
	_, real, _ := collectBoth(t, loadNA1(t), 64*1024)
	if len(real) < 3 {
		t.Fatalf("need at least 3 real packets, have %d", len(real))
	}

	var payload []byte
	for _, p := range real[:3] {
		payload = append(payload, opusControlHeader(len(p.Data))...)
		payload = append(payload, p.Data...)
	}

	var got []AudioPacket
	d := NewDemuxer(8<<20, func(AU) error { return nil })
	d.OnAudioPacket(func(p AudioPacket) {
		got = append(got, AudioPacket{Data: bytes.Clone(p.Data), PTS: p.PTS, HasPTS: p.HasPTS})
	})
	d.inAudio, d.audioPTS, d.audioHasPTS = true, 1_000_000, true
	d.audio = payload
	d.flushAudio()

	if len(got) != 3 {
		t.Fatalf("recovered %d packets from a batched PES, want 3", len(got))
	}
	if d.AudioParseDrops() != 0 {
		t.Errorf("AudioParseDrops = %d on a well-formed batched PES", d.AudioParseDrops())
	}
	for i, p := range got {
		if !bytes.Equal(p.Data, real[i].Data) {
			t.Errorf("packet %d does not round-trip through the batched framing", i)
		}
		// 20 ms per packet, derived — the PES only stamped the first.
		if want := uint64(1_000_000 + i*na1PTSStep); p.PTS != want {
			t.Errorf("packet %d PTS = %d, want %d (derived from the TOC)", i, p.PTS, want)
		}
		if !p.HasPTS {
			t.Errorf("packet %d lost its PTS", i)
		}
	}
}

// opusControlHeader builds the mapping-spec header for a payload of n bytes:
// the 11-bit 0x3FF prefix (ten ones, so the first byte is 0x7F — not 0xFF),
// no flags, then the size as 0xFF continuation bytes.
func opusControlHeader(n int) []byte {
	h := []byte{0x7f, 0xe0}
	for n >= 0xff {
		h = append(h, 0xff)
		n -= 0xff
	}
	return append(h, byte(n))
}

// A malformed control header costs the packets behind it in that PES — every
// one of their positions depends on it — and nothing else. Video keeps
// flowing, the write does not error, and the loss is counted rather than
// silent.
func TestMalformedControlHeaderDropsThePacketNotTheStream(t *testing.T) {
	var got []AudioPacket
	d := NewDemuxer(8<<20, func(AU) error { return nil })
	d.OnAudioPacket(func(p AudioPacket) { got = append(got, p) })

	// A header whose declared size runs past the buffer: the classic
	// truncated-PES shape.
	d.inAudio, d.audioHasPTS = true, true
	d.audio = append([]byte{0x7f, 0xe0, 0xff, 0xff}, make([]byte, 32)...)
	d.flushAudio()
	if len(got) != 0 {
		t.Errorf("emitted %d packets from a truncated PES", len(got))
	}
	if d.AudioParseDrops() != 1 {
		t.Errorf("AudioParseDrops = %d after a truncated PES, want 1", d.AudioParseDrops())
	}

	// A header that does not sync at all.
	d.inAudio = true
	d.audio = []byte{0xff, 0xe0, 0x10, 0x01, 0x02}
	d.flushAudio()
	if len(got) != 0 {
		t.Errorf("emitted %d packets from an unsynced header — 0xFF is the wrong prefix", len(got))
	}
	if d.AudioParseDrops() != 2 {
		t.Errorf("AudioParseDrops = %d after an unsynced header, want 2", d.AudioParseDrops())
	}

	// And an oversize PES is bounded rather than accumulated.
	d.inAudio = true
	d.audio = d.audio[:0]
	d.appendAudio(make([]byte, maxAudioPES+1))
	if len(d.audio) != 0 || d.inAudio {
		t.Error("an oversize audio PES was accumulated instead of dropped")
	}
	if d.AudioParseDrops() != 3 {
		t.Errorf("AudioParseDrops = %d after an oversize PES, want 3", d.AudioParseDrops())
	}
}

// The prefix is the one thing NA1 corrected in the design's own reading of the
// spec, and it is load-bearing: a demuxer syncing on 0xFF finds nothing,
// forever.
func TestControlHeaderPrefixIs7F(t *testing.T) {
	size, hdr, ok := parseOpusControlHeader([]byte{0x7f, 0xe0, 0xff, 0x29, 0xfc})
	if !ok || size != 296 || hdr != 4 {
		t.Errorf("parseOpusControlHeader(real 4-byte header) = (%d, %d, %v), want (296, 4, true)", size, hdr, ok)
	}
	if _, _, ok := parseOpusControlHeader([]byte{0xff, 0xe0, 0x10, 0xfc}); ok {
		t.Error("0xFF was accepted as the prefix: the 11-bit field holds 0x3FF, which is ten ones")
	}

	// Optional fields shift the payload, so each one's presence has to move
	// the header length by exactly its own size. The base here is 3 bytes —
	// prefix, flags, and a single-byte payload size (0x40 terminates, where
	// the capture's ~320-byte packets need 0xFF plus a remainder).
	for _, tc := range []struct {
		name   string
		flags  byte
		extra  []byte
		hdrLen int
	}{
		{"start trim", 0x10, []byte{0x01, 0x38}, 5},
		{"end trim", 0x08, []byte{0x00, 0x40}, 5},
		{"both trims", 0x18, []byte{0x01, 0x38, 0x00, 0x40}, 7},
		{"control extension", 0x04, []byte{0x02, 0xaa, 0xbb}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := append([]byte{0x7f, 0xe0 | tc.flags, 0x40}, tc.extra...)
			size, hdrLen, ok := parseOpusControlHeader(append(h, make([]byte, 0x40)...))
			if !ok {
				t.Fatalf("rejected a header with %s", tc.name)
			}
			if size != 0x40 || hdrLen != tc.hdrLen {
				t.Errorf("= (size %d, hdrLen %d), want (64, %d)", size, hdrLen, tc.hdrLen)
			}
		})
	}
}
