// Copyright 2026 by the rasterx Authors. All rights reserved.
//
// Variable width stroking for rasterx: stroke a path with a width that
// varies along its arc length, e.g. for pressure-sensitive speed lines
// that taper from thin to thick and back, which a fixed-width Stroker
// cannot express.
package rasterx

import (
	"math"

	"golang.org/x/image/math/fixed"
)

const (
	// defaultVarStrokeWidth is the constant stroke width, in pixels, used
	// by a VarStroker when no width function has been set.
	defaultVarStrokeWidth = 2.0
	// defaultMaxVarStrokeWidth bounds the width a WidthFunc may produce,
	// in pixels. It is a resource limit against extreme width values.
	defaultMaxVarStrokeWidth = 4096.0
	// defaultVarMiterLimit bounds the miter length as a multiple of the
	// half width before a join falls back to a bevel (or round) join.
	defaultVarMiterLimit = 4.0
	// maxVarStrokePoints bounds the number of flattened points kept for a
	// single subpath; points beyond the limit are dropped. This is a
	// resource limit against adversarial geometry.
	maxVarStrokePoints = 1 << 16
	// maxVarFlattenSegments bounds the number of line segments a single
	// bezier curve is flattened into.
	maxVarFlattenSegments = 4096
	// minVarSegLenSq is the squared minimum segment length in fixed point
	// units ((1/8 pixel)^2); shorter segments are merged into their
	// predecessor so that normals stay well defined.
	minVarSegLenSq = 64
)

// WidthFunc returns the stroke width, in pixels, at the normalized arc
// length t (0 <= t <= 1) of the subpath being stroked.
//
// A WidthFunc must be pure: it must not mutate the path being stroked and
// it must not rely on mutable state shared with other goroutines.
// VarStroker copies the subpath geometry into internal buffers before any
// WidthFunc call and only ever calls it sequentially from the goroutine
// that invoked the stroking method, so a well-behaved WidthFunc cannot
// affect path traversal and is never called concurrently by a single
// VarStroker. If the same WidthFunc is shared by several VarStrokers used
// from different goroutines, it must be safe for concurrent calls.
type WidthFunc func(t float64) float64

// ConstantWidth returns a WidthFunc that yields w at every point.
func ConstantWidth(w float64) WidthFunc {
	return func(float64) float64 { return w }
}

// WidthProfile returns a WidthFunc that linearly interpolates the given
// width samples, evenly spaced over the normalized arc length of each
// subpath. A single sample yields a constant width; no samples yields a
// zero width everywhere.
func WidthProfile(samples ...float64) WidthFunc {
	cp := make([]float64, len(samples))
	copy(cp, samples)
	switch len(cp) {
	case 0:
		return func(float64) float64 { return 0 }
	case 1:
		return ConstantWidth(cp[0])
	}
	return func(t float64) float64 {
		if t <= 0 {
			return cp[0]
		}
		if t >= 1 {
			return cp[len(cp)-1]
		}
		x := t * float64(len(cp)-1)
		i := int(x)
		f := x - float64(i)
		return cp[i]*(1-f) + cp[i+1]*f
	}
}

// sanitizeWidth applies the width value rules: non-finite and non-positive
// widths become 0 (the outline tapers to the centerline), and widths above
// maxW are clamped to maxW.
func sanitizeWidth(w, maxW float64) float64 {
	if math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 {
		return 0
	}
	if w > maxW {
		return maxW
	}
	return w
}

