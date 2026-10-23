package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// R74 PoP relays (docs/76 §9): two relay→client messages, each on its own
// server-opened unidirectional stream right after the upgrade on /echo,
// /publish, /subscribe and /room sessions. The browser WebTransport API
// exposes no response headers (the 0x11/0x12 precedent), so both are
// in-band. Clients parse them and never send them.
//
// Deliberate deviation from house parser strictness, as for RelayIdentity:
// TRAILING BYTES ARE TOLERATED — appended fields are how these messages
// grow — while the flags byte stays strict (reserved-must-be-zero). Bits in
// LocalRelayInfo's state byte are tolerant: a reader tests the bits it
// knows and ignores the rest, so a later state bit never makes the message
// unparseable for an older client. (docs/76 §9's "flags (linked, onWall)"
// are realised as that state byte, not as strict flag bits.)
//
// Neither is ever sent on /internal/* or /pop/*: an edge or PoP pump reads
// those sessions' uni streams as media.
const (
	// TypeLocalRelayInfo identifies a LocalRelayInfo message: PoP → client,
	// the PoP's display name, link and party-wall state, the home app
	// origin and the wall code (docs/76 item 45). Allocated 2026-10-22
	// after SessionClosing (0x17).
	TypeLocalRelayInfo = 0x18
	// TypeLocalRelayOffer identifies a LocalRelayOffer message: home →
	// client, a suggestion to switch to a PoP relay on the client's own
	// network (docs/76 item 42). Allocated 2026-10-22.
	TypeLocalRelayOffer = 0x19
)

// LocalRelayInfo state bits (byte 3).
const (
	// LocalRelayLinked is set when the PoP is paired with and linked to its
	// home fleet.
	LocalRelayLinked uint8 = 0x01
	// LocalRelayOnWall is set on a publish session whose broadcast is shown
	// on the party wall.
	LocalRelayOnWall uint8 = 0x02
)

// Size bounds for the R74 messages.
const (
	// LocalRelayInfoMinSize is the shortest LocalRelayInfo: the four header
	// bytes and the three (empty) length-prefixed fields.
	LocalRelayInfoMinSize = 7
	// MaxLocalRelayOriginLen bounds LocalRelayInfo's home app origin.
	MaxLocalRelayOriginLen = 128
	// MaxLocalRelayHandleLen bounds LocalRelayOffer's opaque PoP handle (a
	// Pop CR's metadata.uid is a 36-byte UUID).
	MaxLocalRelayHandleLen = 64
	// MaxLocalRelayURLLen bounds one LocalRelayOffer candidate URL.
	MaxLocalRelayURLLen = 256
	// MaxLocalRelayCandidates bounds LocalRelayOffer's candidate count.
	MaxLocalRelayCandidates = 4
	// MaxLocalRelayHashes bounds the pinned SHA-256 certificate hashes per
	// candidate (the current and the next rolling certificate).
	MaxLocalRelayHashes = 2

	localRelayOfferMinSize = 3 + 1 + 1 + 1 + 4 + 1 // header, handle, name, validity, count
)

var (
	// ErrBadLocalRelayInfo indicates a LocalRelayInfo with a set reserved
	// flag bit, an out-of-range or overrunning field, text that is not
	// valid UTF-8, or a home origin that breaks the origin rule.
	ErrBadLocalRelayInfo = errors.New("wire: invalid local relay info")
	// ErrBadLocalRelayOffer indicates a LocalRelayOffer with a set reserved
	// flag bit, an out-of-range or overrunning field, a zero validity, or a
	// candidate URL that is not an absolute https URL without userinfo,
	// query or fragment.
	ErrBadLocalRelayOffer = errors.New("wire: invalid local relay offer")
)

