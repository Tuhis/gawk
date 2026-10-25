// Package voice is the contract between the relay and gawk-mumble-bridge
// (R79, docs/80): the voice ticket the relay's home pod mints and the bridge
// verifies offline (D7), the fleet-stable temporary channel key (D11), and
// the two internal links that keep the relay the authority over voice (D2):
// the observer link (relay → bridge) and the session link (bridge → relay).
//
// It is public — outside internal/ — for the reason wire and screening are:
// a second top-level module (gawk-mumble-bridge) compiles it in through its
// replace, and a hand-written second copy of the ticket or the link messages
// is exactly what drifts. A semantic change here needs a commit touching
// gawk-mumble-bridge/ in the same PR (CONTRIBUTING.md, the release-coupling
// rule).
//
// Nothing here ever carries voice audio, and nothing here carries a
// publisher IP (D24). Room codes are raw: both links are internal, PSK-gated
// and never routed publicly; logs on either side key rooms by the keyed hash.
package voice

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// TicketTTL is how long a ticket opens a voice session after minting (D7).
// The ticket only opens the session; the session link keeps it authorised.
const TicketTTL = 30 * time.Second

// NonceSize is the byte length of a ticket nonce.
const NonceSize = 16

// ticketVersion is byte 0 of every ticket.
const ticketVersion = 1

// macSize is the HMAC-SHA256 tag length carried in a ticket (full width:
// the ticket is small and the bridge holds no state to rate-limit guesses).
const macSize = sha256.Size

// ticketDomain separates the ticket MAC from every other use of the shared
// secret (the channel key below uses its own prefix).
const ticketDomain = "gawk-voice-ticket-v1\x00"

// MinSecretLen is the shortest -voice-ticket-secret either side accepts.
const MinSecretLen = 16

// Sentinel errors.
var (
	// ErrBadTicket: malformed, wrong MAC, or not a ticket at all.
	ErrBadTicket = errors.New("voice: invalid ticket")
	// ErrTicketExpired: a well-formed ticket past its expiry.
	ErrTicketExpired = errors.New("voice: ticket expired")
)

// Ticket is what the room's home pod vouches for when it answers a
// VoiceJoin (D7). Layout, big-endian:
//
//	byte 0     version (1)
//	uint8      codeLen, then the normalized room code
//	uint16     participant ID
//	uint8      nickLen, then the gawk nickname (UTF-8, ≤ wire.MaxRoomNicknameLen)
//	16 bytes   nonce
//	uint64     epoch: the minting home's room lease generation in cluster
//	           mode, a random per-process value on a single pod
//	int64      expiry, Unix milliseconds
//	32 bytes   HMAC-SHA256(secret, ticketDomain ‖ all preceding bytes)
type Ticket struct {
	Code        string
	Participant uint16
	Nickname    string
	Nonce       [NonceSize]byte
	Epoch       uint64
	Expiry      time.Time
}

// NewNonce returns a fresh random nonce.
func NewNonce() ([NonceSize]byte, error) {
	var n [NonceSize]byte
	_, err := rand.Read(n[:])
	return n, err
}

// MintTicket encodes and signs t.
func MintTicket(secret []byte, t Ticket) ([]byte, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("%w: secret shorter than %d bytes", ErrBadTicket, MinSecretLen)
	}
	code := t.Code
	if !codeShaped(code) {
		return nil, fmt.Errorf("%w: code is not normalized", ErrBadTicket)
	}
	if len(t.Nickname) > wire.MaxRoomNicknameLen || !utf8.ValidString(t.Nickname) {
		return nil, fmt.Errorf("%w: nickname", ErrBadTicket)
	}
	b := make([]byte, 0, 1+1+len(code)+2+1+len(t.Nickname)+NonceSize+8+8+macSize)
	b = append(b, ticketVersion, uint8(len(code)))
	b = append(b, code...)
	b = binary.BigEndian.AppendUint16(b, t.Participant)
	b = append(b, uint8(len(t.Nickname)))
	b = append(b, t.Nickname...)
	b = append(b, t.Nonce[:]...)
	b = binary.BigEndian.AppendUint64(b, t.Epoch)
	b = binary.BigEndian.AppendUint64(b, uint64(t.Expiry.UnixMilli()))
	return append(b, ticketMAC(secret, b)...), nil
}

func ticketMAC(secret, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ticketDomain))
	m.Write(body)
	return m.Sum(nil)
}

