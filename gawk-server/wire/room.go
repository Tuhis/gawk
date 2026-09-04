package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
)

// Room control protocol (R42, docs/44 §4.6). Four message types travel as
// length-prefixed records on ONE bidirectional stream per room control
// session (CONNECT /room/{code}): each record is
// uint16 length (BE) ‖ Version ‖ Type ‖ payload, the reliable-carrier
// record shape (R19) applied to a bidirectional stream. Media never rides
// this stream — a participant's per-broadcast /subscribe sessions are
// untouched by rooms, which is what keeps the datagram path byte-identical.
//
// Parsers are strict in the house style (exact length, reserved bits zero)
// EXCEPT for the two forward-compatibility seams docs/44 §4.11 reserves on
// purpose: an unknown RoomEvent kind or RoomCommand kind parses to
// ErrUnknownRoomKind so a reader can skip the record (chat 0x40–0x4F and
// voice 0x50–0x5F are the reserved sub-ranges), and the capability bitmaps
// are the advertise/request mechanism for those later features.
//
// Layouts, after the common Version ‖ Type prefix:
//
//	0x13 RoomHello (client → relay, once, first record on the stream):
//	  byte 2   uint8 protocol      RoomProtocolVersion
//	  byte 3   uint8 clientKind    RoomClientKind*
//	  byte 4   uint8 wantCaps      RoomCap* bitmap; bits 2-7 reserved (0)
//	  byte 5   uint8 nickLen       0..MaxRoomNicknameLen
//	  bytes 6+ nickname            valid UTF-8
//
//	0x14 RoomState (relay → client, full snapshot; replace, never merge):
//	  byte 2   uint8 flags         RoomStateFlag* bitmap; bits 3-7 reserved
//	  byte 3   uint8 caps          RoomCap* bitmap advertised by the room
//	  4-7      uint32 seq          event sequence this snapshot is current at
//	  8-9      uint16 yourID       the receiving participant's ID
//	  10       uint8 codeLen       1..MaxRoomCodeLen, then the display code
//	  ...      uint8 nameLen       0..MaxRoomDisplayNameLen, then display name
//	  ...      uint8 tokenLen      0 or RoomCreatorTokenSize, then the creator
//	                               token — non-empty ONLY in the first
//	                               snapshot after /room/new (docs/44 §4.4)
//	  ...      uint8 keyLen        0 or RoomKeySize, then the room's HMAC'd
//	                               key — the /statusz + telemetry handle for
//	                               the room (docs/44 D16, §4.10), never the
//	                               code; the client reports it, it cannot
//	                               compute it
//	  ...      uint8 attachCount   then that many Attachment records
//	  ...      uint16 partCount    then that many Participant records
//
//	  Attachment record:
//	    uint8 idLen + broadcast ID, uint8 labelLen + label,
//	    uint8 flags (RoomAttachmentFlag*), uint32 viewerCount
//	  Participant record:
//	    uint16 id, uint8 kind (RoomClientKind*), uint8 flags
//	    (RoomParticipantFlag*), uint8 nickLen + nickname,
//	    uint8 identityLen + identity (reserved, empty in v1 — docs/44 §4.11)
//
//	0x15 RoomEvent (relay → client, one delta):
//	  2-5      uint32 seq          monotonic within the home pod's generation;
//	                               a CommandRejected is addressed to one
//	                               participant and carries the CURRENT seq
//	                               without advancing it (a gap is seq >
//	                               last+1, never seq <= last+1)
//	  6        uint8 kind          RoomEvent* kind
//	  7+       payload by kind:
//	    ParticipantJoined / ParticipantUpdated: Participant record
//	    ParticipantLeft:                        uint16 id
//	    AttachmentAdded / AttachmentUpdated:    Attachment record
//	    AttachmentRemoved: uint8 idLen + broadcast ID, uint8 reason
//	    RoomEnding:        uint8 reason (RoomEndReason*)
//	    CommandRejected:   uint8 command kind, uint8 reason
//	                       (RoomRejectReason*), uint8 msgLen + message
//
//	0x16 RoomCommand (client → relay):
//	  byte 2   uint8 kind          RoomCommand* kind
//	  3+       payload by kind:
//	    Attach:      uint8 idLen + broadcast ID, uint8 tokenLen
//	                 (ResumeTokenSize) + resume token, uint8 labelLen + label
//	    Detach:      uint8 idLen + broadcast ID
//	    SetNickname: uint8 nickLen + nickname
//	    EndRoom, Resync: no payload

// Room control message types (docs/44 D15).
const (
	// TypeRoomHello identifies a RoomHello record: client → relay, the first
	// record on a room control stream.
	TypeRoomHello = 0x13
	// TypeRoomState identifies a RoomState record: relay → client, a full
	// snapshot sent once after RoomHello and again after any adoption or
	// proxy re-establishment. Clients replace, never merge.
	TypeRoomState = 0x14
	// TypeRoomEvent identifies a RoomEvent record: relay → client, one delta.
	TypeRoomEvent = 0x15
	// TypeRoomCommand identifies a RoomCommand record: client → relay.
	TypeRoomCommand = 0x16
)

