// Copyright 2026 by the rasterx Authors. All rights reserved.
// _
// bounds.go provides tight coverage bounds queries for filled and stroked
// paths. All queries are pure calculations: they never mutate the supplied
// Path, the MatrixAdder, the Dasher/Stroker state or any caller object.
package rasterx

import (
	"errors"
	"image"
	"math"

	"golang.org/x/image/math/fixed"
)

// Sentinel errors returned by the bounds queries. Callers can use errors.Is
// to distinguish them; wrapped errors keep the same sentinel.
var (
	// ErrSingularMatrix is returned when the Matrix2D linear part is not
	// invertible (its determinant is zero or within the singular epsilon).
	ErrSingularMatrix = errors.New("rasterx: bounds matrix is singular")
	// ErrNonFinite is returned when a matrix, style or coordinate parameter
	// contains a NaN or infinite value.
	ErrNonFinite = errors.New("rasterx: bounds parameter is not finite")
	// ErrEmptyPath is returned when the queried Path contains no geometry.
	ErrEmptyPath = errors.New("rasterx: bounds path is empty")
	// ErrInvalidValue is returned for a negative stroke width, a non-positive
	// dash length or another out-of-range style value.
	ErrInvalidValue = errors.New("rasterx: invalid bounds style value")
	// ErrValueOverflow is returned when a transformed coordinate, stroke width
	// or dash length cannot be represented as fixed.Int26_6. The returned
	// rectangle is still a valid conservative box clamped to the representable
	// range, so callers may choose to ignore the error and repaint the clamp.
	ErrValueOverflow = errors.New("rasterx: bounds value overflows fixed.Int26_6")
)

const (
	// singularEps is the minimum absolute determinant of a Matrix2D linear
	// part that is considered invertible.
	singularEps = 1e-12
	// extremaRelEps guards the polynomial root finders against near-zero
	// curvature (a quadratic/cubic derivative coefficient at this scale is
	// treated as zero). It is relative to the magnitude of the control
	// points, so flat and near-flat curves simply fall back to their
	// endpoints, which always bound the curve exactly.
	extremaRelEps = 1e-12
	// boundsRoundEps is the slack (in fixed.Int26_6 units) applied before
	// outward rounding so a mathematically exact integer boundary is not
	// pushed out by floating point noise. 1e-9 of a fixed unit is far below
	// the 1/64 grid resolution.
	boundsRoundEps = 1e-9
	// strokeFlatEps pads the analytic stroke box to absorb the fixed point
	// normal rounding (one fixed unit) and the cubic approximation error of
	// round caps/joins (well under 1e-3 of the radius). Two fixed units,
	// 1/32 of a pixel, is the documented upper bound.
	strokeFlatEps = 2.0 / 64.0
	// miterRatioClamp is the explicit upper bound applied to the miter ratio
	// when converting the join allowance into axis-aligned coverage. Ratios
	// above 32768 cannot produce a larger *representable* Int26_6 box anyway;
	// the clamp keeps near-collinear joins with extreme miter limits finite
	// and predictable. The actual stroker still applies its own miter limit.
	miterRatioClamp = 32768.0
	// capSqrt2 marks the per-axis expansion of square-family end caps.
	capSqrt2 = math.Sqrt2
)

// fixedPixelMax is the largest pixel coordinate representable as fixed.Int26_6.
const fixedPixelMax = float64(math.MaxInt32-1) / 64.0

// CapStyle selects the end cap used by StrokeBounds. It mirrors the package
// CapFunc variables without requiring callers to compare function values.
type CapStyle uint8

const (
	// CapButt is a flat cap ending exactly at the path endpoint.
	CapButt CapStyle = iota
	// CapSquare is a half-width square projecting past the endpoint.
	CapSquare
	// CapRound is a semicircle of the stroke half-width.
	CapRound
	// CapCubic is a cubic bezier cap.
	CapCubic
	// CapQuadratic is a quadratic bezier cap.
	CapQuadratic
)

