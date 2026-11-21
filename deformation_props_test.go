// Copyright 2026 The rasterx Authors. All rights reserved.
//
// Property assertions on the transform/stroke relationship. These are
// algebraic identities that must hold for any correct rasterizer, so they do
// not need a golden image and they fail loudly when composition order,
// coverage, or cap/join geometry is wrong.
package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
)

// ---------------------------------------------------------------------------
// Matrix composition laws
// ---------------------------------------------------------------------------

func TestMatrixCompositionOrder(t *testing.T) {
	// Matrix2D methods map to SVG's "new transform on the right": chained
	// calls apply the newest operation first. This is what MatrixAdder and
	// AddRect/AddEllipse rely on, so assert it explicitly against an
	// independent reference.
	m := Identity.Translate(10, 20).Scale(2, 3).Rotate(math.Pi / 2)
	r := refIdentity().translate(10, 20).scale(2, 3).rotate(math.Pi / 2)
	for _, p := range [][2]float64{{0, 0}, {1, 0}, {0, 1}, {5, -3}, {-2.5, 7.25}} {
		gx, gy := m.Transform(p[0], p[1])
		want := r.apply(refPoint{p[0], p[1]})
		if math.Abs(gx-want.X) > 1e-9 || math.Abs(gy-want.Y) > 1e-9 {
			t.Fatalf("Transform(%v) = (%.9f,%.9f), want (%.9f,%.9f)", p, gx, gy, want.X, want.Y)
		}
	}

	// Changing the order of multiplication must change the point mapping. If
	// Transform/Mult silently commute (e.g. by dropping one operand), this is
	// the check that notices.
	a := Identity.Translate(10, 0).Scale(2, 2)
	b := Identity.Scale(2, 2).Translate(10, 0)
	ax, ay := a.Transform(0, 0)
	bx, by := b.Transform(0, 0)
	if ax == bx && ay == by {
		t.Fatalf("Translate(10).Scale(2) and Scale(2).Translate(10) map the origin identically (%v,%v); composition order was lost", ax, ay)
	}
	if want := (refPoint{10, 0}); ax != want.X || ay != want.Y {
		t.Fatalf("Translate(10).Scale(2) maps origin to (%v,%v), want %v", ax, ay, want)
	}
	if want := (refPoint{20, 0}); bx != want.X || by != want.Y {
		t.Fatalf("Scale(2).Translate(10) maps origin to (%v,%v), want %v", bx, by, want)
	}

	// Rotation must be a rotation: preserve length and turn +x towards +y.
	rot := Identity.Rotate(0.37)
	px, py := rot.Transform(3, 4)
	if math.Abs(math.Hypot(px, py)-5) > 1e-9 {
		t.Fatalf("Rotate does not preserve length: |(3,4)| -> %.9f", math.Hypot(px, py))
	}
	qx, qy := rot.Transform(1, 0)
	if !(qx > 0 && qy > 0) {
		t.Fatalf("positive-angle Rotate sends +x to (%.3f,%.3f); expected counter-clockwise in this matrix convention", qx, qy)
	}

	// Invert must undo Mult in either direction.
	for _, m2 := range []Matrix2D{
		Identity.Translate(4, -9).Rotate(0.8).Scale(1.7, 0.6),
		Identity.SkewX(0.3).Scale(-2, 1.5),
	} {
		inv := m2.Invert()
		for _, p := range [][2]float64{{0, 0}, {3.5, -1.25}, {-8, 6}} {
			x, y := m2.Transform(p[0], p[1])
			rx, ry := inv.Transform(x, y)
			if math.Abs(rx-p[0]) > 1e-7 || math.Abs(ry-p[1]) > 1e-7 {
				t.Fatalf("Invert did not round-trip %v through %+v: got (%.7f,%.7f)", p, m2, rx, ry)
			}
		}
		prod := m2.Mult(inv)
		if math.Abs(prod.A-1) > 1e-9 || math.Abs(prod.D-1) > 1e-9 ||
			math.Abs(prod.B) > 1e-9 || math.Abs(prod.C) > 1e-9 ||
			math.Abs(prod.E) > 1e-7 || math.Abs(prod.F) > 1e-7 {
			t.Fatalf("Mult(Invert) = %+v, want identity", prod)
		}
	}
}

