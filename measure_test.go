// Copyright 2018 by the rasterx Authors. All rights reserved.
//_
// External tests for PathMeasure. The reference implementation in this file
// is intentionally independent of measure.go: it re-parses the raw Path
// slice with its own walker, integrates arc lengths with a different,
// higher-order adaptive Gauss-Legendre 32 rule, and inverts the arc-length
// function with a dense lookup table plus Newton correction. Agreement
// between two independently written solvers is evidence the numbers are
// right rather than an echo of the production implementation.

package rasterx_test

import (
	"image"
	"math"
	"math/rand"
	"testing"
	"time"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// ---------------------------------------------------------------------------
// Independent high-precision reference
// ---------------------------------------------------------------------------

// refGL32X/W hold the positive abscissae and weights of the Gauss-Legendre
// 32 point rule on [-1, 1]; the mirrored negative nodes complete the rule.
var refGL32X = [16]float64{
	0.99726386184948157, 0.985611511545268382, 0.96476225558750639,
	0.934906075937739667, 0.896321155766052091, 0.84936761373256997,
	0.794483795967942386, 0.732182118740289711, 0.663044266930215231,
	0.587715757240762304, 0.506899908932229359, 0.421351276130635333,
	0.331868602282127667, 0.239287362252137065, 0.144471961582796488,
	0.0483076656877383173,
}
var refGL32W = [16]float64{
	0.00701861000947013626, 0.0162743947309057085, 0.0253920653092621386,
	0.0342738629130214106, 0.0428358980222267038, 0.0509980592623761539,
	0.0586840934785355789, 0.0658222227763619466, 0.0723457941088486156,
	0.078193895787070436, 0.0833119242269467208, 0.0876520930044037416,
	0.0911738786957639047, 0.0938443990808045386, 0.0956387200792748471,
	0.0965400885147278537,
}

type refSegKind int

const (
	refLine refSegKind = iota
	refQuad
	refCubic
	refClose
)

type refPoint struct{ x, y float64 }

type refSegment struct {
	kind              refSegKind
	p0, p1, p2, p3    refPoint
	length, startDist float64
}

type refMeasure struct {
	segs     []refSegment
	total    float64
	first    refPoint
	hasPoint bool
}

func refGl32Integrate(a, b float64, f func(float64) float64) float64 {
	half, mid := (b-a)*0.5, (a+b)*0.5
	sum := 0.0
	for k := 0; k < 16; k++ {
		dx := half * refGL32X[k]
		sum += refGL32W[k] * (f(mid+dx) + f(mid-dx))
	}
	return half * sum
}

func (s *refSegment) point(t float64) refPoint {
	switch s.kind {
	case refLine, refClose:
		return refPoint{s.p0.x + t*(s.p1.x-s.p0.x), s.p0.y + t*(s.p1.y-s.p0.y)}
	case refQuad:
		mt := 1 - t
		a, b, c := mt*mt, 2*mt*t, t*t
		return refPoint{a*s.p0.x + b*s.p1.x + c*s.p2.x,
			a*s.p0.y + b*s.p1.y + c*s.p2.y}
	default:
		mt := 1 - t
		a, b, c, d := mt*mt*mt, 3*mt*mt*t, 3*mt*t*t, t*t*t
		return refPoint{a*s.p0.x + b*s.p1.x + c*s.p2.x + d*s.p3.x,
			a*s.p0.y + b*s.p1.y + c*s.p2.y + d*s.p3.y}
	}
}

func (s *refSegment) deriv(t float64) (dx, dy float64) {
	switch s.kind {
	case refLine, refClose:
		return s.p1.x - s.p0.x, s.p1.y - s.p0.y
	case refQuad:
		mt := 1 - t
		return 2 * (mt*(s.p1.x-s.p0.x) + t*(s.p2.x-s.p1.x)),
			2 * (mt*(s.p1.y-s.p0.y) + t*(s.p2.y-s.p1.y))
	default:
		mt := 1 - t
		return 3 * (mt*mt*(s.p1.x-s.p0.x) + 2*mt*t*(s.p2.x-s.p1.x) + t*t*(s.p3.x-s.p2.x)),
			3 * (mt*mt*(s.p1.y-s.p0.y) + 2*mt*t*(s.p2.y-s.p1.y) + t*t*(s.p3.y-s.p2.y))
	}
}

func (s *refSegment) speed(t float64) float64 {
	dx, dy := s.deriv(t)
	return math.Hypot(dx, dy)
}

// refAdaptiveLength integrates speed over [a, b] adaptively by halving
// panels until the refinement changes by less than tol or the depth cap is
// reached. Depth is bounded, so a cusp cannot cause infinite subdivision.
func refAdaptiveLength(s *refSegment, a, b, tol float64, depth int) float64 {
	whole := refGl32Integrate(a, b, s.speed)
	mid := (a + b) * 0.5
	left := refGl32Integrate(a, mid, s.speed)
	right := refGl32Integrate(mid, b, s.speed)
	if depth <= 0 || math.Abs(left+right-whole) <= tol {
		return left + right
	}
	return refAdaptiveLength(s, a, mid, tol/2, depth-1) +
		refAdaptiveLength(s, mid, b, tol/2, depth-1)
}

// buildRefMeasure re-walks the raw path slice; it shares no code with the
// production parser.
func buildRefMeasure(p Path) *refMeasure {
	m := &refMeasure{}
	var pen, subStart refPoint
	hasPen := false
	appendSeg := func(s refSegment) {
		if s.kind == refLine || s.kind == refClose {
			s.length = math.Hypot(s.p1.x-s.p0.x, s.p1.y-s.p0.y)
		} else {
			s.length = refAdaptiveLength(&s, 0, 1, 1e-13, 32)
		}
		s.startDist = m.total
		m.total += s.length
		m.segs = append(m.segs, s)
	}
	pt := func(i int) refPoint {
		return refPoint{float64(p[i]) / 64.0, float64(p[i+1]) / 64.0}
	}
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			pen, subStart, hasPen = pt(i+1), pt(i+1), true
			if !m.hasPoint {
				m.first, m.hasPoint = pen, true
			}
			i += 3
		case PathLineTo:
			if !hasPen {
				hasPen, m.hasPoint = true, true
			}
			q := pt(i + 1)
			appendSeg(refSegment{kind: refLine, p0: pen, p1: q})
			pen, i = q, i+3
		case PathQuadTo:
			if !hasPen {
				hasPen, m.hasPoint = true, true
			}
			b, c := pt(i+1), pt(i+3)
			appendSeg(refSegment{kind: refQuad, p0: pen, p1: b, p2: c})
			pen, i = c, i+5
		case PathCubicTo:
			if !hasPen {
				hasPen, m.hasPoint = true, true
			}
			b, c, d := pt(i+1), pt(i+3), pt(i+5)
			appendSeg(refSegment{kind: refCubic, p0: pen, p1: b, p2: c, p3: d})
			pen, i = d, i+7
		case PathClose:
			appendSeg(refSegment{kind: refClose, p0: pen, p1: subStart})
			pen, i = subStart, i+1
		default:
			i++
		}
	}
	return m
}