// VarStroker strokes paths with a width that varies along the arc length
// of each subpath. It satisfies the Adder interface, so a Path can be
// replayed into it with AddTo, and it embeds a Filler so Draw, Clear,
// SetBounds, SetClip and the Scanner configuration work as usual.
//
// The zero value is not ready for use; use NewVarStroker.
//
// Geometry and width rules:
//
//   - Lines, quadratic and cubic beziers are flattened to a polyline per
//     subpath (beziers with at most maxVarFlattenSegments segments).
//     Segments shorter than 1/8 pixel are merged. A subpath is capped at
//     maxVarStrokePoints points; further points are dropped.
//   - WidthFn is evaluated once per flattened vertex at the normalized
//     arc length t of the subpath. For closed subpaths t is normalized
//     over the full loop including the closing edge. Dashes do not
//     renormalize t: a dash inherits the t of its position on the
//     subpath.
//   - Width values that are zero, negative, NaN or infinite are treated
//     as 0: the outline tapers to the centerline point. A subpath whose
//     width is 0 everywhere produces no output. Widths above MaxWidth
//     are clamped to MaxWidth.
//   - Normals are the port (left of travel) unit normals of each
//     segment. At interior vertices the offset point is the miter point
//     (normalized sum of the adjacent normals, scaled by half the width
//     over the cosine of half the turn angle). When the miter length
//     exceeds MiterLimit times the half width, both sides of the vertex
//     fall back to a bevel join, or to a round join when JoinMode is
//     Round, Arc or ArcClip. A 180 degree reversal is beveled. JoinMode
//     Bevel bevels every convex join.
//   - Open subpaths are ended with CapL (trailing end) and CapT (start);
//     a nil cap is treated as ButtCap. Closed subpaths (Stop(true)) get
//     no caps; the last-to-first join uses the same join rule as any
//     interior vertex.
//   - Each Start begins a new subpath and t restarts at 0; a previous
//     unclosed subpath is flushed as an open subpath. Subpaths are
//     rasterized independently, so the Scanner's clip rectangle applies
//     to each emitted subpath as usual.
//   - An empty subpath (fewer than two points or zero total length)
//     produces no output.
//
// A VarStroker, like the rest of rasterx, is not safe for concurrent use.
type VarStroker struct {
	Filler
	// WidthFn maps the normalized arc length of the current subpath to
	// the stroke width in pixels. If nil, a constant width of
	// defaultVarStrokeWidth pixels is used.
	WidthFn WidthFunc
	// MaxWidth clamps the width produced by WidthFn, in pixels.
	// Values <= 0 select the default.
	MaxWidth float64
	// MiterLimit bounds the miter length as a multiple of the half
	// width. Values <= 0 select the default.
	MiterLimit float64
	// JoinMode selects the join style: Round, Arc or ArcClip produce
	// round joins, Bevel produces bevel joins, and Miter or MiterClip
	// produce miter joins that fall back to bevel past MiterLimit.
	JoinMode JoinMode
	// CapL and CapT cap the trailing and leading ends of open subpaths
	// and of each dash. Nil is treated as ButtCap.
	CapL, CapT CapFunc

	dashes     []float64 // sanitized dash pattern in pixels; nil = solid
	dashOffset float64

	pts       []fixed.Point26_6
	inPath    bool
	truncated bool // set when maxVarStrokePoints was reached
}

// NewVarStroker returns a VarStroker with default values: a constant 2
// pixel width, butt caps, miter joins with a miter limit of 4, and no
// dashes. If scanner is nil a ScannerGV is used.
func NewVarStroker(width, height int, scanner Scanner) *VarStroker {
	r := new(VarStroker)
	if scanner != nil {
		r.Scanner = scanner
	} else {
		r.Scanner = new(ScannerGV)
	}
	r.SetBounds(width, height)
	r.SetWinding(true)
	r.WidthFn = ConstantWidth(defaultVarStrokeWidth)
	r.MaxWidth = defaultMaxVarStrokeWidth
	r.MiterLimit = defaultVarMiterLimit
	r.JoinMode = MiterClip
	r.CapL, r.CapT = ButtCap, ButtCap
	return r
}

// SetStrokeWidth sets a constant stroke width in pixels.
func (r *VarStroker) SetStrokeWidth(w float64) {
	r.WidthFn = ConstantWidth(w)
}

// SetStrokeWidthProfile sets the stroke width from samples evenly spaced
// over the normalized arc length of each subpath, linearly interpolated.
func (r *VarStroker) SetStrokeWidthProfile(samples []float64) {
	r.WidthFn = WidthProfile(samples...)
}

// SetStrokeWidthFunc sets an arbitrary width callback. Passing nil
// restores the default constant width.
func (r *VarStroker) SetStrokeWidthFunc(fn WidthFunc) {
	if fn == nil {
		fn = ConstantWidth(defaultVarStrokeWidth)
	}
	r.WidthFn = fn
}

