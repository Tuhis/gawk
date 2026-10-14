package scorecmd

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-sampler/internal/nativetest"
)

func writePNG(t *testing.T, path string, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 160, 90))
	for y := 0; y < 90; y++ {
		for x := 0; x < 160; x++ {
			img.Set(x, y, c)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// D19: the offline recall check runs the scoring path over image files and
// prints numbers only. With the test model, magenta is the "explicit" class.
func TestScoreDirectory(t *testing.T) {
	lib := nativetest.ONNXRuntime(t)
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "FEMALE_GENITALIA_EXPOSED", "secret-name-1.png"), color.RGBA{255, 0, 255, 255})
	writePNG(t, filepath.Join(dir, "FEMALE_GENITALIA_EXPOSED", "secret-name-2.png"), color.RGBA{255, 0, 255, 255})
	writePNG(t, filepath.Join(dir, "misc", "secret-name-3.png"), color.RGBA{0, 0, 0, 255})
	if err := os.WriteFile(filepath.Join(dir, "misc", "broken.png"), []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), []string{
		"-model", filepath.Join(nativetest.Testdata(t), "models", "test-320.onnx"),
		"-onnxruntime-lib", lib,
		"-thresholds", "0.5,0.9",
		dir,
	}, func(string) string { return "" }, &out)
	if err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{
		"images: 3 scored, 1 unreadable",
		"all (sample score)              3  0.667  0.667",
		"FEMALE_GENITALIA_EXPOSED        2  1.000  1.000",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "secret-name") {
		t.Fatalf("the report names a file:\n%s", report)
	}
}

func TestParseThresholds(t *testing.T) {
	got, err := parseThresholds(" 0.9,0.5 ,")
	if err != nil || len(got) != 2 || got[0] != 0.5 || got[1] != 0.9 {
		t.Fatalf("parseThresholds = %v, %v", got, err)
	}
	for _, bad := range []string{"", "0", "1.5", "x"} {
		if _, err := parseThresholds(bad); err == nil {
			t.Errorf("parseThresholds(%q) succeeded", bad)
		}
	}
}

func TestRunNeedsADirectory(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), nil, func(string) string { return "" }, &out); err == nil {
		t.Fatal("score without a directory succeeded")
	}
}
