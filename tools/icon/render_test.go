package main

import (
	"image"
	"os"
	"path/filepath"
	"testing"
)

// iconDir is the committed source and derivatives, relative to this package.
func iconDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "assets", "icon"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func loadIconDoc(t *testing.T) *Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(iconDir(t), svgName))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseSVG(data)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestRenderFillsTheRectAndLeavesTheCornersTransparent(t *testing.T) {
	doc, err := ParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">` +
		`<rect x="0" y="0" width="100" height="100" rx="30" fill="#863bff"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	img, err := Render(doc, 100)
	if err != nil {
		t.Fatal(err)
	}
	if c := img.NRGBAAt(50, 50); c != nrgba(0x86, 0x3b, 0xff, 255) {
		t.Fatalf("centre %v", c)
	}
	// Edge midpoints are inside the rounded rect; the four corners are not.
	for _, p := range []image.Point{{50, 0}, {0, 50}, {99, 50}, {50, 99}} {
		if c := img.NRGBAAt(p.X, p.Y); c.A != 255 {
			t.Fatalf("edge %v alpha %d, want opaque", p, c.A)
		}
	}
	for _, p := range []image.Point{{0, 0}, {99, 0}, {0, 99}, {99, 99}} {
		if c := img.NRGBAAt(p.X, p.Y); c.A != 0 {
			t.Fatalf("corner %v alpha %d, want transparent", p, c.A)
		}
	}
}

func TestRenderScalesTheViewBoxToTheRequestedSize(t *testing.T) {
	doc, err := ParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">` +
		`<rect width="10" height="5" fill="#fff"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	img, err := Render(doc, 40)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 40 {
		t.Fatalf("size %v", img.Bounds())
	}
	if img.NRGBAAt(20, 10).A != 255 || img.NRGBAAt(20, 30).A != 0 {
		t.Fatalf("the top half should be filled and the bottom half empty")
	}
}

func TestRenderRefusesANonSquareViewBox(t *testing.T) {
	doc, err := ParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 46">` +
		`<rect width="10" height="5" fill="#fff"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render(doc, 16); err == nil {
		t.Fatal("rendered a non-square viewBox without complaint")
	}
}

func TestIconRendersABoltOnATile(t *testing.T) {
	doc := loadIconDoc(t)
	img, err := Render(doc, 256)
	if err != nil {
		t.Fatal(err)
	}
	// Tile colour at a tile point, white on the bolt's body, transparent at
	// the corner the radius cuts off.
	if c := img.NRGBAAt(20, 128); c != nrgba(0x86, 0x3b, 0xff, 255) {
		t.Fatalf("tile pixel %v", c)
	}
	if c := img.NRGBAAt(128, 128); c != nrgba(255, 255, 255, 255) {
		t.Fatalf("bolt pixel %v", c)
	}
	if c := img.NRGBAAt(0, 0); c.A != 0 {
		t.Fatalf("corner pixel %v, want transparent", c)
	}
	all, err := RenderAll(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range Sizes {
		if all[s] == nil || all[s].Bounds().Dx() != s {
			t.Fatalf("size %d missing or wrong", s)
		}
	}
}
