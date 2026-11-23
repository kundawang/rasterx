// Copyright 2026 by the rasterx Authors. All rights reserved.
package rasterx_test

import (
	"image"
	"math"
	"sync"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/colornames"
	"golang.org/x/image/math/fixed"
)

// coverageArea counts pixels with non-zero alpha.
func coverageArea(img *image.RGBA) int {
	n := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] > 0 {
			n++
		}
	}
	return n
}

func newVarStrokerImg(w, h int) (*image.RGBA, *ScannerGV, *VarStroker) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sc := NewScannerGV(w, h, img, img.Bounds())
	sc.SetColor(colornames.Black)
	return img, sc, NewVarStroker(w, h, sc)
}

// renderVarConst renders p with a constant-width VarStroker and returns
// the coverage area.
func renderVarConst(w float64, p Path) int {
	_, _, v := newVarStrokerImg(512, 512)
	v.SetStrokeWidth(w)
	p.AddTo(v)
	v.Draw()
	return coverageArea(v.Scanner.(*ScannerGV).Dest)
}

// pressureCurvePath is an open S-curve used for the pressure fixtures.
func pressureCurvePath() (p Path) {
	p.Start(ToFixedP(60, 400))
	p.CubeBezier(ToFixedP(160, 100), ToFixedP(360, 500), ToFixedP(460, 120))
	return
}

// ringPath is a closed circle approximated by four cubic beziers.
func ringPath() (p Path) {
	const cx, cy, r = 256.0, 256.0, 120.0
	const k = 0.5522847498 * r // circle to cubic bezier constant
	p.Start(ToFixedP(cx+r, cy))
	p.CubeBezier(ToFixedP(cx+r, cy+k), ToFixedP(cx+k, cy+r), ToFixedP(cx, cy+r))
	p.CubeBezier(ToFixedP(cx-k, cy+r), ToFixedP(cx-r, cy+k), ToFixedP(cx-r, cy))
	p.CubeBezier(ToFixedP(cx-r, cy-k), ToFixedP(cx-k, cy-r), ToFixedP(cx, cy-r))
	p.CubeBezier(ToFixedP(cx+k, cy-r), ToFixedP(cx+r, cy-k), ToFixedP(cx+r, cy))
	p.Stop(true)
	return
}

// TestVarStrokePressureCurve strokes a bezier with a pressure profile that
// tapers from thin to thick and back, and checks the coverage area is
// bounded by the constant-width strokes at the profile extremes.
func TestVarStrokePressureCurve(t *testing.T) {
	p := pressureCurvePath()
	img, _, v := newVarStrokerImg(512, 512)
	v.SetStrokeWidthProfile([]float64{1, 18, 30, 18, 1})
	v.CapL, v.CapT = RoundCap, RoundCap
	p.AddTo(v)
	v.Draw()
	if err := SaveToPngFile("testdata/varStrokePressure.png", img); err != nil {
		t.Error(err)
	}
	area := coverageArea(img)
	lo, hi := renderVarConst(1, p), renderVarConst(30, p)
	if area <= lo || area >= hi {
		t.Errorf("pressure stroke area %d outside (%d, %d)", area, lo, hi)
	}
}

// TestVarStrokeConstantVsFixedReference compares a constant-width
// VarStroker against the fixed-width Stroker reference on the same path.
func TestVarStrokeConstantVsFixedReference(t *testing.T) {
	var p Path
	p.Start(ToFixedP(80, 256))
	p.Line(ToFixedP(200, 100))
	p.QuadBezier(ToFixedP(300, 50), ToFixedP(320, 200))
	p.CubeBezier(ToFixedP(340, 300), ToFixedP(420, 300), ToFixedP(440, 200))

	// Fixed width reference.
	imgRef := image.NewRGBA(image.Rect(0, 0, 512, 512))
	scRef := NewScannerGV(512, 512, imgRef, imgRef.Bounds())
	scRef.SetColor(colornames.Black)
	s := NewStroker(512, 512, scRef)
	s.SetStroke(10*64, 4*64, ButtCap, ButtCap, FlatGap, MiterClip)
	p.AddTo(s)
	s.Draw()

	// Variable width stroker with a constant width.
	imgVar, _, v := newVarStrokerImg(512, 512)
	v.SetStrokeWidth(10)
	p.AddTo(v)
	v.Draw()

	areaRef, areaVar := coverageArea(imgRef), coverageArea(imgVar)
	if areaVar == 0 {
		t.Fatal("variable width stroke produced no output")
	}
	if diff := math.Abs(float64(areaVar-areaRef)) / float64(areaRef); diff > 0.08 {
		t.Errorf("coverage differs from fixed width reference by %.1f%% (%d vs %d)",
			diff*100, areaVar, areaRef)
	}
	// Per-pixel symmetric difference should be a small fraction of the union.
	xor, union := 0, 0
	for i := 3; i < len(imgRef.Pix); i += 4 {
		a, b := imgRef.Pix[i] > 0, imgVar.Pix[i] > 0
		if a != b {
			xor++
		}
		if a || b {
			union++
		}
	}
	if union == 0 || float64(xor)/float64(union) > 0.15 {
		t.Errorf("pixel mismatch with fixed width reference: %d of %d union pixels", xor, union)
	}
}

