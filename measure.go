// Copyright 2018 by the rasterx Authors. All rights reserved.
//_
// Path metric (arc-length) queries for rasterx paths.
//
// A PathMeasure breaks a Path into independently measured segments (lines,
// quadratic and cubic Beziers, and explicit close segments), integrates the
// arc length of every segment, and answers point/tangent queries parameterized
// by distance along the path rather than by the curve's t parameter. This is
// what an application needs to move an icon at constant speed along a
// spline. The segmentation rules live in this file and do not depend on the
// Filler, Stroker, Dasher or any Scanner.
//
// All measurements are taken in the same fixed.Int26_6 coordinate space as
// the Path (one unit is 1/64 of a user unit). The heavy numerics are carried
// out in float64 and the fixed point wrappers round back to fixed.Int26_6.
// Queries are read-only: neither the source Path nor any rasterizer is
// modified.

package rasterx

import (
	"errors"
	"math"

	"golang.org/x/image/math/fixed"
)

// Errors returned by the checked PathMeasure constructors and queries.
var (
	// ErrMalformedPath is returned when a Path is truncated or contains a
	// command token that is not one of PathMoveTo, PathLineTo, PathQuadTo,
	// PathCubicTo or PathClose.
	ErrMalformedPath = errors.New("rasterx: malformed path")

	// ErrNonFiniteCoordinate is returned when a path coordinate or a control
	// point evaluates to a NaN or infinite value. Such values cannot be
	// measured and are rejected instead of propagating into the integrator.
	ErrNonFiniteCoordinate = errors.New("rasterx: non-finite path coordinate")

	// ErrInvalidDistance is returned by checked distance queries when the
	// distance argument is NaN or infinite. Finite out-of-range distances are
	// not an error: they are clamped to the path ends.
	ErrInvalidDistance = errors.New("rasterx: invalid distance")

	// ErrZeroLengthPath is returned by checked point queries on a measure
	// whose total length is zero, i.e. an empty path or one containing only
	// zero-length segments. The unchecked queries return the first pen
	// position and a zero tangent in that case.
	ErrZeroLengthPath = errors.New("rasterx: path has zero length")
)

// segmentKind identifies how a measured segment is parameterized.
type segmentKind uint8

const (
	segLine segmentKind = iota
	segQuad
	segCubic
	segClose
)

// measureTableN is the number of equal-parameter panels whose arc lengths are
// precomputed for every curved segment. The table bounds the work done by any
// later point query and, together with the 16-point Gauss-Legendre rule used
// on each panel, keeps the integrated length accurate well beyond fixed point
// precision even for tightly curved splines.
const measureTableN = 48

// fPoint is a float64 coordinate pair used internally by the measure.
type fPoint struct{ x, y float64 }

func (p fPoint) finite() bool {
	return math.IsNaN(p.x) == false && math.IsInf(p.x, 0) == false &&
		math.IsNaN(p.y) == false && math.IsInf(p.y, 0) == false
}

// pathSegment holds one geometric segment of a measured path together with
// its cumulative arc-length lookup table. table[k] is the arc length from the
// segment start to parameter t = k/measureTableN; table[measureTableN]
// therefore equals length. Zero-length segments keep a zeroed table.
type pathSegment struct {
	kind              segmentKind
	p0, p1, p2, p3    fPoint
	startDist, length float64
	table             [measureTableN + 1]float64
}

// PathMeasure stores the arc-length metrics of a Path. It is safe for
// concurrent point queries once constructed; the constructor copies every
// coordinate it needs, so later mutation of the source Path cannot affect the
// measure and the measure never writes back to the Path.
type PathMeasure struct {
	segments []pathSegment
	// total is the sum of every segment length, including the closing
	// segment of every closed sub-path.
	total float64
	// firstPoint is the first pen position and is only used when total is
	// zero, where there is no defined point on the curve. It is the zero
	// point when drawing commands appear before the first move-to, matching
	// the implicit pen-at-origin convention.
	firstPoint fPoint
}

// Gauss-Legendre 16 point abscissae and weights on [-1, 1].
var (
	gl16X = [8]float64{
		0.09501250983763744, 0.28160355077925891, 0.45801677765722739,
		0.61787624440264375, 0.75540440835500303, 0.86563120238783175,
		0.94457502307323258, 0.98940093499164993,
	}
	gl16W = [8]float64{
		0.18945061045506846, 0.18260341504492359, 0.16915651939500254,
		0.14959598881657673, 0.12462897125553387, 0.09515851168249278,
		0.06225352393864789, 0.02715245941175409,
	}
)