// StrokeStyle collects the stroking parameters relevant to coverage.
type StrokeStyle struct {
	// Width is the stroke width in transformed device space (the same
	// convention as Stroker.SetStroke fed through a MatrixAdder: the path
	// is mapped by the matrix first, then this width is applied). Must be
	// finite and non-negative; a zero width collapses the stroke to the
	// path spine.
	Width float64
	// MiterLimit is the SVG style miter ratio, e.g. 4 for the package
	// default. Values below 1 are clamped to 1, a zero or negative value
	// selects the default of 4, and values above miterRatioClamp are
	// clamped in the bounds allowance.
	MiterLimit float64
	// Cap selects the cap drawn at open path ends and dash boundaries.
	Cap CapStyle
	// JoinMode selects Arc, ArcClip, Miter, MiterClip, Bevel or Round joins.
	JoinMode JoinMode
	// Dashes is the dash pattern in transformed device space lengths. An
	// empty or nil slice means a solid stroke. Every entry must be finite
	// and strictly positive.
	Dashes []float64
	// DashOffset is the SVG dash phase in transformed device space lengths.
	// It may be negative and is applied exactly as the Dasher applies it.
	DashOffset float64
}

// fPt is a float64 point in pixel units.
type fPt struct{ x, y float64 }

// fRect is an axis aligned float64 box.
type fRect struct {
	minX, minY, maxX, maxY float64
	ok                     bool
}

func (r *fRect) addPoint(x, y float64) {
	if !r.ok {
		r.minX, r.maxX, r.minY, r.maxY = x, x, y, y
		r.ok = true
		return
	}
	if x < r.minX {
		r.minX = x
	}
	if x > r.maxX {
		r.maxX = x
	}
	if y < r.minY {
		r.minY = y
	}
	if y > r.maxY {
		r.maxY = y
	}
}

func (r *fRect) addPt(p fPt) { r.addPoint(p.x, p.y) }

func (r *fRect) union(o fRect) {
	if !o.ok {
		return
	}
	r.addPoint(o.minX, o.minY)
	r.addPoint(o.maxX, o.maxY)
}

func (r *fRect) grow(d float64) {
	if !r.ok {
		return
	}
	r.minX -= d
	r.minY -= d
	r.maxX += d
	r.maxY += d
}

// quadPoint evaluates a quadratic bezier.
func quadPoint(p0, p1, p2 fPt, t float64) fPt {
	mt := 1 - t
	a, b, c := mt*mt, 2*mt*t, t*t
	return fPt{a*p0.x + b*p1.x + c*p2.x, a*p0.y + b*p1.y + c*p2.y}
}

// cubePoint evaluates a cubic bezier.
func cubePoint(p0, p1, p2, p3 fPt, t float64) fPt {
	mt := 1 - t
	a, b, c, d := mt*mt*mt, 3*mt*mt*t, 3*mt*t*t, t*t*t
	return fPt{
		a*p0.x + b*p1.x + c*p2.x + d*p3.x,
		a*p0.y + b*p1.y + c*p2.y + d*p3.y}
}

// addQuadExtrema adds the endpoints and the single derivative root per axis
// of the quadratic segment.
func (r *fRect) addQuadExtrema(p0, p1, p2 fPt) {
	r.addPt(p0)
	r.addPt(p2)
	eps := extremaRelEps * maxF(1, absF(p0.x), absF(p1.x), absF(p2.x),
		absF(p0.y), absF(p1.y), absF(p2.y))
	if dx := p2.x - 2*p1.x + p0.x; math.Abs(dx) > eps {
		if t := (p0.x - p1.x) / dx; t > 0 && t < 1 {
			r.addPt(quadPoint(p0, p1, p2, t))
		}
	}
	if dy := p2.y - 2*p1.y + p0.y; math.Abs(dy) > eps {
		if t := (p0.y - p1.y) / dy; t > 0 && t < 1 {
			r.addPt(quadPoint(p0, p1, p2, t))
		}
	}
}

// addCubeExtrema adds the endpoints and the up to two derivative roots per
// axis of the cubic segment. The quadratic is solved with the stable
// Numerical Recipes formula.
func (r *fRect) addCubeExtrema(p0, p1, p2, p3 fPt) {
	r.addPt(p0)
	r.addPt(p3)
	eps := extremaRelEps * maxF(1, absF(p0.x), absF(p1.x), absF(p2.x), absF(p3.x),
		absF(p0.y), absF(p1.y), absF(p2.y), absF(p3.y))
	addAxisRoots := func(v0, v1, v2, v3 float64, eval func(t float64) fPt) {
		a := 3 * (-v0 + 3*v1 - 3*v2 + v3)
		b := 6 * (v0 - 2*v1 + v2)
		c := 3 * (v1 - v0)
		switch {
		case math.Abs(a) <= eps:
			if math.Abs(b) > eps {
				if t := -c / b; t > 0 && t < 1 {
					r.addPt(eval(t))
				}
			}
		default:
			disc := b*b - 4*a*c
			if disc < 0 {
				return
			}
			sq := math.Sqrt(disc)
			q := -0.5 * (b + math.Copysign(sq, b))
			if t1 := q / a; t1 > 0 && t1 < 1 {
				r.addPt(eval(t1))
			}
			if math.Abs(q) > eps {
				if t2 := c / q; t2 > 0 && t2 < 1 {
					r.addPt(eval(t2))
				}
			}
		}
	}
	addAxisRoots(p0.x, p1.x, p2.x, p3.x, func(t float64) fPt { return cubePoint(p0, p1, p2, p3, t) })
	addAxisRoots(p0.y, p1.y, p2.y, p3.y, func(t float64) fPt { return cubePoint(p0, p1, p2, p3, t) })
}

