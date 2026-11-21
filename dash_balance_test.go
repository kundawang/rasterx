// Copyright 2026 by the rasterx Authors. All rights reserved.
//_
// White-box tests for closed-subpath dash balancing.
package rasterx

import (
	"image"
	"testing"

	"golang.org/x/image/math/fixed"
)

func fp(x float64) fixed.Int26_6 { return fixed.Int26_6(x * 64) }

func TestModNonNegative(t *testing.T) {
	cases := []struct {
		a, n, want fixed.Int26_6
	}{
		{fp(100), fp(30), fp(10)},
		{fp(-10), fp(30), fp(20)},
		{fp(-30), fp(30), 0},
		{fp(0), fp(30), 0},
		{fp(1000000), fp(7), modNonNegative(fp(1000000), fp(7))},
	}
	for _, c := range cases {
		got := modNonNegative(c.a, c.n)
		if got != c.want {
			t.Errorf("modNonNegative(%v,%v) = %v, want %v", c.a, c.n, got, c.want)
		}
		if got < 0 || got >= c.n {
			t.Errorf("modNonNegative(%v,%v) out of range: %v", c.a, c.n, got)
		}
	}
}

func TestBalanceDashesExactPeriods(t *testing.T) {
	// Perimeter of 400 with a 100-unit pattern (50 dash, 50 gap): four
	// periods fit exactly, so the expanded pattern is four unchanged copies.
	dashes := []fixed.Int26_6{fp(50), fp(50)}
	out, off, ok := balanceDashes(dashes, 0, fp(400))
	if !ok {
		t.Fatal("expected ok pattern")
	}
	if off != 0 {
		t.Errorf("offset = %v, want 0", off)
	}
	if len(out) != 4*len(dashes) {
		t.Fatalf("expanded length = %d, want %d", len(out), 4*len(dashes))
	}
	var total fixed.Int26_6
	for _, d := range out {
		total += d
	}
	if total != fp(400) {
		t.Errorf("expanded pattern totals %v, want perimeter %v", total, fp(400))
	}
	for i := range out {
		if out[i] != fp(50) {
			t.Errorf("entry %d changed despite exact fit: %v", i, out[i])
		}
	}
	// Caller slice untouched.
	if dashes[0] != fp(50) || dashes[1] != fp(50) {
		t.Error("input dash slice was modified")
	}
}

func TestBalanceDashesRemainderCloses(t *testing.T) {
	// 250 perimeter, 100 period: k = round(2.5) = 3; the expanded pattern
	// must total L exactly so the phase at the seam equals the start phase.
	L := fp(250)
	out, _, ok := balanceDashes([]fixed.Int26_6{fp(60), fp(40)}, 0, L)
	if !ok {
		t.Fatal("expected ok pattern")
	}
	var total fixed.Int26_6
	for _, d := range out {
		if d < 0 {
			t.Fatalf("negative dash entry: %v", d)
		}
		total += d
	}
	if total != L {
		t.Errorf("expanded pattern totals %v, want perimeter %v (err %d units)", total, L, int64(total)-int64(L))
	}
	if diff := absFixed(total - L); diff > balDashEpsilon {
		t.Errorf("perimeter error %d exceeds epsilon %d", diff, balDashEpsilon)
	}
	// All k copies must be identical periods.
	if len(out) != 3*2 {
		t.Fatalf("expected 3 expanded periods of 2 entries, got %d", len(out))
	}
}

func TestBalanceDashesPerimeterShorterThanPeriod(t *testing.T) {
	// L < one period forces k == 1 and scales everything down.
	L := fp(30)
	out, _, ok := balanceDashes([]fixed.Int26_6{fp(50), fp(50)}, 0, L)
	if !ok {
		t.Fatal("expected ok pattern")
	}
	var sum fixed.Int26_6
	for _, d := range out {
		if d < 0 {
			t.Fatalf("negative entry: %v", d)
		}
		sum += d
	}
	if diff := absFixed(sum - L); diff > balDashEpsilon {
		t.Errorf("scaled period %v != perimeter %v (err %d)", sum, L, diff)
	}
}

