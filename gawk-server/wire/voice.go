package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Room voice through the Mumble bridge (R79, docs/80 D20). Four data-plane
// messages travel on a browser's voice session with gawk-mumble-bridge
// (CONNECT /voice on the bridge's public listener), never on a relay
// session:
//
//   - VoiceHello, VoiceState and VoiceControl are RECORDS on the one
//     bidirectional stream the browser opens (the room record framing:
//     uint16 length ‖ Version ‖ Type ‖ payload, AppendRoomRecord /
//     ParseRoomRecordLength).
//   - VoiceFrame is a DATAGRAM in both directions: one Opus packet each,
//     like AudioFrame. Voice has no keyframes and no reassembly, and a lost
//     10 ms frame is better than a late one.
//
// The bridge closes a voice session with 4015 or 4016 (below) and sends the
// code in-band first (SessionClosing, docs/59), as the relay does for the
// codes a client acts on. 4002 keeps its relay meaning on the bridge: the
// pod drains, rejoin at once with a fresh ticket.
//
// Layouts, after the common Version ‖ Type prefix:
//
//	0x1A VoiceHello (browser → bridge, the first record on the stream):
//	  byte 2   uint8 protocol      VoiceProtocolVersion
//	  byte 3   uint8 flags         VoiceHelloFlag*; bits 3-7 reserved (0)
//	  4-5      uint16 ticketLen    1..MaxVoiceTicketLen
//	  6+       ticket              opaque here (gawk-server/voice parses it)
//
//	0x1B VoiceFrame (datagram, both directions; VoiceFrameHeaderSize bytes
//	of header, then the payload):
//	  byte 2   uint8 flags         VoiceFrameFlag*; bits 1-7 reserved (0)
//	  byte 3   uint8 target        Mumble voice target 0..MaxVoiceTarget:
//	                               0 normal talk; the bridge passes on what
//	                               murmur reported (1 whisper to a channel,
//	                               2 direct whisper, 31 loopback). A browser
//	                               sends 0.
//	  4-7      uint32 speaker      the speaker's Mumble session ID; 0 from a
//	                               browser (the bridge knows who it is)
//	  8-11     uint32 seq          Mumble sequence number, in 10 ms units
//	                               (a 10 ms frame advances it by 1, a 20 ms
//	                               frame by 2), truncated to 32 bits
//	  12+      payload             one Opus packet, 1..MaxVoicePayload bytes;
//	                               empty ONLY with the end-of-transmission
//	                               flag (a terminator after the last frame)
//
//	0x1C VoiceState (bridge → browser record, whenever it changes):
//	  byte 2   uint8 flags         VoiceStateFlag*; bits 6-7 reserved (0)
//	  3-6      uint32 session      the browser's own Mumble session ID, 0
//	                               until its Mumble connection is up
//	  7        uint8 nameLen       0..MaxVoiceNameLen, then the browser's
//	                               Mumble name (UTF-8), "Alice [web]"
//
//	0x1D VoiceControl (browser → bridge record):
//	  byte 2   uint8 flags         VoiceControlFlag*; bits 2-7 reserved (0)
const (
	// TypeVoiceHello identifies a VoiceHello record. Allocated 2026-10-26
	// after LocalRelayOffer (0x19), as docs/80 D20 asked.
	TypeVoiceHello = 0x1A
	// TypeVoiceFrame identifies a VoiceFrame datagram.
	TypeVoiceFrame = 0x1B
	// TypeVoiceState identifies a VoiceState record.
	TypeVoiceState = 0x1C
	// TypeVoiceControl identifies a VoiceControl record.
	TypeVoiceControl = 0x1D
)

// CloseCodeVoiceRemoved (4015) closes a browser's voice session because the
// person was removed from voice (docs/80 D17): a gawk kill or ban, the room
// ending or removing them, a Mumble kick or ban of their bridged user, or a
// newer voice session of the same participant (newest wins, D5). TERMINAL
// for voice: the browser does not rejoin voice by itself. It does not touch
// the room control session. Sent in-band first. Allocated 2026-10-26 (R79).
const CloseCodeVoiceRemoved = 4015

