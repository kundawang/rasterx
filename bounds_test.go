// Copyright 2026 by the rasterx Authors. All rights reserved.
//
// Bounds query tests. The tests use two independent references rather than
// reusing the production code paths:
//
//   - a dense analytical sampler that walks every bezier independently;
//   - the real vector rasterizer's rendered pixels, scanned out of an RGBA
//     image, plus ScannerGV.GetPathExtent and clip intersection.
package rasterx_test

import (
	"errors"
	"image"
	"math"
	"math/rand"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/colornames"
	"golang.org/x/image/math/fixed"
)

const samplesPerCurve = 4000

// ---------------- independent curve/path reference ----------------

type refRect struct {
	minX, minY, maxX, maxY float64
	ok                     bool
}

func (r *refRect) add(x, y float64) {
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

func (r *refRect) grow(d float64) {
	r.minX -= d
	r.minY -= d
	r.maxX += d
	r.maxY += d
}

func qEval(p0, p1, p2 [2]float64, t float64) (x, y float64) {
	mt := 1 - t
	a, b, c := mt*mt, 2*mt*t, t*t
	return a*p0[0] + b*p1[0] + c*p2[0], a*p0[1] + b*p1[1] + c*p2[1]
}

func cEval(p0, p1, p2, p3 [2]float64, t float64) (x, y float64) {
	mt := 1 - t
	a, b, c, d := mt*mt*mt, 3*mt*mt*t, 3*mt*t*t, t*t*t
	return a*p0[0] + b*p1[0] + c*p2[0] + d*p3[0],
		a*p0[1] + b*p1[1] + c*p2[1] + d*p3[1]
}

// referencePathBox independently dense-samples the whole path.
func referencePathBox(t *testing.T, p Path, m Matrix2D) refRect {
	t.Helper()
	var box refRect
	var cur, sub [2]float64
	mapF := func(pt fixed.Point26_6) [2]float64 {
		x, y := m.Transform(float64(pt.X)/64, float64(pt.Y)/64)
		return [2]float64{x, y}
	}
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			cur = mapF(fp26(p[i+1], p[i+2]))
			sub = cur
			box.add(cur[0], cur[1])
			i += 3
		case PathLineTo:
			cur = mapF(fp26(p[i+1], p[i+2]))
			box.add(cur[0], cur[1])
			i += 3
		case PathQuadTo:
			p1 := mapF(fp26(p[i+1], p[i+2]))
			p2 := mapF(fp26(p[i+3], p[i+4]))
			for k := 0; k <= samplesPerCurve; k++ {
				x, y := qEval(cur, p1, p2, float64(k)/samplesPerCurve)
				box.add(x, y)
			}
			cur = p2
			i += 5
		case PathCubicTo:
			p1 := mapF(fp26(p[i+1], p[i+2]))
			p2 := mapF(fp26(p[i+3], p[i+4]))
			p3 := mapF(fp26(p[i+5], p[i+6]))
			for k := 0; k <= samplesPerCurve; k++ {
				x, y := cEval(cur, p1, p2, p3, float64(k)/samplesPerCurve)
				box.add(x, y)
			}
			cur = p3
			i += 7
		case PathClose:
			box.add(sub[0], sub[1])
			cur = sub
			i++
		default:
			t.Fatalf("bad path command %d at %d", p[i], i)
		}
	}
	return box
}

func fp26(x, y fixed.Int26_6) fixed.Point26_6 {
	return fixed.Point26_6{X: x, Y: y}
}

func rectFloats(r fixed.Rectangle26_6) (minX, minY, maxX, maxY float64) {
	return float64(r.Min.X) / 64, float64(r.Min.Y) / 64,
		float64(r.Max.X) / 64, float64(r.Max.Y) / 64
}

// assertContains verifies r fully contains the reference box, and reports
// the slack so tests can also assert tightness.
func assertContains(t *testing.T, name string, r fixed.Rectangle26_6, ref refRect, slack float64) {
	t.Helper()
	minX, minY, maxX, maxY := rectFloats(r)
	if minX > ref.minX+slack || minY > ref.minY+slack ||
		maxX < ref.maxX-slack || maxY < ref.maxY-slack {
		t.Errorf("%s: box [%.4f,%.4f,%.4f,%.4f] misses ref [%.4f,%.4f,%.4f,%.4f]",
			name, minX, minY, maxX, maxY, ref.minX, ref.minY, ref.maxX, ref.maxY)
	}
}

// assertTight verifies the box never expands more than slack past the ref.
func assertTight(t *testing.T, name string, r fixed.Rectangle26_6, ref refRect, slack float64) {
	t.Helper()
	minX, minY, maxX, maxY := rectFloats(r)
	if ref.minX-minX > slack || ref.minY-minY > slack ||
		maxX-ref.maxX > slack || maxY-ref.maxY > slack {
		t.Errorf("%s: box [%.4f,%.4f,%.4f,%.4f] too loose vs ref [%.4f,%.4f,%.4f,%.4f]",
			name, minX, minY, maxX, maxY, ref.minX, ref.minY, ref.maxX, ref.maxY)
	}
}

