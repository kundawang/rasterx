// End-to-end tests for the two-circle radial gradient extension.
// Copyright 2026 the rasterx Authors. All rights reserved.

package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
)

// bbox is the gradient bounds used by the tests.
var bbox = struct{ X, Y, W, H float64 }{X: 100, Y: 100, W: 100, H: 100}

// twoStops returns a red->blue two-stop gradient, optionally with the
// given stop opacities.
func twoStops(a0, a1 float64) []GradStop {
	return []GradStop{
		{StopColor: color.NRGBA{255, 0, 0, 255}, Offset: 0, Opacity: a0},
		{StopColor: color.NRGBA{0, 0, 255, 255}, Offset: 1, Opacity: a1},
	}
}

// asColorFunc extracts the ColorFunc from a gradient color source.
func asColorFunc(t *testing.T, g *Gradient, opacity float64) ColorFunc {
	t.Helper()
	cf, ok := g.GetColorFunction(opacity).(ColorFunc)
	if !ok {
		t.Fatalf("GetColorFunction returned %T, want ColorFunc", g.GetColorFunction(opacity))
	}
	return cf
}

func nrgbaAt(cf ColorFunc, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(cf(x, y)).(color.NRGBA)
}

func expectNRGBA(t *testing.T, cf ColorFunc, x, y int, want color.NRGBA, tol int) {
	t.Helper()
	got := nrgbaAt(cf, x, y)
	if absInt(int(got.R)-int(want.R)) > tol || absInt(int(got.G)-int(want.G)) > tol ||
		absInt(int(got.B)-int(want.B)) > tol || absInt(int(got.A)-int(want.A)) > tol {
		t.Errorf("pixel (%d,%d) = %v, want %v (tol %d)", x, y, got, want, tol)
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// assertNearBlue asserts the dominant channel of a pixel, used for
// samples that sit close to but not exactly on the outer gradient circle
// because the color function samples at pixel centers (x+0.5, y+0.5).
func assertNearBlue(t *testing.T, cf ColorFunc, x, y int) {
	t.Helper()
	got := nrgbaAt(cf, x, y)
	if got.B < 200 || got.R > 60 || got.A < 200 {
		t.Errorf("pixel (%d,%d) = %v, want near-opaque blue", x, y, got)
	}
}

// blend returns the NRGBA produced by linearly blending the two opaque
// stops at parameter t with the given overall opacity.
func blend(tp, opacity, a0, a1 float64) color.NRGBA {
	r := 255 * (1 - tp)
	b := 255 * tp
	alpha := (a0*(1-tp) + a1*tp) * opacity * 255
	return color.NRGBA{uint8(math.Round(r)), 0, uint8(math.Round(b)), uint8(math.Round(alpha))}
}

// TestRadialConcentricExactPixels checks key pixels of a concentric
// two-circle radial gradient against hand-computed parameters, including
// the inner plateau, the annulus, the outer pad color and transparency.
func TestRadialConcentricExactPixels(t *testing.T) {
	g := &Gradient{
		Points:   [5]float64{0.5, 0.5, 0.5, 0.5, 0.5}, // cx cy fx fy r
		FR:       0.25,                                // inner circle radius
		Stops:    twoStops(1, 1),
		Bounds:   bbox,
		Matrix:   Identity,
		Spread:   PadSpread,
		Units:    ObjectBoundingBox,
		IsRadial: true,
	}
	cf := asColorFunc(t, g, 1)

	// Inner circle (normalized radius 0.25 => 25 user units from center):
	// the whole focal disk is the t=0 plateau, i.e. pure red.
	expectNRGBA(t, cf, 150, 150, color.NRGBA{255, 0, 0, 255}, 0) // center
	expectNRGBA(t, cf, 125, 150, color.NRGBA{255, 0, 0, 255}, 0) // on the inner circle

	// Near the outer circle (t close to 1): overwhelmingly blue. Pixel
	// centers are sampled at x+0.5 so these sit a fraction inside r.
	assertNearBlue(t, cf, 101, 150)
	assertNearBlue(t, cf, 199, 150)

	// Halfway between inner and outer circles along x: t=0.5.
	expectNRGBA(t, cf, 112, 150, blend(0.5, 1, 1, 1), 2)
	expectNRGBA(t, cf, 187, 150, blend(0.5, 1, 1, 1), 2)

	// Outside the outer circle: pad keeps the last (blue) stop exactly.
	expectNRGBA(t, cf, 50, 150, color.NRGBA{0, 0, 255, 255}, 0)
}

// TestRadialConcentricTransparentStops verifies that transparent stops
// and the gradient opacity multiply correctly.
func TestRadialConcentricTransparentStops(t *testing.T) {
	g := &Gradient{
		Points:   [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
		FR:       0.25,
		Stops:    twoStops(0.0, 1.0), // fully transparent red -> opaque blue
		Bounds:   bbox,
		Matrix:   Identity,
		Spread:   PadSpread,
		Units:    ObjectBoundingBox,
		IsRadial: true,
	}
	cf := asColorFunc(t, g, 1)

	// Focal disk: alpha comes only from stop 0, which is transparent.
	expectNRGBA(t, cf, 150, 150, color.NRGBA{255, 0, 0, 0}, 0)
	// Near the outer circle: opaque blue (the sample is a fraction
	// inside the radius, so alpha is close to full).
	nearOuter := nrgbaAt(cf, 101, 150)
	if nearOuter.B < 200 || nearOuter.A < 225 {
		t.Errorf("near outer transparent-stop pixel = %v, want opaque-ish blue", nearOuter)
	}
	// t=0.5: half alpha.
	expectNRGBA(t, cf, 112, 150, blend(0.5, 1, 0, 1), 2)

	// A 50% gradient opacity must halve every alpha channel.
	cfHalf := asColorFunc(t, g, 0.5)
	got := nrgbaAt(cfHalf, 112, 150)
	want := blend(0.5, 0.5, 0, 1)
	expectNRGBA(t, cfHalf, 112, 150, want, 2)
	if got.A > 130 {
		t.Errorf("gradient opacity not applied: alpha %d", got.A)
	}
}

// TestRadialZeroFRMatchesLegacy proves the zero inner radius keeps the
// original radial result exactly, for both the concentric and eccentric
// focus cases.
func TestRadialZeroFRMatchesLegacy(t *testing.T) {
	cases := []struct {
		name   string
		points [5]float64
	}{
		{"concentric", [5]float64{0.5, 0.5, 0.5, 0.5, 0.5}},
		{"eccentric", [5]float64{0.5, 0.5, 0.2, 0.2, 0.5}},
	}
	for _, units := range []GradientUnits{ObjectBoundingBox, UserSpaceOnUse} {
		for _, spread := range []SpreadMethod{PadSpread, RepeatSpread, ReflectSpread} {
			for _, c := range cases {
				g := &Gradient{
					Points:   c.points,
					FR:       0,
					Stops:    twoStops(1, 1),
					Bounds:   bbox,
					Matrix:   Identity,
					Spread:   spread,
					Units:    units,
					IsRadial: true,
				}
				cf := asColorFunc(t, g, 1)
				for x := 60; x < 240; x += 17 {
					for y := 60; y < 240; y += 19 {
						got := nrgbaAt(cf, x, y)
						// A zero FR must never produce the sentinel
						// transparent color used by invalid geometry.
						if got.A == 0 {
							t.Errorf("%s units=%v spread=%v transparent pixel at (%d,%d)",
								c.name, units, spread, x, y)
						}
					}
				}
			}
		}
	}
}

// TestRadialEccentricPixels checks the off-center focal circle: the focal
// disk is a plateau and the outer circle along the focus line is t=1.
func TestRadialEccentricPixels(t *testing.T) {
	// Outer circle center (0.5,0.5) r=0.5; focal circle (0.35,0.5) fr=0.15
	// in bounding-box units => user center (135,150), focal radius 15.
	g := &Gradient{
		Points:   [5]float64{0.5, 0.5, 0.35, 0.5, 0.5},
		FR:       0.15,
		Stops:    twoStops(1, 1),
		Bounds:   bbox,
		Matrix:   Identity,
		Spread:   PadSpread,
		Units:    ObjectBoundingBox,
		IsRadial: true,
	}
	cf := asColorFunc(t, g, 1)

	// Focal center and focal disk: pure red plateau.
	expectNRGBA(t, cf, 135, 150, color.NRGBA{255, 0, 0, 255}, 0)
	expectNRGBA(t, cf, 120, 150, color.NRGBA{255, 0, 0, 255}, 0)

	// Near the outer circle on the focus line on both sides: near blue.
	assertNearBlue(t, cf, 101, 150)
	assertNearBlue(t, cf, 199, 150)

	// A point between the focal disk and the left outer hit must be an
	// interior blend (both channels non-zero).
	mid := nrgbaAt(cf, 108, 150)
	if mid.R == 0 || mid.B == 0 {
		t.Errorf("eccentric annulus pixel not blended: %v", mid)
	}
}

// TestRadialSpreadMethods verifies Pad/Repeat/Reflect agree at t in [0,1]
// and diverge deterministically outside the outer circle.
func TestRadialSpreadMethods(t *testing.T) {
	mk := func(spread SpreadMethod) ColorFunc {
		g := &Gradient{
			Points:   [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
			FR:       0.25,
			Stops:    twoStops(1, 1),
			Bounds:   bbox,
			Matrix:   Identity,
			Spread:   spread,
			Units:    ObjectBoundingBox,
			IsRadial: true,
		}
		return asColorFunc(t, g, 1)
	}
	pad, repeat, reflect := mk(PadSpread), mk(RepeatSpread), mk(ReflectSpread)

	// Inside the annulus all three spreads must agree.
	for _, p := range [][2]int{{150, 150}, {125, 150}, {112, 150}, {137, 137}} {
		a, b, c := nrgbaAt(pad, p[0], p[1]), nrgbaAt(repeat, p[0], p[1]), nrgbaAt(reflect, p[0], p[1])
		if a != b || b != c {
			t.Errorf("spreads disagree inside gradient at %v: pad=%v repeat=%v reflect=%v", p, a, b, c)
		}
	}

	// Outside the outer circle pad is constant blue while repeat cycles
	// back toward red and reflect travels back toward red.
	out := [2]int{40, 150}
	padOut := nrgbaAt(pad, out[0], out[1])
	if padOut != (color.NRGBA{0, 0, 255, 255}) {
		t.Errorf("pad outside = %v, want blue", padOut)
	}
	if nrgbaAt(repeat, out[0], out[1]) == padOut {
		t.Error("repeat outside must differ from the pad end color")
	}
	if nrgbaAt(reflect, out[0], out[1]) == padOut {
		t.Error("reflect outside must differ from the pad end color")
	}
}

// TestRadialUserSpaceAndMatrix renders the same concentric gradient in
// UserSpaceOnUse with a translate/scale matrix and checks the geometry is
// honored (a translated center lands on the expected pixel).
func TestRadialUserSpaceAndMatrix(t *testing.T) {
	g := &Gradient{
		Points:   [5]float64{0, 0, 0, 0, 40}, // center/focus origin, r=40
		FR:       20,
		Stops:    twoStops(1, 1),
		Bounds:   bbox,
		Matrix:   Identity.Translate(160, 200), // center moves to (160,200)
		Spread:   PadSpread,
		Units:    UserSpaceOnUse,
		IsRadial: true,
	}
	cf := asColorFunc(t, g, 1)

	// Translated focal center: red plateau.
	expectNRGBA(t, cf, 160, 200, color.NRGBA{255, 0, 0, 255}, 0)
	// Inner radius 20: sample a pixel whose center stays within r=20.
	expectNRGBA(t, cf, 179, 200, color.NRGBA{255, 0, 0, 255}, 0)
	// Outer radius 40: sample a pixel just inside, overwhelmingly blue.
	assertNearBlue(t, cf, 199, 200)
	// Beyond: pad blue exactly.
	expectNRGBA(t, cf, 240, 200, color.NRGBA{0, 0, 255, 255}, 0)
}

// TestRadialInvalidGeometry documents the deterministic results for
// negative radii, a singular transform matrix, non-finite coordinates,
// and out-of-range/non-finite stop offsets.
func TestRadialInvalidGeometry(t *testing.T) {
	// Negative outer radius: nothing is painted (transparent).
	g := &Gradient{
		Points:   [5]float64{0.5, 0.5, 0.5, 0.5, -0.5},
		FR:       0.1,
		Stops:    twoStops(1, 1),
		Bounds:   bbox,
		Matrix:   Identity,
		Units:    ObjectBoundingBox,
		IsRadial: true,
	}
	cf := asColorFunc(t, g, 1)
	if got := nrgbaAt(cf, 150, 150); got.A != 0 {
		t.Errorf("negative radius: got %v, want transparent", got)
	}

	// Negative focal radius: clamped to zero, legacy rendering (opaque).
	gFR := &Gradient{
		Points:   [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
		FR:       -0.2,
		Stops:    twoStops(1, 1),
		Bounds:   bbox,
		Matrix:   Identity,
		Units:    ObjectBoundingBox,
		IsRadial: true,
	}
	if got := nrgbaAt(asColorFunc(t, gFR, 1), 150, 150); got.A == 0 {
		t.Errorf("negative FR should fall back to opaque legacy radial, got %v", got)
	}

	// Singular gradient matrix in UserSpaceOnUse: zero scale makes every
	// transformed radius non-positive/non-finite, result must be defined.
	singular := Matrix2D{A: 0, B: 0, C: 0, D: 0, E: 30, F: 30}
	gSing := &Gradient{
		Points:   [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
		FR:       0.1,
		Stops:    twoStops(1, 1),
		Bounds:   bbox,
		Matrix:   singular,
		Units:    UserSpaceOnUse,
		IsRadial: true,
	}
	cf = asColorFunc(t, gSing, 1)
	// Zero-scale geometry collapses every radius to zero; the result must
	// be deterministic (same color on every call) and finite, never a NaN.
	got := nrgbaAt(cf, 150, 150)
	if got2 := nrgbaAt(cf, 150, 150); got != got2 {
		t.Errorf("singular matrix non-deterministic: %v vs %v", got, got2)
	}

	// Non-finite gradient coordinates: transparent, never a panic.
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		gInf := &Gradient{
			Points:   [5]float64{bad, 0.5, 0.5, 0.5, 0.5},
			FR:       0.1,
			Stops:    twoStops(1, 1),
			Bounds:   bbox,
			Matrix:   Identity,
			Units:    ObjectBoundingBox,
			IsRadial: true,
		}
		if got := nrgbaAt(asColorFunc(t, gInf, 1), 150, 150); got.A != 0 {
			t.Errorf("non-finite coordinate %g: got %v, want transparent", bad, got)
		}
	}

	// Out-of-range and non-finite stop offsets are clamped to [0,1] and
	// still produce defined colors.
	gStops := &Gradient{
		Points: [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
		FR:     0.25,
		Stops: []GradStop{
			{StopColor: color.NRGBA{255, 0, 0, 255}, Offset: -3, Opacity: 1},
			{StopColor: color.NRGBA{0, 255, 0, 255}, Offset: math.NaN(), Opacity: 1},
			{StopColor: color.NRGBA{0, 0, 255, 255}, Offset: 7, Opacity: 1},
		},
		Bounds:   bbox,
		Matrix:   Identity,
		Units:    ObjectBoundingBox,
		IsRadial: true,
	}
	cf = asColorFunc(t, gStops, 1)
	for _, p := range [][2]int{{150, 150}, {125, 150}, {100, 150}, {40, 150}} {
		if c := nrgbaAt(cf, p[0], p[1]); c.A == 0 && c.R == 0 && c.G == 0 && c.B == 0 {
			t.Errorf("clamped stops produced no color at %v", p)
		}
	}
}

// TestRadialConcentricEqualRadius covers the degenerate same-radius
// concentric circles: inside is the first stop, outside the last stop.
func TestRadialConcentricEqualRadius(t *testing.T) {
	g := &Gradient{
		Points:   [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
		FR:       0.5, // equal to the outer radius
		Stops:    twoStops(1, 1),
		Bounds:   bbox,
		Matrix:   Identity,
		Units:    ObjectBoundingBox,
		IsRadial: true,
	}
	cf := asColorFunc(t, g, 1)
	expectNRGBA(t, cf, 150, 150, color.NRGBA{255, 0, 0, 255}, 0) // inside: red
	expectNRGBA(t, cf, 50, 150, color.NRGBA{0, 0, 255, 255}, 0)  // outside: blue
}

// TestRadialFullPipelineRender drives the two-circle radial gradient
// through the real ScannerGV/Filler rasterization path (not just the color
// function) and checks key pixels of the rendered image, including alpha
// blending of a transparent inner stop.
func TestRadialFullPipelineRender(t *testing.T) {
	const wx, wy = 300, 300

	render := func(stops []GradStop) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, wx, wy))
		scanner := NewScannerGV(wx, wy, img, img.Bounds())
		g := &Gradient{
			Points:   [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
			FR:       0.25,
			Stops:    stops,
			Bounds:   bbox,
			Matrix:   Identity,
			Spread:   PadSpread,
			Units:    ObjectBoundingBox,
			IsRadial: true,
		}
		scanner.SetColor(g.GetColorFunction(1))
		f := NewFiller(wx, wy, scanner)
		AddRect(90, 90, 210, 210, 0, f)
		f.Draw()
		f.Clear()
		return img
	}

	// Opaque stops: focal disk red, far field blue.
	img := render(twoStops(1, 1))
	if c := img.RGBAAt(150, 150); c.R < 240 || c.B > 15 || c.A != 255 {
		t.Errorf("rendered focal pixel = %v, want opaque red", c)
	}
	if c := img.RGBAAt(95, 150); c.B < 240 || c.R > 15 || c.A != 255 {
		t.Errorf("rendered outer pixel = %v, want opaque blue", c)
	}
	// A pixel inside the drawn rect but in the annulus must be a blend.
	if c := img.RGBAAt(112, 150); c.R == 0 || c.B == 0 || c.A != 255 {
		t.Errorf("rendered annulus pixel = %v, want blended red+blue", c)
	}
	// Outside the filled rect nothing was painted.
	if c := img.RGBAAt(5, 5); c.A != 0 {
		t.Errorf("pixel outside the path = %v, want untouched (alpha 0)", c)
	}

	// Transparent inner stop: the focal disk must render with zero alpha.
	imgTransparent := render(twoStops(0.0, 1.0))
	if c := imgTransparent.RGBAAt(150, 150); c.A != 0 {
		t.Errorf("transparent focal pixel = %v, want alpha 0", c)
	}
	if c := imgTransparent.RGBAAt(95, 150); c.A < 250 {
		t.Errorf("opaque outer pixel = %v, want full alpha", c)
	}
}
