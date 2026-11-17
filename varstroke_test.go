// Copyright 2026 by the rasterx Authors. All rights reserved.
//
// Tests for the variable width stroker (varstroke.go).
package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// alphaAt returns the 8 bit alpha of the pixel at x, y.
func alphaAt(img *image.RGBA, x, y int) int {
	_, _, _, a := img.At(x, y).RGBA()
	return int(a >> 8)
}

// countFilled returns the number of pixels with alpha > 128.
func countFilled(img *image.RGBA) int {
	b := img.Bounds()
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if alphaAt(img, x, y) > 128 {
				n++
			}
		}
	}
	return n
}

func newVarStrokeImage(wx, wy int) (*image.RGBA, *ScannerGV, *VarStroker) {
	img := image.NewRGBA(image.Rect(0, 0, wx, wy))
	scanner := NewScannerGV(wx, wy, img, img.Bounds())
	scanner.SetColor(color.RGBA{255, 255, 255, 255})
	v := NewVarStroker(wx, wy, scanner)
	return img, scanner, v
}

// TestVarStrokeTaperedLine strokes a straight horizontal line tapering from
// width 40 to width 8 and checks the outline boundaries column by column.
func TestVarStrokeTaperedLine(t *testing.T) {
	const (
		wx, wy = 512, 512
		x0, x1 = 60.0, 452.0
		y0     = 256.0
		w0, w1 = 40.0, 8.0
	)
	img, scanner, v := newVarStrokeImage(wx, wy)
	v.SetStroke(1<<6, 4<<6, ButtCap, nil, Bevel)
	v.SetWidths(WidthPoint{Pos: 0, Width: w0}, WidthPoint{Pos: 1, Width: w1})
	v.Start(ToFixedP(x0, y0))
	v.Line(ToFixedP(x1, y0))
	v.Stop(false)
	v.Draw()

	halfAt := func(x float64) float64 {
		w := w0 + (w1-w0)*(x-x0)/(x1-x0)
		return w / 2
	}
	// Boundary check per column: well inside the local half width must be
	// filled, well outside must be empty.
	for x := int(x0) + 2; x <= int(x1)-2; x += 7 {
		h := halfAt(float64(x))
		for y := 0; y < wy; y++ {
			d := math.Abs(float64(y) - y0)
			a := alphaAt(img, x, y)
			switch {
			case d < h-2 && a <= 128:
				t.Fatalf("x=%d y=%d: inside taper (h=%.2f) but alpha=%d", x, y, h, a)
			case d > h+2 && a != 0:
				t.Fatalf("x=%d y=%d: outside taper (h=%.2f) but alpha=%d", x, y, h, a)
			}
		}
	}
	// The stroke must be symmetric about the center line: the filled range
	// of each column must be centered on y0 and span the local width.
	for x := int(x0) + 2; x <= int(x1)-2; x += 13 {
		lo, hi := -1, -1
		for y := 0; y < wy; y++ {
			if alphaAt(img, x, y) > 128 {
				if lo < 0 {
					lo = y
				}
				hi = y
			}
		}
		if lo < 0 {
			t.Fatalf("x=%d: nothing filled", x)
		}
		if c := float64(lo+hi)/2 - y0; c < -1.5 || c > 1.5 {
			t.Fatalf("x=%d: filled range [%d,%d] not centered on %v", x, lo, hi, y0)
		}
		if h := float64(hi-lo+1) / 2; math.Abs(h-halfAt(float64(x))) > 1.5 {
			t.Fatalf("x=%d: filled half width %.2f, want %.2f", x, h, halfAt(float64(x)))
		}
	}
	// Extent check: butt caps, so the outline spans the line exactly.
	ext := scanner.GetPathExtent()
	checkExtent(t, ext, x0, y0-w0/2, x1, y0+w0/2, 1.0)
}

// TestVarStrokeTaperedLineRoundCap checks that caps are controllable: round
// caps extend the outline by the local half width at each end.
func TestVarStrokeTaperedLineRoundCap(t *testing.T) {
	const (
		wx, wy = 512, 512
		x0, x1 = 100.0, 400.0
		y0     = 256.0
		w0, w1 = 40.0, 8.0
	)
	_, scanner, v := newVarStrokeImage(wx, wy)
	v.SetStroke(1<<6, 4<<6, RoundCap, nil, Bevel)
	v.SetWidths(WidthPoint{Pos: 0, Width: w0}, WidthPoint{Pos: 1, Width: w1})
	v.Start(ToFixedP(x0, y0))
	v.Line(ToFixedP(x1, y0))
	v.Stop(false)
	v.Draw()

	ext := scanner.GetPathExtent()
	// Round cap at the start has radius w0/2 = 20, at the end w1/2 = 4.
	checkExtent(t, ext, x0-w0/2, y0-w0/2, x1+w1/2, y0+w0/2, 1.0)
}

