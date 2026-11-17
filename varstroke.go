// Copyright 2026 by the rasterx Authors. All rights reserved.
//
// varstroke.go implements a variable width stroker: the stroke width
// changes continuously along the path, interpolated by arc length
// between user supplied width control points.

package rasterx

import (
	"math"
	"sort"

	"golang.org/x/image/math/fixed"
)

// joinCrossEps is the cross product epsilon (for unit tangents scaled by
// 64) below which two consecutive segments are considered co-linear and
// no join geometry is needed. 6/4096 ~= 0.08 degrees.
const joinCrossEps = 6

type (
	// WidthPoint maps a position along a stroked path, expressed as a
	// fraction of the total path arc length, to the stroke width at that
	// position. The width between control points is linearly interpolated
	// in arc length, so the stroke boundary is continuous across line,
	// quadratic and cubic bezier segment boundaries.
	WidthPoint struct {
		Pos   float64 // Fraction of total arc length in [0,1]; clamped.
		Width float64 // Stroke width in path units; negatives become 0.
	}

	// VarStroker strokes paths with a width that varies along the path.
	// It flattens the path, parameterizes it by arc length, evaluates the
	// width profile set with SetWidths at each flattened vertex, and emits
	// the two offset outlines (with joins and end caps) to its Filler.
	//
	// If no width control points are set, it strokes with the constant
	// width given to SetStroke, like a plain Stroker.
	//
	// Edge case handling:
	//  - Negative widths are clamped to 0; a zero width region tapers to
	//    the center line.
	//  - Control points may be supplied in any order; they are sorted by
	//    Pos. Duplicate positions create a step in the width profile.
	//  - Zero length segments are removed before stroking; a path with no
	//    total length produces no output.
	//  - At sharp corners the miter limit (a multiple of the local half
	//    width) selects between miter, clipped miter and bevel joins;
	//    near-parallel segments always fall back to a bevel join.
	//  - Curves narrower than the local half width may produce
	//    self-intersecting inner edges; these are filled with the non-zero
	//    winding rule, as in the rest of the package.
	//  - Closed paths are joined all the way around with no end caps; the
	//    width profile spans the whole loop including the closing segment,
	//    so matching widths at Pos 0 and 1 give a seamless loop.
	VarStroker struct {
		Filler
		CapT, CapL CapFunc // Trailing and leading cap funcs may be set separately

		JoinMode JoinMode

		widths []WidthPoint  // sorted, clamped width control points
		u      fixed.Int26_6 // half width used when no width profile is set
		mLimit fixed.Int26_6 // miter limit as a multiple of the local half width

		pts    []fixed.Point26_6 // flattened points of the current subpath
		inPath bool
	}
)

// Compile time check that VarStroker can accumulate paths.
var _ Adder = (*VarStroker)(nil)

// NewVarStroker returns a ptr to a VarStroker with default values:
// a constant 2 unit wide stroke, butt caps and miter-clip joins with a
// miter limit of 4. Use SetStroke and SetWidths to configure it.
func NewVarStroker(width, height int, scanner Scanner) *VarStroker {
	r := new(VarStroker)
	r.Scanner = scanner
	r.SetBounds(width, height)
	r.SetWinding(true)
	r.SetStroke(2<<6, 4<<6, ButtCap, nil, MiterClip)
	return r
}

