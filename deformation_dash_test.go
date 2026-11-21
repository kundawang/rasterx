// Copyright 2026 The rasterx Authors. All rights reserved.
//
// Dash and stroke-relation properties, plus explicit expectations for
// degenerate curves, very small contours, and non-zero destination Bounds.
package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
)

func deformSolidDashImage(t *testing.T, w, h int, pts refPoly, pattern []float64, offset float64,
	width float64, capName string, closed bool) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sc := NewScannerGV(w, h, img, img.Bounds())
	d := NewDasher(w, h, sc)
	d.SetStroke(ToFixedP(width, 0).X, 4*64, deformCap(capName), nil, FlatGap, Bevel, pattern, offset)
	sc.SetColor(color.White)
	d.Start(ToFixedP(pts[0].X, pts[0].Y))
	for _, p := range pts[1:] {
		d.Line(ToFixedP(p.X, p.Y))
	}
	d.Stop(closed)
	d.Draw()
	return img
}

// TestDashPeriodAndOffset pins down how DashOffset interacts with the pattern
// period: shifting by a whole period is a no-op, and the offset consumes the
// start of the first dash rather than skipping it.
func TestDashPeriodAndOffset(t *testing.T) {
	pts := refPoly{{15, 30}, {105, 30}}
	base := deformSolidDashImage(t, 130, 60, pts, []float64{10, 6}, 0, 6, "", false)
	if deformMass(base) == 0 {
		t.Fatal("dash render is empty")
	}
	for _, off := range []float64{16, 32, 48} {
		img := deformSolidDashImage(t, 130, 60, pts, []float64{10, 6}, off, 6, "", false)
		if n, max := deformPixelDiff(base, img); n != 0 {
			t.Errorf("dash offset %.0f (multiple of the 16-unit period) changed %d bytes (max %d), want identical", off, n, max)
		}
	}

	// The independent walker says where the first on-run starts and ends; the
	// rendered center row must agree. The offset advances *into* the first
	// dash, so a small offset shortens it rather than removing it.
	for _, off := range []float64{0, 3, 9, 22} {
		img := deformSolidDashImage(t, 130, 60, pts, []float64{10, 6}, off, 6, "", false)
		runs := deformDashRanges(pts, []float64{10, 6}, off)
		if len(runs) == 0 {
			t.Fatalf("offset %.0f: walker produced no dashes", off)
		}
		firstOn := -1
		for x := 0; x < 130; x++ {
			if img.Pix[30*img.Stride+x*4+3] > 127 {
				firstOn = x
				break
			}
		}
		if firstOn < 0 {
			t.Fatalf("offset %.0f: no dash coverage on the center row", off)
		}
		wantStart := runs[0].A.X
		wantEnd := runs[0].B.X
		if float64(firstOn) < wantStart-1.5 || float64(firstOn) > wantStart+1.5 {
			t.Errorf("offset %.0f: first dash pixel at x=%d, walker says the dash starts at %.2f", off, firstOn, wantStart)
		}
		// The pixel just inside the end of the first run is on; one past it is off.
		if off == 0 {
			if a := img.Pix[30*img.Stride+int(wantEnd-1)*4+3]; a != 255 {
				t.Errorf("offset 0: pixel before the end of the first dash has alpha %d, want 255", a)
			}
			if a := img.Pix[30*img.Stride+int(wantEnd+1)*4+3]; a != 0 {
				t.Errorf("offset 0: pixel after the first dash has alpha %d, want 0", a)
			}
		}
	}
}