// CloseCodeRoomEnded (4007) is sent on a room CONTROL session when the room
// ends: empty-grace expiry, an explicit end from a creator-token holder, or
// deletion of the Room CR by the operator (docs/44 §4.4). TERMINAL for the
// room session only — a client must not reconnect to the room — while the
// participant's media sessions have their own lifecycle and are untouched
// (D1: attached broadcasts keep streaming to anyone watching them directly).
// Never sent on a publish or subscribe session. Allocated 2026-09-03 (R42).
const CloseCodeRoomEnded = 4007

// Room protocol constants.
const (
	// RoomProtocolVersion is the room control protocol version a RoomHello
	// carries. Independent of the datagram Version byte so the control
	// protocol can evolve (chat, voice) without touching the media wire.
	RoomProtocolVersion = 1
	// RoomRecordHeaderSize is the uint16 length prefix in front of every
	// record on a room control stream — the carrier record shape.
	RoomRecordHeaderSize = 2
	// MaxRoomRecordSize bounds one record's framed bytes (Version ‖ Type ‖
	// payload, what the length prefix counts): a reader must never allocate
	// more than this from an untrusted length. A RoomState for a full room
	// (-max-room-participants default 50, four attachments) is under 4 KiB.
	MaxRoomRecordSize = 16384
	// MaxRoomNicknameLen bounds a participant nickname in bytes (UTF-8).
	MaxRoomNicknameLen = 32
	// MaxRoomCodeLen bounds a room's display code: a static slug is 3–32
	// characters (docs/44 §4.1); a dynamic code is broadcastid.Length.
	MaxRoomCodeLen = 32
	// MaxRoomDisplayNameLen bounds a static room's optional display name.
	MaxRoomDisplayNameLen = 64
	// MaxRoomLabelLen bounds a broadcaster-chosen attachment label.
	MaxRoomLabelLen = 32
	// MaxRoomIdentityLen bounds the reserved participant identity field
	// (docs/44 §4.11: a key fingerprint later; empty in v1).
	MaxRoomIdentityLen = 64
	// MaxRoomRejectMessageLen bounds a CommandRejected message.
	MaxRoomRejectMessageLen = 128
	// RoomCreatorTokenSize is the byte length of a dynamic room's creator
	// token: the same truncated-HMAC construction as the resume token
	// (docs/44 D8), with its own domain-separation prefix.
	RoomCreatorTokenSize = 16
	// RoomKeySize is the byte length of a room's HMAC'd key as carried in
	// RoomState — the same 6-byte digest TelemetryHello carries for a
	// broadcast (TelemetryBroadcastKeySize), so telemetry keys both kinds
	// of object the same way.
	RoomKeySize = 6
	// ResumeTokenSize is the byte length of a broadcast resume token
	// (R17 W2) as it appears inside a RoomCommand.Attach — the attach proof
	// (docs/44 D9). 128 bits of HMAC-SHA256.
	ResumeTokenSize = 16
)

// RoomClientKind names what kind of client a participant is (RoomHello
// clientKind, Participant.kind).
const (
	RoomClientWebViewer      = 0
	RoomClientWebBroadcaster = 1
	RoomClientNative         = 2
	roomClientKindMax        = RoomClientNative
)

// Room capability bits (RoomHello wantCaps, RoomState caps). Both reserved
// for docs/44 §4.11's integrations; a v1 room advertises none.
const (
	RoomCapChat  = 0x01
	RoomCapVoice = 0x02
	roomCapMask  = RoomCapChat | RoomCapVoice
)

// RoomState flags.
const (
	// RoomStateFlagDynamic is set for a dynamic room (clear: static).
	RoomStateFlagDynamic = 0x01
	// RoomStateFlagCreator is set when the receiving participant presented
	// a valid creator token (may detach anyone, may end the room).
	RoomStateFlagCreator = 0x02
	// RoomStateFlagAttachOK is set when the receiving participant is allowed
	// to attach (a dynamic room, or a static room whose attach secret was
	// presented or is unset).
	RoomStateFlagAttachOK = 0x04
	roomStateFlagMask     = RoomStateFlagDynamic | RoomStateFlagCreator | RoomStateFlagAttachOK
)

// Attachment flags.
const (
	// RoomAttachmentFlagLive is set while the broadcast's publisher session
	// is up; clear means "broadcaster away" (within the broadcast grace).
	RoomAttachmentFlagLive = 0x01
	roomAttachmentFlagMask = RoomAttachmentFlagLive
)

// Participant flags.
const (
	// RoomParticipantFlagSpeaking is reserved for the voice integration
	// (docs/44 §4.11) and carried from day one so the roster's speaking
	// indicator does not need a wire change later.
	RoomParticipantFlagSpeaking = 0x01
	// RoomParticipantFlagStreaming is set while the participant has at least
	// one attached broadcast.
	RoomParticipantFlagStreaming = 0x02
	roomParticipantFlagMask      = RoomParticipantFlagSpeaking | RoomParticipantFlagStreaming
)