// TestVarStrokeFixedWidth checks that with no width control points the
// stroker produces a constant width outline.
func TestVarStrokeFixedWidth(t *testing.T) {
	const (
		wx, wy = 512, 512
		y0     = 256.0
		width  = 20.0
	)
	img, _, v := newVarStrokeImage(wx, wy)
	v.SetStroke(width*64, 4<<6, ButtCap, nil, Bevel)
	v.Start(ToFixedP(60, y0))
	v.Line(ToFixedP(300, y0))
	v.Line(ToFixedP(450, y0+100))
	v.Stop(false)
	v.Draw()

	// On the horizontal part every column must span y0 +/- width/2.
	for x := 62; x <= 290; x += 11 {
		for y := 0; y < wy; y++ {
			d := math.Abs(float64(y) - y0)
			a := alphaAt(img, x, y)
			switch {
			case d < width/2-2 && a <= 128:
				t.Fatalf("x=%d y=%d: inside constant stroke but alpha=%d", x, y, a)
			case d > width/2+2 && a != 0:
				t.Fatalf("x=%d y=%d: outside constant stroke but alpha=%d", x, y, a)
			}
		}
	}
}

// cubicPt evaluates a cubic bezier at t.
func cubicPt(p0, p1, p2, p3 [2]float64, t float64) [2]float64 {
	mt := 1 - t
	return [2]float64{
		mt*mt*mt*p0[0] + 3*mt*mt*t*p1[0] + 3*mt*t*t*p2[0] + t*t*t*p3[0],
		mt*mt*mt*p0[1] + 3*mt*mt*t*p1[1] + 3*mt*t*t*p2[1] + t*t*t*p3[1],
	}
}

// splitCubic splits a cubic bezier at t with de Casteljau.
func splitCubic(p0, p1, p2, p3 [2]float64, t float64) (q1, q2, q3, r1, r2 [2]float64) {
	lerp := func(a, b [2]float64) [2]float64 {
		return [2]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t}
	}
	q1 = lerp(p0, p1)
	m1 := lerp(p1, p2)
	r2 = lerp(p2, p3)
	q2 = lerp(q1, m1)
	r1 = lerp(m1, r2)
	q3 = lerp(q2, r1)
	return
}

// arcLen approximates the arc length of a cubic by dense sampling.
func arcLen(p0, p1, p2, p3 [2]float64) float64 {
	const n = 256
	prev := p0
	l := 0.0
	for i := 1; i <= n; i++ {
		p := cubicPt(p0, p1, p2, p3, float64(i)/n)
		l += math.Hypot(p[0]-prev[0], p[1]-prev[1])
		prev = p
	}
	return l
}

