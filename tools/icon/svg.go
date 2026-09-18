package main

// The SVG subset assets/icon/gawk.svg is written in, and nothing more:
// <rect> with a corner radius, <g> carrying translate/scale transforms, and
// <path> data using M L H V C A Z in either case. A general SVG library was
// rejected (docs/53 D2): the one candidate in pure Go is untagged and
// unmaintained, and the icon needs fifty lines of parser, not a renderer.
//
// Everything here resolves to absolute cubic Béziers in viewBox units. Arcs
// are converted at parse time (the corner of the bolt's tail is the only one),
// so the renderer knows exactly one curve type.

import (
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Op is one path segment with absolute coordinates.
type Op struct {
	Kind  OpKind
	P     [3]Point // LineTo/MoveTo use P[0]; CubeTo uses all three (c1, c2, end).
	Close bool
}

// OpKind is the segment type.
type OpKind uint8

const (
	// MoveTo starts a subpath at P[0].
	MoveTo OpKind = iota
	// LineTo draws a straight segment to P[0].
	LineTo
	// CubeTo draws a cubic Bézier through control points P[0], P[1] to P[2].
	CubeTo
	// ClosePath closes the current subpath.
	ClosePath
)

// Point is a 2-D coordinate in viewBox units.
type Point struct{ X, Y float64 }

// Shape is one filled outline.
type Shape struct {
	Fill RGBA
	Ops  []Op
}

// RGBA is a straight-alpha colour.
type RGBA struct{ R, G, B, A uint8 }

// Document is the parsed icon.
type Document struct {
	// ViewBox is the coordinate space the shapes are in.
	ViewBox struct{ MinX, MinY, W, H float64 }
	Shapes  []Shape
}

// affine is the 2×3 matrix [a c e; b d f] that maps (x, y) to
// (a·x + c·y + e, b·x + d·y + f).
type affine struct{ A, B, C, D, E, F float64 }

func identity() affine { return affine{A: 1, D: 1} }

func (m affine) apply(p Point) Point {
	return Point{m.A*p.X + m.C*p.Y + m.E, m.B*p.X + m.D*p.Y + m.F}
}

// mul returns m·n, i.e. n applied first, then m.
func (m affine) mul(n affine) affine {
	return affine{
		A: m.A*n.A + m.C*n.B,
		B: m.B*n.A + m.D*n.B,
		C: m.A*n.C + m.C*n.D,
		D: m.B*n.C + m.D*n.D,
		E: m.A*n.E + m.C*n.F + m.E,
		F: m.B*n.E + m.D*n.F + m.F,
	}
}

type xmlNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xmlNode  `xml:",any"`
}

func (n xmlNode) attr(name string) (string, bool) {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value, true
		}
	}
	return "", false
}

// allowedAttrs is the per-element attribute allowlist. Everything the
// renderer would ignore is rejected rather than tolerated, because gawk.svg
// is also installed as the scalable icon and drawn by the desktop's real SVG
// engine: an `opacity`, `stroke`, `fill-rule`, `style` or a `transform` on a
// shape would change that rendering while `check` stayed green — the exact
// drift D2 exists to prevent.
var allowedAttrs = map[string]map[string]bool{
	"svg":  {"xmlns": true, "width": true, "height": true, "viewBox": true},
	"g":    {"transform": true},
	"rect": {"x": true, "y": true, "width": true, "height": true, "rx": true, "ry": true, "fill": true},
	"path": {"d": true, "fill": true},
}

func checkAttrs(n xmlNode) error {
	allowed := allowedAttrs[n.XMLName.Local]
	for _, a := range n.Attrs {
		if !allowed[a.Name.Local] || a.Name.Space != "" {
			return fmt.Errorf("svg: <%s %s=…>: attribute is outside the supported subset (it would change how the desktop draws the scalable icon but not how tools/icon renders it)", n.XMLName.Local, a.Name.Local)
		}
	}
	return nil
}

