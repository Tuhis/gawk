package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// SessionClosing (R57, docs/59): relay→client, the close code of a session
// the relay is about to close, sent on its own server-opened unidirectional
// stream a settle interval before the WebTransport close itself.
//
// It exists because the close code does not reach Chrome. webtransport-go
// writes the WT_CLOSE_SESSION capsule and then STOP_SENDINGs the CONNECT
// stream, quic-go packs that control frame ahead of the capsule in the same
// packet, and Chrome fails the session on it before it reads the capsule:
// `WebTransport.closed` rejects "Connection lost." with no code (measured
// 2026-09-24: 1 close code of 84 reached Chrome; quic-go/webtransport-go#242,
// closed upstream as a Chromium bug). The in-band copy is what a browser
// acts on when the close arrives without one; a client that did read the
// close code keeps using it.
//
// Sent only for the codes that change what a client does next — 4000 to a
// viewer, 4004 and 4006 to a publisher or viewer — and only on external
// publish/subscribe sessions: never on /internal/* (the edge client reads
// every uni stream as a keyframe), and not on room control sessions, whose
// RoomEnding event already carries 4007's meaning in-band (docs/44 §4.6).
//
// Layout (SessionClosingSize bytes):
//
//	byte 0    uint8  version (Version)
//	byte 1    uint8  type (TypeSessionClosing)
//	bytes 2-5 uint32 code, big-endian, in the gawk application range
//	                 [MinGawkCloseCode, MaxGawkCloseCode]
//
// Clients parse it and never send it. Strict: exactly SessionClosingSize
// bytes. A client that does not know the type ignores the stream, like
// every other unknown server message (the native broadcasters already do).
const (
	// TypeSessionClosing identifies a SessionClosing message. Allocated
	// 2026-09-24 after the room control types 0x13–0x16.
	TypeSessionClosing = 0x17
	// SessionClosingSize is the exact size of a SessionClosing message.
	SessionClosingSize = 6
	// MinGawkCloseCode / MaxGawkCloseCode bound the application close codes
	// gawk allocates (4000 CloseCodeBroadcastEnded upward).
	MinGawkCloseCode = 4000
	MaxGawkCloseCode = 4999
)

// ErrBadSessionClosing indicates a SessionClosing message of the wrong size
// or carrying a code outside the gawk range.
var ErrBadSessionClosing = errors.New("wire: invalid session closing")

// AppendSessionClosing appends a SessionClosing message for code to dst and
// returns the extended slice.
func AppendSessionClosing(dst []byte, code uint32) ([]byte, error) {
	if code < MinGawkCloseCode || code > MaxGawkCloseCode {
		return nil, fmt.Errorf("%w: code %d outside %d-%d", ErrBadSessionClosing, code, MinGawkCloseCode, MaxGawkCloseCode)
	}
	dst = append(dst, Version, TypeSessionClosing)
	return binary.BigEndian.AppendUint32(dst, code), nil
}

// ParseSessionClosing parses a SessionClosing message and returns its code.
func ParseSessionClosing(msg []byte) (uint32, error) {
	if len(msg) < SessionClosingSize {
		return 0, fmt.Errorf("%w: %d bytes, need %d for session closing", ErrShortDatagram, len(msg), SessionClosingSize)
	}
	if msg[0] != Version {
		return 0, fmt.Errorf("%w: 0x%02x", ErrBadVersion, msg[0])
	}
	if msg[1] != TypeSessionClosing {
		return 0, fmt.Errorf("%w: got 0x%02x, want session closing 0x%02x", ErrBadType, msg[1], TypeSessionClosing)
	}
	if len(msg) != SessionClosingSize {
		return 0, fmt.Errorf("%w: %d bytes, want %d", ErrBadSessionClosing, len(msg), SessionClosingSize)
	}
	code := binary.BigEndian.Uint32(msg[2:])
	if code < MinGawkCloseCode || code > MaxGawkCloseCode {
		return 0, fmt.Errorf("%w: code %d outside %d-%d", ErrBadSessionClosing, code, MinGawkCloseCode, MaxGawkCloseCode)
	}
	return code, nil
}
