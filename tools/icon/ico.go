package main

// The Windows icon container and the 32-bit DIB entries inside it. Both are
// written and read here: the reader is what the drift check and the tests
// use, so the writer is never its own witness.
//
// Entries below 256 px are DIBs (BITMAPINFOHEADER, 32 bpp BGRA, bottom-up,
// with the legacy 1-bpp AND mask); the 256 px entry is PNG, which is what
// Vista+ expects and what keeps the file small. Explorer picks the closest
// entry to the size it draws.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"slices"
)

const (
	bitmapInfoHeaderSize = 40
	pngEntrySize         = 256
)

// icoEntry is one directory entry, in the ICO (16-byte) form. The .res group
// directory uses the same fields with a resource ID in place of the offset.
type icoEntry struct {
	Width, Height uint8
	Colors        uint8
	Reserved      uint8
	Planes        uint16
	BitCount      uint16
	Size          uint32
	Offset        uint32
}

// encodeEntry produces the payload for one size: a DIB below 256 px and a PNG
// at 256 px, plus the directory entry describing it (offset left zero).
func encodeEntry(img *image.NRGBA) (icoEntry, []byte, error) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if w != h || w <= 0 || w > 256 {
		return icoEntry{}, nil, fmt.Errorf("ico: %dx%d is not a square of at most 256 px", w, h)
	}
	var data []byte
	if w == pngEntrySize {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return icoEntry{}, nil, err
		}
		data = buf.Bytes()
	} else {
		data = encodeDIB(img)
	}
	e := icoEntry{
		Width:    uint8(w % 256), // 0 means 256
		Height:   uint8(h % 256),
		Planes:   1,
		BitCount: 32,
		Size:     uint32(len(data)),
	}
	return e, data, nil
}

// encodeDIB writes a 32-bpp icon bitmap: header, XOR (colour) rows bottom-up,
// then the AND mask rows bottom-up, each mask row padded to 4 bytes.
func encodeDIB(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	maskStride := ((w + 31) / 32) * 4
	xorSize := w * h * 4
	andSize := maskStride * h
	var buf bytes.Buffer
	hdr := [bitmapInfoHeaderSize]byte{}
	binary.LittleEndian.PutUint32(hdr[0:], bitmapInfoHeaderSize)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(w))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(h*2)) // XOR + AND
	binary.LittleEndian.PutUint16(hdr[12:], 1)
	binary.LittleEndian.PutUint16(hdr[14:], 32)
	binary.LittleEndian.PutUint32(hdr[16:], 0) // BI_RGB
	binary.LittleEndian.PutUint32(hdr[20:], uint32(xorSize+andSize))
	buf.Write(hdr[:])
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	row := make([]byte, maskStride)
	for y := h - 1; y >= 0; y-- {
		clear(row)
		for x := 0; x < w; x++ {
			if img.NRGBAAt(x, y).A == 0 {
				row[x/8] |= 0x80 >> (x % 8)
			}
		}
		buf.Write(row)
	}
	return buf.Bytes()
}

// decodeEntry inverts encodeEntry: PNG by signature, DIB otherwise.
func decodeEntry(data []byte) (*image.NRGBA, error) {
	if bytes.HasPrefix(data, []byte("\x89PNG")) {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return toNRGBA(img), nil
	}
	return decodeDIB(data)
}

func decodeDIB(data []byte) (*image.NRGBA, error) {
	if len(data) < bitmapInfoHeaderSize {
		return nil, fmt.Errorf("dib: %d bytes is shorter than a header", len(data))
	}
	if hs := binary.LittleEndian.Uint32(data[0:]); hs != bitmapInfoHeaderSize {
		return nil, fmt.Errorf("dib: header size %d, want %d", hs, bitmapInfoHeaderSize)
	}
	w := int(int32(binary.LittleEndian.Uint32(data[4:])))
	h2 := int(int32(binary.LittleEndian.Uint32(data[8:])))
	bpp := binary.LittleEndian.Uint16(data[14:])
	if w <= 0 || h2 <= 0 || h2%2 != 0 || bpp != 32 {
		return nil, fmt.Errorf("dib: %dx%d @ %d bpp is not a 32-bpp icon bitmap", w, h2, bpp)
	}
	h := h2 / 2
	if len(data) < bitmapInfoHeaderSize+w*h*4 {
		return nil, fmt.Errorf("dib: truncated pixel data")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	p := bitmapInfoHeaderSize
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, nrgba(data[p+2], data[p+1], data[p], data[p+3]))
			p += 4
		}
	}
	return img, nil
}

