package detect

import (
	"fmt"
	"image"
	"image/color"
	"math"
)

// The model's input is the frame letterboxed into a square: scaled to fit with
// its aspect ratio kept, padded with black on the right and bottom, RGB in
// 0–1, NCHW (docs/71 §9.5). NudeNet pads to a square first and then resizes;
// scaling first and padding after is the same picture. In the service ffmpeg
// does the scaling and padding (D4) and Tensor only converts; the offline
// `score` subcommand, which reads image files, letterboxes with Letterbox.

// Tensor converts a size×size RGB24 frame into the model's [1,3,size,size]
// float32 input.
func Tensor(rgb []byte, size int) ([]float32, error) {
	plane := size * size
	if len(rgb) != plane*3 {
		return nil, fmt.Errorf("detect: frame has %d bytes, want %d for %dx%d RGB24", len(rgb), plane*3, size, size)
	}
	out := make([]float32, plane*3)
	for i := 0; i < plane; i++ {
		out[i] = float32(rgb[3*i]) / 255
		out[plane+i] = float32(rgb[3*i+1]) / 255
		out[2*plane+i] = float32(rgb[3*i+2]) / 255
	}
	return out, nil
}

// Fit is the size of a w×h picture scaled to fit a size×size square with its
// aspect ratio kept — the picture's share of a letterboxed input.
func Fit(w, h, size int) (int, int) {
	if w >= h {
		return size, max(1, int(math.Round(float64(h)*float64(size)/float64(w))))
	}
	return max(1, int(math.Round(float64(w)*float64(size)/float64(h)))), size
}

// Letterbox scales img to fit a size×size square (bilinear), pads the rest
// black on the right and bottom, and returns it as RGB24 together with the
// picture's region — the clip Decode takes.
func Letterbox(img image.Image, size int) ([]byte, Box) {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := Fit(sw, sh, size)
	out := make([]byte, size*size*3)
	sx := float64(sw) / float64(dw)
	sy := float64(sh) / float64(dh)
	for y := 0; y < dh; y++ {
		// Pixel-centre mapping, the convention of OpenCV's INTER_LINEAR and
		// ffmpeg's bilinear scaler alike.
		fy := (float64(y)+0.5)*sy - 0.5
		y0 := clampInt(int(math.Floor(fy)), 0, sh-1)
		y1 := clampInt(y0+1, 0, sh-1)
		wy := clampFloat(fy-float64(y0), 0, 1)
		for x := 0; x < dw; x++ {
			fx := (float64(x)+0.5)*sx - 0.5
			x0 := clampInt(int(math.Floor(fx)), 0, sw-1)
			x1 := clampInt(x0+1, 0, sw-1)
			wx := clampFloat(fx-float64(x0), 0, 1)
			c00 := rgbAt(img, b.Min.X+x0, b.Min.Y+y0)
			c10 := rgbAt(img, b.Min.X+x1, b.Min.Y+y0)
			c01 := rgbAt(img, b.Min.X+x0, b.Min.Y+y1)
			c11 := rgbAt(img, b.Min.X+x1, b.Min.Y+y1)
			o := (y*size + x) * 3
			for ch := 0; ch < 3; ch++ {
				top := c00[ch]*(1-wx) + c10[ch]*wx
				bot := c01[ch]*(1-wx) + c11[ch]*wx
				out[o+ch] = uint8(clampFloat(math.Round(top*(1-wy)+bot*wy), 0, 255))
			}
		}
	}
	return out, Box{W: float64(dw), H: float64(dh)}
}

func rgbAt(img image.Image, x, y int) [3]float64 {
	c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	// Transparency composites onto black, the colour of the padding.
	a := float64(c.A) / 255
	return [3]float64{float64(c.R) * a, float64(c.G) * a, float64(c.B) * a}
}

func clampInt(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

func clampFloat(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(v, hi))
}
