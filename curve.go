// Copyright 2026 by the rasterx Authors. All rights reserved.
//_
// curve.go contains the single Bézier curve subdivision core shared by the
// filling, stroking and dashed-stroking rasterizers. Keeping the flatness
// tolerance and the segment count heuristic in one place guarantees that the
// filled and stroked edges of the same curve are always subdivided identically.

package rasterx

import (
	"math"

	"golang.org/x/image/math/fixed"
)

const (
	// flattenTolerance is the single flatness tolerance used by every curve
	// flattening path in the package (fill, stroke and dash). It is expressed
	// in squared fixed-point units relative to the deviation of the control
	// points; changing it here changes every caller consistently, so the fill
	// and stroke edges can never drift apart.
	flattenTolerance = float32(3)
	// flattenMinDeviation is the deviation below which a curve is approximated
	// by a single line segment. It matches the threshold historically used by
	// golang.org/x/image/vector.
	flattenMinDeviation = float32(0.333)
)

// devSquaredF32 returns a measure of how curvy the sequence (ax, ay) to
// (bx, by) to (cx, cy) is. It determines how many line segments will
// approximate a Bézier curve. The comments and rationale are copied from
// golang.org/x/image/vector:
//
// http://lists.nongnu.org/archive/html/freetype-devel/2016-08/msg00080.html
// gives the rationale for this evenly spaced heuristic instead of a recursive
// de Casteljau approach:
//
// The reason for the subdivision by n is that the "flatness" computation is
// semi-expensive (it's done once rather than on each potential subdivision)
// and also because it often yields fewer subdivisions. Taking a circular arc
// as a simplifying assumption (ie a spherical cow), where this heuristic gets
// n, a recursive approach would get 2^⌈lg n⌉, which is expected to be 33% more
// in the limit.
func devSquaredF32(ax, ay, bx, by, cx, cy float32) float32 {
	devx := ax - 2*bx + cx
	devy := ay - 2*by + cy
	return devx*devx + devy*devy
}

// curveSegmentCount maps a curve deviation measure to the number of straight
// line segments used to approximate the curve. The subdivision is iterative,
// so a large deviation yields a large but bounded loop count rather than
// recursive subdivision: with float32 control points the result is always
// finite and small enough to terminate.
func curveSegmentCount(devsq float32) int {
	if devsq < flattenMinDeviation {
		return 1
	}
	return 1 + int(math.Sqrt(math.Sqrt(float64(flattenTolerance*devsq))))
}

// flattenQuadF32 is the float32 quadratic subdivision core. It invokes emit
// once for every generated line end point (including the curve end point) and
// returns the number of segments.
func flattenQuadF32(ax, ay, bx, by, cx, cy float32, emit func(x, y float32)) int {
	n := curveSegmentCount(devSquaredF32(ax, ay, bx, by, cx, cy))
	t, nInv := float32(0), 1/float32(n)
	for i := 0; i < n-1; i++ {
		t += nInv

		mt := 1 - t
		t1 := mt * mt
		t2 := mt * t * 2
		t3 := t * t
		emit(
			ax*t1+bx*t2+cx*t3,
			ay*t1+by*t2+cy*t3)
	}
	emit(cx, cy)
	return n
}

// flattenCubeF32 is the float32 cubic subdivision core. The larger of the two
// endpoint control polygon deviations governs the segment count, exactly as in
// golang.org/x/image/vector.
func flattenCubeF32(ax, ay, bx, by, cx, cy, dx, dy float32, emit func(x, y float32)) int {
	devsq := devSquaredF32(ax, ay, bx, by, dx, dy)
	if devsqAlt := devSquaredF32(ax, ay, cx, cy, dx, dy); devsq < devsqAlt {
		devsq = devsqAlt
	}
	n := curveSegmentCount(devsq)
	t, nInv := float32(0), 1/float32(n)
	for i := 0; i < n-1; i++ {
		t += nInv

		tsq := t * t
		mt := 1 - t
		mtsq := mt * mt
		t1 := mtsq * mt
		t2 := mtsq * t * 3
		t3 := mt * tsq * 3
		t4 := tsq * t
		emit(
			ax*t1+bx*t2+cx*t3+dx*t4,
			ay*t1+by*t2+cy*t3+dy*t4)
	}
	emit(dx, dy)
	return n
}