// RoomEvent kinds. 0x40–0x4F (chat) and 0x50–0x5F (voice) are reserved;
// a parser returns ErrUnknownRoomKind for any kind it does not know.
const (
	RoomEventParticipantJoined  = 0x01
	RoomEventParticipantLeft    = 0x02
	RoomEventParticipantUpdated = 0x03
	RoomEventAttachmentAdded    = 0x10
	RoomEventAttachmentRemoved  = 0x11
	RoomEventAttachmentUpdated  = 0x12
	RoomEventRoomEnding         = 0x20
	RoomEventCommandRejected    = 0x30
)

// RoomCommand kinds. Same reserved sub-ranges as events.
const (
	RoomCommandAttach      = 0x01
	RoomCommandDetach      = 0x02
	RoomCommandSetNickname = 0x03
	RoomCommandEndRoom     = 0x04
	RoomCommandResync      = 0x05
)

// RoomEndReason values (RoomEvent.RoomEnding).
const (
	RoomEndReasonEmpty    = 1
	RoomEndReasonCreator  = 2
	RoomEndReasonOperator = 3
)

// RoomDetachReason values (RoomEvent.AttachmentRemoved).
const (
	RoomDetachReasonPublisher = 0
	RoomDetachReasonCreator   = 1
	RoomDetachReasonExpired   = 2
	RoomDetachReasonRoomEnd   = 3
)

// RoomRejectReason values (RoomEvent.CommandRejected). Every distinct
// failure a command can hit crosses the wire as its own reason, never a
// catch-all (CODE-REVIEW.md "error mapping at boundaries").
const (
	// RoomRejectLimit: -max-room-broadcasts (or the CR's override) reached.
	RoomRejectLimit = 1
	// RoomRejectBadProof: the attach resume token does not verify.
	RoomRejectBadProof = 2
	// RoomRejectNotFound: the broadcast is unknown to the fleet, or the
	// attachment to detach does not exist.
	RoomRejectNotFound = 3
	// RoomRejectForbidden: the participant lacks the grant (attach without
	// the attach secret, detach-other / end without the creator token).
	RoomRejectForbidden = 4
	// RoomRejectAlreadyAttached: the broadcast is attached to another room
	// (D1: at most one room per broadcast).
	RoomRejectAlreadyAttached = 5
	// RoomRejectUnsupported: the relay does not implement the command kind
	// (a reserved chat/voice command on a v1 relay).
	RoomRejectUnsupported = 6
	// RoomRejectUnavailable: the command needs the API server and it is
	// unreachable (docs/44 §6, fail closed).
	RoomRejectUnavailable = 7
)

// Sentinel errors for the room protocol.
var (
	// ErrBadRoomRecord indicates a room record whose length prefix is zero
	// or exceeds MaxRoomRecordSize.
	ErrBadRoomRecord = errors.New("wire: invalid room record")
	// ErrBadRoomHello indicates a RoomHello that fails validation.
	ErrBadRoomHello = errors.New("wire: invalid room hello")
	// ErrBadRoomState indicates a RoomState that fails validation.
	ErrBadRoomState = errors.New("wire: invalid room state")
	// ErrBadRoomEvent indicates a RoomEvent that fails validation.
	ErrBadRoomEvent = errors.New("wire: invalid room event")
	// ErrBadRoomCommand indicates a RoomCommand that fails validation.
	ErrBadRoomCommand = errors.New("wire: invalid room command")
	// ErrUnknownRoomKind indicates a RoomEvent or RoomCommand of a kind this
	// implementation does not know — the docs/44 §4.11 reserved ranges. A
	// reader skips the record; a relay answers RoomRejectUnsupported.
	ErrUnknownRoomKind = errors.New("wire: unknown room event/command kind")
)

// RoomHello is the parsed contents of a RoomHello record.
type RoomHello struct {
	// Protocol is the room protocol version; RoomProtocolVersion today.
	Protocol uint8
	// ClientKind is one of the RoomClient* constants.
	ClientKind uint8
	// WantCaps is the requested RoomCap* bitmap (0 in v1 clients).
	WantCaps uint8
	// Nickname is the requested display name; may be empty (the relay then
	// assigns one).
	Nickname string
}

// RoomAttachment is one attached broadcast as carried in RoomState and the
// attachment events.
type RoomAttachment struct {
	BroadcastID string
	Label       string
	Live        bool
	ViewerCount uint32
}

// RoomParticipant is one participant as carried in RoomState and the
// participant events.
type RoomParticipant struct {
	ID       uint16
	Kind     uint8
	Flags    uint8
	Nickname string
	// Identity is reserved (docs/44 §4.11); empty in v1.
	Identity string
}