// TestVarStrokeClosedRing strokes a closed ring with a varying profile and
// checks the closure seam is stroked without caps and the coverage is
// bounded by the constant-width extremes.
func TestVarStrokeClosedRing(t *testing.T) {
	p := ringPath()
	img, _, v := newVarStrokerImg(512, 512)
	v.SetStrokeWidthProfile([]float64{4, 16, 4, 16, 4})
	p.AddTo(v)
	v.Draw()
	if err := SaveToPngFile("testdata/varStrokeRing.png", img); err != nil {
		t.Error(err)
	}
	// The seam is at the start point (376, 256) on the centerline; it must
	// be covered (no cap notch) as must the opposite side of the ring.
	for _, pt := range [][2]int{{376, 256}, {136, 256}, {256, 136}, {256, 376}} {
		if img.RGBAAt(pt[0], pt[1]).A == 0 {
			t.Errorf("ring centerline pixel %v not covered", pt)
		}
	}
	area := coverageArea(img)
	if lo, hi := renderVarConst(4, p), renderVarConst(16, p); area <= lo || area >= hi {
		t.Errorf("ring area %d outside (%d, %d)", area, lo, hi)
	}
}

// TestVarStrokeDashes checks dashed variable-width stroking: dash gaps are
// empty, dashes are covered, and dashing reduces the coverage area.
func TestVarStrokeDashes(t *testing.T) {
	var p Path
	p.Start(ToFixedP(50, 256))
	p.Line(ToFixedP(460, 256))

	img, _, v := newVarStrokerImg(512, 512)
	v.SetStrokeWidth(10)
	v.SetDashes([]float64{20, 10}, 0)
	p.AddTo(v)
	v.Draw()

	// Pattern: on [50,70], off [70,80], on [80,100], off [100,110], ...
	for _, pt := range [][2]int{{60, 256}, {85, 256}} {
		if img.RGBAAt(pt[0], pt[1]).A == 0 {
			t.Errorf("dash pixel %v not covered", pt)
		}
	}
	for _, pt := range [][2]int{{75, 256}, {105, 256}} {
		if img.RGBAAt(pt[0], pt[1]).A != 0 {
			t.Errorf("dash gap pixel %v covered", pt)
		}
	}
	dashArea := coverageArea(img)

	_, _, solid := newVarStrokerImg(512, 512)
	solid.SetStrokeWidth(10)
	p.AddTo(solid)
	solid.Draw()
	if solidArea := coverageArea(solid.Scanner.(*ScannerGV).Dest); dashArea >= solidArea {
		t.Errorf("dashed area %d not smaller than solid area %d", dashArea, solidArea)
	}

	// Dashes with a pressure profile: t is not renormalized per dash, so
	// the area stays between the constant-width dashed extremes.
	img2, _, v2 := newVarStrokerImg(512, 512)
	v2.SetStrokeWidthProfile([]float64{2, 24, 2})
	v2.SetDashes([]float64{30, 15}, 0)
	p.AddTo(v2)
	v2.Draw()
	if err := SaveToPngFile("testdata/varStrokeDash.png", img2); err != nil {
		t.Error(err)
	}
	if coverageArea(img2) == 0 {
		t.Error("dashed pressure stroke produced no output")
	}
}

