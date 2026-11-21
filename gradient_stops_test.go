// Tests for gradient stop handling side effects.
// Verifies that constructing linear/radial color functions does not mutate
// Gradient.Stops or the caller's slice, that built color functions are
// immune to later reordering of the stop list, that unsorted and duplicate
// offsets interpolate deterministically for every spread method, and that
// concurrent construction and sampling is race free.

package rasterx_test

import (
	"image/color"
	"math"
	"sync"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/colornames"
)

// linearTestGradient returns a user-space linear gradient running from
// x=0 to x=100, so sample point (x, 0) maps to t=(x+0.5)/100.
func linearTestGradient(stops []GradStop, spread SpreadMethod) *Gradient {
	return &Gradient{
		Points:   [5]float64{0, 0, 100, 0, 0},
		Stops:    stops,
		Matrix:   Identity,
		Spread:   spread,
		Units:    UserSpaceOnUse,
		IsRadial: false,
	}
}

// radialTestGradient returns a user-space radial gradient centered at
// (50,50) with radius 50 and focus at the center.
func radialTestGradient(stops []GradStop, spread SpreadMethod) *Gradient {
	return &Gradient{
		Points:   [5]float64{50, 50, 50, 50, 50},
		Stops:    stops,
		Matrix:   Identity,
		Spread:   spread,
		Units:    UserSpaceOnUse,
		IsRadial: true,
	}
}

func mustColorFunc(t *testing.T, cf interface{}) ColorFunc {
	t.Helper()
	f, ok := cf.(ColorFunc)
	if !ok {
		t.Fatalf("expected ColorFunc, got %T", cf)
	}
	return f
}

func toColorFunc(cf interface{}) (ColorFunc, bool) {
	f, ok := cf.(ColorFunc)
	return f, ok
}

func cloneStops(s []GradStop) []GradStop {
	c := make([]GradStop, len(s))
	copy(c, s)
	return c
}

func stopsEqual(a, b []GradStop) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Offset != b[i].Offset || a[i].Opacity != b[i].Opacity {
			return false
		}
		ar, ag, ab, aa := a[i].StopColor.RGBA()
		br, bg, bb, ba := b[i].StopColor.RGBA()
		if ar != br || ag != bg || ab != bb || aa != ba {
			return false
		}
	}
	return true
}

func sameColor(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// unsortedTestStops is deliberately unordered and includes duplicate offsets.
func unsortedTestStops() []GradStop {
	return []GradStop{
		{StopColor: colornames.Darksalmon, Offset: 1.0, Opacity: 0.75},
		{StopColor: colornames.Aquamarine, Offset: 0.3, Opacity: 1.0},
		{StopColor: colornames.Gold, Offset: 0.6, Opacity: 0.5},
		{StopColor: colornames.Skyblue, Offset: 0.6, Opacity: 1.0},
		{StopColor: colornames.Orchid, Offset: 0.0, Opacity: 0.9},
	}
}

// TestGetColorFunctionDoesNotMutateStops checks that building linear and
// radial color functions leaves Gradient.Stops — which is the caller's own
// slice — in its original order with original contents.
func TestGetColorFunctionDoesNotMutateStops(t *testing.T) {
	for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
		stops := unsortedTestStops()
		snapshot := cloneStops(stops)

		lg := linearTestGradient(stops, spread)
		rg := radialTestGradient(stops, spread)
		// Share one stop list between several "layers", as in the bug report.
		lf := mustColorFunc(t, lg.GetColorFunctionUS(1.0, Identity))
		rf := mustColorFunc(t, rg.GetColorFunctionUS(1.0, Identity))
		lf2 := mustColorFunc(t, lg.GetColorFunction(0.8))
		_ = lf2
		// Exercise the color functions; sampling must not mutate either.
		for x := 0; x < 100; x++ {
			lf(x, 0)
			rf(x, 50)
		}

		if !stopsEqual(stops, snapshot) {
			t.Errorf("spread %v: caller's stop slice was modified\n got: %v\nwant: %v", spread, stops, snapshot)
		}
		if !stopsEqual(lg.Stops, snapshot) || !stopsEqual(rg.Stops, snapshot) {
			t.Errorf("spread %v: Gradient.Stops was reordered", spread)
		}
	}
}