// SetStroke sets the constant stroke parameters. width is the stroke width
// used when no width profile is set with SetWidths. miterLimit is the miter
// cutoff as a multiple of the local (possibly varying) half width, used for
// the Miter and MiterClip join modes. capL and capT are the capping
// functions for the leading and trailing ends of open paths; if one is nil
// the other is used at both ends, if both are nil both ends are ButtCapped.
// jm is the JoinMode; Arc and ArcClip are treated as Round, since the
// variable width outline is built from flattened geometry.
func (r *VarStroker) SetStroke(width, miterLimit fixed.Int26_6, capL, capT CapFunc, jm JoinMode) {
	r.u = width / 2
	if r.u < 0 {
		r.u = 0
	}
	r.mLimit = miterLimit
	if r.mLimit < 1<<6 {
		r.mLimit = 1 << 6 // miter limit is at least 1, as in SVG
	}
	r.JoinMode = jm
	r.CapL, r.CapT = capL, capT
	if r.CapT == nil {
		if r.CapL == nil {
			r.CapT = ButtCap
		} else {
			r.CapT = r.CapL
		}
	}
	if r.CapL == nil {
		r.CapL = r.CapT
	}
}

// SetWidths sets the width control points for the variable width stroke.
// The points are copied, clamped to valid ranges (Pos to [0,1], Width to
// >= 0) and sorted by Pos, so they may be supplied in any order. Widths
// before the first and after the last control point are held constant.
// Calling SetWidths with no points restores the constant width mode.
func (r *VarStroker) SetWidths(wp ...WidthPoint) {
	r.widths = r.widths[:0]
	for _, w := range wp {
		if math.IsNaN(w.Pos) || math.IsNaN(w.Width) {
			continue
		}
		if w.Pos < 0 {
			w.Pos = 0
		} else if w.Pos > 1 {
			w.Pos = 1
		}
		if w.Width < 0 {
			w.Width = 0
		}
		r.widths = append(r.widths, w)
	}
	sort.SliceStable(r.widths, func(i, j int) bool { return r.widths[i].Pos < r.widths[j].Pos })
}

// halfWidthAt returns the half stroke width in fixed point at arc length s
// of a path of total arc length total.
func (r *VarStroker) halfWidthAt(s, total fixed.Int26_6) fixed.Int26_6 {
	ws := r.widths
	if len(ws) == 0 {
		return r.u
	}
	if total <= 0 {
		return 0
	}
	pos := float64(s) / float64(total)
	if pos <= ws[0].Pos {
		return widthToFixedHalf(ws[0].Width)
	}
	if last := ws[len(ws)-1]; pos >= last.Pos {
		return widthToFixedHalf(last.Width)
	}
	// First control point strictly past pos; ws[j-1].Pos <= pos < ws[j].Pos,
	// so the interval is non-empty even with duplicate positions.
	j := sort.Search(len(ws), func(i int) bool { return ws[i].Pos > pos })
	w0, w1 := ws[j-1], ws[j]
	t := (pos - w0.Pos) / (w1.Pos - w0.Pos)
	return widthToFixedHalf(w0.Width + t*(w1.Width-w0.Width))
}

// widthToFixedHalf converts a full width in path units to a fixed point
// half width.
func widthToFixedHalf(w float64) fixed.Int26_6 {
	if w <= 0 {
		return 0
	}
	return fixed.Int26_6(w * 32) // (w / 2) * 64
}

// Start begins a new subpath. If the previous subpath was not ended with
// Stop, it is stroked as an open path first, mirroring Path.AddTo.
func (r *VarStroker) Start(a fixed.Point26_6) {
	if r.inPath {
		r.render(false)
	}
	r.pts = append(r.pts[:0], a)
	r.inPath = true
}

// Line adds a line segment to the current subpath.
func (r *VarStroker) Line(b fixed.Point26_6) {
	if !r.inPath {
		r.Start(b)
		return
	}
	r.pts = append(r.pts, b)
}

// QuadBezier adds a quadratic bezier segment to the current subpath,
// flattened to lines.
func (r *VarStroker) QuadBezier(b, c fixed.Point26_6) {
	if !r.inPath {
		r.Start(c)
		return
	}
	a := r.pts[len(r.pts)-1]
	if a == b || b == c { // degenerate bezier
		r.Line(c)
		return
	}
	QuadTo(float32(a.X), float32(a.Y),
		float32(b.X), float32(b.Y),
		float32(c.X), float32(c.Y),
		func(dx, dy float32) {
			r.pts = append(r.pts, fixed.Point26_6{X: fixed.Int26_6(dx), Y: fixed.Int26_6(dy)})
		})
}

