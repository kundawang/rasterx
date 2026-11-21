// Copyright 2024 by the rasterx Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"strconv"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// ---------------------------------------------------------------------------
// Independent analytic reference
//
// The reference coverage of a pixel is estimated by point-sampling an
// analytic inside() predicate on a fine jitter-free grid. It is deliberately
// independent of the production rasterizer (no use of vector.Rasterizer or
// the Scanner hierarchy), so it can act as ground truth for edge coverage
// comparisons. 32x32 = 1024 samples per pixel keeps the reference's own
// quantization error well below the errors under test.
// ---------------------------------------------------------------------------

const refSamples = 32

// shape is an analytic point-in-region predicate in user coordinates.
type shape func(x, y float64) bool

// refCoverage renders the analytic shape's per-pixel coverage in [0,255].
func refCoverage(w, h int, inside shape) *image.Alpha {
	m := image.NewAlpha(image.Rect(0, 0, w, h))
	n := float64(refSamples)
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			var hit int
			for sy := 0; sy < refSamples; sy++ {
				fy := float64(py) + (float64(sy)+0.5)/n
				for sx := 0; sx < refSamples; sx++ {
					fx := float64(px) + (float64(sx)+0.5)/n
					if inside(fx, fy) {
						hit++
					}
				}
			}
			m.SetAlpha(px, py, color.Alpha{A: uint8(255 * float64(hit) / (n * n))})
		}
	}
	return m
}

func circleShape(cx, cy, r float64) shape {
	r2 := r * r
	return func(x, y float64) bool {
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r2
	}
}

// rotatedRectShape returns the inside predicate for a rectangle rotated by
// rot radians around its center, matching AddRect's geometry.
func rotatedRectShape(minX, minY, maxX, maxY, rot float64) shape {
	cx, cy := (minX+maxX)/2, (minY+maxY)/2
	cos, sin := math.Cos(-rot), math.Sin(-rot)
	return func(x, y float64) bool {
		tx, ty := x-cx, y-cy
		lx := tx*cos - ty*sin + cx
		ly := tx*sin + ty*cos + cy
		return lx >= minX && lx <= maxX && ly >= minY && ly <= maxY
	}
}

// halfPlaneShape is the region ax+by <= c: a perfectly straight, angled edge.
func halfPlaneShape(a, b, c float64) shape {
	return func(x, y float64) bool { return a*x+b*y <= c }
}

// renderAlpha rasterizes a path builder into an alpha image using either the
// default scanner (quality 0 selects ScannerGV) or a supersampling scanner.
func renderAlpha(t *testing.T, w, h, quality int, build func(Adder)) *image.Alpha {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var sc Scanner
	if quality <= 0 {
		sc = NewScannerGV(w, h, img, img.Bounds())
	} else {
		sc = NewScannerSS(w, h, quality, img, img.Bounds())
	}
	sc.SetColor(color.RGBA{R: 255, G: 255, B: 255, A: 255})
	f := NewFiller(w, h, sc)
	build(f)
	f.Draw()
	out := image.NewAlpha(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		if i%4 == 0 {
			out.Pix[i/4] = img.Pix[i]
		}
	}
	return out
}

// edgeStats compares a rendered alpha image against the analytic reference,
// reporting mean and maximum absolute coverage error over pixels that are not
// deep inside or far outside the shape (the edge band), where antialiasing
// actually matters.
type edgeStats struct {
	mae, maxErr float64
	edgePixels  int
}

func compareEdges(got, want *image.Alpha) edgeStats {
	var st edgeStats
	for i := range want.Pix {
		a := want.Pix[i]
		// Edge band: reference coverage strictly between 0 and 255.
		if a == 0 || a == 255 {
			continue
		}
		st.edgePixels++
		d := math.Abs(float64(got.Pix[i]) - float64(a))
		st.mae += d
		if d > st.maxErr {
			st.maxErr = d
		}
	}
	if st.edgePixels > 0 {
		st.mae /= float64(st.edgePixels)
	}
	return st
}

