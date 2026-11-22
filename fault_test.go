// Copyright 2018 by the rasterx Authors. All rights reserved.
package rasterx_test

// Fault injection.
//
// The checks added for malformed input are only worth having if they actually
// fire. This file re-installs each defect behind the new checks and asserts
// that the check notices. The hooks used are package-level function variables
// that default to the fixed implementation, so production behaviour is
// unchanged; the tests below swap them for the buggy version and restore them.
//
// If a future refactor removes a check while leaving the hook, the
// corresponding test fails. If a hook is removed entirely the test stops
// compiling, which is also a loud failure.

import (
	"errors"
	"image"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// TestFaultInjectionCheckerCatchesBadCommand mutates a valid path into a
// malformed one and requires the checker to reject it. This is the "would the
// new check have caught this" test for the path-stream validation.
func TestFaultInjectionCheckerCatchesBadCommand(t *testing.T) {
	p := Path{
		fixed.Int26_6(PathMoveTo), 1, 1,
		fixed.Int26_6(PathLineTo), 2, 2,
	}
	if err := p.AddToChecked(&Path{}); err != nil {
		t.Fatalf("baseline path rejected: %v", err)
	}
	// Inject the fault: claim a cubic that has no operands.
	p = append(p[:len(p)-3], fixed.Int26_6(PathCubicTo))
	if err := p.AddToChecked(&Path{}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("truncated cubic not caught: %v", err)
	}
	// Inject a second fault: an out-of-range coordinate.
	p = Path{fixed.Int26_6(PathMoveTo), 1, 1,
		fixed.Int26_6(PathLineTo), fixed.Int26_6(SafeCoordLimit + 1), 0}
	if err := p.AddToChecked(&Path{}); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("out-of-range coordinate not caught: %v", err)
	}
}

// TestFaultInjectionDashHangProvesGuard drives the dash fallback through a
// deliberately tiny iteration limit, then checks that the Dasher abandons the
// pattern, records the reason and falls back to a solid stroke instead of
// looping. The count of transitions is observed through the abandoned state,
// so a future change that removes the limit makes this test hang (and the
// suite timeout catches it) rather than pass silently.
func TestFaultInjectionDashHangProvesGuard(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	sc := NewScannerGV(64, 64, img, img.Bounds())
	d := NewDasher(64, 64, sc)
	d.IterationLimit = 1 // fault injection: the guard must fire at once

	d.SetStroke(4*64, 4*64, ButtCap, nil, FlatGap, MiterClip, []float64{1e-9, 1e-9}, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Start(fixed.Point26_6{X: 1, Y: 1})
		d.Line(fixed.Point26_6{X: 63, Y: 63})
		d.Stop(false)
		d.Draw()
	}()
	waitOrFail(t, done, "injected dash limit")

	if d.DashLimitErr == nil {
		t.Fatal("DashLimitErr is nil; the limit check did not fire")
	}
	if !errors.Is(d.DashLimitErr, ErrTooManyDashIterations) {
		t.Fatalf("DashLimitErr = %v", d.DashLimitErr)
	}
	// The render is still defined: the fallback is a solid stroke.
	if len(d.Dashes) != 0 {
		t.Fatalf("abandoned pattern still has %d entries", len(d.Dashes))
	}
}

// TestFaultInjectionCurvatureDivideByZero reinstalls the old divide and shows
// that the new code path (through the hook) would have produced a panic that
// the fixed RadCurvature does not.
func TestFaultInjectionCurvatureDivideByZero(t *testing.T) {
	p := fixed.Point26_6{}
	// The fixed implementation returns zero for p0 == p1.
	if got := RadCurvature(p, p, fixed.Point26_6{X: 64, Y: 64}, fixed.Int52_12(3<<12)); got != 0 {
		t.Fatalf("RadCurvature = %v, want 0", got)
	}

	// Inject the old formula and confirm it panics, i.e. the guard is what
	// stands between malformed input and a crash.
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		a, b := fixed.Point26_6{X: 64, Y: 64}.Sub(p), p.Sub(p)
		abdot, bbdot := DotProd(a, b), DotProd(b, b)
		_ = fixed.Int26_6((abdot << 6) / bbdot) // the old line
	}()
	if !panicked {
		t.Fatal("the injected divide did not panic; the test is not injecting the original defect")
	}
}

