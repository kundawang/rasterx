// Copyright 2026 by the rasterx Authors. All rights reserved.
//_
// hit.go adds geometric hit testing (point/rectangle pick queries) to
// rasterx without rasterizing the whole canvas. The query reuses the exact
// same Filler/Stroker/Dasher pipeline that rendering uses, so cap, join,
// miter, dash and curve-flattening rules are identical to a drawn image.

package rasterx

import (
	"errors"
	"image"
	"math"

	"golang.org/x/image/math/fixed"
)

// Supported envelope for hit geometry. rasterx performs its stroke math in
// fixed.Int26_6 with int32 intermediates, so coordinates and stroke widths
// outside these limits cannot be processed without arithmetic overflow.
// NewHitTester returns ErrHitLimit when the input exceeds them.
const (
	// hitMaxCoord is the largest absolute device coordinate (in Int26_6
	// units) accepted for any path point: 1<<22 == 4,194,304 pixels.
	hitMaxCoord = 1 << 22
	// hitMaxDelta bounds the length of any single segment (control-point
	// delta in Int26_6 units); longer explicit lines are split.
	hitMaxDelta = 1 << 18
	// MaxHitStrokeWidth is the largest stroke width (Int26_6 units,
	// 1024 pixels) guaranteed not to overflow the stroke arithmetic.
	MaxHitStrokeWidth = 1 << 16
	// hitMaxMiterExtent bounds the projected miter length (u * miterLimit
	// in Int26_6 units, i.e. 256 pixels).
	hitMaxMiterExtent = 1 << 14
	// hitMaxDash bounds an individual dash length (Int26_6 units).
	hitMaxDash = 1 << 22
)

// Errors returned by the hit testing API.
var (
	// ErrHitNonFinite is returned when a path, transform, query point or
	// stroke parameter contains a NaN or infinite value.
	ErrHitNonFinite = errors.New("rasterx: hit test input contains a non-finite value")
	// ErrHitSingular is returned when the transform matrix cannot be
	// inverted (zero determinant).
	ErrHitSingular = errors.New("rasterx: hit test transform is singular")
	// ErrHitLimit is returned when a coordinate, segment length, stroke
	// width or miter extent is too large to evaluate safely.
	ErrHitLimit = errors.New("rasterx: hit test input exceeds the supported coordinate or stroke-width envelope")
	// ErrHitBadSample is returned when a rect sample count is out of range.
	ErrHitBadSample = errors.New("rasterx: hit test sample count must be between 1 and 128")
	// ErrHitBadThreshold is returned when a pixel threshold is negative.
	ErrHitBadThreshold = errors.New("rasterx: hit test pixel threshold must not be negative")
)

// HitSource reports which painted part of the shape was hit. Values are bit
// flags, so HitFill|HitStroke means both cover the query.
type HitSource uint8

const (
	// HitNone means neither the fill nor the stroke covers the query.
	HitNone HitSource = 0
	// HitFill means the interior (fill) covers the query.
	HitFill HitSource = 1 << iota
	// HitStroke means the stroked outline covers the query.
	HitStroke
)

// StrokeConfig carries the stroking parameters for a hit query. It mirrors the
// arguments of Dasher.SetStroke; an empty Dashes slice means a solid stroke.
type StrokeConfig struct {
	Width      fixed.Int26_6   // stroke width in Int26_6 units
	MiterLimit fixed.Int26_6   // miter limit in Int26_6 units
	CapL       CapFunc         // leading cap; nil follows SetStroke defaults
	CapT       CapFunc         // trailing cap; nil follows SetStroke defaults
	Gap        GapFunc         // join gap function; nil follows SetStroke defaults
	Join       JoinMode        // join mode
	Dashes     []fixed.Int26_6 // dash pattern in Int26_6 units; nil/empty = solid
	DashOffset fixed.Int26_6   // offset into the dash pattern, Int26_6 units
}

