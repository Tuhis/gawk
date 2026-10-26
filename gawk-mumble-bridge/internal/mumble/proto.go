package mumble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// The control channel: TLS over TCP, each message framed as
//
//	uint16 type ‖ uint32 length ‖ payload
//
// big-endian, the payload a protobuf message from Mumble.proto (BSD-3, The
// Mumble Developers) — except UDPTunnel, whose payload is a raw voice
// packet. The handful of messages the bridge needs are encoded and decoded
// by hand with protowire; the field numbers below are Mumble.proto's.

// Message types, in Mumble.proto order.
const (
	msgVersion             = 0
	msgUDPTunnel           = 1
	msgAuthenticate        = 2
	msgPing                = 3
	msgReject              = 4
	msgServerSync          = 5
	msgChannelRemove       = 6
	msgChannelState        = 7
	msgUserRemove          = 8
	msgUserState           = 9
	msgBanList             = 10
	msgTextMessage         = 11
	msgPermissionDenied    = 12
	msgACL                 = 13
	msgQueryUsers          = 14
	msgCryptSetup          = 15
	msgContextActionModify = 16
	msgContextAction       = 17
	msgUserList            = 18
	msgVoiceTarget         = 19
	msgPermissionQuery     = 20
	msgCodecVersion        = 21
	msgUserStats           = 22
	msgRequestBlob         = 23
	msgServerConfig        = 24
	msgSuggestConfig       = 25
)

// maxMessageLen bounds one control message. Murmur's own limit is 8 MiB
// (a texture can be large); the bridge never asks for blobs, so anything
// near this is a broken or hostile server.
const maxMessageLen = 8 << 20

const frameHeaderLen = 6

// errFrame is a control frame that cannot be read.
var errFrame = errors.New("mumble: bad control frame")

// appendFrame frames one message.
func appendFrame(dst []byte, typ uint16, payload []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, typ)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(payload)))
	return append(dst, payload...)
}

