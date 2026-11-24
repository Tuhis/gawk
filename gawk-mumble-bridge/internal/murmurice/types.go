package murmurice

import (
	"fmt"
	"strconv"
	"strings"
)

// User is the part of murmur's MumbleServer::User a roster needs, and
// nothing more (D10). The decoder reads the address, comment, identity,
// plugin context and client details off the wire and drops them; this type
// has no field to keep them in, and TestNoIdentifyingFields keeps it so.
type User struct {
	Session  uint32
	UserID   int32 // registered user ID, -1 when unregistered
	Name     string
	Channel  uint32
	Mute     bool // server mute
	Deaf     bool // server deafen
	Suppress bool
	SelfMute bool
	SelfDeaf bool
}

// Channel is the part of MumbleServer::Channel the bridge needs.
type Channel struct {
	ID        uint32
	Parent    uint32
	Name      string
	Temporary bool
}

// Version is murmur's Meta.getVersion.
type Version struct {
	Major, Minor, Patch int
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less orders versions.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// ParseVersion reads "1.6.1001".
func ParseVersion(s string) (Version, error) {
	p := strings.Split(strings.TrimSpace(s), ".")
	if len(p) != 3 {
		return Version{}, fmt.Errorf("version %q", s)
	}
	var v [3]int
	for i, x := range p {
		n, err := strconv.Atoi(x)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("version %q", s)
		}
		v[i] = n
	}
	return Version{v[0], v[1], v[2]}, nil
}

// CallbacksUnsafe is D16: murmur 1.6.0 up to and including 1.6.870 crashes
// or drops the wrong callback when one is dead (F1–F3), so the bridge never
// registers one there.
func (v Version) CallbacksUnsafe() bool {
	return v.Major == 1 && v.Minor == 6 && v.Patch <= 870
}

// user reads one MumbleServer::User (identical in 1.5 and 1.6, MI0).
func (d *decoder) user() User {
	var u User
	u.Session = uint32(d.i32())
	u.UserID = d.i32()
	u.Mute, u.Deaf, u.Suppress = d.boolean(), d.boolean(), d.boolean()
	d.boolean() // prioritySpeaker
	u.SelfMute, u.SelfDeaf = d.boolean(), d.boolean()
	d.boolean() // recording
	u.Channel = uint32(d.i32())
	u.Name = d.str()
	d.i32() // onlinesecs
	d.i32() // bytespersec
	d.i32() // version
	d.i64() // version2
	// release, os, osversion, identity, context, comment: read, never kept.
	for range 6 {
		d.skipStr()
	}
	d.next(d.size()) // address (NetAddress): read, never kept
	d.boolean()      // tcponly
	d.i32()          // idlesecs
	d.f32()          // udpPing
	d.f32()          // tcpPing
	return u
}

// channel reads one MumbleServer::Channel.
func (d *decoder) channel() Channel {
	var c Channel
	c.ID = uint32(d.i32())
	c.Name = d.str()
	c.Parent = uint32(d.i32())
	for n := d.size(); n > 0 && d.err() == nil; n-- {
		d.i32() // links
	}
	d.skipStr() // description
	c.Temporary = d.boolean()
	d.i32() // position
	return c
}

// textMessage reads and drops a MumbleServer::TextMessage: the bridge never
// reads users' messages.
func (d *decoder) textMessage() {
	for range 3 { // sessions, channels, trees
		for n := d.size(); n > 0 && d.err() == nil; n-- {
			d.i32()
		}
	}
	d.skipStr() // text
}

// Event ops: the seven ServerCallback operations.
const (
	OpUserConnected       = "userConnected"
	OpUserDisconnected    = "userDisconnected"
	OpUserStateChanged    = "userStateChanged"
	OpUserTextMessage     = "userTextMessage"
	OpChannelCreated      = "channelCreated"
	OpChannelRemoved      = "channelRemoved"
	OpChannelStateChanged = "channelStateChanged"
)

// EventOps is every ServerCallback operation, for metric labels.
var EventOps = []string{OpUserConnected, OpUserDisconnected, OpUserStateChanged, OpUserTextMessage,
	OpChannelCreated, OpChannelRemoved, OpChannelStateChanged}

// Event is one ServerCallback call. User is set for the user* ops, Channel
// for the channel* ops.
type Event struct {
	Op      string
	User    User
	Channel Channel
}

// decodeEvent decodes a ServerCallback operation's parameters, which must
// end exactly where the event does.
func decodeEvent(op string, params []byte) (Event, error) {
	d := newDecoder(params)
	ev := Event{Op: op}
	switch op {
	case OpUserConnected, OpUserDisconnected, OpUserStateChanged:
		ev.User = d.user()
	case OpUserTextMessage:
		ev.User = d.user()
		d.textMessage()
	case OpChannelCreated, OpChannelRemoved, OpChannelStateChanged:
		ev.Channel = d.channel()
	default:
		return Event{}, fmt.Errorf("unknown operation %q", op)
	}
	if err := d.end(); err != nil {
		return Event{}, protoErr(op, err)
	}
	return ev, nil
}