// HitOptions controls a hit query. The zero value is valid: no clipping,
// non-zero winding, 16x16 rect samples and a 0.5 coverage threshold.
type HitOptions struct {
	// Clip restricts the query to this device-pixel rectangle. The zero
	// rectangle disables clipping, matching Scanner.SetClip semantics.
	Clip image.Rectangle
	// EvenOdd selects the even-odd fill rule; the default is non-zero.
	EvenOdd bool
	// SamplesX/SamplesY set the supersampling grid for rectangle queries.
	// Zero values default to 16; values above 128 are rejected.
	SamplesX, SamplesY int
	// CoverageThreshold is the fraction [0,1] of sampled points inside the
	// geometry (after applying PixelThreshold) required for a rectangle to
	// count as a hit. Zero defaults to 0.5.
	CoverageThreshold float64
}

// HitResult is the outcome of a point or rectangle hit query.
type HitResult struct {
	// Source is a bitwise combination of HitFill and HitStroke.
	Source HitSource
	// FillCoverage and StrokeCoverage are in [0,1]. For a point query they
	// are exactly 0 or 1 (boundary included). For a rectangle query they are
	// the supersampled area coverage, ignoring PixelThreshold.
	FillCoverage, StrokeCoverage float64
	// FillDistance and StrokeDistance are signed distances in device pixels
	// from the query point to the fill/stroke boundary: negative inside,
	// zero on the boundary, positive outside. They are only populated for
	// point queries and are zero for rectangle queries.
	FillDistance, StrokeDistance float64
	// Winding is the summed signed crossing count at a point query (0 for
	// rectangle queries). Its parity drives the even-odd rule; its sign
	// drives the non-zero rule.
	Winding int
}

// FillHit reports whether the fill covers the query.
func (h HitResult) FillHit() bool { return h.Source&HitFill != 0 }

// StrokeHit reports whether the stroke covers the query.
func (h HitResult) StrokeHit() bool { return h.Source&HitStroke != 0 }

// AnyHit reports whether either the fill or the stroke covers the query.
func (h HitResult) AnyHit() bool { return h.Source != HitNone }

// hitEdge is one directed line segment emitted by the raster pipeline. A
// stroked outline is not a single closed polygon: the Stroker/Dasher emits
// many short contours (offset edge pairs, cap triangles, join wedges) whose
// combined edge set is resolved by the rasterizer's winding fill. Keeping
// the raw directed edges reproduces that resolution exactly.
type hitEdge struct{ a, b fixed.Point26_6 }

// hitGeometry is the captured directed edge set of one paint (fill or
// stroke), plus a coarse bounds for quick rejection.
type hitGeometry struct {
	edges    []hitEdge
	min, max fixed.Point26_6
}

// hitScanner is a Scanner implementation that records directed edges instead
// of rasterizing. It is the geometry sink of the hit pipeline.
type hitScanner struct {
	geom    hitGeometry
	pen     fixed.Point26_6
	started bool
	nonZero bool
}

func (s *hitScanner) acc(a fixed.Point26_6) {
	g := &s.geom
	if a.X < g.min.X {
		g.min.X = a.X
	}
	if a.Y < g.min.Y {
		g.min.Y = a.Y
	}
	if a.X > g.max.X {
		g.max.X = a.X
	}
	if a.Y > g.max.Y {
		g.max.Y = a.Y
	}
}

// Start implements Scanner. MoveTo simply repositions the pen; no implicit
// edge is added, exactly as in golang.org/x/image/vector.
func (s *hitScanner) Start(a fixed.Point26_6) {
	s.pen, s.started = a, true
	s.acc(a)
}

// Line implements Scanner and stores the directed edge pen -> b.
func (s *hitScanner) Line(b fixed.Point26_6) {
	if !s.started { // tolerate a missing Start
		s.Start(b)
		return
	}
	if b != s.pen { // zero-length edges contribute nothing
		s.geom.edges = append(s.geom.edges, hitEdge{a: s.pen, b: b})
	}
	s.pen = b
	s.acc(b)
}