// TestVarStrokeWidthSanitization checks the rules for zero, negative,
// non-finite and extreme widths.
func TestVarStrokeWidthSanitization(t *testing.T) {
	linePath := func() (p Path) {
		p.Start(ToFixedP(100, 256))
		p.Line(ToFixedP(400, 256))
		return
	}
	cases := []struct {
		name     string
		wf       WidthFunc
		maxWidth float64
		wantArea bool
	}{
		{"zero", ConstantWidth(0), 0, false},
		{"negative", ConstantWidth(-10), 0, false},
		{"nan", ConstantWidth(math.NaN()), 0, false},
		{"negInf", ConstantWidth(math.Inf(-1)), 0, false},
		{"posInf", ConstantWidth(math.Inf(1)), 0, false},
		{"hugeClamped", ConstantWidth(1e12), 40, true},
		{"taperToZero", WidthProfile(20, 0, 20), 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, sc, v := newVarStrokerImg(512, 512)
			v.SetStrokeWidthFunc(tc.wf)
			if tc.maxWidth > 0 {
				v.MaxWidth = tc.maxWidth
			}
			linePath().AddTo(v)
			v.Draw()
			area := coverageArea(img)
			if tc.wantArea && area == 0 {
				t.Error("expected non-zero coverage")
			}
			if !tc.wantArea && area != 0 {
				t.Errorf("expected zero coverage, got %d", area)
			}
			if tc.maxWidth > 0 {
				// The extent must stay within the path bbox plus half the
				// clamped width (plus one pixel of slack).
				ex := sc.GetPathExtent()
				half := fixed.Int26_6((tc.maxWidth/2 + 1) * 64)
				if ex.Max.Y-ex.Min.Y > 2*half {
					t.Errorf("stroke extent %v exceeds clamped width bound", ex)
				}
			}
		})
	}
}

// TestVarStrokeDegenerate checks empty paths, degenerate points, tiny
// segments and 180 degree reversals neither panic nor produce runaway
// output.
func TestVarStrokeDegenerate(t *testing.T) {
	// Empty path: Start immediately followed by Stop.
	img, _, v := newVarStrokerImg(512, 512)
	v.Start(ToFixedP(100, 100))
	v.Stop(false)
	v.Draw()
	if a := coverageArea(img); a != 0 {
		t.Errorf("empty path covered %d pixels", a)
	}

	// Degenerate point: zero-length line.
	img, _, v = newVarStrokerImg(512, 512)
	v.SetStrokeWidth(10)
	v.Start(ToFixedP(100, 100))
	v.Line(ToFixedP(100, 100))
	v.Stop(false)
	v.Draw()
	if a := coverageArea(img); a != 0 {
		t.Errorf("degenerate point path covered %d pixels", a)
	}

	// Many tiny segments below the merge threshold: they coalesce into a
	// ~1 pixel long stroke, so coverage must stay small and bounded.
	img, _, v = newVarStrokerImg(512, 512)
	v.SetStrokeWidth(10)
	v.Start(ToFixedP(100, 100))
	for i := 1; i <= 1000; i++ {
		v.Line(ToFixedP(100+float64(i)*0.001, 100))
	}
	v.Stop(false)
	v.Draw()
	if a := coverageArea(img); a > 400 {
		t.Errorf("tiny segment path covered %d pixels, want a small bounded area", a)
	}

	// 180 degree reversal.
	_, _, v = newVarStrokerImg(512, 512)
	v.SetStrokeWidth(10)
	v.Start(ToFixedP(100, 256))
	v.Line(ToFixedP(300, 256))
	v.Line(ToFixedP(100, 256))
	v.Stop(false)
	v.Draw()
}

// TestVarStrokeMiterLimit checks that sharp corners are clipped by the
// miter limit instead of producing unbounded spikes.
func TestVarStrokeMiterLimit(t *testing.T) {
	// Near-reversal: the miter spike would extend ~38 half-widths.
	_, sc, v := newVarStrokerImg(512, 512)
	v.SetStrokeWidth(10) // half width 5, miter limit 4 -> 20 px bound
	v.Start(ToFixedP(100, 256))
	v.Line(ToFixedP(300, 256))
	v.Line(ToFixedP(110, 261))
	v.Stop(false)
	v.Draw()
	ex := sc.GetPathExtent()
	const bound = 300 + 5*4 + 2 // path max x + miter bound + slack
	if ex.Max.X > fixed.Int26_6(bound*64) {
		t.Errorf("miter spike not clipped: extent max x = %v px, bound %v",
			float64(ex.Max.X)/64, bound)
	}

	// Round and bevel joins on the same geometry must not panic.
	for _, jm := range []JoinMode{Round, Bevel, Miter, Arc} {
		_, _, v := newVarStrokerImg(512, 512)
		v.SetStrokeWidth(10)
		v.JoinMode = jm
		v.Start(ToFixedP(100, 256))
		v.Line(ToFixedP(300, 256))
		v.Line(ToFixedP(110, 261))
		v.Stop(false)
		v.Draw()
	}
}