// TestVarStrokeSegmentConsistency strokes a cubic bezier with a tapering
// width profile in one piece, then strokes the same curve split in two
// cubics with the profile split at the corresponding arc length, and
// verifies the two renderings are essentially identical.
func TestVarStrokeSegmentConsistency(t *testing.T) {
	const wx, wy = 512, 512
	p0 := [2]float64{40, 400}
	p1 := [2]float64{180, 60}
	p2 := [2]float64{340, 500}
	p3 := [2]float64{470, 150}
	const w0, w1 = 30.0, 6.0

	q1, q2, q3, r1, r2 := splitCubic(p0, p1, p2, p3, 0.5)
	// Arc length fraction of the split point.
	frac := arcLen(p0, q1, q2, q3) / arcLen(p0, p1, p2, p3)
	wm := w0 + (w1-w0)*frac // profile is linear in arc length

	render := func(split bool) *image.RGBA {
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetStroke(1<<6, 4<<6, ButtCap, nil, Round)
		if !split {
			v.SetWidths(WidthPoint{Pos: 0, Width: w0}, WidthPoint{Pos: 1, Width: w1})
			v.Start(ToFixedP(p0[0], p0[1]))
			v.CubeBezier(ToFixedP(p1[0], p1[1]), ToFixedP(p2[0], p2[1]), ToFixedP(p3[0], p3[1]))
			v.Stop(false)
		} else {
			v.SetWidths(WidthPoint{Pos: 0, Width: w0}, WidthPoint{Pos: 1, Width: wm})
			v.Start(ToFixedP(p0[0], p0[1]))
			v.CubeBezier(ToFixedP(q1[0], q1[1]), ToFixedP(q2[0], q2[1]), ToFixedP(q3[0], q3[1]))
			v.Stop(false)
			v.SetWidths(WidthPoint{Pos: 0, Width: wm}, WidthPoint{Pos: 1, Width: w1})
			v.Start(ToFixedP(q3[0], q3[1]))
			v.CubeBezier(ToFixedP(r1[0], r1[1]), ToFixedP(r2[0], r2[1]), ToFixedP(p3[0], p3[1]))
			v.Stop(false)
		}
		v.Draw()
		return img
	}

	imgA := render(false)
	imgB := render(true)
	filledA, filledB := countFilled(imgA), countFilled(imgB)
	if filledA == 0 || filledB == 0 {
		t.Fatalf("empty render: %d, %d", filledA, filledB)
	}
	diff := 0
	for y := 0; y < wy; y++ {
		for x := 0; x < wx; x++ {
			if (alphaAt(imgA, x, y) > 128) != (alphaAt(imgB, x, y) > 128) {
				diff++
			}
		}
	}
	// Flattening differs slightly between the whole and split curves, so
	// allow a small fraction of boundary pixels to disagree.
	if max := filledA / 50; diff > max {
		t.Fatalf("split curve render differs in %d pixels (max %d)", diff, max)
	}
}

// TestVarStrokeCurvedBoundary strokes a tapering cubic and verifies that
// every filled pixel lies within the maximum half width of the centerline,
// and that the centerline itself is covered.
func TestVarStrokeCurvedBoundary(t *testing.T) {
	const wx, wy = 512, 512
	p0 := [2]float64{60, 420}
	p1 := [2]float64{160, 40}
	p2 := [2]float64{360, 480}
	p3 := [2]float64{450, 120}
	const maxHalf = 18.0 // widths 36 -> 4

	img, _, v := newVarStrokeImage(wx, wy)
	v.SetStroke(1<<6, 4<<6, ButtCap, nil, Round)
	v.SetWidths(WidthPoint{Pos: 0, Width: 36}, WidthPoint{Pos: 1, Width: 4})
	v.Start(ToFixedP(p0[0], p0[1]))
	v.CubeBezier(ToFixedP(p1[0], p1[1]), ToFixedP(p2[0], p2[1]), ToFixedP(p3[0], p3[1]))
	v.Stop(false)
	v.Draw()

	// Dense centerline samples.
	const n = 512
	var samples [][2]float64
	for i := 0; i <= n; i++ {
		samples = append(samples, cubicPt(p0, p1, p2, p3, float64(i)/n))
	}
	// Every filled pixel must be within maxHalf (+ antialias slack) of the
	// centerline.
	for y := 0; y < wy; y++ {
		for x := 0; x < wx; x++ {
			if alphaAt(img, x, y) <= 128 {
				continue
			}
			best := math.Inf(1)
			for _, s := range samples {
				d := math.Hypot(float64(x)-s[0], float64(y)-s[1])
				if d < best {
					best = d
				}
			}
			if best > maxHalf+2 {
				t.Fatalf("pixel (%d,%d) filled but %.2f from centerline", x, y, best)
			}
		}
	}
	// Every centerline sample must be covered (width is >= 4 everywhere).
	// The first and last samples sit on the butt cap boundary, so skip them.
	for i, s := range samples {
		if i == 0 || i == len(samples)-1 {
			continue
		}
		if alphaAt(img, int(s[0]+0.5), int(s[1]+0.5)) <= 128 {
			t.Fatalf("centerline sample %d at %v not covered", i, s)
		}
	}
}