// SetDashes sets the dash pattern in pixels, applied along the arc length
// of each subpath with the given offset. Elements alternate between dashes
// and gaps; an odd number of elements therefore alternates on and off
// across pattern repetitions, as in SVG. Non-positive and non-finite
// elements are dropped; an empty or nil pattern disables dashing.
func (r *VarStroker) SetDashes(dashes []float64, dashOffset float64) {
	r.dashes = r.dashes[:0]
	for _, d := range dashes {
		if d <= 0 || math.IsNaN(d) || math.IsInf(d, 0) {
			continue
		}
		r.dashes = append(r.dashes, d)
	}
	if len(r.dashes) == 0 {
		r.dashes = nil
	}
	if math.IsNaN(dashOffset) || math.IsInf(dashOffset, 0) {
		dashOffset = 0
	}
	r.dashOffset = dashOffset
}

// Truncated reports whether the last subpath exceeded the point limit and
// had geometry dropped.
func (r *VarStroker) Truncated() bool { return r.truncated }

// Clear resets the rasterizer and any pending subpath.
func (r *VarStroker) Clear() {
	r.pts = r.pts[:0]
	r.inPath = false
	r.truncated = false
	r.Filler.Clear()
}

// Start begins a new subpath. Any pending subpath is flushed as open.
func (r *VarStroker) Start(a fixed.Point26_6) {
	if r.inPath {
		r.flush(false)
	}
	r.pts = append(r.pts[:0], a)
	r.inPath = true
	r.truncated = false
}

// Line adds a line segment to the current subpath.
func (r *VarStroker) Line(b fixed.Point26_6) {
	r.addPt(b)
}

// QuadBezier adds a quadratic bezier segment to the current subpath.
func (r *VarStroker) QuadBezier(b, c fixed.Point26_6) {
	if !r.inPath {
		r.Start(c)
		return
	}
	a := r.pts[len(r.pts)-1]
	if a == b || b == c { // degenerate bezier
		r.addPt(c)
		return
	}
	devsq := devSquared(float32(a.X), float32(a.Y),
		float32(b.X), float32(b.Y), float32(c.X), float32(c.Y))
	n := 1
	if devsq >= 0.333 {
		n = 1 + int(math.Sqrt(math.Sqrt(3*float64(devsq))))
		if n > maxVarFlattenSegments {
			n = maxVarFlattenSegments
		}
	}
	ax, ay := float32(a.X), float32(a.Y)
	bx, by := float32(b.X), float32(b.Y)
	cx, cy := float32(c.X), float32(c.Y)
	for i := 1; i < n; i++ {
		t := float32(i) / float32(n)
		mt := 1 - t
		r.addPt(fixed.Point26_6{
			fixed.Int26_6(ax*mt*mt + bx*2*mt*t + cx*t*t),
			fixed.Int26_6(ay*mt*mt + by*2*mt*t + cy*t*t),
		})
	}
	r.addPt(c)
}

// CubeBezier adds a cubic bezier segment to the current subpath.
func (r *VarStroker) CubeBezier(b, c, d fixed.Point26_6) {
	if !r.inPath {
		r.Start(d)
		return
	}
	a := r.pts[len(r.pts)-1]
	if (a == b && c == d) || (a == b && b == c) || (c == b && d == c) {
		r.addPt(d)
		return
	}
	devsq := devSquared(float32(a.X), float32(a.Y),
		float32(b.X), float32(b.Y), float32(d.X), float32(d.Y))
	if alt := devSquared(float32(a.X), float32(a.Y),
		float32(c.X), float32(c.Y), float32(d.X), float32(d.Y)); devsq < alt {
		devsq = alt
	}
	n := 1
	if devsq >= 0.333 {
		n = 1 + int(math.Sqrt(math.Sqrt(3*float64(devsq))))
		if n > maxVarFlattenSegments {
			n = maxVarFlattenSegments
		}
	}
	ax, ay := float32(a.X), float32(a.Y)
	bx, by := float32(b.X), float32(b.Y)
	cx, cy := float32(c.X), float32(c.Y)
	dx, dy := float32(d.X), float32(d.Y)
	for i := 1; i < n; i++ {
		t := float32(i) / float32(n)
		mt := 1 - t
		mtsq, tsq := mt*mt, t*t
		r.addPt(fixed.Point26_6{
			fixed.Int26_6(ax*mtsq*mt + bx*3*mtsq*t + cx*3*mt*tsq + dx*tsq*t),
			fixed.Int26_6(ay*mtsq*mt + by*3*mtsq*t + cy*3*mt*tsq + dy*tsq*t),
		})
	}
	r.addPt(d)
}