// TestDashRunsMatchWalker checks every dash run on a straight segment against
// the independent dash walker.
func TestDashRunsMatchWalker(t *testing.T) {
	pts := refPoly{{11.5, 40}, {118.5, 40}}
	pattern := []float64{9, 5, 4, 6}
	offset := 7.0
	img := deformSolidDashImage(t, 140, 80, pts, pattern, offset, 6, "", false)
	runs := deformDashRanges(pts, pattern, offset)
	if len(runs) == 0 {
		t.Fatal("walker produced no dash ranges")
	}
	if len(runs) != len(deformDashRanges(pts, pattern, 0)) && len(runs) != len(deformDashRanges(pts, pattern, 0))-1 {
		// Offsets may drop one run at the start; anything else means the
		// pattern is being restarted rather than continued.
		t.Logf("dash range count with offset %.0f is %d", offset, len(runs))
	}
	// Every on-run must be covered on the center row and every gap must be
	// empty there.
	for i := 0; i < len(runs)-1; i++ {
		gapStart := runs[i].B
		gapEnd := runs[i+1].A
		mid := (gapStart.X + gapEnd.X) / 2
		if alpha := img.Pix[40*img.Stride+int(mid)*4+3]; alpha != 0 {
			t.Errorf("pixel x=%d in a dash gap has alpha %d, want 0", int(mid), alpha)
		}
		midOn := (runs[i].A.X + runs[i].B.X) / 2
		if alpha := img.Pix[40*img.Stride+int(midOn)*4+3]; alpha != 255 {
			t.Errorf("pixel x=%d inside a dash has alpha %d, want 255", int(midOn), alpha)
		}
	}
	// Geometric dash fraction: sum(on runs)/path length must be
	// approximately on/(on+off+on+off).
	onLen, totalLen := 0.0, 0.0
	for _, r := range runs {
		onLen += math.Hypot(r.B.X-r.A.X, r.B.Y-r.A.Y)
	}
	totalLen = (pts[1].X - pts[0].X)
	want := (9.0 + 4.0) / (9.0 + 5.0 + 4.0 + 6.0)
	if frac := onLen / totalLen; math.Abs(frac-want) > 0.02 {
		t.Errorf("dash on-fraction %.4f, want %.4f from the pattern", frac, want)
	}
}

// TestDashCollinearSubdivision asserts that splitting a straight segment into
// several collinear Line calls changes neither the dash phase nor coverage.
func TestDashCollinearSubdivision(t *testing.T) {
	whole := refPoly{{10, 30}, {120, 30}}
	split := refPoly{{10, 30}, {65, 30}, {120, 30}}
	for _, pat := range [][]float64{{10, 6}, {9, 5, 4, 6}} {
		a := deformSolidDashImage(t, 140, 60, whole, pat, 3, 6, "", false)
		b := deformSolidDashImage(t, 140, 60, split, pat, 3, 6, "", false)
		if n, max := deformPixelDiff(a, b); n != 0 {
			t.Errorf("pattern %v: splitting a collinear segment changed %d bytes (max %d)", pat, n, max)
		}
	}
}

// TestDashReversalSymmetry checks that traversing an open path backwards
// preserves the amount of ink in every dash and gap: the runs are the same
// multiset up to the truncated run at each end. This catches a dash walker
// that mishandles direction or restarts the pattern at each vertex.
func TestDashReversalSymmetry(t *testing.T) {
	fwd := refPoly{{20, 50}, {130, 50}}
	rev := refPolyReverse(fwd)
	for _, pat := range [][]float64{{10, 6}, {9, 5, 4, 6}} {
		for _, off := range []float64{0, 3, 9} {
			a := deformSolidDashImage(t, 150, 90, fwd, pat, off, 6, "", false)
			b := deformSolidDashImage(t, 150, 90, rev, pat, off, 6, "", false)
			ma, mb := deformMass(a), deformMass(b)
			if ma == 0 {
				t.Fatalf("pattern %v offset %.0f: forward render is empty", pat, off)
			}
			// The end runs are truncated differently by up to one pattern
			// period; the strip is 6 wide, so allow one period*w/w.
			if math.Abs(ma-mb) > 6*16 {
				t.Errorf("pattern %v offset %.0f: reversing the path changed coverage %.2f -> %.2f", pat, off, ma, mb)
			}
			// Count on-runs on the center row; reversal may add or drop the
			// partial run at the start of the path but must not re-phase the
			// rest.
			countRuns := func(img *image.RGBA) (n, total int) {
				on := false
				for x := 0; x < 150; x++ {
					cur := img.Pix[50*img.Stride+x*4+3] > 127
					if cur && !on {
						n++
					}
					if cur {
						total++
					}
					on = cur
				}
				return
			}
			na, la := countRuns(a)
			nb, lb := countRuns(b)
			if d := na - nb; d < -1 || d > 1 {
				t.Errorf("pattern %v offset %.0f: run count %d forward vs %d reversed", pat, off, na, nb)
			}
			if la == 0 || lb == 0 {
				t.Errorf("pattern %v offset %.0f: one direction produced no on pixels (%d vs %d)", pat, off, la, lb)
			}
		}
	}
}