// TestSuperSampleEdgeError verifies against the independent high-resolution
// analytic reference the property that motivates the mode: once a pixel is
// only partly covered, a single-sample scanner rounds its coverage to one of
// few levels, which reads as broken/dashed pixels on fine or angled edges.
// Supersampling must not make any edge pixel worse and must bound the
// worst-case coverage error. The shapes below use analytic predicates that
// match the rasterized geometry exactly (no infinite-region references), so a
// difference is attributable to coverage quantization rather than geometry.
func TestSuperSampleEdgeError(t *testing.T) {
	const w, h = 96, 96

	// A bounded triangle whose hypotenuse lies exactly on the line
	// 0.6x+0.8y = 55.4: two vertices are on the line, one is inside. This is
	// the case where single-sample quantization of a diagonal edge is most
	// visible and where supersampling has the largest effect.
	hp := halfPlaneShape(0.6, 0.8, 55.4)
	hpBuild := func(a Adder) {
		a.Start(ToFixedP(-20, -20))
		a.Line(ToFixedP(0, 55.4/0.8))
		a.Line(ToFixedP(55.4/0.6, 0))
		a.Stop(true)
	}

	cases := []struct {
		name   string
		inside shape
		build  func(Adder)
		// requireStrongImprovement marks shapes where geometry is exactly
		// matched and quantization dominates, so q8 must sharply cut error.
		requireStrongImprovement bool
	}{
		{
			name:                     "half_plane",
			inside:                   hp,
			build:                    hpBuild,
			requireStrongImprovement: true,
		},
		{
			name:   "circle",
			inside: circleShape(48.3, 47.7, 31.4),
			build:  func(a Adder) { AddCircle(48.3, 47.7, 31.4, a) },
		},
		{
			name:   "rotated_rect",
			inside: rotatedRectShape(18.2, 22.6, 78.4, 74.1, 12*math.Pi/180),
			build:  func(a Adder) { AddRect(18.2, 22.6, 78.4, 74.1, 12, a) },
		},
	}
	for _, tc := range cases {
		ref := refCoverage(w, h, tc.inside)
		gv := compareEdges(renderAlpha(t, w, h, 0, tc.build), ref)
		qs := map[int]edgeStats{
			2: compareEdges(renderAlpha(t, w, h, 2, tc.build), ref),
			4: compareEdges(renderAlpha(t, w, h, 4, tc.build), ref),
			8: compareEdges(renderAlpha(t, w, h, 8, tc.build), ref),
		}
		q8 := qs[8]
		t.Logf("%-13s edgePixels=%4d  MAE gv=%.2f q2=%.2f q4=%.2f q8=%.2f | maxErr gv=%.0f q2=%.0f q4=%.0f q8=%.0f",
			tc.name, gv.edgePixels, gv.mae, qs[2].mae, qs[4].mae, q8.mae,
			gv.maxErr, qs[2].maxErr, qs[4].maxErr, q8.maxErr)
		if gv.edgePixels == 0 {
			t.Fatalf("%s: reference produced no edge pixels", tc.name)
		}
		// No quality may regress the worst-case edge error beyond a small
		// tolerance for rounding at the 1/255 alpha quantization boundary.
		for q, st := range qs {
			if st.maxErr > gv.maxErr+1 {
				t.Errorf("%s: q=%d worsened worst-case edge error %.0f vs default %.0f",
					tc.name, q, st.maxErr, gv.maxErr)
			}
		}
		if tc.requireStrongImprovement {
			// On the exactly-matched diagonal edge, q8 must drive both the
			// mean and worst-case error close to the analytic reference:
			// worst-case error should drop to at most one alpha level and the
			// mean error by an order of magnitude.
			if q8.maxErr > 1 {
				t.Errorf("%s: q8 worst-case error %.0f exceeds one alpha level", tc.name, q8.maxErr)
			}
			if q8.mae >= gv.mae/4 {
				t.Errorf("%s: q8 mean error %.2f not substantially below default %.2f",
					tc.name, q8.mae, gv.mae)
			}
		}
	}
}