// assertInside verifies r lies inside the independent geometric envelope ref.
// Unlike assertContains (which requires r to *contain* ref), this checks that
// the query did not expand past a separately derived conservative bound.
func assertInside(t *testing.T, name string, r fixed.Rectangle26_6, ref refRect, slack float64) {
	t.Helper()
	minX, minY, maxX, maxY := rectFloats(r)
	if minX < ref.minX-slack || minY < ref.minY-slack ||
		maxX > ref.maxX+slack || maxY > ref.maxY+slack {
		t.Errorf("%s: box [%.4f,%.4f,%.4f,%.4f] escapes envelope [%.4f,%.4f,%.4f,%.4f]",
			name, minX, minY, maxX, maxY, ref.minX, ref.minY, ref.maxX, ref.maxY)
	}
}

// ---------------- rasterized pixel reference ----------------

// pixelBox renders through the real rasterizer and returns the bounding
// rectangle of touched pixels in pixel coordinates.
func pixelBox(t *testing.T, render func(f *Filler, d *Dasher, img *image.RGBA)) image.Rectangle {
	t.Helper()
	wx, wy := 2000, 2000
	img := image.NewRGBA(image.Rect(0, 0, wx, wy))
	scanner := NewScannerGV(wx, wy, img, img.Bounds())
	scanner.SetColor(colornames.Black)
	d := NewDasher(wx, wy, scanner)
	render(&d.Filler, d, img)
	d.Draw()
	minX, minY, maxX, maxY := wx, wy, 0, 0
	touched := false
	for y := 0; y < wy; y++ {
		for x := 0; x < wx; x++ {
			if c := img.RGBAAt(x, y); c.A != 0 {
				touched = true
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x+1 > maxX {
					maxX = x + 1
				}
				if y+1 > maxY {
					maxY = y + 1
				}
			}
		}
	}
	if !touched {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX, maxY)
}

// assertCoversPixels verifies the PixelCover of r covers every touched
// pixel, allowing the documented outward pixel-rounding margin.
func assertCoversPixels(t *testing.T, name string, r fixed.Rectangle26_6, px image.Rectangle) {
	t.Helper()
	cov := PixelCover(r, nil)
	if !px.In(cov) {
		t.Errorf("%s: pixel cover %v does not contain rendered pixels %v", name, cov, px)
	}
}

// ---------------- fill bounds tests ----------------

func TestFillBoundsCurvesAndNegatives(t *testing.T) {
	p := Path{}
	// Negative coordinates, a quad and an S-shaped cubic with interior extrema.
	p.Start(ToFixedP(-40, -30))
	p.QuadBezier(ToFixedP(60, -120), ToFixedP(100, 20))
	p.CubeBezier(ToFixedP(-80, 80), ToFixedP(180, 160), ToFixedP(-20, 200))
	p.Line(ToFixedP(-100, 50))
	p.Stop(true)

	m := Identity.Translate(-15, 25).Rotate(0.35)
	got, err := FillBounds(p, m)
	if err != nil {
		t.Fatalf("FillBounds: %v", err)
	}
	ref := referencePathBox(t, p, m)
	assertContains(t, "fill curves", got, ref, 1.0/64.0)
	assertTight(t, "fill curves", got, ref, 1.0/64.0)
	if got.Min.X >= 0 || got.Min.Y >= 0 {
		t.Errorf("expected non-zero negative minimum, got %v", got.Min)
	}
}

func TestFillBoundsNonZeroMinAndMatrix(t *testing.T) {
	p := Path{}
	AddRect(100, 100, 200, 160, 0, &p)
	m := Identity.Scale(2, 0.5).Translate(30, -40)
	got, err := FillBounds(p, m)
	if err != nil {
		t.Fatal(err)
	}
	ref := referencePathBox(t, p, m)
	assertContains(t, "rect scaled", got, ref, 1.0/64.0)
	assertTight(t, "rect scaled", got, ref, 1.0/64.0)
	// Matrix2D.Transform applies m to the point, so x' = 2(x+30) in [260,460]
	// and y' = 0.5(y-40) in [30,60]. Outward pixel rounding at the grid edges
	// yields (259,29)-(461,61); the translated minimum is deliberately
	// non-zero, exercising non-zero Bounds.
	want := image.Rect(259, 29, 461, 61)
	if cov := PixelCover(got, nil); cov != want {
		t.Errorf("pixel cover = %v, want %v", cov, want)
	}
}

func TestFillBoundsClip(t *testing.T) {
	p := Path{}
	AddRect(10, 10, 100, 100, 0, &p)
	got, err := FillBounds(p, Identity)
	if err != nil {
		t.Fatal(err)
	}
	clip := image.Rect(40, 40, 80, 80)
	cov := PixelCover(got, &clip)
	if cov != clip {
		t.Errorf("clipped cover = %v, want %v", cov, clip)
	}
	outside := image.Rect(200, 200, 300, 300)
	if cov := PixelCover(got, &outside); !cov.Empty() {
		t.Errorf("fully clipped box should be empty, got %v", cov)
	}
}