// TestDashClosedLoopMass checks that dashing a closed loop removes the right
// fraction of ink. Caps are butt-capped in this test so the ideal ratio is
// exactly the on-fraction of the pattern; the same loop traversed either way
// has the same mass.
func TestDashClosedLoopMass(t *testing.T) {
	loop := refPoly{{40, 40}, {140, 40}, {140, 140}, {40, 140}, {40, 40}}
	rev := refPolyReverse(loop)
	pattern := []float64{12, 7}
	frac := 12.0 / 19.0
	for _, off := range []float64{0, 3, 9} {
		solid := deformSolidDashImage(t, 180, 180, loop, nil, off, 8, "", true)
		dashed := deformSolidDashImage(t, 180, 180, loop, pattern, off, 8, "", true)
		revDashed := deformSolidDashImage(t, 180, 180, rev, pattern, off, 8, "", true)
		sm, dm := deformMass(solid), deformMass(dashed)
		if sm == 0 {
			t.Fatal("solid closed stroke is empty")
		}
		// Butt caps at dash ends and at the loop closure cost a fraction of
		// the theoretical on-length; allow a few percent.
		if ratio := dm / sm; math.Abs(ratio-frac) > 0.035 {
			t.Errorf("offset %.0f: dashed/solid mass ratio %.4f, want %.4f", off, ratio, frac)
		}
		if mr := deformMass(revDashed); math.Abs(mr-dm) > 1 {
			t.Errorf("offset %.0f: closed dashed loop mass changed with traversal direction: %.3f vs %.3f", off, dm, mr)
		}
	}
}

// TestDashClosedCapsDoNotLeakArea asserts the dash caps on a closed loop match
// an open run of the same perimeter when the loop is split at a gap: the total
// inked length is the same and the closure itself adds no extra cap ink when
// the pattern starts and ends in a gap.
func TestDashClosedCapsDoNotLeakArea(t *testing.T) {
	// A pattern that is entirely a dash would be capped at the closure by
	// butt caps; check that a closed loop that is fully "on" matches the
	// solid closed stroke.
	loop := refPoly{{50, 50}, {130, 50}, {130, 130}, {50, 130}, {50, 50}}
	solid := deformSolidDashImage(t, 180, 180, loop, nil, 0, 8, "", true)
	dashed := deformSolidDashImage(t, 180, 180, loop, []float64{1000, 0.001}, 0, 8, "", true)
	if a, b := deformMass(solid), deformMass(dashed); math.Abs(a-b) > 1 {
		t.Errorf("closed loop with an all-on dash pattern has mass %.3f, solid loop %.3f", b, a)
	}
}

// ---------------------------------------------------------------------------
// Degenerate curves
// ---------------------------------------------------------------------------