// TestVarStrokeJoins checks that join mode and miter limit are controllable
// on a 90 degree corner.
func TestVarStrokeJoins(t *testing.T) {
	const (
		wx, wy = 512, 512
		cx, cy = 300, 300 // corner
	)
	// Corner from (100,300) to (300,300) to (300,100), constant width 20,
	// so the half width is 10 and the full miter tip is at distance
	// 10/cos(45) ~= 14.14 from the corner along the bisector.
	render := func(jm JoinMode, miterLimit fixed.Int26_6) *image.RGBA {
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetStroke(20*64, miterLimit, ButtCap, nil, jm)
		v.Start(ToFixedP(100, cy))
		v.Line(ToFixedP(cx, cy))
		v.Line(ToFixedP(cx, 100))
		v.Stop(false)
		v.Draw()
		return img
	}
	miter := render(Miter, 10<<6)
	bevel := render(Bevel, 10<<6)
	clip := render(MiterClip, fixed.Int26_6(76)) // miter limit 1.2
	round := render(Round, 10<<6)

	// Corner geometry (half width 10): the convex side is bottom-right.
	// The miter tip is at (310,310), i.e. x+y=620; the bevel cuts along
	// x+y=610; MiterClip at 1.2 clips along x+y = 600+12*sqrt(2) ~= 617;
	// the round join arc has radius 10 around the corner.
	// (309,309) at x+y=618: inside the miter only.
	if alphaAt(miter, 309, 309) <= 128 {
		t.Error("miter join did not fill the miter triangle")
	}
	if alphaAt(bevel, 309, 309) != 0 {
		t.Error("bevel join filled past the bevel line")
	}
	if alphaAt(clip, 309, 309) != 0 {
		t.Error("miter-clip join filled past the clip line")
	}
	if alphaAt(round, 309, 309) != 0 {
		t.Error("round join filled past the arc")
	}
	// (306,306) at x+y=612: inside miter, clip and round, outside bevel.
	if alphaAt(clip, 306, 306) <= 128 {
		t.Error("miter-clip join did not fill up to the clip line")
	}
	if alphaAt(round, 306, 306) <= 128 {
		t.Error("round join did not fill the arc")
	}
	if alphaAt(bevel, 306, 306) != 0 {
		t.Error("bevel join filled past the bevel line")
	}
	// (304,304) at x+y=608 is inside all joins.
	for name, img := range map[string]*image.RGBA{"miter": miter, "bevel": bevel, "clip": clip, "round": round} {
		if alphaAt(img, 304, 304) <= 128 {
			t.Errorf("%s join did not fill the stroke body at the corner", name)
		}
	}
	// (312,312) at x+y=624 is beyond even the miter tip.
	for name, img := range map[string]*image.RGBA{"miter": miter, "bevel": bevel, "clip": clip, "round": round} {
		if alphaAt(img, 312, 312) != 0 {
			t.Errorf("%s join filled past the miter tip", name)
		}
	}
	// All joins fill the body of the stroke.
	for _, img := range []*image.RGBA{miter, bevel, clip, round} {
		if alphaAt(img, 200, 300) <= 128 || alphaAt(img, 300, 200) <= 128 {
			t.Error("join test: stroke body not filled")
		}
	}
}

// TestVarStrokeClosedPath strokes a closed rectangle with a varying width
// and checks the loop is joined all around with a hole in the middle.
func TestVarStrokeClosedPath(t *testing.T) {
	const wx, wy = 512, 512
	img, _, v := newVarStrokeImage(wx, wy)
	v.SetStroke(1<<6, 4<<6, RoundCap, nil, Miter) // caps must be ignored
	v.SetWidths(WidthPoint{Pos: 0, Width: 24}, WidthPoint{Pos: 0.5, Width: 8},
		WidthPoint{Pos: 1, Width: 24})
	v.Start(ToFixedP(100, 100))
	v.Line(ToFixedP(400, 100))
	v.Line(ToFixedP(400, 300))
	v.Line(ToFixedP(100, 300))
	v.Stop(true)
	v.Draw()

	if countFilled(img) == 0 {
		t.Fatal("closed path rendered nothing")
	}
	// The middle of the loop is a hole.
	if alphaAt(img, 250, 200) != 0 {
		t.Error("interior of closed loop is filled")
	}
	// The top edge carries the width of Pos 0/1 (24 -> half 12).
	if alphaAt(img, 250, 100) <= 128 {
		t.Error("top edge of closed loop not filled")
	}
	if alphaAt(img, 250, 100-14) != 0 || alphaAt(img, 250, 100+14) != 0 {
		t.Error("top edge of closed loop wider than its control width")
	}
	// The left edge midpoint is at Pos ~0.7: width = 8+(24-8)*0.4 = 14.4,
	// so (100,200) on the centerline is filled but 12 units out is empty.
	if alphaAt(img, 100, 200) <= 128 {
		t.Error("left edge of closed loop not filled")
	}
	if alphaAt(img, 100-12, 200) != 0 || alphaAt(img, 100+12, 200) != 0 {
		t.Error("left edge of closed loop wider than its interpolated width")
	}
}

