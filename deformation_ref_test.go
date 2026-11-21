// Copyright 2026 The rasterx Authors. All rights reserved.
//
// Independent reference geometry used by the deformation tests.
//
// Nothing in this file calls rasterx's transform, stroker, or dasher code.
// It intentionally re-implements affine composition, polygon winding,
// stroking outlines, dash walking, and coverage so that a bug in the code
// under test cannot hide by also changing the answer it is compared against.
package rasterx_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"math"
)

// ---------------------------------------------------------------------------
// Affine transforms (independent implementation)
// ---------------------------------------------------------------------------

type refPoint struct {
	X, Y float64
}

type refPoly []refPoint

// refAffine is an SVG-style affine matrix (x' = A*x + C*y + E).
type refAffine struct {
	A, B, C, D, E, F float64
}

func refIdentity() refAffine { return refAffine{1, 0, 0, 1, 0, 0} }

func (m refAffine) apply(p refPoint) refPoint {
	return refPoint{m.A*p.X + m.C*p.Y + m.E, m.B*p.X + m.D*p.Y + m.F}
}

// compose returns m∘n: n is applied to the point first, then m.
func (m refAffine) compose(n refAffine) refAffine {
	return refAffine{
		A: m.A*n.A + m.C*n.B,
		B: m.B*n.A + m.D*n.B,
		C: m.A*n.C + m.C*n.D,
		D: m.B*n.C + m.D*n.D,
		E: m.A*n.E + m.C*n.F + m.E,
		F: m.B*n.E + m.D*n.F + m.F,
	}
}

// translate/scale/rotate mirror the method-chain convention of
// rasterx.Matrix2D: the method's transform is applied before the matrix that
// is already stored in the receiver.
func (m refAffine) translate(x, y float64) refAffine {
	return m.compose(refAffine{1, 0, 0, 1, x, y})
}
func (m refAffine) scale(x, y float64) refAffine {
	return m.compose(refAffine{A: x, D: y})
}
func (m refAffine) rotate(theta float64) refAffine {
	c, s := math.Cos(theta), math.Sin(theta)
	return m.compose(refAffine{A: c, B: s, C: -s, D: c})
}
func (m refAffine) about(cx, cy float64, inner refAffine) refAffine {
	return refIdentity().translate(cx, cy).compose(inner).compose(refIdentity().translate(-cx, -cy))
}

func (m refAffine) invert() refAffine {
	det := m.A*m.D - m.B*m.C
	return refAffine{
		A: m.D / det,
		B: -m.B / det,
		C: -m.C / det,
		D: m.A / det,
		E: (m.C*m.F - m.D*m.E) / det,
		F: (m.B*m.E - m.A*m.F) / det,
	}
}

func (m refAffine) transformPoly(p refPoly) refPoly {
	q := make(refPoly, len(p))
	for i := range p {
		q[i] = m.apply(p[i])
	}
	return q
}

func refPolyFromPairs(pairs [][2]float64) refPoly {
	p := make(refPoly, len(pairs))
	for i, q := range pairs {
		p[i] = refPoint{q[0], q[1]}
	}
	return p
}

func refPolySignedArea(p refPoly) float64 {
	a := 0.0
	for i := range p {
		j := (i + 1) % len(p)
		a += p[i].X*p[j].Y - p[j].X*p[i].Y
	}
	return a / 2
}

func refPolyCCW(p refPoly) refPoly {
	if refPolySignedArea(p) >= 0 {
		return p
	}
	q := make(refPoly, len(p))
	for i := range p {
		q[len(p)-1-i] = p[i]
	}
	return q
}

// refPolyCentroid returns the area centroid of a simple polygon. It is used
// to check where ink landed independently of the rasterizer.
func refPolyCentroid(p refPoly) refPoint {
	var a, cx, cy float64
	for i := range p {
		j := (i + 1) % len(p)
		cross := p[i].X*p[j].Y - p[j].X*p[i].Y
		a += cross
		cx += (p[i].X + p[j].X) * cross
		cy += (p[i].Y + p[j].Y) * cross
	}
	a *= 0.5
	if a == 0 {
		return refPoint{}
	}
	return refPoint{cx / (6 * a), cy / (6 * a)}
}

func refPolyReverse(p refPoly) refPoly {
	q := make(refPoly, len(p))
	for i := range p {
		q[len(p)-1-i] = p[i]
	}
	return q
}

// ---------------------------------------------------------------------------
// Coverage oracle
// ---------------------------------------------------------------------------