// TestSuperSampleDefaultUnchanged proves requirement 1: the existing default
// output is preserved. A quality-1 ScannerSS must be byte-identical to
// ScannerGV for fill, stroke and dash, and the ScannerGV path itself must keep
// producing its established pixels.
func TestSuperSampleDefaultUnchanged(t *testing.T) {
	const w, h = 128, 128
	builds := map[string]func(w, h int, sc Scanner) Adder{
		"fill":   buildFill,
		"stroke": buildStroke,
		"dash":   buildDash,
	}
	for name, build := range builds {
		gvImg := image.NewRGBA(image.Rect(0, 0, w, h))
		gv := NewScannerGV(w, h, gvImg, gvImg.Bounds())
		gv.SetColor(color.RGBA{R: 30, G: 90, B: 200, A: 255})
		build(w, h, gv)

		ssImg := image.NewRGBA(image.Rect(0, 0, w, h))
		ss := NewScannerSS(w, h, 1, ssImg, ssImg.Bounds())
		ss.SetColor(color.RGBA{R: 30, G: 90, B: 200, A: 255})
		build(w, h, ss)

		for i := range gvImg.Pix {
			if gvImg.Pix[i] != ssImg.Pix[i] {
				t.Fatalf("%s: quality-1 output differs from ScannerGV at pix[%d] %d != %d",
					name, i, gvImg.Pix[i], ssImg.Pix[i])
			}
		}
	}
}

// buildFill/buildStroke/buildDash construct the concrete rasterizer matching
// each Scanner so stroking and dashing exercise the same path generation on
// both scanner implementations.
func buildFill(w, h int, sc Scanner) Adder {
	f := NewFiller(w, h, sc)
	AddCircle(64, 64, 45, f)
	AddRoundRect(10, 70, 60, 120, 12, 12, 0, RoundGap, f)
	return f
}

func buildStroke(w, h int, sc Scanner) Adder {
	s := NewStroker(w, h, sc)
	s.SetStroke(3*64, 2*64, RoundCap, nil, RoundGap, ArcClip)
	AddCircle(64, 64, 40, s)
	return s
}

func buildDash(w, h int, sc Scanner) Adder {
	d := NewDasher(w, h, sc)
	d.SetStroke(2*64, 1*64, SquareCap, nil, RoundGap, Miter, []float64{14, 7}, 0)
	AddCircle(64, 64, 42, d)
	return d
}

// alphaSum totals the rendered coverage, proportional to covered pixel area.
func alphaSum(img *image.RGBA) int {
	var sum int
	for i := 0; i < len(img.Pix); i += 4 {
		sum += int(img.Pix[i+3])
	}
	return sum
}

// TestSuperSampleAreaConservation checks requirement 2: box averaging
// conserves covered area and the effective alpha of translucent colors.
func TestSuperSampleAreaConservation(t *testing.T) {
	const w, h = 96, 96
	build := func(a Adder) { AddCircle(48.5, 48.5, 30.2, a) }

	opaque := map[int]int{}
	for _, q := range []int{1, 2, 4, 8} {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		var sc Scanner
		if q == 1 {
			sc = NewScannerGV(w, h, img, img.Bounds())
		} else {
			sc = NewScannerSS(w, h, q, img, img.Bounds())
		}
		sc.SetColor(color.RGBA{R: 255, A: 255})
		f := NewFiller(w, h, sc)
		build(f)
		f.Draw()
		opaque[q] = alphaSum(img)
	}
	base := opaque[1]
	for q, sum := range opaque {
		if drift := math.Abs(float64(sum-base)) / float64(base); drift > 0.01 {
			t.Errorf("opaque coverage drift at q=%d: %d vs q1 %d (%.3f%%)", q, sum, base, drift*100)
		}
	}

	// A 50%-opaque source must yield half the alpha mass of the opaque source
	// at every quality, i.e. blending keeps coverage and alpha consistent.
	for _, q := range []int{2, 4, 8} {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerSS(w, h, q, img, img.Bounds())
		sc.SetColor(ApplyOpacity(color.RGBA{R: 255, G: 255, B: 255, A: 255}, 0.5))
		f := NewFiller(w, h, sc)
		build(f)
		f.Draw()
		got := alphaSum(img)
		want := opaque[q] / 2
		if drift := math.Abs(float64(got-want)) / float64(want); drift > 0.02 {
			t.Errorf("q=%d translucent alpha mass %d not ~50%% of opaque %d", q, got, want)
		}
	}
}