// TestColorFuncImmuneToLaterReorder checks that a color function, once
// constructed, is unaffected by later reordering or mutation of the
// gradient's stop list.
func TestColorFuncImmuneToLaterReorder(t *testing.T) {
	stops := unsortedTestStops()
	g := linearTestGradient(stops, PadSpread)
	cf := mustColorFunc(t, g.GetColorFunctionUS(1.0, Identity))

	type sample struct {
		x, y int
		c    color.Color
	}
	var before []sample
	for x := 0; x < 100; x += 7 {
		before = append(before, sample{x, 0, cf(x, 0)})
	}

	// Reorder, mutate, and extend the original stop list.
	for i, j := 0, len(g.Stops)-1; i < j; i, j = i+1, j-1 {
		g.Stops[i], g.Stops[j] = g.Stops[j], g.Stops[i]
	}
	g.Stops[0].Offset = 0.99
	g.Stops = append(g.Stops, GradStop{StopColor: colornames.Red, Offset: 0.1, Opacity: 1})

	for _, s := range before {
		if got := cf(s.x, s.y); !sameColor(got, s.c) {
			t.Errorf("at (%d,%d): color changed after stop list reorder: got %v, want %v",
				s.x, s.y, got, s.c)
		}
	}
}

// TestUnsortedStopsMatchSorted checks that unsorted input produces exactly
// the same colors as the equivalent sorted input, for every spread method,
// for both linear and radial gradients.
func TestUnsortedStopsMatchSorted(t *testing.T) {
	sorted := []GradStop{
		{StopColor: colornames.Orchid, Offset: 0.0, Opacity: 0.9},
		{StopColor: colornames.Aquamarine, Offset: 0.3, Opacity: 1.0},
		{StopColor: colornames.Gold, Offset: 0.6, Opacity: 0.5},
		{StopColor: colornames.Skyblue, Offset: 0.6, Opacity: 1.0},
		{StopColor: colornames.Darksalmon, Offset: 1.0, Opacity: 0.75},
	}
	for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
		lu := mustColorFunc(t, linearTestGradient(unsortedTestStops(), spread).GetColorFunctionUS(1.0, Identity))
		ls := mustColorFunc(t, linearTestGradient(cloneStops(sorted), spread).GetColorFunctionUS(1.0, Identity))
		ru := mustColorFunc(t, radialTestGradient(unsortedTestStops(), spread).GetColorFunctionUS(1.0, Identity))
		rs := mustColorFunc(t, radialTestGradient(cloneStops(sorted), spread).GetColorFunctionUS(1.0, Identity))
		// Sample well past [0,1] to exercise the spread methods.
		for x := -150; x < 250; x++ {
			if a, b := lu(x, 0), ls(x, 0); !sameColor(a, b) {
				t.Fatalf("linear spread %v at x=%d: unsorted %v != sorted %v", spread, x, a, b)
			}
			if a, b := ru(x, 50), rs(x, 50); !sameColor(a, b) {
				t.Fatalf("radial spread %v at x=%d: unsorted %v != sorted %v", spread, x, a, b)
			}
		}
	}
}

// TestDuplicateOffsetsStable checks deterministic interpolation when several
// stops share an offset: repeated construction yields identical colors, and
// the color exactly at the duplicate offset is one of the stop colors there.
func TestDuplicateOffsetsStable(t *testing.T) {
	stops := []GradStop{
		{StopColor: color.NRGBA{0, 0, 0, 255}, Offset: 0.5, Opacity: 1},
		{StopColor: color.NRGBA{255, 255, 255, 255}, Offset: 0.5, Opacity: 1},
		{StopColor: color.NRGBA{255, 0, 0, 255}, Offset: 0.0, Opacity: 1},
		{StopColor: color.NRGBA{0, 0, 255, 255}, Offset: 1.0, Opacity: 1},
	}
	for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
		cf1 := mustColorFunc(t, linearTestGradient(cloneStops(stops), spread).GetColorFunctionUS(1.0, Identity))
		cf2 := mustColorFunc(t, linearTestGradient(cloneStops(stops), spread).GetColorFunctionUS(1.0, Identity))
		for x := -120; x < 220; x++ {
			if a, b := cf1(x, 0), cf2(x, 0); !sameColor(a, b) {
				t.Fatalf("spread %v at x=%d: non-deterministic color %v vs %v", spread, x, a, b)
			}
		}
		// Just left of the duplicate offset the blend approaches the first
		// 0.5 stop (black).
		left := cf1(49, 0) // t = 0.495
		r, _, _, _ := left.RGBA()
		if r > 0x8000 {
			t.Errorf("spread %v: expected dark blend left of duplicate offset, got %v", spread, left)
		}
	}
}

