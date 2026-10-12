package detect

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Tuhis/gawk/gawk-sampler/internal/nativetest"
)

// referenceVector is one file under testdata/reference/vectors (see the
// generator there): NudeNet's raw candidate rows for a photograph and the
// detections NudeNet made from them.
type referenceVector struct {
	Image      string      `json:"image"`
	Width      int         `json:"width"`
	Height     int         `json:"height"`
	InputSize  int         `json:"inputSize"`
	Anchors    int         `json:"anchors"`
	Rows       [][]float64 `json:"rows"`
	Detections []struct {
		Label string    `json:"label"`
		Score float64   `json:"score"`
		Box   []float64 `json:"box"`
	} `json:"detections"`
}

// CF5: the post-processing matches NudeNet's Python reference on ≥ 10
// photographs, every box score within 1e-3.
func TestDecodeMatchesNudeNetReference(t *testing.T) {
	dir := filepath.Join(nativetest.Testdata(t), "reference", "vectors")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("%d reference vectors, CF5 wants at least 10", len(files))
	}
	compared := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v referenceVector
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(v.Image, func(t *testing.T) {
			if v.Anchors != Anchors(v.InputSize) {
				t.Fatalf("vector has %d anchors, Anchors(%d) = %d", v.Anchors, v.InputSize, Anchors(v.InputSize))
			}
			rows := 4 + NumLabels
			out := make([]float32, rows*v.Anchors)
			for _, r := range v.Rows {
				if len(r) != 1+rows {
					t.Fatalf("row has %d values, want %d", len(r), 1+rows)
				}
				a := int(r[0])
				for i := 0; i < rows; i++ {
					out[i*v.Anchors+a] = float32(r[1+i])
				}
			}
			// NudeNet pads the photograph to a square and resizes that, so the
			// picture's share of the input is its size × input / longer side.
			longer := float64(max(v.Width, v.Height))
			scale := float64(v.InputSize) / longer
			clip := Box{W: float64(v.Width) * scale, H: float64(v.Height) * scale}
			got, err := Decode(out, v.Anchors, clip)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(v.Detections) {
				t.Fatalf("%d detections, reference has %d: got %+v", len(got), len(v.Detections), got)
			}
			// Both lists are in descending score order.
			for i, want := range v.Detections {
				g := got[i]
				if Labels()[g.Label] != want.Label {
					t.Errorf("detection %d: label %s, reference %s", i, Labels()[g.Label], want.Label)
				}
				if d := math.Abs(float64(g.Score) - want.Score); d > 1e-3 {
					t.Errorf("detection %d: score %v, reference %v (|Δ| %.2g > 1e-3)", i, g.Score, want.Score, d)
				}
				// The box agrees too, in the photograph's pixels. The reference
				// truncates to integers, hence the tolerance; a box at the
				// left or top edge is skipped because the reference clips
				// those by moving the box rather than shrinking it.
				if want.Box[0] > 0 && want.Box[1] > 0 {
					gx, gy := g.Box.X/scale, g.Box.Y/scale
					gw, gh := g.Box.W/scale, g.Box.H/scale
					if math.Abs(gx-want.Box[0]) > 2 || math.Abs(gy-want.Box[1]) > 2 ||
						math.Abs(gw-want.Box[2]) > 2 || math.Abs(gh-want.Box[3]) > 2 {
						t.Errorf("detection %d: box [%.1f %.1f %.1f %.1f], reference %v", i, gx, gy, gw, gh, want.Box)
					}
				}
				compared++
			}
		})
	}
	if compared < 10 {
		t.Fatalf("only %d detections compared", compared)
	}
}

// out builds a model output with the given anchor columns set.
func testOutput(anchors int, cols map[int][]float32) []float32 {
	rows := 4 + NumLabels
	out := make([]float32, rows*anchors)
	for a, col := range cols {
		for r, v := range col {
			out[r*anchors+a] = v
		}
	}
	return out
}

// col is one anchor column: a box and one class score.
func col(cx, cy, w, h float32, label int, score float32) []float32 {
	c := make([]float32, 4+NumLabels)
	c[0], c[1], c[2], c[3] = cx, cy, w, h
	c[4+label] = score
	return c
}