// LocalRelayInfo (0x18) layout:
//
//	byte 0    uint8 version (Version)
//	byte 1    uint8 type (TypeLocalRelayInfo)
//	byte 2    uint8 flags, reserved, must be 0
//	byte 3    uint8 state: LocalRelayLinked | LocalRelayOnWall; unknown bits ignored
//	next      uint8 nameLen (0–MaxRelayIdentityNameLen), then UTF-8 display name
//	next      uint8 originLen (0–MaxLocalRelayOriginLen), then the home app
//	          origin: https://host[:port], or http://localhost[:port] /
//	          http://127.0.0.1[:port] for development; empty when unlinked
//	next      uint8 wallLen (0–MaxRoomCodeLen), then the wall's room code
//	rest      ignored (extension space)
//
// All three length bytes are always written, so later phases fill values
// without changing the layout.
type LocalRelayInfo struct {
	// Linked and OnWall are the state bits.
	Linked, OnWall bool
	// Name is the PoP's display name.
	Name string
	// HomeOrigin is the home fleet's app origin, empty when unlinked.
	HomeOrigin string
	// WallCode is the party wall's room code, empty when there is none.
	WallCode string
}

// AppendLocalRelayInfo appends a LocalRelayInfo message encoding m to dst.
func AppendLocalRelayInfo(dst []byte, m LocalRelayInfo) ([]byte, error) {
	if err := validateLocalRelayOrigin(m.HomeOrigin); err != nil {
		return nil, err
	}
	var state uint8
	if m.Linked {
		state |= LocalRelayLinked
	}
	if m.OnWall {
		state |= LocalRelayOnWall
	}
	out := append(dst, Version, TypeLocalRelayInfo, 0, state)
	var err error
	if out, err = appendStr8(out, m.Name, MaxRelayIdentityNameLen, ErrBadLocalRelayInfo, "name"); err != nil {
		return nil, err
	}
	if out, err = appendStr8(out, m.HomeOrigin, MaxLocalRelayOriginLen, ErrBadLocalRelayInfo, "home origin"); err != nil {
		return nil, err
	}
	if out, err = appendStr8(out, m.WallCode, MaxRoomCodeLen, ErrBadLocalRelayInfo, "wall code"); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseLocalRelayInfo parses a LocalRelayInfo message. Trailing bytes and
// unknown state bits are ignored; the flags byte is strict.
func ParseLocalRelayInfo(msg []byte) (LocalRelayInfo, error) {
	if err := checkPrefix(msg, TypeLocalRelayInfo, LocalRelayInfoMinSize, "local relay info"); err != nil {
		return LocalRelayInfo{}, err
	}
	r := &roomReader{buf: msg, off: 2}
	if flags := r.u8(ErrBadLocalRelayInfo); flags != 0 {
		return LocalRelayInfo{}, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadLocalRelayInfo, flags)
	}
	state := r.u8(ErrBadLocalRelayInfo)
	m := LocalRelayInfo{
		Linked: state&LocalRelayLinked != 0,
		OnWall: state&LocalRelayOnWall != 0,
	}
	m.Name = r.str8(ErrBadLocalRelayInfo, "name", MaxRelayIdentityNameLen)
	m.HomeOrigin = r.str8(ErrBadLocalRelayInfo, "home origin", MaxLocalRelayOriginLen)
	m.WallCode = r.str8(ErrBadLocalRelayInfo, "wall code", MaxRoomCodeLen)
	// No r.done(): bytes past the wall code are the extension space.
	if r.err != nil {
		return LocalRelayInfo{}, r.err
	}
	if err := validateLocalRelayOrigin(m.HomeOrigin); err != nil {
		return LocalRelayInfo{}, err
	}
	return m, nil
}