// CloseCodeVoiceUnavailable (4016) closes a browser's voice session because
// the bridge could not connect the person to Mumble (docs/80 D18): murmur
// down or unreachable, the bridge's address autobanned, murmur's user limit
// reached, a channel the bridged user may not enter, or a bridge or room
// voice cap. NOT terminal: the browser says voice is unavailable, with the
// reason from the close, and offers to try again. Sent in-band first.
// Allocated 2026-10-26 (R79).
const CloseCodeVoiceUnavailable = 4016

// Voice protocol constants.
const (
	// VoiceProtocolVersion is the voice protocol version a VoiceHello
	// carries, independent of the datagram Version byte.
	VoiceProtocolVersion = 1
	// VoiceFrameHeaderSize is the fixed VoiceFrame header, including the
	// common 2-byte prefix.
	VoiceFrameHeaderSize = 12
	// MaxVoicePayload is the largest Opus packet a VoiceFrame carries. A
	// 10 ms frame at 40 kbit/s is ~50 bytes; this is the datagram limit.
	MaxVoicePayload = MaxDatagramSize - VoiceFrameHeaderSize
	// MaxVoiceTarget is the largest Mumble voice target (5 bits).
	MaxVoiceTarget = 31
	// MaxVoiceTicketLen bounds the opaque ticket in a VoiceHello and a
	// VoiceTicket event. A real ticket is ~120 bytes.
	MaxVoiceTicketLen = 512
	// MaxVoiceNameLen bounds a Mumble user name carried on the voice wire
	// (VoiceState, the voice roster). Longer names are truncated on a rune
	// boundary by the producer.
	MaxVoiceNameLen = 64
	// MaxVoiceURLLen bounds the bridge URL in a VoiceTicket event.
	MaxVoiceURLLen = 512
	// MaxVoiceRosterEntries bounds one VoiceRosterUpdated event; it keeps
	// the record under MaxRoomRecordSize. A channel bigger than this shows
	// its first entries (community scale is dozens, docs/80 OD15).
	MaxVoiceRosterEntries = 200
)

// VoiceHello flags.
const (
	// VoiceHelloFlagSelfMute: join muted.
	VoiceHelloFlagSelfMute = 0x01
	// VoiceHelloFlagSelfDeaf: join deafened.
	VoiceHelloFlagSelfDeaf = 0x02
	// VoiceHelloFlagListenOnly: this browser never sends voice (listen-only
	// never called getUserMedia, docs/80 D21). The bridge joins the bridged
	// user self-muted and drops any VoiceFrame from it.
	VoiceHelloFlagListenOnly = 0x04
	voiceHelloFlagMask       = VoiceHelloFlagSelfMute | VoiceHelloFlagSelfDeaf | VoiceHelloFlagListenOnly
)

// VoiceFrame flags.
const (
	// VoiceFrameFlagEnd marks the last frame of a talk spurt (Mumble's
	// end-of-transmission bit, the Opus terminator).
	VoiceFrameFlagEnd  = 0x01
	voiceFrameFlagMask = VoiceFrameFlagEnd
)

// VoiceState flags.
const (
	// VoiceStateFlagServerMute: murmur (an admin or an ACL) muted this user.
	VoiceStateFlagServerMute = 0x01
	// VoiceStateFlagServerDeaf: murmur deafened this user.
	VoiceStateFlagServerDeaf = 0x02
	// VoiceStateFlagSuppressed: murmur suppressed this user (no Speak
	// permission in the channel).
	VoiceStateFlagSuppressed = 0x04
	// VoiceStateFlagSelfMute / VoiceStateFlagSelfDeaf echo the user's own
	// state as murmur holds it.
	VoiceStateFlagSelfMute = 0x08
	VoiceStateFlagSelfDeaf = 0x10
	// VoiceStateFlagConnected: the bridged Mumble connection is up and in
	// the room's channel. Clear while the bridge is still connecting.
	VoiceStateFlagConnected = 0x20
	voiceStateFlagMask      = 0x3f
)

// VoiceControl flags.
const (
	VoiceControlFlagSelfMute = 0x01
	VoiceControlFlagSelfDeaf = 0x02
	voiceControlFlagMask     = VoiceControlFlagSelfMute | VoiceControlFlagSelfDeaf
)

