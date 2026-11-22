package rasterx

import (
	"image/color"
	"math"
	"math/rand"
	"sync"
	"testing"
)

// testStops is a periodic ramp: the first and last stops coincide, so
// the repeat spread has no seam and the LUT must track the reference
// everywhere.
func testStops() []*GradStop {
	return []*GradStop{
		{StopColor: color.NRGBA{0xFF, 0x00, 0x00, 0xFF}, Offset: 0.0, Opacity: 1.0},
		{StopColor: color.NRGBA{0x00, 0xFF, 0x00, 0xFF}, Offset: 0.35, Opacity: 0.8},
		{StopColor: color.NRGBA{0x00, 0x00, 0xFF, 0xFF}, Offset: 0.7, Opacity: 1.0},
		{StopColor: color.NRGBA{0xFF, 0x00, 0x00, 0xFF}, Offset: 1.0, Opacity: 1.0},
	}
}

func testLinearGradient(spread SpreadMethod) *Gradient {
	g := &Gradient{Points: [5]float64{0, 0, 1, 0, 0},
		IsRadial: false, Matrix: Identity, Spread: spread}
	g.Bounds.X, g.Bounds.Y, g.Bounds.W, g.Bounds.H = 10, 20, 200, 150
	g.Stops = testStops()
	return g
}

func testRadialGradient(spread SpreadMethod) *Gradient {
	g := &Gradient{Points: [5]float64{0.5, 0.5, 0.5, 0.5, 0.5},
		IsRadial: true, Matrix: Identity, Spread: spread}
	g.Bounds.X, g.Bounds.Y, g.Bounds.W, g.Bounds.H = 0, 0, 200, 200
	g.Stops = testStops()
	return g
}

func nrgbaOf(c color.Color) color.NRGBA {
	return color.NRGBAModel.Convert(c).(color.NRGBA)
}

func maxDelta(a, b color.NRGBA) int {
	d := 0
	for _, v := range [4]int{
		int(a.R) - int(b.R), int(a.G) - int(b.G),
		int(a.B) - int(b.B), int(a.A) - int(b.A)} {
		if v < 0 {
			v = -v
		}
		if v > d {
			d = v
		}
	}
	return d
}

// TestLUTMatchesUncached compares the LUT-backed color function against
// the direct per-pixel stop interpolation for all three spread methods,
// over a grid of pixels. The LUT must reproduce the reference colors
// within one least-significant bit per channel.
func TestLUTMatchesUncached(t *testing.T) {
	for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
		for _, mk := range []func(SpreadMethod) *Gradient{testLinearGradient, testRadialGradient} {
			grad := mk(spread)
			lutF, ok := grad.GetColorFunction(0.9).(ColorFunc)
			if !ok {
				t.Fatalf("spread %d: GetColorFunction did not return a ColorFunc", spread)
			}
			refF := grad.getColorFunctionUncached(0.9).(ColorFunc)
			worst := 0
			for y := 0; y < 220; y += 3 {
				for x := 0; x < 220; x += 3 {
					d := maxDelta(nrgbaOf(lutF(x, y)), nrgbaOf(refF(x, y)))
					if d > worst {
						worst = d
					}
				}
			}
			if worst > 1 {
				t.Errorf("spread %d: worst channel delta %d > 1", spread, worst)
			}
		}
	}
}

// TestLUTGridPointsExact checks that LUT entries at the sample points
// are exactly the colors produced by the direct evaluation.
func TestLUTGridPointsExact(t *testing.T) {
	for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
		stops := testStops()
		sortStopsByOffset(stops)
		const size = 256
		lut := buildGradientLUT(stops, spread, 0.7, size)
		snap := &Gradient{Stops: stops, Spread: spread}
		for i := 0; i < size; i++ {
			// Sample exactly on table entries: repeat uses a periodic
			// table (t = i/size), the others t = i/(size-1).
			tv := float64(i) / float64(size-1)
			if spread == RepeatSpread {
				tv = float64(i) / float64(size)
			}
			got := lut.at(tv)
			want := toNRGBA(snap.tColor(tv, 0.7))
			if d := maxDelta(got, want); d != 0 {
				t.Fatalf("spread %d t=%f: delta %d, got %v want %v", spread, tv, d, got, want)
			}
		}
	}
}