func TestFillBoundsRasterizedReference(t *testing.T) {
	p := GetTestPath()
	got, err := FillBounds(p, Identity)
	if err != nil {
		t.Fatal(err)
	}
	px := pixelBox(t, func(f *Filler, _ *Dasher, _ *image.RGBA) {
		p.AddTo(f)
	})
	assertCoversPixels(t, "fill raster", got, px)

	// And compare against the scanner's own GetPathExtent accumulation.
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	scanner := NewScannerGV(512, 512, img, img.Bounds())
	filler := NewFiller(512, 512, scanner)
	p.AddTo(filler)
	ext := scanner.GetPathExtent()
	eminx, eminy, emaxx, emaxy := rectFloats(ext)
	gminx, gminy, gmaxx, gmaxy := rectFloats(got)
	// The query box must contain the scanner box and stay within a pixel.
	if gminx > eminx || gminy > eminy || gmaxx < emaxx || gmaxy < emaxy {
		t.Errorf("bounds %v does not contain scanner extent %v", got, ext)
	}
	if eminx-gminx > 1 || eminy-gminy > 1 || gmaxx-emaxx > 1 || gmaxy-emaxy > 1 {
		t.Errorf("bounds %v diverges >1px from scanner extent %v", got, ext)
	}
}

func TestBoundsAdderStreaming(t *testing.T) {
	p := getPartPath()
	ba, err := NewBoundsAdder(Identity.Translate(7, -9))
	if err != nil {
		t.Fatal(err)
	}
	p.AddTo(ba)
	got, err := ba.Rect()
	if err != nil {
		t.Fatal(err)
	}
	ref := referencePathBox(t, p, Identity.Translate(7, -9))
	assertContains(t, "streaming", got, ref, 1.0/64.0)
	assertTight(t, "streaming", got, ref, 1.0/64.0)

	// Empty adder reports ErrEmptyPath.
	empty, _ := NewBoundsAdder(Identity)
	if _, err := empty.Rect(); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("empty BoundsAdder err = %v, want ErrEmptyPath", err)
	}
}

// ---------------- stroke bounds tests ----------------

func strokePath() Path {
	p := Path{}
	p.Start(ToFixedP(40, 60))
	p.Line(ToFixedP(200, 60))
	p.CubeBezier(ToFixedP(260, 140), ToFixedP(120, 220), ToFixedP(300, 280))
	p.Line(ToFixedP(300, 360))
	return p
}

func TestStrokeBoundsRasterized(t *testing.T) {
	p := strokePath()
	styles := []StrokeStyle{
		{Width: 10, MiterLimit: 4, Cap: CapButt, JoinMode: MiterClip},
		{Width: 12, MiterLimit: 4, Cap: CapRound, JoinMode: Round},
		{Width: 8, MiterLimit: 4, Cap: CapSquare, JoinMode: Bevel},
		{Width: 6, MiterLimit: 2, Cap: CapButt, JoinMode: Miter},
		{Width: 14, MiterLimit: 4, Cap: CapRound, JoinMode: ArcClip},
		{Width: 10, MiterLimit: 4, Cap: CapSquare, JoinMode: MiterClip,
			Dashes: []float64{20, 8}, DashOffset: 0},
		{Width: 10, MiterLimit: 4, Cap: CapRound, JoinMode: Round,
			Dashes: []float64{15, 10, 5, 10}, DashOffset: -17.5},
		{Width: 4, MiterLimit: 4, Cap: CapQuadratic, JoinMode: MiterClip},
	}
	for i, s := range styles {
		got, err := StrokeBounds(p, Identity, s)
		if err != nil {
			t.Fatalf("style %d: %v", i, err)
		}
		style := s
		px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
			d.SetStroke(ToFixed(style.Width), ToFixed(style.MiterLimit),
				capFunc(style.Cap), nil, gapFunc(style.JoinMode), style.JoinMode,
				style.Dashes, style.DashOffset)
			p.AddTo(d)
		})
		assertCoversPixels(t, "stroke raster", got, px)
	}
}

func TestStrokeBoundsTransformed(t *testing.T) {
	p := strokePath()
	m := Identity.Translate(-60, 30).Rotate(0.5).Scale(1.5, 1.5)
	s := StrokeStyle{Width: 12, MiterLimit: 4, Cap: CapRound, JoinMode: ArcClip,
		Dashes: []float64{25, 12}, DashOffset: 7}
	got, err := StrokeBounds(p, m, s)
	if err != nil {
		t.Fatal(err)
	}
	px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
		mp := &MatrixAdder{Adder: d, M: m}
		d.SetStroke(ToFixed(s.Width), ToFixed(s.MiterLimit), RoundCap, nil,
			RoundGap, s.JoinMode, s.Dashes, s.DashOffset)
		p.AddTo(mp)
	})
	assertCoversPixels(t, "stroke transformed", got, px)

	// Independent geometric envelope: the dense-sampled transformed spine
	// (path box) grown by the device-space half-width, the angle-independent
	// miter allowance and rounding slack. Width is applied after the
	// transform, exactly as in the MatrixAdder + SetStroke reference render.
	ref := referencePathBox(t, p, m)
	r := s.Width / 2.0
	envelope := r + r*s.MiterLimit + 2.0/64.0
	ref.grow(envelope)
	assertInside(t, "stroke analytic", got, ref, 3.0/64.0)
}