// VerifyTicket checks raw's MAC under secret and its expiry against now.
// ErrBadTicket for anything malformed or forged, ErrTicketExpired for a
// genuine ticket past its expiry. Single use is NOT checked here: the home
// pod checks the nonce at registration (D5).
func VerifyTicket(secret, raw []byte, now time.Time) (Ticket, error) {
	if len(secret) < MinSecretLen {
		return Ticket{}, fmt.Errorf("%w: secret shorter than %d bytes", ErrBadTicket, MinSecretLen)
	}
	if len(raw) < 1+1+1+2+1+NonceSize+16+macSize || len(raw) > wire.MaxVoiceTicketLen {
		return Ticket{}, fmt.Errorf("%w: %d bytes", ErrBadTicket, len(raw))
	}
	body, tag := raw[:len(raw)-macSize], raw[len(raw)-macSize:]
	if !hmac.Equal(tag, ticketMAC(secret, body)) {
		return Ticket{}, fmt.Errorf("%w: bad MAC", ErrBadTicket)
	}
	// Past the MAC the bytes are our own, but parse defensively anyway.
	if body[0] != ticketVersion {
		return Ticket{}, fmt.Errorf("%w: version %d", ErrBadTicket, body[0])
	}
	off := 1
	take := func(n int) ([]byte, bool) {
		if off+n > len(body) {
			return nil, false
		}
		v := body[off : off+n]
		off += n
		return v, true
	}
	var t Ticket
	ln, ok := take(1)
	if !ok {
		return Ticket{}, ErrBadTicket
	}
	code, ok := take(int(ln[0]))
	if !ok {
		return Ticket{}, ErrBadTicket
	}
	t.Code = string(code)
	pid, ok := take(2)
	if !ok {
		return Ticket{}, ErrBadTicket
	}
	t.Participant = binary.BigEndian.Uint16(pid)
	if ln, ok = take(1); !ok {
		return Ticket{}, ErrBadTicket
	}
	nick, ok := take(int(ln[0]))
	if !ok || !utf8.Valid(nick) {
		return Ticket{}, ErrBadTicket
	}
	t.Nickname = string(nick)
	nonce, ok := take(NonceSize)
	if !ok {
		return Ticket{}, ErrBadTicket
	}
	copy(t.Nonce[:], nonce)
	tail, ok := take(16)
	if !ok || off != len(body) {
		return Ticket{}, fmt.Errorf("%w: length", ErrBadTicket)
	}
	t.Epoch = binary.BigEndian.Uint64(tail[:8])
	t.Expiry = time.UnixMilli(int64(binary.BigEndian.Uint64(tail[8:])))
	if !codeShaped(t.Code) {
		return Ticket{}, fmt.Errorf("%w: code", ErrBadTicket)
	}
	if !now.Before(t.Expiry) {
		return t, ErrTicketExpired
	}
	return t, nil
}

// codeShaped is a sanity check, not the room-code rule (that is
// rooms.NormalizeCode, which the relay applies before it mints): a
// normalized code is 1–wire.MaxRoomCodeLen bytes of [a-z0-9-]. The package
// stays free of the rooms package's Kubernetes dependencies, which the
// bridge has no use for.
func codeShaped(code string) bool {
	if code == "" || len(code) > wire.MaxRoomCodeLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// ChannelKey is the fleet-stable handle a dynamic room's temporary Mumble
// channel is named by (D11): the first six characters of the standard
// base32 encoding of HMAC-SHA256(secret, "mumble-channel:" + code), over the
// normalized code (the relay sends it normalized). Every relay and bridge
// pod holds the secret, so the name is the same whichever pod runs the
// observer, and it reveals nothing about the code.
func ChannelKey(secret []byte, code string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("mumble-channel:" + code))
	return base32.StdEncoding.EncodeToString(m.Sum(nil))[:6]
}

// DefaultTempChannelTemplate is -mumble-temp-channel-template's default
// (OD32): no room code.
const DefaultTempChannelTemplate = "gawk {key}"

// TempChannelName expands an operator template: {key} is ChannelKey,
// {code} the room's display code (only when the operator asks for it).
func TempChannelName(template string, secret []byte, code, displayCode string) string {
	if template == "" {
		template = DefaultTempChannelTemplate
	}
	out := strings.ReplaceAll(template, "{key}", ChannelKey(secret, code))
	return strings.ReplaceAll(out, "{code}", displayCode)
}

// ChannelPath splits a static room's `integrations.mumble.channel` into
// its path elements below the root ("Friends/Gaming" → [Friends Gaming]).
// Empty elements are dropped; an empty path is the root channel.
func ChannelPath(channel string) []string {
	var out []string
	for _, p := range strings.Split(channel, "/") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