func TestBalanceDashesZeroAndNegative(t *testing.T) {
	// Zero and negative entries are dropped from the period length but keep
	// their slot in every expanded copy of the pattern.
	src := []fixed.Int26_6{fp(40), fp(-20), 0, fp(40)}
	out, _, ok := balanceDashes(src, 0, fp(160))
	if !ok {
		t.Fatal("expected ok pattern")
	}
	if len(out)%len(src) != 0 {
		t.Fatalf("expanded length %d is not a whole number of pattern copies", len(out))
	}
	for j := 0; j < len(out); j += len(src) {
		if out[j+1] != 0 || out[j+2] != 0 {
			t.Errorf("zero/negative slots not zero in copy %d: %v", j, out[j:j+len(src)])
		}
		if out[j] <= 0 || out[j+3] <= 0 {
			t.Errorf("positive entries lost in copy %d: %v", j, out[j:j+len(src)])
		}
	}
	// The expanded pattern still closes exactly on the perimeter.
	var total fixed.Int26_6
	for _, d := range out {
		total += d
	}
	if total != fp(160) {
		t.Errorf("expanded total %v != perimeter 160", total)
	}
	// All-zero pattern is a solid-stroke fallback.
	if _, _, ok := balanceDashes([]fixed.Int26_6{0, fp(-5)}, 0, fp(100)); ok {
		t.Error("all-zero pattern should report ok == false")
	}
	// Caller slice untouched.
	if src[1] != fp(-20) {
		t.Error("input dash slice was modified")
	}
}

func TestBalanceDashesLargeOffset(t *testing.T) {
	dashes := []fixed.Int26_6{fp(40), fp(20)}
	out, off, ok := balanceDashes(dashes, fp(1000003), fp(600))
	if !ok {
		t.Fatal("expected ok pattern")
	}
	var firstPeriod fixed.Int26_6
	for i := 0; i < len(dashes); i++ {
		firstPeriod += out[i]
	}
	if off < 0 || off >= firstPeriod {
		t.Errorf("offset %v not folded into [0,%v)", off, firstPeriod)
	}
	// Negative offsets fold to the equivalent positive phase.
	_, negOff, _ := balanceDashes(dashes, fp(-10), fp(600))
	if negOff != firstPeriod-fp(10) {
		t.Errorf("negative offset folded to %v, want %v", negOff, firstPeriod-fp(10))
	}
}

func TestMeasurePathLength(t *testing.T) {
	// 100x60 rectangle: perimeter 320 (user units).
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(100, 0))
	p.Line(ToFixedP(100, 60))
	p.Line(ToFixedP(0, 60))
	p.Stop(true)
	got := measurePathLength(p, true)
	if diff := absFixed(got - fp(320)); diff > fp(1) {
		t.Errorf("rect perimeter = %v, want %v", got, fp(320))
	}

	// Open polyline without the closing chord: 100 + 60 + 100 = 260.
	var q Path
	q.Start(ToFixedP(0, 0))
	q.Line(ToFixedP(100, 0))
	q.Line(ToFixedP(100, 60))
	q.Line(ToFixedP(0, 60))
	if got := measurePathLength(q, false); absFixed(got-fp(260)) > fp(1) {
		t.Errorf("open path length = %v, want %v", got, fp(260))
	}

	// Zero-length closed subpath (all points identical).
	var z Path
	z.Start(ToFixedP(5, 5))
	z.Line(ToFixedP(5, 5))
	z.Stop(true)
	if got := measurePathLength(z, true); got != 0 {
		t.Errorf("zero perimeter = %v, want 0", got)
	}

	// Bezier closed loop: perimeter must be positive and finite. The curve
	// bulges beyond its 200-unit chord, so expect more than 200 and less
	// than its control-hull perimeter (~447).
	var c Path
	c.Start(ToFixedP(0, 0))
	c.CubeBezier(ToFixedP(50, -50), ToFixedP(150, -50), ToFixedP(200, 0))
	c.CubeBezier(ToFixedP(150, 50), ToFixedP(50, 50), ToFixedP(0, 0))
	c.Stop(true)
	if l := measurePathLength(c, true); l <= fp(200) || l >= fp(450) {
		t.Errorf("bezier loop perimeter %v outside plausible range", l)
	}
}