// deformOracle holds the device-space geometry of the expected result.
//
// For filled paths, orientation is preserved so the oracle's nonzero winding
// rule reproduces hole cancellation. For stroked/dashed paths each piece is
// stored counter-clockwise and the union is the nonzero winding union.
type deformOracle struct {
	W, H      int
	fillPolys []refPoly
	edgePolys []refPoly
}

func (o *deformOracle) addFill(p refPoly) {
	o.fillPolys = append(o.fillPolys, p)
	o.edgePolys = append(o.edgePolys, p)
}

func (o *deformOracle) addUnion(p refPoly) {
	p = refPolyCCW(p)
	o.fillPolys = append(o.fillPolys, p)
	o.edgePolys = append(o.edgePolys, p)
}

func (o *deformOracle) windingAt(x, y float64) int {
	wind := 0
	for _, p := range o.fillPolys {
		for i := range p {
			a, b := p[i], p[(i+1)%len(p)]
			cross := (b.X-a.X)*(y-a.Y) - (x-a.X)*(b.Y-a.Y)
			if a.Y <= y {
				if b.Y > y && cross > 0 {
					wind++
				}
			} else if b.Y <= y && cross < 0 {
				wind--
			}
		}
	}
	return wind
}

// coverage returns per-pixel area coverage estimated with an ss x ss
// regular sub-pixel grid.
func (o *deformOracle) coverage(ss int) [][]float64 {
	cov := make([][]float64, o.H)
	for y := 0; y < o.H; y++ {
		cov[y] = make([]float64, o.W)
		for x := 0; x < o.W; x++ {
			in := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/float64(ss)
					py := float64(y) + (float64(sy)+0.5)/float64(ss)
					if o.windingAt(px, py) != 0 {
						in++
					}
				}
			}
			cov[y][x] = float64(in) / float64(ss*ss)
		}
	}
	return cov
}

// edgeDistance is the distance from a pixel centre to the nearest expected
// outline edge. It decides whether a pixel gets the edge tolerance or the
// much tighter flat-region tolerance.
func (o *deformOracle) edgeDistance(x, y int) float64 {
	cx, cy := float64(x)+0.5, float64(y)+0.5
	best := math.Inf(1)
	for _, p := range o.edgePolys {
		for i := range p {
			a, b := p[i], p[(i+1)%len(p)]
			dx, dy := b.X-a.X, b.Y-a.Y
			l2 := dx*dx + dy*dy
			t := 0.0
			if l2 > 0 {
				t = ((cx-a.X)*dx + (cy-a.Y)*dy) / l2
				if t < 0 {
					t = 0
				} else if t > 1 {
					t = 1
				}
			}
			ex, ey := a.X+t*dx-cx, a.Y+t*dy-cy
			if d := math.Sqrt(ex*ex + ey*ey); d < best {
				best = d
			}
		}
	}
	return best
}

func deformCoverageMass(cov [][]float64) float64 {
	s := 0.0
	for y := range cov {
		for x := range cov[y] {
			s += cov[y][x]
		}
	}
	return s
}