// RoomState is the parsed contents of a RoomState record.
type RoomState struct {
	Flags        uint8
	Caps         uint8
	Seq          uint32
	YourID       uint16
	Code         string
	DisplayName  string
	CreatorToken []byte
	// Key is the room's HMAC'd handle (RoomKeySize bytes) or empty when
	// the relay has none to give.
	Key          []byte
	Attachments  []RoomAttachment
	Participants []RoomParticipant
}

// RoomEvent is the parsed contents of a RoomEvent record. Which fields are
// meaningful depends on Kind (see the layout comment at the top of the
// file).
type RoomEvent struct {
	Seq  uint32
	Kind uint8
	// Participant is set for ParticipantJoined / ParticipantUpdated; only
	// its ID for ParticipantLeft.
	Participant RoomParticipant
	// Attachment is set for AttachmentAdded / AttachmentUpdated; only its
	// BroadcastID for AttachmentRemoved.
	Attachment RoomAttachment
	// Reason is the RoomEndReason* (RoomEnding), RoomDetachReason*
	// (AttachmentRemoved) or RoomRejectReason* (CommandRejected).
	Reason uint8
	// Command is the rejected command's kind (CommandRejected).
	Command uint8
	// Message is the human-readable rejection detail (CommandRejected).
	Message string
}

// RoomCommand is the parsed contents of a RoomCommand record.
type RoomCommand struct {
	Kind uint8
	// BroadcastID is set for Attach / Detach.
	BroadcastID string
	// ResumeToken is the attach proof (Attach only), exactly ResumeTokenSize
	// bytes. Aliases the input record on parse.
	ResumeToken []byte
	// Label is the attachment label (Attach only).
	Label string
	// Nickname is the new nickname (SetNickname only).
	Nickname string
}

// --- record framing ---------------------------------------------------------

// AppendRoomRecord frames one already-encoded room message (Version ‖ Type ‖
// payload, as produced by the AppendRoom* functions) as a length-prefixed
// record for a room control stream.
func AppendRoomRecord(dst, msg []byte) ([]byte, error) {
	if len(msg) < 2 || len(msg) > MaxRoomRecordSize {
		return nil, fmt.Errorf("%w: %d bytes, want 2-%d", ErrBadRoomRecord, len(msg), MaxRoomRecordSize)
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(msg)))
	return append(dst, msg...), nil
}

// ParseRoomRecordLength validates a room record's length prefix and returns
// the framed message length. It exists so a stream reader never allocates
// from an unvalidated length.
func ParseRoomRecordLength(hdr []byte) (int, error) {
	if len(hdr) < RoomRecordHeaderSize {
		return 0, fmt.Errorf("%w: %d bytes, need %d for record header", ErrShortDatagram, len(hdr), RoomRecordHeaderSize)
	}
	n := int(binary.BigEndian.Uint16(hdr[:2]))
	if n < 2 || n > MaxRoomRecordSize {
		return 0, fmt.Errorf("%w: length %d, want 2-%d", ErrBadRoomRecord, n, MaxRoomRecordSize)
	}
	return n, nil
}

// --- shared field helpers -------------------------------------------------

// roomReader is a bounds-checked cursor over one record. Every read reports
// an overrun through the record's sentinel error rather than panicking.
type roomReader struct {
	buf []byte
	off int
	err error
}

func (r *roomReader) fail(err error, format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: "+format, append([]any{err}, args...)...)
	}
}

func (r *roomReader) u8(sentinel error) uint8 {
	if r.err != nil {
		return 0
	}
	if r.off+1 > len(r.buf) {
		r.fail(sentinel, "truncated at byte %d", r.off)
		return 0
	}
	v := r.buf[r.off]
	r.off++
	return v
}

func (r *roomReader) u16(sentinel error) uint16 {
	if r.err != nil {
		return 0
	}
	if r.off+2 > len(r.buf) {
		r.fail(sentinel, "truncated at byte %d", r.off)
		return 0
	}
	v := binary.BigEndian.Uint16(r.buf[r.off:])
	r.off += 2
	return v
}

func (r *roomReader) u32(sentinel error) uint32 {
	if r.err != nil {
		return 0
	}
	if r.off+4 > len(r.buf) {
		r.fail(sentinel, "truncated at byte %d", r.off)
		return 0
	}
	v := binary.BigEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v
}

// bytes8 reads a uint8-length-prefixed byte field, bounded by max.
func (r *roomReader) bytes8(sentinel error, what string, max int) []byte {
	n := int(r.u8(sentinel))
	if r.err != nil {
		return nil
	}
	if n > max {
		r.fail(sentinel, "%s %d bytes, max %d", what, n, max)
		return nil
	}
	if r.off+n > len(r.buf) {
		r.fail(sentinel, "%s overruns record", what)
		return nil
	}
	v := r.buf[r.off : r.off+n]
	r.off += n
	return v
}