// CubeBezier adds a cubic bezier segment to the current subpath,
// flattened to lines.
func (r *VarStroker) CubeBezier(b, c, d fixed.Point26_6) {
	if !r.inPath {
		r.Start(d)
		return
	}
	a := r.pts[len(r.pts)-1]
	if (a == b && c == d) || (a == b && b == c) || (c == b && d == c) { // degenerate bezier
		r.Line(d)
		return
	}
	CubeTo(float32(a.X), float32(a.Y),
		float32(b.X), float32(b.Y),
		float32(c.X), float32(c.Y),
		float32(d.X), float32(d.Y),
		func(ex, ey float32) {
			r.pts = append(r.pts, fixed.Point26_6{X: fixed.Int26_6(ex), Y: fixed.Int26_6(ey)})
		})
}

// Stop ends the current subpath and strokes it. If isClosed is true the
// path is treated as a closed loop: the ends are joined to each other and
// no caps are drawn. Otherwise caps are drawn at both ends.
func (r *VarStroker) Stop(isClosed bool) {
	if !r.inPath {
		return
	}
	r.render(isClosed)
	r.inPath = false
	r.pts = r.pts[:0]
}

// Clear resets the stroker, discarding any unstroked subpath.
func (r *VarStroker) Clear() {
	r.pts = r.pts[:0]
	r.inPath = false
	r.Filler.Clear()
}

// closeEnough reports if two points are within about 1/64 of a unit,
// close enough to be treated as the same point.
func closeEnough(a, b fixed.Point26_6) bool {
	dx, dy := a.X-b.X, a.Y-b.Y
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	return dx+dy <= 1
}

// render strokes the accumulated subpath. It is a no-op for paths with
// zero total length.
func (r *VarStroker) render(closed bool) {
	// Remove zero length (and sub 1/64 unit) segments.
	pts := r.pts[:0]
	for _, p := range r.pts {
		if len(pts) == 0 || !closeEnough(pts[len(pts)-1], p) {
			pts = append(pts, p)
		}
	}
	if closed {
		for len(pts) > 1 && closeEnough(pts[0], pts[len(pts)-1]) {
			pts = pts[:len(pts)-1]
		}
		if len(pts) < 3 { // A closed degenerate loop is just an open line.
			closed = false
		}
	}
	if len(pts) < 2 {
		return
	}
	if closed {
		pts = append(pts, pts[0])
	}

	// Cumulative arc length.
	cum := make([]fixed.Int26_6, len(pts))
	for i := 1; i < len(pts); i++ {
		cum[i] = cum[i-1] + Length(pts[i].Sub(pts[i-1]))
	}
	total := cum[len(cum)-1]
	if total <= 0 {
		return
	}

	// Insert vertices at the width control positions so that the
	// piecewise linear width profile is reproduced exactly, even on a
	// single straight line segment.
	if len(r.widths) > 1 {
		pts, cum = insertWidthPoints(pts, cum, r.widths, total)
	}

	n := len(pts) - 1 // number of segments
	h := make([]fixed.Int26_6, len(pts))
	for i := range pts {
		h[i] = r.halfWidthAt(cum[i], total)
	}
	tan := make([]fixed.Point26_6, n) // unit tangents, scaled to 64
	nrm := make([]fixed.Point26_6, n) // unit port (left) normals
	for i := 0; i < n; i++ {
		tan[i] = ToLength(pts[i+1].Sub(pts[i]), 1<<6)
		nrm[i] = turnPort90(tan[i])
	}

	if closed {
		r.renderClosed(pts, tan, nrm, h)
	} else {
		r.renderOpen(pts, tan, nrm, h)
	}
}