// Draw implements Scanner; all geometry is already captured as edges.
func (s *hitScanner) Draw() {}

// GetPathExtent implements Scanner.
func (s *hitScanner) GetPathExtent() fixed.Rectangle26_6 {
	return fixed.Rectangle26_6{Min: s.geom.min, Max: s.geom.max}
}

// SetBounds implements Scanner. Bounds are irrelevant for geometry capture.
func (s *hitScanner) SetBounds(w, h int) {}

// SetColor implements Scanner. Color (including alpha) never changes the
// geometry of a hit query, so it is accepted and ignored.
func (s *hitScanner) SetColor(color interface{}) {}

// SetWinding implements Scanner.
func (s *hitScanner) SetWinding(useNonZeroWinding bool) { s.nonZero = useNonZeroWinding }

// SetClip implements Scanner. Hit clipping is handled by HitOptions.
func (s *hitScanner) SetClip(rect image.Rectangle) {}

// Clear implements Scanner.
// Clear implements Scanner and restores the bounds sentinels; it is called
// by NewFiller/NewStroker during construction, so it must not leave the
// extent at the zero point.
func (s *hitScanner) Clear() {
	*s = *newHitScanner()
}

// validateAdder carries path-validation helpers. Path commands are validated
// up front in checkPath before anything reaches the fixed-point pipeline.
type validateAdder struct{}

func finiteFixed(p fixed.Point26_6) bool {
	// Int26_6 values are always finite; this guards against a transformed
	// value that wrapped to the Int26_6 extremes after a non-finite matrix.
	return int64(p.X) > math.MinInt32 && int64(p.X) < math.MaxInt32 &&
		int64(p.Y) > math.MinInt32 && int64(p.Y) < math.MaxInt32
}

func (validateAdder) check(p fixed.Point26_6) error {
	if !finiteFixed(p) {
		return ErrHitNonFinite
	}
	if p.X < -hitMaxCoord || p.X > hitMaxCoord ||
		p.Y < -hitMaxCoord || p.Y > hitMaxCoord {
		return ErrHitLimit
	}
	return nil
}

// splitAdder subdivides explicit line segments longer than hitMaxDelta so
// the downstream fixed-point stroke math cannot overflow. Curves are already
// subdivided by QuadTo/CubeTo with the same flattening rule as rendering.
type splitAdder struct {
	next Adder
	last fixed.Point26_6
	have bool
}

func (l *splitAdder) Start(a fixed.Point26_6) {
	l.last, l.have = a, true
	l.next.Start(a)
}

func (l *splitAdder) Line(b fixed.Point26_6) {
	if !l.have {
		l.Start(b)
		return
	}
	dx, dy := int64(b.X)-int64(l.last.X), int64(b.Y)-int64(l.last.Y)
	adx := dx
	if adx < 0 {
		adx = -adx
	}
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	n := int(adx/hitMaxDelta + 1)
	if m := int(ady/hitMaxDelta + 1); m > n {
		n = m
	}
	for i := 1; i <= n; i++ {
		p := fixed.Point26_6{
			X: fixed.Int26_6(int64(l.last.X) + dx*int64(i)/int64(n)),
			Y: fixed.Int26_6(int64(l.last.Y) + dy*int64(i)/int64(n)),
		}
		l.next.Line(p)
	}
	l.last = b
}

func (l *splitAdder) QuadBezier(b, c fixed.Point26_6) {
	l.next.QuadBezier(b, c)
	l.last, l.have = c, true
}

func (l *splitAdder) CubeBezier(b, c, d fixed.Point26_6) {
	l.next.CubeBezier(b, c, d)
	l.last, l.have = d, true
}

func (l *splitAdder) Stop(closeLoop bool) {
	l.next.Stop(closeLoop)
	l.have = false
}