// TestVarStrokeEdgeCases exercises degenerate inputs: negative widths,
// unsorted control points, zero length segments and paths, hairpin corners,
// zero widths and degenerate closed paths.
func TestVarStrokeEdgeCases(t *testing.T) {
	const wx, wy = 512, 512

	t.Run("NegativeWidthClamped", func(t *testing.T) {
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetWidths(WidthPoint{Pos: 0, Width: -30}, WidthPoint{Pos: 1, Width: 20})
		v.Start(ToFixedP(60, 256))
		v.Line(ToFixedP(450, 256))
		v.Stop(false)
		v.Draw()
		// Negative start width clamps to 0: the stroke tapers from the
		// center line, so columns near the start are (nearly) empty.
		if countFilled(img) == 0 {
			t.Fatal("nothing rendered")
		}
		if alphaAt(img, 62, 250) != 0 || alphaAt(img, 62, 262) != 0 {
			t.Error("negative width was not clamped to zero")
		}
	})

	t.Run("UnsortedControlPoints", func(t *testing.T) {
		render := func(wp ...WidthPoint) *image.RGBA {
			img, _, v := newVarStrokeImage(wx, wy)
			v.SetWidths(wp...)
			v.Start(ToFixedP(60, 256))
			v.Line(ToFixedP(450, 256))
			v.Stop(false)
			v.Draw()
			return img
		}
		a := render(WidthPoint{Pos: 1, Width: 30}, WidthPoint{Pos: 0, Width: 10},
			WidthPoint{Pos: 0.5, Width: 24})
		b := render(WidthPoint{Pos: 0, Width: 10}, WidthPoint{Pos: 0.5, Width: 24},
			WidthPoint{Pos: 1, Width: 30})
		for y := 0; y < wy; y++ {
			for x := 0; x < wx; x++ {
				if alphaAt(a, x, y) != alphaAt(b, x, y) {
					t.Fatalf("unsorted control points differ at (%d,%d)", x, y)
				}
			}
		}
	})

	t.Run("ZeroLengthSegments", func(t *testing.T) {
		render := func(extra bool) *image.RGBA {
			img, _, v := newVarStrokeImage(wx, wy)
			v.SetWidths(WidthPoint{Pos: 0, Width: 20}, WidthPoint{Pos: 1, Width: 20})
			v.Start(ToFixedP(60, 256))
			if extra {
				v.Line(ToFixedP(60, 256)) // zero length
				v.Line(ToFixedP(60, 256))
				v.CubeBezier(ToFixedP(60, 256), ToFixedP(60, 256), ToFixedP(60, 256)) // degenerate
			}
			v.Line(ToFixedP(450, 256))
			if extra {
				v.Line(ToFixedP(450, 256)) // zero length at the end
			}
			v.Stop(false)
			v.Draw()
			return img
		}
		a, b := render(true), render(false)
		if countFilled(a) == 0 {
			t.Fatal("nothing rendered")
		}
		for y := 0; y < wy; y++ {
			for x := 0; x < wx; x++ {
				if alphaAt(a, x, y) != alphaAt(b, x, y) {
					t.Fatalf("zero length segments changed the render at (%d,%d)", x, y)
				}
			}
		}
	})

	t.Run("ZeroLengthPath", func(t *testing.T) {
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetWidths(WidthPoint{Pos: 0, Width: 20}, WidthPoint{Pos: 1, Width: 20})
		v.Start(ToFixedP(100, 100))
		v.Line(ToFixedP(100, 100))
		v.Stop(false)
		v.Start(ToFixedP(200, 200))
		v.Stop(true)
		v.Draw()
		if n := countFilled(img); n != 0 {
			t.Fatalf("zero length path rendered %d pixels", n)
		}
	})

	t.Run("AllZeroWidths", func(t *testing.T) {
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetWidths(WidthPoint{Pos: 0, Width: 0}, WidthPoint{Pos: 1, Width: 0})
		v.Start(ToFixedP(60, 256))
		v.Line(ToFixedP(450, 256))
		v.Stop(false)
		v.Draw()
		if n := countFilled(img); n != 0 {
			t.Fatalf("zero width stroke rendered %d pixels", n)
		}
	})

	t.Run("HairpinCorner", func(t *testing.T) {
		// A near 180 degree turn must not explode with any join mode.
		for _, jm := range []JoinMode{Miter, MiterClip, Bevel, Round, Arc, ArcClip} {
			img, _, v := newVarStrokeImage(wx, wy)
			v.SetStroke(16*64, 4<<6, ButtCap, nil, jm)
			v.SetWidths(WidthPoint{Pos: 0, Width: 16}, WidthPoint{Pos: 1, Width: 4})
			v.Start(ToFixedP(100, 300))
			v.Line(ToFixedP(400, 300))
			v.Line(ToFixedP(100, 301)) // sharp reversal
			v.Stop(false)
			v.Draw()
			if countFilled(img) == 0 {
				t.Fatalf("join mode %d: nothing rendered", jm)
			}
		}
	})

	t.Run("ExtremelyNarrowCurve", func(t *testing.T) {
		// A curve much tighter than the stroke width must not panic or
		// produce runaway geometry.
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetWidths(WidthPoint{Pos: 0, Width: 60}, WidthPoint{Pos: 1, Width: 60})
		v.Start(ToFixedP(256, 400))
		v.CubeBezier(ToFixedP(260, 100), ToFixedP(252, 100), ToFixedP(256, 400))
		v.Stop(false)
		v.Draw()
		if countFilled(img) == 0 {
			t.Fatal("nothing rendered")
		}
	})

	t.Run("DegenerateClosedPath", func(t *testing.T) {
		// A two point "closed" path is stroked as an open line.
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetWidths(WidthPoint{Pos: 0, Width: 20}, WidthPoint{Pos: 1, Width: 20})
		v.Start(ToFixedP(100, 256))
		v.Line(ToFixedP(400, 256))
		v.Stop(true)
		v.Draw()
		if countFilled(img) == 0 {
			t.Fatal("nothing rendered")
		}
		if alphaAt(img, 250, 256) <= 128 {
			t.Error("degenerate closed path body not filled")
		}
	})

	t.Run("ImplicitStopOnStart", func(t *testing.T) {
		// A second Start without Stop strokes the first subpath.
		img, _, v := newVarStrokeImage(wx, wy)
		v.SetWidths(WidthPoint{Pos: 0, Width: 20}, WidthPoint{Pos: 1, Width: 20})
		v.Start(ToFixedP(60, 100))
		v.Line(ToFixedP(450, 100))
		v.Start(ToFixedP(60, 400))
		v.Line(ToFixedP(450, 400))
		v.Stop(false)
		v.Draw()
		if alphaAt(img, 250, 100) <= 128 || alphaAt(img, 250, 400) <= 128 {
			t.Error("implicit Stop on Start did not stroke both subpaths")
		}
	})
}

