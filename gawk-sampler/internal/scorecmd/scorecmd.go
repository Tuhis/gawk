// Package scorecmd is the `score <dir>` subcommand (docs/71 D19): the offline
// recall check CF9 records. Recall cannot be measured on live traffic — there
// should be nothing to find — so an operator points this at a local set of
// explicit images and reads how much of it the configured model, labels and
// candidate thresholds catch.
//
// It runs the service's scoring path on image files: the Go standard
// library's decoders instead of ffmpeg (the files are images, not keyframes),
// then the same letterbox, model and post-processing. It prints numbers only:
// never a file name, never an image (D8 holds offline too).
package scorecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/gif"  // registered decoders: GIF
	_ "image/jpeg" // JPEG
	_ "image/png"  // PNG
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Tuhis/gawk/gawk-sampler/internal/config"
	"github.com/Tuhis/gawk/gawk-sampler/internal/detect"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// Run parses args and prints the report to out.
func Run(_ context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("gawk-sampler score", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: gawk-sampler score [flags] <dir>")
		fmt.Fprintln(fs.Output(), "Scores every JPEG, PNG and GIF under <dir>. An image inside a directory named")
		fmt.Fprintln(fs.Output(), "after a detector label (e.g. FEMALE_GENITALIA_EXPOSED/) is a positive of that")
		fmt.Fprintln(fs.Output(), "label; every image is a positive of the flagged set.")
		fs.PrintDefaults()
	}
	model := config.RegisterModelFlags(fs, config.NewEnv(getenv))
	thresholds := fs.String("thresholds", "0.3,0.4,0.5,0.6,0.7,0.8,0.9", "comma-separated candidate thresholds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("score: want exactly one directory")
	}
	m, err := model.Resolve()
	if err != nil {
		return err
	}
	ths, err := parseThresholds(*thresholds)
	if err != nil {
		return err
	}
	if err := detect.Init(m.ONNXRuntimeLib); err != nil {
		return err
	}
	d, err := detect.Load(m.Path, m.Input)
	if err != nil {
		return err
	}
	defer d.Close()
	r, err := Score(d, fs.Arg(0), m.Labels)
	if err != nil {
		return err
	}
	r.Print(out, ths)
	return nil
}

func parseThresholds(v string) ([]float64, error) {
	var out []float64
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, fmt.Errorf("score: threshold %q is not in (0, 1]", s)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("score: no thresholds")
	}
	sort.Float64s(out)
	return out, nil
}

// Result is the scores of one directory: each image's sample score (the max
// over the flagged labels, D7) and, for images under a label-named directory,
// that label's score.
type Result struct {
	Labels     []string
	Sample     []float64
	PerLabel   map[string][]float64
	Unreadable int
}

// Score scores every image under dir.
func Score(d *detect.Detector, dir string, labels []string) (Result, error) {
	r := Result{Labels: labels, PerLabel: map[string][]float64{}}
	all := detect.Labels()
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".jpg", ".jpeg", ".png", ".gif":
		default:
			return nil
		}
		img, err := readImage(path)
		if err != nil {
			r.Unreadable++
			return nil
		}
		rgb, clip := detect.Letterbox(img, d.Size())
		dets, err := d.Detect(rgb, clip)
		if err != nil {
			return err
		}
		scores := detect.LabelScores(dets, all)
		r.Sample = append(r.Sample, screening.SampleScore(scores, labels))
		if parent := filepath.Base(filepath.Dir(path)); slicesContains(all, parent) {
			r.PerLabel[parent] = append(r.PerLabel[parent], scores[parent])
		}
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("score: %w", err)
	}
	if len(r.Sample) == 0 {
		return r, fmt.Errorf("score: no readable image under %s", dir)
	}
	return r, nil
}

func readImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func recall(scores []float64, th float64) float64 {
	n := 0
	for _, s := range scores {
		if s >= th {
			n++
		}
	}
	return float64(n) / float64(len(scores))
}

// Print writes the recall table.
func (r Result) Print(out io.Writer, ths []float64) {
	fmt.Fprintf(out, "images: %d scored, %d unreadable\n", len(r.Sample), r.Unreadable)
	fmt.Fprintf(out, "flagged labels: %s\n\n", strings.Join(r.Labels, ","))
	header := fmt.Sprintf("%-26s %6s", "positives", "n")
	for _, th := range ths {
		header += fmt.Sprintf(" %6.2f", th)
	}
	fmt.Fprintln(out, header)
	row := func(name string, scores []float64) {
		line := fmt.Sprintf("%-26s %6d", name, len(scores))
		for _, th := range ths {
			line += fmt.Sprintf(" %6.3f", recall(scores, th))
		}
		fmt.Fprintln(out, line)
	}
	row("all (sample score)", r.Sample)
	names := make([]string, 0, len(r.PerLabel))
	for n := range r.PerLabel {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		row(n, r.PerLabel[n])
	}
}