// HitTester is a reusable, read-only hit query object. It is built once from
// a path and can answer any number of point/rectangle queries concurrently;
// queries never mutate it, the source path, or any scanner state.
type HitTester struct {
	fill      hitGeometry
	stroke    hitGeometry
	nonZero   bool
	clip      image.Rectangle
	hasStroke bool
}

// validateStrokeConfig checks stroke parameters before any fixed-point math.
func validateStrokeConfig(cfg *StrokeConfig) error {
	if cfg == nil {
		return nil
	}
	w := int64(cfg.Width)
	if w < 0 {
		return ErrHitNonFinite
	}
	if w > MaxHitStrokeWidth {
		return ErrHitLimit
	}
	if int64(cfg.MiterLimit) < 0 {
		return ErrHitNonFinite
	}
	// The stroker stores u*mLimit>>6; bound the projected miter extent.
	if (int64(cfg.Width)/2)*int64(cfg.MiterLimit) > int64(hitMaxMiterExtent)<<6 {
		return ErrHitLimit
	}
	for _, d := range cfg.Dashes {
		if int64(d) < 0 {
			return ErrHitNonFinite
		}
		if int64(d) > hitMaxDash {
			return ErrHitLimit
		}
	}
	return nil
}

// checkPath runs every path command through finite/envelope validation
// without sending anything to the raster pipeline, and returns the path's
// local-space bounds.
func checkPath(p Path) (min, max fixed.Point26_6, err error) {
	min = fixed.Point26_6{X: hitMaxCoord, Y: hitMaxCoord}
	max = fixed.Point26_6{X: -hitMaxCoord, Y: -hitMaxCoord}
	v := validateAdder{}
	have := false
	acc := func(q fixed.Point26_6) {
		have = true
		if q.X < min.X {
			min.X = q.X
		}
		if q.Y < min.Y {
			min.Y = q.Y
		}
		if q.X > max.X {
			max.X = q.X
		}
		if q.Y > max.Y {
			max.Y = q.Y
		}
	}
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			if i+2 >= len(p) {
				return min, max, ErrHitLimit
			}
			a := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			if err := v.check(a); err != nil {
				return min, max, err
			}
			acc(a)
			i += 3
		case PathLineTo:
			if i+2 >= len(p) {
				return min, max, ErrHitLimit
			}
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			if err := v.check(b); err != nil {
				return min, max, err
			}
			acc(b)
			i += 3
		case PathQuadTo:
			if i+4 >= len(p) {
				return min, max, ErrHitLimit
			}
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			if err := v.check(b); err != nil {
				return min, max, err
			}
			if err := v.check(c); err != nil {
				return min, max, err
			}
			acc(b)
			acc(c)
			i += 5
		case PathCubicTo:
			if i+6 >= len(p) {
				return min, max, ErrHitLimit
			}
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			d := fixed.Point26_6{X: p[i+5], Y: p[i+6]}
			if err := v.check(b); err != nil {
				return min, max, err
			}
			if err := v.check(c); err != nil {
				return min, max, err
			}
			if err := v.check(d); err != nil {
				return min, max, err
			}
			acc(b)
			acc(c)
			acc(d)
			i += 7
		case PathClose:
			i++
		default:
			return min, max, ErrHitLimit
		}
	}
	if !have {
		return fixed.Point26_6{}, fixed.Point26_6{}, nil
	}
	return min, max, nil
}