// deformCoverageDigest hashes the coverage raster as quantised uint16 samples
// so fixture reproducibility does not depend on float formatting.
func deformCoverageDigest(cov [][]float64) string {
	h := sha256.New()
	var buf [2]byte
	for y := range cov {
		for x := range cov[y] {
			v := uint16(math.Round(cov[y][x] * 0xffff))
			binary.LittleEndian.PutUint16(buf[:], v)
			h.Write(buf[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Measuring rendered images
// ---------------------------------------------------------------------------

func deformAlpha(img *image.RGBA) [][]float64 {
	b := img.Bounds()
	cov := make([][]float64, b.Dy())
	for y := 0; y < b.Dy(); y++ {
		cov[y] = make([]float64, b.Dx())
		for x := 0; x < b.Dx(); x++ {
			cov[y][x] = float64(img.Pix[y*img.Stride+x*4+3]) / 255
		}
	}
	return cov
}

func deformMass(img *image.RGBA) float64 {
	s := 0.0
	for i := 3; i < len(img.Pix); i += 4 {
		s += float64(img.Pix[i])
	}
	return s / 255
}

func deformBBox(img *image.RGBA) (minX, minY, maxX, maxY int) {
	minX, minY, maxX, maxY = math.MaxInt32, math.MaxInt32, -1, -1
	b := img.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			if img.Pix[y*img.Stride+x*4+3] == 0 {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	return
}

func deformMoments(img *image.RGBA) (mass, cx, cy, varX, varY float64) {
	b := img.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			a := float64(img.Pix[y*img.Stride+x*4+3]) / 255
			mass += a
			cx += a * (float64(x) + 0.5)
			cy += a * (float64(y) + 0.5)
		}
	}
	if mass == 0 {
		return
	}
	cx /= mass
	cy /= mass
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			a := float64(img.Pix[y*img.Stride+x*4+3]) / 255
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			varX += a * dx * dx
			varY += a * dy * dy
		}
	}
	varX /= mass
	varY /= mass
	return
}

// deformPixelDiff returns (differing bytes, maximum absolute byte delta).
func deformPixelDiff(a, b *image.RGBA) (int, int) {
	if len(a.Pix) != len(b.Pix) {
		return len(a.Pix) + len(b.Pix), 255
	}
	n, max := 0, 0
	for i := range a.Pix {
		d := int(a.Pix[i]) - int(b.Pix[i])
		if d < 0 {
			d = -d
		}
		if d > 0 {
			n++
		}
		if d > max {
			max = d
		}
	}
	return n, max
}

// deformShiftDiff compares img (reference, unshifted) with a render of the
// same geometry translated by (dx,dy). Only the overlap is inspected.
func deformShiftDiff(a, b *image.RGBA, dx, dy int) (int, int) {
	ab, bb := a.Bounds(), b.Bounds()
	n, max := 0, 0
	for y := ab.Min.Y; y < ab.Max.Y-dy; y++ {
		for x := ab.Min.X; x < ab.Max.X-dx; x++ {
			va := a.Pix[(y-ab.Min.Y)*a.Stride+(x-ab.Min.X)*4+3]
			vb := b.Pix[(y+dy-bb.Min.Y)*b.Stride+(x+dx-bb.Min.X)*4+3]
			d := int(va) - int(vb)
			if d < 0 {
				d = -d
			}
			if d > 0 {
				n++
			}
			if d > max {
				max = d
			}
		}
	}
	return n, max
}

// ---------------------------------------------------------------------------
// Comparison with coverage and edge tolerances
// ---------------------------------------------------------------------------

type deformTolerance struct {
	EdgeBand float64 // distance from an outline edge that gets the edge tolerance
	EdgeTol  float64 // max coverage error inside the edge band
	FlatTol  float64 // max coverage error away from all edges
	MinIoU   float64 // minimum intersection-over-union at >50% coverage
}

type deformMetrics struct {
	MeanErr, MaxEdgeErr, MaxFlatErr float64
	IoU                             float64
	MassRef, MassAct                float64
	Bad, BadEdge, BadFlat           int
	FirstBad                        string
}

func deformCompare(or *deformOracle, ref [][]float64, img *image.RGBA, tol deformTolerance) deformMetrics {
	act := deformAlpha(img)
	var r deformMetrics
	sumErr := 0.0
	inter, union := 0, 0
	n := 0
	for y := range ref {
		for x := range ref[y] {
			d := math.Abs(ref[y][x] - act[y][x])
			edge := or.edgeDistance(x, y) <= tol.EdgeBand
			limit := tol.FlatTol
			if edge {
				limit = tol.EdgeTol
				if d > r.MaxEdgeErr {
					r.MaxEdgeErr = d
				}
			} else if d > r.MaxFlatErr {
				r.MaxFlatErr = d
			}
			if d > limit {
				r.Bad++
				if edge {
					r.BadEdge++
				} else {
					r.BadFlat++
				}
				if r.FirstBad == "" {
					r.FirstBad = fmt.Sprintf("pixel (%d,%d) ref=%.3f actual=%.3f delta=%.3f tolerance=%.3f edge=%.2f",
						x, y, ref[y][x], act[y][x], d, limit, or.edgeDistance(x, y))
				}
			}
			sumErr += d
			n++
			// Soft IoU on coverage, so sub-pixel and tiny shapes still
			// produce a meaningful number instead of 0/0.
			if ref[y][x] < act[y][x] {
				inter += int(math.Round(ref[y][x] * 1e6))
				union += int(math.Round(act[y][x] * 1e6))
			} else {
				inter += int(math.Round(act[y][x] * 1e6))
				union += int(math.Round(ref[y][x] * 1e6))
			}
		}
	}
	r.MeanErr = sumErr / float64(n)
	if union > 0 {
		r.IoU = float64(inter) / float64(union)
	}
	r.MassRef = deformCoverageMass(ref)
	r.MassAct = deformMass(img)
	return r
}

func (r deformMetrics) String() string {
	return fmt.Sprintf("meanErr=%.5f edgeMax=%.3f flatMax=%.3f IoU=%.5f massRef=%.2f massAct=%.2f bad=%d (edge=%d flat=%d)",
		r.MeanErr, r.MaxEdgeErr, r.MaxFlatErr, r.IoU, r.MassRef, r.MassAct, r.Bad, r.BadEdge, r.BadFlat)
}

func (r deformMetrics) check(t interface {
	Helper()
	Errorf(string, ...interface{})
}, name string) {
	t.Helper()
	if r.Bad != 0 {
		t.Errorf("%s: %d pixels outside tolerance, first: %s", name, r.Bad, r.FirstBad)
	}
}

// ---------------------------------------------------------------------------
// Independent stroker outline
// ---------------------------------------------------------------------------

func refUnit(a, b refPoint) refPoint {
	dx, dy := b.X-a.X, b.Y-a.Y
	l := math.Hypot(dx, dy)
	if l == 0 {
		return refPoint{}
	}
	return refPoint{dx / l, dy / l}
}

func refNormal(d refPoint) refPoint { return refPoint{-d.Y, d.X} }

// deformStrokeOutline builds the expected device-space stroke as a union of
// strip, cap and join polygons. join is "bevel", "miter" or "round"; capName
// is "", "square" or "round" ("" is a butt cap).
func deformStrokeOutline(pts refPoly, width float64, join, capName string, closed bool) []refPoly {
	u := width / 2
	var out []refPoly
	n := len(pts)

	strip := func(a, b refPoint) {
		d := refUnit(a, b)
		if d == (refPoint{}) {
			return
		}
		nrm := refNormal(d)
		out = append(out, refPoly{
			{a.X + u*nrm.X, a.Y + u*nrm.Y},
			{b.X + u*nrm.X, b.Y + u*nrm.Y},
			{b.X - u*nrm.X, b.Y - u*nrm.Y},
			{a.X - u*nrm.X, a.Y - u*nrm.Y},
		})
	}

	segments := n - 1
	if closed {
		segments = n
	}
	for i := 0; i < segments; i++ {
		strip(pts[i], pts[(i+1)%n])
	}

	// Round caps.
	if !closed && capName == "round" && n > 1 {
		disc := func(c, outward refPoint) {
			ang := math.Atan2(outward.Y, outward.X)
			p := refPoly{c}
			for k := 0; k <= 24; k++ {
				a := ang - math.Pi/2 + math.Pi*float64(k)/24
				p = append(p, refPoint{c.X + u*math.Cos(a), c.Y + u*math.Sin(a)})
			}
			out = append(out, p)
		}
		disc(pts[0], refPoint{-refUnit(pts[0], pts[1]).X, -refUnit(pts[0], pts[1]).Y})
		last := n - 1
		d := refUnit(pts[last-1], pts[last])
		disc(pts[last], d)
	}

	// Square caps: extend each end by u along the tangent.
	if !closed && capName == "square" && n > 1 {
		for _, e := range []struct {
			p, d refPoint
			sign float64
		}{
			{pts[0], refUnit(pts[0], pts[1]), -1},
			{pts[n-1], refUnit(pts[n-2], pts[n-1]), 1},
		} {
			nrm := refNormal(e.d)
			q := refPoint{e.p.X + e.sign*u*e.d.X, e.p.Y + e.sign*u*e.d.Y}
			out = append(out, refPoly{
				{e.p.X + u*nrm.X, e.p.Y + u*nrm.Y},
				{q.X + u*nrm.X, q.Y + u*nrm.Y},
				{q.X - u*nrm.X, q.Y - u*nrm.Y},
				{e.p.X - u*nrm.X, e.p.Y - u*nrm.Y},
			})
		}
	}

	// Joins.
	joinAt := func(i int) {
		prev := (i - 1 + n) % n
		next := (i + 1) % n
		d1 := refUnit(pts[prev], pts[i])
		d2 := refUnit(pts[i], pts[next])
		if d1 == (refPoint{}) || d2 == (refPoint{}) {
			return
		}
		n1, n2 := refNormal(d1), refNormal(d2)
		cross := d1.X*d2.Y - d1.Y*d2.X
		if math.Abs(cross) < 1e-9 {
			return
		}
		s := 1.0
		if cross > 0 {
			s = -1
		}
		o1 := refPoint{pts[i].X + s*u*n1.X, pts[i].Y + s*u*n1.Y}
		o2 := refPoint{pts[i].X + s*u*n2.X, pts[i].Y + s*u*n2.Y}
		switch join {
		case "bevel":
			out = append(out, refPoly{pts[i], o1, o2})
		case "miter":
			den := d1.X*d2.Y - d1.Y*d2.X
			t := ((o2.X-o1.X)*d2.Y - (o2.Y-o1.Y)*d2.X) / den
			mp := refPoint{o1.X + t*d1.X, o1.Y + t*d1.Y}
			out = append(out, refPoly{pts[i], o1, mp, o2})
		case "round":
			a0 := math.Atan2(o1.Y-pts[i].Y, o1.X-pts[i].X)
			a1 := math.Atan2(o2.Y-pts[i].Y, o2.X-pts[i].X)
			if a1 < a0 {
				a1 += 2 * math.Pi
			}
			if a1-a0 > math.Pi {
				a1 -= 2 * math.Pi
			}
			p := refPoly{pts[i]}
			for k := 0; k <= 24; k++ {
				a := a0 + (a1-a0)*float64(k)/24
				p = append(p, refPoint{pts[i].X + u*math.Cos(a), pts[i].Y + u*math.Sin(a)})
			}
			out = append(out, p)
		}
	}
	if closed {
		for i := 0; i < n; i++ {
			joinAt(i)
		}
	} else {
		for i := 1; i < n-1; i++ {
			joinAt(i)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Independent dash walker and capsule outline (single polyline, no vertex
// splits inside a dash; used only for open paths whose dash runs may start or
// end at a vertex, which rasterx handles with plain butt geometry there).
// ---------------------------------------------------------------------------

type refSegment struct{ A, B refPoint }

func deformDashRanges(pts refPoly, pattern []float64, offset float64) []refSegment {
	if len(pattern) == 0 {
		var out []refSegment
		for i := 0; i+1 < len(pts); i++ {
			out = append(out, refSegment{pts[i], pts[i+1]})
		}
		return out
	}
	period := 0.0
	for _, d := range pattern {
		period += d
	}
	phase := math.Mod(offset, period)
	if phase < 0 {
		phase += period
	}
	idx := 0
	for phase > pattern[idx] {
		phase -= pattern[idx]
		idx = (idx + 1) % len(pattern)
	}
	var out []refSegment
	for i := 0; i+1 < len(pts); i++ {
		dx, dy := pts[i+1].X-pts[i].X, pts[i+1].Y-pts[i].Y
		seg := math.Hypot(dx, dy)
		if seg == 0 {
			continue
		}
		d := refPoint{dx / seg, dy / seg}
		cur := 0.0
		for cur < seg-1e-9 {
			remaining := pattern[idx] - phase
			next := cur + remaining
			if next > seg {
				if idx%2 == 0 {
					a := refPoint{pts[i].X + d.X*cur, pts[i].Y + d.Y*cur}
					b := pts[i+1]
					if math.Hypot(b.X-a.X, b.Y-a.Y) > 1e-9 {
						out = append(out, refSegment{a, b})
					}
				}
				phase += seg - cur
				cur = seg
				break
			}
			if idx%2 == 0 {
				a := refPoint{pts[i].X + d.X*cur, pts[i].Y + d.Y*cur}
				b := refPoint{pts[i].X + d.X*next, pts[i].Y + d.Y*next}
				out = append(out, refSegment{a, b})
			}
			cur = next
			phase = 0
			idx = (idx + 1) % len(pattern)
		}
	}
	return out
}

// deformDashCapsule builds one dash as a capsule in device space.
func deformDashCapsule(seg refSegment, width float64, capName string) []refPoly {
	u := width / 2
	d := refUnit(seg.A, seg.B)
	if d == (refPoint{}) {
		return nil
	}
	nrm := refNormal(d)
	ext := 0.0
	if capName == "square" {
		ext = u
	}
	a := refPoint{seg.A.X - ext*d.X, seg.A.Y - ext*d.Y}
	b := refPoint{seg.B.X + ext*d.X, seg.B.Y + ext*d.Y}
	out := []refPoly{{
		{a.X + u*nrm.X, a.Y + u*nrm.Y},
		{b.X + u*nrm.X, b.Y + u*nrm.Y},
		{b.X - u*nrm.X, b.Y - u*nrm.Y},
		{a.X - u*nrm.X, a.Y - u*nrm.Y},
	}}
	if capName == "round" {
		for _, e := range []struct {
			c       refPoint
			outward refPoint
		}{
			{seg.A, refPoint{-d.X, -d.Y}},
			{seg.B, d},
		} {
			ang := math.Atan2(e.outward.Y, e.outward.X)
			p := refPoly{e.c}
			for k := 0; k <= 24; k++ {
				t := ang - math.Pi/2 + math.Pi*float64(k)/24
				p = append(p, refPoint{e.c.X + u*math.Cos(t), e.c.Y + u*math.Sin(t)})
			}
			out = append(out, p)
		}
	}
	return out
}
