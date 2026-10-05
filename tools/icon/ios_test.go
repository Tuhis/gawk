package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fullBleedIcon(t *testing.T, size int) (*Document, *image.NRGBA) {
	t.Helper()
	doc := loadIconDoc(t)
	fb, err := FullBleed(doc)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Render(fb, size)
	if err != nil {
		t.Fatal(err)
	}
	return doc, img
}

func TestFullBleedSquaresOffTheTileInItsOwnColour(t *testing.T) {
	doc, img := fullBleedIcon(t, 1024)
	// The tile colour is the SVG's, read from the parsed document.
	tile := doc.Shapes[0].Fill
	want := nrgba(tile.R, tile.G, tile.B, tile.A)
	for _, p := range []image.Point{{0, 0}, {1023, 0}, {0, 1023}, {1023, 1023}} {
		if c := img.NRGBAAt(p.X, p.Y); c != want {
			t.Fatalf("corner %v is %v, want the tile's %v", p, c, want)
		}
	}
	for y := 0; y < 1024; y++ {
		for x := 0; x < 1024; x++ {
			if a := img.NRGBAAt(x, y).A; a != 255 {
				t.Fatalf("pixel (%d,%d) alpha %d, want opaque", x, y, a)
			}
		}
	}
	// FullBleed works on a copy: the document's own tile keeps its radius.
	reg, err := Render(doc, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if c := reg.NRGBAAt(0, 0); c.A != 0 {
		t.Fatalf("the source document's corner is %v after FullBleed, want transparent", c)
	}
}

func TestFullBleedDiffersFromTheIconOnlyAtTheCorners(t *testing.T) {
	// Wherever the regular render is opaque, the variant is the same pixel,
	// bolt included. Where it is translucent, which is only inside the
	// corners the radius cuts off (gawk.svg's rx is 56 of 256 units, inside
	// a quarter of the side), the variant is the tile.
	const size = 1024
	doc, full := fullBleedIcon(t, size)
	reg, err := Render(doc, size)
	if err != nil {
		t.Fatal(err)
	}
	quarter := size / 4
	corners := 0
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			r, f := reg.NRGBAAt(x, y), full.NRGBAAt(x, y)
			if r.A == 255 {
				if f != r {
					t.Fatalf("pixel (%d,%d) is %v, the icon has %v", x, y, f, r)
				}
				continue
			}
			if (x >= quarter && x < size-quarter) || (y >= quarter && y < size-quarter) {
				t.Fatalf("the icon is translucent at (%d,%d), outside the corners", x, y)
			}
			corners++
		}
	}
	if corners == 0 {
		t.Fatal("the icon has no translucent corners, so nothing was squared off")
	}
	if c := full.NRGBAAt(size/2, size/2); c != nrgba(255, 255, 255, 255) {
		t.Fatalf("bolt pixel %v", c)
	}
}

func TestFullBleedNeedsATileCoveringTheViewBox(t *testing.T) {
	for name, body := range map[string]string{
		"path first":     `<path fill="#000" d="M0 0H100V100H0z"/><rect width="100" height="100" fill="#fff"/>`,
		"tile too small": `<rect x="1" width="99" height="100" rx="10" fill="#000"/>`,
		"tile not first": `<rect width="10" height="10" fill="#000"/><rect width="100" height="100" rx="10" fill="#000"/>`,
	} {
		doc, err := ParseSVG([]byte(testSVGHead + body + `</svg>`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := FullBleed(doc); err == nil {
			t.Errorf("%s: squared off a tile that isn't there", name)
		}
	}
	// The box is taken after the transform, a mirroring scale included.
	for _, body := range []string{
		`<g transform="translate(-10 -10) scale(2)"><rect width="60" height="60" rx="10" fill="#000"/></g>`,
		`<g transform="scale(-1 1)"><rect x="-100" width="100" height="100" rx="10" fill="#000"/></g>`,
	} {
		doc, err := ParseSVG([]byte(testSVGHead + body + `</svg>`))
		if err != nil {
			t.Fatal(err)
		}
		fb, err := FullBleed(doc)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		img, err := Render(fb, 100)
		if err != nil {
			t.Fatal(err)
		}
		if c := img.NRGBAAt(0, 0); c.A != 255 {
			t.Fatalf("%s: corner %v, want opaque", body, c)
		}
	}
}

func TestTheCommittedIOSImagesAreOpaqueWithoutAnAlphaChannel(t *testing.T) {
	// iOS rejects an app icon with an alpha channel even when every pixel
	// is opaque, so the file's colour type is asserted, not just its pixels.
	for _, s := range iosSets {
		data, err := os.ReadFile(filepath.Join(iconDir(t), s.pngPath()))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", s.pngPath(), err)
		}
		// image/png reports colour type 2 (truecolour), 8-bit, as RGBA and
		// type 6 (truecolour with alpha) as NRGBA.
		if cfg.ColorModel != color.RGBAModel {
			t.Errorf("%s: colour model %T, want 8-bit truecolour without alpha", s.pngPath(), cfg.ColorModel)
		}
		if ct, err := pngColorType(data); err != nil || ct != 2 {
			t.Errorf("%s: IHDR colour type %d (%v), want 2", s.pngPath(), ct, err)
		}
		if cfg.Width != s.Size || cfg.Height != s.Size {
			t.Errorf("%s: %dx%d, want %d px", s.pngPath(), cfg.Width, cfg.Height, s.Size)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
					t.Fatalf("%s: pixel (%d,%d) is not opaque", s.pngPath(), x, y)
				}
			}
		}
	}
}