func absF(v float64) float64 { return math.Abs(v) }

func maxF(first float64, rest ...float64) float64 {
	m := first
	for _, v := range rest {
		if v > m {
			m = v
		}
	}
	return m
}

// validateMatrix checks finiteness and invertibility. An affine matrix with
// a singular linear part collapses area to a line and cannot carry a stroke
// width, so it is rejected rather than silently producing a degenerate box.
func validateMatrix(m Matrix2D) error {
	if !allFinite(m.A, m.B, m.C, m.D, m.E, m.F) {
		return ErrNonFinite
	}
	if math.Abs(m.A*m.D-m.B*m.C) <= singularEps {
		return ErrSingularMatrix
	}
	return nil
}

func allFinite(v ...float64) bool {
	for _, f := range v {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
	}
	return true
}

func mapPoint(m Matrix2D, p fPt) fPt {
	x, y := m.Transform(p.x, p.y)
	return fPt{x, y}
}

func fixedToFloat(p fixed.Point26_6) fPt {
	return fPt{float64(p.X) / 64.0, float64(p.Y) / 64.0}
}

// fp builds a keyed fixed point from two Int26_6 coordinates.
func fp(x, y fixed.Int26_6) fixed.Point26_6 {
	return fixed.Point26_6{X: x, Y: y}
}

// walkFill accumulates the analytic fill box of a transformed path.
func walkFill(p Path, m Matrix2D) (fRect, error) {
	var (
		box      fRect
		cur, sub fPt
		hasSub   bool
	)
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			cur = mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			if !allFinite(cur.x, cur.y) {
				return box, ErrNonFinite
			}
			sub, hasSub = cur, true
			box.addPt(cur)
			i += 3
		case PathLineTo:
			if !hasSub {
				return box, ErrInvalidValue
			}
			cur = mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			if !allFinite(cur.x, cur.y) {
				return box, ErrNonFinite
			}
			box.addPt(cur) // a line is bounded by its two endpoints
			i += 3
		case PathQuadTo:
			if !hasSub {
				return box, ErrInvalidValue
			}
			p1 := mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			p2 := mapPoint(m, fixedToFloat(fp(p[i+3], p[i+4])))
			if !allFinite(p1.x, p1.y, p2.x, p2.y) {
				return box, ErrNonFinite
			}
			box.addQuadExtrema(cur, p1, p2)
			cur = p2
			i += 5
		case PathCubicTo:
			if !hasSub {
				return box, ErrInvalidValue
			}
			p1 := mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			p2 := mapPoint(m, fixedToFloat(fp(p[i+3], p[i+4])))
			p3 := mapPoint(m, fixedToFloat(fp(p[i+5], p[i+6])))
			if !allFinite(p1.x, p1.y, p2.x, p2.y, p3.x, p3.y) {
				return box, ErrNonFinite
			}
			box.addCubeExtrema(cur, p1, p2, p3)
			cur = p3
			i += 7
		case PathClose:
			// Closing adds a straight segment back to sub, already covered.
			cur = sub
			i++
		default:
			return box, ErrInvalidValue
		}
	}
	return box, nil
}

// finalizeRect rounds a float box outward onto the Int26_6 grid. Coordinates
// outside the representable fixed range are clamped and ErrValueOverflow is
// reported, but the returned rectangle is still a conservative box.
func finalizeRect(box fRect) (fixed.Rectangle26_6, error) {
	if !box.ok {
		return fixed.Rectangle26_6{}, ErrEmptyPath
	}
	var err error
	clamp := func(v float64) fixed.Int26_6 {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > fixedPixelMax {
			err = ErrValueOverflow
			if math.IsNaN(v) || v < 0 {
				v = -fixedPixelMax
			} else {
				v = fixedPixelMax
			}
		}
		return fixed.Int26_6(v)
	}
	minX := clamp(math.Floor(box.minX*64.0 - boundsRoundEps))
	minY := clamp(math.Floor(box.minY*64.0 - boundsRoundEps))
	maxX := clamp(math.Ceil(box.maxX*64.0 + boundsRoundEps))
	maxY := clamp(math.Ceil(box.maxY*64.0 + boundsRoundEps))
	return fixed.Rectangle26_6{
		Min: fixed.Point26_6{X: minX, Y: minY},
		Max: fixed.Point26_6{X: maxX, Y: maxY},
	}, err
}

