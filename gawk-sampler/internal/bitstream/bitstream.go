// Package bitstream turns the relay's cached keyframe — one StreamFrame
// message, verbatim (docs/71 D1) — into a bitstream ffmpeg can read from a
// pipe (D4): AVCC H.264 becomes Annex B with the SPS and PPS from the avcC
// record in front, Annex B H.264 passes through, and VP8/VP9 frames are
// wrapped in a one-frame IVF file.
//
// The input is attacker-influenced: a broadcaster chooses every byte. So every
// length is checked before it is used, and a frame this package cannot make
// sense of is an error for that one sample — never a panic and never a
// guess. What it does accept still goes to a subprocess that may fail on it;
// this package only guarantees the framing.
package bitstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Codec families: the relay's probed vocabulary (X-Gawk-Codec).
const (
	CodecH264 = "h264"
	CodecVP8  = "vp8"
	CodecVP9  = "vp9"
	CodecAV1  = "av1"
)

// Demuxers the converted bitstream is read with: ffmpeg's -f value.
const (
	DemuxH264 = "h264"
	DemuxIVF  = "ivf"
)

// Errors a caller counts by kind.
var (
	// ErrMalformed is a StreamFrame or bitstream whose framing is broken.
	ErrMalformed = errors.New("bitstream: malformed")
	// ErrUnsupported is a codec the minimal ffmpeg does not decode (AV1;
	// nothing sends it today) or that could not be determined.
	ErrUnsupported = errors.New("bitstream: unsupported codec")
)

// Input is one keyframe ready for ffmpeg.
type Input struct {
	// Codec is the family the frame was converted as.
	Codec string
	// Demuxer is ffmpeg's -f for Data.
	Demuxer string
	// Data is the bitstream. It holds frame bytes: it is never logged,
	// written or exposed (D8).
	Data []byte
}

