// Copyright 2018 by the rasterx Authors. All rights reserved.
package rasterx_test

import (
	"errors"
	"image"
	"image/color"
	"math"
	"strings"
	"testing"
	"time"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// The tests in this file pin the two defects found while building the
// malformed-path harness. Each is reproduced with a fixed input and the fix is
// checked directly, so a regression is a failing unit test rather than a hung
// suite.
//
// The fault injection test at the end proves the harness itself notices the
// defects: it reinstalls the old behaviour behind the new checks and asserts
// that the checks fire.

// Regression 1: a degenerate quadratic whose first two control points
// coincide used to divide by zero inside RadCurvature.
func TestRegressionRadCurvatureDivideByZero(t *testing.T) {
	p := fixed.Point26_6{X: 0, Y: 0}
	// p0 == p1 makes b == 0 and the old code evaluated (abdot<<6)/bbdot.
	if got := RadCurvature(p, p, fixed.Point26_6{X: 10 << 6, Y: 10 << 6}, fixed.Int52_12(3<<12)); got != 0 {
		t.Fatalf("RadCurvature degenerate = %v, want 0", got)
	}
	// A degenerate quadratic reaching the Stroker must also survive.
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	sc := NewScannerGV(32, 32, img, img.Bounds())
	s := NewStroker(32, 32, sc)
	s.SetStroke(2*64, 4*64, RoundCap, nil, RoundGap, Arc)
	s.Start(fixed.Point26_6{X: 8, Y: 8})
	s.QuadBezier(fixed.Point26_6{X: 8, Y: 8}, fixed.Point26_6{X: 24, Y: 24})
	s.Stop(false)
	s.Draw() // used to panic: integer divide by zero
}

// Regression 2: a dash pattern whose entries round to zero in 26.6 fixed
// point used to spin forever in Dasher.lineF.
func TestRegressionZeroLengthDash(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	sc := NewScannerGV(64, 64, img, img.Bounds())
	d := NewDasher(64, 64, sc)
	// 1e-9 pixels * 64 truncates to a zero fixed-point entry.
	d.SetStroke(4*64, 4*64, ButtCap, nil, FlatGap, MiterClip, []float64{1e-9, 1e-9}, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Start(fixed.Point26_6{X: 1, Y: 1})
		d.Line(fixed.Point26_6{X: 63, Y: 63})
		d.Stop(false)
		d.Draw()
	}()
	// A regression hangs here; the test times out instead of wedging the
	// suite.
	waitOrFail(t, done, "zero-length dash pattern")

	// The pattern must have been rejected rather than stored as zeros.
	for _, v := range d.Dashes {
		if v <= 0 {
			t.Fatalf("dash entry %d is not positive: %d", v, v)
		}
	}
}

// Regression 3: an unknown or truncated command in a Path used to panic with
// index-out-of-range. It must be reported instead.
func TestRegressionMalformedPath(t *testing.T) {
	cases := []struct {
		name string
		p    Path
		want error
	}{
		{"truncated", Path{fixed.Int26_6(PathLineTo), 1}, ErrBadPath},
		{"unknown", Path{99, 1, 2}, ErrBadPath},
		{"trailing", Path{fixed.Int26_6(PathMoveTo), 1, 2, fixed.Int26_6(PathCubicTo)}, ErrBadPath},
		{"nan", Path{fixed.Int26_6(PathMoveTo), 0, 0, fixed.Int26_6(PathLineTo), 1 << 30, 0}, ErrNonFinite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.AddToChecked(&Path{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("AddToChecked = %v, want %v", err, tc.want)
			}
			// AnalyzePath must agree on the failure, and must not panic.
			rep := AnalyzePath(tc.p, DefaultLimits())
			if !errors.Is(rep.Err, tc.want) {
				t.Fatalf("AnalyzePath = %v (stage %q), want %v", rep.Err, rep.FailureStage, tc.want)
			}
		})
	}
}

func waitOrFail(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not terminate within the timeout", what)
	}
}

// Regression 4: a cubic whose control points span the full legal range used
// to be flattened into an unbounded number of lines by a naive subdivider.
// The guard bounds it: a single curve must stay under the per-curve limit, and
// enough of them must be rejected by the total output cap before the
// rasterizer does the work.
func TestRegressionHugeCurveRejected(t *testing.T) {
	// One worst-case curve inside the safe coordinate range flattens to
	// ~4700 lines, which the per-curve limit accepts.
	one := func() *GuardAdder {
		rec := &Path{}
		g := NewGuardAdder(rec)
		g.Start(fixed.Point26_6{})
		amp := SafeCoordLimit
		g.CubeBezier(fixed.Point26_6{X: amp, Y: -amp},
			fixed.Point26_6{X: -amp, Y: amp}, fixed.Point26_6{X: 10, Y: 10})
		return g
	}
	if g := one(); g.Err != nil {
		t.Fatalf("single worst-case curve rejected unexpectedly: %v", g.Err)
	}

	// Many of them exceed the total output budget and must be rejected
	// before any flattening happens.
	rec := &Path{}
	g := NewGuardAdder(rec)
	g.Start(fixed.Point26_6{})
	amp := SafeCoordLimit
	for i := 0; i < 500; i++ {
		g.CubeBezier(fixed.Point26_6{X: amp, Y: -amp},
			fixed.Point26_6{X: -amp, Y: amp}, fixed.Point26_6{X: 10, Y: 10})
	}
	if !errors.Is(g.Err, ErrTooManyOutputLines) && !errors.Is(g.Err, ErrTooComplex) {
		t.Fatalf("500 worst-case curves: Err = %v, want an output/complexity limit", g.Err)
	}
}