func TestStrokeBoundsClosedPath(t *testing.T) {
	p := Path{}
	AddCircle(120, 120, 80, &p)
	// Closed paths have no caps; butt vs square must give identical boxes.
	butt, err := StrokeBounds(p, Identity, StrokeStyle{Width: 10, MiterLimit: 4, Cap: CapButt, JoinMode: MiterClip})
	if err != nil {
		t.Fatal(err)
	}
	square, err := StrokeBounds(p, Identity, StrokeStyle{Width: 10, MiterLimit: 4, Cap: CapSquare, JoinMode: MiterClip})
	if err != nil {
		t.Fatal(err)
	}
	if butt != square {
		t.Errorf("closed path cap boxes differ: butt %v square %v", butt, square)
	}
	px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
		d.SetStroke(ToFixed(10), ToFixed(4), ButtCap, nil, FlatGap, MiterClip, nil, 0)
		p.AddTo(d)
	})
	assertCoversPixels(t, "closed circle", butt, px)
	// A circle stroke of width 10 spans radius [75,85] around (120,120).
	minX, minY, maxX, maxY := rectFloats(butt)
	if minX > 35 || minY > 35 || maxX < 205 || maxY < 205 {
		t.Errorf("circle stroke box [%.2f,%.2f,%.2f,%.2f] misses expected [35,35,205,205]",
			minX, minY, maxX, maxY)
	}
}

func TestMiterLimitCap(t *testing.T) {
	// A 90-degree turn: the true miter ratio is csc(45deg) ~= 1.414, so a
	// limit of 1.2 clips the spike while a limit of 1000 keeps it.
	p := Path{}
	p.Start(ToFixedP(50, 250))
	p.Line(ToFixedP(250, 250))
	p.Line(ToFixedP(250, 50))

	high, err := StrokeBounds(p, Identity, StrokeStyle{Width: 10, MiterLimit: 1000, Cap: CapButt, JoinMode: Miter})
	if err != nil {
		t.Fatal(err)
	}
	clipped, err := StrokeBounds(p, Identity, StrokeStyle{Width: 10, MiterLimit: 1.2, Cap: CapButt, JoinMode: MiterClip})
	if err != nil {
		t.Fatal(err)
	}
	// Miter join with a huge limit projects further than the clipped variant.
	areaH := boxArea(high)
	areaC := boxArea(clipped)
	if areaH <= areaC {
		t.Errorf("high miter box area %d should exceed clipped %d", areaH, areaC)
	}
	// Both must still cover their actual rendered pixels.
	pxH := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
		d.SetStroke(ToFixed(10), ToFixed(1000), ButtCap, nil, FlatGap, Miter, nil, 0)
		p.AddTo(d)
	})
	assertCoversPixels(t, "miter high", high, pxH)
	pxC := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
		d.SetStroke(ToFixed(10), ToFixed(1.2), ButtCap, nil, FlatGap, MiterClip, nil, 0)
		p.AddTo(d)
	})
	assertCoversPixels(t, "miter clipped", clipped, pxC)

	// The limit-1.2 box must clip the spike that the limit-1000 box keeps.
	if boxArea(high) <= boxArea(clipped) {
		t.Errorf("unclipped area %d should exceed clipped area %d",
			boxArea(high), boxArea(clipped))
	}
}

// TestAnisotropicStroke verifies that stroke width is interpreted in
// transformed device space under unequal x/y scaling, matching the
// MatrixAdder + SetStroke render path pixel for pixel.
// TestClosedTriangleMiter exercises the two joins generated at a path
// closure (one at the last vertex, one at the sub-path start) against the
// rasterized pixels for clipped and unclipped miter joins.
func TestClosedTriangleMiter(t *testing.T) {
	p := Path{}
	p.Start(ToFixedP(120, 60))
	p.Line(ToFixedP(320, 260))
	p.Line(ToFixedP(60, 240))
	p.Stop(true)

	for _, jm := range []JoinMode{Miter, MiterClip, Bevel, Round, Arc, ArcClip} {
		style := StrokeStyle{Width: 14, MiterLimit: 1.6, Cap: CapButt, JoinMode: jm}
		got, err := StrokeBounds(p, Identity, style)
		if err != nil {
			t.Fatalf("%v: %v", jm, err)
		}
		jm := jm
		px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
			d.SetStroke(ToFixed(14), ToFixed(1.6), ButtCap, nil,
				gapFunc(jm), jm, nil, 0)
			p.AddTo(d)
		})
		assertCoversPixels(t, "closed triangle "+joinName(jm), got, px)
	}
}

