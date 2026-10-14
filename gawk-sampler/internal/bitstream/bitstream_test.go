package bitstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream/bitstreamtest"
	"github.com/Tuhis/gawk/gawk-sampler/internal/nativetest"
)

func TestFromStreamFrameShapes(t *testing.T) {
	td := nativetest.Testdata(t)
	h264 := bitstreamtest.Fixture(td, "quadrants.h264")
	for _, s := range bitstreamtest.Shapes(td) {
		t.Run(s.Name, func(t *testing.T) {
			in, err := FromStreamFrame(s.Msg, s.Codec)
			if err != nil {
				t.Fatal(err)
			}
			if in.Codec != s.Codec {
				t.Fatalf("codec %s, want %s", in.Codec, s.Codec)
			}
			switch in.Codec {
			case CodecH264:
				if in.Demuxer != DemuxH264 {
					t.Fatalf("demuxer %s", in.Demuxer)
				}
				// The AVCC round trip lands on the encoder's own NAL units, each
				// behind a start code; the Annex B shapes pass through.
				if got, want := bitstreamtest.NALs(in.Data), bitstreamtest.NALs(h264); len(got) != len(want) {
					t.Fatalf("%d NAL units, fixture has %d", len(got), len(want))
				} else {
					for i := range want {
						if !bytes.Equal(got[i], want[i]) {
							t.Fatalf("NAL %d differs", i)
						}
					}
				}
			default:
				if in.Demuxer != DemuxIVF || !bytes.HasPrefix(in.Data, []byte("DKIF")) {
					t.Fatal("not an IVF file")
				}
			}
		})
	}
}

// The embedded config is the publisher's statement about the bytes and wins
// over the relay's hint; with no config the hint decides.
func TestCodecFromConfigBeatsHint(t *testing.T) {
	td := nativetest.Testdata(t)
	vp8 := bitstreamtest.Fixture(td, "quadrants.vp8")
	in, err := FromStreamFrame(bitstreamtest.StreamFrame(1, "vp8", nil, vp8), "h264")
	if err != nil || in.Codec != CodecVP8 {
		t.Fatalf("codec %q, err %v; want vp8", in.Codec, err)
	}
	in, err = FromStreamFrame(bitstreamtest.StreamFrame(1, "", nil, vp8), "VP8")
	if err != nil || in.Codec != CodecVP8 {
		t.Fatalf("codec %q, err %v; want vp8 from the hint", in.Codec, err)
	}
}

func TestFromStreamFrameRejects(t *testing.T) {
	td := nativetest.Testdata(t)
	h264 := bitstreamtest.Fixture(td, "quadrants.h264")
	record, sample := bitstreamtest.AVCC(h264)
	good := bitstreamtest.StreamFrame(1, "avc1.42C00B", record, sample)
	cases := map[string]struct {
		msg  []byte
		hint string
		want error
	}{
		"empty":               {nil, "h264", ErrMalformed},
		"truncated header":    {good[:10], "h264", ErrMalformed},
		"truncated payload":   {good[:len(good)-1], "h264", ErrMalformed},
		"trailing bytes":      {append(append([]byte(nil), good...), 0), "h264", ErrMalformed},
		"no payload":          {bitstreamtest.StreamFrame(1, "avc1.42C00B", record, nil), "h264", ErrMalformed},
		"av1":                 {bitstreamtest.StreamFrame(1, "av01.0.04M.08", nil, []byte{1, 2, 3}), "av1", ErrUnsupported},
		"no codec anywhere":   {bitstreamtest.StreamFrame(1, "", nil, []byte{1, 2, 3}), "", ErrUnsupported},
		"garbage as annex b":  {bitstreamtest.StreamFrame(1, "", nil, []byte{9, 9, 9, 9}), "h264", ErrMalformed},
		"bad avcC version":    {bitstreamtest.StreamFrame(1, "avc1.42C00B", append([]byte{2}, record[1:]...), sample), "h264", ErrMalformed},
		"avcC truncated":      {bitstreamtest.StreamFrame(1, "avc1.42C00B", record[:9], sample), "h264", ErrMalformed},
		"NAL length overruns": {bitstreamtest.StreamFrame(1, "avc1.42C00B", record, binary.BigEndian.AppendUint32(nil, 1000)), "h264", ErrMalformed},
		"bad embedded config": {badConfig(sample), "h264", ErrMalformed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromStreamFrame(c.msg, c.hint); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// badConfig is a frame whose embedded config block is not a DecoderConfig.
func badConfig(payload []byte) []byte {
	msg := bitstreamtest.StreamFrame(1, "avc1.42C00B", nil, payload)
	msg[24+1] = 0x7f // the config's type byte
	return msg
}

// Every prefix of a valid AVCC frame either converts or is rejected — none
// panics. The input is a broadcaster's to choose.
func TestAVCCToAnnexBNeverPanics(t *testing.T) {
	td := nativetest.Testdata(t)
	record, sample := bitstreamtest.AVCC(bitstreamtest.Fixture(td, "quadrants.h264"))
	for i := 0; i <= len(record); i++ {
		_, _ = AVCCToAnnexB(record[:i], sample)
	}
	for i := 0; i <= len(sample); i++ {
		_, _ = AVCCToAnnexB(record, sample[:i])
	}
	full := bitstreamtest.StreamFrame(1, "avc1.42C00B", record, sample)
	for i := 0; i <= len(full); i++ {
		_, _ = FromStreamFrame(full[:i], "h264")
	}
}

func TestIVFHeader(t *testing.T) {
	td := nativetest.Testdata(t)
	vp8 := bitstreamtest.Fixture(td, "quadrants.vp8")
	f := IVF(CodecVP8, vp8)
	if string(f[8:12]) != "VP80" || binary.LittleEndian.Uint16(f[12:]) != 160 || binary.LittleEndian.Uint16(f[14:]) != 96 {
		t.Fatalf("VP8 IVF header %x", f[:32])
	}
	if binary.LittleEndian.Uint32(f[32:]) != uint32(len(vp8)) || !bytes.Equal(f[44:], vp8) {
		t.Fatal("IVF frame header or payload wrong")
	}
	if f := IVF(CodecVP9, []byte{1}); string(f[8:12]) != "VP90" {
		t.Fatalf("VP9 fourcc %q", f[8:12])
	}
}