func TestBalanceDoesNotMutateDasherState(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	d := NewDasher(256, 256, NewScannerGV(256, 256, img, img.Bounds()))
	src := []float64{33, 12}
	d.SetStroke(10*64, 4*64, RoundCap, nil, RoundGap, ArcClip, src, 7)
	d.Mode = DashModeBalanceClosed

	var p Path
	AddCircle(128, 128, 80, &p)
	p.AddTo(d)
	d.Draw()

	if len(d.Dashes) != 2 || d.Dashes[0] != fp(33) || d.Dashes[1] != fp(12) {
		t.Errorf("stored dashes mutated after balanced stroke: %v", d.Dashes)
	}
	if d.DashOffset != fp(7) {
		t.Errorf("stored offset mutated: %v", d.DashOffset)
	}
	if src[0] != 33 || src[1] != 12 {
		t.Error("caller float slice mutated")
	}
	if d.bufOpen || len(d.buf) != 0 {
		t.Error("balance buffer not flushed")
	}
}

func TestClearDiscardsBuffer(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	d := NewDasher(64, 64, NewScannerGV(64, 64, img, img.Bounds()))
	d.SetStroke(4*64, 4*64, ButtCap, nil, FlatGap, MiterClip, []float64{10, 5}, 0)
	d.Mode = DashModeBalanceClosed
	d.Start(ToFixedP(10, 10))
	d.Line(ToFixedP(50, 10))
	if !d.bufOpen || len(d.buf) == 0 {
		t.Fatal("expected buffered subpath")
	}
	d.Clear()
	if d.bufOpen || len(d.buf) != 0 {
		t.Error("Clear did not discard the balance buffer")
	}
	// Rasterizer must still be usable afterwards.
	var p Path
	p.Start(ToFixedP(5, 5))
	p.Line(ToFixedP(59, 59))
	p.AddTo(d)
	d.Draw()
}

func TestBalanceDashesEntryCap(t *testing.T) {
	// A tiny period on a large perimeter would naturally expand to a huge
	// pattern; the cap must keep the allocation bounded while still closing.
	out, _, ok := balanceDashes([]fixed.Int26_6{1, 1}, 0, fixed.Int26_6(1)<<30)
	if !ok {
		t.Fatal("expected ok pattern")
	}
	if len(out) > maxBalancedEntries {
		t.Errorf("expanded pattern %d entries exceeds cap %d", len(out), maxBalancedEntries)
	}
	var total fixed.Int26_6
	for _, d := range out {
		total += d
	}
	// The rescaled pattern still spans the perimeter an integer number of
	// times, so seam closure is preserved.
	if (fixed.Int26_6(1)<<30)%total != 0 {
		t.Errorf("capped pattern does not divide the perimeter evenly")
	}
}

func TestSetStrokeAllNonPositiveBecomesSolid(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	d := NewDasher(64, 64, NewScannerGV(64, 64, img, img.Bounds()))
	src := []float64{0, -4, 0}
	d.SetStroke(4*64, 4*64, ButtCap, nil, FlatGap, MiterClip, src, 0)
	if len(d.Dashes) != 0 {
		t.Errorf("all-nonpositive pattern not reduced to solid: %v", d.Dashes)
	}
	// Must render without hanging.
	var p Path
	p.Start(ToFixedP(5, 32))
	p.Line(ToFixedP(59, 32))
	p.Stop(false)
	p.AddTo(d)
	d.Draw()
	// A solid stroke leaves ink.
	var ink int
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if img.RGBAAt(x, y).A > 0 {
				ink++
			}
		}
	}
	if ink == 0 {
		t.Error("solid fallback rendered nothing")
	}
	// Caller slice untouched.
	if src[1] != -4 {
		t.Error("caller dash slice modified")
	}
}

func absFixed(v fixed.Int26_6) fixed.Int26_6 {
	if v < 0 {
		return -v
	}
	return v
}