// gl16Integrate integrates f on [a, b] with the 16-point Gauss-Legendre rule.
// A fixed 16-point rule is used rather than recursive subdivision so that
// construction time and floating point behavior are bounded for every input,
// including near-cusped curves.
func gl16Integrate(a, b float64, f func(float64) float64) float64 {
	half, mid := (b-a)*0.5, (a+b)*0.5
	sum := 0.0
	for k := 0; k < 8; k++ {
		dx := half * gl16X[k]
		sum += gl16W[k] * (f(mid+dx) + f(mid-dx))
	}
	return half * sum
}

// speed returns the magnitude of the curve derivative at parameter t.
func (s *pathSegment) speed(t float64) float64 {
	dx, dy := s.deriv(t)
	return math.Hypot(dx, dy)
}

// deriv returns the (unnormalized) derivative vector at parameter t.
func (s *pathSegment) deriv(t float64) (dx, dy float64) {
	switch s.kind {
	case segLine, segClose:
		return s.p1.x - s.p0.x, s.p1.y - s.p0.y
	case segQuad:
		mt := 1 - t
		// B'(t) = 2(1-t)(p1-p0) + 2t(p2-p1)
		a0x, a0y := s.p1.x-s.p0.x, s.p1.y-s.p0.y
		a1x, a1y := s.p2.x-s.p1.x, s.p2.y-s.p1.y
		return 2 * (mt*a0x + t*a1x), 2 * (mt*a0y + t*a1y)
	default: // segCubic
		mt := 1 - t
		// B'(t) = 3(1-t)^2(p1-p0) + 6(1-t)t(p2-p1) + 3t^2(p3-p2)
		a0x, a0y := s.p1.x-s.p0.x, s.p1.y-s.p0.y
		a1x, a1y := s.p2.x-s.p1.x, s.p2.y-s.p1.y
		a2x, a2y := s.p3.x-s.p2.x, s.p3.y-s.p2.y
		return 3 * (mt*mt*a0x + 2*mt*t*a1x + t*t*a2x),
			3 * (mt*mt*a0y + 2*mt*t*a1y + t*t*a2y)
	}
}

// pointAt returns the curve position at parameter t.
func (s *pathSegment) pointAt(t float64) fPoint {
	switch s.kind {
	case segLine, segClose:
		return fPoint{x: s.p0.x + t*(s.p1.x-s.p0.x),
			y: s.p0.y + t*(s.p1.y-s.p0.y)}
	case segQuad:
		mt := 1 - t
		a, b, c := mt*mt, 2*mt*t, t*t
		return fPoint{x: a*s.p0.x + b*s.p1.x + c*s.p2.x,
			y: a*s.p0.y + b*s.p1.y + c*s.p2.y}
	default: // segCubic
		mt := 1 - t
		a, b, c, d := mt*mt*mt, 3*mt*mt*t, 3*mt*t*t, t*t*t
		return fPoint{x: a*s.p0.x + b*s.p1.x + c*s.p2.x + d*s.p3.x,
			y: a*s.p0.y + b*s.p1.y + c*s.p2.y + d*s.p3.y}
	}
}

// buildTable fills the segment arc-length table and sets its length.
func (s *pathSegment) buildTable() {
	if s.kind == segLine || s.kind == segClose {
		// The length of a line is exact; no quadrature is needed.
		s.length = math.Hypot(s.p1.x-s.p0.x, s.p1.y-s.p0.y)
		for k := 0; k <= measureTableN; k++ {
			s.table[k] = s.length * float64(k) / measureTableN
		}
		return
	}
	// Curved segments are integrated panel by panel. Each panel is short
	// enough for the fixed-order Gauss rule to be effectively exact, and the
	// loop has a fixed iteration count, so even a wildly curved segment
	// cannot trigger unbounded or recursive subdivision.
	prev := 0.0
	for k := 1; k <= measureTableN; k++ {
		t0 := float64(k-1) / measureTableN
		t1 := float64(k) / measureTableN
		prev += gl16Integrate(t0, t1, s.speed)
		s.table[k] = prev
	}
	s.length = s.table[measureTableN]
}

// commandStride maps a path command to the number of fixed.Int26_6 values the
// command occupies in the Path slice, including the command token itself.
func commandStride(cmd PathCommand) (int, bool) {
	switch cmd {
	case PathMoveTo, PathLineTo:
		return 3, true
	case PathQuadTo:
		return 5, true
	case PathCubicTo:
		return 7, true
	case PathClose:
		return 1, true
	default:
		return 0, false
	}
}

