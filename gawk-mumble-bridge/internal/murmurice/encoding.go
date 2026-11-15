package murmurice

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// errShort is a value that ends before its declared length.
var errShort = errors.New("value ends early")

// encoder writes the Ice 1.1 encoding: little-endian integers, sizes as one
// byte below 255 or 255 followed by an int32, strings as size and bytes.
type encoder struct{ b []byte }

func (e *encoder) u8(v uint8) { e.b = append(e.b, v) }

func (e *encoder) boolean(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}

func (e *encoder) i16(v int16) { e.b = binary.LittleEndian.AppendUint16(e.b, uint16(v)) }
func (e *encoder) i32(v int32) { e.b = binary.LittleEndian.AppendUint32(e.b, uint32(v)) }
func (e *encoder) i64(v int64) { e.b = binary.LittleEndian.AppendUint64(e.b, uint64(v)) }
func (e *encoder) f32(v float32) {
	e.b = binary.LittleEndian.AppendUint32(e.b, math.Float32bits(v))
}

func (e *encoder) size(n int) {
	if n < 255 {
		e.u8(uint8(n))
		return
	}
	e.u8(255)
	e.i32(int32(n))
}

func (e *encoder) str(s string) {
	e.size(len(s))
	e.b = append(e.b, s...)
}

// identity is Ice's Identity struct: name, then category.
func (e *encoder) identity(id Identity) {
	e.str(id.Name)
	e.str(id.Category)
}

// encaps writes an encoding-1.1 encapsulation: its int32 size (the six
// header bytes included), the encoding version, then the payload.
func (e *encoder) encaps(payload []byte) {
	e.i32(int32(len(payload) + 6))
	e.u8(1)
	e.u8(1)
	e.b = append(e.b, payload...)
}

// decoder reads the 1.1 encoding with a sticky error: after the first
// failure every read returns zero values and err() reports it.
type decoder struct {
	b   []byte
	off int
	e   error
}

func newDecoder(b []byte) *decoder { return &decoder{b: b} }

func (d *decoder) fail(err error) {
	if d.e == nil {
		d.e = err
	}
}

func (d *decoder) err() error { return d.e }

// remaining is how many bytes are left unread.
func (d *decoder) remaining() int { return len(d.b) - d.off }

// end fails unless every byte was read: each value must use up exactly its
// bytes, so a layout change is an error rather than a wrong roster (D7).
func (d *decoder) end() error {
	if d.e == nil && d.off != len(d.b) {
		d.e = fmt.Errorf("%d bytes left over", len(d.b)-d.off)
	}
	return d.e
}

func (d *decoder) next(n int) []byte {
	if d.e != nil {
		return nil
	}
	if n < 0 || n > len(d.b)-d.off {
		d.fail(errShort)
		return nil
	}
	b := d.b[d.off : d.off+n]
	d.off += n
	return b
}

func (d *decoder) u8() uint8 {
	if b := d.next(1); b != nil {
		return b[0]
	}
	return 0
}

func (d *decoder) boolean() bool {
	switch v := d.u8(); v {
	case 0:
		return false
	case 1:
		return true
	default:
		d.fail(fmt.Errorf("bool %d", v))
		return false
	}
}

func (d *decoder) i16() int16 {
	if b := d.next(2); b != nil {
		return int16(binary.LittleEndian.Uint16(b))
	}
	return 0
}

func (d *decoder) i32() int32 {
	if b := d.next(4); b != nil {
		return int32(binary.LittleEndian.Uint32(b))
	}
	return 0
}

func (d *decoder) i64() int64 {
	if b := d.next(8); b != nil {
		return int64(binary.LittleEndian.Uint64(b))
	}
	return 0
}

func (d *decoder) f32() float32 {
	if b := d.next(4); b != nil {
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	}
	return 0
}

// size reads a size, refusing one larger than the bytes left: every element
// of a sequence takes at least one byte, so a larger count is a lie.
func (d *decoder) size() int {
	n := int(d.u8())
	if n == 255 {
		v := d.i32()
		if v < 0 {
			d.fail(fmt.Errorf("negative size %d", v))
			return 0
		}
		n = int(v)
	}
	if d.e == nil && n > d.remaining() {
		d.fail(errShort)
		return 0
	}
	return n
}

func (d *decoder) str() string { return string(d.next(d.size())) }

// skipStr reads a string's bytes and drops them, for the User fields the
// bridge must not keep (D10).
func (d *decoder) skipStr() { d.next(d.size()) }

func (d *decoder) identity() Identity {
	name := d.str()
	return Identity{Name: name, Category: d.str()}
}

// encaps reads an encapsulation and returns its payload. Only encoding 1.1
// is accepted, and the declared size must fit.
func (d *decoder) encaps() []byte {
	n := d.i32()
	major, minor := d.u8(), d.u8()
	if d.e != nil {
		return nil
	}
	if n < 6 {
		d.fail(fmt.Errorf("encapsulation size %d", n))
		return nil
	}
	if major != 1 || minor != 1 {
		d.fail(fmt.Errorf("encapsulation encoding %d.%d", major, minor))
		return nil
	}
	return d.next(int(n) - 6)
}

// Identity is an Ice object identity.
type Identity struct {
	Name     string
	Category string
}

func (id Identity) String() string {
	if id.Category == "" {
		return id.Name
	}
	return id.Category + "/" + id.Name
}