// IsDegenerateQuad reports whether the quadratic Bézier a-b-c collapses to a
// straight line, in which case flattening reduces to a single Line call.
func IsDegenerateQuad(a, b, c fixed.Point26_6) bool {
	return a == b || b == c
}

// IsDegenerateCube reports whether the cubic Bézier a-b-c-d collapses to a
// straight line, in which case flattening reduces to a single Line call.
func IsDegenerateCube(a, b, c, d fixed.Point26_6) bool {
	return (a == b && c == d) || (a == b && b == c) || (c == b && d == c)
}

// FlattenQuad subdivides the quadratic Bézier running from a to c with control
// point b into straight line segments. It is the reusable subdivision core for
// the filling, stroking and dashing rasterizers.
//
// emit is invoked once per generated segment end point, in curve order,
// including the end point c; the start point a is not emitted because it is
// already the rasterizer's current pen position. The returned values are:
//   - n: the number of generated line segments (len of the emit sequence);
//   - startTan: the unscaled start tangent b-a, i.e. the control polygon edge
//     leaving a;
//   - endTan: the unscaled end tangent c-b, i.e. the control polygon edge
//     entering c.
//
// The tangent vectors are the same edge vectors the stroker feeds into its
// normal and radius-of-curvature calculations, so callers do not have to
// re-derive them.
func FlattenQuad(a, b, c fixed.Point26_6, emit func(p fixed.Point26_6)) (n int, startTan, endTan fixed.Point26_6) {
	startTan = b.Sub(a)
	endTan = c.Sub(b)
	n = flattenQuadF32(float32(a.X), float32(a.Y), // Pts are x64, but does not matter.
		float32(b.X), float32(b.Y),
		float32(c.X), float32(c.Y),
		func(x, y float32) {
			emit(fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)})
		})
	return
}

// FlattenCubic subdivides the cubic Bézier running from a to d with control
// points b and c into straight line segments. emit receives the segment end
// points in curve order, including d, and n is the number of segments.
//
// The returned startTan and endTan are the unscaled control polygon edge
// vectors b-a and d-c; the mathematical derivatives are 3*startTan and
// 3*endTan respectively. The unscaled edges are returned because they match
// the tangent convention used by the stroker's join and curvature
// calculations, for which only the direction (and the ratio between edges)
// matters.
func FlattenCubic(a, b, c, d fixed.Point26_6, emit func(p fixed.Point26_6)) (n int, startTan, endTan fixed.Point26_6) {
	startTan = b.Sub(a)
	endTan = d.Sub(c)
	n = flattenCubeF32(float32(a.X), float32(a.Y),
		float32(b.X), float32(b.Y),
		float32(c.X), float32(c.Y),
		float32(d.X), float32(d.Y),
		func(x, y float32) {
			emit(fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)})
		})
	return
}

// QuadTo flattens the quadratic Bezier curve into lines through the LineTo
// func. It is a float32 convenience wrapper around the shared FlattenQuad
// core and is kept for backward compatibility and for callers that already
// work in float32 coordinates.
// This function is adapted from the version found in golang.org/x/image/vector.
func QuadTo(ax, ay, bx, by, cx, cy float32, LineTo func(dx, dy float32)) {
	flattenQuadF32(ax, ay, bx, by, cx, cy, LineTo)
}

// CubeTo flattens the cubic Bezier curve into lines through the LineTo func.
// It is a float32 convenience wrapper around the shared FlattenCubic core and
// is kept for backward compatibility.
// This function is adapted from the version found in golang.org/x/image/vector.
func CubeTo(ax, ay, bx, by, cx, cy, dx, dy float32, LineTo func(ex, ey float32)) {
	flattenCubeF32(ax, ay, bx, by, cx, cy, dx, dy, LineTo)
}
