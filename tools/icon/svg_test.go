package main

import (
	"math"
	"strings"
	"testing"
)

const testSVGHead = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func pointNear(t *testing.T, got Point, x, y float64) {
	t.Helper()
	if !near(got.X, x) || !near(got.Y, y) {
		t.Fatalf("point %v, want (%g, %g)", got, x, y)
	}
}

func parseOne(t *testing.T, body string) Shape {
	t.Helper()
	doc, err := ParseSVG([]byte(testSVGHead + body + `</svg>`))
	if err != nil {
		t.Fatalf("ParseSVG: %v", err)
	}
	if len(doc.Shapes) != 1 {
		t.Fatalf("%d shapes, want 1", len(doc.Shapes))
	}
	return doc.Shapes[0]
}

func TestPathAbsoluteAndRelativeCommands(t *testing.T) {
	// Every command class the icon uses, once absolute and once relative,
	// with numbers glued together the way the favicon writes them.
	s := parseOne(t, `<path fill="#f00" d="M10 20l5-5L30 40h10H0v5V50z"/>`)
	if s.Fill != (RGBA{255, 0, 0, 255}) {
		t.Fatalf("fill %v", s.Fill)
	}
	kinds := []OpKind{MoveTo, LineTo, LineTo, LineTo, LineTo, LineTo, LineTo, ClosePath}
	if len(s.Ops) != len(kinds) {
		t.Fatalf("%d ops, want %d: %+v", len(s.Ops), len(kinds), s.Ops)
	}
	for i, k := range kinds {
		if s.Ops[i].Kind != k {
			t.Fatalf("op %d kind %v, want %v", i, s.Ops[i].Kind, k)
		}
	}
	pointNear(t, s.Ops[0].P[0], 10, 20)
	pointNear(t, s.Ops[1].P[0], 15, 15) // l5-5
	pointNear(t, s.Ops[2].P[0], 30, 40) // L30 40
	pointNear(t, s.Ops[3].P[0], 40, 40) // h10
	pointNear(t, s.Ops[4].P[0], 0, 40)  // H0
	pointNear(t, s.Ops[5].P[0], 0, 45)  // v5
	pointNear(t, s.Ops[6].P[0], 0, 50)  // V50
}

func TestPathCubicsRelativeAndGluedNumbers(t *testing.T) {
	// "-.664.845-2.021.375-2.021-.698" is six numbers with no separators
	// between most of them — the exact spelling in the favicon.
	s := parseOne(t, `<path fill="#000" d="M25.946 44.938c-.664.845-2.021.375-2.021-.698C1 2 3 4 5 6"/>`)
	if len(s.Ops) != 3 || s.Ops[1].Kind != CubeTo || s.Ops[2].Kind != CubeTo {
		t.Fatalf("ops %+v", s.Ops)
	}
	c := s.Ops[1]
	pointNear(t, c.P[0], 25.946-0.664, 44.938+0.845)
	pointNear(t, c.P[1], 25.946-2.021, 44.938+0.375)
	pointNear(t, c.P[2], 25.946-2.021, 44.938-0.698)
	pointNear(t, s.Ops[2].P[0], 1, 2)
	pointNear(t, s.Ops[2].P[2], 5, 6)
}

func TestImplicitLinetoAfterMoveto(t *testing.T) {
	s := parseOne(t, `<path fill="#000" d="M0 0 10 0 10 10m5 5 1 1"/>`)
	kinds := []OpKind{MoveTo, LineTo, LineTo, MoveTo, LineTo}
	if len(s.Ops) != len(kinds) {
		t.Fatalf("%d ops: %+v", len(s.Ops), s.Ops)
	}
	for i, k := range kinds {
		if s.Ops[i].Kind != k {
			t.Fatalf("op %d kind %v, want %v", i, s.Ops[i].Kind, k)
		}
	}
	pointNear(t, s.Ops[3].P[0], 15, 15)
	pointNear(t, s.Ops[4].P[0], 16, 16)
}

func TestArcBecomesQuarterCircleCubic(t *testing.T) {
	// A quarter circle of radius 10 from (10,0) to (0,10), sweeping through
	// the positive quadrant: one cubic whose control points sit at the
	// standard kappa distance and whose midpoint lies on the circle.
	s := parseOne(t, `<path fill="#000" d="M10 0A10 10 0 0 1 0 10"/>`)
	if len(s.Ops) != 2 || s.Ops[1].Kind != CubeTo {
		t.Fatalf("ops %+v", s.Ops)
	}
	c := s.Ops[1]
	pointNear(t, c.P[2], 0, 10)
	if !near(c.P[0].X, 10) || math.Abs(c.P[0].Y-kappa*10) > 1e-3 {
		t.Fatalf("control 1 %v, want (10, %g)", c.P[0], kappa*10)
	}
	if math.Abs(c.P[1].X-kappa*10) > 1e-3 || !near(c.P[1].Y, 10) {
		t.Fatalf("control 2 %v, want (%g, 10)", c.P[1], kappa*10)
	}
	// Midpoint of the Bézier at t=0.5 must be within a hair of the circle.
	mx := 0.125*10 + 0.375*c.P[0].X + 0.375*c.P[1].X + 0.125*c.P[2].X
	my := 0.125*0 + 0.375*c.P[0].Y + 0.375*c.P[1].Y + 0.125*c.P[2].Y
	if r := math.Hypot(mx, my); math.Abs(r-10) > 0.01 {
		t.Fatalf("midpoint radius %g, want 10", r)
	}
}