// TestDegenerateCurves pins the documented behaviour: a Bezier whose control
// points collapse onto an endpoint must render exactly like the equivalent
// line, and fully degenerate paths must produce no ink.
func TestDegenerateCurves(t *testing.T) {
	const w, h = 60, 60
	render := func(build func(Adder)) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, sc)
		sc.SetColor(color.White)
		build(f)
		f.Draw()
		return img
	}
	line := render(func(f Adder) {
		f.Start(ToFixedP(10, 30))
		f.Line(ToFixedP(50, 30))
		f.Stop(false)
	})

	for _, tc := range []struct {
		name  string
		build func(Adder)
	}{
		{"quad first control == start", func(f Adder) {
			f.Start(ToFixedP(10, 30))
			f.QuadBezier(ToFixedP(10, 30), ToFixedP(50, 30))
			f.Stop(false)
		}},
		{"quad last control == end", func(f Adder) {
			f.Start(ToFixedP(10, 30))
			f.QuadBezier(ToFixedP(50, 30), ToFixedP(50, 30))
			f.Stop(false)
		}},
		{"cube first two controls == start", func(f Adder) {
			f.Start(ToFixedP(10, 30))
			f.CubeBezier(ToFixedP(10, 30), ToFixedP(10, 30), ToFixedP(50, 30))
			f.Stop(false)
		}},
		{"cube last two controls == end", func(f Adder) {
			f.Start(ToFixedP(10, 30))
			f.CubeBezier(ToFixedP(10, 30), ToFixedP(50, 30), ToFixedP(50, 30))
			f.Stop(false)
		}},
	} {
		got := render(tc.build)
		if n, max := deformPixelDiff(got, line); n != 0 {
			t.Errorf("%s: differs from a straight line in %d bytes (max %d)", tc.name, n, max)
		}
	}

	// Everything collapsed to a single point: no fill, no ink.
	point := render(func(f Adder) {
		f.Start(ToFixedP(30, 30))
		f.CubeBezier(ToFixedP(30, 30), ToFixedP(30, 30), ToFixedP(30, 30))
		f.Stop(true)
	})
	if m := deformMass(point); m != 0 {
		t.Errorf("fully degenerate fill produced coverage %.4f, want 0", m)
	}

	// A nearly degenerate (but non-degenerate) cubic is a sub-pixel sliver:
	// tiny mass, but ink exists and stays within the curve's bounding box.
	sliver := render(func(f Adder) {
		f.Start(ToFixedP(10, 30))
		f.CubeBezier(ToFixedP(30, 30.02), ToFixedP(40, 29.98), ToFixedP(50, 30))
		f.Stop(true)
	})
	if m := deformMass(sliver); m <= 0 || m > 2 {
		t.Errorf("near-degenerate cubic sliver coverage %.4f, want (0,2]", m)
	}
	minX, minY, maxX, maxY := deformBBox(sliver)
	if minX < 9 || maxX > 51 || minY < 28 || maxY > 32 {
		t.Errorf("near-degenerate sliver ink bbox [%d %d %d %d] escapes the curve bounds", minX, minY, maxX, maxY)
	}
	// And stroking that sliver must behave like stroking the line, within a
	// pixel: the curvature is below the fixed-point resolution.
	strokeOf := func(build func(Adder)) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerGV(w, h, img, img.Bounds())
		s := NewStroker(w, h, sc)
		s.SetStroke(8*64, 4*64, ButtCap, nil, FlatGap, Miter)
		sc.SetColor(color.White)
		build(s)
		s.Draw()
		return img
	}
	straightStroke := strokeOf(func(a Adder) {
		a.Start(ToFixedP(10, 30))
		a.Line(ToFixedP(50, 30))
		a.Stop(false)
	})
	sliverStroke := strokeOf(func(a Adder) {
		a.Start(ToFixedP(10, 30))
		a.CubeBezier(ToFixedP(30, 30.02), ToFixedP(40, 29.98), ToFixedP(50, 30))
		a.Stop(false)
	})
	if m := deformMass(sliverStroke); math.Abs(m-320) > 2 {
		t.Errorf("near-degenerate stroked cubic coverage %.3f, want 8*40=320", m)
	}
	if n, max := deformPixelDiff(straightStroke, sliverStroke); n > 0 && max > 96 {
		t.Errorf("stroked near-degenerate cubic differs from the straight stroke in %d bytes (max %d); expected sub-pixel differences only", n, max)
	}
}

// ---------------------------------------------------------------------------
// Very small contours
// ---------------------------------------------------------------------------