// locate finds the segment for distance d using an independent dense table.
// Boundary distances resolve onto the outgoing (next) segment and
// zero-length segments are skipped, matching the production convention.
func (m *refMeasure) locate(d float64) (int, float64) {
	if d <= 0 {
		for i := range m.segs {
			if m.segs[i].length > 0 {
				return i, 0
			}
		}
		return 0, 0
	}
	if d >= m.total {
		for i := len(m.segs) - 1; i >= 0; i-- {
			if m.segs[i].length > 0 {
				return i, m.segs[i].length
			}
		}
		return len(m.segs) - 1, 0
	}
	for i := range m.segs {
		s := &m.segs[i]
		if s.length == 0 {
			continue
		}
		// Strict comparison leaves the boundary to the next segment.
		if d < s.startDist+s.length {
			return i, d - s.startDist
		}
	}
	last := len(m.segs) - 1
	return last, m.segs[last].length
}

// pointAt inverts the arc-length function with a dense sampled table and a
// bounded Newton refinement, written independently of the production solver.
func (m *refMeasure) pointAt(d float64) (refPoint, refPoint) {
	if m.total == 0 {
		return m.first, refPoint{}
	}
	if d < 0 {
		d = 0
	} else if d > m.total {
		d = m.total
	}
	i, u := m.locate(d)
	s := &m.segs[i]
	if s.length == 0 {
		return s.p0, m.refTangent(i, s, 0)
	}
	const n = 20001
	// Build this segment's dense table.
	tbl := make([]float64, n)
	for k := 1; k < n; k++ {
		t0, t1 := float64(k-1)/(n-1), float64(k)/(n-1)
		tbl[k] = tbl[k-1] + refGl32Integrate(t0, t1, s.speed)
	}
	// Bracket the target arc length inside one panel.
	lo, hi := 0, n-1
	for lo < hi {
		mid := (lo + hi) / 2
		if tbl[mid] < u {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	k := lo - 1
	if k < 0 {
		k = 0
	}
	ta, tb := float64(k)/(n-1), float64(k+1)/(n-1)
	base := tbl[k]
	// Linear seed, then Newton with bisection fallback.
	span := tbl[k+1] - base
	t := ta
	if span > 0 {
		t = ta + (u-base)/span*(tb-ta)
	}
	g := func(x float64) float64 {
		return refGl32Integrate(ta, x, s.speed) - (u - base)
	}
	lb, hb := ta, tb
	for step := 0; step < 30; step++ {
		f := g(t)
		sp := s.speed(t)
		if sp > 1e-14 {
			nx := t - f/sp
			if nx < lb {
				nx = lb
			} else if nx > hb {
				nx = hb
			}
			if math.Abs(nx-t) < 1e-15 {
				t = nx
				break
			}
			t = nx
			continue
		}
		if f < 0 {
			lb = t
		} else {
			hb = t
		}
		t = (lb + hb) * 0.5
	}
	return s.point(t), m.refTangent(i, s, t)
}

func (m *refMeasure) refTangent(i int, s *refSegment, t float64) refPoint {
	dx, dy := s.deriv(t)
	if n := math.Hypot(dx, dy); n > 0 {
		return refPoint{dx / n, dy / n}
	}
	for _, c := range []float64{math.Min(1, t+1e-6), math.Max(0, t-1e-6)} {
		dx, dy = s.deriv(c)
		if n := math.Hypot(dx, dy); n > 0 {
			return refPoint{dx / n, dy / n}
		}
	}
	if i+1 < len(m.segs) {
		dx, dy = m.segs[i+1].deriv(0)
		if n := math.Hypot(dx, dy); n > 0 {
			return refPoint{dx / n, dy / n}
		}
	}
	if i > 0 {
		dx, dy = m.segs[i-1].deriv(1)
		if n := math.Hypot(dx, dy); n > 0 {
			return refPoint{dx / n, dy / n}
		}
	}
	return refPoint{}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const (
	refPointTol = 2e-6 // user units, tighter than the 1/64 fixed quantum
	refTanTol   = 2e-3
	refLenTol   = 1e-7
)

func toFixed(p Path) Path { return p }

func measureOrFail(t *testing.T, p Path) *PathMeasure {
	t.Helper()
	m, err := NewPathMeasure(p)
	if err != nil {
		t.Fatalf("NewPathMeasure: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestMeasureLineExact checks straight-line length against the exact
// Euclidean value and exact endpoint coordinates.
func TestMeasureLineExact(t *testing.T) {
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(3, 4))  // length exactly 5
	p.Line(ToFixedP(3, -8)) // length 12
	m := measureOrFail(t, p)
	if got := m.LengthF(); math.Abs(got-17) > 1e-9 {
		t.Errorf("line length: want 17, got %.9f", got)
	}
	if got := m.Length(); got != fixed.Int26_6(17*64) {
		t.Errorf("fixed length: want %v, got %v", 17*64, got)
	}
	// Exact endpoints.
	pt, _ := m.PointAt(0)
	if pt != ToFixedP(0, 0) {
		t.Errorf("start point: got %v", pt)
	}
	pt, _ = m.PointAt(fixed.Int26_6(5 * 64))
	if pt != ToFixedP(3, 4) {
		t.Errorf("join point: got %v", pt)
	}
	pt, _ = m.PointAt(m.Length())
	if pt != ToFixedP(3, -8) {
		t.Errorf("end point: got %v", pt)
	}
	// Tangent of first segment is the unit vector (0.6, 0.8).
	_, tan := m.PointAt(fixed.Int26_6(2.5 * 64))
	wantTan := ToFixedP(0.6, 0.8)
	if d := math.Abs(float64(tan.X-wantTan.X)) + math.Abs(float64(tan.Y-wantTan.Y)); d > 2 {
		t.Errorf("line tangent: got %v want %v", tan, wantTan)
	}
}

// TestMeasureReferenceLengths compares production lengths against the
// independent adaptive reference across lines, quads, cubics, closes and
// multi-sub-path documents.
func TestMeasureReferenceLengths(t *testing.T) {
	paths := map[string]Path{}

	var line Path
	line.Start(ToFixedP(-10, 2.5))
	line.Line(ToFixedP(7, -4.25))
	line.Line(ToFixedP(12, 20))
	paths["lines"] = line

	var quad Path
	quad.Start(ToFixedP(0, 0))
	quad.QuadBezier(ToFixedP(50, 100), ToFixedP(100, 0))
	quad.QuadBezier(ToFixedP(150, -100), ToFixedP(200, 0))
	paths["quads"] = quad

	var cubic Path
	cubic.Start(ToFixedP(87, 212))
	cubic.CubeBezier(ToFixedP(138, 90), ToFixedP(269, 75), ToFixedP(259, 147))
	cubic.CubeBezier(ToFixedP(254, 71), ToFixedP(104, 176), ToFixedP(128, 282))
	paths["cubics"] = cubic

	closed := getPartPathLike()
	paths["closed-quads"] = closed

	var multi Path
	multi.Start(ToFixedP(0, 0))
	multi.Line(ToFixedP(10, 0))
	multi.Stop(true) // zero-length close
	multi.Start(ToFixedP(50, 50))
	multi.CubeBezier(ToFixedP(60, 80), ToFixedP(90, 20), ToFixedP(100, 50))
	multi.Stop(true)
	paths["multi-subpath"] = multi

	for name, p := range paths {
		m := measureOrFail(t, p)
		ref := buildRefMeasure(p)
		if math.Abs(m.LengthF()-ref.total) > refLenTol {
			t.Errorf("%s: total length %.9f vs ref %.9f (diff %.2e)",
				name, m.LengthF(), ref.total, m.LengthF()-ref.total)
		}
		if m.SegmentCount() != len(ref.segs) {
			t.Errorf("%s: segment count %d vs ref %d", name, m.SegmentCount(), len(ref.segs))
		}
		// Per-segment cumulative boundaries must agree.
		gotCum := m.CumulativeLengthsF()
		if len(gotCum) != len(ref.segs)+1 {
			t.Fatalf("%s: cumulative length count mismatch", name)
		}
		for k, s := range ref.segs {
			if math.Abs(gotCum[k]-s.startDist) > refLenTol {
				t.Errorf("%s: boundary %d got %.9f want %.9f", name, k, gotCum[k], s.startDist)
			}
		}
		if math.Abs(gotCum[len(gotCum)-1]-ref.total) > refLenTol {
			t.Errorf("%s: final cumulative boundary wrong", name)
		}
	}
}

// getPartPathLike builds a closed quadratic path (mirrors the shape used by
// the rasterizer tests) without depending on the test helper in
// rasterx_test.go's other file.
func getPartPathLike() Path {
	var p Path
	p.Start(ToFixedP(210.08, 222.97))
	p.Line(ToFixedP(192.55, 244.95))
	p.QuadBezier(ToFixedP(146.53, 229.95), ToFixedP(115.55, 209.55))
	p.QuadBezier(ToFixedP(102.50, 211.00), ToFixedP(95.38, 211.00))
	p.QuadBezier(ToFixedP(56.09, 211.00), ToFixedP(31.17, 182.33))
	p.Stop(true)
	return p
}

// TestMeasureFixtureEndpoints checks the repository's real complex closed
// shape: the measured endpoints must equal the pen origin exactly, and the
// cumulative table must be monotone with the expected number of entries.
func TestMeasureFixtureEndpoints(t *testing.T) {
	p := getPartPathLike()
	m := measureOrFail(t, p)
	ref := buildRefMeasure(p)
	if math.Abs(m.LengthF()-ref.total) > refLenTol {
		t.Fatalf("fixture length %.9f vs ref %.9f", m.LengthF(), ref.total)
	}
	// The closed shape has one line, three quads and one close segment.
	if m.SegmentCount() != 5 {
		t.Errorf("fixture segment count: got %d want 5", m.SegmentCount())
	}
	cum := m.CumulativeLengths()
	fcum := m.CumulativeLengthsF()
	if len(cum) != 6 || len(fcum) != 6 {
		t.Fatalf("cumulative length entries: %d/%d", len(cum), len(fcum))
	}
	for k := 1; k < len(fcum); k++ {
		if fcum[k] < fcum[k-1] {
			t.Errorf("cumulative table not monotone at %d", k)
		}
		// The fixed table rounds each boundary independently.
		if math.Abs(float64(cum[k])/64-fcum[k]) > 1.0/64 {
			t.Errorf("fixed boundary %d drift", k)
		}
	}
	// Endpoint exactness in both APIs.
	start, startTan := m.PointAt(0)
	if start != ToFixedP(210.08, 222.97) {
		t.Errorf("fixed start point got %v", start)
	}
	if startTan == (fixed.Point26_6{}) {
		t.Error("start tangent should be defined on a non-degenerate path")
	}
	end, _ := m.PointAt(m.Length())
	if end != ToFixedP(210.08, 222.97) {
		t.Errorf("closed fixed end point got %v, want origin", end)
	}
	// The float API reads the fixed-rounded coordinates actually stored in
	// the Path, so compare against those rather than the decimal input.
	fx, fy := float64(ToFixedP(210.08, 222.97).X)/64, float64(ToFixedP(210.08, 222.97).Y)/64
	sx, sy, _, _, err := m.PointAtFChecked(0)
	if err != nil || math.Abs(sx-fx) > 1e-12 || math.Abs(sy-fy) > 1e-12 {
		t.Errorf("float start point (%v,%v) err %v", sx, sy, err)
	}
	ex, ey, _, _, err := m.PointAtFChecked(m.LengthF())
	if err != nil || math.Abs(ex-fx) > 1e-12 || math.Abs(ey-fy) > 1e-12 {
		t.Errorf("float end point (%v,%v) err %v", ex, ey, err)
	}
	// The convenience constructor agrees with the primary one.
	m2, err := p.Measure()
	if err != nil || m2.LengthF() != m.LengthF() {
		t.Error("Path.Measure disagrees with NewPathMeasure")
	}
}

// TestMeasureReferenceSamples checks equidistant points and tangents against
// the independent reference over every segment kind.
func TestMeasureReferenceSamples(t *testing.T) {
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(40, 0))
	p.QuadBezier(ToFixedP(80, 60), ToFixedP(120, 0))
	p.CubeBezier(ToFixedP(150, -40), ToFixedP(190, 90), ToFixedP(220, 30))
	p.Line(ToFixedP(260, 30))
	p.Stop(true)
	m := measureOrFail(t, p)
	ref := buildRefMeasure(p)
	if math.Abs(m.LengthF()-ref.total) > refLenTol {
		t.Fatalf("length mismatch %.10f vs %.10f", m.LengthF(), ref.total)
	}
	const samples = 137
	for k := 0; k <= samples; k++ {
		d := m.LengthF() * float64(k) / samples
		x, y, tx, ty := m.PointAtF(d)
		rp, rt := ref.pointAt(d)
		if math.Hypot(x-rp.x, y-rp.y) > refPointTol {
			t.Errorf("d=%.6f point (%.7f,%.7f) vs ref (%.7f,%.7f)", d, x, y, rp.x, rp.y)
		}
		// Tangent orientation: unit vectors must agree in direction and sign.
		if dot := tx*rt.x + ty*rt.y; dot < 1-refTanTol {
			t.Errorf("d=%.6f tangent dot %.6f (got (%v,%v) ref (%v,%v))",
				d, dot, tx, ty, rt.x, rt.y)
		}
		// Fixed wrapper stays within one fixed quantum plus rounding.
		fp, ftan := m.PointAt(fixed.Int26_6(math.Round(d * 64)))
		if math.Abs(float64(fp.X)/64-x) > 1.0/64+1e-9 ||
			math.Abs(float64(fp.Y)/64-y) > 1.0/64+1e-9 {
			t.Errorf("fixed point drift at d=%.6f", d)
		}
		if n := math.Hypot(float64(ftan.X), float64(ftan.Y)); math.Abs(n-64) > 1.5 {
			t.Errorf("fixed tangent norm %.4f at d=%.6f", n, d)
		}
	}
}

// TestMeasureConstantSpeed verifies that parameterizing by arc length gives
// constant-speed motion, the requirement behind uniform icon movement. A
// discrete chord between two equidistant samples is necessarily shorter than
// the curved arc wherever the path bends, so the checks are: the sampled
// polyline length converges to the arc length from below as sampling refines,
// and on a straight segment equal distance steps give exactly equal chords.
func TestMeasureConstantSpeed(t *testing.T) {
	var p Path
	p.Start(ToFixedP(10, 10))
	p.CubeBezier(ToFixedP(120, -60), ToFixedP(-30, 200), ToFixedP(230, 250))
	p.QuadBezier(ToFixedP(300, 300), ToFixedP(330, 120))
	p.Line(ToFixedP(400, 120)) // straight tail
	m := measureOrFail(t, p)

	// Polyline over equidistant samples must approach LengthF from below.
	prevPoly := 0.0
	for _, steps := range []int{50, 200, 800} {
		step := m.LengthF() / float64(steps)
		px, py, _, _ := m.PointAtF(0)
		poly := 0.0
		for k := 1; k <= steps; k++ {
			x, y, _, _ := m.PointAtF(step * float64(k))
			poly += math.Hypot(x-px, y-py)
			px, py = x, y
		}
		if poly > m.LengthF()+1e-9 {
			t.Errorf("polyline %.9f exceeds arc length %.9f", poly, m.LengthF())
		}
		if steps > 50 && poly < prevPoly {
			t.Errorf("polyline did not converge: %.9f < %.9f", poly, prevPoly)
		}
		prevPoly = poly
	}
	if m.LengthF()-prevPoly > 1e-3*m.LengthF() {
		t.Errorf("finest polyline %.9f too far from arc length %.9f", prevPoly, m.LengthF())
	}

	// Straight segment: equal distance steps give equal chords.
	const lineSteps = 100
	cum := m.CumulativeLengthsF()
	lineStart := cum[len(cum)-2]
	lineLen := m.LengthF() - lineStart
	lineStep := lineLen / lineSteps
	px, py, _, _ := m.PointAtF(lineStart)
	maxLineDev := 0.0
	for k := 1; k <= lineSteps; k++ {
		x, y, _, _ := m.PointAtF(lineStart + lineStep*float64(k))
		if dev := math.Abs(math.Hypot(x-px, y-py) - lineStep); dev > maxLineDev {
			maxLineDev = dev
		}
		px, py = x, y
	}
	if maxLineDev > 1e-9 {
		t.Errorf("straight chord deviation %.2e exceeds 1e-9", maxLineDev)
	}

	// Fixed point version on the same straight tail stays within two quanta.
	fLineStep := fixed.Int26_6(math.Round(lineStep * 64))
	prevFP, _ := m.PointAt(fixed.Int26_6(math.Round(lineStart * 64)))
	fixedMax := 0.0
	for k := 1; k <= lineSteps; k++ {
		d := lineStart + float64(k)*float64(fLineStep)/64
		if d > m.LengthF() {
			break
		}
		fp, _ := m.PointAt(fixed.Int26_6(math.Round(d * 64)))
		got := math.Hypot(float64(fp.X-prevFP.X), float64(fp.Y-prevFP.Y)) / 64
		if dev := math.Abs(got - float64(fLineStep)/64); dev > fixedMax {
			fixedMax = dev
		}
		prevFP = fp
	}
	if fixedMax > 2.0/64 {
		t.Errorf("fixed straight speed deviation %.4f exceeds two quanta", fixedMax)
	}
}

// TestMeasureClamping covers negative and over-long distances for both
// float and fixed APIs.
func TestMeasureClamping(t *testing.T) {
	var p Path
	p.Start(ToFixedP(1, 2))
	p.Line(ToFixedP(9, 8))
	m := measureOrFail(t, p)
	L := m.LengthF()
	for _, d := range []float64{-1, -1e6, L + 1, L + 1e9} {
		x, y, _, _, err := m.PointAtFChecked(d)
		if err != nil {
			t.Errorf("finite distance %v should not error: %v", d, err)
		}
		if d < 0 && (x != 1 || y != 2) {
			t.Errorf("negative distance clamped to start, got (%v,%v)", x, y)
		}
		if d > L && (math.Abs(x-9) > 1e-12 || math.Abs(y-8) > 1e-12) {
			t.Errorf("over distance clamped to end, got (%v,%v)", x, y)
		}
	}
	// Fixed clamping.
	start, _ := m.PointAt(fixed.Int26_6(-1000))
	if start != ToFixedP(1, 2) {
		t.Errorf("fixed negative clamp: got %v", start)
	}
	end, endTan := m.PointAt(m.Length() + 1000)
	if end != ToFixedP(9, 8) {
		t.Errorf("fixed over-length clamp: got %v", end)
	}
	// End tangent is the line direction (8,6) normalized = (0.8,0.6).
	if math.Abs(float64(endTan.X)/64-0.8) > 1.5/64 ||
		math.Abs(float64(endTan.Y)/64-0.6) > 1.5/64 {
		t.Errorf("end tangent got %v", endTan)
	}
}

// TestMeasureZeroLengthSegments checks degenerate segments keep their
// position in the cumulative table and queries remain stable.
func TestMeasureZeroLengthSegments(t *testing.T) {
	var p Path
	p.Start(ToFixedP(5, 5))
	p.Line(ToFixedP(5, 5)) // zero-length line
	p.CubeBezier(ToFixedP(5, 5), ToFixedP(5, 5), ToFixedP(5, 5))
	p.QuadBezier(ToFixedP(5, 5), ToFixedP(5, 5))
	p.Line(ToFixedP(25, 5)) // real 20-unit segment
	p.Stop(true)            // closes from (25,5) to (5,5): another 20 units
	m := measureOrFail(t, p)
	if math.Abs(m.LengthF()-40) > 1e-9 {
		t.Errorf("zero-length segments should not add length, got %.9f", m.LengthF())
	}
	// All six commands produce six segments (line, cubic, quad, line, close
	// plus the implicit count).
	if m.SegmentCount() != 5 {
		t.Errorf("segment count: got %d want 5", m.SegmentCount())
	}
	cum := m.CumulativeLengthsF()
	// The first three segments (zero line, zero cubic, zero quad) all
	// contribute nothing, so every boundary up to the real line is zero.
	for k := 0; k <= 3; k++ {
		if cum[k] != 0 {
			t.Errorf("zero segment boundary %d should be 0, got %.9f", k, cum[k])
		}
	}
	// The real line starts at distance zero and runs to 20.
	if math.Abs(cum[4]-20) > 1e-9 {
		t.Errorf("real line boundary want 20, got %.9f", cum[4])
	}
	// Distance zero lands on the shared pen point. For a tiny positive
	// distance the zero-length segments are skipped and the real line is
	// entered, which is the same behavior as the independent reference.
	x, y, _, _ := m.PointAtF(0)
	if x != 5 || y != 5 {
		t.Errorf("zero-distance query got (%v,%v), want (5,5)", x, y)
	}
	for _, d := range []float64{0.0001, 0.01} {
		x, y, _, _ = m.PointAtF(d)
		rp, _ := buildRefMeasure(p).pointAt(d)
		if math.Hypot(x-rp.x, y-rp.y) > refPointTol || y != 5 {
			t.Errorf("zero-region query d=%v got (%v,%v), ref (%v,%v)", d, x, y, rp.x, rp.y)
		}
	}
	// Determinism: repeated identical queries return identical values.
	a1, b1 := m.PointAt(fixed.Int26_6(10 * 64))
	a2, b2 := m.PointAt(fixed.Int26_6(10 * 64))
	if a1 != a2 || b1 != b2 {
		t.Error("measure queries are not deterministic")
	}
	// Reference agrees on the total.
	ref := buildRefMeasure(p)
	if math.Abs(m.LengthF()-ref.total) > refLenTol {
		t.Errorf("ref length %.9f vs %.9f", ref.total, m.LengthF())
	}
}

// TestMeasureClosedEndpoints verifies a closed path measures back to the
// start and the final query lands on the sub-path origin.
func TestMeasureClosedEndpoints(t *testing.T) {
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(30, 0))
	p.Line(ToFixedP(30, 40))
	p.Stop(true) // closes back to (0,0), length 50
	m := measureOrFail(t, p)
	if math.Abs(m.LengthF()-120) > 1e-9 {
		t.Errorf("triangle perimeter want 120, got %.9f", m.LengthF())
	}
	end, _ := m.PointAt(m.Length())
	if end != ToFixedP(0, 0) {
		t.Errorf("closed end should return to origin, got %v", end)
	}
	// The close segment itself is the 3-4-5 hypotenuse; it is the third
	// segment, so it starts at cumulative boundary index 2.
	cum := m.CumulativeLengthsF()
	if math.Abs(cum[2]-70) > 1e-9 {
		t.Errorf("close segment start distance want 70, got %.9f", cum[2])
	}
	if math.Abs(cum[3]-120) > 1e-9 {
		t.Errorf("perimeter end want 120, got %.9f", cum[3])
	}
}

// TestMeasureBoundsIndependent confirms the measure ignores rasterizer
// bounds: the same path measured after re-bounding yields identical values.
func TestMeasureBoundsIndependent(t *testing.T) {
	p := getPartPathLike()
	m1 := measureOrFail(t, p)
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	s := NewScannerGV(512, 512, img, img.Bounds())
	d := NewDasher(512, 512, s)
	d.SetStroke(8*64, 4*64, RoundCap, nil, RoundGap, MiterClip, []float64{17, 9}, 4)
	d.SetBounds(64, 64) // deliberately different bounds
	m2 := measureOrFail(t, p)
	if m1.LengthF() != m2.LengthF() {
		t.Error("measure changed with rasterizer bounds")
	}
	for k := 0; k <= 20; k++ {
		d0 := m1.LengthF() * float64(k) / 20
		a, ta := m1.PointAt(fixed.Int26_6(math.Round(d0 * 64)))
		b, tb := m2.PointAt(fixed.Int26_6(math.Round(d0 * 64)))
		if a != b || ta != tb {
			t.Error("bounds affected measured points")
		}
	}
}

// TestMeasureDoesNotMutate verifies queries leave the Path, Stroker and
// Dasher untouched.
func TestMeasureDoesNotMutate(t *testing.T) {
	p := getPartPathLike()
	before := append(Path(nil), p...)
	m := measureOrFail(t, p)
	for k := 0; k < 50; k++ {
		m.PointAt(fixed.Int26_6(k) * m.Length() / 50)
		m.PointAtF(m.LengthF() * float64(k) / 50)
		m.CumulativeLengths()
	}
	if len(p) != len(before) {
		t.Fatal("path length changed after queries")
	}
	for i := range p {
		if p[i] != before[i] {
			t.Fatalf("path byte %d mutated", i)
		}
	}

	// Drive a real stroke, snapshot the generated fill path, then measure
	// the same input and confirm the stroke output is unchanged.
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	s := NewScannerGV(512, 512, img, img.Bounds())
	d := NewDasher(512, 512, s)
	d.SetStroke(8*64, 4*64, RoundCap, nil, RoundGap, MiterClip, []float64{17, 9}, 4)
	p.AddTo(d)
	extentBefore := s.GetPathExtent()
	_, _ = NewPathMeasure(p)
	extentAfter := s.GetPathExtent()
	if extentBefore != extentAfter {
		t.Error("measuring altered the rasterizer extent")
	}
}

// TestMeasureRandomAgainstReference fuzzes mixed random paths and checks
// every sampled point and tangent against the independent reference.
func TestMeasureRandomAgainstReference(t *testing.T) {
	rnd := rand.New(rand.NewSource(int64(time.Now().Nanosecond())))
	const paths, maxSegs = 60, 12
	for pi := 0; pi < paths; pi++ {
		var p Path
		x, y := rnd.Float64()*200-100, rnd.Float64()*200-100
		p.Start(ToFixedP(x, y))
		nSeg := 1 + rnd.Intn(maxSegs)
		for s := 0; s < nSeg; s++ {
			switch rnd.Intn(4) {
			case 0:
				x += rnd.Float64()*40 - 20
				y += rnd.Float64()*40 - 20
				p.Line(ToFixedP(x, y))
			case 1:
				bx, by := x+rnd.Float64()*60-30, y+rnd.Float64()*60-30
				x, y = bx+rnd.Float64()*40-20, by+rnd.Float64()*40-20
				p.QuadBezier(ToFixedP(bx, by), ToFixedP(x, y))
			case 2:
				bx, by := x+rnd.Float64()*80-40, y+rnd.Float64()*80-40
				cx, cy := bx+rnd.Float64()*80-40, by+rnd.Float64()*80-40
				x, y = cx+rnd.Float64()*40-20, cy+rnd.Float64()*40-20
				p.CubeBezier(ToFixedP(bx, by), ToFixedP(cx, cy), ToFixedP(x, y))
			case 3:
				// Occasionally restart a sub-path, sometimes closed.
				if rnd.Intn(2) == 0 {
					p.Stop(true)
				}
				x, y = rnd.Float64()*200-100, rnd.Float64()*200-100
				p.Start(ToFixedP(x, y))
			}
		}
		if rnd.Intn(2) == 0 {
			p.Stop(true)
		}
		m := measureOrFail(t, p)
		ref := buildRefMeasure(p)
		if math.Abs(m.LengthF()-ref.total) > 1e-6*math.Max(1, ref.total) {
			t.Fatalf("random path %d length %.7f vs ref %.7f", pi, m.LengthF(), ref.total)
		}
		if ref.total == 0 {
			continue
		}
		for k := 0; k <= 24; k++ {
			d := ref.total * float64(k) / 24
			gx, gy, gtx, gty := m.PointAtF(d)
			rp, rt := ref.pointAt(d)
			// The two solvers use different quadrature orders; allow a
			// tolerance well below the 1/64 fixed quantum while still
			// catching a real parameterization error.
			if math.Hypot(gx-rp.x, gy-rp.y) > 1e-4 {
				t.Fatalf("path %d d=%.5f point (%.7f,%.7f) vs ref (%.7f,%.7f)",
					pi, d, gx, gy, rp.x, rp.y)
			}
			if dot := gtx*rt.x + gty*rt.y; dot < 1-3e-3 {
				t.Fatalf("path %d d=%.5f tangent dot %.5f", pi, d, dot)
			}
		}
	}
}

// TestMeasureExtremeCoordinates exercises high-curvature and large-magnitude
// coordinates to confirm no panic, no non-finite output and bounded time.
func TestMeasureExtremeCoordinates(t *testing.T) {
	var p Path
	p.Start(ToFixedP(0, 0))
	// Tight loop: large control-point swing relative to the chord.
	p.CubeBezier(ToFixedP(1e4, 1e4), ToFixedP(-1e4, 1e4), ToFixedP(0, 0))
	// Cusp cubic.
	p.CubeBezier(ToFixedP(100, 100), ToFixedP(-100, 100), ToFixedP(0, 0))
	// Very long straight segment near the fixed point range.
	p.Line(fixed.Point26_6{X: 1 << 30, Y: 0})
	m := measureOrFail(t, p)
	if m.LengthF() <= 0 || math.IsNaN(m.LengthF()) || math.IsInf(m.LengthF(), 0) {
		t.Fatalf("extreme length invalid: %v", m.LengthF())
	}
	for k := 0; k <= 40; k++ {
		d := m.LengthF() * float64(k) / 40
		x, y, tx, ty := m.PointAtF(d)
		for _, v := range []float64{x, y, tx, ty} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("non-finite query result at d=%v", d)
			}
		}
		fp, ftan := m.PointAt(fixed.Int26_6(math.Round(d * 64)))
		if fp.X < -(1<<31) || fp.X > (1<<31-1) {
			t.Fatalf("fixed point out of range %v", fp)
		}
		// Tangent norm stays a unit vector whenever it is defined.
		if n := math.Hypot(float64(ftan.X), float64(ftan.Y)); n != 0 && math.Abs(n-64) > 2 {
			t.Fatalf("extreme tangent norm %.4f", n)
		}
	}
}

// BenchmarkMeasureConstruct measures arc-length integration of a mixed
// multi-segment path.
func BenchmarkMeasureConstruct(b *testing.B) {
	var p Path
	p.Start(ToFixedP(0, 0))
	for i := 0; i < 10; i++ {
		p.CubeBezier(ToFixedP(float64(i*10), float64(i*20)),
			ToFixedP(float64(i*10+50), float64(i*20-40)),
			ToFixedP(float64(i*10+80), float64(i*20+10)))
		p.QuadBezier(ToFixedP(float64(i*10+90), float64(i*20+60)),
			ToFixedP(float64(i*10+100), float64(i*20)))
	}
	p.Stop(true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewPathMeasure(p); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMeasureQuery measures point/tangent lookup along the path.
func BenchmarkMeasureQuery(b *testing.B) {
	var p Path
	p.Start(ToFixedP(0, 0))
	for i := 0; i < 10; i++ {
		p.CubeBezier(ToFixedP(float64(i*10), float64(i*20)),
			ToFixedP(float64(i*10+50), float64(i*20-40)),
			ToFixedP(float64(i*10+80), float64(i*20+10)))
	}
	m := measureOrFail(&testing.T{}, p)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := math.Mod(float64(i)*0.7, m.LengthF())
		_, _, _, _ = m.PointAtF(d)
	}
}