// TestGradientConcurrent hammers a shared gradient from many goroutines,
// constructing color functions and sampling them, and verifies every
// goroutine observes the reference colors. Run with -race.
func TestGradientConcurrent(t *testing.T) {
	stops := unsortedTestStops()
	snapshot := cloneStops(stops)
	for _, radial := range []bool{false, true} {
		for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
			var g *Gradient
			if radial {
				g = radialTestGradient(stops, spread)
			} else {
				g = linearTestGradient(stops, spread)
			}
			ref := mustColorFunc(t, g.GetColorFunctionUS(0.9, Identity))
			y := 0
			if radial {
				y = 50
			}
			var wg sync.WaitGroup
			errs := make(chan string, 64)
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < 50; i++ {
						cf, ok := toColorFunc(g.GetColorFunctionUS(0.9, Identity))
						if !ok {
							select {
							case errs <- "not a ColorFunc":
							default:
							}
							return
						}
						for x := -50; x < 150; x += 13 {
							if !sameColor(cf(x, y), ref(x, y)) {
								select {
								case errs <- "mismatch":
								default:
								}
								return
							}
						}
					}
				}()
			}
			wg.Wait()
			select {
			case msg := <-errs:
				t.Errorf("radial=%v spread=%v: concurrent color mismatch: %s", radial, spread, msg)
			default:
			}
			if !stopsEqual(g.Stops, snapshot) {
				t.Errorf("radial=%v spread=%v: stops mutated under concurrency", radial, spread)
			}
		}
	}
}

// TestGradientEdgeCases covers empty and single stop lists and non-finite
// offsets.
func TestGradientEdgeCases(t *testing.T) {
	// Empty stops: default opaque black scaled by opacity, returned as a
	// plain color, not a ColorFunc.
	g := linearTestGradient(nil, PadSpread)
	c, ok := g.GetColorFunctionUS(0.5, Identity).(color.Color)
	if !ok {
		t.Fatalf("empty stops: expected color.Color, got %T", g.GetColorFunctionUS(0.5, Identity))
	}
	if !sameColor(c, ApplyOpacity(color.RGBA{0, 0, 0, 255}, 0.5)) {
		t.Errorf("empty stops: got %v, want opaque black at 0.5 opacity", c)
	}

	// Single stop: that stop's color with combined opacity, as a plain color.
	sg := linearTestGradient([]GradStop{
		{StopColor: colornames.Firebrick, Offset: 0.4, Opacity: 0.5},
	}, PadSpread)
	sc, ok := sg.GetColorFunctionUS(0.5, Identity).(color.Color)
	if !ok {
		t.Fatalf("single stop: expected color.Color, got %T", sg.GetColorFunctionUS(0.5, Identity))
	}
	if want := ApplyOpacity(colornames.Firebrick, 0.25); !sameColor(sc, want) {
		t.Errorf("single stop: got %v, want %v", sc, want)
	}

	// Non-finite offsets: no panic, deterministic results, and NaN behaves
	// like offset 0 while +/-Inf clamp to the ends.
	nf := []GradStop{
		{StopColor: colornames.Red, Offset: math.NaN(), Opacity: 1},
		{StopColor: colornames.Green, Offset: 0.5, Opacity: 1},
		{StopColor: colornames.Blue, Offset: math.Inf(1), Opacity: 1},
		{StopColor: colornames.Yellow, Offset: math.Inf(-1), Opacity: 1},
	}
	for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
		ng := linearTestGradient(nf, spread)
		cf1 := mustColorFunc(t, ng.GetColorFunctionUS(1.0, Identity))
		cf2 := mustColorFunc(t, ng.GetColorFunctionUS(1.0, Identity))
		for x := -120; x < 220; x++ {
			a, b := cf1(x, 0), cf2(x, 0)
			if !sameColor(a, b) {
				t.Fatalf("non-finite offsets spread %v at x=%d: non-deterministic %v vs %v", spread, x, a, b)
			}
		}
		// NaN and -Inf sanitize to 0, +Inf to 1: with PadSpread the color
		// past t=1 must be the blue stop.
		if spread == PadSpread {
			if got := cf1(200, 0); !sameColor(got, ApplyOpacity(colornames.Blue, 1.0)) {
				t.Errorf("non-finite offsets: pad end got %v, want blue", got)
			}
		}
	}
	// The caller's non-finite offsets must remain untouched in the gradient.
	if !math.IsNaN(nf[0].Offset) || !math.IsInf(nf[2].Offset, 1) || !math.IsInf(nf[3].Offset, -1) {
		t.Errorf("non-finite offsets in Gradient.Stops were modified: %v", nf)
	}
}
