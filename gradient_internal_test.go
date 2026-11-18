// Internal unit tests for the two-circle radial gradient geometry.
// Copyright 2026 the rasterx Authors. All rights reserved.

package rasterx

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestIsFinite(t *testing.T) {
	for _, v := range []float64{0, 1, -1, 1e-300, 1e308, math.SmallestNonzeroFloat64} {
		if !isFinite(v) {
			t.Errorf("isFinite(%g) = false, want true", v)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if isFinite(v) {
			t.Errorf("isFinite(%g) = true, want false", v)
		}
	}
}

func TestClampFocalRadius(t *testing.T) {
	if got := clampFocalRadius(0.4); got != 0.4 {
		t.Errorf("clampFocalRadius(0.4) = %v, want 0.4", got)
	}
	for _, v := range []float64{0, -0.5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := clampFocalRadius(v); got != 0 {
			t.Errorf("clampFocalRadius(%g) = %v, want 0 (legacy path)", v, got)
		}
	}
}

func TestConstrainFocalCircle(t *testing.T) {
	// Already contained: unchanged.
	fx, fy, fr := constrainFocalCircle(0.2, 0, 0, 0, 0.5)
	if !approx(fx, 0.2, 1e-12) || !approx(fy, 0, 1e-12) || !approx(fr, 0.5, 1e-12) {
		t.Errorf("contained focal circle altered: %v %v %v", fx, fy, fr)
	}

	// Focus outside the unit circle is projected onto it along the
	// center/focus line, and fr shrinks to fit.
	fx, fy, fr = constrainFocalCircle(3, 4, 0, 0, 0.2)
	if !approx(math.Hypot(fx, fy), 1, 1e-12) {
		t.Errorf("outside focus not projected onto unit circle: %v %v", fx, fy)
	}
	if !approx(fx, 0.6, 1e-12) || !approx(fy, 0.8, 1e-12) {
		t.Errorf("projection point wrong: %v %v", fx, fy)
	}
	if fr != 0 {
		t.Errorf("fr on the outer circumference must shrink to 0, got %v", fr)
	}

	// Focal radius too large for an interior focus: shrink so the focal
	// circle stays tangent to the outer circle.
	fx, fy, fr = constrainFocalCircle(0.5, 0, 0, 0, 0.8)
	if !approx(fx, 0.5, 1e-12) || !approx(fy, 0, 1e-12) || !approx(fr, 0.5, 1e-12) {
		t.Errorf("focal radius not shrunk to fit: %v %v %v", fx, fy, fr)
	}

	// Inner circle as large as the outer one: collapse to a full-size
	// concentric circle (deterministic degenerate annulus).
	fx, fy, fr = constrainFocalCircle(0.3, 0, 0, 0, 1)
	if fx != 0 || fy != 0 || fr != 1 {
		t.Errorf("fr>=1 must collapse to concentric unit circle: %v %v %v", fx, fy, fr)
	}
	fx, fy, fr = constrainFocalCircle(0, 0, 0, 0, 2)
	if fx != 0 || fy != 0 || fr != 1 {
		t.Errorf("concentric oversized fr must clamp to unit circle: %v %v %v", fx, fy, fr)
	}
}

func TestRadialTwoCircleTConcentric(t *testing.T) {
	const fr = 0.25
	// Points inside the focal disk plateau at t = 0.
	for _, d := range []float64{0, 0.1, fr} {
		if got, ok := radialTwoCircleT(d, 0, 0, 0, fr, 0, 0); !ok || got != 0 {
			t.Errorf("inside focal disk d=%v: got (%v,%v), want (0,true)", d, got, ok)
		}
	}
	// Outer circle maps to t = 1.
	if got, ok := radialTwoCircleT(1, 0, 0, 0, fr, 0, 0); !ok || !approx(got, 1, 1e-12) {
		t.Errorf("outer circle: got (%v,%v), want (1,true)", got, ok)
	}
	// Midpoint of the annulus maps to t = 0.5.
	if got, ok := radialTwoCircleT((1+fr)/2, 0, 0, 0, fr, 0, 0); !ok || !approx(got, 0.5, 1e-12) {
		t.Errorf("annulus midpoint: got (%v,%v), want (0.5,true)", got, ok)
	}
	// Outside the outer circle keeps extrapolating (pad/repeat/reflect
	// are applied later by tColor).
	if got, ok := radialTwoCircleT(1.5, 0, 0, 0, fr, 0, 0); !ok || got <= 1 {
		t.Errorf("outside point must extrapolate t>1, got (%v,%v)", got, ok)
	}

	// Degenerate concentric annulus with equal radii.
	if got, ok := radialTwoCircleT(0.5, 0, 0, 0, 1, 0, 0); !ok || got != 0 {
		t.Errorf("inside equal-radius circles: got (%v,%v), want (0,true)", got, ok)
	}
	if got, ok := radialTwoCircleT(1.5, 0, 0, 0, 1, 0, 0); !ok || got != 1 {
		t.Errorf("outside equal-radius circles: got (%v,%v), want (1,true)", got, ok)
	}
}

func TestRadialTwoCircleTEccentric(t *testing.T) {
	// Unit outer circle at origin, focal circle centered at (0.5,0)
	// with radius 0.25 (fully contained).
	fx, fy, fr, cx, cy := 0.5, 0.0, 0.25, 0.0, 0.0

	// Focal disk interior plateaus at zero.
	if got, ok := radialTwoCircleT(fx, fy, fx, fy, fr, cx, cy); !ok || got != 0 {
		t.Errorf("focal center: got (%v,%v), want (0,true)", got, ok)
	}
	if got, ok := radialTwoCircleT(fx+fr, fy, fx, fy, fr, cx, cy); !ok || got != 0 {
		t.Errorf("focal circle boundary: got (%v,%v), want (0,true)", got, ok)
	}

	// Point on the outer circle along the focus line maps to t = 1.
	// Ray from (0.5,0) through (-1,0) hits the outer circle at (-1,0).
	if got, ok := radialTwoCircleT(-1, 0, fx, fy, fr, cx, cy); !ok || !approx(got, 1, 1e-9) {
		t.Errorf("outer circle along focus line: got (%v,%v), want (1,true)", got, ok)
	}

	// A point halfway (in distance ratio) between focal and outer circles
	// maps to t = 0.5. From the focus at 0.5 going left, the outer hit is
	// at -1, distance L = 1.5; the t=0.5 point sits at fr+0.5*(L-fr).
	px := fx - (fr + 0.5*(1.5-fr))
	if got, ok := radialTwoCircleT(px, 0, fx, fy, fr, cx, cy); !ok || !approx(got, 0.5, 1e-9) {
		t.Errorf("annulus midpoint along focus line: got (%v,%v), want (0.5,true)", got, ok)
	}

	// Every returned value for a grid of points must be finite.
	for ix := -20; ix <= 20; ix++ {
		for iy := -20; iy <= 20; iy++ {
			got, ok := radialTwoCircleT(float64(ix)/10, float64(iy)/10, fx, fy, fr, cx, cy)
			if ok && !isFinite(got) {
				t.Errorf("non-finite t at (%v,%v)", ix, iy)
			}
		}
	}
}

func TestRadialTwoCircleTNoIntersection(t *testing.T) {
	// Across a wide range of contained/near-degenerate focal circles,
	// every result must be a finite t together with a defined ok flag;
	// the ok==false branch exists for tangent/near-coincident geometry
	// and must never leak a NaN into the color pipeline.
	var fr float64
	for fr = 0.1; fr <= 1.0; fr += 0.1 {
		for _, f := range [][2]float64{{0, 0}, {0.3, 0}, {0.6, 0.2}, {-0.4, 0.4}} {
			nfx, nfy, nfr := constrainFocalCircle(f[0], f[1], 0, 0, fr)
			for ix := -30; ix <= 30; ix++ {
				for iy := -30; iy <= 30; iy++ {
					got, ok := radialTwoCircleT(float64(ix)/10, float64(iy)/10,
						nfx, nfy, nfr, 0, 0)
					if ok && !isFinite(got) {
						t.Errorf("non-finite t at (%v,%v) fr=%v f=%v", ix, iy, fr, f)
					}
				}
			}
		}
	}
}