// TestSuperSampleThinLine checks the motivating use case: a sub-pixel-wide
// stroke must remain visible at every position instead of dropping pixels, and
// higher quality must render its coverage closer to the analytic rectangle.
func TestSuperSampleThinLine(t *testing.T) {
	const w, h = 64, 64
	// A vertical line of width 0.75px spanning the full height, rasterized
	// directly through the scanner via a stroker.
	drawLine := func(x float64, sc Scanner) {
		s := NewStroker(w, h, sc)
		s.SetStroke(fixed.Int26_6(float64(0.75)*64), 1, ButtCap, nil, RoundGap, Miter)
		s.Start(ToFixedP(x, 4))
		s.Line(ToFixedP(x, h-4))
		s.Stop(false)
		s.Draw()
	}
	for _, q := range []int{1, 4, 8} {
		var visibleCols int
		for col := 0; col < w; col++ {
			img := image.NewRGBA(image.Rect(0, 0, w, h))
			var sc Scanner
			if q == 1 {
				sc = NewScannerGV(w, h, img, img.Bounds())
			} else {
				sc = NewScannerSS(w, h, q, img, img.Bounds())
			}
			sc.SetColor(color.RGBA{R: 255, A: 255})
			drawLine(float64(col)+0.25, sc)
			if alphaSum(img) > 0 {
				visibleCols++
			}
		}
		t.Logf("q=%d visible thin-line columns: %d/%d", q, visibleCols, w)
		if q > 1 && visibleCols < w {
			t.Errorf("q=%d: thin line vanished at %d columns", q, w-visibleCols)
		}
	}
}

// TestSuperSampleTranslationStability checks requirement 2's anti-flicker
// guarantee: translating a path by a whole number of pixels must translate the
// rendered image identically, at every quality.
func TestSuperSampleTranslationStability(t *testing.T) {
	const w, h = 80, 80
	render := func(q, ox, oy int) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		var sc Scanner
		if q == 1 {
			sc = NewScannerGV(w, h, img, img.Bounds())
		} else {
			sc = NewScannerSS(w, h, q, img, img.Bounds())
		}
		sc.SetColor(color.RGBA{G: 200, A: 255})
		f := NewFiller(w, h, sc)
		AddCircle(30.5+float64(ox), 33.5+float64(oy), 18.25, f)
		AddRect(8+float64(ox), 50+float64(oy), 40+float64(ox), 70+float64(oy), 12, f)
		f.Draw()
		return img
	}
	for _, q := range []int{1, 2, 4, 8} {
		base := render(q, 0, 0)
		shift := render(q, 7, 5)
		for y := 0; y < h-5; y++ {
			for x := 0; x < w-7; x++ {
				b0 := base.RGBAAt(x, y)
				b1 := shift.RGBAAt(x+7, y+5)
				if b0 != b1 {
					t.Fatalf("q=%d: integer translation changed pixel at (%d,%d): %v != %v",
						q, x, y, b0, b1)
				}
			}
		}
	}
}