// str8 reads a uint8-length-prefixed UTF-8 string field, bounded by max.
func (r *roomReader) str8(sentinel error, what string, max int) string {
	b := r.bytes8(sentinel, what, max)
	if r.err != nil {
		return ""
	}
	if !utf8.Valid(b) {
		r.fail(sentinel, "%s is not valid UTF-8", what)
		return ""
	}
	return string(b)
}

// broadcastID reads a uint8-length-prefixed broadcast ID and normalizes it.
func (r *roomReader) broadcastID(sentinel error) string {
	b := r.bytes8(sentinel, "broadcast id", broadcastid.Length)
	if r.err != nil {
		return ""
	}
	id, err := broadcastid.Normalize(string(b))
	if err != nil {
		r.fail(sentinel, "broadcast id %q", string(b))
		return ""
	}
	return id
}

// done asserts the record was consumed exactly (house strictness).
func (r *roomReader) done(sentinel error) error {
	if r.err != nil {
		return r.err
	}
	if r.off != len(r.buf) {
		return fmt.Errorf("%w: %d trailing bytes", sentinel, len(r.buf)-r.off)
	}
	return nil
}

func checkPrefix(msg []byte, msgType uint8, minLen int, what string) error {
	if len(msg) < minLen {
		return fmt.Errorf("%w: %d bytes, need at least %d for %s", ErrShortDatagram, len(msg), minLen, what)
	}
	if msg[0] != Version {
		return fmt.Errorf("%w: 0x%02x", ErrBadVersion, msg[0])
	}
	if msg[1] != msgType {
		return fmt.Errorf("%w: got 0x%02x, want %s 0x%02x", ErrBadType, msg[1], what, msgType)
	}
	return nil
}

func appendStr8(dst []byte, s string, max int, sentinel error, what string) ([]byte, error) {
	if len(s) > max {
		return nil, fmt.Errorf("%w: %s %d bytes, max %d", sentinel, what, len(s), max)
	}
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("%w: %s is not valid UTF-8", sentinel, what)
	}
	dst = append(dst, uint8(len(s)))
	return append(dst, s...), nil
}

func appendBroadcastID(dst []byte, id string, sentinel error) ([]byte, error) {
	norm, err := broadcastid.Normalize(id)
	if err != nil {
		return nil, fmt.Errorf("%w: broadcast id %q", sentinel, id)
	}
	dst = append(dst, uint8(len(norm)))
	return append(dst, norm...), nil
}

func appendRoomAttachment(dst []byte, a RoomAttachment, sentinel error) ([]byte, error) {
	var err error
	if dst, err = appendBroadcastID(dst, a.BroadcastID, sentinel); err != nil {
		return nil, err
	}
	if dst, err = appendStr8(dst, a.Label, MaxRoomLabelLen, sentinel, "label"); err != nil {
		return nil, err
	}
	var flags uint8
	if a.Live {
		flags |= RoomAttachmentFlagLive
	}
	dst = append(dst, flags)
	return binary.BigEndian.AppendUint32(dst, a.ViewerCount), nil
}

func (r *roomReader) attachment(sentinel error) RoomAttachment {
	var a RoomAttachment
	a.BroadcastID = r.broadcastID(sentinel)
	a.Label = r.str8(sentinel, "label", MaxRoomLabelLen)
	flags := r.u8(sentinel)
	if r.err == nil && flags&^roomAttachmentFlagMask != 0 {
		r.fail(sentinel, "reserved attachment flag bits set (0x%02x)", flags)
	}
	a.Live = flags&RoomAttachmentFlagLive != 0
	a.ViewerCount = r.u32(sentinel)
	return a
}

func appendRoomParticipant(dst []byte, p RoomParticipant, sentinel error) ([]byte, error) {
	if p.Kind > roomClientKindMax {
		return nil, fmt.Errorf("%w: participant kind %d", sentinel, p.Kind)
	}
	if p.Flags&^roomParticipantFlagMask != 0 {
		return nil, fmt.Errorf("%w: reserved participant flag bits set (0x%02x)", sentinel, p.Flags)
	}
	dst = binary.BigEndian.AppendUint16(dst, p.ID)
	dst = append(dst, p.Kind, p.Flags)
	var err error
	if dst, err = appendStr8(dst, p.Nickname, MaxRoomNicknameLen, sentinel, "nickname"); err != nil {
		return nil, err
	}
	return appendStr8(dst, p.Identity, MaxRoomIdentityLen, sentinel, "identity")
}

func (r *roomReader) participant(sentinel error) RoomParticipant {
	var p RoomParticipant
	p.ID = r.u16(sentinel)
	p.Kind = r.u8(sentinel)
	if r.err == nil && p.Kind > roomClientKindMax {
		r.fail(sentinel, "participant kind %d", p.Kind)
	}
	p.Flags = r.u8(sentinel)
	if r.err == nil && p.Flags&^roomParticipantFlagMask != 0 {
		r.fail(sentinel, "reserved participant flag bits set (0x%02x)", p.Flags)
	}
	p.Nickname = r.str8(sentinel, "nickname", MaxRoomNicknameLen)
	p.Identity = r.str8(sentinel, "identity", MaxRoomIdentityLen)
	return p
}