// FillBounds returns the tight axis-aligned coverage rectangle of filling
// the path under matrix m. Curve extrema are found analytically from the
// transformed control points rather than by flattening, so negative
// coordinates and non-zero (translated) minima are handled exactly.
func FillBounds(p Path, m Matrix2D) (fixed.Rectangle26_6, error) {
	if err := validateMatrix(m); err != nil {
		return fixed.Rectangle26_6{}, err
	}
	if len(p) == 0 {
		return fixed.Rectangle26_6{}, ErrEmptyPath
	}
	box, err := walkFill(p, m)
	if err != nil {
		return fixed.Rectangle26_6{}, err
	}
	return finalizeRect(box)
}

// BoundsAdder is a streaming Adder that accumulates analytic fill bounds.
// It is useful when a path is generated incrementally and cannot be stored
// in a Path first. The zero value is not usable; construct it with
// NewBoundsAdder. Points are transformed by M before measurement; pass
// Identity when the points are already transformed (for example when
// BoundsAdder is itself wrapped in a MatrixAdder).
type BoundsAdder struct {
	M       Matrix2D
	box     fRect
	cur     fPt
	sub     fPt
	hasSub  bool
	started bool
	err     error
}

// NewBoundsAdder validates m and returns a ready BoundsAdder.
func NewBoundsAdder(m Matrix2D) (*BoundsAdder, error) {
	if err := validateMatrix(m); err != nil {
		return nil, err
	}
	return &BoundsAdder{M: m}, nil
}

func (b *BoundsAdder) mapOf(a fixed.Point26_6) (fPt, bool) {
	p := mapPoint(b.M, fixedToFloat(a))
	return p, allFinite(p.x, p.y)
}

// Start begins a new sub-path.
func (b *BoundsAdder) Start(a fixed.Point26_6) {
	if b.err != nil {
		return
	}
	p, ok := b.mapOf(a)
	if !ok {
		b.err = ErrNonFinite
		return
	}
	b.cur, b.sub, b.hasSub, b.started = p, p, true, true
	b.box.addPt(p)
}

// Line adds a line segment.
func (b *BoundsAdder) Line(a fixed.Point26_6) {
	if b.err != nil {
		return
	}
	if !b.hasSub {
		b.err = ErrInvalidValue
		return
	}
	p, ok := b.mapOf(a)
	if !ok {
		b.err = ErrNonFinite
		return
	}
	b.box.addPt(p)
	b.cur = p
}

// QuadBezier adds a quadratic segment including its derivative extrema.
func (b *BoundsAdder) QuadBezier(c1, c2 fixed.Point26_6) {
	if b.err != nil {
		return
	}
	if !b.hasSub {
		b.err = ErrInvalidValue
		return
	}
	p1, ok1 := b.mapOf(c1)
	p2, ok2 := b.mapOf(c2)
	if !ok1 || !ok2 {
		b.err = ErrNonFinite
		return
	}
	b.box.addQuadExtrema(b.cur, p1, p2)
	b.cur = p2
}

// CubeBezier adds a cubic segment including its derivative extrema.
func (b *BoundsAdder) CubeBezier(c1, c2, c3 fixed.Point26_6) {
	if b.err != nil {
		return
	}
	if !b.hasSub {
		b.err = ErrInvalidValue
		return
	}
	p1, ok1 := b.mapOf(c1)
	p2, ok2 := b.mapOf(c2)
	p3, ok3 := b.mapOf(c3)
	if !ok1 || !ok2 || !ok3 {
		b.err = ErrNonFinite
		return
	}
	b.box.addCubeExtrema(b.cur, p1, p2, p3)
	b.cur = p3
}

// Stop closes the current sub-path when closeLoop is true. An open stop only
// marks the sub-path boundary; stroke cap allowances are handled separately
// by StrokeBounds.
func (b *BoundsAdder) Stop(closeLoop bool) {
	if b.err != nil {
		return
	}
	if closeLoop && b.hasSub {
		b.box.addPt(b.sub)
		b.cur = b.sub
	}
}

