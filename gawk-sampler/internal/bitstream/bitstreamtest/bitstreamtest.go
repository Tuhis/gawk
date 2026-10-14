// Package bitstreamtest builds StreamFrame messages from the committed
// fixtures for the sampler's tests: the shapes the three broadcaster surfaces
// actually send (docs/71 D4) — the web broadcaster's AVCC H.264 with an avcC
// DecoderConfig, the desktop and iOS Annex B H.264 with no extradata, and
// VP8/VP9 raw frames.
package bitstreamtest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Fixture reads testdata/bitstreams/<name> from the module root.
func Fixture(testdata, name string) []byte {
	b, err := os.ReadFile(filepath.Join(testdata, "bitstreams", name))
	if err != nil {
		panic(err)
	}
	return b
}

// NALs splits an Annex B stream into NAL units (start codes removed).
func NALs(annexB []byte) [][]byte {
	var out [][]byte
	i := 0
	start := -1
	for i+3 <= len(annexB) {
		if annexB[i] == 0 && annexB[i+1] == 0 && annexB[i+2] == 1 {
			if start >= 0 {
				end := i
				if end > start && annexB[end-1] == 0 {
					end--
				}
				out = append(out, annexB[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(annexB) {
		out = append(out, annexB[start:])
	}
	return out
}

// AVCC converts an Annex B access unit into what WebCodecs emits in `avc`
// format: an avcC record holding its SPS and PPS, and the remaining NAL units
// with 4-byte length prefixes.
func AVCC(annexB []byte) (record, sample []byte) {
	var sps, pps [][]byte
	for _, n := range NALs(annexB) {
		switch n[0] & 0x1f {
		case 7:
			sps = append(sps, n)
		case 8:
			pps = append(pps, n)
		default:
			sample = binary.BigEndian.AppendUint32(sample, uint32(len(n)))
			sample = append(sample, n...)
		}
	}
	if len(sps) == 0 || len(pps) == 0 {
		panic("fixture has no SPS or PPS")
	}
	record = []byte{1, sps[0][1], sps[0][2], sps[0][3], 0xff, 0xe0 | byte(len(sps))}
	for _, s := range sps {
		record = binary.BigEndian.AppendUint16(record, uint16(len(s)))
		record = append(record, s...)
	}
	record = append(record, byte(len(pps)))
	for _, p := range pps {
		record = binary.BigEndian.AppendUint16(record, uint16(len(p)))
		record = append(record, p...)
	}
	return record, sample
}

// StreamFrame builds a keyframe StreamFrame message. An empty codec embeds no
// DecoderConfig (ConfigLen 0).
func StreamFrame(frameID uint32, codec string, extradata, payload []byte) []byte {
	var cfg []byte
	if codec != "" {
		var err error
		cfg, err = wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: codec, Extradata: extradata})
		if err != nil {
			panic(err)
		}
	}
	msg, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe:   true,
		FrameID:    frameID,
		ConfigLen:  uint32(len(cfg)),
		PayloadLen: uint32(len(payload)),
	})
	if err != nil {
		panic(err)
	}
	msg = append(msg, cfg...)
	return append(msg, payload...)
}

// Shape names one of the broadcaster shapes.
type Shape struct {
	Name  string
	Codec string // the relay's X-Gawk-Codec
	Msg   []byte
}

// Shapes builds every broadcaster shape of the quadrants fixture.
func Shapes(testdata string) []Shape {
	h264 := Fixture(testdata, "quadrants.h264")
	record, sample := AVCC(h264)
	return []Shape{
		{"web AVCC H.264", "h264", StreamFrame(1, "avc1.42C00B", record, sample)},
		{"desktop Annex B H.264", "h264", StreamFrame(2, "avc1.42C00B", nil, h264)},
		{"Annex B H.264 without a config", "h264", StreamFrame(3, "", nil, h264)},
		{"VP8", "vp8", StreamFrame(4, "vp8", nil, Fixture(testdata, "quadrants.vp8"))},
		{"VP9", "vp9", StreamFrame(5, "vp09.00.10.08", nil, Fixture(testdata, "quadrants.vp9"))},
	}
}

// Equal reports whether two byte slices match, for messages that would
// otherwise print frame bytes.
func Equal(a, b []byte) error {
	if !bytes.Equal(a, b) {
		return fmt.Errorf("byte slices differ (%d vs %d bytes)", len(a), len(b))
	}
	return nil
}