// Sentinel errors for the voice messages.
var (
	ErrBadVoiceHello   = errors.New("wire: invalid voice hello")
	ErrBadVoiceFrame   = errors.New("wire: invalid voice frame")
	ErrBadVoiceState   = errors.New("wire: invalid voice state")
	ErrBadVoiceControl = errors.New("wire: invalid voice control")
)

// VoiceHello is the parsed contents of a VoiceHello record.
type VoiceHello struct {
	Protocol uint8
	Flags    uint8
	// Ticket aliases the input record on parse.
	Ticket []byte
}

// VoiceFrameHeader is the parsed header of a VoiceFrame datagram.
type VoiceFrameHeader struct {
	End     bool
	Target  uint8
	Speaker uint32
	Seq     uint32
}

// VoiceState is the parsed contents of a VoiceState record.
type VoiceState struct {
	Flags   uint8
	Session uint32
	Name    string
}

// AppendVoiceHello appends a VoiceHello message encoding h to dst.
func AppendVoiceHello(dst []byte, h VoiceHello) ([]byte, error) {
	if h.Protocol != VoiceProtocolVersion {
		return nil, fmt.Errorf("%w: protocol %d, want %d", ErrBadVoiceHello, h.Protocol, VoiceProtocolVersion)
	}
	if h.Flags&^voiceHelloFlagMask != 0 {
		return nil, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadVoiceHello, h.Flags)
	}
	if len(h.Ticket) == 0 || len(h.Ticket) > MaxVoiceTicketLen {
		return nil, fmt.Errorf("%w: ticket %d bytes, want 1-%d", ErrBadVoiceHello, len(h.Ticket), MaxVoiceTicketLen)
	}
	dst = append(dst, Version, TypeVoiceHello, h.Protocol, h.Flags)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(h.Ticket)))
	return append(dst, h.Ticket...), nil
}

// ParseVoiceHello parses a VoiceHello message. Strict: exact length, known
// protocol, reserved bits zero, a non-empty ticket within bounds.
func ParseVoiceHello(msg []byte) (VoiceHello, error) {
	if err := checkPrefix(msg, TypeVoiceHello, 6, "voice hello"); err != nil {
		return VoiceHello{}, err
	}
	h := VoiceHello{Protocol: msg[2], Flags: msg[3]}
	if h.Protocol != VoiceProtocolVersion {
		return VoiceHello{}, fmt.Errorf("%w: protocol %d, want %d", ErrBadVoiceHello, h.Protocol, VoiceProtocolVersion)
	}
	if h.Flags&^voiceHelloFlagMask != 0 {
		return VoiceHello{}, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadVoiceHello, h.Flags)
	}
	n := int(binary.BigEndian.Uint16(msg[4:6]))
	if n == 0 || n > MaxVoiceTicketLen {
		return VoiceHello{}, fmt.Errorf("%w: ticket %d bytes, want 1-%d", ErrBadVoiceHello, n, MaxVoiceTicketLen)
	}
	if 6+n != len(msg) {
		return VoiceHello{}, fmt.Errorf("%w: ticket length %d in a %d-byte record", ErrBadVoiceHello, n, len(msg))
	}
	h.Ticket = msg[6:]
	return h, nil
}

// AppendVoiceFrame appends a VoiceFrame datagram to dst. The payload may be
// empty only when h.End is set.
func AppendVoiceFrame(dst []byte, h VoiceFrameHeader, payload []byte) ([]byte, error) {
	if len(payload) > MaxVoicePayload || (len(payload) == 0 && !h.End) {
		return nil, fmt.Errorf("%w: payload %d bytes (end %v), want 1-%d", ErrBadVoiceFrame, len(payload), h.End, MaxVoicePayload)
	}
	if h.Target > MaxVoiceTarget {
		return nil, fmt.Errorf("%w: target %d, max %d", ErrBadVoiceFrame, h.Target, MaxVoiceTarget)
	}
	var flags uint8
	if h.End {
		flags |= VoiceFrameFlagEnd
	}
	dst = append(dst, Version, TypeVoiceFrame, flags, h.Target)
	dst = binary.BigEndian.AppendUint32(dst, h.Speaker)
	dst = binary.BigEndian.AppendUint32(dst, h.Seq)
	return append(dst, payload...), nil
}

