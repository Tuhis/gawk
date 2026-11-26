// Package proxyproto decodes the PROXY protocol version 2 header that
// gawk-mumble-bridge sends before each voice user's Mumble connection
// (docs/92 D2). It implements the decoder half only; the bridge has its own
// encoder, and the two are tested against each other and against the
// specification's byte layouts.
//
// It accepts exactly one shape: the PROXY command, TCP over IPv4 or TCP over
// IPv6, with any TLVs skipped. Everything else is refused, and so is a source
// address the proxy must never bind (unspecified, multicast, broadcast).
//
// The header is read with exact-length reads straight off the connection, so
// no byte after it is ever consumed: whatever follows (the bridge's TLS
// ClientHello) is still in the socket for the splice to murmur.
//
// Nothing here logs. Errors never carry the address (docs/92 D10).
package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
)

// Signature is the 12-byte magic every v2 header starts with.
var Signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// MaxBodyLen bounds the address-and-TLV block the decoder accepts. The
// bridge sends 12 or 36 bytes; the cap keeps a hostile peer from making the
// proxy allocate the protocol's 64 KiB maximum.
const MaxBodyLen = 1024

const (
	cmdLocal = 0x0
	cmdProxy = 0x1

	famTCP4 = 0x11
	famTCP6 = 0x21
)

// Errors, each a distinct refusal (the proxy maps them to result labels).
var (
	// ErrNotV2 is anything without the v2 signature, a v1 text header
	// included.
	ErrNotV2 = errors.New("proxyproto: not a PROXY protocol v2 header")
	// ErrVersion is the signature with a version other than 2.
	ErrVersion = errors.New("proxyproto: unsupported version")
	// ErrCommand is LOCAL or an unknown command: the bridge always sends
	// PROXY.
	ErrCommand = errors.New("proxyproto: command is not PROXY")
	// ErrFamily is UNSPEC, UDP, UNIX or an unknown family.
	ErrFamily = errors.New("proxyproto: family is not TCP over IPv4 or IPv6")
	// ErrLength is a body too short for its family, or over MaxBodyLen.
	ErrLength = errors.New("proxyproto: bad header length")
	// ErrSource is a source address the proxy refuses to bind.
	ErrSource = errors.New("proxyproto: refused source address")
)

// Header is what the proxy uses from a decoded header.
type Header struct {
	// Source is the client's address, unmapped (an IPv4 address carried
	// in an IPv6 header arrives as IPv4).
	Source netip.AddrPort
}

// Read decodes one v2 header from r. It never reads past the header. The
// caller sets the deadline; an I/O error (a timeout included) is returned
// as is, wrapped in nothing.
func Read(r io.Reader) (Header, error) {
	var fixed [16]byte
	if _, err := io.ReadFull(r, fixed[:12]); err != nil {
		return Header{}, err
	}
	if !bytes.Equal(fixed[:12], Signature) {
		return Header{}, ErrNotV2
	}
	if _, err := io.ReadFull(r, fixed[12:]); err != nil {
		return Header{}, err
	}
	if fixed[12]>>4 != 2 {
		return Header{}, ErrVersion
	}
	n := int(binary.BigEndian.Uint16(fixed[14:16]))
	if n > MaxBodyLen {
		return Header{}, ErrLength
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Header{}, err
	}
	return parse(fixed[12]&0x0f, fixed[13], body)
}

func parse(cmd, fam byte, body []byte) (Header, error) {
	if cmd != cmdProxy {
		// LOCAL included: "connect as yourself" is never what the bridge
		// means, and honouring it would let murmur see the proxy's own
		// loopback address for a person.
		return Header{}, ErrCommand
	}
	var src netip.Addr
	var port uint16
	switch fam {
	case famTCP4:
		if len(body) < 12 {
			return Header{}, ErrLength
		}
		src = netip.AddrFrom4([4]byte(body[0:4]))
		port = binary.BigEndian.Uint16(body[8:10])
	case famTCP6:
		if len(body) < 36 {
			return Header{}, ErrLength
		}
		src = netip.AddrFrom16([16]byte(body[0:16])).Unmap()
		port = binary.BigEndian.Uint16(body[32:34])
	default:
		return Header{}, ErrFamily
	}
	// Whatever follows the addresses is TLVs, skipped (D2).
	if !Bindable(src) {
		return Header{}, ErrSource
	}
	return Header{Source: netip.AddrPortFrom(src, port)}, nil
}

// Bindable reports whether the proxy may spoof a as a source address: not
// unspecified, multicast or the IPv4 broadcast address. Loopback is allowed:
// a loopback source is simply a local connection (the e2e uses it).
func Bindable(a netip.Addr) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.IsMulticast():
		return false
	case a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return false
	}
	return true
}