// ParseSVG parses the icon subset described at the top of this file.
func ParseSVG(data []byte) (*Document, error) {
	var root xmlNode
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("svg: %w", err)
	}
	if root.XMLName.Local != "svg" {
		return nil, fmt.Errorf("svg: root element is <%s>, want <svg>", root.XMLName.Local)
	}
	if err := checkAttrs(root); err != nil {
		return nil, err
	}
	doc := &Document{}
	vb, ok := root.attr("viewBox")
	if !ok {
		return nil, fmt.Errorf("svg: no viewBox")
	}
	f := strings.Fields(vb)
	if len(f) != 4 {
		return nil, fmt.Errorf("svg: viewBox %q: want four numbers", vb)
	}
	var nums [4]float64
	for i, s := range f {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("svg: viewBox %q: %w", vb, err)
		}
		nums[i] = v
	}
	doc.ViewBox.MinX, doc.ViewBox.MinY, doc.ViewBox.W, doc.ViewBox.H = nums[0], nums[1], nums[2], nums[3]
	if doc.ViewBox.W <= 0 || doc.ViewBox.H <= 0 {
		return nil, fmt.Errorf("svg: viewBox %q: empty", vb)
	}
	if err := doc.walk(root.Nodes, identity()); err != nil {
		return nil, err
	}
	if len(doc.Shapes) == 0 {
		return nil, fmt.Errorf("svg: no shapes")
	}
	return doc, nil
}

func (doc *Document) walk(nodes []xmlNode, m affine) error {
	for _, n := range nodes {
		if allowedAttrs[n.XMLName.Local] == nil {
			return fmt.Errorf("svg: unsupported element <%s>", n.XMLName.Local)
		}
		if err := checkAttrs(n); err != nil {
			return err
		}
		switch n.XMLName.Local {
		case "g":
			gm := m
			if t, ok := n.attr("transform"); ok {
				tm, err := parseTransform(t)
				if err != nil {
					return err
				}
				gm = m.mul(tm)
			}
			if err := doc.walk(n.Nodes, gm); err != nil {
				return err
			}
		case "rect":
			s, err := parseRect(n, m)
			if err != nil {
				return err
			}
			doc.Shapes = append(doc.Shapes, s)
		case "path":
			s, err := parsePath(n, m)
			if err != nil {
				return err
			}
			doc.Shapes = append(doc.Shapes, s)
		default:
			return fmt.Errorf("svg: unsupported element <%s>", n.XMLName.Local)
		}
	}
	return nil
}

func parseFill(n xmlNode) (RGBA, error) {
	s, ok := n.attr("fill")
	if !ok {
		return RGBA{}, fmt.Errorf("svg: <%s> has no fill", n.XMLName.Local)
	}
	return parseColor(s)
}

func parseColor(s string) (RGBA, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		return RGBA{}, fmt.Errorf("svg: colour %q: only #rgb/#rrggbb are supported", s)
	}
	h := s[1:]
	switch len(h) {
	case 3:
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	case 6:
	default:
		return RGBA{}, fmt.Errorf("svg: colour %q: only #rgb/#rrggbb are supported", s)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return RGBA{}, fmt.Errorf("svg: colour %q: %w", s, err)
	}
	return RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}, nil
}

func numAttr(n xmlNode, name string, def float64) (float64, error) {
	s, ok := n.attr(name)
	if !ok {
		return def, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("svg: <%s %s=%q>: %w", n.XMLName.Local, name, s, err)
	}
	return v, nil
}

// kappa is the cubic Bézier control distance that approximates a quarter
// circle: 4(√2−1)/3.
const kappa = 0.5522847498307936

func parseRect(n xmlNode, m affine) (Shape, error) {
	fill, err := parseFill(n)
	if err != nil {
		return Shape{}, err
	}
	var x, y, w, h, rx float64
	for _, a := range []struct {
		name string
		dst  *float64
	}{{"x", &x}, {"y", &y}, {"width", &w}, {"height", &h}, {"rx", &rx}} {
		if *a.dst, err = numAttr(n, a.name, 0); err != nil {
			return Shape{}, err
		}
	}
	if w <= 0 || h <= 0 {
		return Shape{}, fmt.Errorf("svg: <rect> without a positive width and height")
	}
	if ry, err := numAttr(n, "ry", rx); err != nil {
		return Shape{}, err
	} else if ry != rx {
		return Shape{}, fmt.Errorf("svg: <rect rx=%g ry=%g>: only ry == rx is supported", rx, ry)
	}
	rx = math.Min(rx, math.Min(w, h)/2)
	pb := pathBuilder{m: m}
	if rx == 0 {
		pb.moveTo(Point{x, y})
		pb.lineTo(Point{x + w, y})
		pb.lineTo(Point{x + w, y + h})
		pb.lineTo(Point{x, y + h})
		pb.close()
		return Shape{Fill: fill, Ops: pb.ops}, nil
	}
	k := kappa * rx
	pb.moveTo(Point{x + rx, y})
	pb.lineTo(Point{x + w - rx, y})
	pb.cubeTo(Point{x + w - rx + k, y}, Point{x + w, y + rx - k}, Point{x + w, y + rx})
	pb.lineTo(Point{x + w, y + h - rx})
	pb.cubeTo(Point{x + w, y + h - rx + k}, Point{x + w - rx + k, y + h}, Point{x + w - rx, y + h})
	pb.lineTo(Point{x + rx, y + h})
	pb.cubeTo(Point{x + rx - k, y + h}, Point{x, y + h - rx + k}, Point{x, y + h - rx})
	pb.lineTo(Point{x, y + rx})
	pb.cubeTo(Point{x, y + rx - k}, Point{x + rx - k, y}, Point{x + rx, y})
	pb.close()
	return Shape{Fill: fill, Ops: pb.ops}, nil
}