// TestLUTRepeatSeam uses a non-periodic ramp (the repeat seam is a real
// discontinuity in the reference) and verifies the LUT error is confined
// to one table cell around the wrap.
func TestLUTRepeatSeam(t *testing.T) {
	stops := testStops()
	stops[3] = &GradStop{StopColor: color.NRGBA{0xFF, 0xFF, 0x00, 0xFF}, Offset: 1.0, Opacity: 0.5}
	sortStopsByOffset(stops)
	const size = 1024
	lut := buildGradientLUT(stops, RepeatSpread, 1.0, size)
	ref := &Gradient{Stops: stops, Spread: RepeatSpread}
	rng := rand.New(rand.NewSource(7))
	bad := 0
	for i := 0; i < 100000; i++ {
		tv := rng.Float64()*8 - 4
		if d := maxDelta(lut.at(tv), toNRGBA(ref.tColor(tv, 1.0))); d > 1 {
			_, frac := math.Modf(math.Abs(tv))
			dist := math.Min(frac, 1-frac) // distance to the wrap
			if dist > 1.5/size {
				t.Fatalf("t=%f: delta %d away from seam", tv, d)
			}
			bad++
		}
	}
	if bad == 0 {
		t.Log("no seam-straddling samples hit; test may be too lax")
	}
}

// TestLUTReflectFullRange verifies the reflect fold against the direct
// reflect implementation over t in [0,2) and beyond.
func TestLUTReflectFullRange(t *testing.T) {
	stops := testStops()
	sortStopsByOffset(stops)
	lut := buildGradientLUT(stops, ReflectSpread, 1.0, 1024)
	ref := &Gradient{Stops: stops, Spread: ReflectSpread}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		tv := rng.Float64()*8 - 4 // exercise negative and multi-period t
		got := lut.at(tv)
		want := toNRGBA(ref.tColor(tv, 1.0))
		if d := maxDelta(got, want); d > 1 {
			t.Fatalf("reflect t=%f: delta %d, got %v want %v", tv, d, got, want)
		}
	}
}

// TestLUTEmptyAndSingleStop keeps the legacy behavior: no stops yields
// the magenta error color, one stop yields that (opaque) color, and
// neither touches the LUT cache.
func TestLUTEmptyAndSingleStop(t *testing.T) {
	ClearGradientLUTCache()
	g := testLinearGradient(PadSpread)
	g.Stops = nil
	if c, ok := g.GetColorFunction(1).(color.NRGBA); !ok || c != (color.NRGBA{255, 0, 255, 255}) {
		t.Errorf("empty stops: got %v", g.GetColorFunction(1))
	}
	g.Stops = []*GradStop{{StopColor: color.NRGBA{1, 2, 3, 255}, Offset: 0.5, Opacity: 1}}
	if c, ok := g.GetColorFunction(1).(color.NRGBA); !ok || c != (color.NRGBA{1, 2, 3, 255}) {
		t.Errorf("single stop: got %v", g.GetColorFunction(1))
	}
	if n, _, _, _ := GradientLUTCacheStats(); n != 0 {
		t.Errorf("empty/single stop gradients must not populate the LUT cache, got %d entries", n)
	}
}

// TestLUTDuplicateOffsets ensures duplicate stop offsets (including a
// zero-width segment) neither crash nor corrupt the ramp. Away from the
// discontinuity the LUT must track the reference; within one LUT cell of
// the step the lerp may straddle it.
func TestLUTDuplicateOffsets(t *testing.T) {
	stops := []*GradStop{
		{StopColor: color.NRGBA{0xFF, 0x00, 0x00, 0xFF}, Offset: 0.0, Opacity: 1},
		{StopColor: color.NRGBA{0x00, 0xFF, 0x00, 0xFF}, Offset: 0.5, Opacity: 1},
		{StopColor: color.NRGBA{0x00, 0x00, 0xFF, 0xFF}, Offset: 0.5, Opacity: 1}, // hard step
		{StopColor: color.NRGBA{0xFF, 0xFF, 0x00, 0xFF}, Offset: 1.0, Opacity: 1},
	}
	for _, spread := range []SpreadMethod{PadSpread, ReflectSpread, RepeatSpread} {
		g := &Gradient{Points: [5]float64{0, 0, 1, 0, 0}, Matrix: Identity, Spread: spread}
		g.Bounds.X, g.Bounds.Y, g.Bounds.W, g.Bounds.H = 0, 0, 100, 100
		g.Stops = append([]*GradStop(nil), stops...)
		lutF, ok := g.GetColorFunction(1).(ColorFunc)
		if !ok {
			t.Fatalf("spread %d: expected ColorFunc", spread)
		}
		refF := g.getColorFunctionUncached(1).(ColorFunc)
		cell := 100.0 / float64(gradientLUTSize-1) // x pixels per LUT cell
		for x := 0; x < 100; x++ {
			// skip pixels whose t falls within one cell of the step at t=0.5
			if math.Abs(float64(x)+0.5-50) < cell+1 {
				continue
			}
			if d := maxDelta(nrgbaOf(lutF(x, 50)), nrgbaOf(refF(x, 50))); d > 1 {
				t.Errorf("spread %d x=%d: delta %d", spread, x, d)
			}
		}
	}
}