func joinName(j JoinMode) string {
	switch j {
	case Arc:
		return "Arc"
	case ArcClip:
		return "ArcClip"
	case Miter:
		return "Miter"
	case MiterClip:
		return "MiterClip"
	case Bevel:
		return "Bevel"
	default:
		return "Round"
	}
}

func TestUnionRectAndStreamingErrors(t *testing.T) {
	a := fixed.Rectangle26_6{
		Min: fp26(fixed.Int26_6(10), fixed.Int26_6(20)),
		Max: fp26(fixed.Int26_6(30), fixed.Int26_6(40))}
	b := fixed.Rectangle26_6{
		Min: fp26(fixed.Int26_6(-5), fixed.Int26_6(25)),
		Max: fp26(fixed.Int26_6(20), fixed.Int26_6(50))}
	u := UnionRect(a, b)
	want := fixed.Rectangle26_6{
		Min: fp26(fixed.Int26_6(-5), fixed.Int26_6(20)),
		Max: fp26(fixed.Int26_6(30), fixed.Int26_6(50))}
	if u != want {
		t.Errorf("UnionRect = %v, want %v", u, want)
	}

	// Segment commands before any Start record an error and stay latched.
	ba, err := NewBoundsAdder(Identity)
	if err != nil {
		t.Fatal(err)
	}
	ba.Line(ToFixedP(1, 1))
	if !errors.Is(ba.Err(), ErrInvalidValue) {
		t.Errorf("line before start err = %v", ba.Err())
	}
	ba2, _ := NewBoundsAdder(Identity)
	ba2.QuadBezier(ToFixedP(1, 1), ToFixedP(2, 2))
	if !errors.Is(ba2.Err(), ErrInvalidValue) {
		t.Errorf("quad before start err = %v", ba2.Err())
	}
	ba3, _ := NewBoundsAdder(Identity)
	ba3.CubeBezier(ToFixedP(1, 1), ToFixedP(2, 2), ToFixedP(3, 3))
	if !errors.Is(ba3.Err(), ErrInvalidValue) {
		t.Errorf("cube before start err = %v", ba3.Err())
	}
	if _, err := NewBoundsAdder(Matrix2D{A: 0, B: 0, C: 0, D: 0}); !errors.Is(err, ErrSingularMatrix) {
		t.Errorf("singular adder err = %v", err)
	}
}

func TestAnisotropicStroke(t *testing.T) {
	p := Path{}
	p.Start(ToFixedP(100, 100))
	p.Line(ToFixedP(300, 100))
	m := Identity.Scale(4, 2)
	s := StrokeStyle{Width: 12, MiterLimit: 4, Cap: CapRound, JoinMode: Round}
	got, err := StrokeBounds(p, m, s)
	if err != nil {
		t.Fatal(err)
	}
	px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
		mp := &MatrixAdder{Adder: d, M: m}
		d.SetStroke(ToFixed(12), ToFixed(4), RoundCap, nil, RoundGap, Round, nil, 0)
		p.AddTo(mp)
	})
	assertCoversPixels(t, "aniso", got, px)
	// Tightness: at most one pixel of outward rounding slack per edge.
	cov := PixelCover(got, nil)
	if cov.Min.X < px.Min.X-1 || cov.Min.Y < px.Min.Y-1 ||
		cov.Max.X > px.Max.X+1 || cov.Max.Y > px.Max.Y+1 {
		t.Errorf("aniso cover %v too loose vs pixels %v", cov, px)
	}
}

func TestSkewAndReflectionMatrix(t *testing.T) {
	p := Path{}
	AddCircle(0, 0, 50, &p) // centered on the origin, spanning negatives
	matrices := map[string]Matrix2D{
		"skew":       Identity.SkewX(0.6),
		"reflection": {A: -1, B: 0, C: 0, D: 1, E: 0, F: 0}, // det = -1
		"uniform":    Identity.Scale(3, 3),
	}
	for name, m := range matrices {
		got, err := FillBounds(p, m)
		if err != nil {
			t.Fatalf("%s fill: %v", name, err)
		}
		ref := referencePathBox(t, p, m)
		assertContains(t, name+" fill", got, ref, 1.0/64.0)
		assertTight(t, name+" fill", got, ref, 1.0/64.0)

		s := StrokeStyle{Width: 10, MiterLimit: 4, Cap: CapRound, JoinMode: Round}
		srect, serr := StrokeBounds(p, m, s)
		if serr != nil {
			t.Fatalf("%s stroke: %v", name, serr)
		}
		// Rendered pixels must be covered after the same transform.
		px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
			mp := &MatrixAdder{Adder: d, M: m}
			d.SetStroke(ToFixed(10), ToFixed(4), RoundCap, nil, RoundGap, Round, nil, 0)
			p.AddTo(mp)
		})
		assertCoversPixels(t, name+" stroke", srect, px)
	}

	// Uniform scale 3 with round joins: there can be no miter spike, so the
	// box must equal the analytic spine box plus half-width and slack. The
	// spine box comes from FillBounds, whose extrema math is independently
	// verified above against dense curve sampling.
	ub, _ := StrokeBounds(p, matrices["uniform"],
		StrokeStyle{Width: 10, MiterLimit: 4, Cap: CapButt, JoinMode: Round})
	spine, err := FillBounds(p, matrices["uniform"])
	if err != nil {
		t.Fatal(err)
	}
	allow := 5.0 + 1.0 // device half-width plus one-pixel slack
	env := refRect{
		minX: float64(spine.Min.X)/64 - allow,
		minY: float64(spine.Min.Y)/64 - allow,
		maxX: float64(spine.Max.X)/64 + allow,
		maxY: float64(spine.Max.Y)/64 + allow,
		ok:   true}
	assertInside(t, "uniform stroke", ub, env, 3.0/64.0)
}