// --- RoomHello (0x13) -------------------------------------------------------

// AppendRoomHello appends a RoomHello message encoding h to dst.
func AppendRoomHello(dst []byte, h RoomHello) ([]byte, error) {
	if h.Protocol != RoomProtocolVersion {
		return nil, fmt.Errorf("%w: protocol %d, want %d", ErrBadRoomHello, h.Protocol, RoomProtocolVersion)
	}
	if h.ClientKind > roomClientKindMax {
		return nil, fmt.Errorf("%w: client kind %d", ErrBadRoomHello, h.ClientKind)
	}
	if h.WantCaps&^roomCapMask != 0 {
		return nil, fmt.Errorf("%w: reserved capability bits set (0x%02x)", ErrBadRoomHello, h.WantCaps)
	}
	dst = append(dst, Version, TypeRoomHello, h.Protocol, h.ClientKind, h.WantCaps)
	return appendStr8(dst, h.Nickname, MaxRoomNicknameLen, ErrBadRoomHello, "nickname")
}

// ParseRoomHello parses a RoomHello message. Strict: exact length, known
// protocol version and client kind, reserved capability bits zero.
func ParseRoomHello(msg []byte) (RoomHello, error) {
	if err := checkPrefix(msg, TypeRoomHello, 6, "room hello"); err != nil {
		return RoomHello{}, err
	}
	r := &roomReader{buf: msg, off: 2}
	var h RoomHello
	h.Protocol = r.u8(ErrBadRoomHello)
	if r.err == nil && h.Protocol != RoomProtocolVersion {
		r.fail(ErrBadRoomHello, "protocol %d, want %d", h.Protocol, RoomProtocolVersion)
	}
	h.ClientKind = r.u8(ErrBadRoomHello)
	if r.err == nil && h.ClientKind > roomClientKindMax {
		r.fail(ErrBadRoomHello, "client kind %d", h.ClientKind)
	}
	h.WantCaps = r.u8(ErrBadRoomHello)
	if r.err == nil && h.WantCaps&^roomCapMask != 0 {
		r.fail(ErrBadRoomHello, "reserved capability bits set (0x%02x)", h.WantCaps)
	}
	h.Nickname = r.str8(ErrBadRoomHello, "nickname", MaxRoomNicknameLen)
	if err := r.done(ErrBadRoomHello); err != nil {
		return RoomHello{}, err
	}
	return h, nil
}

// --- RoomState (0x14) -------------------------------------------------------

// AppendRoomState appends a RoomState message encoding s to dst.
func AppendRoomState(dst []byte, s RoomState) ([]byte, error) {
	if s.Flags&^roomStateFlagMask != 0 {
		return nil, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadRoomState, s.Flags)
	}
	if s.Caps&^roomCapMask != 0 {
		return nil, fmt.Errorf("%w: reserved capability bits set (0x%02x)", ErrBadRoomState, s.Caps)
	}
	if len(s.Code) == 0 {
		return nil, fmt.Errorf("%w: empty code", ErrBadRoomState)
	}
	if len(s.CreatorToken) != 0 && len(s.CreatorToken) != RoomCreatorTokenSize {
		return nil, fmt.Errorf("%w: creator token %d bytes, want 0 or %d", ErrBadRoomState, len(s.CreatorToken), RoomCreatorTokenSize)
	}
	if len(s.Key) != 0 && len(s.Key) != RoomKeySize {
		return nil, fmt.Errorf("%w: key %d bytes, want 0 or %d", ErrBadRoomState, len(s.Key), RoomKeySize)
	}
	if len(s.Attachments) > 255 {
		return nil, fmt.Errorf("%w: %d attachments, max 255", ErrBadRoomState, len(s.Attachments))
	}
	if len(s.Participants) > 65535 {
		return nil, fmt.Errorf("%w: %d participants, max 65535", ErrBadRoomState, len(s.Participants))
	}
	dst = append(dst, Version, TypeRoomState, s.Flags, s.Caps)
	dst = binary.BigEndian.AppendUint32(dst, s.Seq)
	dst = binary.BigEndian.AppendUint16(dst, s.YourID)
	var err error
	if dst, err = appendStr8(dst, s.Code, MaxRoomCodeLen, ErrBadRoomState, "code"); err != nil {
		return nil, err
	}
	if dst, err = appendStr8(dst, s.DisplayName, MaxRoomDisplayNameLen, ErrBadRoomState, "display name"); err != nil {
		return nil, err
	}
	dst = append(dst, uint8(len(s.CreatorToken)))
	dst = append(dst, s.CreatorToken...)
	dst = append(dst, uint8(len(s.Key)))
	dst = append(dst, s.Key...)
	dst = append(dst, uint8(len(s.Attachments)))
	for _, a := range s.Attachments {
		if dst, err = appendRoomAttachment(dst, a, ErrBadRoomState); err != nil {
			return nil, err
		}
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(s.Participants)))
	for _, p := range s.Participants {
		if dst, err = appendRoomParticipant(dst, p, ErrBadRoomState); err != nil {
			return nil, err
		}
	}
	if len(dst) > MaxRoomRecordSize {
		return nil, fmt.Errorf("%w: %d bytes exceeds MaxRoomRecordSize", ErrBadRoomState, len(dst))
	}
	return dst, nil
}

