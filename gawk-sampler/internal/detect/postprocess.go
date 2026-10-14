package detect

import (
	"fmt"
	"sort"
)

// The YOLOv8 detection head the NudeNet exports end in (docs/71 §9.5). Its
// output is one [4+C, A] matrix per image, row-major: rows 0–3 are each
// anchor's box as centre x, centre y, width and height in input pixels, and
// row 4+c is class c's confidence. There is no separate objectness row.
const (
	// CandidateFloor is the confidence an anchor's best class needs to be
	// considered at all.
	CandidateFloor float32 = 0.2
	// KeepFloor is the confidence a candidate needs to survive NMS. It is a
	// strict bound, like the reference's.
	KeepFloor float32 = 0.25
	// IoUThreshold: a candidate overlapping an already-kept box by more than
	// this is suppressed, whatever either box's class.
	IoUThreshold = 0.45
)

// Box is an axis-aligned rectangle in model-input pixels: top-left corner and
// size.
type Box struct {
	X, Y, W, H float64
}

func (b Box) area() float64 {
	if b.W <= 0 || b.H <= 0 {
		return 0
	}
	return b.W * b.H
}

// intersect is the overlap of b and c (zero-sized when they do not overlap).
func (b Box) intersect(c Box) Box {
	x0, y0 := max(b.X, c.X), max(b.Y, c.Y)
	x1, y1 := min(b.X+b.W, c.X+c.W), min(b.Y+b.H, c.Y+c.H)
	if x1 <= x0 || y1 <= y0 {
		return Box{X: x0, Y: y0}
	}
	return Box{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func iou(a, b Box) float64 {
	inter := a.intersect(b).area()
	if inter == 0 {
		return 0
	}
	return inter / (a.area() + b.area() - inter)
}

// Detection is one box the post-processing kept.
type Detection struct {
	// Label is the class index into Labels().
	Label int
	Score float32
	Box   Box
}

// Decode reads one image's output — out holds (4+NumLabels)·anchors values,
// row-major — and returns the kept detections, highest score first.
//
// Each anchor proposes its best class when that class reaches CandidateFloor.
// Proposals are clipped to clip, the part of the input the picture occupies:
// the rest is letterbox padding, and a box reaching into it is only as large
// as the picture lets it be. Then a class-agnostic greedy NMS keeps, in
// descending score order (ties in anchor order), every proposal above
// KeepFloor that overlaps no kept box by more than IoUThreshold.
func Decode(out []float32, anchors int, clip Box) ([]Detection, error) {
	rows := 4 + NumLabels
	if anchors <= 0 || len(out) != rows*anchors {
		return nil, fmt.Errorf("detect: output has %d values, want %d rows × %d anchors", len(out), rows, anchors)
	}
	at := func(row, anchor int) float32 { return out[row*anchors+anchor] }

	var cands []Detection
	for a := 0; a < anchors; a++ {
		best, label := at(4, a), 0
		for c := 1; c < NumLabels; c++ {
			if s := at(4+c, a); s > best {
				best, label = s, c
			}
		}
		if best < CandidateFloor {
			continue
		}
		cx, cy := float64(at(0, a)), float64(at(1, a))
		w, h := float64(at(2, a)), float64(at(3, a))
		box := Box{X: cx - w/2, Y: cy - h/2, W: w, H: h}.intersect(clip)
		cands = append(cands, Detection{Label: label, Score: best, Box: box})
	}

	// Stable: equal scores keep anchor order, so the result does not depend
	// on the sort algorithm.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
	var kept []Detection
	for _, c := range cands {
		if c.Score <= KeepFloor {
			break
		}
		suppressed := false
		for _, k := range kept {
			if iou(c.Box, k.Box) > IoUThreshold {
				suppressed = true
				break
			}
		}
		if !suppressed {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// LabelScores is each named label's highest kept score, 0 for a label with no
// kept box (docs/71 §9.5). Every name in names gets an entry; names the
// model does not know score 0.
func LabelScores(dets []Detection, names []string) map[string]float64 {
	out := make(map[string]float64, len(names))
	for _, n := range names {
		out[n] = 0
	}
	for _, d := range dets {
		n := labels[d.Label]
		if v, ok := out[n]; ok && float64(d.Score) > v {
			out[n] = float64(d.Score)
		}
	}
	return out
}

// Anchors is the anchor count of a YOLOv8 head at a square input of size
// pixels: one per cell of the stride-8, -16 and -32 grids.
func Anchors(size int) int {
	n := 0
	for _, stride := range []int{8, 16, 32} {
		g := size / stride
		n += g * g
	}
	return n
}