func TestExtremeMiterAndOverflow(t *testing.T) {
	// Near-collinear join with an extreme miter limit: the analytic
	// allowance must be clamped at miterRatioClamp rather than exploding.
	p := Path{}
	p.Start(ToFixedP(20, 200))
	p.Line(ToFixedP(300, 200))
	p.Line(ToFixedP(340, 199.5))
	got, err := StrokeBounds(p, Identity,
		StrokeStyle{Width: 6, MiterLimit: 1e9, Cap: CapButt, JoinMode: Miter})
	if err != nil {
		t.Fatalf("extreme miter: %v", err)
	}
	// The worst allowed excursion is half-width * 32768 = 3*32768 = 98304.
	// Add the 600-ish span plus rounding slack; it must stay finite and
	// well inside the Int26_6 range.
	minX, minY, maxX, maxY := rectFloats(got)
	if math.IsNaN(minX+minY+maxX+maxY) || math.IsInf(minX, 0) {
		t.Fatalf("extreme miter produced non-finite box: %v", got)
	}
	if math.Abs(minY) > 1e5 || math.Abs(maxY) > 1e5 {
		t.Errorf("miter clamp failed, box spans %v", got)
	}

	// A coordinate outside the representable Int26_6 range must report
	// ErrValueOverflow while still returning a clamped conservative box.
	big := Path{}
	big.Start(ToFixedP(0, 0))
	big.Line(ToFixedP(4e7, 0)) // 4e7 px exceeds MaxInt32/64 ~= 3.35e7
	rect, oerr := FillBounds(big, Identity)
	if !errors.Is(oerr, ErrValueOverflow) {
		t.Errorf("overflow fill err = %v, want ErrValueOverflow", oerr)
	}
	if rect.Min.X == 0 && rect.Max.X == 0 {
		t.Errorf("overflow fill returned an empty box")
	}
	srect, serr := StrokeBounds(big, Identity, StrokeStyle{Width: 2, MiterLimit: 4})
	if !errors.Is(serr, ErrValueOverflow) {
		t.Errorf("overflow stroke err = %v, want ErrValueOverflow", serr)
	}
	if srect.Min.X == 0 && srect.Max.X == 0 {
		t.Errorf("overflow stroke returned an empty box")
	}
}

func TestMatrixAdderImmutability(t *testing.T) {
	// A MatrixAdder wrapping a BoundsAdder must behave identically to the
	// direct matrix query, and neither object mutates the input path.
	p := getPartPath()
	before := p.ToSVGPath()
	m := Identity.Translate(50, -60).Rotate(-0.25)
	want, err := FillBounds(p, m)
	if err != nil {
		t.Fatal(err)
	}
	ba, err := NewBoundsAdder(Identity)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &MatrixAdder{Adder: ba, M: m}
	p.AddTo(wrapped)
	got, err := ba.Rect()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("MatrixAdder+BoundsAdder %v != FillBounds %v", got, want)
	}
	if after := p.ToSVGPath(); after != before {
		t.Errorf("query mutated the path")
	}
}

func boxArea(r fixed.Rectangle26_6) int {
	return int(r.Max.X-r.Min.X) * int(r.Max.Y-r.Min.Y)
}

func TestNearZeroCurvature(t *testing.T) {
	// Control points nearly collinear: derivative coefficient under the
	// relative epsilon must not produce NaNs or missed endpoints.
	p := Path{}
	p.Start(ToFixedP(0, 100))
	p.QuadBezier(ToFixedP(50, 100.0000001), ToFixedP(100, 100))
	p.CubeBezier(ToFixedP(150, 99.9999999), ToFixedP(200, 100), ToFixedP(250, 100))
	got, err := FillBounds(p, Identity)
	if err != nil {
		t.Fatal(err)
	}
	ref := referencePathBox(t, p, Identity)
	assertContains(t, "near-flat", got, ref, 1.0/64.0)
	_, err = StrokeBounds(p, Identity, StrokeStyle{Width: 4, MiterLimit: 4, Cap: CapRound, JoinMode: Round})
	if err != nil {
		t.Fatalf("near-flat stroke: %v", err)
	}
}