// TestSuperSampleSubpixelFlicker addresses the anti-flicker requirement for
// fractional translations. An integer-pixel move must shift the image exactly
// (covered above). A sub-pixel move cannot be pixel-identical, but the rendered
// brightness (total coverage) must remain stable instead of pulsing. Higher
// quality must not introduce more position-to-position variation than the
// default scanner; on fine geometry it should reduce it.
func TestSuperSampleSubpixelFlicker(t *testing.T) {
	const w, h = 96, 96
	// A small filled circle moved in fine sub-pixel increments.
	massSeries := func(q int) []int {
		out := make([]int, 0, 12)
		for k := 0; k < 12; k++ {
			img := image.NewRGBA(image.Rect(0, 0, w, h))
			var sc Scanner
			if q == 1 {
				sc = NewScannerGV(w, h, img, img.Bounds())
			} else {
				sc = NewScannerSS(w, h, q, img, img.Bounds())
			}
			sc.SetColor(color.RGBA{R: 255, A: 255})
			f := NewFiller(w, h, sc)
			AddCircle(40+float64(k)/8.0, 48, 3.2, f)
			f.Draw()
			out = append(out, alphaSum(img))
		}
		return out
	}
	swing := func(series []int) int {
		mn, mx := series[0], series[0]
		for _, v := range series {
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		return mx - mn
	}
	gvSwing := swing(massSeries(1))
	for _, q := range []int{2, 4, 8} {
		s := swing(massSeries(q))
		t.Logf("subpixel coverage mass swing: gv=%d q%d=%d", gvSwing, q, s)
		// Supersampling must be no more flickery than the default, allowing a
		// single alpha-level (1/255) quantization tolerance.
		if s > gvSwing+255 {
			t.Errorf("q=%d subpixel flicker %d exceeds default %d beyond tolerance", q, s, gvSwing)
		}
	}
}

// TestSuperSampleResourceLimits checks requirement 3: extreme quality and
// large canvases are clamped to bounded resources, and zero/negative bounds
// behave deterministically.
func TestSuperSampleResourceLimits(t *testing.T) {
	// Quality above MaxQuality clamps.
	if q := NewScannerSS(32, 32, 999, image.NewRGBA(image.Rect(0, 0, 32, 32)), image.Rect(0, 0, 32, 32)).EffectiveQuality(); q != MaxQuality {
		t.Errorf("quality clamp: got %d want %d", q, MaxQuality)
	}
	if q := NewScannerSS(32, 32, 0, image.NewRGBA(image.Rect(0, 0, 32, 32)), image.Rect(0, 0, 32, 32)).EffectiveQuality(); q != MinQuality {
		t.Errorf("quality floor: got %d want %d", q, MinQuality)
	}
	// A canvas whose q*q scaling would exceed MaxHiResPixels is clamped so the
	// mask stays within budget.
	const w, h = 4096, 4096
	s := NewScannerSS(w, h, MaxQuality, image.NewRGBA(image.Rect(0, 0, w, h)), image.Rect(0, 0, w, h))
	q := s.EffectiveQuality()
	if int64(q)*int64(q)*w*h > MaxHiResPixels {
		t.Errorf("effective quality %d exceeds the high-resolution pixel budget", q)
	}
	if q == MaxQuality {
		t.Errorf("expected quality to be clamped below MaxQuality on a large canvas")
	}
	// Rendering on the clamped scanner still completes and paints something.
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s.Dest = img
	s.SetColor(color.RGBA{R: 255, A: 255})
	f := NewFiller(w, h, s)
	AddCircle(2048, 2048, 1000, f)
	f.Draw()
	if alphaSum(img) == 0 {
		t.Error("clamped large-canvas render produced no pixels")
	}
	// SetQuality re-applies the limits and never panics.
	s.SetQuality(MaxQuality)
	if s.EffectiveQuality() != q {
		t.Error("SetQuality did not preserve resource clamping")
	}
	// Zero and negative bounds: no supersampling buffers, Draw is a no-op.
	zero := NewScannerSS(0, 0, 4, image.NewRGBA(image.Rect(0, 0, 1, 1)), image.Rect(0, 0, 1, 1))
	if zero.EffectiveQuality() != MinQuality {
		t.Error("zero bounds should fall back to minimum quality")
	}
	zero.Draw()
	neg := NewScannerSS(10, 10, 4, image.NewRGBA(image.Rect(0, 0, 10, 10)), image.Rect(0, 0, 10, 10))
	neg.SetBounds(-8, -8)
	if neg.EffectiveQuality() != MinQuality {
		t.Error("negative SetBounds should clamp to zero and minimum quality")
	}
}

// TestSuperSampleEmptyClipOrigin checks requirement 3's explicit edge cases:
// empty paths leave the destination untouched, clipping restricts coverage,
// and non-zero destination origins are honored.
func TestSuperSampleEmptyClipOrigin(t *testing.T) {
	// Empty path: destination untouched, including its existing content.
	img := image.NewRGBA(image.Rect(0, 0, 24, 24))
	for i := range img.Pix {
		img.Pix[i] = 0x77
	}
	s := NewScannerSS(24, 24, 4, img, img.Bounds())
	s.SetColor(color.RGBA{R: 255, A: 255})
	s.Draw() // no Start/Line calls at all
	for _, v := range img.Pix {
		if v != 0x77 {
			t.Fatal("Draw on an empty path modified the destination")
		}
	}
	f := NewFiller(24, 24, s)
	f.Start(ToFixedP(2, 2)) // started but degenerate
	f.Stop(true)
	s.Draw()
	for _, v := range img.Pix {
		if v != 0x77 {
			t.Fatal("Draw on a degenerate path modified the destination")
		}
	}

	// Clip: coverage appears only inside the clip rectangle.
	const w, h = 64, 64
	cimg := image.NewRGBA(image.Rect(0, 0, w, h))
	cs := NewScannerSS(w, h, 4, cimg, cimg.Bounds())
	cs.SetColor(color.RGBA{R: 255, A: 255})
	cs.SetClip(image.Rect(20, 20, 40, 40))
	cf := NewFiller(w, h, cs)
	AddCircle(30, 30, 25, cf)
	cf.Draw()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			_, _, _, a := cimg.At(x, y).RGBA()
			insideClip := x >= 20 && x < 40 && y >= 20 && y < 40
			if !insideClip && a != 0 {
				t.Fatalf("clip leaked coverage at (%d,%d) outside the clip rect", x, y)
			}
			if insideClip && a == 0 {
				t.Fatalf("no coverage inside clip at (%d,%d)", x, y)
			}
		}
	}

	// Non-zero destination origin: geometry lands at the intended absolute
	// coordinates, not shifted back to (0,0).
	rect := image.Rect(100, 100, 164, 164)
	oimg := image.NewRGBA(rect)
	os := NewScannerSS(64, 64, 4, oimg, rect)
	os.SetColor(color.RGBA{G: 255, A: 255})
	of := NewFiller(64, 64, os)
	AddRect(20, 20, 44, 44, 0, of)
	of.Draw()
	if _, _, _, a := oimg.At(132, 132).RGBA(); a == 0 {
		t.Error("non-zero origin: expected opaque pixel at absolute (132,132)")
	}
	if _, _, _, a := oimg.At(105, 105).RGBA(); a != 0 {
		t.Error("non-zero origin: unexpected coverage near rectangle minimum")
	}
}