// NewPathMeasure measures the given path. The returned error is non-nil if
// the path is malformed or carries non-finite coordinates; an empty (but
// otherwise valid) path returns a measure whose Length is zero and whose
// checked point queries return ErrZeroLengthPath. The source path is never
// modified.
func NewPathMeasure(p Path) (*PathMeasure, error) {
	m := &PathMeasure{}
	var pen, subStart fPoint
	hasPen, firstSet := false, false
	for i := 0; i < len(p); {
		cmd := PathCommand(p[i])
		stride, ok := commandStride(cmd)
		if ok == false || i+stride > len(p) {
			return nil, ErrMalformedPath
		}
		read := func(offset int) (fPoint, bool) {
			pt := fPoint{x: float64(p[i+offset]) / 64.0,
				y: float64(p[i+offset+1]) / 64.0}
			return pt, pt.finite()
		}
		switch cmd {
		case PathMoveTo:
			pt, ok := read(1)
			if ok == false {
				return nil, ErrNonFiniteCoordinate
			}
			pen, subStart, hasPen = pt, pt, true
			if firstSet == false {
				m.firstPoint, firstSet = pt, true
			}
		case PathLineTo:
			pt, ok := read(1)
			if ok == false {
				return nil, ErrNonFiniteCoordinate
			}
			if hasPen == false { // Match the implicit pen-at-origin convention.
				hasPen = true
			}
			m.appendSegment(pathSegment{kind: segLine, p0: pen, p1: pt})
			pen = pt
		case PathQuadTo:
			b, ok1 := read(1)
			c, ok2 := read(3)
			if ok1 == false || ok2 == false {
				return nil, ErrNonFiniteCoordinate
			}
			if hasPen == false {
				hasPen = true
			}
			m.appendSegment(pathSegment{kind: segQuad, p0: pen, p1: b, p2: c})
			pen = c
		case PathCubicTo:
			b, ok1 := read(1)
			c, ok2 := read(3)
			d, ok3 := read(5)
			if ok1 == false || ok2 == false || ok3 == false {
				return nil, ErrNonFiniteCoordinate
			}
			if hasPen == false {
				hasPen = true
			}
			m.appendSegment(pathSegment{kind: segCubic, p0: pen, p1: b, p2: c, p3: d})
			pen = d
		case PathClose:
			// The close adds an explicit straight segment back to the
			// sub-path start, even when its length is zero, so that its
			// presence is visible in the cumulative lengths.
			m.appendSegment(pathSegment{kind: segClose, p0: pen, p1: subStart})
			pen = subStart
		}
		i += stride
	}
	return m, nil
}

// appendSegment measures and registers a segment, chaining its cumulative
// start distance to the preceding segment.
func (m *PathMeasure) appendSegment(s pathSegment) {
	s.buildTable()
	s.startDist = m.total
	m.total += s.length
	m.segments = append(m.segments, s)
}

// Measure is a convenience wrapper around NewPathMeasure.
func (p Path) Measure() (*PathMeasure, error) {
	return NewPathMeasure(p)
}

// LengthF returns the total arc length of the path in user units as a
// float64. Closed sub-paths include the length of their closing segment and
// every sub-path contributes independently.
func (m *PathMeasure) LengthF() float64 { return m.total }

// Length returns the total arc length rounded to fixed.Int26_6 units.
func (m *PathMeasure) Length() fixed.Int26_6 {
	return fixed.Int26_6(math.Round(m.total * 64.0))
}

// SegmentCount returns the number of measured segments, including
// zero-length and close segments.
func (m *PathMeasure) SegmentCount() int { return len(m.segments) }

// CumulativeLengthsF returns the cumulative arc length at every segment
// boundary, starting with zero. The returned slice has SegmentCount()+1
// entries and its last element equals LengthF(); it is a copy and may be
// retained by the caller.
func (m *PathMeasure) CumulativeLengthsF() []float64 {
	out := make([]float64, len(m.segments)+1)
	for i, s := range m.segments {
		out[i] = s.startDist
	}
	out[len(m.segments)] = m.total
	return out
}

// CumulativeLengths returns CumulativeLengthsF rounded to fixed.Int26_6
// units.
func (m *PathMeasure) CumulativeLengths() []fixed.Int26_6 {
	f := m.CumulativeLengthsF()
	out := make([]fixed.Int26_6, len(f))
	for i, v := range f {
		out[i] = fixed.Int26_6(math.Round(v * 64.0))
	}
	return out
}

// clampDistance maps any finite distance onto [0, total]. Negative distances
// map to the path start and distances past the end map to its end.
func (m *PathMeasure) clampDistance(d float64) float64 {
	if d < 0 {
		return 0
	}
	if d > m.total {
		return m.total
	}
	return d
}