// Err returns the first error recorded while accumulating, if any.
func (b *BoundsAdder) Err() error { return b.err }

// Rect returns the accumulated bounds rounded outward onto the Int26_6 grid.
func (b *BoundsAdder) Rect() (fixed.Rectangle26_6, error) {
	if b.err != nil {
		return fixed.Rectangle26_6{}, b.err
	}
	if !b.started {
		return fixed.Rectangle26_6{}, ErrEmptyPath
	}
	return finalizeRect(b.box)
}

// UnionRect returns the smallest Int26_6 rectangle containing both a and b.
func UnionRect(a, b fixed.Rectangle26_6) fixed.Rectangle26_6 {
	return fixed.Rectangle26_6{
		Min: fixed.Point26_6{
			X: minInt(a.Min.X, b.Min.X),
			Y: minInt(a.Min.Y, b.Min.Y)},
		Max: fixed.Point26_6{
			X: maxInt(a.Max.X, b.Max.X),
			Y: maxInt(a.Max.Y, b.Max.Y)}}
}

func minInt(a, b fixed.Int26_6) fixed.Int26_6 {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b fixed.Int26_6) fixed.Int26_6 {
	if a > b {
		return a
	}
	return b
}

// floorDiv is Euclidean floor division, correct for negative numerators.
func floorDiv(x, d fixed.Int26_6) int {
	q := int(x) / int(d)
	if rem := int(x) % int(d); rem != 0 && rem < 0 {
		q--
	}
	return q
}

// ceilDiv is ceiling division, correct for negative numerators.
func ceilDiv(x, d fixed.Int26_6) int {
	q := int(x) / int(d)
	if rem := int(x) % int(d); rem != 0 && rem > 0 {
		q++
	}
	return q
}

// PixelCover returns the integer pixel rectangle touched when repainting b.
// Pixel column i covers the half-open span [i, i+1), so the box is floored on
// its minimum and ceiled on its maximum; this covers partially touched edge
// pixels instead of rounding them away. When clip is non-nil the result is
// intersected with it, yielding an empty rectangle for a fully clipped box.
func PixelCover(b fixed.Rectangle26_6, clip *image.Rectangle) image.Rectangle {
	r := image.Rect(
		floorDiv(b.Min.X, 64), floorDiv(b.Min.Y, 64),
		ceilDiv(b.Max.X, 64), ceilDiv(b.Max.Y, 64))
	if clip != nil {
		r = r.Intersect(*clip)
	}
	return r
}

// strokeJoin records an interior junction together with the tangent vectors
// of the two meeting segments, so the miter allowance can track the actual
// turn angle instead of assuming every join is a sharp spike.
type strokeJoin struct {
	v          fPt
	tanT, tanL fPt
}

// strokeWalk holds the analytic geometry of a path's stroked spine.
type strokeWalk struct {
	spine    fRect        // exact extrema box of the spine
	joins    []strokeJoin // interior segment junction vertices
	openEnds []fPt        // endpoints of open sub-paths (both ends each)
}

// seg describes one flattened spine segment with endpoint tangents in
// transformed device space.
type seg struct {
	start, end       fPt
	tanStart, tanEnd fPt
}