// insertWidthPoints splits the polyline at the arc length positions of the
// interior width control points, returning the refined polyline and its
// cumulative arc lengths. ws must be sorted by Pos.
func insertWidthPoints(pts []fixed.Point26_6, cum []fixed.Int26_6, ws []WidthPoint,
	total fixed.Int26_6) ([]fixed.Point26_6, []fixed.Int26_6) {
	pos := make([]fixed.Int26_6, 0, len(ws))
	for _, w := range ws { // ws is sorted, so pos is sorted
		if w.Pos > 0 && w.Pos < 1 {
			if s := fixed.Int26_6(w.Pos * float64(total)); s > 1 && s < total-1 {
				pos = append(pos, s)
			}
		}
	}
	if len(pos) == 0 {
		return pts, cum
	}
	out := make([]fixed.Point26_6, 0, len(pts)+len(pos))
	outCum := make([]fixed.Int26_6, 0, len(cum)+len(pos))
	out, outCum = append(out, pts[0]), append(outCum, cum[0])
	k := 0
	for i := 1; i < len(pts); i++ {
		segLen := cum[i] - cum[i-1]
		for k < len(pos) && pos[k] < cum[i]-1 {
			if pos[k] > cum[i-1]+1 && segLen > 0 {
				t := float64(pos[k]-cum[i-1]) / float64(segLen)
				out = append(out, fixed.Point26_6{
					X: pts[i-1].X + fixed.Int26_6(float64(pts[i].X-pts[i-1].X)*t),
					Y: pts[i-1].Y + fixed.Int26_6(float64(pts[i].Y-pts[i-1].Y)*t),
				})
				outCum = append(outCum, pos[k])
			}
			k++
		}
		out, outCum = append(out, pts[i]), append(outCum, cum[i])
	}
	return out, outCum
}

// renderOpen strokes an open polyline: the left outline forward, the right
// outline backward, then the end caps.
func (r *VarStroker) renderOpen(pts []fixed.Point26_6, tan, nrm []fixed.Point26_6, h []fixed.Int26_6) {
	ra := &r.Filler
	n := len(tan)
	last := len(pts) - 1

	// Left (port side) outline, forward.
	ra.Start(pts[0].Add(nrm[0].Mul(h[0])))
	for i := 1; i < last; i++ {
		a := pts[i].Add(nrm[i-1].Mul(h[i]))
		b := pts[i].Add(nrm[i].Mul(h[i]))
		cross := int64(tan[i-1].X)*int64(tan[i].Y) - int64(tan[i-1].Y)*int64(tan[i].X)
		r.emitJoin(ra, pts[i], a, tan[i-1], b, tan[i], cross > joinCrossEps,
			nrm[i-1].Add(nrm[i]), h[i])
	}
	ra.Line(pts[last].Add(nrm[n-1].Mul(h[last])))
	// Across the butt edge at the end.
	ra.Line(pts[last].Sub(nrm[n-1].Mul(h[last])))
	// Right (starboard side) outline, backward.
	for i := last - 1; i >= 1; i-- {
		a := pts[i].Sub(nrm[i].Mul(h[i]))
		b := pts[i].Sub(nrm[i-1].Mul(h[i]))
		cross := int64(tan[i-1].X)*int64(tan[i].Y) - int64(tan[i-1].Y)*int64(tan[i].X)
		r.emitJoin(ra, pts[i], a, tan[i], b, tan[i-1], cross < -joinCrossEps,
			Invert(nrm[i-1].Add(nrm[i])), h[i])
	}
	ra.Line(pts[0].Sub(nrm[0].Mul(h[0])))
	ra.Line(pts[0].Add(nrm[0].Mul(h[0]))) // close across the butt edge at the start

	// End caps; the local half width is encoded in the normal length. The
	// closingAdder ensures every cap subpath is explicitly closed, as the
	// scan converters require balanced edges.
	cc := &closingAdder{a: ra}
	r.CapL(cc, pts[last], nrm[n-1].Mul(h[last]))
	cc.Stop(false)
	r.CapT(cc, pts[0], Invert(nrm[0].Mul(h[0])))
	cc.Stop(false)
}