// FromStreamFrame converts a StreamFrame message. codecHint is the relay's
// X-Gawk-Codec; the frame's own embedded DecoderConfig wins when it has one,
// because it is the publisher's statement about these exact bytes.
func FromStreamFrame(msg []byte, codecHint string) (Input, error) {
	hdr, err := wire.ParseStreamFrameHeader(msg)
	if err != nil {
		return Input{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	total := wire.StreamFrameHeaderSize + int(hdr.ConfigLen) + int(hdr.PayloadLen)
	if total != len(msg) {
		return Input{}, fmt.Errorf("%w: header declares %d bytes, message has %d", ErrMalformed, total, len(msg))
	}
	if hdr.PayloadLen == 0 {
		return Input{}, fmt.Errorf("%w: empty payload", ErrMalformed)
	}
	payload := msg[wire.StreamFrameHeaderSize+int(hdr.ConfigLen):]

	codec := strings.ToLower(strings.TrimSpace(codecHint))
	var extradata []byte
	if hdr.ConfigLen > 0 {
		cfg, err := wire.ParseDecoderConfig(msg[wire.StreamFrameHeaderSize : wire.StreamFrameHeaderSize+int(hdr.ConfigLen)])
		if err != nil {
			return Input{}, fmt.Errorf("%w: embedded config: %v", ErrMalformed, err)
		}
		if family := family(cfg.Codec); family != "" {
			codec = family
		}
		extradata = cfg.Extradata
	}

	switch codec {
	case CodecH264:
		if len(extradata) > 0 {
			data, err := AVCCToAnnexB(extradata, payload)
			if err != nil {
				return Input{}, err
			}
			return Input{Codec: codec, Demuxer: DemuxH264, Data: data}, nil
		}
		if !hasStartCode(payload) {
			return Input{}, fmt.Errorf("%w: H.264 without avcC extradata is not Annex B", ErrMalformed)
		}
		return Input{Codec: codec, Demuxer: DemuxH264, Data: payload}, nil
	case CodecVP8, CodecVP9:
		return Input{Codec: codec, Demuxer: DemuxIVF, Data: IVF(codec, payload)}, nil
	case "":
		return Input{}, fmt.Errorf("%w: no codec in the frame or the response", ErrUnsupported)
	default:
		return Input{}, fmt.Errorf("%w: %s", ErrUnsupported, codec)
	}
}

// family maps a WebCodecs codec string onto the relay's family vocabulary,
// "" for one it does not know. The relay's own mapping is internal to its
// module; this is the same four prefixes.
func family(codec string) string {
	switch {
	case strings.HasPrefix(codec, "avc1"), strings.HasPrefix(codec, "avc3"):
		return CodecH264
	case strings.HasPrefix(codec, "vp09"):
		return CodecVP9
	case codec == "vp8":
		return CodecVP8
	case strings.HasPrefix(codec, "av01"):
		return CodecAV1
	}
	return ""
}

var startCode = []byte{0, 0, 0, 1}

func hasStartCode(b []byte) bool {
	return bytes.HasPrefix(b, startCode) || bytes.HasPrefix(b, startCode[1:])
}

// AVCCToAnnexB rebuilds an Annex B access unit from an AVCC one: the SPS and
// PPS of the avcC record (ISO/IEC 14496-15 §5.3.3.1) first, then each
// length-prefixed NAL unit of the sample, every unit behind a 4-byte start
// code. A keyframe from WebCodecs in `avc` format carries no parameter sets
// of its own, so without the record's there is nothing to decode.
func AVCCToAnnexB(avcc, sample []byte) ([]byte, error) {
	if len(avcc) < 7 || avcc[0] != 1 {
		return nil, fmt.Errorf("%w: avcC record too short or version %d", ErrMalformed, firstByte(avcc))
	}
	lengthSize := int(avcc[4]&0x03) + 1
	if lengthSize == 3 {
		return nil, fmt.Errorf("%w: avcC NAL length size 3", ErrMalformed)
	}
	out := make([]byte, 0, len(avcc)+len(sample)+32)
	p := 5
	// SPS count is the low 5 bits of byte 5; the PPS count is one byte.
	for list := 0; list < 2; list++ {
		if p >= len(avcc) {
			return nil, fmt.Errorf("%w: avcC record truncated", ErrMalformed)
		}
		n := int(avcc[p])
		if list == 0 {
			n &= 0x1f
		}
		p++
		for i := 0; i < n; i++ {
			if p+2 > len(avcc) {
				return nil, fmt.Errorf("%w: avcC record truncated", ErrMalformed)
			}
			l := int(binary.BigEndian.Uint16(avcc[p:]))
			p += 2
			if l == 0 || p+l > len(avcc) {
				return nil, fmt.Errorf("%w: avcC parameter set overruns the record", ErrMalformed)
			}
			out = append(out, startCode...)
			out = append(out, avcc[p:p+l]...)
			p += l
		}
		if list == 0 && n == 0 {
			return nil, fmt.Errorf("%w: avcC record has no SPS", ErrMalformed)
		}
	}
	nals := 0
	for q := 0; q < len(sample); {
		if q+lengthSize > len(sample) {
			return nil, fmt.Errorf("%w: NAL length prefix truncated", ErrMalformed)
		}
		var l int
		for i := 0; i < lengthSize; i++ {
			l = l<<8 | int(sample[q+i])
		}
		q += lengthSize
		if l == 0 || l > len(sample)-q {
			return nil, fmt.Errorf("%w: NAL unit of %d bytes overruns the sample", ErrMalformed, l)
		}
		out = append(out, startCode...)
		out = append(out, sample[q:q+l]...)
		q += l
		nals++
	}
	if nals == 0 {
		return nil, fmt.Errorf("%w: sample holds no NAL unit", ErrMalformed)
	}
	return out, nil
}

func firstByte(b []byte) int {
	if len(b) == 0 {
		return -1
	}
	return int(b[0])
}

// IVF wraps one VP8 or VP9 frame in a one-frame IVF file, the container
// ffmpeg's ivf demuxer reads. The header's frame size is advisory — the
// decoder takes the real one from the frame — so VP8's is filled in from its
// keyframe header when it parses and left zero otherwise.
func IVF(codec string, frame []byte) []byte {
	fourcc := "VP80"
	if codec == CodecVP9 {
		fourcc = "VP90"
	}
	var w, h uint16
	if codec == CodecVP8 {
		w, h = vp8Size(frame)
	}
	out := make([]byte, 0, 32+12+len(frame))
	out = append(out, "DKIF"...)
	out = binary.LittleEndian.AppendUint16(out, 0)  // version
	out = binary.LittleEndian.AppendUint16(out, 32) // header length
	out = append(out, fourcc...)
	out = binary.LittleEndian.AppendUint16(out, w)
	out = binary.LittleEndian.AppendUint16(out, h)
	out = binary.LittleEndian.AppendUint32(out, 30) // time base denominator
	out = binary.LittleEndian.AppendUint32(out, 1)  // time base numerator
	out = binary.LittleEndian.AppendUint32(out, 1)  // frame count
	out = binary.LittleEndian.AppendUint32(out, 0)  // unused
	out = binary.LittleEndian.AppendUint32(out, uint32(len(frame)))
	out = binary.LittleEndian.AppendUint64(out, 0) // pts
	return append(out, frame...)
}

// vp8Size reads a VP8 keyframe's dimensions (RFC 6386 §9.1): a 3-byte frame
// tag whose low bit is 0 for a keyframe, the start code 9d 01 2a, then 14-bit
// width and height. Zero when the frame is not a parseable keyframe.
func vp8Size(f []byte) (uint16, uint16) {
	if len(f) < 10 || f[0]&1 != 0 || f[3] != 0x9d || f[4] != 0x01 || f[5] != 0x2a {
		return 0, 0
	}
	return binary.LittleEndian.Uint16(f[6:]) & 0x3fff, binary.LittleEndian.Uint16(f[8:]) & 0x3fff
}