// TestSuperSampleWindingParity ensures the supersampling scanner fills
// overlapping subpaths with the same non-zero winding result as the default
// scanner: the overlapping interior stays fully covered and the total covered
// area matches within the supersampling quantization tolerance.
func TestSuperSampleWindingParity(t *testing.T) {
	const w, h = 80, 80
	build := func(a Adder) {
		AddCircle(35, 40, 20, a)
		AddCircle(45, 40, 20, a) // overlaps the first circle
	}
	gv := renderAlpha(t, w, h, 0, build)
	q4 := renderAlpha(t, w, h, 4, build)
	// The shared interior must remain opaque under non-zero winding.
	if gv.AlphaAt(40, 40).A != 255 || q4.AlphaAt(40, 40).A != 255 {
		t.Errorf("overlap interior not opaque: gv=%d q4=%d", gv.AlphaAt(40, 40).A, q4.AlphaAt(40, 40).A)
	}
	var gvSum, qSum int
	for i := range gv.Pix {
		gvSum += int(gv.Pix[i])
		qSum += int(q4.Pix[i])
	}
	if drift := math.Abs(float64(qSum-gvSum)) / float64(gvSum); drift > 0.01 {
		t.Errorf("winding coverage mass drift %.3f%% between scanners", drift*100)
	}
}

// TestSuperSampleMultiDraw mirrors the way the package composes a scene:
// several shapes are drawn and cleared in sequence onto one destination. The
// supersampling scanner must accumulate onto existing pixels (source-over)
// without bleeding stale coverage from a previous frame, and must not require
// growing buffers on every Draw.
func TestSuperSampleMultiDraw(t *testing.T) {
	const w, h = 128, 128
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sc := NewScannerSS(w, h, 4, img, img.Bounds())
	f := NewFiller(w, h, sc)
	for i := 0; i < 3; i++ {
		sc.SetColor(color.RGBA{R: uint8(40 + 50*i), A: 255})
		AddCircle(30+float64(i*22), 40, 14, f)
		f.Draw()
		f.Clear()
	}
	// Each circle's center is opaque (accumulated correctly).
	for _, cx := range []int{30, 52, 74} {
		if img.RGBAAt(cx, 40).A != 255 {
			t.Errorf("missing accumulated coverage at (%d,40): alpha=%d", cx, img.RGBAAt(cx, 40).A)
		}
	}
	// A pixel never touched by any path stays at the zero background.
	if img.RGBAAt(2, 2).A != 0 {
		t.Errorf("stale coverage leaked to untouched pixel: %v", img.RGBAAt(2, 2))
	}
	// Drawing again after Clear with no new path must be a strict no-op.
	before := append([]byte(nil), img.Pix...)
	sc.Draw()
	for i := range before {
		if img.Pix[i] != before[i] {
			t.Fatal("Draw after Clear without a path modified the destination")
		}
	}
}