// ---------------- error and edge cases ----------------

func TestBoundsErrors(t *testing.T) {
	p := Path{}
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(10, 10))

	if _, err := FillBounds(nil, Identity); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("empty fill err = %v", err)
	}
	if _, err := StrokeBounds(nil, Identity, StrokeStyle{Width: 2}); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("empty stroke err = %v", err)
	}
	singular := Matrix2D{A: 1, B: 0, C: 2, D: 0, E: 0, F: 0}
	if _, err := FillBounds(p, singular); !errors.Is(err, ErrSingularMatrix) {
		t.Errorf("singular fill err = %v", err)
	}
	if _, err := StrokeBounds(p, singular, StrokeStyle{Width: 2}); !errors.Is(err, ErrSingularMatrix) {
		t.Errorf("singular stroke err = %v", err)
	}
	nonFinite := Matrix2D{A: 1, B: math.NaN(), C: 0, D: 1, E: 0, F: 0}
	if _, err := FillBounds(p, nonFinite); !errors.Is(err, ErrNonFinite) {
		t.Errorf("non-finite matrix err = %v", err)
	}
	if _, err := StrokeBounds(p, Identity, StrokeStyle{Width: math.Inf(1)}); !errors.Is(err, ErrNonFinite) {
		t.Errorf("non-finite width err = %v", err)
	}
	if _, err := StrokeBounds(p, Identity, StrokeStyle{Width: -1}); !errors.Is(err, ErrInvalidValue) {
		t.Errorf("negative width err = %v", err)
	}
	if _, err := StrokeBounds(p, Identity, StrokeStyle{Width: 2, Dashes: []float64{0}}); !errors.Is(err, ErrInvalidValue) {
		t.Errorf("zero dash err = %v", err)
	}
	if _, err := StrokeBounds(p, Identity, StrokeStyle{Width: 2, Dashes: []float64{-4}}); !errors.Is(err, ErrInvalidValue) {
		t.Errorf("negative dash err = %v", err)
	}
}

func TestZeroWidthStroke(t *testing.T) {
	p := strokePath()
	got, err := StrokeBounds(p, Identity, StrokeStyle{Width: 0, MiterLimit: 4, Cap: CapButt, JoinMode: MiterClip})
	if err != nil {
		t.Fatal(err)
	}
	ref := referencePathBox(t, p, Identity)
	// Zero width collapses to the spine box plus flatten slack.
	assertContains(t, "zero width", got, ref, 2.0/64.0)
}

func TestDashPhaseAndImmutability(t *testing.T) {
	p := Path{}
	p.Start(ToFixedP(20, 20))
	p.Line(ToFixedP(320, 20))
	before := p.ToSVGPath()

	base := StrokeStyle{Width: 8, MiterLimit: 4, Cap: CapButt, JoinMode: MiterClip,
		Dashes: []float64{30, 20}}
	s0 := base
	s0.DashOffset = 0
	s1 := base
	s1.DashOffset = 25 // shifts where the first dash starts
	b0, err := StrokeBounds(p, Identity, s0)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := StrokeBounds(p, Identity, s1)
	if err != nil {
		t.Fatal(err)
	}
	// The aggregate box may coincide across a dash period; what must hold
	// is that each phase's box covers its own rendered pattern and never
	// overshoots the solid-stroke envelope.
	solid, err := StrokeBounds(p, Identity,
		StrokeStyle{Width: 8, MiterLimit: 4, Cap: CapButt, JoinMode: MiterClip})
	if err != nil {
		t.Fatal(err)
	}
	for off, box := range map[float64]fixed.Rectangle26_6{0: b0, 25: b1} {
		off := off
		px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
			d.SetStroke(ToFixed(8), ToFixed(4), ButtCap, nil, FlatGap, MiterClip,
				[]float64{30, 20}, off)
			p.AddTo(d)
		})
		assertCoversPixels(t, "dash phase", box, px)
		if !PixelCover(box, nil).In(PixelCover(solid, nil)) {
			t.Errorf("dashed box %v escapes solid box %v", box, solid)
		}
	}
	// A phase beyond the pattern period changes the leading cap position,
	// which is visible as a different pixel set in the raster reference.
	px0 := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
		d.SetStroke(ToFixed(8), ToFixed(4), ButtCap, nil, FlatGap, MiterClip,
			[]float64{30, 20}, 0)
		p.AddTo(d)
	})
	px1 := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
		d.SetStroke(ToFixed(8), ToFixed(4), ButtCap, nil, FlatGap, MiterClip,
			[]float64{30, 20}, 7)
		p.AddTo(d)
	})
	if px0 == px1 {
		t.Error("dash phase did not change the rendered pixels")
	}
	if after := p.ToSVGPath(); after != before {
		t.Errorf("StrokeBounds mutated the path:\nbefore %s\nafter  %s", before, after)
	}
}

// ---------------- randomized property tests ----------------

