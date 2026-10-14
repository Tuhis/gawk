package detect

// Scorer scores whole model-input frames for a label set: the service's view
// of a Detector. A frame the service decoded has no known picture region —
// ffmpeg letterboxed it — so boxes are clipped to the whole input.
type Scorer struct {
	Detector *Detector
	Labels   []string
}

// Scores returns every label's score for one size×size RGB24 frame.
func (s Scorer) Scores(rgb []byte) (map[string]float64, error) {
	size := float64(s.Detector.Size())
	dets, err := s.Detector.Detect(rgb, Box{W: size, H: size})
	if err != nil {
		return nil, err
	}
	return LabelScores(dets, s.Labels), nil
}