// Stop ends the current subpath and strokes it. If isClosed is true the
// subpath is treated as a closed loop: no caps are drawn and the
// last-to-first join follows the join rule.
func (r *VarStroker) Stop(isClosed bool) {
	if !r.inPath {
		return
	}
	r.flush(isClosed)
}

// addPt appends a point to the current subpath, merging segments shorter
// than the minimum segment length and enforcing the point limit.
func (r *VarStroker) addPt(b fixed.Point26_6) {
	if !r.inPath {
		r.Start(b)
		return
	}
	if len(r.pts) >= maxVarStrokePoints {
		r.truncated = true
		return
	}
	last := r.pts[len(r.pts)-1]
	dx, dy := int64(b.X-last.X), int64(b.Y-last.Y)
	if dx*dx+dy*dy < minVarSegLenSq {
		return
	}
	r.pts = append(r.pts, b)
}

// ptDist returns the distance between two fixed points in pixels.
func ptDist(a, b fixed.Point26_6) float64 {
	dx := float64(b.X-a.X) / 64
	dy := float64(b.Y-a.Y) / 64
	return math.Hypot(dx, dy)
}

// flush strokes the pending subpath.
func (r *VarStroker) flush(closed bool) {
	pts := r.pts
	r.inPath = false
	if len(pts) < 2 {
		return
	}
	if closed {
		if len(pts) < 3 {
			closed = false // a closed 2-point path is just a line
		} else if dx, dy := int64(pts[0].X-pts[len(pts)-1].X), int64(pts[0].Y-pts[len(pts)-1].Y); dx*dx+dy*dy < minVarSegLenSq {
			pts = pts[:len(pts)-1] // drop duplicate of the start point
		}
	}
	cum := make([]float64, len(pts))
	for i := 1; i < len(pts); i++ {
		cum[i] = cum[i-1] + ptDist(pts[i-1], pts[i])
	}
	total := cum[len(cum)-1]
	if closed {
		total += ptDist(pts[len(pts)-1], pts[0])
	}
	if total <= 0 {
		return
	}
	if len(r.dashes) > 0 {
		if closed { // dash around the closure
			cpts := make([]fixed.Point26_6, len(pts)+1)
			copy(cpts, pts)
			cpts[len(pts)] = pts[0]
			r.emitDashed(cpts, append(cum, total), total)
		} else {
			r.emitDashed(pts, cum, total)
		}
		return
	}
	ts := make([]float64, len(pts))
	for i := range ts {
		ts[i] = cum[i] / total
	}
	r.strokePoly(pts, ts, closed)
}

// offsetPt returns p displaced by d pixels along the unit vector (nx, ny).
func offsetPt(p fixed.Point26_6, nx, ny, d float64) fixed.Point26_6 {
	return fixed.Point26_6{
		p.X + fixed.Int26_6(nx*d*64),
		p.Y + fixed.Int26_6(ny*d*64),
	}
}

// appendArcPts appends points on the arc of radius r centered at c from
// unit direction (nx0, ny0) to (nx1, ny1), taking the short way around.
func appendArcPts(dst []fixed.Point26_6, c fixed.Point26_6, nx0, ny0, nx1, ny1, r float64) []fixed.Point26_6 {
	a0 := math.Atan2(ny0, nx0)
	da := math.Atan2(ny1, nx1) - a0
	for da > math.Pi {
		da -= 2 * math.Pi
	}
	for da < -math.Pi {
		da += 2 * math.Pi
	}
	steps := int(math.Abs(da)/(math.Pi/8)) + 1
	for i := 1; i < steps; i++ {
		a := a0 + da*float64(i)/float64(steps)
		dst = append(dst, offsetPt(c, math.Cos(a), math.Sin(a), r))
	}
	return dst
}

