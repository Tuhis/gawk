package main

// Review findings on PR #328, each written as a failing test first.

import (
	"testing"
	"time"
)

func TestNumbersAfterClosepathAreAnErrorNotAHang(t *testing.T) {
	// The grammar has no implicit repetition after Z; the parser used to
	// spin forever here (nothing consumed, cmd still 'Z').
	for _, d := range []string{"M0 0 L1 1 Z 5 5", "M0 0 L1 1 z 5 5"} {
		done := make(chan error, 1)
		go func() { _, err := parsePathData(d, identity()); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%q parsed", d)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%q: parser hung", d)
		}
	}
}

func TestRenderingAttributesOutsideTheSubsetAreRejected(t *testing.T) {
	// Anything that would change how a real SVG engine draws the scalable
	// icon but that this renderer ignores must be an error, or `check` stays
	// green while the PNG/ICO/RES and the installed SVG drift apart.
	for name, body := range map[string]string{
		"transform on path": `<path fill="#000" transform="scale(2)" d="M0 0"/>`,
		"transform on rect": `<rect width="1" height="1" fill="#000" transform="scale(2)"/>`,
		"opacity":           `<path fill="#000" opacity="0.5" d="M0 0"/>`,
		"fill-opacity":      `<path fill="#000" fill-opacity="0.5" d="M0 0"/>`,
		"stroke":            `<path fill="#000" stroke="#fff" d="M0 0"/>`,
		"stroke-width":      `<path fill="#000" stroke-width="2" d="M0 0"/>`,
		"fill-rule":         `<path fill="#000" fill-rule="evenodd" d="M0 0"/>`,
		"style":             `<path fill="#000" style="fill:#fff" d="M0 0"/>`,
		"opacity on g":      `<g opacity="0.5"><path fill="#000" d="M0 0"/></g>`,
		"class on g":        `<g class="x"><path fill="#000" d="M0 0"/></g>`,
	} {
		if _, err := ParseSVG([]byte(testSVGHead + body + `</svg>`)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
	// The root may carry a style attribute nobody honours either.
	if _, err := ParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1" style="x"><path fill="#000" d="M0 0"/></svg>`)); err == nil {
		t.Error("style on <svg> parsed")
	}
	// But the attributes the icon actually uses are fine, including the XML
	// namespace on the root.
	if _, err := ParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 10 10">` +
		`<rect x="0" y="0" width="10" height="10" rx="2" ry="2" fill="#000"/>` +
		`<g transform="translate(1 1)"><path fill="#fff" d="M0 0"/></g></svg>`)); err != nil {
		t.Errorf("the allowed subset was rejected: %v", err)
	}
}

func TestEqualRxRyInDifferentSpellingsIsAccepted(t *testing.T) {
	for _, ry := range []string{"56", "56.0", "56.", "5.6e1"} {
		if _, err := ParseSVG([]byte(testSVGHead + `<rect width="100" height="100" rx="56" ry="` + ry + `" fill="#000"/></svg>`)); err != nil {
			t.Errorf("ry=%q rejected: %v", ry, err)
		}
	}
	if _, err := ParseSVG([]byte(testSVGHead + `<rect width="100" height="100" rx="56" ry="57" fill="#000"/></svg>`)); err == nil {
		t.Error("ry != rx accepted")
	}
}