// pathBuilder accumulates ops, applying the transform to every point it is
// handed (all in untransformed user units).
type pathBuilder struct {
	m   affine
	ops []Op
}

func (b *pathBuilder) moveTo(p Point) {
	b.ops = append(b.ops, Op{Kind: MoveTo, P: [3]Point{b.m.apply(p)}})
}

func (b *pathBuilder) lineTo(p Point) {
	b.ops = append(b.ops, Op{Kind: LineTo, P: [3]Point{b.m.apply(p)}})
}

func (b *pathBuilder) cubeTo(c1, c2, p Point) {
	b.ops = append(b.ops, Op{Kind: CubeTo, P: [3]Point{b.m.apply(c1), b.m.apply(c2), b.m.apply(p)}})
}

func (b *pathBuilder) close() {
	b.ops = append(b.ops, Op{Kind: ClosePath})
}

// parseTransform accepts a whitespace-separated list of translate(x[ y]) and
// scale(s[ sy]) in SVG order (leftmost applied last to the point).
func parseTransform(s string) (affine, error) {
	m := identity()
	rest := strings.TrimSpace(s)
	for rest != "" {
		open := strings.IndexByte(rest, '(')
		closeIdx := strings.IndexByte(rest, ')')
		if open < 0 || closeIdx < open {
			return affine{}, fmt.Errorf("svg: transform %q: malformed", s)
		}
		name := strings.TrimSpace(rest[:open])
		args := strings.FieldsFunc(rest[open+1:closeIdx], func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' })
		vals := make([]float64, len(args))
		for i, a := range args {
			v, err := strconv.ParseFloat(a, 64)
			if err != nil {
				return affine{}, fmt.Errorf("svg: transform %q: %w", s, err)
			}
			vals[i] = v
		}
		var t affine
		switch {
		case name == "translate" && (len(vals) == 1 || len(vals) == 2):
			t = identity()
			t.E = vals[0]
			if len(vals) == 2 {
				t.F = vals[1]
			}
		case name == "scale" && (len(vals) == 1 || len(vals) == 2):
			t = affine{A: vals[0], D: vals[0]}
			if len(vals) == 2 {
				t.D = vals[1]
			}
		default:
			return affine{}, fmt.Errorf("svg: transform %q: only translate(x[ y]) and scale(s[ sy]) are supported", s)
		}
		m = m.mul(t)
		rest = strings.TrimSpace(rest[closeIdx+1:])
	}
	return m, nil
}

func parsePath(n xmlNode, m affine) (Shape, error) {
	fill, err := parseFill(n)
	if err != nil {
		return Shape{}, err
	}
	d, ok := n.attr("d")
	if !ok {
		return Shape{}, fmt.Errorf("svg: <path> without d")
	}
	ops, err := parsePathData(d, m)
	if err != nil {
		return Shape{}, err
	}
	return Shape{Fill: fill, Ops: ops}, nil
}

// pathScanner tokenises SVG path data. Numbers may run together
// ("-2.021.375" is two numbers), which is why this is not strings.Fields.
type pathScanner struct {
	s string
	i int
}

func (sc *pathScanner) skipSep() {
	for sc.i < len(sc.s) {
		c := sc.s[sc.i]
		if c == ' ' || c == ',' || c == '\t' || c == '\n' || c == '\r' {
			sc.i++
			continue
		}
		break
	}
}

func (sc *pathScanner) peekCommand() (byte, bool) {
	sc.skipSep()
	if sc.i >= len(sc.s) {
		return 0, false
	}
	c := sc.s[sc.i]
	if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
		return c, true
	}
	return 0, false
}

func (sc *pathScanner) atEnd() bool {
	sc.skipSep()
	return sc.i >= len(sc.s)
}