// walkStroke collects spine extrema, join vertices (with tangents) and open
// endpoints in transformed device space.
func walkStroke(p Path, m Matrix2D) (strokeWalk, error) {
	var w strokeWalk
	var (
		cur, sub       fPt
		hasSub, closed bool
		segs           []seg
	)
	addJoin := func(v fPt, tanT, tanL fPt) {
		w.joins = append(w.joins, strokeJoin{v: v, tanT: tanT, tanL: tanL})
	}
	flushOpen := func() {
		if hasSub && !closed {
			w.openEnds = append(w.openEnds, sub, cur)
		}
	}
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			flushOpen()
			cur = mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			if !allFinite(cur.x, cur.y) {
				return w, ErrNonFinite
			}
			sub, hasSub, closed = cur, true, false
			segs = segs[:0]
			w.spine.addPt(cur)
			i += 3
		case PathLineTo:
			if !hasSub {
				return w, ErrInvalidValue
			}
			end := mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			if !allFinite(end.x, end.y) {
				return w, ErrNonFinite
			}
			tan := end.sub(cur)
			if len(segs) > 0 {
				addJoin(cur, segs[len(segs)-1].tanEnd, tan)
			}
			segs = append(segs, seg{cur, end, tan, tan})
			cur = end
			w.spine.addPt(cur)
			i += 3
		case PathQuadTo:
			if !hasSub {
				return w, ErrInvalidValue
			}
			p1 := mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			p2 := mapPoint(m, fixedToFloat(fp(p[i+3], p[i+4])))
			if !allFinite(p1.x, p1.y, p2.x, p2.y) {
				return w, ErrNonFinite
			}
			tanS, tanE := p1.sub(cur), p2.sub(p1)
			if len(segs) > 0 {
				addJoin(cur, segs[len(segs)-1].tanEnd, tanS)
			}
			segs = append(segs, seg{cur, p2, tanS, tanE})
			w.spine.addQuadExtrema(cur, p1, p2)
			cur = p2
			i += 5
		case PathCubicTo:
			if !hasSub {
				return w, ErrInvalidValue
			}
			p1 := mapPoint(m, fixedToFloat(fp(p[i+1], p[i+2])))
			p2 := mapPoint(m, fixedToFloat(fp(p[i+3], p[i+4])))
			p3 := mapPoint(m, fixedToFloat(fp(p[i+5], p[i+6])))
			if !allFinite(p1.x, p1.y, p2.x, p2.y, p3.x, p3.y) {
				return w, ErrNonFinite
			}
			tanS, tanE := p1.sub(cur), p3.sub(p2)
			if len(segs) > 0 {
				addJoin(cur, segs[len(segs)-1].tanEnd, tanS)
			}
			segs = append(segs, seg{cur, p3, tanS, tanE})
			w.spine.addCubeExtrema(cur, p1, p2, p3)
			cur = p3
			i += 7
		case PathClose:
			if hasSub && len(segs) > 0 {
				tanClose := sub.sub(cur)
				if tanClose.len2() > 0 {
					// A non-degenerate closing line creates two joins: one
					// where it leaves the last explicit vertex and one where
					// it meets the first segment at the sub-path start.
					addJoin(cur, segs[len(segs)-1].tanEnd, tanClose)
					addJoin(sub, tanClose, segs[0].tanStart)
				} else {
					// The last vertex already equals the start: one join with
					// the two adjacent segment tangents.
					addJoin(sub, segs[len(segs)-1].tanEnd, segs[0].tanStart)
				}
			}
			cur, closed = sub, true
			i++
		default:
			return w, ErrInvalidValue
		}
	}
	flushOpen()
	return w, nil
}

func (p fPt) sub(q fPt) fPt { return fPt{p.x - q.x, p.y - q.y} }
func (p fPt) len2() float64 { return p.x*p.x + p.y*p.y }