// validateLocalRelayOrigin is the home-origin rule, applied on append and on
// parse: empty, or printable ASCII forming https://host[:port] — no
// userinfo, path, query or fragment — or one of the development origins
// http://localhost[:port] and http://127.0.0.1[:port]. It is a hand check
// (see splitLocalRelayAuthority) so the three wire copies agree exactly.
func validateLocalRelayOrigin(origin string) error {
	if origin == "" {
		return nil
	}
	if len(origin) > MaxLocalRelayOriginLen {
		return fmt.Errorf("%w: home origin %d bytes, max %d", ErrBadLocalRelayInfo, len(origin), MaxLocalRelayOriginLen)
	}
	if !urlBytesOK(origin) {
		return fmt.Errorf("%w: home origin has a non-URL byte", ErrBadLocalRelayInfo)
	}
	var auth string
	var loopbackOnly bool
	switch {
	case strings.HasPrefix(origin, "https://"):
		auth = origin[len("https://"):]
	case strings.HasPrefix(origin, "http://"):
		auth, loopbackOnly = origin[len("http://"):], true
	default:
		return fmt.Errorf("%w: home origin %q is not https or loopback http", ErrBadLocalRelayInfo, origin)
	}
	if strings.ContainsAny(auth, "/?#@") {
		return fmt.Errorf("%w: home origin %q is not a bare origin", ErrBadLocalRelayInfo, origin)
	}
	host, ok := splitLocalRelayAuthority(auth)
	if !ok {
		return fmt.Errorf("%w: home origin %q has a bad host or port", ErrBadLocalRelayInfo, origin)
	}
	if loopbackOnly && host != "localhost" && host != "127.0.0.1" {
		return fmt.Errorf("%w: home origin %q: http is allowed only for localhost and 127.0.0.1", ErrBadLocalRelayInfo, origin)
	}
	return nil
}

// LocalRelayOffer (0x19) layout:
//
//	byte 0    uint8 version (Version)
//	byte 1    uint8 type (TypeLocalRelayOffer)
//	byte 2    uint8 flags, reserved, must be 0
//	next      uint8 handleLen (1–MaxLocalRelayHandleLen), then the opaque PoP
//	          handle, printable ASCII
//	next      uint8 nameLen (0–MaxRelayIdentityNameLen), then UTF-8 display name
//	next      uint32 validity in seconds, big-endian, ≥ 1
//	next      uint8 candCount (1–MaxLocalRelayCandidates), then per candidate:
//	            uint16 urlLen (1–MaxLocalRelayURLLen), big-endian, then an
//	              absolute https URL, printable ASCII, with no userinfo,
//	              query or fragment
//	            uint8 hashCount (0–MaxLocalRelayHashes), then hashCount × 32
//	              raw SHA-256 certificate hashes
//	rest      ignored (extension space)
//
// Candidate order is the sender's preference. The message can exceed
// MaxDatagramSize; it rides a uni stream.
type LocalRelayOffer struct {
	// Handle identifies the PoP across pods, restarts and re-pairs (the Pop
	// CR's metadata.uid); clients remember opt-outs against it.
	Handle string
	// Name is the PoP's display name.
	Name string
	// Validity is how long, in seconds, the offer may be acted on.
	Validity uint32
	// Candidates are the URLs to probe, in preference order.
	Candidates []LocalRelayCandidate
}

// LocalRelayCandidate is one URL a client may probe, with the SHA-256
// certificate hashes to pin when it is an IP literal.
type LocalRelayCandidate struct {
	URL    string
	Hashes [][32]byte
}

// AppendLocalRelayOffer appends a LocalRelayOffer message encoding m to dst.
func AppendLocalRelayOffer(dst []byte, m LocalRelayOffer) ([]byte, error) {
	if err := validateLocalRelayHandle(m.Handle); err != nil {
		return nil, err
	}
	if m.Validity == 0 {
		return nil, fmt.Errorf("%w: validity 0", ErrBadLocalRelayOffer)
	}
	if n := len(m.Candidates); n == 0 || n > MaxLocalRelayCandidates {
		return nil, fmt.Errorf("%w: %d candidates, want 1-%d", ErrBadLocalRelayOffer, n, MaxLocalRelayCandidates)
	}
	for i, c := range m.Candidates {
		if err := validateLocalRelayURL(c.URL); err != nil {
			return nil, fmt.Errorf("candidate %d: %w", i, err)
		}
		if len(c.Hashes) > MaxLocalRelayHashes {
			return nil, fmt.Errorf("%w: candidate %d has %d hashes, max %d", ErrBadLocalRelayOffer, i, len(c.Hashes), MaxLocalRelayHashes)
		}
	}
	out := append(dst, Version, TypeLocalRelayOffer, 0, uint8(len(m.Handle)))
	out = append(out, m.Handle...)
	var err error
	if out, err = appendStr8(out, m.Name, MaxRelayIdentityNameLen, ErrBadLocalRelayOffer, "name"); err != nil {
		return nil, err
	}
	out = binary.BigEndian.AppendUint32(out, m.Validity)
	out = append(out, uint8(len(m.Candidates)))
	for _, c := range m.Candidates {
		out = binary.BigEndian.AppendUint16(out, uint16(len(c.URL)))
		out = append(out, c.URL...)
		out = append(out, uint8(len(c.Hashes)))
		for _, h := range c.Hashes {
			out = append(out, h[:]...)
		}
	}
	return out, nil
}