// strokePoly strokes the polyline pts with per-vertex normalized arc
// lengths ts. When closed is true the polyline is a loop and no caps are
// drawn.
func (r *VarStroker) strokePoly(pts []fixed.Point26_6, ts []float64, closed bool) {
	n := len(pts)
	if n < 2 {
		return
	}
	wf := r.WidthFn
	if wf == nil {
		wf = ConstantWidth(defaultVarStrokeWidth)
	}
	maxW := r.MaxWidth
	if maxW <= 0 {
		maxW = defaultMaxVarStrokeWidth
	}
	halves := make([]float64, n)
	any := false
	for i := range pts {
		halves[i] = sanitizeWidth(wf(ts[i]), maxW) / 2
		if halves[i] > 0 {
			any = true
		}
	}
	if !any {
		return
	}

	// Per-segment port (left of travel) unit normals.
	segN := n - 1
	if closed {
		segN = n
	}
	nx := make([]float64, segN)
	ny := make([]float64, segN)
	for i := 0; i < segN; i++ {
		j := i + 1
		if j == n {
			j = 0
		}
		sx := float64(pts[j].X-pts[i].X) / 64
		sy := float64(pts[j].Y-pts[i].Y) / 64
		l := math.Hypot(sx, sy)
		if l == 0 { // duplicate point; reuse the previous normal
			if i > 0 {
				nx[i], ny[i] = nx[i-1], ny[i-1]
			} else {
				nx[i], ny[i] = 0, -1
			}
			continue
		}
		nx[i], ny[i] = sy/l, -sx/l
	}

	roundJoin := r.JoinMode == Round || r.JoinMode == Arc || r.JoinMode == ArcClip
	bevelAlways := r.JoinMode == Bevel
	mLimit := r.MiterLimit
	if mLimit <= 0 {
		mLimit = defaultVarMiterLimit
	}

	// addJoin appends a bevel or round join between the two offset points
	// on one side of vertex p.
	addJoin := func(dst []fixed.Point26_6, p fixed.Point26_6, nx0, ny0, nx1, ny1, h float64) []fixed.Point26_6 {
		dst = append(dst, offsetPt(p, nx0, ny0, h))
		if roundJoin && h > 0 {
			dst = appendArcPts(dst, p, nx0, ny0, nx1, ny1, h)
		}
		return append(dst, offsetPt(p, nx1, ny1, h))
	}

	left := make([]fixed.Point26_6, 0, n+8)
	right := make([]fixed.Point26_6, 0, n+8)
	for i := 0; i < n; i++ {
		p := pts[i]
		h := halves[i]
		if !closed && i == 0 {
			left = append(left, offsetPt(p, nx[0], ny[0], h))
			right = append(right, offsetPt(p, -nx[0], -ny[0], h))
			continue
		}
		if !closed && i == n-1 {
			left = append(left, offsetPt(p, nx[segN-1], ny[segN-1], h))
			right = append(right, offsetPt(p, -nx[segN-1], -ny[segN-1], h))
			continue
		}
		s0 := i - 1
		if closed && i == 0 {
			s0 = segN - 1
		}
		s1 := i
		mx, my := nx[s0]+nx[s1], ny[s0]+ny[s1]
		ml := math.Hypot(mx, my)
		if ml < 1e-9 { // 180 degree reversal: bevel both sides
			left = addJoin(left, p, nx[s0], ny[s0], nx[s1], ny[s1], h)
			right = addJoin(right, p, -nx[s0], -ny[s0], -nx[s1], -ny[s1], h)
			continue
		}
		mx, my = mx/ml, my/ml
		cosHalf := mx*nx[s1] + my*ny[s1]
		miterLen := h
		if cosHalf > 1e-9 {
			miterLen = h / cosHalf
		}
		if h == 0 || (!bevelAlways && miterLen <= mLimit*h) {
			left = append(left, offsetPt(p, mx, my, miterLen))
			right = append(right, offsetPt(p, -mx, -my, miterLen))
			continue
		}
		// The miter limit is exceeded: both sides fall back to a bevel or
		// round join. Near a 180 degree reversal the miter point of either
		// side can lie arbitrarily far from the vertex, so exempting the
		// concave side (as some renderers do for mild turns) is not sound
		// here; the small concave notch a bevel cuts is covered by the
		// overlapping stroke body at such turn angles.
		left = addJoin(left, p, nx[s0], ny[s0], nx[s1], ny[s1], h)
		right = addJoin(right, p, -nx[s0], -ny[s0], -nx[s1], -ny[s1], h)
	}

	f := &r.Filler
	f.Start(left[0])
	for i := 1; i < len(left); i++ {
		f.Line(left[i])
	}
	for i := len(right) - 1; i >= 0; i-- {
		f.Line(right[i])
	}
	// Close the outline contour: the scanner accumulates signed edge
	// coverage, so every emitted contour must have balanced edges or the
	// fill floods past the intended region.
	f.Stop(true)
	if !closed {
		capL, capT := r.CapL, r.CapT
		if capL == nil {
			capL = ButtCap
		}
		if capT == nil {
			capT = ButtCap
		}
		endNorm := fixed.Point26_6{
			fixed.Int26_6(nx[segN-1] * halves[n-1] * 64),
			fixed.Int26_6(ny[segN-1] * halves[n-1] * 64),
		}
		startNorm := fixed.Point26_6{
			fixed.Int26_6(nx[0] * halves[0] * 64),
			fixed.Int26_6(ny[0] * halves[0] * 64),
		}
		capL(f, pts[n-1], endNorm)
		f.Stop(true) // balance the cap contour's edges
		capT(f, pts[0], Invert(startNorm))
		f.Stop(true) // balance the cap contour's edges
	}
}