func TestTheCatalogsAreWhatXcodeExpects(t *testing.T) {
	// Read back with a generic decoder rather than the writer's own types.
	info := map[string]any{"author": "xcode", "version": 1.0}
	for path, want := range map[string]map[string]any{
		"ios/AppIcon.xcassets/Contents.json": {"info": info},
		"ios/AppIcon.xcassets/AppIcon.appiconset/Contents.json": {
			"images": []any{map[string]any{
				"filename": "AppIcon.png", "idiom": "universal", "platform": "ios", "size": "1024x1024",
			}},
			"info": info,
		},
		"ios/Mark.xcassets/Contents.json": {"info": info},
		"ios/Mark.xcassets/Mark.imageset/Contents.json": {
			"images": []any{map[string]any{"filename": "Mark.png", "idiom": "universal"}},
			"info":   info,
		},
	} {
		data, err := os.ReadFile(filepath.Join(iconDir(t), path))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", path, got, want)
		}
	}
	// An entry that names a size names the PNG's.
	for _, s := range iosSets {
		if s.Image.Size != "" && s.Image.Size != fmt.Sprintf("%dx%d", s.Size, s.Size) {
			t.Errorf("%s: size %q for a %d px image", s.Set, s.Image.Size, s.Size)
		}
	}
}

// generatedIconDir is a scratch directory holding gawk.svg and everything
// Generate writes from it on this machine. A test that alters one file in
// it sees Check fail on that file, and only that one: the committed
// derivatives are CI's renders, and another architecture's rounding of a
// translucent edge pixel can fail the check first.
func generatedIconDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join(iconDir(t), svgName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, svgName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err != nil {
		t.Fatalf("fresh output fails the check: %v", err)
	}
	return dir
}

// checkFailsOn asserts that Check fails on dir, naming the file at path
// (under dir) and, when given, the reason.
func checkFailsOn(t *testing.T, dir, path, reason string) {
	t.Helper()
	err := Check(dir)
	if err == nil {
		t.Errorf("%s: passed the check", path)
		return
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, path)) || !strings.Contains(err.Error(), reason) {
		t.Errorf("%s: %v, want an error naming the file and %q", path, err, reason)
	}
}

func TestCheckFailsOnAnAlteredAppIcon(t *testing.T) {
	doc, full := fullBleedIcon(t, 1024)
	reg, err := Render(doc, 1024)
	if err != nil {
		t.Fatal(err)
	}

	// The rounded icon flattened onto black: opaque, so no alpha channel,
	// and wrong only at the corners.
	flat := image.NewNRGBA(reg.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), reg, image.Point{}, draw.Over)
	// The right pixels within the tolerance, but one of them a hair short
	// of opaque, so png.Encode writes an alpha channel.
	alpha := image.NewNRGBA(full.Bounds())
	copy(alpha.Pix, full.Pix)
	alpha.SetNRGBA(0, 0, nrgba(full.Pix[0], full.Pix[1], full.Pix[2], 254))

	for name, c := range map[string]struct {
		img    image.Image
		reason string
	}{
		"rounded, transparent corners":  {reg, "alpha channel"},
		"rounded, flattened onto black": {flat, "pixel (0,0)"},
		"one pixel's alpha":             {alpha, "alpha channel"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := generatedIconDir(t)
			var buf bytes.Buffer
			if err := png.Encode(&buf, c.img); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, iosSets[0].pngPath()), buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			checkFailsOn(t, dir, iosSets[0].pngPath(), c.reason)
		})
	}
}

func TestCheckFailsWhenAContentsJSONDiffers(t *testing.T) {
	files, err := iosJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		// A trailing space: the same JSON to a parser, a different file to
		// the check.
		dir := generatedIconDir(t)
		if err := os.WriteFile(filepath.Join(dir, f.Path), append(bytes.Clone(f.Data), ' '), 0o644); err != nil {
			t.Fatal(err)
		}
		checkFailsOn(t, dir, f.Path, "differs")
	}
	dir := generatedIconDir(t)
	rel := filepath.Join(iosDir, iosSets[0].Catalog, iosSets[0].Set, "Contents.json")
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(data, []byte(`"1024x1024"`), []byte(`"512x512"`), 1)
	if bytes.Equal(edited, data) {
		t.Fatal("the app icon's size was not where this test expects it")
	}
	if err := os.WriteFile(filepath.Join(dir, rel), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	checkFailsOn(t, dir, rel, "differs")
}

func TestPNGColorTypeRejectsWhatIsNotAPNG(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     nil,
		"truncated": []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"),
		"not png":   bytes.Repeat([]byte{'x'}, 64),
	} {
		if _, err := pngColorType(data); err == nil {
			t.Errorf("%s: read a colour type", name)
		}
	}
}