// TestLUTCacheIsolation verifies that gradients differing in stops,
// spread, opacity, matrix, units, bounds or points never share a cached
// LUT: each variant must be a cache miss and produce its own colors.
func TestLUTCacheIsolation(t *testing.T) {
	ClearGradientLUTCache()
	base := testLinearGradient(PadSpread)

	variants := []*Gradient{base}
	// different stop color
	g2 := testLinearGradient(PadSpread)
	g2.Stops[1] = &GradStop{StopColor: color.NRGBA{0x12, 0x34, 0x56, 0xFF}, Offset: 0.35, Opacity: 0.8}
	variants = append(variants, g2)
	// different stop offset
	g3 := testLinearGradient(PadSpread)
	g3.Stops[1] = &GradStop{StopColor: color.NRGBA{0x00, 0xFF, 0x00, 0xFF}, Offset: 0.36, Opacity: 0.8}
	variants = append(variants, g3)
	// different spread
	variants = append(variants, testLinearGradient(RepeatSpread))
	// different matrix
	g5 := testLinearGradient(PadSpread)
	g5.Matrix = Identity.Translate(5, 0)
	variants = append(variants, g5)
	// different units
	g6 := testLinearGradient(PadSpread)
	g6.Units = UserSpaceOnUse
	variants = append(variants, g6)
	// different bounds
	g7 := testLinearGradient(PadSpread)
	g7.Bounds.W = 300
	variants = append(variants, g7)

	for i, g := range variants {
		g.GetColorFunction(1.0)
		if n, _, _, _ := GradientLUTCacheStats(); n != i+1 {
			t.Fatalf("variant %d: expected %d cache entries, got %d", i, i+1, n)
		}
	}
	// A second pass must be all hits and identical colors.
	for i, g := range variants {
		before, _, hits, _ := GradientLUTCacheStats()
		f1 := g.GetColorFunction(1.0).(ColorFunc)
		after, _, hits2, _ := GradientLUTCacheStats()
		if after != before || hits2 != hits+1 {
			t.Errorf("variant %d: second GetColorFunction should hit the cache", i)
		}
		ref := g.getColorFunctionUncached(1.0).(ColorFunc)
		if d := maxDelta(nrgbaOf(f1(40, 40)), nrgbaOf(ref(40, 40))); d > 1 {
			t.Errorf("variant %d: cached LUT color delta %d", i, d)
		}
	}
}

// TestLUTSnapshotAndInvalidation checks the snapshot rule: mutating the
// gradient after GetColorFunction does not change already-issued color
// functions, and the next GetColorFunction call sees the new state.
func TestLUTSnapshotAndInvalidation(t *testing.T) {
	ClearGradientLUTCache()
	g := testLinearGradient(PadSpread)
	before := g.GetColorFunction(1.0).(ColorFunc)
	cBefore := nrgbaOf(before(60, 60))

	// Mutate stops and matrix.
	g.Stops[0].StopColor = color.NRGBA{0x00, 0x00, 0x00, 0xFF}
	g.Stops = append(g.Stops, &GradStop{StopColor: color.NRGBA{0x80, 0x80, 0x80, 0xFF}, Offset: 0.15, Opacity: 1})
	g.Matrix = Identity.Translate(3, 0)

	if c := nrgbaOf(before(60, 60)); c != cBefore {
		t.Errorf("previously returned ColorFunc changed after mutation: %v -> %v", cBefore, c)
	}
	after := g.GetColorFunction(1.0).(ColorFunc)
	if nrgbaOf(after(60, 60)) == cBefore {
		t.Errorf("GetColorFunction after mutation returned stale colors")
	}
	if n, _, _, _ := GradientLUTCacheStats(); n != 2 {
		t.Errorf("expected 2 cache entries after mutation, got %d", n)
	}
}