// TestMatrixAdderChainsLikeComposedMatrix asserts that nesting two
// MatrixAdders is the same as a single adder with the composed matrix. This is
// the regression test for "the matrix order is wrong but a single golden PNG
// still looks plausible".
func TestMatrixAdderChainsLikeComposedMatrix(t *testing.T) {
	const w, h = 120, 120
	poly := refPoly{{20, 30}, {95, 22}, {88, 90}, {36, 70}}

	mOuter := Identity.Translate(7, 5)
	mInner := Identity.Rotate(0.31).Scale(1.1, 0.9)
	mCombined := mInner.Mult(mOuter) // mInner applied to the result of mOuter

	renderFill := func(build func(*Filler)) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, sc)
		sc.SetColor(color.White)
		build(f)
		f.Draw()
		return img
	}
	emit := func(a Adder) {
		a.Start(ToFixedP(poly[0].X, poly[0].Y))
		for _, p := range poly[1:] {
			a.Line(ToFixedP(p.X, p.Y))
		}
		a.Stop(true)
	}

	chained := renderFill(func(f *Filler) {
		emit(&MatrixAdder{Adder: &MatrixAdder{Adder: f, M: mInner}, M: mOuter})
	})
	combined := renderFill(func(f *Filler) {
		emit(&MatrixAdder{Adder: f, M: mCombined})
	})

	n, max := deformPixelDiff(chained, combined)
	if n != 0 {
		t.Fatalf("nested MatrixAdder differs from composed matrix in %d bytes (max %d)", n, max)
	}

	// And the composed matrix must actually move ink somewhere else, so this
	// cannot pass by all three renders being blank.
	if deformMass(chained) == 0 {
		t.Fatal("render is empty; matrix-chain test is vacuous")
	}
	// Cross-check the expected corner location through the independent affine
	// reference: the composed matrix must map the path bbox centre where the
	// chained renderers put the ink centroid.
	_, cx, cy, _, _ := deformMoments(chained)
	om := refAffine{mOuter.A, mOuter.B, mOuter.C, mOuter.D, mOuter.E, mOuter.F}
	im := refAffine{mInner.A, mInner.B, mInner.C, mInner.D, mInner.E, mInner.F}
	moved := make(refPoly, len(poly))
	for i, p := range poly {
		moved[i] = im.apply(om.apply(p))
	}
	want := refPolyCentroid(moved)
	if math.Hypot(cx-want.X, cy-want.Y) > 1.5 {
		t.Errorf("ink centroid (%.2f,%.2f) is far from the reference-transformed path centroid (%.2f,%.2f); matrix order is wrong",
			cx, cy, want.X, want.Y)
	}
}

// ---------------------------------------------------------------------------
// Translation equivariance
// ---------------------------------------------------------------------------