func (sc *pathScanner) number() (float64, error) {
	sc.skipSep()
	start := sc.i
	if sc.i < len(sc.s) && (sc.s[sc.i] == '-' || sc.s[sc.i] == '+') {
		sc.i++
	}
	digits := 0
	for sc.i < len(sc.s) && sc.s[sc.i] >= '0' && sc.s[sc.i] <= '9' {
		sc.i++
		digits++
	}
	if sc.i < len(sc.s) && sc.s[sc.i] == '.' {
		sc.i++
		for sc.i < len(sc.s) && sc.s[sc.i] >= '0' && sc.s[sc.i] <= '9' {
			sc.i++
			digits++
		}
	}
	if digits == 0 {
		return 0, fmt.Errorf("svg: path data: expected a number at offset %d", start)
	}
	if sc.i < len(sc.s) && (sc.s[sc.i] == 'e' || sc.s[sc.i] == 'E') {
		sc.i++
		if sc.i < len(sc.s) && (sc.s[sc.i] == '-' || sc.s[sc.i] == '+') {
			sc.i++
		}
		for sc.i < len(sc.s) && sc.s[sc.i] >= '0' && sc.s[sc.i] <= '9' {
			sc.i++
		}
	}
	return strconv.ParseFloat(sc.s[start:sc.i], 64)
}

// flag reads an arc flag, which is a single '0' or '1' and may be glued to
// its neighbours without a separator.
func (sc *pathScanner) flag() (bool, error) {
	sc.skipSep()
	if sc.i >= len(sc.s) || (sc.s[sc.i] != '0' && sc.s[sc.i] != '1') {
		return false, fmt.Errorf("svg: path data: expected an arc flag at offset %d", sc.i)
	}
	v := sc.s[sc.i] == '1'
	sc.i++
	return v, nil
}

func parsePathData(d string, m affine) ([]Op, error) {
	sc := &pathScanner{s: d}
	pb := pathBuilder{m: m}
	var cur, start Point
	var cmd byte
	for !sc.atEnd() {
		if c, ok := sc.peekCommand(); ok {
			cmd = c
			sc.i++
		} else if cmd == 0 {
			return nil, fmt.Errorf("svg: path data must start with a command")
		} else if cmd == 'Z' || cmd == 'z' {
			// Closepath takes no arguments and repeats nothing, so a number
			// here is malformed — and without this branch nothing would be
			// consumed and the loop would never advance.
			return nil, fmt.Errorf("svg: path data: number after closepath at offset %d", sc.i)
		} else if cmd == 'M' {
			cmd = 'L' // implicit lineto after a moveto
		} else if cmd == 'm' {
			cmd = 'l'
		}
		rel := cmd >= 'a' && cmd <= 'z'
		base := Point{}
		if rel {
			base = cur
		}
		switch cmd {
		case 'M', 'm':
			x, err := sc.number()
			if err != nil {
				return nil, err
			}
			y, err := sc.number()
			if err != nil {
				return nil, err
			}
			cur = Point{base.X + x, base.Y + y}
			start = cur
			pb.moveTo(cur)
		case 'L', 'l':
			x, err := sc.number()
			if err != nil {
				return nil, err
			}
			y, err := sc.number()
			if err != nil {
				return nil, err
			}
			cur = Point{base.X + x, base.Y + y}
			pb.lineTo(cur)
		case 'H', 'h':
			x, err := sc.number()
			if err != nil {
				return nil, err
			}
			cur = Point{base.X + x, cur.Y}
			pb.lineTo(cur)
		case 'V', 'v':
			y, err := sc.number()
			if err != nil {
				return nil, err
			}
			cur = Point{cur.X, base.Y + y}
			pb.lineTo(cur)
		case 'C', 'c':
			var v [6]float64
			for i := range v {
				f, err := sc.number()
				if err != nil {
					return nil, err
				}
				v[i] = f
			}
			c1 := Point{base.X + v[0], base.Y + v[1]}
			c2 := Point{base.X + v[2], base.Y + v[3]}
			cur = Point{base.X + v[4], base.Y + v[5]}
			pb.cubeTo(c1, c2, cur)
		case 'A', 'a':
			rx, err := sc.number()
			if err != nil {
				return nil, err
			}
			ry, err := sc.number()
			if err != nil {
				return nil, err
			}
			rot, err := sc.number()
			if err != nil {
				return nil, err
			}
			large, err := sc.flag()
			if err != nil {
				return nil, err
			}
			sweep, err := sc.flag()
			if err != nil {
				return nil, err
			}
			x, err := sc.number()
			if err != nil {
				return nil, err
			}
			y, err := sc.number()
			if err != nil {
				return nil, err
			}
			end := Point{base.X + x, base.Y + y}
			for _, c := range arcToCubics(cur, end, rx, ry, rot, large, sweep) {
				pb.cubeTo(c[0], c[1], c[2])
			}
			cur = end
		case 'Z', 'z':
			pb.close()
			cur = start
		default:
			return nil, fmt.Errorf("svg: path data: unsupported command %q", cmd)
		}
	}
	return pb.ops, nil
}

