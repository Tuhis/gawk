package mumble

import (
	"errors"
	"fmt"
)

// The legacy voice packet (Mumble < 1.5, "UDP" format), which murmur 1.5 and
// 1.6 still speak to a client that reports a version below 1.5.0 — the
// reason -mumble-client-version defaults to 1.4.0 (docs/80 §11.4). It rides
// the TCP control channel inside a UDPTunnel message (type 1), whose payload
// is the raw packet, not a protobuf.
//
//	byte 0    type (3 bits) << 5 | target (5 bits)
//	varint    session          server → client only: the speaker
//	varint    sequence         in 10 ms units
//	varint    opus header      payload length (13 bits) | 0x2000 terminator
//	bytes     the Opus packet
//	          (positional audio, three floats, may follow; ignored)

// Legacy voice packet types (the top three bits of the header byte).
const (
	audioTypeCELTAlpha = 0
	audioTypePing      = 1
	audioTypeSpeex     = 2
	audioTypeCELTBeta  = 3
	// AudioTypeOpus is the only codec the bridge carries.
	AudioTypeOpus = 4
)

const (
	opusTerminator = 0x2000
	opusLenMask    = 0x1FFF
	// MaxOpusLen is the largest Opus payload the 13-bit length can say.
	MaxOpusLen = opusLenMask
	// MaxTarget is the largest voice target (5 bits).
	MaxTarget = 31
)

// Errors from the voice packet codec.
var (
	ErrAudioPacket = errors.New("mumble: malformed voice packet")
	// ErrNotOpus is a well-formed packet of another codec (or a legacy ping),
	// which the bridge drops.
	ErrNotOpus = errors.New("mumble: voice packet is not Opus")
)

// AudioPacket is one Opus packet as murmur delivered it, NOT decoded (D6).
type AudioPacket struct {
	// Session is the speaker's Mumble session ID.
	Session uint32
	// Seq is the speaker's sequence number, in 10 ms units.
	Seq int64
	// Target is the voice target: 0 normal talk, 1-30 whisper targets, 31
	// server loopback.
	Target uint8
	// End is the end-of-transmission (terminator) bit.
	End bool
	// Opus is the packet's payload. It may be empty only when End is set.
	Opus []byte
}

// AppendClientAudio encodes a client → server Opus packet.
func AppendClientAudio(dst []byte, target uint8, seq int64, opus []byte, end bool) ([]byte, error) {
	if target > MaxTarget {
		return nil, fmt.Errorf("%w: target %d", ErrAudioPacket, target)
	}
	if len(opus) > MaxOpusLen {
		return nil, fmt.Errorf("%w: %d-byte payload exceeds %d", ErrAudioPacket, len(opus), MaxOpusLen)
	}
	dst = append(dst, AudioTypeOpus<<5|target)
	dst = AppendVarint(dst, seq)
	hdr := int64(len(opus))
	if end {
		hdr |= opusTerminator
	}
	dst = AppendVarint(dst, hdr)
	return append(dst, opus...), nil
}

// AppendServerAudio encodes a server → client Opus packet (what murmur
// sends). The bridge never sends one; tests and the e2e stand-in use it.
func AppendServerAudio(dst []byte, p AudioPacket) ([]byte, error) {
	if p.Target > MaxTarget {
		return nil, fmt.Errorf("%w: target %d", ErrAudioPacket, p.Target)
	}
	if len(p.Opus) > MaxOpusLen {
		return nil, fmt.Errorf("%w: %d-byte payload exceeds %d", ErrAudioPacket, len(p.Opus), MaxOpusLen)
	}
	dst = append(dst, AudioTypeOpus<<5|p.Target)
	dst = AppendVarint(dst, int64(p.Session))
	dst = AppendVarint(dst, p.Seq)
	hdr := int64(len(p.Opus))
	if p.End {
		hdr |= opusTerminator
	}
	dst = AppendVarint(dst, hdr)
	return append(dst, p.Opus...), nil
}

// ParseServerAudio decodes a server → client voice packet. The payload
// aliases b. ErrNotOpus for a well-formed packet of another type.
func ParseServerAudio(b []byte) (AudioPacket, error) {
	return parseAudio(b, true)
}

// ParseClientAudio decodes a client → server voice packet (Session is 0).
func ParseClientAudio(b []byte) (AudioPacket, error) {
	return parseAudio(b, false)
}

func parseAudio(b []byte, fromServer bool) (AudioPacket, error) {
	if len(b) < 1 {
		return AudioPacket{}, fmt.Errorf("%w: empty", ErrAudioPacket)
	}
	typ := b[0] >> 5
	p := AudioPacket{Target: b[0] & MaxTarget}
	if typ != AudioTypeOpus {
		return AudioPacket{}, fmt.Errorf("%w: type %d", ErrNotOpus, typ)
	}
	off := 1
	next := func(what string) (int64, error) {
		v, n, err := ReadVarint(b[off:])
		if err != nil {
			return 0, fmt.Errorf("%w: %s: %v", ErrAudioPacket, what, err)
		}
		off += n
		return v, nil
	}
	if fromServer {
		s, err := next("session")
		if err != nil {
			return AudioPacket{}, err
		}
		if s < 0 || s > 0xFFFFFFFF {
			return AudioPacket{}, fmt.Errorf("%w: session %d", ErrAudioPacket, s)
		}
		p.Session = uint32(s)
	}
	seq, err := next("sequence")
	if err != nil {
		return AudioPacket{}, err
	}
	p.Seq = seq
	hdr, err := next("opus header")
	if err != nil {
		return AudioPacket{}, err
	}
	if hdr < 0 {
		return AudioPacket{}, fmt.Errorf("%w: opus header %d", ErrAudioPacket, hdr)
	}
	p.End = hdr&opusTerminator != 0
	n := int(hdr & opusLenMask)
	if off+n > len(b) {
		return AudioPacket{}, fmt.Errorf("%w: %d-byte payload in %d remaining bytes", ErrAudioPacket, n, len(b)-off)
	}
	p.Opus = b[off : off+n]
	if n == 0 && !p.End {
		return AudioPacket{}, fmt.Errorf("%w: empty payload without terminator", ErrAudioPacket)
	}
	return p, nil
}