func TestArcSweepFlagPicksTheOtherSide(t *testing.T) {
	// Same endpoints, sweep=0: the arc goes the other way round, through the
	// negative quadrant, so the first control point heads to negative y.
	s := parseOne(t, `<path fill="#000" d="M10 0A10 10 0 0 0 0 10"/>`)
	if len(s.Ops) != 2 {
		t.Fatalf("ops %+v", s.Ops)
	}
	if s.Ops[1].P[0].Y >= 0 {
		t.Fatalf("sweep=0 control point %v should head to negative y", s.Ops[1].P[0])
	}
	pointNear(t, s.Ops[1].P[2], 0, 10)
}

func TestArcFlagsMayBeGluedToTheNextNumber(t *testing.T) {
	// "a2.26 2.26 0 0 0-2.262-2.262" — the favicon's spelling: the second
	// flag runs straight into the negative x.
	// The chord is a hair longer than r·√2, so this is a 90.1° arc and
	// splits into two cubics; the last one must land exactly on the end.
	s := parseOne(t, `<path fill="#000" d="M10 10a2.26 2.26 0 0 0-2.262-2.262"/>`)
	if len(s.Ops) != 3 || s.Ops[1].Kind != CubeTo || s.Ops[2].Kind != CubeTo {
		t.Fatalf("ops %+v", s.Ops)
	}
	pointNear(t, s.Ops[2].P[2], 10-2.262, 10-2.262)
}

func TestLargeArcSplitsIntoSeveralCubics(t *testing.T) {
	// Three quarters of a circle: three cubics, landing exactly on the end.
	s := parseOne(t, `<path fill="#000" d="M10 0A10 10 0 1 1 0 -10"/>`)
	if len(s.Ops) != 4 {
		t.Fatalf("%d ops, want a move and three cubics: %+v", len(s.Ops), s.Ops)
	}
	pointNear(t, s.Ops[3].P[2], 0, -10)
}

func TestRectWithRadiusIsFourLinesAndFourCorners(t *testing.T) {
	s := parseOne(t, `<rect x="0" y="0" width="100" height="100" rx="20" fill="#863bff"/>`)
	if s.Fill != (RGBA{0x86, 0x3b, 0xff, 255}) {
		t.Fatalf("fill %v", s.Fill)
	}
	var lines, cubes int
	for _, op := range s.Ops {
		switch op.Kind {
		case LineTo:
			lines++
		case CubeTo:
			cubes++
		}
	}
	if lines != 4 || cubes != 4 || s.Ops[len(s.Ops)-1].Kind != ClosePath {
		t.Fatalf("lines %d cubes %d ops %+v", lines, cubes, s.Ops)
	}
	pointNear(t, s.Ops[0].P[0], 20, 0)
	pointNear(t, s.Ops[1].P[0], 80, 0)
	pointNear(t, s.Ops[2].P[2], 100, 20)
}

func TestGroupTransformAppliesTranslateThenScaleInSVGOrder(t *testing.T) {
	// SVG applies the rightmost transform to the point first: scale(2) then
	// translate(10 20). (1,1) → (2,2) → (12,22).
	s := parseOne(t, `<g transform="translate(10 20) scale(2)"><path fill="#000" d="M1 1L2 3"/></g>`)
	pointNear(t, s.Ops[0].P[0], 12, 22)
	pointNear(t, s.Ops[1].P[0], 14, 26)
}

func TestNestedGroupsCompose(t *testing.T) {
	s := parseOne(t, `<g transform="translate(10)"><g transform="scale(3)"><path fill="#000" d="M1 1"/></g></g>`)
	pointNear(t, s.Ops[0].P[0], 13, 3)
}

func TestUnsupportedInputIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"element":   `<circle r="1" fill="#000"/>`,
		"command":   `<path fill="#000" d="M0 0Q1 1 2 2"/>`,
		"transform": `<g transform="rotate(45)"><path fill="#000" d="M0 0"/></g>`,
		"colour":    `<path fill="red" d="M0 0"/>`,
		"no fill":   `<path d="M0 0"/>`,
		"no start":  `<path fill="#000" d="10 10"/>`,
		"ry":        `<rect width="10" height="10" rx="2" ry="3" fill="#000"/>`,
	} {
		if _, err := ParseSVG([]byte(testSVGHead + body + `</svg>`)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
	if _, err := ParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><path fill="#000" d="M0 0"/></svg>`)); err == nil || !strings.Contains(err.Error(), "viewBox") {
		t.Errorf("missing viewBox: %v", err)
	}
}

func TestTheIconItselfParses(t *testing.T) {
	doc := loadIconDoc(t)
	if len(doc.Shapes) != 2 {
		t.Fatalf("%d shapes, want the tile and the bolt", len(doc.Shapes))
	}
	if doc.ViewBox.W != 256 || doc.ViewBox.H != 256 {
		t.Fatalf("viewBox %+v", doc.ViewBox)
	}
	// The bolt must sit inside the tile: every point within the viewBox.
	for _, op := range doc.Shapes[1].Ops {
		for _, p := range op.P {
			if op.Kind == ClosePath {
				continue
			}
			if p.X < 0 || p.Y < 0 || p.X > 256 || p.Y > 256 {
				t.Fatalf("bolt point %v is outside the tile", p)
			}
		}
	}
}