// arcToCubics converts an SVG elliptical arc (from p1 to p2) into cubic
// Béziers, one per quarter turn at most, following the SVG implementation
// notes (F.6.5, endpoint to centre parameterisation). A degenerate arc
// (coincident endpoints or a zero radius) becomes a straight line, as the
// specification requires.
func arcToCubics(p1, p2 Point, rx, ry, rotDeg float64, large, sweep bool) [][3]Point {
	if p1 == p2 {
		return nil
	}
	rx, ry = math.Abs(rx), math.Abs(ry)
	if rx == 0 || ry == 0 {
		return [][3]Point{{p1, p2, p2}}
	}
	phi := rotDeg * math.Pi / 180
	cosPhi, sinPhi := math.Cos(phi), math.Sin(phi)
	dx, dy := (p1.X-p2.X)/2, (p1.Y-p2.Y)/2
	x1p := cosPhi*dx + sinPhi*dy
	y1p := -sinPhi*dx + cosPhi*dy
	lambda := x1p*x1p/(rx*rx) + y1p*y1p/(ry*ry)
	if lambda > 1 {
		s := math.Sqrt(lambda)
		rx *= s
		ry *= s
	}
	num := rx*rx*ry*ry - rx*rx*y1p*y1p - ry*ry*x1p*x1p
	den := rx*rx*y1p*y1p + ry*ry*x1p*x1p
	coef := 0.0
	if den != 0 && num > 0 {
		coef = math.Sqrt(num / den)
	}
	if large == sweep {
		coef = -coef
	}
	cxp := coef * rx * y1p / ry
	cyp := -coef * ry * x1p / rx
	cx := cosPhi*cxp - sinPhi*cyp + (p1.X+p2.X)/2
	cy := sinPhi*cxp + cosPhi*cyp + (p1.Y+p2.Y)/2

	angle := func(ux, uy, vx, vy float64) float64 {
		dot := ux*vx + uy*vy
		l := math.Hypot(ux, uy) * math.Hypot(vx, vy)
		a := math.Acos(math.Max(-1, math.Min(1, dot/l)))
		if ux*vy-uy*vx < 0 {
			a = -a
		}
		return a
	}
	theta1 := angle(1, 0, (x1p-cxp)/rx, (y1p-cyp)/ry)
	dTheta := angle((x1p-cxp)/rx, (y1p-cyp)/ry, (-x1p-cxp)/rx, (-y1p-cyp)/ry)
	if !sweep && dTheta > 0 {
		dTheta -= 2 * math.Pi
	} else if sweep && dTheta < 0 {
		dTheta += 2 * math.Pi
	}

	// The epsilon absorbs the float error in the centre computation: a
	// chord of r·√2 is a quarter circle and must become one cubic, not two.
	n := int(math.Ceil(math.Abs(dTheta)/(math.Pi/2) - 1e-6))
	if n < 1 {
		n = 1
	}
	delta := dTheta / float64(n)
	k := 4.0 / 3.0 * math.Tan(delta/4)
	pt := func(t float64) Point {
		c, s := math.Cos(t), math.Sin(t)
		return Point{cx + rx*c*cosPhi - ry*s*sinPhi, cy + rx*c*sinPhi + ry*s*cosPhi}
	}
	deriv := func(t float64) Point {
		c, s := math.Cos(t), math.Sin(t)
		return Point{-rx*s*cosPhi - ry*c*sinPhi, -rx*s*sinPhi + ry*c*cosPhi}
	}
	out := make([][3]Point, 0, n)
	for i := 0; i < n; i++ {
		ta := theta1 + float64(i)*delta
		tb := ta + delta
		pa, pb := pt(ta), pt(tb)
		da, db := deriv(ta), deriv(tb)
		c1 := Point{pa.X + k*da.X, pa.Y + k*da.Y}
		c2 := Point{pb.X - k*db.X, pb.Y - k*db.Y}
		if i == n-1 {
			pb = p2 // land exactly on the endpoint, not within float error of it
		}
		out = append(out, [3]Point{c1, c2, pb})
	}
	return out
}