// locate maps a clamped distance to a segment index and the remaining
// distance inside that segment. A distance landing exactly on a segment
// boundary is resolved onto the following (outgoing) segment at its start
// parameter, so crossing a join never makes the measured point jump back
// along the segment just finished. Zero-length segments are skipped: they
// occupy no arc length, so a positive distance always lands on a segment
// that actually carries it and the tangent is well defined. The final end
// point is produced by the last non-degenerate segment at its end
// parameter.
func (m *PathMeasure) locate(d float64) (int, float64) {
	if d <= 0 || len(m.segments) == 0 {
		// At the very start, advance to the first segment that has length
		// unless the whole path is degenerate.
		for i := range m.segments {
			if m.segments[i].length > 0 {
				return i, 0
			}
		}
		return 0, 0
	}
	if d >= m.total {
		for i := len(m.segments) - 1; i >= 0; i-- {
			if m.segments[i].length > 0 {
				return i, m.segments[i].length
			}
		}
		return len(m.segments) - 1, 0
	}
	// Last segment whose start distance is at most d, then walk forward
	// past any zero-length segments.
	lo, hi := 0, len(m.segments)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if m.segments[mid].startDist <= d {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	i := lo - 1
	if i < 0 {
		i = 0
	}
	for i < len(m.segments) && m.segments[i].length == 0 {
		i++
	}
	if i >= len(m.segments) {
		i = len(m.segments) - 1
	}
	return i, d - m.segments[i].startDist
}

// solveParameter inverts the segment arc-length function: given an arc length
// u measured from the segment start (0 <= u <= segment length), it returns
// the corresponding curve parameter. The search is confined to the table
// panel containing u and mixes Newton iterations with bounded bisection
// fallback, so a vanishing derivative (a cusp or degenerate control cage)
// cannot stall or diverge the query.
func (m *PathMeasure) solveParameter(s *pathSegment, u float64) float64 {
	if s.length == 0 {
		return 0
	}
	if u <= 0 {
		return 0
	}
	if u >= s.length {
		return 1
	}
	// Find the panel: last k with table[k] <= u (ties take the lower panel).
	k0, k1 := 0, measureTableN
	for k0 < k1 {
		mid := k0 + (k1-k0+1)/2
		if s.table[mid] <= u {
			k0 = mid
		} else {
			k1 = mid - 1
		}
	}
	tk := float64(k0) / measureTableN
	t1 := float64(k0+1) / measureTableN
	base := s.table[k0]
	target := u - base // arc length to cover inside the panel
	g := func(t float64) float64 {
		return gl16Integrate(tk, t, s.speed) - target
	}
	lo, hi := tk, t1
	t := (lo + hi) * 0.5
	const maxSteps = 64
	for step := 0; step < maxSteps; step++ {
		f := g(t)
		speed := s.speed(t)
		if speed > 1e-12*s.length+1e-30 {
			next := t - f/speed
			// Accept the Newton step only while it stays inside the
			// bracket; clamping an overshoot to the boundary would repeat
			// the same point and stall. Any out-of-range step falls through
			// to a bisection that halves the bracket instead.
			if next > lo && next < hi && math.Abs(next-t) > 1e-15 {
				if f < 0 {
					lo = t
				} else {
					hi = t
				}
				t = next
				if hi-lo <= 1e-14 {
					break
				}
				continue
			}
		}
		// Bisection fallback: g is monotone because speed is non-negative.
		if f < 0 {
			lo = t
		} else {
			hi = t
		}
		t = (lo + hi) * 0.5
		if hi-lo <= 1e-14 {
			break
		}
	}
	if t < tk {
		t = tk
	} else if t > t1 {
		t = t1
	}
	return t
}

// tangentAt returns the normalized tangent unit vector at parameter t on
// segment i, resolving zero derivatives (cusps, constant segments) by
// sampling nearby parameters first and neighboring segments last.
func (m *PathMeasure) tangentAt(i int, s *pathSegment, t float64) fPoint {
	dx, dy := s.deriv(t)
	if norm := math.Hypot(dx, dy); norm > 0 {
		return fPoint{x: dx / norm, y: dy / norm}
	}
	const eps = 1e-6
	for _, cand := range []float64{math.Min(t+eps, 1), math.Max(t-eps, 0)} {
		dx, dy = s.deriv(cand)
		if norm := math.Hypot(dx, dy); norm > 0 {
			return fPoint{x: dx / norm, y: dy / norm}
		}
	}
	// The whole segment is degenerate; borrow a direction from a neighbor.
	if i+1 < len(m.segments) {
		n := &m.segments[i+1]
		if dx, dy = n.deriv(0); math.Hypot(dx, dy) > 0 {
			norm := math.Hypot(dx, dy)
			return fPoint{x: dx / norm, y: dy / norm}
		}
	}
	if i > 0 {
		n := &m.segments[i-1]
		if dx, dy = n.deriv(1); math.Hypot(dx, dy) > 0 {
			norm := math.Hypot(dx, dy)
			return fPoint{x: dx / norm, y: dy / norm}
		}
	}
	return fPoint{}
}

// pointAtF is the shared float64 query implementation assuming d is already
// clamped.
func (m *PathMeasure) pointAtF(d float64) (pt, tan fPoint) {
	if m.total == 0 {
		return m.firstPoint, fPoint{}
	}
	i, u := m.locate(d)
	s := &m.segments[i]
	t := m.solveParameter(s, u)
	pt = s.pointAt(t)
	tan = m.tangentAt(i, s, t)
	return pt, tan
}

// PointAtF returns the point and normalized tangent vector at the given
// distance along the path. Negative distances clamp to the path start and
// distances larger than LengthF clamp to its end, so the call never fails for
// a finite argument. On a zero-length path it returns the first pen position
// and a zero tangent.
func (m *PathMeasure) PointAtF(distance float64) (x, y, tx, ty float64) {
	if math.IsNaN(distance) || math.IsInf(distance, 0) {
		distance = 0
	}
	pt, tan := m.pointAtF(m.clampDistance(distance))
	return pt.x, pt.y, tan.x, tan.y
}

// PointAtFChecked behaves like PointAtF but returns ErrInvalidDistance for a
// NaN or infinite distance and ErrZeroLengthPath on a zero-length path.
// Finite out-of-range distances are still clamped.
func (m *PathMeasure) PointAtFChecked(distance float64) (x, y, tx, ty float64, err error) {
	if math.IsNaN(distance) || math.IsInf(distance, 0) {
		return 0, 0, 0, 0, ErrInvalidDistance
	}
	if m.total == 0 {
		return m.firstPoint.x, m.firstPoint.y, 0, 0, ErrZeroLengthPath
	}
	pt, tan := m.pointAtF(m.clampDistance(distance))
	return pt.x, pt.y, tan.x, tan.y, nil
}

// roundFixed converts a float64 user-unit coordinate to fixed.Int26_6 using
// round-to-nearest and clamps it to the fixed.Int26_6 range so an extreme
// coordinate cannot wrap.
func roundFixed(v float64) fixed.Int26_6 {
	r := math.Round(v * 64.0)
	const minFixed = -1 << 31
	const maxFixed = 1<<31 - 1
	if r >= maxFixed {
		return maxFixed
	}
	if r <= minFixed {
		return minFixed
	}
	return fixed.Int26_6(r)
}

// PointAt returns the point and unit tangent at the given fixed point
// distance, clamping the distance to the path ends. The tangent components
// are fixed.Int26_6 values whose combined length is one fixed unit (64).
// Endpoints are returned exactly: the point at distance zero is the first
// pen position and the point at the full length is the final pen position.
// On a zero-length path it returns the first pen position and a zero tangent.
func (m *PathMeasure) PointAt(distance fixed.Int26_6) (point, tangent fixed.Point26_6) {
	if m.total == 0 {
		return fixed.Point26_6{X: roundFixed(m.firstPoint.x), Y: roundFixed(m.firstPoint.y)},
			fixed.Point26_6{}
	}
	d := m.clampDistance(float64(distance) / 64.0)
	// Endpoints are exact because solveParameter returns t = 0 or t = 1 at
	// the path ends and pointAt then evaluates the control polygon directly.
	pt, tan := m.pointAtF(d)
	return fixed.Point26_6{X: roundFixed(pt.x), Y: roundFixed(pt.y)},
		fixed.Point26_6{X: roundFixed(tan.x), Y: roundFixed(tan.y)}
}

// PointAtChecked behaves like PointAt but returns ErrZeroLengthPath when the
// path has no measurable length. Fixed point distances cannot be non-finite,
// and finite out-of-range distances are clamped.
func (m *PathMeasure) PointAtChecked(distance fixed.Int26_6) (point, tangent fixed.Point26_6, err error) {
	if m.total == 0 {
		return fixed.Point26_6{X: roundFixed(m.firstPoint.x), Y: roundFixed(m.firstPoint.y)},
			fixed.Point26_6{}, ErrZeroLengthPath
	}
	point, tangent = m.PointAt(distance)
	return point, tangent, nil
}