// ParseLocalRelayOffer parses a LocalRelayOffer message. Trailing bytes are
// ignored; the flags byte is strict and every declared field must fit and
// validate.
func ParseLocalRelayOffer(msg []byte) (LocalRelayOffer, error) {
	if err := checkPrefix(msg, TypeLocalRelayOffer, localRelayOfferMinSize, "local relay offer"); err != nil {
		return LocalRelayOffer{}, err
	}
	r := &roomReader{buf: msg, off: 2}
	if flags := r.u8(ErrBadLocalRelayOffer); flags != 0 {
		return LocalRelayOffer{}, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadLocalRelayOffer, flags)
	}
	var m LocalRelayOffer
	m.Handle = string(r.bytes8(ErrBadLocalRelayOffer, "handle", MaxLocalRelayHandleLen))
	if r.err != nil {
		return LocalRelayOffer{}, r.err
	}
	if err := validateLocalRelayHandle(m.Handle); err != nil {
		return LocalRelayOffer{}, err
	}
	m.Name = r.str8(ErrBadLocalRelayOffer, "name", MaxRelayIdentityNameLen)
	m.Validity = r.u32(ErrBadLocalRelayOffer)
	if r.err == nil && m.Validity == 0 {
		r.fail(ErrBadLocalRelayOffer, "validity 0")
	}
	count := int(r.u8(ErrBadLocalRelayOffer))
	if r.err == nil && (count == 0 || count > MaxLocalRelayCandidates) {
		r.fail(ErrBadLocalRelayOffer, "%d candidates, want 1-%d", count, MaxLocalRelayCandidates)
	}
	if r.err != nil {
		return LocalRelayOffer{}, r.err
	}
	m.Candidates = make([]LocalRelayCandidate, 0, count)
	for i := 0; i < count; i++ {
		n := int(r.u16(ErrBadLocalRelayOffer))
		if r.err != nil {
			return LocalRelayOffer{}, r.err
		}
		if n == 0 || n > MaxLocalRelayURLLen {
			return LocalRelayOffer{}, fmt.Errorf("%w: candidate %d urlLen %d, want 1-%d", ErrBadLocalRelayOffer, i, n, MaxLocalRelayURLLen)
		}
		if r.off+n > len(msg) {
			return LocalRelayOffer{}, fmt.Errorf("%w: candidate %d URL overruns message", ErrBadLocalRelayOffer, i)
		}
		c := LocalRelayCandidate{URL: string(msg[r.off : r.off+n])}
		r.off += n
		if err := validateLocalRelayURL(c.URL); err != nil {
			return LocalRelayOffer{}, fmt.Errorf("candidate %d: %w", i, err)
		}
		hashes := int(r.u8(ErrBadLocalRelayOffer))
		if r.err != nil {
			return LocalRelayOffer{}, r.err
		}
		if hashes > MaxLocalRelayHashes {
			return LocalRelayOffer{}, fmt.Errorf("%w: candidate %d has %d hashes, max %d", ErrBadLocalRelayOffer, i, hashes, MaxLocalRelayHashes)
		}
		if r.off+32*hashes > len(msg) {
			return LocalRelayOffer{}, fmt.Errorf("%w: candidate %d hashes overrun message", ErrBadLocalRelayOffer, i)
		}
		for j := 0; j < hashes; j++ {
			var h [32]byte
			copy(h[:], msg[r.off:r.off+32])
			r.off += 32
			c.Hashes = append(c.Hashes, h)
		}
		m.Candidates = append(m.Candidates, c)
	}
	// No r.done(): bytes past the last candidate are the extension space.
	return m, nil
}