// ParseRoomState parses a RoomState message. The returned CreatorToken and
// Key alias msg (no copy, like every other parser here): a caller that
// keeps the state past the record buffer's lifetime must copy them. Strict:
// exact length, reserved bits zero.
func ParseRoomState(msg []byte) (RoomState, error) {
	if err := checkPrefix(msg, TypeRoomState, 16, "room state"); err != nil {
		return RoomState{}, err
	}
	r := &roomReader{buf: msg, off: 2}
	var s RoomState
	s.Flags = r.u8(ErrBadRoomState)
	if r.err == nil && s.Flags&^roomStateFlagMask != 0 {
		r.fail(ErrBadRoomState, "reserved flag bits set (0x%02x)", s.Flags)
	}
	s.Caps = r.u8(ErrBadRoomState)
	if r.err == nil && s.Caps&^roomCapMask != 0 {
		r.fail(ErrBadRoomState, "reserved capability bits set (0x%02x)", s.Caps)
	}
	s.Seq = r.u32(ErrBadRoomState)
	s.YourID = r.u16(ErrBadRoomState)
	s.Code = r.str8(ErrBadRoomState, "code", MaxRoomCodeLen)
	if r.err == nil && s.Code == "" {
		r.fail(ErrBadRoomState, "empty code")
	}
	s.DisplayName = r.str8(ErrBadRoomState, "display name", MaxRoomDisplayNameLen)
	tok := r.bytes8(ErrBadRoomState, "creator token", RoomCreatorTokenSize)
	if r.err == nil && len(tok) != 0 && len(tok) != RoomCreatorTokenSize {
		r.fail(ErrBadRoomState, "creator token %d bytes, want 0 or %d", len(tok), RoomCreatorTokenSize)
	}
	if len(tok) > 0 {
		s.CreatorToken = tok
	}
	key := r.bytes8(ErrBadRoomState, "key", RoomKeySize)
	if r.err == nil && len(key) != 0 && len(key) != RoomKeySize {
		r.fail(ErrBadRoomState, "key %d bytes, want 0 or %d", len(key), RoomKeySize)
	}
	if len(key) > 0 {
		s.Key = key
	}
	n := int(r.u8(ErrBadRoomState))
	for i := 0; i < n && r.err == nil; i++ {
		s.Attachments = append(s.Attachments, r.attachment(ErrBadRoomState))
	}
	m := int(r.u16(ErrBadRoomState))
	for i := 0; i < m && r.err == nil; i++ {
		s.Participants = append(s.Participants, r.participant(ErrBadRoomState))
	}
	if err := r.done(ErrBadRoomState); err != nil {
		return RoomState{}, err
	}
	return s, nil
}

// --- RoomEvent (0x15) -------------------------------------------------------

// AppendRoomEvent appends a RoomEvent message encoding e to dst. Unknown
// kinds are refused with ErrUnknownRoomKind: a relay only emits what it
// implements.
func AppendRoomEvent(dst []byte, e RoomEvent) ([]byte, error) {
	dst = append(dst, Version, TypeRoomEvent)
	dst = binary.BigEndian.AppendUint32(dst, e.Seq)
	dst = append(dst, e.Kind)
	var err error
	switch e.Kind {
	case RoomEventParticipantJoined, RoomEventParticipantUpdated:
		dst, err = appendRoomParticipant(dst, e.Participant, ErrBadRoomEvent)
	case RoomEventParticipantLeft:
		dst = binary.BigEndian.AppendUint16(dst, e.Participant.ID)
	case RoomEventAttachmentAdded, RoomEventAttachmentUpdated:
		dst, err = appendRoomAttachment(dst, e.Attachment, ErrBadRoomEvent)
	case RoomEventAttachmentRemoved:
		if dst, err = appendBroadcastID(dst, e.Attachment.BroadcastID, ErrBadRoomEvent); err == nil {
			dst = append(dst, e.Reason)
		}
	case RoomEventRoomEnding:
		dst = append(dst, e.Reason)
	case RoomEventCommandRejected:
		dst = append(dst, e.Command, e.Reason)
		dst, err = appendStr8(dst, e.Message, MaxRoomRejectMessageLen, ErrBadRoomEvent, "message")
	default:
		return nil, fmt.Errorf("%w: event 0x%02x", ErrUnknownRoomKind, e.Kind)
	}
	if err != nil {
		return nil, err
	}
	return dst, nil
}