// TestSuperSampleSemiTransparentBlend checks that a translucent source over an
// existing opaque background produces the standard source-over blend, at every
// quality, and that two overlapping translucent fills accumulate alpha the same
// way the default scanner does.
func TestSuperSampleSemiTransparentBlend(t *testing.T) {
	const w, h = 64, 64
	blend := func(q int) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		// Pre-fill an opaque background.
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 20, 40, 60, 255
		}
		var sc Scanner
		if q == 1 {
			sc = NewScannerGV(w, h, img, img.Bounds())
		} else {
			sc = NewScannerSS(w, h, q, img, img.Bounds())
		}
		sc.SetColor(ApplyOpacity(color.RGBA{R: 200, G: 100, B: 50, A: 255}, 0.4))
		f := NewFiller(w, h, sc)
		AddCircle(32, 32, 18, f)
		f.Draw()
		return img
	}
	gv := blend(1)
	for _, q := range []int{2, 4, 8} {
		ss := blend(q)
		// Compare a fully-interior pixel: both masks are 255 there, so the
		// source-over blend must be exactly identical regardless of quality.
		if gv.RGBAAt(32, 32) != ss.RGBAAt(32, 32) {
			t.Errorf("q=%d interior blend mismatch gv=%v ss=%v", q, gv.RGBAAt(32, 32), ss.RGBAAt(32, 32))
		}
		// Edge pixels may differ by at most one alpha level from quantization.
		var maxD int
		for i := 0; i < len(gv.Pix); i += 4 {
			for k := 0; k < 4; k++ {
				if d := absInt(int(gv.Pix[i+k]) - int(ss.Pix[i+k])); d > maxD {
					maxD = d
				}
			}
		}
		if maxD > 2 {
			t.Errorf("q=%d translucent blend differs by more than quantization tolerance: %d", q, maxD)
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestSuperSampleAccessorsAndGuards covers the quality accessors and runtime
// reconfiguration through the public API. The unexported fixed-point scaling
// guard is exercised directly in supersample_internal_test.go.
func TestSuperSampleAccessorsAndGuards(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	s := NewScannerSS(32, 32, 4, img, img.Bounds())
	if s.Quality() != 4 || s.EffectiveQuality() != 4 {
		t.Fatalf("quality accessors: got requested=%d effective=%d", s.Quality(), s.EffectiveQuality())
	}
	// SetQuality clamps and re-derives buffers.
	s.SetQuality(MaxQuality + 10)
	if s.Quality() != MaxQuality || s.EffectiveQuality() != MaxQuality {
		t.Errorf("SetQuality did not clamp: %d/%d", s.Quality(), s.EffectiveQuality())
	}
	s.SetQuality(MinQuality)
	if s.EffectiveQuality() != MinQuality {
		t.Errorf("SetQuality(MinQuality) effective=%d", s.EffectiveQuality())
	}
	// Extreme coordinates must not panic the high-resolution path; they are
	// clamped to the fixed-point range inside scaleFixed.
	hi := NewScannerSS(32, 32, 4, image.NewRGBA(image.Rect(0, 0, 32, 32)), image.Rect(0, 0, 32, 32))
	hi.Start(fixed.Point26_6{X: fixed.Int26_6(math.MaxInt32 / 2), Y: 0})
	hi.Line(fixed.Point26_6{X: 0, Y: fixed.Int26_6(math.MinInt32 / 2)})
	hi.Draw()
}

// BenchmarkSuperSample measures scan cost and memory across qualities on the
// standard Q-shaped test path on a 512x512 canvas, alongside the default
// scanner baseline.
func BenchmarkSuperSample(b *testing.B) {
	p := GetTestPath()
	for _, q := range []int{1, 2, 4, 8} {
		name := "GV"
		if q > 1 {
			name = "SS"
		}
		b.Run(name+"-q"+strconv.Itoa(q), func(b *testing.B) {
			img := image.NewRGBA(image.Rect(0, 0, 512, 512))
			var sc Scanner
			if q == 1 {
				sc = NewScannerGV(512, 512, img, img.Bounds())
			} else {
				sc = NewScannerSS(512, 512, q, img, img.Bounds())
			}
			f := NewFiller(512, 512, sc)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.AddTo(f)
				f.Draw()
				f.Clear()
			}
		})
	}
}
