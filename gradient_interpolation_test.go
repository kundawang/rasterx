// Copyright 2026 The rasterx Authors. All rights reserved.
//
// Tests for the optional gradient interpolation modes (sRGB, linear-light
// and premultiplied alpha) and for the robustness of gradient color
// function construction.
package rasterx_test

import (
	"image/color"
	"math"
	"reflect"
	"sync"
	"testing"

	. "github.com/srwiley/rasterx"
)

// ---- independent float64 reference implementation -----------------------

func refDecode(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func refEncode(c float64) float64 {
	if c <= 0.0031308 {
		return 12.92 * c
	}
	return 1.055*math.Pow(c, 1.0/2.4) - 0.055
}

func refRound8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 0xFF
	}
	return uint8(math.Round(v * 0xFF))
}

// legacyRef reimplements the historical rasterx sRGB blend (PadSpread,
// 0 <= t <= 1) so the default mode can be checked for exact equality.
func legacyRef(stops []GradStop, t, opacity float64) color.NRGBA {
	place := 0
	for place != len(stops) && t > stops[place].Offset {
		place++
	}
	if place == 0 {
		return ApplyOpacity(stops[0].StopColor, stops[0].Opacity*opacity)
	}
	if place == len(stops) {
		s := stops[len(stops)-1]
		return ApplyOpacity(s.StopColor, s.Opacity*opacity)
	}
	s1, s2 := stops[place-1], stops[place]
	tp := (t - s1.Offset) / (s2.Offset - s1.Offset)
	r1, g1, b1, _ := s1.StopColor.RGBA()
	r2, g2, b2, _ := s2.StopColor.RGBA()
	return ApplyOpacity(color.RGBA{
		uint8((float64(r1)*(1-tp) + float64(r2)*tp) / 256),
		uint8((float64(g1)*(1-tp) + float64(g2)*tp) / 256),
		uint8((float64(b1)*(1-tp) + float64(b2)*tp) / 256),
		0xFF}, (s1.Opacity*(1-tp)+s2.Opacity*tp)*opacity)
}

// ---- helpers --------------------------------------------------------------

var allModes = []GradientInterpolation{
	SRGBInterpolation, LinearLightInterpolation, PremulAlphaInterpolation}
var allSpreads = []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread}

func mkLinear(stops []GradStop, mode GradientInterpolation, spread SpreadMethod) *Gradient {
	return &Gradient{
		Points:        [5]float64{0, 0, 1, 0, 0},
		Bounds:        struct{ X, Y, W, H float64 }{X: 10, Y: 20, W: 200, H: 4},
		Matrix:        Identity,
		Spread:        spread,
		Interpolation: mode,
		Stops:         stops,
	}
}

func colorFuncOf(t *testing.T, g *Gradient, opacity float64) ColorFunc {
	t.Helper()
	cf, ok := g.GetColorFunction(opacity).(ColorFunc)
	if !ok {
		t.Fatalf("GetColorFunction did not return a ColorFunc, got %T",
			g.GetColorFunction(opacity))
	}
	return cf
}

func nrgba(c color.Color) color.NRGBA { return color.NRGBAModel.Convert(c).(color.NRGBA) }

func absDiff(a, b uint8) int {
	d := int(a) - int(b)
	if d < 0 {
		d = -d
	}
	return d
}

// tAtX maps a pixel x coordinate to the gradient parameter for mkLinear
// gradients (Bounds.X = 10, Bounds.W = 200).
func tAtX(x int) float64 { return (float64(x) + 0.5 - 10) / 200 }

// ---- per-pixel reference tests -------------------------------------------