// TestVarStrokeQuadContinuity checks that a tapering width profile is
// continuous across line/quad/cubic segment boundaries: the rendered band
// has no gaps along a mixed path.
func TestVarStrokeQuadContinuity(t *testing.T) {
	const wx, wy = 512, 512
	img, _, v := newVarStrokeImage(wx, wy)
	v.SetStroke(1<<6, 4<<6, ButtCap, nil, Round)
	v.SetWidths(WidthPoint{Pos: 0, Width: 8}, WidthPoint{Pos: 0.5, Width: 28},
		WidthPoint{Pos: 1, Width: 8})
	v.Start(ToFixedP(60, 256))
	v.Line(ToFixedP(160, 256))
	v.QuadBezier(ToFixedP(260, 156), ToFixedP(360, 256))
	v.CubeBezier(ToFixedP(400, 296), ToFixedP(420, 216), ToFixedP(460, 256))
	v.Stop(false)
	v.Draw()

	// Walk the centerline of the mixed path and confirm coverage: the
	// stroke must be gapless across the segment boundaries.
	checkPts := [][2]float64{}
	for x := 60.0; x <= 160; x += 2 {
		checkPts = append(checkPts, [2]float64{x, 256})
	}
	for i := 0; i <= 64; i++ { // quad centerline
		t := float64(i) / 64
		mt := 1 - t
		checkPts = append(checkPts, [2]float64{
			mt*mt*160 + 2*mt*t*260 + t*t*360,
			mt*mt*256 + 2*mt*t*156 + t*t*256})
	}
	for i := 0; i <= 64; i++ { // cubic centerline
		p := cubicPt([2]float64{360, 256}, [2]float64{400, 296},
			[2]float64{420, 216}, [2]float64{460, 256}, float64(i)/64)
		checkPts = append(checkPts, p)
	}
	for i, p := range checkPts {
		// The path ends sit on the butt cap boundary; skip them.
		if p[0] <= 61 || p[0] >= 459 {
			continue
		}
		if alphaAt(img, int(p[0]+0.5), int(p[1]+0.5)) <= 128 {
			t.Fatalf("gap in variable width stroke at centerline point %d: %v", i, p)
		}
	}
}