// checkTransformedBounds verifies that the four corners of the local bounds,
// after applying m, stay inside the device-space envelope. An affine map
// sends the bounding rectangle into a parallelogram whose every point is a
// convex combination of the transformed corners, so checking the corners is
// sufficient and also guarantees MatrixAdder.TFixed cannot wrap an Int26_6.
func checkTransformedBounds(min, max fixed.Point26_6, m Matrix2D) error {
	corners := [4]fixed.Point26_6{
		{X: min.X, Y: min.Y}, {X: max.X, Y: min.Y},
		{X: max.X, Y: max.Y}, {X: min.X, Y: max.Y},
	}
	for _, c := range corners {
		t := m.TFixed(c)
		if !finiteFixed(t) ||
			t.X < -hitMaxCoord || t.X > hitMaxCoord ||
			t.Y < -hitMaxCoord || t.Y > hitMaxCoord {
			return ErrHitLimit
		}
	}
	return nil
}

// finiteMatrix rejects NaN/Inf matrix entries.
func finiteMatrix(m Matrix2D) bool {
	for _, v := range [6]float64{m.A, m.B, m.C, m.D, m.E, m.F} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func newHitScanner() *hitScanner {
	return &hitScanner{
		geom: hitGeometry{
			// min starts beyond the positive envelope (no point is larger,
			// so the first point always replaces it) and max starts beyond
			// the negative envelope, covering points in any quadrant.
			min: fixed.Point26_6{X: hitMaxCoord + 1, Y: hitMaxCoord + 1},
			max: fixed.Point26_6{X: -hitMaxCoord - 1, Y: -hitMaxCoord - 1},
		},
	}
}

// NewHitTester builds a HitTester for p using the given transform (Identity is
// valid), optional stroke configuration (nil tests the fill only) and fill
// rule options. The source path is never modified. An empty path yields a
// tester that reports no hits. Non-finite coordinates, a singular transform
// and over-limit stroke widths are rejected with the documented errors.
func NewHitTester(p Path, m Matrix2D, stroke *StrokeConfig, opts HitOptions) (*HitTester, error) {
	if !finiteMatrix(m) {
		return nil, ErrHitNonFinite
	}
	// A 2D affine matrix is singular iff its linear part has zero
	// determinant; reject it so hit semantics stay well defined.
	if det := m.A*m.D - m.B*m.C; det == 0 || math.IsNaN(det) || math.IsInf(det, 0) {
		return nil, ErrHitSingular
	}
	localMin, localMax, err := checkPath(p)
	if err != nil {
		return nil, err
	}
	if err := checkTransformedBounds(localMin, localMax, m); err != nil {
		return nil, err
	}
	if err := validateStrokeConfig(stroke); err != nil {
		return nil, err
	}

	ht := &HitTester{nonZero: !opts.EvenOdd, clip: opts.Clip}

	// Fill pipeline: transform -> line splitting -> Filler -> edge sink.
	fillScanner := newHitScanner()
	filler := NewFiller(1, 1, fillScanner)
	filler.SetWinding(!opts.EvenOdd)
	p.AddTo(&splitAdder{next: &MatrixAdder{Adder: filler, M: m}})
	ht.fill = fillScanner.geom

	if stroke != nil && stroke.Width > 0 {
		// Stroke pipeline: transform -> line splitting -> Stroker/Dasher
		// (the same code path and cap/join/miter/dash rules as rendering).
		strokeScanner := newHitScanner()
		var strokeAdder Adder
		if len(stroke.Dashes) == 0 {
			s := NewStroker(1, 1, strokeScanner)
			s.SetStroke(stroke.Width, stroke.MiterLimit,
				stroke.CapL, stroke.CapT, stroke.Gap, stroke.Join)
			strokeAdder = s
		} else {
			d := NewDasher(1, 1, strokeScanner)
			dashes := make([]float64, len(stroke.Dashes))
			for i, v := range stroke.Dashes {
				dashes[i] = float64(v) / 64.0
			}
			d.SetStroke(stroke.Width, stroke.MiterLimit,
				stroke.CapL, stroke.CapT, stroke.Gap, stroke.Join,
				dashes, float64(stroke.DashOffset)/64.0)
			strokeAdder = d
		}
		p.AddTo(&splitAdder{next: &MatrixAdder{Adder: strokeAdder, M: m}})
		ht.stroke = strokeScanner.geom
		ht.hasStroke = true
	}
	return ht, nil
}

// ---- Exact point/edge geometry in Int26_6 arithmetic ----------------------

// onSegment reports whether p lies on segment ab. All cross/dot products are
// evaluated in int64, so boundary tangency is exact and deterministic.
func onSegment(a, b, p fixed.Point26_6) bool {
	ax, ay := int64(a.X), int64(a.Y)
	bx, by := int64(b.X), int64(b.Y)
	px, py := int64(p.X), int64(p.Y)
	cross := (bx-ax)*(py-ay) - (by-ay)*(px-ax)
	if cross != 0 {
		return false
	}
	dot := (px-ax)*(px-bx) + (py-ay)*(py-by)
	return dot <= 0
}

// edgeWinding returns the signed crossing count of p against the directed
// edge set. A point exactly on an edge reports onEdge, giving boundary
// tangency a defined and stable semantics (the boundary counts as inside).
//
// The ray is the horizontal line through p extending to +x. The test uses
// the standard winding-number rule with a half-open edge: a crossing counts
// only when an edge straddles the ray with its start strictly on one side.
// Screen coordinates grow downward, so "upward" means decreasing y.
func edgeWinding(edges []hitEdge, p fixed.Point26_6) (winding int, onEdge bool) {
	px, py := int64(p.X), int64(p.Y)
	for _, e := range edges {
		ax, ay := int64(e.a.X), int64(e.a.Y)
		bx, by := int64(e.b.X), int64(e.b.Y)
		if onSegment(e.a, e.b, p) {
			return 1, true
		}
		// cross > 0 means the intersection of the edge with the ray lies to
		// the right of p (the ray points toward +x).
		cross := (bx-ax)*(py-ay) - (by-ay)*(px-ax)
		switch {
		case ay <= py && by > py: // edge crosses upward (toward -y), ray hits it
			if cross > 0 {
				winding++
			}
		case ay > py && by <= py: // edge crosses downward (toward +y)
			if cross < 0 {
				winding--
			}
		}
	}
	return winding, false
}

// pointIn reports whether p is covered by the edge set under the winding
// rule, plus the summed signed crossing count. A coarse bounds check rejects
// points outside every edge without iterating the edge set.
func (g hitGeometry) pointIn(p fixed.Point26_6, nonZero bool) (inside bool, winding int) {
	if len(g.edges) == 0 {
		return false, 0
	}
	if p.X < g.min.X || p.X > g.max.X || p.Y < g.min.Y || p.Y > g.max.Y {
		return false, 0
	}
	winding, edge := edgeWinding(g.edges, p)
	if edge {
		return true, 1
	}
	if nonZero {
		return winding != 0, winding
	}
	return winding&1 != 0, winding
}

// distToEdgeSet returns the Euclidean distance from p to the nearest edge in
// Int26_6 units.
func (g hitGeometry) distToEdgeSet(p fixed.Point26_6) float64 {
	best := math.Inf(1)
	px, py := float64(p.X), float64(p.Y)
	for _, e := range g.edges {
		ax, ay := float64(e.a.X), float64(e.a.Y)
		bx, by := float64(e.b.X), float64(e.b.Y)
		dx, dy := bx-ax, by-ay
		l2 := dx*dx + dy*dy
		if l2 == 0 {
			continue
		}
		t := ((px-ax)*dx + (py-ay)*dy) / l2
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
		cx, cy := ax+t*dx, ay+t*dy
		if d := math.Hypot(px-cx, py-cy); d < best {
			best = d
		}
	}
	return best
}

// signedDistance returns the signed distance from p to the painted region:
// negative inside, zero on the boundary, positive outside.
func (g hitGeometry) signedDistance(p fixed.Point26_6, nonZero bool) (float64, bool, int) {
	inside, winding := g.pointIn(p, nonZero)
	if len(g.edges) == 0 {
		return math.Inf(1), false, 0
	}
	best := g.distToEdgeSet(p)
	if inside {
		return -best, true, winding
	}
	return best, false, winding
}

// pointInClip applies the optional clip rectangle to a device-space point.
func (ht *HitTester) pointInClip(p fixed.Point26_6) bool {
	if ht.clip == image.ZR {
		return true
	}
	x := float64(p.X) / 64.0
	y := float64(p.Y) / 64.0
	return x >= float64(ht.clip.Min.X) && x < float64(ht.clip.Max.X) &&
		y >= float64(ht.clip.Min.Y) && y < float64(ht.clip.Max.Y)
}

// hitPixelLimit is the largest absolute device-pixel coordinate accepted by
// point and rectangle queries (hitMaxCoord Int26_6 units / 64).
const hitPixelLimit = hitMaxCoord / 64

// pointToFixed converts a device-pixel coordinate pair to Int26_6, rejecting
// values whose rounded fixed representation would leave the envelope or wrap
// an int32.
func pointToFixed(x, y float64) (fixed.Point26_6, error) {
	if math.Abs(x) > hitPixelLimit || math.Abs(y) > hitPixelLimit {
		return fixed.Point26_6{}, ErrHitLimit
	}
	fx, fy := math.Round(x*64), math.Round(y*64)
	if fx > math.MaxInt32 || fx < math.MinInt32 ||
		fy > math.MaxInt32 || fy < math.MinInt32 {
		return fixed.Point26_6{}, ErrHitLimit
	}
	return fixed.Point26_6{X: fixed.Int26_6(fx), Y: fixed.Int26_6(fy)}, nil
}

// thresholdToFixed converts a non-negative pixel threshold to Int26_6,
// rejecting values outside the geometry envelope.
func thresholdToFixed(thresh float64) (fixed.Int26_6, error) {
	if thresh > hitPixelLimit {
		return 0, ErrHitLimit
	}
	return fixed.Int26_6(math.Round(thresh * 64)), nil
}

// HitPoint answers a point query at device-pixel coordinates (x, y). The
// pixel threshold grows the pickable region around thin strokes: any point
// within threshold pixels of the geometry counts as a hit, while coverage
// stays strictly 0/1. It does not mutate the tester or any source state.
func (ht *HitTester) HitPoint(x, y, pixelThreshold float64) (HitResult, error) {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return HitResult{}, ErrHitNonFinite
	}
	if math.IsNaN(pixelThreshold) || math.IsInf(pixelThreshold, 0) || pixelThreshold < 0 {
		return HitResult{}, ErrHitBadThreshold
	}
	p, err := pointToFixed(x, y)
	if err != nil {
		return HitResult{}, err
	}
	res := HitResult{}
	if !ht.pointInClip(p) {
		return res, nil
	}
	thresh, err := thresholdToFixed(pixelThreshold)
	if err != nil {
		return HitResult{}, err
	}

	dFill, fillInside, w := ht.fill.signedDistance(p, ht.nonZero)
	res.Winding = w
	res.FillDistance = dFill / 64.0
	if fillInside {
		res.Source |= HitFill
		res.FillCoverage = 1
	} else if dFill <= float64(thresh) {
		res.Source |= HitFill
	}

	if ht.hasStroke {
		dStroke, strokeInside, _ := ht.stroke.signedDistance(p, true)
		res.StrokeDistance = dStroke / 64.0
		if strokeInside {
			res.Source |= HitStroke
			res.StrokeCoverage = 1
		} else if dStroke <= float64(thresh) {
			res.Source |= HitStroke
		}
	}
	return res, nil
}