// emitDashed strokes the polyline pts (with cumulative arc lengths cum and
// total length total) as a series of dashes. Each dash is stroked as an
// open polyline with the configured caps; widths are sampled at the
// subpath-normalized arc length, so dashes do not renormalize t.
func (r *VarStroker) emitDashed(pts []fixed.Point26_6, cum []float64, total float64) {
	sum := 0.0
	for _, d := range r.dashes {
		sum += d
	}
	if sum <= 0 {
		return
	}
	off := math.Mod(r.dashOffset, sum)
	if off < 0 {
		off += sum
	}
	place := 0
	on := true // the first dash element is a dash, not a gap
	for off > r.dashes[place] {
		off -= r.dashes[place]
		on = !on
		place++
		if place == len(r.dashes) {
			place = 0
		}
	}
	leftInDash := r.dashes[place] - off
	pos := 0.0
	for pos < total {
		step := leftInDash
		if pos+step > total {
			step = total - pos
		}
		if on && step > 0 {
			sp, st := extractSubPoly(pts, cum, pos, pos+step, total)
			if len(sp) >= 2 {
				r.strokePoly(sp, st, false)
			}
		}
		pos += step
		leftInDash -= step
		if leftInDash <= 0 {
			place++
			if place == len(r.dashes) {
				place = 0
			}
			on = !on
			leftInDash = r.dashes[place]
		}
	}
}

// extractSubPoly returns the sub-polyline of pts between arc lengths s0
// and s1, with interpolated endpoints, together with the subpath-normalized
// arc length of each returned point.
func extractSubPoly(pts []fixed.Point26_6, cum []float64, s0, s1, total float64) (sp []fixed.Point26_6, st []float64) {
	for i := 1; i < len(cum); i++ {
		if cum[i] <= s0 {
			continue
		}
		if cum[i-1] >= s1 {
			break
		}
		a, b := s0, s1
		if a < cum[i-1] {
			a = cum[i-1]
		}
		if b > cum[i] {
			b = cum[i]
		}
		segLen := cum[i] - cum[i-1]
		lerp := func(s float64) fixed.Point26_6 {
			if segLen <= 0 {
				return pts[i-1]
			}
			f := (s - cum[i-1]) / segLen
			return fixed.Point26_6{
				pts[i-1].X + fixed.Int26_6(float64(pts[i].X-pts[i-1].X)*f),
				pts[i-1].Y + fixed.Int26_6(float64(pts[i].Y-pts[i-1].Y)*f),
			}
		}
		if len(sp) == 0 {
			sp = append(sp, lerp(a))
			st = append(st, a/total)
		}
		if pb := lerp(b); pb != sp[len(sp)-1] {
			sp = append(sp, pb)
			st = append(st, b/total)
		}
	}
	return sp, st
}