// TestTranslationEquivariance asserts that moving the geometry by an integer
// vector moves the rendered coverage exactly, in both directions. A
// composition-order bug that scales the translation, or a bounds bug that
// ignores the destination origin, shows up here immediately.
func TestTranslationEquivariance(t *testing.T) {
	type renderer func(dx, dy float64, viaMatrix bool) *image.RGBA

	cases := []struct {
		name   string
		w, h   int
		shift  [2]int
		render renderer
	}{
		{
			name:  "fill",
			w:     96,
			h:     96,
			shift: [2]int{9, 7},
			render: func(dx, dy float64, viaMatrix bool) *image.RGBA {
				img := image.NewRGBA(image.Rect(0, 0, 96, 96))
				sc := NewScannerGV(96, 96, img, img.Bounds())
				f := NewFiller(96, 96, sc)
				sc.SetColor(color.White)
				poly := refPoly{{14.3, 18.7}, {58.9, 12.1}, {61.7, 47.3}, {37.1, 62.9}, {15.9, 44.2}}
				add := func(a Adder) {
					a.Start(ToFixedP(poly[0].X+dx, poly[0].Y+dy))
					for _, p := range poly[1:] {
						a.Line(ToFixedP(p.X+dx, p.Y+dy))
					}
					a.Stop(true)
				}
				if viaMatrix {
					add(&MatrixAdder{Adder: f, M: Identity.Translate(dx, dy)})
					// Draw the local coordinates through the matrix, not the
					// pre-shifted ones.
					f.Clear()
					add = func(a Adder) {
						a.Start(ToFixedP(poly[0].X, poly[0].Y))
						for _, p := range poly[1:] {
							a.Line(ToFixedP(p.X, p.Y))
						}
						a.Stop(true)
					}
					add(&MatrixAdder{Adder: f, M: Identity.Translate(dx, dy)})
				} else {
					add(f)
				}
				f.Draw()
				return img
			},
		},
		{
			name:  "stroke_bevel",
			w:     150,
			h:     120,
			shift: [2]int{9, 7},
			render: func(dx, dy float64, viaMatrix bool) *image.RGBA {
				img := image.NewRGBA(image.Rect(0, 0, 150, 120))
				sc := NewScannerGV(150, 120, img, img.Bounds())
				s := NewStroker(150, 120, sc)
				s.SetStroke(7*64, 4*64, ButtCap, nil, FlatGap, Bevel)
				sc.SetColor(color.White)
				pts := refPoly{{18.25, 82.5}, {48.75, 30.25}, {92.5, 78.75}, {128.5, 34.5}}
				m := Identity
				if viaMatrix {
					m = Identity.Translate(dx, dy)
					dx, dy = 0, 0
				}
				ma := &MatrixAdder{Adder: s, M: m}
				ma.Start(ToFixedP(pts[0].X+dx, pts[0].Y+dy))
				for _, p := range pts[1:] {
					ma.Line(ToFixedP(p.X+dx, p.Y+dy))
				}
				ma.Stop(false)
				s.Draw()
				return img
			},
		},
		{
			name:  "dash_round",
			w:     150,
			h:     120,
			shift: [2]int{9, 7},
			render: func(dx, dy float64, viaMatrix bool) *image.RGBA {
				img := image.NewRGBA(image.Rect(0, 0, 150, 120))
				sc := NewScannerGV(150, 120, img, img.Bounds())
				d := NewDasher(150, 120, sc)
				d.SetStroke(7*64, 4*64, RoundCap, nil, RoundGap, Round, []float64{9, 5}, 2)
				sc.SetColor(color.White)
				pts := refPoly{{18.25, 82.5}, {48.75, 30.25}, {92.5, 78.75}, {128.5, 34.5}}
				m := Identity
				if viaMatrix {
					m = Identity.Translate(dx, dy)
					dx, dy = 0, 0
				}
				ma := &MatrixAdder{Adder: d, M: m}
				ma.Start(ToFixedP(pts[0].X+dx, pts[0].Y+dy))
				for _, p := range pts[1:] {
					ma.Line(ToFixedP(p.X+dx, p.Y+dy))
				}
				ma.Stop(false)
				d.Draw()
				return img
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			base := c.render(0, 0, false)
			if deformMass(base) == 0 {
				t.Fatal("base render is empty")
			}
			for _, viaMatrix := range []bool{false, true} {
				shifted := c.render(float64(c.shift[0]), float64(c.shift[1]), viaMatrix)
				n, max := deformShiftDiff(base, shifted, c.shift[0], c.shift[1])
				if n != 0 {
					t.Fatalf("viaMatrix=%v: %d bytes of the overlap changed under translation by %v (max delta %d)",
						viaMatrix, n, c.shift, max)
				}
				m0, _, _, _, _ := deformMoments(base)
				m1, _, _, _, _ := deformMoments(shifted)
				if math.Abs(m1-m0) > 1e-9 {
					t.Fatalf("viaMatrix=%v: mass changed from %.9f to %.9f under translation", viaMatrix, m0, m1)
				}
			}
		})
	}
}