// ParseRoomEvent parses a RoomEvent message. An unknown kind (including the
// reserved chat/voice ranges) returns ErrUnknownRoomKind with the Seq and
// Kind filled in, so a reader can skip it without losing its place in the
// sequence.
func ParseRoomEvent(msg []byte) (RoomEvent, error) {
	if err := checkPrefix(msg, TypeRoomEvent, 7, "room event"); err != nil {
		return RoomEvent{}, err
	}
	r := &roomReader{buf: msg, off: 2}
	var e RoomEvent
	e.Seq = r.u32(ErrBadRoomEvent)
	e.Kind = r.u8(ErrBadRoomEvent)
	switch e.Kind {
	case RoomEventParticipantJoined, RoomEventParticipantUpdated:
		e.Participant = r.participant(ErrBadRoomEvent)
	case RoomEventParticipantLeft:
		e.Participant.ID = r.u16(ErrBadRoomEvent)
	case RoomEventAttachmentAdded, RoomEventAttachmentUpdated:
		e.Attachment = r.attachment(ErrBadRoomEvent)
	case RoomEventAttachmentRemoved:
		e.Attachment.BroadcastID = r.broadcastID(ErrBadRoomEvent)
		e.Reason = r.u8(ErrBadRoomEvent)
	case RoomEventRoomEnding:
		e.Reason = r.u8(ErrBadRoomEvent)
	case RoomEventCommandRejected:
		e.Command = r.u8(ErrBadRoomEvent)
		e.Reason = r.u8(ErrBadRoomEvent)
		e.Message = r.str8(ErrBadRoomEvent, "message", MaxRoomRejectMessageLen)
	default:
		return e, fmt.Errorf("%w: event 0x%02x", ErrUnknownRoomKind, e.Kind)
	}
	if err := r.done(ErrBadRoomEvent); err != nil {
		return RoomEvent{}, err
	}
	return e, nil
}

// --- RoomCommand (0x16) -----------------------------------------------------

// AppendRoomCommand appends a RoomCommand message encoding c to dst.
func AppendRoomCommand(dst []byte, c RoomCommand) ([]byte, error) {
	dst = append(dst, Version, TypeRoomCommand, c.Kind)
	var err error
	switch c.Kind {
	case RoomCommandAttach:
		if len(c.ResumeToken) != ResumeTokenSize {
			return nil, fmt.Errorf("%w: resume token %d bytes, want %d", ErrBadRoomCommand, len(c.ResumeToken), ResumeTokenSize)
		}
		if dst, err = appendBroadcastID(dst, c.BroadcastID, ErrBadRoomCommand); err != nil {
			return nil, err
		}
		dst = append(dst, uint8(len(c.ResumeToken)))
		dst = append(dst, c.ResumeToken...)
		dst, err = appendStr8(dst, c.Label, MaxRoomLabelLen, ErrBadRoomCommand, "label")
	case RoomCommandDetach:
		dst, err = appendBroadcastID(dst, c.BroadcastID, ErrBadRoomCommand)
	case RoomCommandSetNickname:
		dst, err = appendStr8(dst, c.Nickname, MaxRoomNicknameLen, ErrBadRoomCommand, "nickname")
	case RoomCommandEndRoom, RoomCommandResync:
		// no payload
	default:
		return nil, fmt.Errorf("%w: command 0x%02x", ErrUnknownRoomKind, c.Kind)
	}
	if err != nil {
		return nil, err
	}
	return dst, nil
}

// ParseRoomCommand parses a RoomCommand message. The returned ResumeToken
// aliases msg. An unknown kind returns ErrUnknownRoomKind with Kind filled
// in, so the relay can answer RoomRejectUnsupported.
func ParseRoomCommand(msg []byte) (RoomCommand, error) {
	if err := checkPrefix(msg, TypeRoomCommand, 3, "room command"); err != nil {
		return RoomCommand{}, err
	}
	r := &roomReader{buf: msg, off: 2}
	var c RoomCommand
	c.Kind = r.u8(ErrBadRoomCommand)
	switch c.Kind {
	case RoomCommandAttach:
		c.BroadcastID = r.broadcastID(ErrBadRoomCommand)
		tok := r.bytes8(ErrBadRoomCommand, "resume token", ResumeTokenSize)
		if r.err == nil && len(tok) != ResumeTokenSize {
			r.fail(ErrBadRoomCommand, "resume token %d bytes, want %d", len(tok), ResumeTokenSize)
		}
		c.ResumeToken = tok
		c.Label = r.str8(ErrBadRoomCommand, "label", MaxRoomLabelLen)
	case RoomCommandDetach:
		c.BroadcastID = r.broadcastID(ErrBadRoomCommand)
	case RoomCommandSetNickname:
		c.Nickname = r.str8(ErrBadRoomCommand, "nickname", MaxRoomNicknameLen)
	case RoomCommandEndRoom, RoomCommandResync:
	default:
		return c, fmt.Errorf("%w: command 0x%02x", ErrUnknownRoomKind, c.Kind)
	}
	if err := r.done(ErrBadRoomCommand); err != nil {
		return RoomCommand{}, err
	}
	return c, nil
}
