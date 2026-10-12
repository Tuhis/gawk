package detect

import (
	"image"
	"image/color"
	"testing"
)

func TestTensorLayout(t *testing.T) {
	const size = 2
	rgb := []byte{
		255, 0, 0, 0, 255, 0,
		0, 0, 255, 255, 255, 255,
	}
	got, err := Tensor(rgb, size)
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{
		1, 0, 0, 1, // R plane
		0, 1, 0, 1, // G plane
		0, 0, 1, 1, // B plane
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Tensor = %v, want %v", got, want)
		}
	}
	if _, err := Tensor(rgb[:5], size); err == nil {
		t.Fatal("a short frame converted")
	}
}

func TestFit(t *testing.T) {
	for _, c := range []struct{ w, h, size, ww, wh int }{
		{1920, 1080, 320, 320, 180},
		{1080, 1920, 320, 180, 320},
		{100, 100, 640, 640, 640},
		{4000, 1, 320, 320, 1},
	} {
		if w, h := Fit(c.w, c.h, c.size); w != c.ww || h != c.wh {
			t.Errorf("Fit(%d,%d,%d) = %d,%d; want %d,%d", c.w, c.h, c.size, w, h, c.ww, c.wh)
		}
	}
}

// Letterbox keeps the aspect ratio, pads black right and bottom, and keeps
// colours.
func TestLetterbox(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 36))
	for y := 0; y < 36; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{255, 0, 255, 255}
			if x >= 32 {
				c = color.RGBA{0, 255, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	rgb, clip := Letterbox(img, 32)
	if clip.W != 32 || clip.H != 18 {
		t.Fatalf("clip %+v, want 32×18", clip)
	}
	px := func(x, y int) [3]byte {
		o := (y*32 + x) * 3
		return [3]byte{rgb[o], rgb[o+1], rgb[o+2]}
	}
	if p := px(4, 9); p != [3]byte{255, 0, 255} {
		t.Errorf("left half %v, want magenta", p)
	}
	if p := px(28, 9); p != [3]byte{0, 255, 0} {
		t.Errorf("right half %v, want green", p)
	}
	if p := px(16, 25); p != [3]byte{0, 0, 0} {
		t.Errorf("padding %v, want black", p)
	}
}