// TestSubpixelTranslationEquivariance checks the smoother invariant that
// survives fractional shifts: mass, centroid and second moments move by the
// shift and are otherwise preserved. Raster antialiasing cannot be bit-exact
// under a fractional shift, hence the moment comparison.
func TestSubpixelTranslationEquivariance(t *testing.T) {
	const w, h = 96, 96
	poly := refPoly{{14.3, 18.7}, {58.9, 12.1}, {61.7, 47.3}, {37.1, 62.9}, {15.9, 44.2}}
	render := func(dx, dy float64) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, sc)
		sc.SetColor(color.White)
		ma := &MatrixAdder{Adder: f, M: Identity.Translate(dx, dy)}
		ma.Start(ToFixedP(poly[0].X, poly[0].Y))
		for _, p := range poly[1:] {
			ma.Line(ToFixedP(p.X, p.Y))
		}
		ma.Stop(true)
		f.Draw()
		return img
	}
	m0, cx0, cy0, vx0, vy0 := deformMoments(render(0, 0))
	for _, d := range [][2]float64{{0.37, 0.61}, {-3.4, 2.8}, {0.125, 0.875}} {
		m, cx, cy, vx, vy := deformMoments(render(d[0], d[1]))
		if ratio := m / m0; math.Abs(ratio-1) > 5e-4 {
			t.Errorf("shift %v: coverage mass ratio %.6f deviates from 1", d, ratio)
		}
		if dx := cx - cx0 - d[0]; math.Abs(dx) > 0.03 {
			t.Errorf("shift %v: centroid x moved by %.4f, want %.4f", d, cx-cx0, d[0])
		}
		if dy := cy - cy0 - d[1]; math.Abs(dy) > 0.03 {
			t.Errorf("shift %v: centroid y moved by %.4f, want %.4f", d, cy-cy0, d[1])
		}
		if ratio := vx / vx0; math.Abs(ratio-1) > 2e-3 {
			t.Errorf("shift %v: x variance ratio %.5f deviates from 1", d, ratio)
		}
		if ratio := vy / vy0; math.Abs(ratio-1) > 2e-3 {
			t.Errorf("shift %v: y variance ratio %.5f deviates from 1", d, ratio)
		}
	}
}

// ---------------------------------------------------------------------------
// Closed-path direction
// ---------------------------------------------------------------------------

// TestClosedPathDirection asserts the two direction invariants:
//   - filling is orientation independent for a single non-self-intersecting
//     loop, because nonzero winding flips sign uniformly;
//   - a hole requires opposite orientation: same direction fills the interior,
//     opposite direction leaves it empty.
func TestClosedPathDirection(t *testing.T) {
	const w, h = 100, 100
	outer := refPoly{{10, 10}, {90, 10}, {90, 90}, {10, 90}}
	inner := refPoly{{30, 30}, {70, 30}, {70, 70}, {30, 70}}

	renderLoops := func(loops ...refPoly) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, sc)
		sc.SetColor(color.White)
		for _, p := range loops {
			f.Start(ToFixedP(p[0].X, p[0].Y))
			for _, q := range p[1:] {
				f.Line(ToFixedP(q.X, q.Y))
			}
			f.Stop(true)
		}
		f.Draw()
		return img
	}
	alphaCenter := func(img *image.RGBA) uint8 { return img.Pix[50*img.Stride+50*4+3] }

	ccwOuter := refPolyCCW(outer)
	sameDir := renderLoops(ccwOuter, refPolyCCW(inner))
	oppositeDir := renderLoops(ccwOuter, refPolyReverse(refPolyCCW(inner)))

	if got := alphaCenter(sameDir); got == 0 {
		t.Errorf("nested loops wound in the same direction left the centre empty (alpha=%d); nonzero winding should fill it", got)
	}
	if got := alphaCenter(oppositeDir); got != 0 {
		t.Errorf("nested loops wound in opposite directions filled the centre (alpha=%d); it should be a hole", got)
	}

	// A single loop fills the same interior whichever way it is wound.
	fwd := renderLoops(ccwOuter)
	rev := renderLoops(refPolyReverse(ccwOuter))
	if n, max := deformPixelDiff(fwd, rev); n != 0 {
		t.Errorf("reversing a single closed loop changed %d bytes (max %d); fill must be orientation independent", n, max)
	}
	if a, b := deformMass(fwd), deformMass(rev); math.Abs(a-b) > 1e-9 {
		t.Errorf("single-loop mass depends on direction: %.6f vs %.6f", a, b)
	}

	// The hole must change area by exactly the inner square's area (accounting
	// for the 0.5 pixel offset of the centre sample: the interior measures
	// 40*40 inside edges that sit on integer coordinates).
	if diff := deformMass(sameDir) - deformMass(oppositeDir); math.Abs(diff-1600) > 1 {
		t.Errorf("hole removed %.3f coverage, want 1600", diff)
	}

	// A closed stroke has the same ink for either traversal direction.
	renderStroke := func(p refPoly) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerGV(w, h, img, img.Bounds())
		s := NewStroker(w, h, sc)
		s.SetStroke(9*64, 4*64, ButtCap, nil, FlatGap, Miter)
		sc.SetColor(color.White)
		s.Start(ToFixedP(p[0].X, p[0].Y))
		for _, q := range p[1:] {
			s.Line(ToFixedP(q.X, q.Y))
		}
		s.Stop(true)
		s.Draw()
		return img
	}
	if n, max := deformPixelDiff(renderStroke(outer), renderStroke(refPolyReverse(outer))); n != 0 {
		t.Errorf("closed stroke changed %d bytes (max %d) when traversed in reverse", n, max)
	}
}

