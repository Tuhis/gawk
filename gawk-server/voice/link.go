package voice

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The two internal links (docs/80 D2). Both are WebTransport sessions
// gated by -voice-bridge-psk (the `psk` query parameter, as on the relay's
// /internal/* routes), and on both the dialer opens ONE bidirectional
// stream and the two sides exchange Messages on it, each framed as
// uint32 length (big-endian) ‖ JSON. The session ending, from either side
// and for any reason, ends what the link stands for: the observer, or the
// browser's voice session.
//
// Observer link — the room's HOME relay pod dials the bridge's internal
// listener, CONNECT ObservePath?psk=…, when a room becomes voice-live
// (D12), and redials with a backoff while it stays voice-live (D4). The
// bridge pod that accepts opens the room's observer Mumble connection.
//
//	relay  → bridge  Observe        first message: which room, which channel
//	bridge → relay   Roster         the channel's users, on every change;
//	                                Available false (with Reason) while the
//	                                observer cannot reach or enter the
//	                                channel
//
// Session link — the bridge pod that accepted a browser's voice session
// dials the relay, CONNECT SessionPath + code + ?psk=…, through the relay's
// Service; a non-home relay pod proxies it to the home (as /internal/room).
//
//	bridge → relay   Register       first message: the browser's ticket and
//	                                its source address (for gawk IP bans,
//	                                D17 — the voice client's own address,
//	                                never a publisher's)
//	relay  → bridge  Accepted       the home took it; open the Mumble user
//	bridge → relay   Joined         the bridged user is in the channel
//	bridge → relay   State          its mute/deafen state changed
//	relay  → bridge  Close          end this voice session with Code
//	bridge → relay   Left           the Mumble side ended it (a kick, a ban,
//	                                murmur gone), with the code the browser
//	                                was closed with
//
// A refused Register is a Close followed by the session close.
const (
	// ObservePath is the bridge's internal route the relay dials.
	ObservePath = "/internal/observe"
	// SessionPath is the relay's route the bridge dials, + the room code.
	SessionPath = "/internal/voice/"
	// LinkOrigin is the Origin header both link dialers send; the relay
	// already accepts it on /internal/* (internalEdgeOrigin, restated here
	// because that constant is internal; the relay test pins the two equal).
	LinkOrigin = "gawk-server://native-internal-edge"
	// MaxMessageSize bounds one link message. A roster of a few hundred
	// users is well under it.
	MaxMessageSize = 256 << 10
)

// Message types.
const (
	TypeObserve  = "observe"
	TypeRoster   = "roster"
	TypeRegister = "register"
	TypeAccepted = "accepted"
	TypeJoined   = "joined"
	TypeState    = "state"
	TypeClose    = "close"
	TypeLeft     = "left"
)

// Message is one link message. Type says which fields are meaningful.
type Message struct {
	Type string `json:"type"`

	// Observe: the room (normalized code) and its mapping.
	Code string `json:"code,omitempty"`
	// DisplayCode is the code as shown, for a {code} temp-channel template.
	DisplayCode string `json:"displayCode,omitempty"`
	// Channel is a static room's channel path ("Friends/Gaming"); empty
	// with Temporary.
	Channel string `json:"channel,omitempty"`
	// Temporary asks for the dynamic room's temporary channel (D11).
	Temporary bool `json:"temporary,omitempty"`
	// Epoch is the home's epoch for the room (the ticket's, D7). Carried
	// on Observe so a bridge can log a home move; never trusted for auth.
	Epoch uint64 `json:"epoch,omitempty"`

	// Roster: the channel's users and whether the observer is in it.
	Available bool     `json:"available,omitempty"`
	Users     []User   `json:"users,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Problems  []string `json:"problems,omitempty"`

	// Register: the raw ticket and the voice client's address.
	Ticket []byte `json:"ticket,omitempty"`
	Remote string `json:"remote,omitempty"`

	// Joined / State: the bridged user's Mumble session, name and state.
	Session uint32 `json:"session,omitempty"`
	Name    string `json:"name,omitempty"`
	Flags   uint8  `json:"flags,omitempty"`

	// Close / Left: the close code (4015, 4016, 4002) and its reason.
	CloseCode uint32 `json:"closeCode,omitempty"`
}

// User is one Mumble user in a room's channel as the observer sees it.
type User struct {
	Session uint32 `json:"session"`
	Name    string `json:"name"`
	// Flags is the wire.RoomVoiceUser* bitmap.
	Flags uint8 `json:"flags,omitempty"`
}

// ErrMessageTooLarge is returned for a frame longer than MaxMessageSize.
var ErrMessageTooLarge = errors.New("voice: link message too large")

// WriteMessage frames and writes one message.
func WriteMessage(w io.Writer, m Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > MaxMessageSize {
		return fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, len(body))
	}
	buf := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	_, err = w.Write(append(buf, body...))
	return err
}

// ReadMessage reads one framed message. The length is validated before any
// allocation.
func ReadMessage(r io.Reader) (Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Message{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxMessageSize {
		return Message{}, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Message{}, err
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return Message{}, fmt.Errorf("voice: link message: %w", err)
	}
	if m.Type == "" {
		return Message{}, errors.New("voice: link message without a type")
	}
	return m, nil
}
