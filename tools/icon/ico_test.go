package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"
)

// testImages is a distinguishable image per size: a gradient with a
// transparent top-left pixel, so byte order and the AND mask both get
// exercised.
func testImages(sizes ...int) map[int]*image.NRGBA {
	out := map[int]*image.NRGBA{}
	for _, s := range sizes {
		img := image.NewNRGBA(image.Rect(0, 0, s, s))
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				img.SetNRGBA(x, y, nrgba(uint8(x*255/s), uint8(y*255/s), uint8(s), 255))
			}
		}
		img.SetNRGBA(0, 0, nrgba(0, 0, 0, 0))
		out[s] = img
	}
	return out
}

func sameImage(t *testing.T, got, want *image.NRGBA) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("bounds %v, want %v", got.Bounds(), want.Bounds())
	}
	if !bytes.Equal(got.Pix, want.Pix) {
		t.Fatalf("pixels differ")
	}
}

func TestDIBRoundTripsAndCarriesTheMask(t *testing.T) {
	img := testImages(9)[9] // 9 px wide: the mask row needs padding
	data := encodeDIB(img)
	maskStride := 4 // ceil(9/32)*4
	if len(data) != bitmapInfoHeaderSize+9*9*4+maskStride*9 {
		t.Fatalf("dib length %d", len(data))
	}
	if h := int32(binary.LittleEndian.Uint32(data[8:])); h != 18 {
		t.Fatalf("biHeight %d, want 2*9", h)
	}
	// The transparent pixel is (0,0), the top-left; rows are bottom-up so it
	// is the first pixel of the LAST mask row, and its mask bit is set.
	mask := data[bitmapInfoHeaderSize+9*9*4:]
	if mask[maskStride*8]&0x80 == 0 {
		t.Fatalf("AND mask bit for the transparent pixel is clear")
	}
	if mask[0]&0x80 != 0 {
		t.Fatalf("AND mask bit for an opaque pixel is set")
	}
	back, err := decodeDIB(data)
	if err != nil {
		t.Fatal(err)
	}
	sameImage(t, back, img)
}

func TestICOWritesSevenEntriesSmallestFirstWithPNGAt256(t *testing.T) {
	images := testImages(Sizes...)
	data, err := WriteICO(images)
	if err != nil {
		t.Fatal(err)
	}
	// Independent read of the directory, not through ParseICO.
	if n := binary.LittleEndian.Uint16(data[4:]); int(n) != len(Sizes) {
		t.Fatalf("count %d", n)
	}
	for i, s := range Sizes {
		e := data[6+16*i:]
		wantW := uint8(s % 256)
		if e[0] != wantW || e[1] != wantW {
			t.Fatalf("entry %d size bytes %d,%d want %d", i, e[0], e[1], wantW)
		}
		if binary.LittleEndian.Uint16(e[6:]) != 32 {
			t.Fatalf("entry %d bit count", i)
		}
		off := binary.LittleEndian.Uint32(e[12:])
		size := binary.LittleEndian.Uint32(e[8:])
		payload := data[off : off+size]
		if s == 256 {
			img, err := png.Decode(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("256 px entry is not PNG: %v", err)
			}
			sameImage(t, toNRGBA(img), images[256])
		} else if bytes.HasPrefix(payload, []byte("\x89PNG")) {
			t.Fatalf("%d px entry is PNG, want DIB", s)
		}
	}
	parsed, err := ParseICO(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != len(Sizes) {
		t.Fatalf("parsed %d", len(parsed))
	}
	for i, s := range Sizes {
		sameImage(t, parsed[i].Image, images[s])
		if parsed[i].IsPNG != (s == 256) {
			t.Fatalf("entry %d png flag", i)
		}
	}
}

func TestParseICORejectsGarbage(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":      {},
		"not ico":    []byte("\x89PNG\r\n\x1a\n"),
		"truncated":  {0, 0, 1, 0, 2, 0, 16, 16, 0, 0, 1, 0, 32, 0},
		"past end":   {0, 0, 1, 0, 1, 0, 16, 16, 0, 0, 1, 0, 32, 0, 0xff, 0xff, 0, 0, 22, 0, 0, 0},
		"wrong size": mislabelledICO(t),
	} {
		if _, err := ParseICO(data); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// mislabelledICO is a valid container whose directory claims 32 px for a
// 16 px image.
func mislabelledICO(t *testing.T) []byte {
	t.Helper()
	data, err := WriteICO(testImages(16))
	if err != nil {
		t.Fatal(err)
	}
	data[6] = 32
	data[7] = 32
	return data
}

func TestEncodeEntryRejectsNonSquareOrOversized(t *testing.T) {
	if _, _, err := encodeEntry(image.NewNRGBA(image.Rect(0, 0, 16, 8))); err == nil {
		t.Error("non-square accepted")
	}
	if _, _, err := encodeEntry(image.NewNRGBA(image.Rect(0, 0, 512, 512))); err == nil {
		t.Error("512 px accepted")
	}
}