// WriteICO assembles the container. Entries are ordered by size, smallest
// first, because that is the order every tool lists them in.
func WriteICO(images map[int]*image.NRGBA) ([]byte, error) {
	sizes := sortedSizes(images)
	if len(sizes) == 0 {
		return nil, fmt.Errorf("ico: no images")
	}
	type enc struct {
		e    icoEntry
		data []byte
	}
	encs := make([]enc, 0, len(sizes))
	for _, s := range sizes {
		e, data, err := encodeEntry(images[s])
		if err != nil {
			return nil, err
		}
		encs = append(encs, enc{e, data})
	}
	var buf bytes.Buffer
	hdr := []byte{0, 0, 1, 0, 0, 0}
	binary.LittleEndian.PutUint16(hdr[4:], uint16(len(encs)))
	buf.Write(hdr)
	offset := uint32(6 + 16*len(encs))
	for _, en := range encs {
		en.e.Offset = offset
		writeICOEntry(&buf, en.e)
		offset += en.e.Size
	}
	for _, en := range encs {
		buf.Write(en.data)
	}
	return buf.Bytes(), nil
}

func writeICOEntry(buf *bytes.Buffer, e icoEntry) {
	var b [16]byte
	b[0], b[1], b[2], b[3] = e.Width, e.Height, e.Colors, e.Reserved
	binary.LittleEndian.PutUint16(b[4:], e.Planes)
	binary.LittleEndian.PutUint16(b[6:], e.BitCount)
	binary.LittleEndian.PutUint32(b[8:], e.Size)
	binary.LittleEndian.PutUint32(b[12:], e.Offset)
	buf.Write(b[:])
}

func readICOEntry(b []byte) icoEntry {
	return icoEntry{
		Width: b[0], Height: b[1], Colors: b[2], Reserved: b[3],
		Planes:   binary.LittleEndian.Uint16(b[4:]),
		BitCount: binary.LittleEndian.Uint16(b[6:]),
		Size:     binary.LittleEndian.Uint32(b[8:]),
		Offset:   binary.LittleEndian.Uint32(b[12:]),
	}
}

// ICOImage is one decoded container entry.
type ICOImage struct {
	Entry icoEntry
	IsPNG bool
	Image *image.NRGBA
}

// ParseICO reads a container back. It is deliberately strict: the drift
// check relies on it noticing anything the writer would not have produced.
func ParseICO(data []byte) ([]ICOImage, error) {
	if len(data) < 6 || data[0] != 0 || data[1] != 0 || data[2] != 1 || data[3] != 0 {
		return nil, fmt.Errorf("ico: not an icon container")
	}
	n := int(binary.LittleEndian.Uint16(data[4:]))
	if len(data) < 6+16*n {
		return nil, fmt.Errorf("ico: directory truncated")
	}
	out := make([]ICOImage, 0, n)
	for i := 0; i < n; i++ {
		e := readICOEntry(data[6+16*i:])
		end := uint64(e.Offset) + uint64(e.Size)
		if end > uint64(len(data)) {
			return nil, fmt.Errorf("ico: entry %d points past the end", i)
		}
		payload := data[e.Offset:end]
		img, err := decodeEntry(payload)
		if err != nil {
			return nil, fmt.Errorf("ico: entry %d: %w", i, err)
		}
		want := int(e.Width)
		if want == 0 {
			want = 256
		}
		if img.Bounds().Dx() != want || img.Bounds().Dy() != want {
			return nil, fmt.Errorf("ico: entry %d says %d px but holds %dx%d", i, want, img.Bounds().Dx(), img.Bounds().Dy())
		}
		out = append(out, ICOImage{Entry: e, IsPNG: bytes.HasPrefix(payload, []byte("\x89PNG")), Image: img})
	}
	return out, nil
}

func sortedSizes(images map[int]*image.NRGBA) []int {
	sizes := make([]int, 0, len(images))
	for s := range images {
		sizes = append(sizes, s)
	}
	slices.Sort(sizes)
	return sizes
}

func nrgba(r, g, b, a uint8) color.NRGBA { return color.NRGBA{R: r, G: g, B: b, A: a} }

func toNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok {
		return n
	}
	b := img.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}
