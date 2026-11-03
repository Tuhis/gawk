package media

import (
	"bytes"
	"testing"

	"github.com/pion/rtp/codecs"

	"github.com/Tuhis/gawk/gawk-server/publisher"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

func frameOf(a testmedia.AU, mtu int) Frame {
	return Frame{Timestamp: 9000, Payloads: testmedia.Packetize(a, mtu)}
}

func TestSPSParse(t *testing.T) {
	for _, c := range []testmedia.SPSOptions{
		{Profile: 100, Level: 42, Width: 1920, Height: 1080},
		{Profile: 66, Level: 31, Width: 1280, Height: 720},
		{Profile: 77, Level: 40, Width: 2560, Height: 1440},
		{Profile: 100, Level: 51, Width: 3840, Height: 2160},
	} {
		s, err := ParseSPS(testmedia.SPS(c))
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if s.Width != c.Width || s.Height != c.Height || s.ProfileIDC != c.Profile || s.LevelIDC != c.Level {
			t.Fatalf("%+v: parsed %+v", c, s)
		}
		if !s.Supported() {
			t.Fatalf("%+v unsupported", c)
		}
	}
	if _, err := ParseSPS([]byte{0x67, 100, 0, 42}); err == nil {
		t.Fatal("truncated SPS parsed")
	}
	if s, _ := ParseSPS(testmedia.SPS(testmedia.SPSOptions{Profile: 100, Level: 60})); s.Supported() {
		t.Fatal("level 6.0 must be unsupported (D5: up to 5.2)")
	}
}

// The depacketizer agrees with an independent packetizer (pion's), which
// aggregates SPS+PPS into a STAP-A and fragments with FU-A like OBS's.
func TestDepacketizeMatchesPionPayloader(t *testing.T) {
	sps := testmedia.SPS(testmedia.SPSOptions{})
	for _, au := range []testmedia.AU{testmedia.IDR(sps, 7000), testmedia.P(900, 1), testmedia.P(4000, 2)} {
		var pl codecs.H264Payloader
		payloads := pl.Payload(1200, au.AnnexB())
		nals, err := depacketize(payloads)
		if err != nil {
			t.Fatal(err)
		}
		if got := annexB(nals); !bytes.Equal(got, au.AnnexB()) {
			t.Fatalf("round trip differs: %d vs %d bytes", len(got), len(au.AnnexB()))
		}
	}
}

func TestDepacketizeRefusesMalformed(t *testing.T) {
	sps := testmedia.SPS(testmedia.SPSOptions{})
	fu := testmedia.Packetize(testmedia.IDR(sps, 5000), 1200)
	cases := map[string][][]byte{
		"FU-A without its start": fu[2:],
		"FU-A without its end":   fu[:len(fu)-1],
		"STAP-B (interleaved)":   {{0x79, 0, 0, 0, 1, 0x65}},
		"forbidden bit":          {{0xe5, 0x88}},
		"truncated STAP-A":       {{0x78, 0, 9, 0x67}},
		"NAL type 0":             {{0x00, 0x11}},
	}
	for name, payloads := range cases {
		if _, err := depacketize(payloads); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// WG6: "a B-frame stream is refused with reason".
func TestAssemblerRefusesBFrames(t *testing.T) {
	a := NewAssembler()
	sps := testmedia.SPS(testmedia.SPSOptions{})
	if r := a.Frame(frameOf(testmedia.IDR(sps, 3000), 1200)); r.AU == nil || !r.AU.Keyframe {
		t.Fatalf("IDR: %+v", r)
	}
	if r := a.Frame(frameOf(testmedia.P(800, 1), 1200)); r.AU == nil {
		t.Fatal("P refused")
	}
	r := a.Frame(frameOf(testmedia.B(800), 1200))
	if r.Violation == nil || r.Violation.Kind != ViolationBFrames {
		t.Fatalf("B frame: %+v", r)
	}
}

func TestAssemblerInvalidH264(t *testing.T) {
	a := NewAssembler()
	r := a.Frame(Frame{Payloads: [][]byte{{0x79, 0, 1}}})
	if r.Violation == nil || r.Violation.Kind != ViolationInvalid {
		t.Fatalf("got %+v", r)
	}
	a = NewAssembler()
	bad := testmedia.SPS(testmedia.SPSOptions{Profile: 110}) // High 10
	r = a.Frame(frameOf(testmedia.IDR(bad, 1000), 1200))
	if r.Violation == nil || r.Violation.Kind != ViolationInvalid {
		t.Fatalf("High 10: %+v", r)
	}
}

// SPS/PPS before every IDR: missing ones are inserted from the last seen
// (in-band, a parameter-set-only frame, or sprop).
func TestAssemblerParameterSetsBeforeIDR(t *testing.T) {
	sps := testmedia.SPS(testmedia.SPSOptions{})
	a := NewAssembler()
	for i := 0; i < 2; i++ {
		if r := a.Frame(frameOf(testmedia.IDRNoParams(500), 1200)); !r.Skipped || r.Violation != nil {
			t.Fatalf("IDR %d without any parameter sets: %+v", i, r)
		}
	}
	if r := a.Frame(frameOf(testmedia.IDRNoParams(500), 1200)); r.Violation == nil || r.Violation.Kind != ViolationInvalid {
		t.Fatalf("third IDR without parameter sets must be invalid: %+v", r)
	}

	a = NewAssembler()
	a.SetParameterSets(sps, testmedia.PPS()) // sprop-parameter-sets
	r := a.Frame(frameOf(testmedia.IDRNoParams(500), 1200))
	if r.AU == nil || !r.AU.Keyframe {
		t.Fatalf("IDR with sprop: %+v", r)
	}
	if c, ok := publisher.ParseCodecString(r.AU.Data); !ok || c != "avc1.64002A" {
		t.Fatalf("prepended SPS not found: %q %v", c, ok)
	}

	// A separate parameter-set-only frame, then a bare IDR.
	a = NewAssembler()
	if r := a.Frame(Frame{Payloads: [][]byte{sps, testmedia.PPS()}}); r.AU != nil || r.Violation != nil || r.SPS == nil {
		t.Fatalf("params-only frame: %+v", r)
	}
	if r := a.Frame(frameOf(testmedia.IDRNoParams(500), 1200)); r.AU == nil || !bytes.Contains(r.AU.Data, sps) {
		t.Fatalf("bare IDR after params frame: %+v", r)
	}
}

func TestAssemblerParamsChangedMarksEncoderRestart(t *testing.T) {
	a := NewAssembler()
	a.Frame(frameOf(testmedia.IDR(testmedia.SPS(testmedia.SPSOptions{}), 500), 1200))
	r := a.Frame(frameOf(testmedia.IDR(testmedia.SPS(testmedia.SPSOptions{Width: 1280, Height: 720}), 500), 1200))
	if r.AU == nil || !r.AU.ParamsChanged || r.SPS.Width != 1280 {
		t.Fatalf("got %+v", r)
	}
}

func TestPacketStartsFrame(t *testing.T) {
	sps := testmedia.SPS(testmedia.SPSOptions{})
	idr := testmedia.Packetize(testmedia.IDR(sps, 5000), 1200)
	if !PacketStartsFrame(idr[0]) || PacketStartsFrame(idr[2]) {
		t.Fatal("STAP-A opens, a middle FU-A doesn't")
	}
	if !PacketStartsFrame(idr[1]) {
		t.Fatal("the IDR's FU-A start with first_mb 0 opens a frame (its STAP-A lost)")
	}
	second := testmedia.Slice(false, testmedia.SliceP, 40, 100, 1)
	if PacketStartsFrame(second) {
		t.Fatal("a second slice (first_mb 40) doesn't open a frame")
	}
}

// A padding-only RTP packet (empty payload once pion strips the padding)
// consumes a sequence number but carries no NAL; it must not read as
// malformed H.264.
func TestPaddingOnlyPacketsAreHarmless(t *testing.T) {
	p := NewPipeline(PipelineConfig{Reorder: DefaultReorderConfig(), GOP: DefaultGOPConfig()})
	fs := &frameStream{}
	sps := testmedia.SPS(testmedia.SPSOptions{})
	var aus int
	feed := func(pk Packet) {
		o := p.Video(pk, t0, 0)
		if o.Violation != nil {
			t.Fatalf("violation: %v", o.Violation)
		}
		aus += len(o.AUs)
	}
	for _, pk := range fs.packets(testmedia.IDR(sps, 3000)) {
		feed(pk)
	}
	pad := Packet{Seq: fs.s.Seq, Timestamp: fs.s.TS - 1500}
	fs.s.Seq++
	feed(pad)
	for _, pk := range fs.packets(testmedia.P(800, 1)) {
		feed(pk)
	}
	if aus != 2 {
		t.Fatalf("AUs %d, want 2", aus)
	}
}