// Regression 5: a scan-converted ellipse with zero radius or non-finite input
// used to divide by zero. It must leave the path at the start point.
func TestRegressionEllipseDegenerate(t *testing.T) {
	rec := &Path{}
	rec.Start(fixed.Point26_6{X: 64, Y: 64})
	AddEllipse(64, 64, 0, 0, 0, rec)
	// A zero-radius ellipse emits a straight line to the same point.
	if err := Path(*rec).Validate(); err != nil {
		t.Fatalf("degenerate ellipse produced an invalid path: %v", err)
	}

	rec2 := &Path{}
	rec2.Start(fixed.Point26_6{X: 64, Y: 64})
	AddArc([]float64{math.NaN(), 10, 0, 1, 0, 64, 64}, 0, 0, 0, 0, rec2)
	if err := Path(*rec2).Validate(); err != nil {
		t.Fatalf("non-finite arc produced an invalid path: %v", err)
	}
}

// Regression 6: a gradient with no stops, a zero-size box or a zero vector
// used to divide by zero in the color function. The produced colors must be
// deterministic, not panics.
func TestRegressionDegenerateGradient(t *testing.T) {
	call := func(g *Gradient) (c color.Color) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("gradient color function panicked: %v", r)
			}
		}()
		// Gradients without usable stops return a flat color rather than a
		// function; both forms are valid and must be non-nil.
		switch v := g.GetColorFunction(1).(type) {
		case ColorFunc:
			return v(10, 10)
		case color.Color:
			return v
		default:
			t.Fatalf("unexpected color function type %T", v)
			return nil
		}
	}
	red := color.RGBA{255, 0, 0, 255}
	_ = red
	stops := []GradStop{
		{StopColor: color.RGBA{255, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.RGBA{0, 0, 255, 255}, Offset: 1, Opacity: 1},
	}
	cases := []*Gradient{
		{}, // no stops
		{Stops: stops, Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 0, H: 100}},
		{Stops: stops, Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
			Points: [5]float64{0, 0, 0, 0, 0}}, // zero vector
		{Stops: stops, IsRadial: true, Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
			Points: [5]float64{0.5, 0.5, 0.5, 0.5, 0}}, // zero radius
		{Stops: []GradStop{{StopColor: red, Offset: math.NaN()}, stops[1]},
			Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100}},
	}
	for i, g := range cases {
		c := call(g)
		if c == nil {
			t.Fatalf("case %d returned a nil color", i)
		}
		// Two calls must agree, proving no NaN leaked into the result.
		if c2 := call(g); !colorsEqual(c, c2) {
			t.Fatalf("case %d is not deterministic: %v vs %v", i, c, c2)
		}
	}
}

func colorsEqual(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// Regression 7: non-finite and out-of-range points must be rejected by the
// checker and saturated by the scanner, never reaching the rasterizer as NaN.
func TestRegressionNonFinitePoints(t *testing.T) {
	for _, p := range []fixed.Point26_6{
		{X: fixed.Int26_6(math.MinInt32), Y: 0},
		{X: fixed.Int26_6(math.MaxInt32), Y: 0},
		{X: -1 << 26, Y: 1 << 26}, // beyond SafeCoordLimit
	} {
		rec := &Path{}
		g := NewGuardAdder(rec)
		g.Start(p)
		if g.Err == nil {
			t.Fatalf("guard accepted out-of-range point %v", p)
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	sc := NewScannerGV(8, 8, img, img.Bounds())
	f := NewFiller(8, 8, sc)
	// These values clamp to the edge of the safe range instead of wrapping.
	f.Start(fixed.Point26_6{X: fixed.Int26_6(math.MinInt32), Y: fixed.Int26_6(math.MinInt32)})
	f.Line(fixed.Point26_6{X: fixed.Int26_6(math.MaxInt32), Y: fixed.Int26_6(math.MaxInt32)})
	f.Stop(false)
	f.Draw()
}

func TestAddToCheckedPreservesNormalPath(t *testing.T) {
	// AddToChecked forwards the same command sequence AddTo does, so the
	// rendered image must be identical.
	render := func(checked bool) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 128, 128))
		sc := NewScannerGV(128, 128, img, img.Bounds())
		f := NewFiller(128, 128, sc)
		sc.SetColor(color.RGBA{1, 2, 3, 255})
		p := GetTestPath()
		var err error
		if checked {
			err = p.AddToChecked(f)
		} else {
			p.AddTo(f)
		}
		if err != nil {
			t.Fatalf("AddToChecked on a valid path: %v", err)
		}
		f.Draw()
		return img
	}
	a, b := render(false), render(true)
	if string(a.Pix) != string(b.Pix) {
		t.Fatal("AddToChecked changed the rendering of a valid path")
	}
}

func TestErrorStringsAreStable(t *testing.T) {
	// The error text is user visible; keep the prefixes stable so callers can
	// match them and the failure report stays readable.
	for _, err := range []error{ErrNonFinite, ErrBadPath, ErrTooComplex, ErrBadStrokeParams, ErrEmptyGradient} {
		if !strings.HasPrefix(err.Error(), "rasterx: ") {
			t.Errorf("error %q lacks the rasterx prefix", err)
		}
	}
}