// miterJoinAllowance returns the axis-aligned box allowance around a join
// vertex for miter-family joins. r is the device-space half stroke width and
// ratioLimit the effective SVG miter ratio (already clamped). Degenerate or
// near-collinear tangents return r, meaning the spine-tube growth alone
// covers the join.
func miterJoinAllowance(tanT, tanL fPt, r, ratioLimit float64) float64 {
	lt2, ll2 := tanT.len2(), tanL.len2()
	if lt2 == 0 || ll2 == 0 {
		return r
	}
	// cos of the turn angle between the *unit* tangents.
	cos := (tanT.x*tanL.x + tanT.y*tanL.y) / math.Sqrt(lt2*ll2)
	cos = clampF(cos, -1, 1)
	// csc(theta/2) = sqrt(2/(1+cos(theta))).
	den := 1 + cos
	if den <= 1e-12 {
		// Near reversal: bound by the explicit miter clamp.
		return r * ratioLimit
	}
	ratio := math.Sqrt(2 / den)
	if ratio > ratioLimit {
		ratio = ratioLimit
	}
	return r * ratio
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// boundsScanner implements Scanner purely to capture the outline geometry
// that the Stroker/Dasher emits. It never allocates an image or mask.
type boundsScanner struct {
	box      fRect
	overflow bool
}

func (s *boundsScanner) set(a fixed.Point26_6) {
	x, y := float64(a.X)/64.0, float64(a.Y)/64.0
	if !allFinite(x, y) || math.Abs(x) > fixedPixelMax || math.Abs(y) > fixedPixelMax {
		s.overflow = true
		return
	}
	s.box.addPoint(x, y)
}

func (s *boundsScanner) Start(a fixed.Point26_6) { s.set(a) }
func (s *boundsScanner) Line(a fixed.Point26_6)  { s.set(a) }
func (s *boundsScanner) Draw()                   {}
func (s *boundsScanner) GetPathExtent() fixed.Rectangle26_6 {
	r, _ := finalizeRect(s.box)
	return r
}
func (s *boundsScanner) SetBounds(int, int)   {}
func (s *boundsScanner) SetColor(interface{}) {}
func (s *boundsScanner) SetWinding(bool)      {}
func (s *boundsScanner) Clear()               {}

// transformPath returns an independent copy of p with every coordinate
// mapped through m. The caller's Path is never modified.
func transformPath(p Path, m Matrix2D) (Path, error) {
	out := make(Path, 0, len(p))
	addPt := func(x, y fixed.Int26_6) error {
		f := mapPoint(m, fixedToFloat(fp(x, y)))
		if !allFinite(f.x, f.y) {
			return ErrNonFinite
		}
		if math.Abs(f.x) > fixedPixelMax || math.Abs(f.y) > fixedPixelMax {
			return ErrValueOverflow
		}
		out = append(out, fixed.Int26_6(math.Round(f.x*64.0)), fixed.Int26_6(math.Round(f.y*64.0)))
		return nil
	}
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			out = append(out, fixed.Int26_6(PathMoveTo))
			if err := addPt(p[i+1], p[i+2]); err != nil {
				return nil, err
			}
			i += 3
		case PathLineTo:
			out = append(out, fixed.Int26_6(PathLineTo))
			if err := addPt(p[i+1], p[i+2]); err != nil {
				return nil, err
			}
			i += 3
		case PathQuadTo:
			out = append(out, fixed.Int26_6(PathQuadTo))
			if err := addPt(p[i+1], p[i+2]); err != nil {
				return nil, err
			}
			if err := addPt(p[i+3], p[i+4]); err != nil {
				return nil, err
			}
			i += 5
		case PathCubicTo:
			out = append(out, fixed.Int26_6(PathCubicTo))
			if err := addPt(p[i+1], p[i+2]); err != nil {
				return nil, err
			}
			if err := addPt(p[i+3], p[i+4]); err != nil {
				return nil, err
			}
			if err := addPt(p[i+5], p[i+6]); err != nil {
				return nil, err
			}
			i += 7
		case PathClose:
			out = append(out, fixed.Int26_6(PathClose))
			i++
		default:
			return nil, ErrInvalidValue
		}
	}
	return out, nil
}

func capFuncs(c CapStyle) (capL, capT CapFunc) {
	switch c {
	case CapSquare:
		return SquareCap, SquareCap
	case CapRound:
		return RoundCap, RoundCap
	case CapCubic:
		return CubicCap, CubicCap
	case CapQuadratic:
		return QuadraticCap, QuadraticCap
	default:
		return ButtCap, ButtCap
	}
}

func gapFuncFor(jm JoinMode) GapFunc {
	if jm == Round {
		return RoundGap
	}
	return FlatGap
}

// validateStyle checks the stroke parameters against the same constraints the
// stroker and dasher rely on. Negative dash phases are explicitly allowed.
func validateStyle(s StrokeStyle) error {
	if !allFinite(s.Width, s.MiterLimit, s.DashOffset) {
		return ErrNonFinite
	}
	if s.Width < 0 {
		return ErrInvalidValue
	}
	if s.Width > fixedPixelMax {
		return ErrValueOverflow
	}
	for _, d := range s.Dashes {
		if !allFinite(d) {
			return ErrNonFinite
		}
		if d <= 0 {
			return ErrInvalidValue
		}
		if math.Abs(d) > fixedPixelMax {
			return ErrValueOverflow
		}
	}
	if math.Abs(s.DashOffset) > fixedPixelMax {
		return ErrValueOverflow
	}
	return nil
}