// renderClosed strokes a closed loop: the left outline and the reversed
// right outline form two closed rings with opposite orientations, so the
// non-zero winding rule fills exactly the band between them. No caps.
func (r *VarStroker) renderClosed(pts []fixed.Point26_6, tan, nrm []fixed.Point26_6, h []fixed.Int26_6) {
	ra := &r.Filler
	n := len(tan) // number of segments == number of vertices

	// Left outline ring, forward. Start at the outgoing edge point of
	// vertex 0 and process vertices 1..n-1,0 so the ring closes on itself.
	b0 := pts[0].Add(nrm[0].Mul(h[0]))
	ra.Start(b0)
	for k := 1; k <= n; k++ {
		i := k % n
		ip := (i + n - 1) % n
		a := pts[i].Add(nrm[ip].Mul(h[i]))
		b := pts[i].Add(nrm[i].Mul(h[i]))
		cross := int64(tan[ip].X)*int64(tan[i].Y) - int64(tan[ip].Y)*int64(tan[i].X)
		r.emitJoin(ra, pts[i], a, tan[ip], b, tan[i], cross > joinCrossEps,
			nrm[ip].Add(nrm[i]), h[i])
	}
	ra.Line(b0) // explicitly close the ring

	// Right outline ring, backward.
	c0 := pts[0].Sub(nrm[n-1].Mul(h[0]))
	ra.Start(c0)
	for k := n - 1; k >= 0; k-- {
		i := k
		ip := (i + n - 1) % n
		a := pts[i].Sub(nrm[i].Mul(h[i]))
		b := pts[i].Sub(nrm[ip].Mul(h[i]))
		cross := int64(tan[ip].X)*int64(tan[i].Y) - int64(tan[ip].Y)*int64(tan[i].X)
		r.emitJoin(ra, pts[i], a, tan[i], b, tan[ip], cross < -joinCrossEps,
			Invert(nrm[ip].Add(nrm[i])), h[i])
	}
	ra.Line(c0) // explicitly close the ring
}

// emitJoin emits the join geometry at a path vertex for one side of the
// outline. a and b are the offset points where the incoming and outgoing
// edges meet the vertex, tanIn and tanOut are the unit tangents of the
// segments those edges belong to, convex indicates the side is the outside
// of the turn, and bis is the bisector direction pointing toward the convex
// side (used for miter clipping). h is the local half width. Miter and clip
// intersections use the constant width offset lines through a and b (along
// the centerline tangents) rather than the tapered edge lines, which keeps
// the join geometry stable when the width changes quickly.
func (r *VarStroker) emitJoin(ra *Filler, p, a, tanIn, b, tanOut fixed.Point26_6,
	convex bool, bis fixed.Point26_6, h fixed.Int26_6) {
	if !convex {
		ra.Line(a)
		ra.Line(b)
		return
	}
	switch r.JoinMode {
	case Round, Arc, ArcClip:
		a1, a2 := a.Sub(p), b.Sub(p)
		clockwise := int64(a1.X)*int64(a2.Y) > int64(a1.Y)*int64(a2.X)
		strokeArc(ra, p, a, b, clockwise, 0, 0, ra.Line)
		ra.Line(b) // ensure a clean join to the arc end
	case Miter, MiterClip:
		limit := (int64(r.mLimit) * int64(h)) >> 6 // miter limit in fixed units
		if limit > 1<<30 {
			limit = 1 << 30
		}
		if x, ok := intersectWithin(a, tanIn, b, tanOut, p, float64(limit)); ok {
			ra.Line(x)
			return
		}
		if r.JoinMode == MiterClip && limit > 0 && (bis.X != 0 || bis.Y != 0) {
			// Clip the miter with the line perpendicular to the bisector
			// at the miter limit distance from the vertex.
			c := p.Add(ToLength(bis, fixed.Int26_6(limit)))
			cd := turnPort90(ToLength(bis, 1<<6))
			p1, ok1 := lineIntersect(a, tanIn, c, cd)
			p2, ok2 := lineIntersect(b, tanOut, c, cd)
			if ok1 && ok2 {
				ra.Line(p1)
				ra.Line(p2)
				return
			}
		}
		// Miter limit exceeded, or near-parallel edges: bevel.
		ra.Line(a)
		ra.Line(b)
	default: // Bevel
		ra.Line(a)
		ra.Line(b)
	}
}

