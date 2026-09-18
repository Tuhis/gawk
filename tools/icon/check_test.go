package main

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// copyIconDir copies the committed assets into a scratch directory so a test
// can corrupt them.
func copyIconDir(t *testing.T) string {
	t.Helper()
	src := iconDir(t)
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, pngDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{svgName, icoName, resName} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range Sizes {
		data, err := os.ReadFile(pngPath(src, s))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pngPath(dst, s), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestCommittedDerivativesMatchTheSource(t *testing.T) {
	// The CI drift check, run as a test too so a local `go test` catches an
	// edited SVG before the push.
	if err := Check(iconDir(t)); err != nil {
		t.Fatalf("committed derivatives drift from %s: %v", svgName, err)
	}
}

func TestGenerateThenCheckIsClean(t *testing.T) {
	dir := copyIconDir(t)
	// Wipe the derivatives; Generate must recreate every one of them.
	for _, name := range []string{icoName, resName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, pngDir)); err != nil {
		t.Fatal(err)
	}
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err != nil {
		t.Fatal(err)
	}
}

func TestCheckFailsOnAShiftedRender(t *testing.T) {
	dir := copyIconDir(t)
	doc := loadIconDoc(t)
	img, err := Render(doc, 32)
	if err != nil {
		t.Fatal(err)
	}
	// Shift the whole render one pixel right — a real, small edit, not a
	// rounding difference — and commit it as the 32 px PNG.
	shifted := image.NewNRGBA(img.Bounds())
	draw.Draw(shifted, img.Bounds().Add(image.Pt(1, 0)), img, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, shifted); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pngPath(dir, 32), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err == nil {
		t.Fatal("a shifted PNG passed the check")
	}
}

func TestCheckFailsWhenTheSourceChanges(t *testing.T) {
	dir := copyIconDir(t)
	data, err := os.ReadFile(filepath.Join(dir, svgName))
	if err != nil {
		t.Fatal(err)
	}
	// A different tile colour: every derivative is now stale.
	edited := bytes.Replace(data, []byte(`fill="#863bff"`), []byte(`fill="#ff3b86"`), 1)
	if bytes.Equal(edited, data) {
		t.Fatal("the tile colour was not where this test expects it")
	}
	if err := os.WriteFile(filepath.Join(dir, svgName), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err == nil {
		t.Fatal("stale derivatives passed the check")
	}
}

func TestCheckFailsOnAMissingIcoEntry(t *testing.T) {
	dir := copyIconDir(t)
	images, err := renderSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	delete(images, 24)
	ico, err := WriteICO(images)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, icoName), ico, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err == nil {
		t.Fatal("an .ico missing a size passed the check")
	}
}

func TestCheckToleratesAUnitOfRoundingButNotMore(t *testing.T) {
	a := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	b := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	a.SetNRGBA(1, 1, nrgba(100, 100, 100, 255))
	b.SetNRGBA(1, 1, nrgba(100+tolerance, 100, 100, 255))
	if err := compare(a, b); err != nil {
		t.Fatalf("within tolerance: %v", err)
	}
	b.SetNRGBA(1, 1, nrgba(100+tolerance+1, 100, 100, 255))
	if err := compare(a, b); err == nil {
		t.Fatal("beyond tolerance passed")
	}
	if err := compare(a, image.NewNRGBA(image.Rect(0, 0, 3, 3))); err == nil {
		t.Fatal("size mismatch passed")
	}
}