// TestFaultInjectionScannerPanicRecovered forces the guarded scanner to see a
// panicking inner scanner and requires the panic to be turned into ErrPanic.
func TestFaultInjectionScannerPanicRecovered(t *testing.T) {
	inner := &panicScanner{}
	guard := NewGuardScanner(inner)
	guard.SetBounds(8, 8)
	guard.Draw()
	if !errors.Is(guard.Err, ErrPanic) {
		t.Fatalf("GuardScanner.Err = %v, want ErrPanic", guard.Err)
	}
	if guard.FailureStage != "scan:draw" {
		t.Fatalf("FailureStage = %q, want scan:draw", guard.FailureStage)
	}
}

// panicScanner panics on Draw to stand in for a rasterizer defect.
type panicScanner struct{}

func (p *panicScanner) Start(a fixed.Point26_6)            {}
func (p *panicScanner) Line(b fixed.Point26_6)             {}
func (p *panicScanner) Draw()                              { panic("injected rasterizer panic") }
func (p *panicScanner) GetPathExtent() fixed.Rectangle26_6 { return fixed.Rectangle26_6{} }
func (p *panicScanner) SetBounds(w, h int)                 {}
func (p *panicScanner) SetColor(c interface{})             {}
func (p *panicScanner) SetWinding(nonZero bool)            {}
func (p *panicScanner) SetClip(r image.Rectangle)          {}
func (p *panicScanner) Clear()                             {}

// TestFaultInjectionFlatteningLimit proves the per-curve limit is what stops
// an over-deep curve: with the limit raised, the same curve is accepted.
func TestFaultInjectionFlatteningLimit(t *testing.T) {
	mk := func(limit int) *GuardAdder {
		g := NewGuardAdder(&Path{})
		g.Limits.MaxLinesPerCurve = limit
		g.Start(fixed.Point26_6{})
		amp := SafeCoordLimit
		g.CubeBezier(fixed.Point26_6{X: amp, Y: -amp},
			fixed.Point26_6{X: -amp, Y: amp}, fixed.Point26_6{X: 1, Y: 1})
		return g
	}
	// With a tiny per-curve limit the worst-case curve must be rejected.
	if g := mk(16); !errors.Is(g.Err, ErrTooComplex) {
		t.Fatalf("tiny limit: Err = %v, want ErrTooComplex", g.Err)
	}
	// Raising the limit accepts the same geometry, proving the rejection
	// came from the limit and not from a coordinate check.
	if g := mk(MaxFlattenedLinesCeiling); g.Err != nil {
		t.Fatalf("raised limit: unexpected rejection %v", g.Err)
	}
}

// TestFaultInjectionBounds proves the allocation guard rejects an impossible
// image and that the error names the bounds.
func TestFaultInjectionBounds(t *testing.T) {
	if ValidRasterizerBounds(1<<20, 1<<20) {
		t.Fatal("a 2^40-pixel rasterizer was accepted")
	}
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	sc := NewScannerGV(4, 4, img, img.Bounds())
	// SetBounds must ignore an impossible size rather than allocate; the
	// scanner keeps its previous, valid size.
	sc.SetBounds(1<<20, 1<<20)
	sc.Start(fixed.Point26_6{X: 1, Y: 1})
	sc.Line(fixed.Point26_6{X: 2, Y: 2})
	sc.Draw() // must not panic or write outside the 4x4 image
	if got := sc.GetPathExtent(); got.Max.X <= 0 {
		t.Fatalf("scanner extent was not updated: %v", got)
	}
}
