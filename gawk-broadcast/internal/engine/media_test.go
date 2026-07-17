package engine

import "testing"

// ParseResolution is shared by the CLI flag and the GUI settings field — one
// parser, so the two shells cannot drift on what "1920x1080" means.
func TestParseResolution(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w, h int
		ok   bool
	}{
		{"1920x1080", 1920, 1080, true},
		{"2560X1440", 2560, 1440, true}, // case-insensitive separator
		{" 1280 x 720 ", 1280, 720, true},
		{"1080p", 0, 0, false},
		{"1920", 0, 0, false},
		{"0x1080", 0, 0, false},
		{"1920x-1080", 0, 0, false},
		{"", 0, 0, false},
	} {
		w, h, err := ParseResolution(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("ParseResolution(%q) err = %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && (w != tc.w || h != tc.h) {
			t.Errorf("ParseResolution(%q) = %dx%d, want %dx%d", tc.in, w, h, tc.w, tc.h)
		}
	}
}