// ParseVoiceFrame parses a VoiceFrame datagram. The payload aliases dgram.
func ParseVoiceFrame(dgram []byte) (VoiceFrameHeader, []byte, error) {
	if err := checkPrefix(dgram, TypeVoiceFrame, VoiceFrameHeaderSize, "voice frame"); err != nil {
		return VoiceFrameHeader{}, nil, err
	}
	flags := dgram[2]
	if flags&^voiceFrameFlagMask != 0 {
		return VoiceFrameHeader{}, nil, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadVoiceFrame, flags)
	}
	h := VoiceFrameHeader{
		End:     flags&VoiceFrameFlagEnd != 0,
		Target:  dgram[3],
		Speaker: binary.BigEndian.Uint32(dgram[4:8]),
		Seq:     binary.BigEndian.Uint32(dgram[8:12]),
	}
	if h.Target > MaxVoiceTarget {
		return VoiceFrameHeader{}, nil, fmt.Errorf("%w: target %d, max %d", ErrBadVoiceFrame, h.Target, MaxVoiceTarget)
	}
	payload := dgram[VoiceFrameHeaderSize:]
	if len(payload) == 0 && !h.End {
		return VoiceFrameHeader{}, nil, fmt.Errorf("%w: empty payload without end of transmission", ErrBadVoiceFrame)
	}
	return h, payload, nil
}

// AppendVoiceState appends a VoiceState message encoding s to dst.
func AppendVoiceState(dst []byte, s VoiceState) ([]byte, error) {
	if s.Flags&^voiceStateFlagMask != 0 {
		return nil, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadVoiceState, s.Flags)
	}
	dst = append(dst, Version, TypeVoiceState, s.Flags)
	dst = binary.BigEndian.AppendUint32(dst, s.Session)
	return appendStr8(dst, s.Name, MaxVoiceNameLen, ErrBadVoiceState, "name")
}

// ParseVoiceState parses a VoiceState message. Strict.
func ParseVoiceState(msg []byte) (VoiceState, error) {
	if err := checkPrefix(msg, TypeVoiceState, 8, "voice state"); err != nil {
		return VoiceState{}, err
	}
	r := &roomReader{buf: msg, off: 2}
	var s VoiceState
	s.Flags = r.u8(ErrBadVoiceState)
	if r.err == nil && s.Flags&^voiceStateFlagMask != 0 {
		r.fail(ErrBadVoiceState, "reserved flag bits set (0x%02x)", s.Flags)
	}
	s.Session = r.u32(ErrBadVoiceState)
	s.Name = r.str8(ErrBadVoiceState, "name", MaxVoiceNameLen)
	if err := r.done(ErrBadVoiceState); err != nil {
		return VoiceState{}, err
	}
	return s, nil
}

// AppendVoiceControl appends a VoiceControl message carrying flags.
func AppendVoiceControl(dst []byte, flags uint8) ([]byte, error) {
	if flags&^voiceControlFlagMask != 0 {
		return nil, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadVoiceControl, flags)
	}
	return append(dst, Version, TypeVoiceControl, flags), nil
}

// ParseVoiceControl parses a VoiceControl message and returns its flags.
func ParseVoiceControl(msg []byte) (uint8, error) {
	if err := checkPrefix(msg, TypeVoiceControl, 3, "voice control"); err != nil {
		return 0, err
	}
	if len(msg) != 3 {
		return 0, fmt.Errorf("%w: %d bytes, want 3", ErrBadVoiceControl, len(msg))
	}
	if msg[2]&^voiceControlFlagMask != 0 {
		return 0, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadVoiceControl, msg[2])
	}
	return msg[2], nil
}

// TruncateVoiceName cuts a Mumble name to MaxVoiceNameLen bytes on a rune
// boundary, replacing invalid UTF-8 first. Producers call it before they
// put a name on the wire.
func TruncateVoiceName(s string) string {
	if !utf8.ValidString(s) {
		s = string([]rune(s))
	}
	for len(s) > MaxVoiceNameLen {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}