func validateLocalRelayHandle(handle string) error {
	if len(handle) == 0 || len(handle) > MaxLocalRelayHandleLen {
		return fmt.Errorf("%w: handle %d bytes, want 1-%d", ErrBadLocalRelayOffer, len(handle), MaxLocalRelayHandleLen)
	}
	for i := 0; i < len(handle); i++ {
		if handle[i] < 0x20 || handle[i] > 0x7e {
			return fmt.Errorf("%w: non-printable handle byte 0x%02x", ErrBadLocalRelayOffer, handle[i])
		}
	}
	return nil
}

// validateLocalRelayURL is the candidate-URL rule, applied on append and on
// parse: printable ASCII, "https://", a non-empty host[:port] authority with
// no userinfo, an optional path, and no query or fragment. A hand check, the
// same in all three wire copies.
func validateLocalRelayURL(raw string) error {
	if len(raw) == 0 || len(raw) > MaxLocalRelayURLLen {
		return fmt.Errorf("%w: URL %d bytes, want 1-%d", ErrBadLocalRelayOffer, len(raw), MaxLocalRelayURLLen)
	}
	if !urlBytesOK(raw) {
		return fmt.Errorf("%w: URL has a non-URL byte", ErrBadLocalRelayOffer)
	}
	if !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("%w: URL %q is not an absolute https URL", ErrBadLocalRelayOffer, raw)
	}
	if strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("%w: URL %q carries a query or a fragment", ErrBadLocalRelayOffer, raw)
	}
	auth := raw[len("https://"):]
	if i := strings.IndexByte(auth, '/'); i >= 0 {
		auth = auth[:i]
	}
	if strings.ContainsRune(auth, '@') {
		return fmt.Errorf("%w: URL %q carries userinfo", ErrBadLocalRelayOffer, raw)
	}
	if _, ok := splitLocalRelayAuthority(auth); !ok {
		return fmt.Errorf("%w: URL %q has a bad host or port", ErrBadLocalRelayOffer, raw)
	}
	return nil
}

// urlBytesOK reports whether s is printable ASCII with no space.
func urlBytesOK(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// splitLocalRelayAuthority checks a host[:port] authority and returns the
// host. The host is non-empty, either a bracketed IPv6 literal or a run with
// no ':' or brackets; a port, when present, is 1–5 digits valued 1–65535.
func splitLocalRelayAuthority(auth string) (host string, ok bool) {
	var port string
	hasPort := false
	if strings.HasPrefix(auth, "[") {
		end := strings.IndexByte(auth, ']')
		if end < 2 {
			return "", false
		}
		host = auth[:end+1]
		if strings.ContainsRune(host[1:end], '[') {
			return "", false
		}
		rest := auth[end+1:]
		if rest != "" {
			if rest[0] != ':' {
				return "", false
			}
			port, hasPort = rest[1:], true
		}
	} else {
		host = auth
		if i := strings.IndexByte(auth, ':'); i >= 0 {
			host, port, hasPort = auth[:i], auth[i+1:], true
		}
		if host == "" || strings.ContainsAny(host, "[]") {
			return "", false
		}
	}
	if hasPort {
		if len(port) == 0 || len(port) > 5 {
			return "", false
		}
		n := 0
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return "", false
			}
			n = n*10 + int(port[i]-'0')
		}
		if n == 0 || n > 65535 {
			return "", false
		}
	}
	return host, true
}
