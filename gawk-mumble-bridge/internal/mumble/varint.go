package mumble

import (
	"encoding/binary"
	"errors"
)

// Mumble's own variable-length integer (the "PacketDataStream" varint of
// the legacy voice packet, docs: mumble-protocol "Variable length integer
// encoding"). It is NOT the protobuf varint: the length is in the leading
// bits of the first byte and the value is big-endian.
//
//	0xxxxxxx                       7-bit positive number
//	10xxxxxx + 1 byte              14-bit positive number
//	110xxxxx + 2 bytes             21-bit positive number
//	1110xxxx + 3 bytes             28-bit positive number
//	111100__ + int (32-bit)        32-bit positive number
//	111101__ + long (64-bit)       64-bit number
//	111110__ + varint              negative recursive varint (~varint)
//	111111xx                       byte-inverted negative two-bit number (~xx)
//
// Encoding follows the reference implementation (PacketDataStream
// operator<<(quint64)): a negative value whose complement fits 32 bits is
// written as its complement, either inline (-1..-4) or after 0xF8; any other
// value as the shortest positive form.

// ErrVarint is returned for a truncated varint.
var ErrVarint = errors.New("mumble: truncated varint")

// AppendVarint appends v in Mumble's varint encoding.
func AppendVarint(dst []byte, v int64) []byte {
	i := uint64(v)
	if v < 0 && ^i < 0x100000000 {
		i = ^i
		if i <= 0x3 {
			return append(dst, 0xFC|byte(i))
		}
		dst = append(dst, 0xF8)
	}
	switch {
	case i < 0x80:
		return append(dst, byte(i))
	case i < 0x4000:
		return append(dst, 0x80|byte(i>>8), byte(i))
	case i < 0x200000:
		return append(dst, 0xC0|byte(i>>16), byte(i>>8), byte(i))
	case i < 0x10000000:
		return append(dst, 0xE0|byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	case i < 0x100000000:
		dst = append(dst, 0xF0)
		return binary.BigEndian.AppendUint32(dst, uint32(i))
	default:
		dst = append(dst, 0xF4)
		return binary.BigEndian.AppendUint64(dst, i)
	}
}

// ReadVarint decodes one varint from b and returns it with the number of
// bytes it used.
func ReadVarint(b []byte) (int64, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrVarint
	}
	v := b[0]
	need := func(n int) error {
		if len(b) < n {
			return ErrVarint
		}
		return nil
	}
	switch {
	case v&0x80 == 0x00:
		return int64(v & 0x7F), 1, nil
	case v&0xC0 == 0x80:
		if err := need(2); err != nil {
			return 0, 0, err
		}
		return int64(v&0x3F)<<8 | int64(b[1]), 2, nil
	case v&0xE0 == 0xC0:
		if err := need(3); err != nil {
			return 0, 0, err
		}
		return int64(v&0x1F)<<16 | int64(b[1])<<8 | int64(b[2]), 3, nil
	case v&0xF0 == 0xE0:
		if err := need(4); err != nil {
			return 0, 0, err
		}
		return int64(v&0x0F)<<24 | int64(b[1])<<16 | int64(b[2])<<8 | int64(b[3]), 4, nil
	}
	switch v & 0xFC {
	case 0xF0:
		if err := need(5); err != nil {
			return 0, 0, err
		}
		return int64(binary.BigEndian.Uint32(b[1:5])), 5, nil
	case 0xF4:
		if err := need(9); err != nil {
			return 0, 0, err
		}
		return int64(binary.BigEndian.Uint64(b[1:9])), 9, nil
	case 0xF8:
		inner, n, err := ReadVarint(b[1:])
		if err != nil {
			return 0, 0, err
		}
		return ^inner, 1 + n, nil
	default: // 0xFC
		return ^int64(v & 0x03), 1, nil
	}
}