// StrokeBounds returns the tight axis-aligned coverage rectangle of stroking
// the path under matrix m with style s. The style width, miter limit and dash
// lengths are interpreted in *transformed device space*, matching the
// package's standard MatrixAdder + SetStroke usage: the path is mapped by m
// first, then the stroker applies the given width. The returned box always
// contains the full rendered stroke:
//
//   - the spine box uses analytic curve extrema on the transformed points;
//   - the half-width allowance r = Width/2 covers every round/bevel join and
//     round/butt cap, since transformed tangents already carry the scale;
//   - miter-family joins add r times the angle-derived miter ratio, bounded
//     by the miter limit and the miterRatioClamp upper bound;
//   - square-family caps on open ends add the sqrt(2)*r allowance;
//   - the outline actually emitted by a Dasher configured with the same
//     parameters is unioned in, so dash phase, cap, join and miter clip
//     behavior matches the rasterizer exactly.
//
// Dashing can only remove coverage (dash boundaries lie on the spine), so the
// dashed box is always contained in the solid-stroke allowance.
func StrokeBounds(p Path, m Matrix2D, s StrokeStyle) (fixed.Rectangle26_6, error) {
	if err := validateMatrix(m); err != nil {
		return fixed.Rectangle26_6{}, err
	}
	if len(p) == 0 {
		return fixed.Rectangle26_6{}, ErrEmptyPath
	}
	if err := validateStyle(s); err != nil {
		return fixed.Rectangle26_6{}, err
	}

	// Analytic conservative box, computed first so it is available even if
	// the fixed point stroker overflows.
	walk, err := walkStroke(p, m)
	if err != nil {
		return fixed.Rectangle26_6{}, err
	}
	if !walk.spine.ok {
		return fixed.Rectangle26_6{}, ErrEmptyPath
	}

	// Width is applied in device space after the transform, so the
	// half-width allowance is simply Width/2; the transformed tangents used
	// for the miter ratio already incorporate the matrix scaling.
	r := s.Width / 2.0

	analytic := walk.spine
	pad := r
	squareFamily := s.Cap == CapSquare || s.Cap == CapCubic || s.Cap == CapQuadratic
	if len(s.Dashes) > 0 && squareFamily {
		// Interior square dash caps can project half a width tangentially
		// off any spine point, so take the isotropic worst case.
		pad = r * capSqrt2
	}
	analytic.grow(pad)

	if s.JoinMode == Miter || s.JoinMode == MiterClip || s.JoinMode == Arc || s.JoinMode == ArcClip {
		ratio := s.MiterLimit
		switch {
		case ratio <= 0:
			ratio = 4
		case ratio < 1:
			ratio = 1
		}
		if ratio > miterRatioClamp {
			ratio = miterRatioClamp
		}
		// Only genuinely sharp turns can project beyond the half-width
		// tube. The miter ratio is csc(theta/2) where theta is the turn
		// angle between the unit tangents; near-collinear joins are already
		// covered by the spine box grown by r, so skip them for tightness.
		for _, j := range walk.joins {
			allow := miterJoinAllowance(j.tanT, j.tanL, r, ratio)
			if allow <= r {
				continue
			}
			analytic.addPoint(j.v.x-allow, j.v.y-allow)
			analytic.addPoint(j.v.x+allow, j.v.y+allow)
		}
	}
	if squareFamily {
		capAllow := r * capSqrt2
		for _, e := range walk.openEnds {
			analytic.addPoint(e.x-capAllow, e.y-capAllow)
			analytic.addPoint(e.x+capAllow, e.y+capAllow)
		}
	}

	// Exact-outline layer: drive the real Dasher through a measuring
	// scanner. The transformed path is a private copy; nothing the caller
	// holds is touched.
	devPath, terr := transformPath(p, m)
	outRect, _ := finalizeRect(analytic)
	if terr != nil {
		// Keep the analytic clamp as the conservative answer.
		return outRect, terr
	}

	scanner := new(boundsScanner)
	d := NewDasher(0, 0, scanner)
	capL, capT := capFuncs(s.Cap)
	miterFixed := fixed.Int26_6(math.Round(effectiveMiter(s.MiterLimit) * 64.0))
	widthFixed := fixed.Int26_6(math.Round(s.Width * 64.0))
	d.SetStroke(widthFixed, miterFixed, capL, capT, gapFuncFor(s.JoinMode), s.JoinMode, s.Dashes, s.DashOffset)
	devPath.AddTo(d)

	if scanner.overflow {
		// The stroker emitted coordinates outside Int26_6 range; the
		// analytic box remains a valid clamped conservative result.
		rect, _ := finalizeRect(analytic)
		return rect, ErrValueOverflow
	}
	if scanner.box.ok {
		var exact fRect
		exact.union(scanner.box)
		exact.grow(strokeFlatEps) // fixed point normal and arc flatten slack
		analytic.union(exact)
	}

	return finalizeRect(analytic)
}

// effectiveMiter normalizes the style miter ratio to the value handed to
// SetStroke, mirroring the clamps documented on StrokeStyle.
func effectiveMiter(ratio float64) float64 {
	switch {
	case ratio <= 0:
		return 4
	case ratio < 1:
		return 1
	case ratio > miterRatioClamp:
		return miterRatioClamp
	default:
		return ratio
	}
}