// TestLinearLightBlackWhite checks linear-light interpolation of a
// black-to-white gradient against a float64 sRGB-EOTF reference, per pixel.
func TestLinearLightBlackWhite(t *testing.T) {
	g := mkLinear([]GradStop{
		{StopColor: color.NRGBA{0, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 1, Opacity: 1},
	}, LinearLightInterpolation, PadSpread)
	cf := colorFuncOf(t, g, 1)

	maxErr := 0
	for x := 10; x < 210; x++ {
		want := refRound8(refEncode(tAtX(x))) // decode(0)=0, decode(1)=1
		got := nrgba(cf(x, 22))
		if got.A != 0xFF {
			t.Fatalf("x=%d: alpha = %d, want 255", x, got.A)
		}
		if got.R != got.G || got.G != got.B {
			t.Fatalf("x=%d: gray gradient produced %v", x, got)
		}
		if d := absDiff(got.R, want); d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 1 {
		t.Errorf("max per-pixel error vs float reference = %d, want <= 1", maxErr)
	}
	// The motivating case: the midpoint must be much brighter than the
	// sRGB-space midpoint (~127); linear light gives ~188.
	mid := nrgba(cf(110, 22)) // t = 0.5025
	if mid.R < 180 {
		t.Errorf("linear-light midpoint = %d, want >= 180 (sRGB midpoint is ~127)", mid.R)
	}
}

// TestLegacySRGBDefaultUnchanged verifies that the zero-value interpolation
// mode reproduces the historical rasterx arithmetic exactly, per pixel.
func TestLegacySRGBDefaultUnchanged(t *testing.T) {
	stops := []GradStop{
		{StopColor: color.RGBA{200, 100, 50, 255}, Offset: 0.2, Opacity: 1.0},
		{StopColor: color.RGBA{10, 220, 90, 255}, Offset: 0.5, Opacity: 0.5},
		{StopColor: color.RGBA{0, 0, 255, 255}, Offset: 0.9, Opacity: 1.0},
	}
	g := mkLinear(stops, SRGBInterpolation, PadSpread) // zero value mode
	const opacity = 0.8
	cf := colorFuncOf(t, g, opacity)
	for x := 10; x < 210; x++ {
		want := legacyRef(stops, tAtX(x), opacity)
		got := nrgba(cf(x, 22))
		if got != want {
			t.Fatalf("x=%d: legacy mode = %v, historical reference = %v", x, got, want)
		}
	}
	// Black/white midpoint keeps the historical truncated value 127.
	bw := mkLinear([]GradStop{
		{StopColor: color.RGBA{0, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.RGBA{255, 255, 255, 255}, Offset: 1, Opacity: 1},
	}, SRGBInterpolation, PadSpread)
	bw.Bounds = struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 201, H: 4}
	if got := nrgba(colorFuncOf(t, bw, 1)(100, 2)); got.R != 127 { // t == 0.5 exactly
		t.Errorf("legacy black/white midpoint = %d, want 127", got.R)
	}
}

// TestPremulAlphaTransparentEndpoint checks that a fully transparent stop
// contributes no hue in premultiplied mode: a red-to-transparent-blue
// gradient stays pure red and only fades alpha.
func TestPremulAlphaTransparentEndpoint(t *testing.T) {
	g := mkLinear([]GradStop{
		{StopColor: color.NRGBA{255, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.NRGBA{0, 0, 255, 255}, Offset: 1, Opacity: 0},
	}, PremulAlphaInterpolation, PadSpread)
	cf := colorFuncOf(t, g, 1)

	maxErr := 0
	for x := 10; x < 209; x++ { // skip t==1 where alpha is exactly 0
		tt := tAtX(x)
		wantA := refRound8(1 - tt)
		got := nrgba(cf(x, 22))
		if got.R != 0xFF || got.G != 0 || got.B != 0 {
			t.Errorf("x=%d: premul color = %v, want pure red with fading alpha", x, got)
		}
		if d := absDiff(got.A, wantA); d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 1 {
		t.Errorf("max alpha error vs float reference = %d, want <= 1", maxErr)
	}
	// The legacy sRGB mode blends the hues, giving a purple midpoint.
	legacy := mkLinear(g.Stops, SRGBInterpolation, PadSpread)
	if got := nrgba(colorFuncOf(t, legacy, 1)(110, 22)); got.B < 100 {
		t.Errorf("legacy midpoint blue channel = %d, want >= 100 (hue bleed)", got.B)
	}
}

// TestLinearLightSemiTransparent checks linear-light interpolation with
// semi-transparent stops against the float reference, color and alpha.
func TestLinearLightSemiTransparent(t *testing.T) {
	g := mkLinear([]GradStop{
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 1, Opacity: 0},
	}, LinearLightInterpolation, PadSpread)
	cf := colorFuncOf(t, g, 0.5) // overall opacity folds into alpha

	maxErr := 0
	for x := 10; x < 210; x++ {
		tt := tAtX(x)
		wantA := refRound8((1 - tt) * 0.5)
		got := nrgba(cf(x, 22))
		if got.R != 0xFF || got.G != 0xFF || got.B != 0xFF {
			t.Errorf("x=%d: color = %v, want white", x, got)
		}
		if d := absDiff(got.A, wantA); d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 1 {
		t.Errorf("max alpha error vs float reference = %d, want <= 1", maxErr)
	}
}

// TestLinearLightRadial checks the linear-light mode on a radial gradient
// against the float reference.
func TestLinearLightRadial(t *testing.T) {
	g := &Gradient{
		Points:        [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
		Bounds:        struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
		Matrix:        Identity,
		IsRadial:      true,
		Interpolation: LinearLightInterpolation,
		Stops: []GradStop{
			{StopColor: color.NRGBA{0, 0, 0, 255}, Offset: 0, Opacity: 1},
			{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 1, Opacity: 1},
		},
	}
	cf := colorFuncOf(t, g, 1)
	maxErr := 0
	for x := 0; x < 100; x += 3 {
		for y := 0; y < 100; y += 3 {
			dx, dy := float64(x)+0.5-50, float64(y)+0.5-50
			tt := math.Sqrt(dx*dx+dy*dy) / 50
			if tt > 1 {
				tt = 1 // pad spread clamps to the last stop
			}
			want := refRound8(refEncode(tt))
			got := nrgba(cf(x, y))
			if d := absDiff(got.R, want); d > maxErr {
				maxErr = d
			}
		}
	}
	if maxErr > 1 {
		t.Errorf("radial: max per-pixel error vs float reference = %d, want <= 1", maxErr)
	}
}

// TestModesAcrossSpreads verifies every interpolation mode is honored for
// all three spread methods: matched parameter positions on pad, repeat and
// reflect gradients must produce the same color in each mode.
func TestModesAcrossSpreads(t *testing.T) {
	stops := []GradStop{
		{StopColor: color.NRGBA{0, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 1, Opacity: 1},
	}
	mk := func(spread SpreadMethod) *Gradient {
		g := mkLinear(stops, SRGBInterpolation, spread)
		g.Bounds = struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 4}
		return g
	}
	for _, mode := range allModes {
		pad := mk(PadSpread)
		pad.Interpolation = mode
		repeat := mk(RepeatSpread)
		repeat.Interpolation = mode
		reflectG := mk(ReflectSpread)
		reflectG.Interpolation = mode
		cfPad := colorFuncOf(t, pad, 1)
		cfRep := colorFuncOf(t, repeat, 1)
		cfRef := colorFuncOf(t, reflectG, 1)
		// x=49 -> t=0.495; x=149 -> t=1.495 -> repeat mod 0.495,
		// reflect 1.495 -> mirrored to 0.505; x=50 -> t=0.505.
		p49, p50 := nrgba(cfPad(49, 2)), nrgba(cfPad(50, 2))
		if got := nrgba(cfRep(149, 2)); maxChanDiff(got, p49) > 1 {
			t.Errorf("mode %d repeat: %v vs pad %v", mode, got, p49)
		}
		if got := nrgba(cfRef(149, 2)); maxChanDiff(got, p50) > 1 {
			t.Errorf("mode %d reflect: %v vs pad %v", mode, got, p50)
		}
	}
}

func maxChanDiff(a, b color.NRGBA) int {
	m := absDiff(a.R, b.R)
	if d := absDiff(a.G, b.G); d > m {
		m = d
	}
	if d := absDiff(a.B, b.B); d > m {
		m = d
	}
	if d := absDiff(a.A, b.A); d > m {
		m = d
	}
	return m
}

// ---- conversion stability -------------------------------------------------

// messyStops returns unsorted stops with duplicate and out-of-range offsets.
func messyStops() []GradStop {
	return []GradStop{
		{StopColor: color.NRGBA{255, 0, 0, 255}, Offset: 0.8, Opacity: 1},
		{StopColor: color.NRGBA{0, 255, 0, 255}, Offset: 0.2, Opacity: 0.5},
		{StopColor: color.NRGBA{0, 0, 255, 255}, Offset: 0.8, Opacity: 1}, // duplicate offset
		{StopColor: color.NRGBA{255, 255, 0, 128}, Offset: -0.3, Opacity: 1},
		{StopColor: color.NRGBA{0, 255, 255, 255}, Offset: 1.4, Opacity: 1},
	}
}

// TestInputStopsNotModified verifies GetColorFunctionUS never mutates the
// gradient's stop slice, for every mode and spread, linear and radial.
func TestInputStopsNotModified(t *testing.T) {
	orig := messyStops()
	for _, mode := range allModes {
		for _, spread := range allSpreads {
			g := mkLinear(orig, mode, spread)
			g.GetColorFunctionUS(0.7, Identity)
			if !reflect.DeepEqual(g.Stops, orig) {
				t.Errorf("mode %d spread %d: stops mutated: %v", mode, spread, g.Stops)
			}
			rg := &Gradient{
				Points: [5]float64{0.5, 0.5, 0.4, 0.4, 0.5}, IsRadial: true,
				Bounds: struct{ X, Y, W, H float64 }{X: 3, Y: 5, W: 40, H: 30},
				Matrix: Identity, Spread: spread, Interpolation: mode, Stops: orig,
			}
			rg.GetColorFunctionUS(0.7, Identity)
			if !reflect.DeepEqual(rg.Stops, orig) {
				t.Errorf("radial mode %d spread %d: stops mutated: %v", mode, spread, rg.Stops)
			}
		}
	}
}

// TestDeterministicWithMessyStops checks that unsorted, duplicate and
// out-of-range offsets give stable, repeatable colors in all modes/spreads.
func TestDeterministicWithMessyStops(t *testing.T) {
	for _, mode := range allModes {
		for _, spread := range allSpreads {
			g1 := mkLinear(messyStops(), mode, spread)
			g2 := mkLinear(messyStops(), mode, spread)
			cf1 := colorFuncOf(t, g1, 0.9)
			cf2 := colorFuncOf(t, g2, 0.9)
			for x := -20; x < 240; x++ {
				c1, c2 := nrgba(cf1(x, 22)), nrgba(cf2(x, 22))
				if c1 != c2 {
					t.Fatalf("mode %d spread %d x=%d: %v != %v", mode, spread, x, c1, c2)
				}
			}
		}
	}
}

// ---- robustness -----------------------------------------------------------

// TestNonFiniteAndDegenerate checks defined results for non-finite
// parameters, empty/single stop lists, huge gamma and degenerate geometry.
// None of these may panic or propagate NaN into a color.
func TestNonFiniteAndDegenerate(t *testing.T) {
	bw := []GradStop{
		{StopColor: color.NRGBA{0, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 1, Opacity: 1},
	}
	sampleAll := func(cf ColorFunc) {
		for x := -5; x < 215; x++ {
			_ = nrgba(cf(x, 22)) // must not panic; uint8 cannot hold NaN
		}
	}

	// Non-finite overall opacity.
	for _, op := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 2.5, -1} {
		for _, mode := range allModes {
			cf := colorFuncOf(t, mkLinear(bw, mode, PadSpread), op)
			sampleAll(cf)
			got := nrgba(cf(110, 22))
			switch {
			case math.IsNaN(op) || op < 0:
				if got.A != 0 {
					t.Errorf("opacity %v mode %d: alpha = %d, want 0", op, mode, got.A)
				}
			case op >= 1: // +Inf and >1 clamp to fully opaque
				if got.A != 0xFF {
					t.Errorf("opacity %v mode %d: alpha = %d, want 255", op, mode, got.A)
				}
			}
		}
	}

	// Non-finite stop offsets get deterministic sanitized values:
	// NaN and -Inf become 0, +Inf becomes 1.
	wild := []GradStop{
		{StopColor: color.NRGBA{0, 0, 0, 255}, Offset: math.NaN(), Opacity: 1},
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: math.Inf(1), Opacity: 1},
		{StopColor: color.NRGBA{128, 128, 128, 255}, Offset: math.Inf(-1), Opacity: 1},
	}
	norm := []GradStop{ // same order as the sanitized wild stops after stable sort
		{StopColor: color.NRGBA{0, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.NRGBA{128, 128, 128, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 1, Opacity: 1},
	}
	for _, mode := range allModes {
		cfWild := colorFuncOf(t, mkLinear(wild, mode, ReflectSpread), 1)
		cfNorm := colorFuncOf(t, mkLinear(norm, mode, ReflectSpread), 1)
		for x := 10; x < 210; x++ {
			if a, b := nrgba(cfWild(x, 22)), nrgba(cfNorm(x, 22)); a != b {
				t.Fatalf("mode %d x=%d: non-finite offsets %v, sanitized %v", mode, x, a, b)
			}
		}
	}

	// Extreme and invalid gamma values must not produce NaN; invalid
	// (non-positive or non-finite) gamma falls back to the sRGB transfer.
	base := mkLinear(bw, LinearLightInterpolation, PadSpread)
	cfBase := colorFuncOf(t, base, 1)
	for _, gamma := range []float64{1e300, 1e-300, math.NaN(), math.Inf(1), math.Inf(-1), -3, 0} {
		g := mkLinear(bw, LinearLightInterpolation, PadSpread)
		g.Gamma = gamma
		cf := colorFuncOf(t, g, 1)
		sampleAll(cf)
		if gamma <= 0 || math.IsNaN(gamma) || math.IsInf(gamma, 0) {
			if a, b := nrgba(cf(110, 22)), nrgba(cfBase(110, 2+20)); a != b {
				t.Errorf("gamma %v: midpoint %v, want sRGB fallback %v", gamma, a, b)
			}
		}
	}
	// A valid custom gamma is actually used: gamma 1 is the identity
	// transfer, so the midpoint is the plain mean.
	g1 := mkLinear(bw, LinearLightInterpolation, PadSpread)
	g1.Gamma = 1
	if got := nrgba(colorFuncOf(t, g1, 1)(110, 22)); absDiff(got.R, 128) > 1 {
		t.Errorf("gamma=1 midpoint = %d, want ~128", got.R)
	}

	// Empty and single stop lists return plain colors, not ColorFuncs.
	empty := mkLinear(nil, LinearLightInterpolation, PadSpread)
	if got := empty.GetColorFunction(1); got != (color.NRGBA{255, 0, 255, 255}) {
		t.Errorf("empty stops = %v, want magenta error color", got)
	}
	single := mkLinear(bw[:1], PremulAlphaInterpolation, PadSpread)
	if got := single.GetColorFunction(1); got != (color.NRGBA{0, 0, 0, 255}) {
		t.Errorf("single stop = %v, want opaque black", got)
	}

	// Degenerate geometry: zero bounds, zero-length gradient vector,
	// zero radius — all must yield defined colors without NaN.
	zeroBounds := mkLinear(bw, LinearLightInterpolation, PadSpread)
	zeroBounds.Bounds = struct{ X, Y, W, H float64 }{}
	sampleAll(colorFuncOf(t, zeroBounds, 1))

	zeroVec := mkLinear(bw, PremulAlphaInterpolation, PadSpread)
	zeroVec.Points = [5]float64{0.5, 0.5, 0.5, 0.5, 0}
	if got := nrgba(colorFuncOf(t, zeroVec, 1)(100, 22)); got.A != 0xFF {
		t.Errorf("zero-length vector: alpha = %d, want 255 (first stop)", got.A)
	}

	zeroR := &Gradient{
		Points: [5]float64{0.5, 0.5, 0.5, 0.5, 0}, IsRadial: true,
		Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
		Matrix: Identity, Interpolation: LinearLightInterpolation, Stops: bw,
	}
	_ = nrgba(colorFuncOf(t, zeroR, 1)(50, 50))
}

// TestConcurrentColorFunc exercises concurrent sampling of one color
// function and concurrent color-function construction on one Gradient.
// Run with -race to validate there are no data races.
func TestConcurrentColorFunc(t *testing.T) {
	g := mkLinear(messyStops(), LinearLightInterpolation, ReflectSpread)
	cf := colorFuncOf(t, g, 0.9)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				_ = cf((i*7+w)%300-50, i%9)
			}
		}(w)
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = g.GetColorFunctionUS(0.5, Identity)
			}
		}()
	}
	wg.Wait()
}