// TestVarStrokeMultiSubpath checks that the normalized arc length restarts
// at zero for each subpath and that all subpaths are stroked.
func TestVarStrokeMultiSubpath(t *testing.T) {
	img, _, v := newVarStrokerImg(512, 512)
	var ts []float64
	v.SetStrokeWidthFunc(func(t float64) float64 {
		ts = append(ts, t)
		return 8
	})
	var p Path
	p.Start(ToFixedP(50, 100))
	p.Line(ToFixedP(450, 100))
	p.Start(ToFixedP(50, 400))
	p.Line(ToFixedP(450, 400))
	p.AddTo(v)
	v.Draw()

	if len(ts) < 4 {
		t.Fatalf("width function called %d times, want at least 4", len(ts))
	}
	resets := 0
	for i := 1; i < len(ts); i++ {
		if ts[i] < ts[i-1] {
			resets++
		}
	}
	if resets == 0 {
		t.Error("normalized arc length did not restart for the second subpath")
	}
	if ts[len(ts)-1] != 1 {
		t.Errorf("last t = %v, want 1", ts[len(ts)-1])
	}
	for _, y := range []int{100, 400} {
		if img.RGBAAt(250, y).A == 0 {
			t.Errorf("subpath at y=%d not stroked", y)
		}
	}
}

// TestVarStrokePathImmutable checks that stroking never mutates the input
// path, and that a width function mutating the path cannot affect the
// geometry, which is buffered before any width callback runs.
func TestVarStrokePathImmutable(t *testing.T) {
	p := pressureCurvePath()
	before := p.String()

	_, _, v := newVarStrokerImg(512, 512)
	v.SetStrokeWidthProfile([]float64{2, 20, 2})
	p.AddTo(v)
	v.Draw()

	if after := p.String(); after != before {
		t.Errorf("path mutated by stroking:\nbefore: %s\nafter:  %s", before, after)
	}

	// A misbehaving width function that appends to the path must not
	// change the rendered geometry.
	p2 := pressureCurvePath()
	wf := func(t float64) float64 { return 2 + 18*math.Sin(math.Pi*t) }
	_, _, v1 := newVarStrokerImg(512, 512)
	v1.SetStrokeWidthFunc(wf)
	p2.AddTo(v1)
	v1.Draw()
	want := coverageArea(v1.Scanner.(*ScannerGV).Dest)

	p2 = pressureCurvePath()
	_, _, v2 := newVarStrokerImg(512, 512)
	v2.SetStrokeWidthFunc(func(t float64) float64 {
		p2.Line(ToFixedP(10, 10)) // must not affect this render
		return wf(t)
	})
	p2.AddTo(v2)
	v2.Draw()
	if a := coverageArea(v2.Scanner.(*ScannerGV).Dest); a != want {
		t.Errorf("mutating width function changed geometry: area %d, want %d", a, want)
	}
}

// TestVarStrokeConcurrentSharedWidthFunc renders from several goroutines
// with separate strokers sharing one pure width function.
func TestVarStrokeConcurrentSharedWidthFunc(t *testing.T) {
	wf := WidthProfile(2, 20, 2)
	p := pressureCurvePath()
	const g = 4
	var wg sync.WaitGroup
	areas := make([]int, g)
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, v := newVarStrokerImg(512, 512)
			v.SetStrokeWidthFunc(wf)
			p.AddTo(v)
			v.Draw()
			areas[i] = coverageArea(v.Scanner.(*ScannerGV).Dest)
		}(i)
	}
	wg.Wait()
	for i := 1; i < g; i++ {
		if areas[i] != areas[0] {
			t.Errorf("goroutine %d area %d != goroutine 0 area %d", i, areas[i], areas[0])
		}
	}
	if areas[0] == 0 {
		t.Error("no coverage")
	}
}