// ---------------------------------------------------------------------------
// Cap and join area
// ---------------------------------------------------------------------------

// TestStrokeCapAreas checks cap geometry by area on a segment long enough that
// the body area is exactly width*length and each cap contributes a known
// amount: butt 0, square width^2/2 per end, round pi*(width/2)^2/2 per end.
func TestStrokeCapAreas(t *testing.T) {
	const width = 20.0
	const length = 97.3
	const lengthI = 97 // deliberately non-integer so caps cannot cancel
	u := width / 2
	body := width * length

	render := func(c CapFunc) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 170, 90))
		sc := NewScannerGV(170, 90, img, img.Bounds())
		s := NewStroker(170, 90, sc)
		s.SetStroke(ToFixedP(width, 0).X, 4*64, c, nil, FlatGap, Miter)
		sc.SetColor(color.White)
		s.Start(ToFixedP(35, 45))
		s.Line(ToFixedP(35+length, 45))
		s.Stop(false)
		s.Draw()
		return img
	}

	for _, tc := range []struct {
		name string
		cap  CapFunc
		want float64
		tol  float64
	}{
		{"butt", ButtCap, body, 0.05},
		{"square", SquareCap, body + 2*width*u, 0.05},
		{"round", RoundCap, body + math.Pi*u*u, 1.2},
	} {
		got := deformMass(render(tc.cap))
		if math.Abs(got-tc.want) > tc.tol {
			t.Errorf("%s cap: coverage %.3f, want %.3f (±%.2f)", tc.name, got, tc.want, tc.tol)
		}
	}

	// Leading and trailing caps are independent, so swapping them swaps the
	// area contributed by each end without changing the total.
	oneWay := func(l, tr CapFunc) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 200, 90))
		sc := NewScannerGV(200, 90, img, img.Bounds())
		s := NewStroker(200, 90, sc)
		s.SetStroke(ToFixedP(width, 0).X, 4*64, l, tr, FlatGap, Miter)
		sc.SetColor(color.White)
		s.Start(ToFixedP(50, 45))
		s.Line(ToFixedP(50+length, 45))
		s.Stop(false)
		s.Draw()
		return img
	}
	a := oneWay(ButtCap, RoundCap)
	b := oneWay(RoundCap, ButtCap)
	wantOne := body + math.Pi*u*u/2
	if got := deformMass(a); math.Abs(got-wantOne) > 1.2 {
		t.Errorf("butt+round caps: coverage %.3f, want %.3f", got, wantOne)
	}
	// The same total ink, placed at opposite ends.
	if ma, mb := deformMass(a), deformMass(b); math.Abs(ma-mb) > 0.5 {
		t.Errorf("swapping leading/trailing caps changed total coverage %.3f -> %.3f", ma, mb)
	}
	// The centerline is y=45, so the outermost pixels of a round cap sit near
	// y=45; sample just outside the butt end on the centerline.
	la := a.Pix[45*a.Stride+(50-1)*4+3]
	lb := b.Pix[45*b.Stride+(50-1)*4+3]
	ra := a.Pix[45*a.Stride+(50+lengthI+1)*4+3]
	rb := b.Pix[45*b.Stride+(50+lengthI+1)*4+3]
	if !(la > 0 && lb == 0) {
		t.Errorf("alpha just before the start %d vs %d: expected only (CapL=round) to cover it", la, lb)
	}
	if !(ra == 0 && rb > 0) {
		t.Errorf("alpha just after the end %d vs %d: expected only (CapT=round) to cover it", ra, rb)
	}

	// A zero-length stroke must collapse to just the cap.
	zero := func(c CapFunc) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 80, 80))
		sc := NewScannerGV(80, 80, img, img.Bounds())
		s := NewStroker(80, 80, sc)
		s.SetStroke(ToFixedP(width, 0).X, 4*64, c, nil, FlatGap, Miter)
		sc.SetColor(color.White)
		s.Start(ToFixedP(40, 40))
		s.Line(ToFixedP(40, 40))
		s.Stop(false)
		s.Draw()
		return img
	}
	if got := deformMass(zero(ButtCap)); got != 0 {
		t.Errorf("zero-length butt stroke has coverage %.3f, want 0", got)
	}
	if got := deformMass(zero(SquareCap)); math.Abs(got-4*u*u) > 0.05 {
		t.Errorf("zero-length square stroke has coverage %.3f, want %.3f", got, 4*u*u)
	}
	if got := deformMass(zero(RoundCap)); math.Abs(got-math.Pi*u*u) > 1.2 {
		t.Errorf("zero-length round stroke has coverage %.3f, want %.3f", got, math.Pi*u*u)
	}
}

