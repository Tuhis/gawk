// Package detect turns one decoded frame into per-label scores (docs/71 D7,
// §9.5): the model's input tensor, ONNX Runtime inference, and the YOLOv8
// post-processing that reads the detector's output.
//
// The pre- and post-processing reproduce what NudeNet's own Python does, so a
// score here means what it means there, but they are written from the YOLOv8
// output format, not ported: NudeNet's code is AGPL-3.0 and none of it may
// enter the repository (docs/71 §8.1). The reference vectors under
// testdata/reference hold the two to the same answers.
package detect

// labels is NudeNet's detector vocabulary in class-index order: row 4+i of
// the model's output is the score of labels[i]. It is data the model was
// trained with (the 320n and 640m exports carry the same list in their
// `names` metadata), restated here because interoperating with the model
// needs it.
var labels = [...]string{
	"FEMALE_GENITALIA_COVERED",
	"FACE_FEMALE",
	"BUTTOCKS_EXPOSED",
	"FEMALE_BREAST_EXPOSED",
	"FEMALE_GENITALIA_EXPOSED",
	"MALE_BREAST_EXPOSED",
	"ANUS_EXPOSED",
	"FEET_EXPOSED",
	"BELLY_COVERED",
	"FEET_COVERED",
	"ARMPITS_COVERED",
	"ARMPITS_EXPOSED",
	"FACE_MALE",
	"BELLY_EXPOSED",
	"MALE_GENITALIA_EXPOSED",
	"ANUS_COVERED",
	"FEMALE_BREAST_COVERED",
	"BUTTOCKS_COVERED",
}

// NumLabels is the class count the model's output must carry.
const NumLabels = len(labels)

// Labels returns the vocabulary in class-index order, in a fresh slice.
func Labels() []string {
	out := make([]string, NumLabels)
	copy(out, labels[:])
	return out
}

// LabelIndex is the class index of name, or false for a name the model does
// not know.
func LabelIndex(name string) (int, bool) {
	for i, l := range labels {
		if l == name {
			return i, true
		}
	}
	return 0, false
}