// lineIntersectF calculates the intersection of the line through p1 along
// d1 with the line through p2 along d2 in floating point. ok is false if
// the lines are (nearly) parallel or degenerate.
func lineIntersectF(p1, d1, p2, d2 fixed.Point26_6) (x, y float64, ok bool) {
	d1x, d1y := float64(d1.X), float64(d1.Y)
	d2x, d2y := float64(d2.X), float64(d2.Y)
	l1 := math.Hypot(d1x, d1y)
	l2 := math.Hypot(d2x, d2y)
	if l1 == 0 || l2 == 0 {
		return
	}
	det := d1x*d2y - d2x*d1y
	if math.Abs(det) < 1e-6*l1*l2 {
		return // nearly parallel
	}
	dx, dy := float64(p2.X-p1.X), float64(p2.Y-p1.Y)
	t := (dx*d2y - dy*d2x) / det
	if math.Abs(t)*l1 > 1<<28 {
		return // intersection unreasonably far; treat as parallel
	}
	return float64(p1.X) + d1x*t, float64(p1.Y) + d1y*t, true
}

// lineIntersect is lineIntersectF returning a fixed point.
func lineIntersect(p1, d1, p2, d2 fixed.Point26_6) (fixed.Point26_6, bool) {
	x, y, ok := lineIntersectF(p1, d1, p2, d2)
	return fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)}, ok
}

// intersectWithin returns the intersection of the two lines, and ok == true
// only if the intersection exists and is within limit of c.
func intersectWithin(p1, d1, p2, d2, c fixed.Point26_6, limit float64) (fixed.Point26_6, bool) {
	x, y, ok := lineIntersectF(p1, d1, p2, d2)
	if !ok {
		return fixed.Point26_6{}, false
	}
	if math.Hypot(x-float64(c.X), y-float64(c.Y)) > limit {
		return fixed.Point26_6{}, false
	}
	return fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)}, true
}

// closingAdder wraps an Adder and explicitly closes every subpath with a
// final line back to its start point. The scan converters used by rasterx
// accumulate signed coverage and require the edges of every subpath to be
// balanced; CapFunc implementations such as ButtCap emit open subpaths, so
// cap drawing is wrapped with this.
type closingAdder struct {
	a          Adder
	first, cur fixed.Point26_6
	open       bool
}

func (c *closingAdder) Start(p fixed.Point26_6) {
	c.Stop(false) // close any previous subpath first
	c.first, c.cur, c.open = p, p, true
	c.a.Start(p)
}

func (c *closingAdder) Line(p fixed.Point26_6) {
	c.cur = p
	c.a.Line(p)
}

func (c *closingAdder) QuadBezier(b, d fixed.Point26_6) {
	c.cur = d
	c.a.QuadBezier(b, d)
}

func (c *closingAdder) CubeBezier(b, d, e fixed.Point26_6) {
	c.cur = e
	c.a.CubeBezier(b, d, e)
}

// Stop closes the current subpath if it is open.
func (c *closingAdder) Stop(closeLoop bool) {
	if c.open && c.cur != c.first {
		c.a.Line(c.first)
	}
	c.open = false
}