// TestLUTCacheLimits checks entry/memory bounds and LRU eviction.
func TestLUTCacheLimits(t *testing.T) {
	ClearGradientLUTCache()
	defer func() {
		SetGradientLUTCacheLimit(DefaultMaxGradientLUTEntries, DefaultMaxGradientLUTBytes)
		ClearGradientLUTCache()
	}()

	SetGradientLUTCacheLimit(4, 4*gradientLUTSize*4)
	for i := 0; i < 10; i++ {
		g := testLinearGradient(PadSpread)
		g.Matrix = Identity.Translate(float64(i), 0) // distinct keys
		g.GetColorFunction(1.0)
	}
	n, b, _, _ := GradientLUTCacheStats()
	if n > 4 {
		t.Errorf("entries %d exceed limit 4", n)
	}
	if b > 4*gradientLUTSize*4 {
		t.Errorf("bytes %d exceed limit %d", b, 4*gradientLUTSize*4)
	}

	// Zero limits disable caching entirely.
	SetGradientLUTCacheLimit(0, 0)
	ClearGradientLUTCache()
	g := testLinearGradient(PadSpread)
	g.GetColorFunction(1.0)
	g.GetColorFunction(1.0)
	if n, _, _, misses := GradientLUTCacheStats(); n != 0 || misses != 2 {
		t.Errorf("disabled cache: entries=%d misses=%d, want 0 and 2", n, misses)
	}
}

// TestLUTConcurrent hammers the cache and LUTs from many goroutines;
// run with -race. Readers must observe valid colors throughout, and
// ClearGradientLUTCache must be safe against concurrent use.
func TestLUTConcurrent(t *testing.T) {
	ClearGradientLUTCache()
	grads := make([]*Gradient, 4)
	for i := range grads {
		grads[i] = testLinearGradient(SpreadMethod(i % 3))
		grads[i].Matrix = Identity.Translate(float64(i), 0)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				g := grads[(w+i)%len(grads)]
				f := g.GetColorFunction(1.0).(ColorFunc)
				for p := 0; p < 64; p++ {
					c := nrgbaOf(f(p, p))
					if c.A == 0 && g.Stops[0].Opacity != 0 {
						t.Errorf("unexpected transparent pixel")
						return
					}
				}
				if i%50 == 0 {
					ClearGradientLUTCache()
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestLUTSizeClamp verifies the LUT resolution limits.
func TestLUTSizeClamp(t *testing.T) {
	ClearGradientLUTCache()
	defer func() { SetGradientLUTSize(DefaultGradientLUTSize); ClearGradientLUTCache() }()

	SetGradientLUTSize(1)
	g := testLinearGradient(PadSpread)
	g.GetColorFunction(1.0)
	if _, b, _, _ := GradientLUTCacheStats(); b != MinGradientLUTSize*4 {
		t.Errorf("min clamp: bytes=%d want %d", b, MinGradientLUTSize*4)
	}
	ClearGradientLUTCache()
	SetGradientLUTSize(1 << 20)
	g.GetColorFunction(1.0)
	if _, b, _, _ := GradientLUTCacheStats(); b != MaxGradientLUTSize*4 {
		t.Errorf("max clamp: bytes=%d want %d", b, MaxGradientLUTSize*4)
	}
}

// TestLUTOpacity confirms the opacity parameter is part of the snapshot.
func TestLUTOpacity(t *testing.T) {
	ClearGradientLUTCache()
	g := testLinearGradient(PadSpread)
	opaque := g.GetColorFunction(1.0).(ColorFunc)
	half := g.GetColorFunction(0.5).(ColorFunc)
	co, ch := nrgbaOf(opaque(60, 60)), nrgbaOf(half(60, 60))
	if ch.A >= co.A {
		t.Errorf("opacity 0.5 alpha %d not below 1.0 alpha %d", ch.A, co.A)
	}
	if n, _, _, _ := GradientLUTCacheStats(); n != 2 {
		t.Errorf("different opacities must not share a LUT, entries=%d", n)
	}
}