// TestStrokeJoinAreas checks join geometry by area for a 90 degree turn. With
// half-width u the three joins differ by exactly known amounts:
//
//	miter: +u^2/2 relative to two disjoint segments?  (see below)
//	bevel: -u^2/2
//	round: -u^2 + pi*u^2/4
//
// measured against the sum of the two segment bodies, which overlap in the
// u-by-u square shared by the two strips.
func TestStrokeJoinAreas(t *testing.T) {
	const width = 20.0
	u := width / 2
	l1, l2 := 97.3, 93.7
	// The two strips overlap in a u-by-u square; a right-angle miter fills an
	// equal u-by-u square on the outside, so the miter total is exactly the
	// sum of the two strip lengths times the width.
	base := width * (l1 + l2)

	render := func(jm JoinMode, gp GapFunc, ml float64) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 200, 170))
		sc := NewScannerGV(200, 170, img, img.Bounds())
		s := NewStroker(200, 170, sc)
		s.SetStroke(ToFixedP(width, 0).X, ToFixedP(ml, 0).X, ButtCap, nil, gp, jm)
		sc.SetColor(color.White)
		s.Start(ToFixedP(35, 140))
		s.Line(ToFixedP(35+l1, 140))
		s.Line(ToFixedP(35+l1, 140-l2))
		s.Stop(false)
		s.Draw()
		return img
	}

	for _, tc := range []struct {
		name string
		jm   JoinMode
		gp   GapFunc
		want float64
		tol  float64
	}{
		{"miter", Miter, FlatGap, base, 0.5},
		{"miter-clip", MiterClip, FlatGap, base, 0.5}, // limit high enough not to clip
		{"bevel", Bevel, FlatGap, base - u*u/2, 0.5},
		{"round", Round, RoundGap, base - u*u + math.Pi*u*u/4, 0.5},
		{"arc", Arc, RoundGap, base, 0.5}, // straight segments: arc degenerates to miter
	} {
		got := deformMass(render(tc.jm, tc.gp, 4))
		if math.Abs(got-tc.want) > tc.tol {
			t.Errorf("%s join: coverage %.3f, want %.3f (±%.2f)", tc.name, got, tc.want, tc.tol)
		}
	}

	// Miter clipping must be monotone in the miter limit and stop changing
	// once the limit exceeds the actual miter length (sqrt(2) here).
	prev := 0.0
	for _, ml := range []float64{1.0, 1.1, 1.2, 1.3, 1.5, 2, 4} {
		got := deformMass(render(MiterClip, FlatGap, ml))
		if got+1e-6 < prev {
			t.Errorf("miter-clip coverage fell from %.3f to %.3f when the limit rose to %.1f", prev, got, ml)
		}
		prev = got
	}
	// At limit 1 clipping must remove at least the bevel amount of corner.
	full := deformMass(render(MiterClip, FlatGap, 4))
	clipped := deformMass(render(MiterClip, FlatGap, 1.0))
	if clipped >= full-1 {
		t.Errorf("miter-clip at limit 1 removed only %.3f coverage; a miter limit of 1 must cut the corner", full-clipped)
	}
	// And must never be smaller than a bevel.
	if bevel := deformMass(render(Bevel, FlatGap, 1)); clipped < bevel-0.1 {
		t.Errorf("miter-clip at limit 1 (%.3f) is smaller than a bevel (%.3f)", clipped, bevel)
	}
}
