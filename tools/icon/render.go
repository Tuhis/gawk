package main

import (
	"fmt"
	"image"
	"image/color"

	"golang.org/x/image/vector"
)

// Sizes is the square pixel size set every derivative carries (docs/53 D9):
// the hicolor sizes GNOME and KDE ask for, plus the Explorer view sizes and
// the 24 px Windows title bar.
var Sizes = []int{16, 24, 32, 48, 64, 128, 256}

// Render rasterises the document into a size×size straight-alpha image. The
// viewBox is mapped onto the square; a non-square viewBox is an error rather
// than a stretch, because an icon that quietly stretched would pass every
// structural check and look wrong.
func Render(doc *Document, size int) (*image.NRGBA, error) {
	if size <= 0 {
		return nil, fmt.Errorf("render: size %d", size)
	}
	if doc.ViewBox.W != doc.ViewBox.H {
		return nil, fmt.Errorf("render: viewBox %gx%g is not square", doc.ViewBox.W, doc.ViewBox.H)
	}
	scale := float64(size) / doc.ViewBox.W
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for _, s := range doc.Shapes {
		z := vector.NewRasterizer(size, size) // DrawOp defaults to draw.Over
		tx := func(p Point) (float32, float32) {
			return float32((p.X - doc.ViewBox.MinX) * scale), float32((p.Y - doc.ViewBox.MinY) * scale)
		}
		for _, op := range s.Ops {
			switch op.Kind {
			case MoveTo:
				x, y := tx(op.P[0])
				z.MoveTo(x, y)
			case LineTo:
				x, y := tx(op.P[0])
				z.LineTo(x, y)
			case CubeTo:
				x1, y1 := tx(op.P[0])
				x2, y2 := tx(op.P[1])
				x3, y3 := tx(op.P[2])
				z.CubeTo(x1, y1, x2, y2, x3, y3)
			case ClosePath:
				z.ClosePath()
			}
		}
		z.ClosePath()
		src := image.NewUniform(color.NRGBA{s.Fill.R, s.Fill.G, s.Fill.B, s.Fill.A})
		z.Draw(dst, dst.Bounds(), src, image.Point{})
	}
	out := image.NewNRGBA(dst.Bounds())
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			out.Set(x, y, dst.RGBAAt(x, y))
		}
	}
	return out, nil
}

// RenderAll renders every entry of Sizes.
func RenderAll(doc *Document) (map[int]*image.NRGBA, error) {
	out := make(map[int]*image.NRGBA, len(Sizes))
	for _, s := range Sizes {
		img, err := Render(doc, s)
		if err != nil {
			return nil, err
		}
		out[s] = img
	}
	return out, nil
}