// readFrame reads one framed message; the length is checked before any
// allocation.
func readFrame(r io.Reader) (uint16, []byte, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	typ := binary.BigEndian.Uint16(hdr[:2])
	n := binary.BigEndian.Uint32(hdr[2:])
	if n > maxMessageLen {
		return 0, nil, fmt.Errorf("%w: type %d is %d bytes", errFrame, typ, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

// --- encoding helpers --------------------------------------------------------

func pbUint(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func pbBool(b []byte, num protowire.Number, v bool) []byte {
	return pbUint(b, num, protowire.EncodeBool(v))
}

func pbString(b []byte, num protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

// --- the messages the bridge sends -------------------------------------------

// Version is a Mumble client version.
type Version struct {
	Major, Minor, Patch uint16
}

// V1 is the legacy 32-bit form (major << 16 | minor << 8 | patch).
func (v Version) V1() uint32 {
	p := v.Patch
	if p > 255 {
		p = 255
	}
	m := v.Minor
	if m > 255 {
		m = 255
	}
	return uint32(v.Major)<<16 | uint32(m)<<8 | uint32(p)
}

// V2 is the 64-bit form Mumble 1.5 added (major << 48 | minor << 32 |
// patch << 16).
func (v Version) V2() uint64 {
	return uint64(v.Major)<<48 | uint64(v.Minor)<<32 | uint64(v.Patch)<<16
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool { return v.V2() < o.V2() }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// ParseVersion parses "major.minor.patch".
func ParseVersion(s string) (Version, error) {
	var v Version
	var extra string
	n, err := fmt.Sscanf(s+" .", "%d.%d.%d %s", &v.Major, &v.Minor, &v.Patch, &extra)
	if err != nil || n != 4 || extra != "." {
		return Version{}, fmt.Errorf("mumble: version %q is not major.minor.patch", s)
	}
	return v, nil
}

func encodeVersion(v Version, release, os, osVersion string) []byte {
	var b []byte
	b = pbUint(b, 1, uint64(v.V1()))
	b = pbString(b, 2, release)
	b = pbString(b, 3, os)
	b = pbString(b, 4, osVersion)
	return pbUint(b, 5, v.V2())
}

func encodeAuthenticate(username, password string, tokens []string) []byte {
	var b []byte
	b = pbString(b, 1, username)
	if password != "" {
		b = pbString(b, 2, password)
	}
	for _, t := range tokens {
		b = pbString(b, 3, t)
	}
	return pbBool(b, 5, true) // opus
}

func encodePing(timestamp uint64) []byte {
	return pbUint(nil, 1, timestamp)
}

// userStateUpdate is the subset of UserState the bridge sends: only the
// set fields are encoded.
type userStateUpdate struct {
	Session  uint32
	Channel  *uint32
	SelfMute *bool
	SelfDeaf *bool
	Mute     *bool
	Deaf     *bool
}

func encodeUserState(u userStateUpdate) []byte {
	b := pbUint(nil, 1, uint64(u.Session))
	if u.Channel != nil {
		b = pbUint(b, 5, uint64(*u.Channel))
	}
	if u.Mute != nil {
		b = pbBool(b, 6, *u.Mute)
	}
	if u.Deaf != nil {
		b = pbBool(b, 7, *u.Deaf)
	}
	if u.SelfMute != nil {
		b = pbBool(b, 9, *u.SelfMute)
	}
	if u.SelfDeaf != nil {
		b = pbBool(b, 10, *u.SelfDeaf)
	}
	return b
}

func encodeChannelCreate(parent uint32, name string, temporary bool) []byte {
	b := pbUint(nil, 2, uint64(parent))
	b = pbString(b, 3, name)
	if temporary {
		b = pbBool(b, 8, true)
	}
	return b
}

func encodeUserRemove(session uint32, reason string, ban bool) []byte {
	b := pbUint(nil, 1, uint64(session))
	if reason != "" {
		b = pbString(b, 3, reason)
	}
	if ban {
		b = pbBool(b, 4, true)
	}
	return b
}

// --- decoding ----------------------------------------------------------------

// field is one decoded protobuf field.
type field struct {
	num protowire.Number
	typ protowire.Type
	u   uint64 // varint, fixed32, fixed64
	b   []byte // bytes
}

// fields decodes every top-level field of a protobuf message.
func fields(msg []byte) ([]field, error) {
	var out []field
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		msg = msg[n:]
		f := field{num: num, typ: typ}
		switch typ {
		case protowire.VarintType:
			f.u, n = protowire.ConsumeVarint(msg)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(msg)
			f.u = uint64(v)
		case protowire.Fixed64Type:
			f.u, n = protowire.ConsumeFixed64(msg)
		case protowire.BytesType:
			f.b, n = protowire.ConsumeBytes(msg)
		default:
			n = protowire.ConsumeFieldValue(num, typ, msg)
		}
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		msg = msg[n:]
		out = append(out, f)
	}
	return out, nil
}

// RejectType is Mumble.proto's Reject.RejectType.
type RejectType int

// Reject types.
const (
	RejectNone              RejectType = 0
	RejectWrongVersion      RejectType = 1
	RejectInvalidUsername   RejectType = 2
	RejectWrongUserPW       RejectType = 3
	RejectWrongServerPW     RejectType = 4
	RejectUsernameInUse     RejectType = 5
	RejectServerFull        RejectType = 6
	RejectNoCertificate     RejectType = 7
	RejectAuthenticatorFail RejectType = 8
	RejectNoNewConnections  RejectType = 9
)

var rejectNames = map[RejectType]string{
	RejectNone: "None", RejectWrongVersion: "WrongVersion", RejectInvalidUsername: "InvalidUsername",
	RejectWrongUserPW: "WrongUserPW", RejectWrongServerPW: "WrongServerPW", RejectUsernameInUse: "UsernameInUse",
	RejectServerFull: "ServerFull", RejectNoCertificate: "NoCertificate", RejectAuthenticatorFail: "AuthenticatorFail",
	RejectNoNewConnections: "NoNewConnections",
}

func (t RejectType) String() string {
	if s, ok := rejectNames[t]; ok {
		return s
	}
	return fmt.Sprintf("RejectType(%d)", int(t))
}

func decodeReject(msg []byte) (*RejectError, error) {
	fs, err := fields(msg)
	if err != nil {
		return nil, err
	}
	r := &RejectError{}
	for _, f := range fs {
		switch f.num {
		case 1:
			r.Type = RejectType(f.u)
		case 2:
			r.Reason = string(f.b)
		}
	}
	return r, nil
}

// DenyType is Mumble.proto's PermissionDenied.DenyType.
type DenyType int

// Deny types.
const (
	DenyText                 DenyType = 0
	DenyPermission           DenyType = 1
	DenySuperUser            DenyType = 2
	DenyChannelName          DenyType = 3
	DenyTextTooLong          DenyType = 4
	DenyH9K                  DenyType = 5
	DenyTemporaryChannel     DenyType = 6
	DenyMissingCertificate   DenyType = 7
	DenyUserName             DenyType = 8
	DenyChannelFull          DenyType = 9
	DenyNestingLimit         DenyType = 10
	DenyChannelCountLimit    DenyType = 11
	DenyChannelListenerLimit DenyType = 12
	DenyUserListenerLimit    DenyType = 13
)

var denyNames = map[DenyType]string{
	DenyText: "Text", DenyPermission: "Permission", DenySuperUser: "SuperUser", DenyChannelName: "ChannelName",
	DenyTextTooLong: "TextTooLong", DenyH9K: "H9K", DenyTemporaryChannel: "TemporaryChannel",
	DenyMissingCertificate: "MissingCertificate", DenyUserName: "UserName", DenyChannelFull: "ChannelFull",
	DenyNestingLimit: "NestingLimit", DenyChannelCountLimit: "ChannelCountLimit",
	DenyChannelListenerLimit: "ChannelListenerLimit", DenyUserListenerLimit: "UserListenerLimit",
}

func (t DenyType) String() string {
	if s, ok := denyNames[t]; ok {
		return s
	}
	return fmt.Sprintf("DenyType(%d)", int(t))
}

// Permission bits (ChanACL::Perm) the bridge names in errors.
const (
	PermWrite           = 0x1
	PermTraverse        = 0x2
	PermEnter           = 0x4
	PermSpeak           = 0x8
	PermMuteDeafen      = 0x10
	PermMove            = 0x20
	PermMakeChannel     = 0x40
	PermLinkChannel     = 0x80
	PermWhisper         = 0x100
	PermTextMessage     = 0x200
	PermMakeTempChannel = 0x400
	PermKick            = 0x10000
	PermBan             = 0x20000
)

// PermissionDenied is a decoded PermissionDenied message.
type PermissionDenied struct {
	Type       DenyType
	Permission uint32
	Channel    uint32
	Session    uint32
	Reason     string
	Name       string
}

func decodePermissionDenied(msg []byte) (PermissionDenied, error) {
	fs, err := fields(msg)
	if err != nil {
		return PermissionDenied{}, err
	}
	var d PermissionDenied
	for _, f := range fs {
		switch f.num {
		case 1:
			d.Permission = uint32(f.u)
		case 2:
			d.Channel = uint32(f.u)
		case 3:
			d.Session = uint32(f.u)
		case 4:
			d.Reason = string(f.b)
		case 5:
			d.Type = DenyType(f.u)
		case 6:
			d.Name = string(f.b)
		}
	}
	return d, nil
}

type serverSync struct {
	session    uint32
	hasSession bool
	maxBW      uint32
}

func decodeServerSync(msg []byte) (serverSync, error) {
	fs, err := fields(msg)
	if err != nil {
		return serverSync{}, err
	}
	var s serverSync
	for _, f := range fs {
		switch f.num {
		case 1:
			s.session, s.hasSession = uint32(f.u), true
		case 2:
			s.maxBW = uint32(f.u)
		}
	}
	return s, nil
}

// channelState is a decoded ChannelState: the set fields only.
type channelState struct {
	id        uint32
	hasID     bool
	parent    *uint32
	name      *string
	temporary *bool
}

func decodeChannelState(msg []byte) (channelState, error) {
	fs, err := fields(msg)
	if err != nil {
		return channelState{}, err
	}
	var c channelState
	for _, f := range fs {
		switch f.num {
		case 1:
			c.id, c.hasID = uint32(f.u), true
		case 2:
			v := uint32(f.u)
			c.parent = &v
		case 3:
			s := string(f.b)
			c.name = &s
		case 8:
			v := f.u != 0
			c.temporary = &v
		}
	}
	return c, nil
}

func decodeChannelRemove(msg []byte) (uint32, error) {
	fs, err := fields(msg)
	if err != nil {
		return 0, err
	}
	for _, f := range fs {
		if f.num == 1 {
			return uint32(f.u), nil
		}
	}
	return 0, errors.New("mumble: ChannelRemove without channel_id")
}

// userState is a decoded UserState: the set fields only.
type userState struct {
	session    uint32
	hasSession bool
	actor      uint32
	hasActor   bool
	name       *string
	userID     *uint32
	channel    *uint32
	mute       *bool
	deaf       *bool
	suppress   *bool
	selfMute   *bool
	selfDeaf   *bool
}

func decodeUserState(msg []byte) (userState, error) {
	fs, err := fields(msg)
	if err != nil {
		return userState{}, err
	}
	var u userState
	b := func(f field) *bool { v := f.u != 0; return &v }
	u32 := func(f field) *uint32 {
		v := uint32(min(f.u, math.MaxUint32))
		return &v
	}
	for _, f := range fs {
		switch f.num {
		case 1:
			u.session, u.hasSession = uint32(f.u), true
		case 2:
			u.actor, u.hasActor = uint32(f.u), true
		case 3:
			s := string(f.b)
			u.name = &s
		case 4:
			u.userID = u32(f)
		case 5:
			u.channel = u32(f)
		case 6:
			u.mute = b(f)
		case 7:
			u.deaf = b(f)
		case 8:
			u.suppress = b(f)
		case 9:
			u.selfMute = b(f)
		case 10:
			u.selfDeaf = b(f)
		}
	}
	return u, nil
}

type userRemove struct {
	session  uint32
	actor    uint32
	hasActor bool
	reason   string
	ban      bool
}

func decodeUserRemove(msg []byte) (userRemove, error) {
	fs, err := fields(msg)
	if err != nil {
		return userRemove{}, err
	}
	var r userRemove
	for _, f := range fs {
		switch f.num {
		case 1:
			r.session = uint32(f.u)
		case 2:
			r.actor, r.hasActor = uint32(f.u), true
		case 3:
			r.reason = string(f.b)
		case 4:
			r.ban = f.u != 0
		}
	}
	return r, nil
}

// ACLEntry is one channel ACL entry for SetACL (an operator action: the
// bridge itself never edits ACLs; tests and gawk-mumblesim set up the
// temporary-channel parent with it, docs/80 §5).
type ACLEntry struct {
	// Group is the group the entry applies to ("all", "auth", "#token").
	Group     string
	ApplyHere bool
	ApplySubs bool
	Grant     uint32
	Deny      uint32
}

func encodeACL(channel uint32, entries []ACLEntry) []byte {
	b := pbUint(nil, 1, uint64(channel))
	b = pbBool(b, 2, true) // inherit_acls
	for _, e := range entries {
		var a []byte
		a = pbBool(a, 1, e.ApplyHere)
		a = pbBool(a, 2, e.ApplySubs)
		a = pbBool(a, 3, false) // inherited
		a = pbString(a, 5, e.Group)
		a = pbUint(a, 6, uint64(e.Grant))
		a = pbUint(a, 7, uint64(e.Deny))
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendBytes(b, a)
	}
	return b
}
