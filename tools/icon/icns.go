package main

// The Apple icon container the macOS bundle names in CFBundleIconFile. The
// Dock and Finder never read a window's icon, so this file is the only way
// the mark reaches them (docs/54 D14).
//
// The layout is a big-endian "icns" + total length, then elements of a
// four-character type + element length (header included) + payload. Every
// element here carries a PNG, which macOS has accepted for all of these
// types since 10.7 and which is what iconutil writes; the legacy RGB+mask
// types are not produced. As with the .ico, the reader exists for the drift
// check and the tests, so the writer is never its own witness.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"slices"
)

// icnsTypes is every element the file carries, in the order written: each
// point size at 1x, then 2x, the same set iconutil makes from a full
// .iconset. A 2x type repeats the pixels of the next size up's 1x type,
// because macOS picks the element by type, not by pixel size.
var icnsTypes = []struct {
	Type string
	Size int
}{
	{"icp4", 16},   // 16 pt
	{"ic11", 32},   // 16 pt @2x
	{"icp5", 32},   // 32 pt
	{"ic12", 64},   // 32 pt @2x
	{"ic07", 128},  // 128 pt
	{"ic13", 256},  // 128 pt @2x
	{"ic08", 256},  // 256 pt
	{"ic14", 512},  // 256 pt @2x
	{"ic09", 512},  // 512 pt
	{"ic10", 1024}, // 512 pt @2x — the Retina Dock
}

// ICNSSizes is the distinct pixel sizes icnsTypes needs. 512 and 1024 are
// beyond the shared Sizes set (docs/53 D9) and are rendered for the .icns
// only; they are not committed as PNGs.
var ICNSSizes = func() []int {
	var out []int
	for _, e := range icnsTypes {
		if !slices.Contains(out, e.Size) {
			out = append(out, e.Size)
		}
	}
	slices.Sort(out)
	return out
}()

func icnsSize(typ string) (int, bool) {
	for _, e := range icnsTypes {
		if e.Type == typ {
			return e.Size, true
		}
	}
	return 0, false
}

// WriteICNS assembles the container from one image per ICNSSizes entry.
func WriteICNS(images map[int]*image.NRGBA) ([]byte, error) {
	encoded := map[int][]byte{}
	for _, s := range ICNSSizes {
		img, ok := images[s]
		if !ok {
			return nil, fmt.Errorf("icns: no %d px image", s)
		}
		if b := img.Bounds(); b.Dx() != s || b.Dy() != s {
			return nil, fmt.Errorf("icns: the %d px image is %dx%d", s, b.Dx(), b.Dy())
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		encoded[s] = buf.Bytes()
	}
	var body bytes.Buffer
	for _, e := range icnsTypes {
		data := encoded[e.Size]
		var hdr [8]byte
		copy(hdr[:], e.Type)
		binary.BigEndian.PutUint32(hdr[4:], uint32(8+len(data)))
		body.Write(hdr[:])
		body.Write(data)
	}
	out := make([]byte, 8, 8+body.Len())
	copy(out, "icns")
	binary.BigEndian.PutUint32(out[4:], uint32(8+body.Len()))
	return append(out, body.Bytes()...), nil
}

// ParseICNS reads a container back, keyed by element type. It is strict in
// the same way ParseICO is: an unknown or repeated type, a length that does
// not add up, a non-PNG payload or a payload of the wrong size is an error.
func ParseICNS(data []byte) (map[string]*image.NRGBA, error) {
	if len(data) < 8 || string(data[:4]) != "icns" {
		return nil, fmt.Errorf("icns: not an icns container")
	}
	if n := binary.BigEndian.Uint32(data[4:]); uint64(n) != uint64(len(data)) {
		return nil, fmt.Errorf("icns: header says %d bytes, file is %d", n, len(data))
	}
	out := map[string]*image.NRGBA{}
	for p := 8; p < len(data); {
		if len(data)-p < 8 {
			return nil, fmt.Errorf("icns: element header at %d truncated", p)
		}
		typ := string(data[p : p+4])
		n := uint64(binary.BigEndian.Uint32(data[p+4:]))
		if n < 8 || uint64(p)+n > uint64(len(data)) {
			return nil, fmt.Errorf("icns: %s: length %d does not fit", typ, n)
		}
		want, ok := icnsSize(typ)
		if !ok {
			return nil, fmt.Errorf("icns: unexpected element type %q", typ)
		}
		if _, dup := out[typ]; dup {
			return nil, fmt.Errorf("icns: %s appears twice", typ)
		}
		payload := data[p+8 : p+int(n)]
		if !bytes.HasPrefix(payload, []byte("\x89PNG")) {
			return nil, fmt.Errorf("icns: %s is not PNG", typ)
		}
		img, err := png.Decode(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("icns: %s: %w", typ, err)
		}
		if b := img.Bounds(); b.Dx() != want || b.Dy() != want {
			return nil, fmt.Errorf("icns: %s holds %dx%d, want %d px", typ, b.Dx(), b.Dy(), want)
		}
		out[typ] = toNRGBA(img)
		p += int(n)
	}
	return out, nil
}
