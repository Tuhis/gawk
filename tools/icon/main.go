// Command icon generates and checks the gawk application icon (R44, docs/53).
//
//	go run ./tools/icon generate            # re-render assets/icon/{png,gawk.ico,gawk.res}
//	go run ./tools/icon check               # fail if the committed files drift from gawk.svg
//	go run ./tools/icon verify-exe FILE.exe # fail unless FILE.exe carries an icon resource
//
// The source is assets/icon/gawk.svg; the outputs are committed so that
// neither broadcaster's build needs an SVG rasteriser. The check compares
// decoded pixels, not bytes, so a compress/flate change between Go releases
// is not "drift" while any real change to the shape is.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
)

const (
	svgName = "gawk.svg"
	icoName = "gawk.ico"
	resName = "gawk.res"
	pngDir  = "png"
	// tolerance is the largest per-channel difference the check accepts.
	// x/image/vector uses float32 for the larger sizes and Go fuses
	// multiply-adds on arm64, so a rendering on another architecture may
	// differ from the committed one by a unit in a few edge pixels. A real
	// edit to the shape differs by hundreds.
	tolerance = 2
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "generate":
		dir := assetsDir(os.Args[2:])
		err = Generate(dir)
	case "check":
		dir := assetsDir(os.Args[2:])
		err = Check(dir)
		if err == nil {
			fmt.Printf("%s: derivatives match %s\n", dir, svgName)
		}
	case "verify-exe":
		if len(os.Args) != 3 {
			usage()
		}
		err = VerifyEXE(os.Args[2])
		if err == nil {
			fmt.Printf("%s: carries RT_GROUP_ICON and RT_ICON\n", os.Args[2])
		}
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "icon:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: icon generate [DIR] | check [DIR] | verify-exe FILE.exe")
	os.Exit(2)
}

// assetsDir is the explicit argument, else the nearest assets/icon at or
// above the working directory — so `go run ./tools/icon` works from the repo
// root and `go run .` works from tools/icon.
func assetsDir(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	if len(args) > 1 {
		usage()
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "icon:", err)
		os.Exit(1)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		cand := filepath.Join(dir, "assets", "icon")
		if _, err := os.Stat(filepath.Join(cand, svgName)); err == nil {
			return cand
		}
		if filepath.Dir(dir) == dir {
			fmt.Fprintln(os.Stderr, "icon: no assets/icon/gawk.svg at or above", wd)
			os.Exit(1)
		}
	}
}

func pngPath(dir string, size int) string {
	return filepath.Join(dir, pngDir, fmt.Sprintf("gawk-%d.png", size))
}

func renderSource(dir string) (map[int]*image.NRGBA, error) {
	data, err := os.ReadFile(filepath.Join(dir, svgName))
	if err != nil {
		return nil, err
	}
	doc, err := ParseSVG(data)
	if err != nil {
		return nil, err
	}
	return RenderAll(doc)
}

// Generate renders the SVG and writes every derivative.
func Generate(dir string) error {
	images, err := renderSource(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, pngDir), 0o755); err != nil {
		return err
	}
	for _, s := range Sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, images[s]); err != nil {
			return err
		}
		if err := os.WriteFile(pngPath(dir, s), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	ico, err := WriteICO(images)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, icoName), ico, 0o644); err != nil {
		return err
	}
	res, err := WriteRES(images)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, resName), res, 0o644)
}

// Check re-renders the SVG and compares the committed derivatives against it,
// pixel by pixel and structure by structure.
func Check(dir string) error {
	images, err := renderSource(dir)
	if err != nil {
		return err
	}
	for _, s := range Sizes {
		data, err := os.ReadFile(pngPath(dir, s))
		if err != nil {
			return err
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("%s: %w", pngPath(dir, s), err)
		}
		if err := compare(toNRGBA(img), images[s]); err != nil {
			return fmt.Errorf("%s: %w", pngPath(dir, s), err)
		}
	}

	ico, err := os.ReadFile(filepath.Join(dir, icoName))
	if err != nil {
		return err
	}
	entries, err := ParseICO(ico)
	if err != nil {
		return fmt.Errorf("%s: %w", icoName, err)
	}
	if len(entries) != len(Sizes) {
		return fmt.Errorf("%s: %d entries, want %d", icoName, len(entries), len(Sizes))
	}
	for i, s := range Sizes {
		e := entries[i]
		if e.Image.Bounds().Dx() != s {
			return fmt.Errorf("%s: entry %d is %d px, want %d", icoName, i, e.Image.Bounds().Dx(), s)
		}
		if e.IsPNG != (s == pngEntrySize) {
			return fmt.Errorf("%s: entry %d (%d px) png=%v, want %v", icoName, i, s, e.IsPNG, s == pngEntrySize)
		}
		if err := compare(e.Image, images[s]); err != nil {
			return fmt.Errorf("%s: entry %d (%d px): %w", icoName, i, s, err)
		}
	}

	res, err := os.ReadFile(filepath.Join(dir, resName))
	if err != nil {
		return err
	}
	resImages, err := ResIcons(res)
	if err != nil {
		return fmt.Errorf("%s: %w", resName, err)
	}
	if len(resImages) != len(Sizes) {
		return fmt.Errorf("%s: %d icons, want %d", resName, len(resImages), len(Sizes))
	}
	for _, s := range Sizes {
		img, ok := resImages[s]
		if !ok {
			return fmt.Errorf("%s: no %d px icon", resName, s)
		}
		if err := compare(img, images[s]); err != nil {
			return fmt.Errorf("%s: %d px: %w", resName, s, err)
		}
	}
	return nil
}

// compare reports the first pixel whose channels differ by more than the
// tolerance, or a size mismatch.
func compare(got, want *image.NRGBA) error {
	if got.Bounds() != want.Bounds() {
		return fmt.Errorf("size %v, want %v", got.Bounds().Size(), want.Bounds().Size())
	}
	b := want.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g, w := got.NRGBAAt(x, y), want.NRGBAAt(x, y)
			if absDiff(g.R, w.R) > tolerance || absDiff(g.G, w.G) > tolerance ||
				absDiff(g.B, w.B) > tolerance || absDiff(g.A, w.A) > tolerance {
				return fmt.Errorf("pixel (%d,%d) is %v, want %v — regenerate with `go run ./tools/icon generate`", x, y, g, w)
			}
		}
	}
	return nil
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}