// HitPointF is the fixed-point convenience wrapper around HitPoint.
func (ht *HitTester) HitPointF(p fixed.Point26_6, pixelThreshold float64) (HitResult, error) {
	return ht.HitPoint(float64(p.X)/64.0, float64(p.Y)/64.0, pixelThreshold)
}

// HitRect answers a rectangle query at device-pixel rectangle r. It returns
// the supersampled fill and stroke coverage (fraction of sample points
// covered) and reports a hit when the coverage after applying the pixel
// threshold meets opts.CoverageThreshold (default 0.5).
func (ht *HitTester) HitRect(r image.Rectangle, pixelThreshold float64, opts HitOptions) (HitResult, error) {
	if r.Empty() {
		return HitResult{}, nil
	}
	if math.IsNaN(pixelThreshold) || math.IsInf(pixelThreshold, 0) || pixelThreshold < 0 {
		return HitResult{}, ErrHitBadThreshold
	}
	for _, v := range [4]float64{
		float64(r.Min.X), float64(r.Min.Y),
		float64(r.Max.X), float64(r.Max.Y),
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return HitResult{}, ErrHitNonFinite
		}
		if math.Abs(v) > hitPixelLimit {
			return HitResult{}, ErrHitLimit
		}
	}

	nx, ny := opts.SamplesX, opts.SamplesY
	if nx == 0 {
		nx = 16
	}
	if ny == 0 {
		ny = 16
	}
	if nx < 1 || nx > 128 || ny < 1 || ny > 128 {
		return HitResult{}, ErrHitBadSample
	}
	covThresh := opts.CoverageThreshold
	if covThresh == 0 {
		covThresh = 0.5
	}
	if covThresh < 0 || covThresh > 1 || math.IsNaN(covThresh) {
		return HitResult{}, ErrHitBadThreshold
	}

	clip := ht.clip
	if clip != image.ZR {
		r = r.Intersect(clip)
		if r.Empty() {
			return HitResult{}, nil
		}
	}
	thresh, err := thresholdToFixed(pixelThreshold)
	if err != nil {
		return HitResult{}, err
	}

	minX := fixed.Int26_6(r.Min.X << 6)
	minY := fixed.Int26_6(r.Min.Y << 6)
	spanX := fixed.Int26_6((r.Dx()) << 6)
	spanY := fixed.Int26_6((r.Dy()) << 6)

	total := nx * ny
	fillCount, strokeCount := 0, 0
	fillExpand, strokeExpand := 0, 0
	for iy := 0; iy < ny; iy++ {
		py := minY + fixed.Int26_6((int64(2*iy+1)*int64(spanY))/(2*int64(ny)))
		for ix := 0; ix < nx; ix++ {
			px := minX + fixed.Int26_6((int64(2*ix+1)*int64(spanX))/(2*int64(nx)))
			p := fixed.Point26_6{X: px, Y: py}

			dFill, inside, _ := ht.fill.signedDistance(p, ht.nonZero)
			if inside {
				fillCount++
				fillExpand++
			} else if dFill <= float64(thresh) {
				fillExpand++
			}

			if ht.hasStroke {
				dStroke, sInside, _ := ht.stroke.signedDistance(p, true)
				if sInside {
					strokeCount++
					strokeExpand++
				} else if dStroke <= float64(thresh) {
					strokeExpand++
				}
			}
		}
	}

	res := HitResult{
		FillCoverage:   float64(fillCount) / float64(total),
		StrokeCoverage: float64(strokeCount) / float64(total),
	}
	if float64(fillExpand)/float64(total) >= covThresh {
		res.Source |= HitFill
	}
	if float64(strokeExpand)/float64(total) >= covThresh {
		res.Source |= HitStroke
	}
	return res, nil
}

// FillEdges reports the number of captured fill edges; mainly useful for
// tests and diagnostics.
func (ht *HitTester) FillEdges() int { return len(ht.fill.edges) }

// StrokeEdges reports the number of captured stroke edges; mainly useful for
// tests and diagnostics.
func (ht *HitTester) StrokeEdges() int { return len(ht.stroke.edges) }