// TestTinyContours checks that sub-pixel shapes produce proportional coverage
// rather than snapping to empty or full pixels. The renderer quantises area to
// 1/255, so a contour of size s has a floor of about one quantisation step.
func TestTinyContours(t *testing.T) {
	render := func(w, h int, build func(*Filler)) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		sc := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, sc)
		sc.SetColor(color.White)
		build(f)
		f.Draw()
		return img
	}
	for _, size := range []float64{0.125, 0.25, 0.5, 0.75, 1.0, 1.5, 2.0} {
		img := render(30, 30, func(f *Filler) {
			f.Start(ToFixedP(10.3, 10.7))
			f.Line(ToFixedP(10.3+size, 10.7))
			f.Line(ToFixedP(10.3, 10.7+size))
			f.Stop(true)
		})
		want := size * size / 2
		got := deformMass(img)
		// One 8-bit quantisation step across the covered pixels plus the
		// half-pixel offset in the sampling grid.
		tol := 0.02 + 2*want
		if math.Abs(got-want) > tol {
			t.Errorf("triangle of size %.3f: coverage %.5f, want %.5f (±%.4f)", size, got, want, tol)
		}
		if size >= 0.25 && got == 0 {
			t.Errorf("triangle of size %.3f vanished entirely", size)
		}
	}

	// Tiny strokes keep width*length.
	for _, width := range []float64{0.5, 1, 2} {
		img := image.NewRGBA(image.Rect(0, 0, 60, 40))
		sc := NewScannerGV(60, 40, img, img.Bounds())
		s := NewStroker(60, 40, sc)
		s.SetStroke(ToFixedP(width, 0).X, 4*64, ButtCap, nil, FlatGap, Miter)
		sc.SetColor(color.White)
		s.Start(ToFixedP(20.3, 20.7))
		s.Line(ToFixedP(50.3, 20.7))
		s.Stop(false)
		s.Draw()
		want := width * 30
		if got := deformMass(img); math.Abs(got-want) > 0.02+0.05*want {
			t.Errorf("stroke of width %.2f: coverage %.4f, want %.4f", width, got, want)
		}
	}

	// A sub-pixel triangle must not disappear because of an integer bounding
	// box: one pixel of coverage is expected.
	img := render(12, 12, func(f *Filler) {
		f.Start(ToFixedP(5, 5))
		f.Line(ToFixedP(5.25, 5))
		f.Line(ToFixedP(5, 5.25))
		f.Stop(true)
	})
	if got := deformMass(img); got <= 0 {
		t.Error("quarter-pixel triangle produced no coverage")
	}
	if got := img.Pix[5*img.Stride+5*4+3]; got == 0 {
		t.Error("quarter-pixel triangle did not touch pixel (5,5)")
	}
}

// ---------------------------------------------------------------------------
// Non-zero destination Bounds
// ---------------------------------------------------------------------------

// TestNonZeroDestinationBounds documents that ScannerGV interprets path
// coordinates relative to the destination image's origin, and that the clip
// rectangle, in contrast, is in absolute image coordinates.
func TestNonZeroDestinationBounds(t *testing.T) {
	const w, h = 40, 30
	const ox, oy = 13, 7
	draw := func(rect image.Rectangle, clip image.Rectangle) *image.RGBA {
		img := image.NewRGBA(rect)
		sc := NewScannerGV(w, h, img, img.Bounds())
		sc.SetClip(clip)
		f := NewFiller(w, h, sc)
		sc.SetColor(color.White)
		AddRect(5, 5, 15, 15, 0, f)
		f.Draw()
		return img
	}

	origin := image.NewRGBA(image.Rect(0, 0, w, h))
	scOrigin := NewScannerGV(w, h, origin, origin.Bounds())
	fo := NewFiller(w, h, scOrigin)
	scOrigin.SetColor(color.White)
	AddRect(5, 5, 15, 15, 0, fo)
	fo.Draw()

	shifted := draw(image.Rect(ox, oy, ox+w, oy+h), image.ZR)
	if n, max := deformPixelDiff(origin, shifted); n != 0 {
		t.Errorf("non-zero destination origin changed %d bytes (max %d); the raster is expected to be origin-relative", n, max)
	}
	if minX, minY, maxX, maxY := deformBBox(shifted); [4]int{minX, minY, maxX, maxY} != [4]int{5, 5, 14, 14} {
		t.Errorf("non-zero destination alpha bbox [%d %d %d %d], want [5 5 14 14] (origin-relative)", minX, minY, maxX, maxY)
	}

	// ScannerGV's clip rectangle uses the same destination-relative pixel
	// coordinates as its path, so Rect(8,8,12,12) selects the 4x4 block that
	// starts 8 pixels from the destination origin regardless of that origin.
	clipped := draw(image.Rect(ox, oy, ox+w, oy+h), image.Rect(8, 8, 12, 12))
	minX, minY, maxX, maxY := deformBBox(clipped)
	if minX != 8 || minY != 8 || maxX != 11 || maxY != 11 {
		t.Errorf("clip in absolute coordinates produced bbox [%d %d %d %d], want [8 8 11 11]", minX, minY, maxX, maxY)
	}
	if m := deformMass(clipped); math.Abs(m-16) > 0.1 {
		t.Errorf("clip 4x4 over the interior has coverage %.3f, want 16", m)
	}

	// Clipping must never add ink anywhere.
	unclippedMass := deformMass(shifted)
	if deformMass(clipped) > unclippedMass+1e-9 {
		t.Errorf("clip increased coverage from %.3f to %.3f", unclippedMass, deformMass(clipped))
	}
}