func randomPath(rnd *rand.Rand, n int) Path {
	p := Path{}
	x, y := rnd.Float64()*200-100, rnd.Float64()*200-100
	p.Start(ToFixedP(x, y))
	for i := 0; i < n; i++ {
		switch rnd.Intn(3) {
		case 0:
			x += rnd.Float64()*80 - 40
			y += rnd.Float64()*80 - 40
			p.Line(ToFixedP(x, y))
		case 1:
			p.QuadBezier(
				ToFixedP(x+rnd.Float64()*120-60, y+rnd.Float64()*120-60),
				ToFixedP(x+rnd.Float64()*120-60, y+rnd.Float64()*120-60))
			x, y = float64(p[len(p)-2])/64, float64(p[len(p)-1])/64
		default:
			p.CubeBezier(
				ToFixedP(x+rnd.Float64()*160-80, y+rnd.Float64()*160-80),
				ToFixedP(x+rnd.Float64()*160-80, y+rnd.Float64()*160-80),
				ToFixedP(x+rnd.Float64()*160-80, y+rnd.Float64()*160-80))
			x, y = float64(p[len(p)-2])/64, float64(p[len(p)-1])/64
		}
	}
	if rnd.Intn(2) == 0 {
		p.Stop(true)
	}
	return p
}

func TestRandomFillBounds(t *testing.T) {
	rnd := rand.New(rand.NewSource(263263))
	for i := 0; i < 200; i++ {
		p := randomPath(rnd, 6)
		m := Identity.
			Translate(rnd.Float64()*40-20, rnd.Float64()*40-20).
			Rotate((rnd.Float64()-0.5)*math.Pi).
			Scale(0.3+rnd.Float64()*2, 0.3+rnd.Float64()*2)
		got, err := FillBounds(p, m)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		ref := referencePathBox(t, p, m)
		assertContains(t, "random fill", got, ref, 1.0/64.0)
		assertTight(t, "random fill", got, ref, 1.0/64.0)

		// Streaming adder must agree with the Path query.
		ba, _ := NewBoundsAdder(m)
		p.AddTo(ba)
		streamed, err := ba.Rect()
		if err != nil {
			t.Fatalf("case %d stream: %v", i, err)
		}
		if streamed != got {
			t.Errorf("case %d: streamed %v != path query %v", i, streamed, got)
		}
	}
}

func TestRandomStrokeBounds(t *testing.T) {
	rnd := rand.New(rand.NewSource(97531))
	joins := []JoinMode{Arc, ArcClip, Miter, MiterClip, Bevel, Round}
	caps := []CapStyle{CapButt, CapSquare, CapRound, CapCubic, CapQuadratic}
	for i := 0; i < 120; i++ {
		p := randomPath(rnd, 5)
		m := Identity.
			Translate(rnd.Float64()*30-15, rnd.Float64()*30-15).
			Rotate((rnd.Float64() - 0.5) * 0.8)
		s := StrokeStyle{
			Width:      1 + rnd.Float64()*12,
			MiterLimit: 1 + rnd.Float64()*6,
			Cap:        caps[rnd.Intn(len(caps))],
			JoinMode:   joins[rnd.Intn(len(joins))],
		}
		if rnd.Intn(2) == 0 {
			s.Dashes = []float64{5 + rnd.Float64()*30, 3 + rnd.Float64()*20}
			s.DashOffset = (rnd.Float64() - 0.5) * 40
		}
		got, err := StrokeBounds(p, m, s)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		// Independent geometric envelope: dense transformed spine box grown
		// by the device half-width plus the bounded miter allowance.
		ref := referencePathBox(t, p, m)
		ratio := s.MiterLimit
		if ratio <= 0 {
			ratio = 4
		} else if ratio < 1 {
			ratio = 1
		}
		if ratio > 32768 {
			ratio = 32768
		}
		envelope := s.Width / 2 * (1 + ratio)
		ref.grow(envelope + 3.0/64.0)
		assertInside(t, "random stroke envelope", got, ref, 3.0/64.0)

		style := s
		px := pixelBox(t, func(_ *Filler, d *Dasher, _ *image.RGBA) {
			mp := &MatrixAdder{Adder: d, M: m}
			d.SetStroke(ToFixed(style.Width), ToFixed(style.MiterLimit),
				capFunc(style.Cap), nil, gapFunc(style.JoinMode), style.JoinMode,
				style.Dashes, style.DashOffset)
			p.AddTo(mp)
		})
		assertCoversPixels(t, "random stroke pixels", got, px)
	}
}

// ---------------- test helpers mapping styles to package funcs ----------------

func ToFixed(v float64) fixed.Int26_6 { return fixed.Int26_6(math.Round(v * 64)) }

func capFunc(c CapStyle) CapFunc {
	switch c {
	case CapSquare:
		return SquareCap
	case CapRound:
		return RoundCap
	case CapCubic:
		return CubicCap
	case CapQuadratic:
		return QuadraticCap
	default:
		return ButtCap
	}
}

func gapFunc(j JoinMode) GapFunc {
	if j == Round {
		return RoundGap
	}
	return FlatGap
}