// TestVarStrokeRender draws a demo sheet of variable width strokes for
// visual inspection, following the style of TestShapes.
func TestVarStrokeRender(t *testing.T) {
	const wx, wy = 600, 400
	img, scanner, v := newVarStrokeImage(wx, wy)
	scanner.SetColor(color.RGBA{0, 0, 128, 255})

	// Tapering line, butt caps.
	v.SetStroke(1<<6, 4<<6, ButtCap, nil, Bevel)
	v.SetWidths(WidthPoint{Pos: 0, Width: 30}, WidthPoint{Pos: 1, Width: 2})
	v.Start(ToFixedP(40, 50))
	v.Line(ToFixedP(560, 50))
	v.Stop(false)

	// Thick-thin-thick curve, round joins and caps.
	v.SetStroke(1<<6, 4<<6, RoundCap, nil, Round)
	v.SetWidths(WidthPoint{Pos: 0, Width: 6}, WidthPoint{Pos: 0.5, Width: 34},
		WidthPoint{Pos: 1, Width: 6})
	v.Start(ToFixedP(40, 130))
	v.CubeBezier(ToFixedP(200, 60), ToFixedP(400, 200), ToFixedP(560, 130))
	v.Stop(false)

	// Mixed path with miter-clip joins.
	v.SetStroke(1<<6, 2<<6, SquareCap, nil, MiterClip)
	v.SetWidths(WidthPoint{Pos: 0, Width: 24}, WidthPoint{Pos: 0.3, Width: 8},
		WidthPoint{Pos: 0.7, Width: 28}, WidthPoint{Pos: 1, Width: 10})
	v.Start(ToFixedP(40, 240))
	v.Line(ToFixedP(200, 240))
	v.QuadBezier(ToFixedP(300, 180), ToFixedP(360, 240))
	v.Line(ToFixedP(560, 240))
	v.Stop(false)

	// Closed loop.
	v.SetStroke(1<<6, 4<<6, RoundCap, nil, Miter)
	v.SetWidths(WidthPoint{Pos: 0, Width: 26}, WidthPoint{Pos: 0.5, Width: 6},
		WidthPoint{Pos: 1, Width: 26})
	v.Start(ToFixedP(120, 330))
	v.Line(ToFixedP(300, 330))
	v.Line(ToFixedP(300, 380))
	v.Line(ToFixedP(120, 380))
	v.Stop(true)

	// A brush-like signature stroke.
	v.SetStroke(1<<6, 4<<6, RoundCap, nil, Round)
	v.SetWidths(WidthPoint{Pos: 0, Width: 2}, WidthPoint{Pos: 0.25, Width: 18},
		WidthPoint{Pos: 0.6, Width: 4}, WidthPoint{Pos: 1, Width: 22})
	v.Start(ToFixedP(360, 370))
	v.CubeBezier(ToFixedP(420, 300), ToFixedP(500, 390), ToFixedP(570, 320))
	v.Stop(false)

	v.Draw()
	if err := SaveToPngFile("testdata/varstroke.png", img); err != nil {
		t.Error(err)
	}
}

// checkExtent verifies the path extent against expected float bounds.
func checkExtent(t *testing.T, ext fixed.Rectangle26_6, minX, minY, maxX, maxY, tol float64) {
	t.Helper()
	got := [4]float64{float64(ext.Min.X) / 64, float64(ext.Min.Y) / 64,
		float64(ext.Max.X) / 64, float64(ext.Max.Y) / 64}
	want := [4]float64{minX, minY, maxX, maxY}
	for i := range got {
		if math.Abs(got[i]-want[i]) > tol {
			t.Fatalf("extent %v, want %v (tol %v)", got, want, tol)
		}
	}
}