func TestDecodeFloorsAndNMS(t *testing.T) {
	full := Box{W: 320, H: 320}
	face, feet := 1, 7
	out := testOutput(10, map[int][]float32{
		0: col(100, 100, 40, 40, face, 0.9),
		1: col(102, 101, 40, 40, feet, 0.6),  // overlaps anchor 0 heavily: suppressed although another class
		2: col(250, 250, 30, 30, feet, 0.5),  // separate: kept
		3: col(50, 250, 30, 30, face, 0.25),  // at KeepFloor: not above it, dropped
		4: col(50, 50, 30, 30, face, 0.19),   // under CandidateFloor
		5: col(250, 250, 30, 30, face, 0.45), // overlaps anchor 2 entirely: suppressed
	})
	got, err := Decode(out, 10, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Label != face || got[1].Label != feet || got[1].Score != 0.5 {
		t.Fatalf("Decode = %+v, want the 0.9 face and the 0.5 feet", got)
	}
	scores := LabelScores(got, []string{"FACE_FEMALE", "FEET_EXPOSED", "ANUS_EXPOSED", "NOT_A_LABEL"})
	want := map[string]float64{"FACE_FEMALE": float64(float32(0.9)), "FEET_EXPOSED": 0.5, "ANUS_EXPOSED": 0, "NOT_A_LABEL": 0}
	for k, v := range want {
		if scores[k] != v {
			t.Errorf("LabelScores[%s] = %v, want %v", k, scores[k], v)
		}
	}
}

// An anchor's best class is its proposal: a strong second class on the same
// anchor is not a detection of its own.
func TestDecodeOneClassPerAnchor(t *testing.T) {
	c := col(100, 100, 40, 40, 1, 0.7)
	c[4+4] = 0.65
	got, err := Decode(testOutput(4, map[int][]float32{0: c}), 4, Box{W: 320, H: 320})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Label != 1 {
		t.Fatalf("Decode = %+v, want one detection of class 1", got)
	}
}

// Clipping to the picture changes overlaps: a box reaching into the
// letterbox padding is only as large as the picture lets it be, so it can
// suppress a neighbour its full extent would have left alone.
func TestDecodeClipsToPicture(t *testing.T) {
	out := testOutput(4, map[int][]float32{
		0: col(100, 190, 40, 100, 1, 0.9), // y 140–240
		1: col(100, 150, 40, 40, 7, 0.8),  // y 130–170
	})
	full, err := Decode(out, 4, Box{W: 320, H: 320})
	if err != nil {
		t.Fatal(err)
	}
	clipped, err := Decode(out, 4, Box{W: 320, H: 180})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 2 || len(clipped) != 1 {
		t.Fatalf("unclipped kept %d, clipped kept %d; want 2 and 1", len(full), len(clipped))
	}
	if clipped[0].Box.H != 40 {
		t.Fatalf("clipped box height %v, want 40", clipped[0].Box.H)
	}
}

func TestDecodeRejectsWrongShape(t *testing.T) {
	if _, err := Decode(make([]float32, 10), 2100, Box{W: 320, H: 320}); err == nil {
		t.Fatal("a short output decoded")
	}
}

func TestAnchors(t *testing.T) {
	if Anchors(320) != 2100 || Anchors(640) != 8400 {
		t.Fatalf("Anchors(320)=%d Anchors(640)=%d", Anchors(320), Anchors(640))
	}
}

func TestLabels(t *testing.T) {
	ls := Labels()
	if len(ls) != 18 {
		t.Fatalf("%d labels", len(ls))
	}
	sorted := append([]string(nil), ls...)
	sort.Strings(sorted)
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			t.Fatalf("label %s repeats", sorted[i])
		}
	}
	// The three the default -labels names, at the indices the test model and
	// the reference vectors use.
	for name, idx := range map[string]int{"FEMALE_GENITALIA_EXPOSED": 4, "ANUS_EXPOSED": 6, "MALE_GENITALIA_EXPOSED": 14} {
		if i, ok := LabelIndex(name); !ok || i != idx {
			t.Errorf("LabelIndex(%s) = %d, %v; want %d", name, i, ok, idx)
		}
	}
	ls[0] = "edited"
	if Labels()[0] == "edited" {
		t.Fatal("Labels shares its backing array")
	}
}
