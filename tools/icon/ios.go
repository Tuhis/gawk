package main

// The iOS app's icon (R68 K13, docs/70). iOS reads it from an asset catalog
// compiled into the app, so this writes the catalogs themselves, PNGs and
// Contents.json, under assets/icon/ios/, and gawk-ios/app/project.yml lists
// them as sources.
//
// iOS draws the icon its own way: it masks the corners itself, and it
// rejects an app icon with transparency or an alpha channel. So the iOS
// variant is full-bleed: the same document with the background tile's
// corner radius dropped, opaque to the edge and written as truecolour PNG
// without alpha. The bolt and the tile's colour are the SVG's own.
//
// Two catalogs, so that each target takes only what it uses. The Live
// Activity's widget extension (docs/70 D15) can't show an app icon set, so
// Mark.xcassets carries the same variant as a plain image set, which the
// widget draws and clips itself.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
)

// iosDir holds the catalogs, under the assets directory.
const iosDir = "ios"

// iosSet is one image set: its catalog, its directory inside the catalog,
// the PNG's pixel size and the set's one Contents.json image entry.
type iosSet struct {
	Catalog, Set string
	Size         int
	Image        xcassetsImage
}

func (s iosSet) pngPath() string {
	return filepath.Join(iosDir, s.Catalog, s.Set, s.Image.Filename)
}

// iosSets is every image set the catalogs hold, one catalog each.
var iosSets = []iosSet{
	// The single-size app icon: Xcode derives every size iOS needs from
	// the one 1024 px image.
	{"AppIcon.xcassets", "AppIcon.appiconset", 1024,
		xcassetsImage{Filename: "AppIcon.png", Idiom: "universal", Platform: "ios", Size: "1024x1024"}},
	// The Live Activity's mark, single scale.
	{"Mark.xcassets", "Mark.imageset", 120,
		xcassetsImage{Filename: "Mark.png", Idiom: "universal"}},
}

// xcassets is the part of an asset catalog's Contents.json these catalogs
// use. Field order is key order, so the bytes are stable.
type xcassets struct {
	Images []xcassetsImage `json:"images,omitempty"`
	Info   xcassetsInfo    `json:"info"`
}

type xcassetsImage struct {
	Filename string `json:"filename"`
	Idiom    string `json:"idiom"`
	Platform string `json:"platform,omitempty"`
	Size     string `json:"size,omitempty"`
}

type xcassetsInfo struct {
	Author  string `json:"author"`
	Version int    `json:"version"`
}

// xcodeInfo is the info block Xcode itself writes.
var xcodeInfo = xcassetsInfo{Author: "xcode", Version: 1}

// textFile is one generated file, by path under the assets directory.
type textFile struct {
	Path string
	Data []byte
}

// iosJSON is every Contents.json the catalogs carry, as generate writes
// them and as check expects them, byte for byte.
func iosJSON() ([]textFile, error) {
	var out []textFile
	for _, s := range iosSets {
		for _, f := range []struct {
			path string
			c    xcassets
		}{
			{filepath.Join(iosDir, s.Catalog, "Contents.json"), xcassets{Info: xcodeInfo}},
			{filepath.Join(iosDir, s.Catalog, s.Set, "Contents.json"), xcassets{Images: []xcassetsImage{s.Image}, Info: xcodeInfo}},
		} {
			data, err := json.MarshalIndent(f.c, "", "  ")
			if err != nil {
				return nil, err
			}
			out = append(out, textFile{f.path, append(data, '\n')})
		}
	}
	return out, nil
}

// FullBleed is doc with the background tile's corners squared off. The tile
// is the first shape, a <rect> covering the whole viewBox; it is redrawn as
// a plain rectangle over the same box in the same fill, and every other
// shape is left as it is.
func FullBleed(doc *Document) (*Document, error) {
	vb := doc.ViewBox
	if len(doc.Shapes) == 0 {
		return nil, fmt.Errorf("full-bleed: no shapes")
	}
	t := doc.Shapes[0].Rect
	if t == nil || t.MinX > vb.MinX || t.MinY > vb.MinY || t.MaxX < vb.MinX+vb.W || t.MaxY < vb.MinY+vb.H {
		return nil, fmt.Errorf("full-bleed: the first shape is not a <rect> covering the whole viewBox, so there is no tile to square off")
	}
	out := *doc
	out.Shapes = slices.Clone(doc.Shapes)
	out.Shapes[0].Ops = rectOps(identity(), t.MinX, t.MinY, t.MaxX-t.MinX, t.MaxY-t.MinY, 0)
	return &out, nil
}

// renderIOS renders the full-bleed variant at every size iosSets needs. A
// render with any translucent pixel is an error, not a warning: png.Encode
// writes an alpha channel for it, and iOS rejects that.
func renderIOS(dir string) (map[int]*image.NRGBA, error) {
	doc, err := loadSource(dir)
	if err != nil {
		return nil, err
	}
	fb, err := FullBleed(doc)
	if err != nil {
		return nil, err
	}
	out := map[int]*image.NRGBA{}
	for _, s := range iosSets {
		img, err := Render(fb, s.Size)
		if err != nil {
			return nil, err
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if a := img.NRGBAAt(x, y).A; a != 255 {
					return nil, fmt.Errorf("ios: the %d px full-bleed render has alpha %d at (%d,%d)", s.Size, a, x, y)
				}
			}
		}
		out[s.Size] = img
	}
	return out, nil
}

// generateIOS writes both catalogs.
func generateIOS(dir string) error {
	images, err := renderIOS(dir)
	if err != nil {
		return err
	}
	for _, s := range iosSets {
		var buf bytes.Buffer
		if err := png.Encode(&buf, images[s.Size]); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, s.pngPath()), buf.Bytes()); err != nil {
			return err
		}
	}
	files, err := iosJSON()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := writeFile(filepath.Join(dir, f.Path), f.Data); err != nil {
			return err
		}
	}
	return nil
}

// checkIOS compares the committed catalogs with a fresh render: each PNG's
// pixels and the absence of an alpha channel, and each Contents.json byte
// for byte.
func checkIOS(dir string) error {
	images, err := renderIOS(dir)
	if err != nil {
		return err
	}
	for _, s := range iosSets {
		path := filepath.Join(dir, s.pngPath())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		ct, err := pngColorType(data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if ct&pngColorAlpha != 0 {
			return fmt.Errorf("%s: has an alpha channel (PNG colour type %d), which iOS rejects — regenerate with `go run ./tools/icon generate`", path, ct)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := compare(toNRGBA(img), images[s.Size]); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	files, err := iosJSON()
	if err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, f.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, f.Data) {
			return fmt.Errorf("%s: differs from what generate writes — regenerate with `go run ./tools/icon generate`", path)
		}
	}
	return nil
}

// pngColorAlpha is the colour-type bit that means an alpha channel: types
// 4 (grey + alpha) and 6 (truecolour + alpha).
const pngColorAlpha = 4

// pngColorType is the colour type in a PNG's IHDR, which the format
// requires to be the first chunk.
func pngColorType(data []byte) (byte, error) {
	// Signature (8), IHDR length (4) and type (4), width (4), height (4),
	// bit depth (1), then the colour type.
	if len(data) < 26 || string(data[:8]) != "\x89PNG\r\n\x1a\n" || string(data[12:16]) != "IHDR" {
		return 0, fmt.Errorf("not a PNG")
	}
	return data[25], nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
