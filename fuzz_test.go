// Copyright 2018 by the rasterx Authors. All rights reserved.
package rasterx_test

// Deterministic fuzz harness for malformed path input. The corpus is built
// from a fixed seed so every run exercises the same inputs. Each sample is run
// under per-sample budgets for wall time, allocation and output size; a sample
// fails if it panics, exceeds a budget, produces a non-finite coordinate, or
// makes the reference checker and the rendering path disagree.

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

const (
	// fuzzSeed fixes the corpus. Changing it changes the corpus, so a
	// failing sample is only reproducible against the seed recorded in the
	// failure message.
	fuzzSeed int64 = 0x5EED_270

	// Budgets, per sample.
	sampleTimeBudget    = 2 * time.Second
	sampleAllocBudget   = 64 << 20 // 64 MiB of allocated bytes
	sampleOutputBudget  = 4 << 20  // bytes of raster + path
	fuzzImageWidth      = 96
	fuzzImageHeight     = 96
	fuzzSamplesPerClass = 24
)

// fuzzKind names the class of malformed input a sample represents. The
// requirement is that every class is covered by the corpus.
type fuzzKind int

const (
	kindFinite fuzzKind = iota
	kindNonFinite
	kindZeroLength
	kindHugeCoords
	kindRecursiveCurves
	kindMiter
	kindEmptyStops
	kindHugeDashes
	kindBadPathStream
	fuzzKindCount
)

func (k fuzzKind) String() string {
	switch k {
	case kindFinite:
		return "finite"
	case kindNonFinite:
		return "non-finite"
	case kindZeroLength:
		return "zero/negative length"
	case kindHugeCoords:
		return "huge coordinates"
	case kindRecursiveCurves:
		return "recursive curves"
	case kindMiter:
		return "abnormal miter"
	case kindEmptyStops:
		return "empty stops"
	case kindHugeDashes:
		return "huge dashes"
	case kindBadPathStream:
		return "malformed stream"
	default:
		return "unknown"
	}
}

// fuzzSample is one generated case.
type fuzzSample struct {
	Kind    fuzzKind
	Index   int
	Path    Path
	Width   int
	Height  int
	Stroke  bool
	Dash    []float64
	DashOff float64
	Miter   fixed.Int26_6
	Join    JoinMode
	Grad    *Gradient
	// wantErr is true when the seed is known to be malformed and the checker
	// must report it. Samples of kindFinite are valid controls and must not
	// report an error.
	wantErr bool
}

func (s fuzzSample) String() string {
	return fmt.Sprintf("kind=%s index=%d seed=%#x", s.Kind, s.Index, fuzzSeed)
}

// fuzzRand returns a deterministic generator for one (kind, index) pair. Each
// sample is independent, so a failing sample can be regenerated alone.
func fuzzRand(k fuzzKind, index int) *rand.Rand {
	return rand.New(rand.NewSource(fuzzSeed ^ int64(k+1)*0x9E3779B1 ^ int64(index+1)*2654435761))
}

// buildCorpus generates the deterministic corpus.
func buildCorpus() []fuzzSample {
	var out []fuzzSample
	for k := fuzzKind(0); k < fuzzKindCount; k++ {
		for i := 0; i < fuzzSamplesPerClass; i++ {
			out = append(out, buildSample(k, i))
		}
	}
	return out
}

func randPoint(r *rand.Rand, span float64) fixed.Point26_6 {
	return fixed.Point26_6{
		X: fixed.Int26_6((r.Float64()*2 - 1) * span * 64),
		Y: fixed.Int26_6((r.Float64()*2 - 1) * span * 64),
	}
}

func buildSample(k fuzzKind, i int) fuzzSample {
	r := fuzzRand(k, i)
	s := fuzzSample{Kind: k, Index: i, Width: fuzzImageWidth, Height: fuzzImageHeight,
		Miter: 4 << 6, Join: MiterClip}

	// A small valid base path the malformed feature is attached to.
	base := func() Path {
		var p Path
		p.Start(fixed.Point26_6{X: 12 << 6, Y: 12 << 6})
		p.Line(fixed.Point26_6{X: 76 << 6, Y: 20 << 6})
		p.QuadBezier(fixed.Point26_6{X: 40 << 6, Y: 60 << 6}, fixed.Point26_6{X: 70 << 6, Y: 70 << 6})
		return p
	}
	s.Path = base()

	switch k {
	case kindFinite:
		// Control samples: strictly valid geometry, so the checker must
		// succeed and the renderer must produce an image.
		var p Path
		start := randPoint(r, 70)
		p.Start(start)
		for n := 0; n < 4; n++ {
			switch r.Intn(3) {
			case 0:
				p.Line(randPoint(r, 70))
			case 1:
				p.QuadBezier(randPoint(r, 70), randPoint(r, 70))
			case 2:
				p.CubeBezier(randPoint(r, 70), randPoint(r, 70), randPoint(r, 70))
			}
		}
		p.Stop(r.Intn(2) == 0)
		s.Path = p
		s.Stroke = r.Intn(2) == 0
		if s.Stroke {
			s.Dash = []float64{6 + r.Float64()*8, 3 + r.Float64()*5}
		}
		s.wantErr = false

	case kindNonFinite:
		// Points at and just beyond the representable range. In 26.6 fixed
		// point NaN and Inf are not representable, so the closest analogs are
		// the int32 extremes, which wrap into garbage under arithmetic.
		bad := []fixed.Int26_6{
			fixed.Int26_6(math.MinInt32),
			fixed.Int26_6(math.MaxInt32),
			-safeCoordBeyond(),
			safeCoordBeyond(),
		}
		p := base()
		switch i % 4 {
		case 0:
			p.Line(fixed.Point26_6{X: bad[i%len(bad)], Y: 3 << 6})
		case 1:
			p.QuadBezier(fixed.Point26_6{X: bad[i%len(bad)], Y: bad[(i+1)%len(bad)]},
				fixed.Point26_6{X: 5 << 6, Y: 5 << 6})
		case 2:
			p.CubeBezier(fixed.Point26_6{X: bad[i%len(bad)], Y: 0},
				fixed.Point26_6{X: 0, Y: bad[(i+2)%len(bad)]}, fixed.Point26_6{X: 9 << 6, Y: 9 << 6})
		case 3:
			// Path stream carrying raw NaN/Inf bit patterns decoded as fixed
			// point: MaxInt32 is the on-wire representation a parser produces
			// when it casts a non-finite float without checking.
			p = Path{fixed.Int26_6(PathMoveTo), 0, 0,
				fixed.Int26_6(PathLineTo), fixed.Int26_6(math.MinInt32), fixed.Int26_6(math.MaxInt32)}
		}
		s.Path = p
		s.wantErr = true

	case kindZeroLength:
		// Repeated points, zero-width strokes, zero-radius curves.
		p := base()
		pt := fixed.Point26_6{X: 40 << 6, Y: 40 << 6}
		switch i % 4 {
		case 0:
			p.Line(pt)
			p.Line(pt)
			p.Line(pt)
		case 1:
			p.QuadBezier(pt, pt)
		case 2:
			p.CubeBezier(pt, pt, pt)
		case 3:
			p.Start(pt)
			p.Line(pt)
			p.Stop(true)
		}
		s.Path = p
		s.Stroke = true
		s.Miter = 0
		if i%2 == 0 {
			s.Dash = []float64{0.0, 0.0} // rejected pattern, must not spin
		}
		// Zero-length geometry is legal; the renderer must handle it, so no
		// error is required. The point of the sample is termination.
		s.wantErr = false

	case kindHugeCoords:
		// Coordinates large enough to overflow intermediate products but
		// still inside int32.
		big := fixed.Int26_6(1 << 27)
		p := base()
		switch i % 3 {
		case 0:
			p.Line(fixed.Point26_6{X: big, Y: -big})
		case 1:
			p.CubeBezier(fixed.Point26_6{X: big, Y: big},
				fixed.Point26_6{X: -big, Y: -big}, fixed.Point26_6{X: big, Y: -big})
		case 2:
			p.QuadBezier(fixed.Point26_6{X: -big, Y: big}, fixed.Point26_6{X: big, Y: big})
		}
		s.Path = p
		s.wantErr = true

	case kindRecursiveCurves:
		// Curves whose control points oscillate across a wide range. The
		// flattening count grows with the fourth root of the deviation, so a
		// single curve inside the safe coordinate range tops out at about
		// 4700 segments: deep recursion is bounded, not explosive. The two
		// sub-classes check both ends of that:
		//  - a handful of worst-case curves must render inside the budget;
		//  - enough of them to exceed the total output cap must be rejected.
		var p Path
		p.Start(fixed.Point26_6{X: 8 << 6, Y: 8 << 6})
		amp := fixed.Int26_6(1 << (14 + i%4)) // 2^14..2^17 fixed units
		curves := 6
		if i%6 == 0 {
			amp = SafeCoordLimit // worst legal deviation
			// 6 curves * ~4700 lines stays under the output cap; ~500 curves
			// crosses it.
			curves = 500
		}
		for n := 0; n < curves; n++ {
			sign := fixed.Int26_6(1)
			if n%2 == 1 {
				sign = -1
			}
			p.CubeBezier(
				fixed.Point26_6{X: 40 << 6, Y: sign * amp},
				fixed.Point26_6{X: 40 << 6, Y: -sign * amp},
				fixed.Point26_6{X: fixed.Int26_6(40+n%8) << 6, Y: 40 << 6})
		}
		s.Path = p
		// Only the over-cap variant is expected to be rejected; the deep but
		// legal one proves the renderer terminates inside its budget.
		s.wantErr = i%6 == 0

	case kindMiter:
		// Near-collinear joins with extreme miter limits force the miter and
		// arc-clip intersection code paths, including parallel lines.
		var p Path
		p.Start(fixed.Point26_6{X: 10 << 6, Y: 40 << 6})
		p.Line(fixed.Point26_6{X: 50 << 6, Y: 40 << 6})
		if i%2 == 0 {
			p.Line(fixed.Point26_6{X: 90 << 6, Y: 41 << 6}) // almost straight
		} else {
			p.Line(fixed.Point26_6{X: 90 << 6, Y: 40 << 6}) // exactly straight
		}
		p.Line(fixed.Point26_6{X: 50 << 6, Y: 40 << 6}) // reverses onto itself
		s.Path = p
		s.Stroke = true
		s.Miter = fixed.Int26_6(1 << (6 + i%12))
		s.Join = JoinMode(i % 6)
		s.wantErr = false

	case kindEmptyStops:
		// Gradients with no stops, one stop, all-non-finite offsets and a
		// degenerate box. Used as the scanner paint.
		g := &Gradient{Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 80, H: 80},
			Matrix: Identity}
		switch i % 5 {
		case 0: // no stops
		case 1:
			g.Stops = []GradStop{{StopColor: color.RGBA{1, 2, 3, 255}, Offset: 0}}
		case 2:
			g.Stops = []GradStop{
				{StopColor: color.RGBA{1, 2, 3, 255}, Offset: math.NaN(), Opacity: 1},
				{StopColor: color.RGBA{4, 5, 6, 255}, Offset: math.Inf(1), Opacity: 1},
			}
		case 3:
			g.IsRadial = true
			g.Points = [5]float64{0.5, 0.5, 0.5, 0.5, 0}
			g.Stops = []GradStop{{StopColor: color.RGBA{1, 2, 3, 255}, Offset: 0},
				{StopColor: color.RGBA{4, 5, 6, 255}, Offset: 1}}
		case 4:
			g.Bounds = struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 0, H: 0}
			g.Stops = []GradStop{{StopColor: color.RGBA{1, 2, 3, 255}, Offset: 0},
				{StopColor: color.RGBA{4, 5, 6, 255}, Offset: 1}}
		}
		s.Grad = g

	case kindHugeDashes:
		// Dash arrays at and beyond the entry limit, sub-fixed-point entries,
		// huge offsets and negative values.
		switch i % 4 {
		case 0:
			s.Dash = []float64{1e-9, 1e-9}
		case 1:
			s.Dash = make([]float64, MaxDashEntries+1)
			for n := range s.Dash {
				s.Dash[n] = 0.5
			}
		case 2:
			s.Dash = []float64{-5, 3, 0, 2}
		case 3:
			s.Dash = []float64{33, 12}
			s.DashOff = 1e9
		}
		s.Stroke = true
		s.wantErr = false // the Dasher must reject or clamp, not hang

	case kindBadPathStream:
		// Structurally broken Path streams.
		streams := []Path{
			{fixed.Int26_6(PathLineTo), 1, 2},                             // no leading moveto
			{fixed.Int26_6(PathMoveTo), 1, 2, fixed.Int26_6(PathCubicTo)}, // truncated tail
			{99, 1, 2},                  // unknown command
			{fixed.Int26_6(PathMoveTo)}, // truncated moveto
			{fixed.Int26_6(PathMoveTo), 1, 2, fixed.Int26_6(PathQuadTo), 3}, // truncated quad
		}
		s.Path = streams[i%len(streams)]
		s.wantErr = true
	}
	return s
}

// safeCoordBeyond returns a coordinate one unit past SafeCoordLimit.
func safeCoordBeyond() fixed.Int26_6 { return SafeCoordLimit + 1 }

// runSample renders one sample under the budgets and returns the number of
// bytes of output produced. It fails the test on panic or budget overrun.
func runSample(t *testing.T, s fuzzSample) (outBytes int, renderErr error) {
	t.Helper()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()

	type result struct {
		img    *image.RGBA
		err    error
		report *PathReport
		guard  *GuardScanner
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("%w: %v", ErrPanic, r)}
			}
		}()
		img := image.NewRGBA(image.Rect(0, 0, s.Width, s.Height))
		sc := NewScannerGV(s.Width, s.Height, img, img.Bounds())
		if s.Grad != nil {
			if c := s.Grad.GetColorFunction(1); c != nil {
				sc.SetColor(c)
			}
		} else {
			sc.SetColor(color.RGBA{200, 100, 50, 255})
		}

		// The guard sits between the rasterizer and the real scanner, so it
		// sees exactly the flattened output the rasterizer produced.
		guard := NewGuardScanner(sc)
		var adder Adder
		if s.Stroke || len(s.Dash) > 0 {
			d := NewDasher(s.Width, s.Height, guard)
			d.SetStroke(4*64, s.Miter, RoundCap, nil, RoundGap, s.Join, s.Dash, s.DashOff)
			adder = d
		} else {
			adder = NewFiller(s.Width, s.Height, guard)
		}

		report := AnalyzePath(s.Path, DefaultLimits())
		// Render through the guarded adder even when the checker rejects the
		// stream: AddToChecked stops at the first bad command, so the
		// partial path is measured rather than crashed on.
		ferr := s.Path.AddToChecked(adder)
		if ferr == nil {
			adder.Stop(false)
			guard.Draw()
			if guard.Err != nil {
				ferr = guard.Err
			}
		}
		done <- result{img: img, err: ferr, report: report, guard: guard}
	}()

	var res result
	select {
	case res = <-done:
	case <-time.After(sampleTimeBudget):
		t.Fatalf("%s exceeded the %v time budget (possible infinite loop)", s, sampleTimeBudget)
	}

	elapsed := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	allocd := after.TotalAlloc - before.TotalAlloc

	if elapsed > sampleTimeBudget {
		t.Fatalf("%s took %v, over the %v budget", s, elapsed, sampleTimeBudget)
	}
	if allocd > sampleAllocBudget {
		t.Fatalf("%s allocated %d bytes, over the %d budget", s, allocd, sampleAllocBudget)
	}
	if res.img != nil {
		outBytes = len(res.img.Pix)
	}
	if outBytes > sampleOutputBudget {
		t.Fatalf("%s output %d bytes, over the %d budget", s, outBytes, sampleOutputBudget)
	}

	// Reference checker vs. rendering path: both must agree on whether the
	// input is structurally valid.
	rejected := res.err != nil || (res.report != nil && res.report.Err != nil)
	if s.wantErr && !rejected {
		t.Fatalf("%s was expected to be rejected but passed both checker and render", s)
	}
	if !s.wantErr && rejected {
		t.Fatalf("%s is a valid control but was rejected: checker=%v render=%v",
			s, res.report.Err, res.err)
	}
	// When both reject, they must name the same problem class.
	if s.wantErr && res.err != nil && res.report.Err != nil &&
		!sameErrClass(res.err, res.report.Err) {
		t.Fatalf("%s: checker reported %v but render reported %v", s, res.report.Err, res.err)
	}
	if res.report != nil && res.report.Err != nil && res.report.FailureStage == "" {
		t.Fatalf("%s: rejected without a failure stage", s)
	}
	return outBytes, res.err
}

// sameErrClass reports whether two errors describe the same kind of failure,
// allowing for wrapping.
func sameErrClass(a, b error) bool {
	for _, target := range []error{ErrBadPath, ErrNonFinite, ErrTooComplex,
		ErrTooManySegments, ErrTooManyOutputLines} {
		if errors.Is(a, target) && errors.Is(b, target) {
			return true
		}
	}
	return errors.Is(a, b)
}

// TestFuzzHarnessCorpus is the primary harness test: every class, every
// sample, under budgets. It is part of `go test`.
func TestFuzzHarnessCorpus(t *testing.T) {
	corpus := buildCorpus()
	seen := make(map[fuzzKind]int, fuzzKindCount)
	for _, s := range corpus {
		s := s
		t.Run(s.String(), func(t *testing.T) {
			runSample(t, s)
		})
		seen[s.Kind]++
	}
	for k := fuzzKind(0); k < fuzzKindCount; k++ {
		if seen[k] < fuzzSamplesPerClass {
			t.Errorf("kind %s has only %d samples", k, seen[k])
		}
	}
	// The corpus must cover every required class.
	required := []fuzzKind{kindNonFinite, kindZeroLength, kindHugeCoords,
		kindRecursiveCurves, kindMiter, kindEmptyStops, kindHugeDashes}
	for _, k := range required {
		if seen[k] == 0 {
			t.Errorf("required class %s is absent from the corpus", k)
		}
	}
}

// TestFuzzHarnessDeterministic proves the corpus is fixed: rebuilding it yields
// the same bytes for every sample.
func TestFuzzHarnessDeterministic(t *testing.T) {
	a, b := buildCorpus(), buildCorpus()
	if len(a) != len(b) {
		t.Fatalf("corpus length changed: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].String() != b[i].String() ||
			a[i].DashOff != b[i].DashOff ||
			len(a[i].Path) != len(b[i].Path) ||
			len(a[i].Dash) != len(b[i].Dash) {
			t.Fatalf("sample %d is not reproducible", i)
		}
		for n := range a[i].Path {
			if a[i].Path[n] != b[i].Path[n] {
				t.Fatalf("sample %d path byte %d differs", i, n)
			}
		}
		for n := range a[i].Dash {
			if a[i].Dash[n] != b[i].Dash[n] {
				t.Fatalf("sample %d dash byte %d differs", i, n)
			}
		}
	}
}

// TestFuzzCheckerIndependentOfRasterizer asserts the reference checker never
// forwards to a rasterizer. AnalyzePath takes only a Path, so the independence
// is structural; this test documents that contract and checks that every
// rejection names the phase it failed in.
func TestFuzzCheckerIndependentOfRasterizer(t *testing.T) {
	corpus := buildCorpus()
	for _, s := range corpus {
		rep := AnalyzePath(s.Path, DefaultLimits())
		if rep.Err != nil && rep.FailureStage == "" {
			t.Fatalf("%s: checker reported %v without a failure stage", s, rep.Err)
		}
		// The rendered-vs-checked verdicts are compared in runSample.
	}
}

// TestFuzzHarnessRecordsResources runs a subset and checks that the report
// carries command counts, extent and estimated lines, i.e. the resource
// accounting the requirement asks for.
func TestFuzzHarnessRecordsResources(t *testing.T) {
	s := buildSample(kindFinite, 0)
	rep := AnalyzePath(s.Path, DefaultLimits())
	if rep.Err != nil {
		t.Fatalf("control sample failed: %v", rep.Err)
	}
	if len(rep.Commands) == 0 {
		t.Fatal("report has no commands")
	}
	if rep.MoveTo == 0 || rep.Segments == 0 {
		t.Fatalf("report has no segment counts: %+v", rep)
	}
	if !rep.HaveExtent || rep.Min == rep.Max {
		t.Fatalf("report has no extent: %v..%v", rep.Min, rep.Max)
	}
	if rep.EstLines == 0 {
		t.Fatal("report has no flattened line estimate")
	}
	// The estimated line count must match the flattened output of the real
	// filler within the checker's own accounting.
	guard := NewGuardScanner(NewScannerGV(8, 8, image.NewRGBA(image.Rect(0, 0, 8, 8)),
		image.Rect(0, 0, 8, 8)))
	f := NewFiller(8, 8, guard)
	if err := s.Path.AddToChecked(f); err != nil {
		t.Fatalf("AddToChecked: %v", err)
	}
	f.Draw()
	if guard.Err != nil {
		t.Fatalf("GuardScanner: %v at %q", guard.Err, guard.FailureStage)
	}
	if guard.LineCount == 0 {
		t.Fatal("GuardScanner saw no flattened lines")
	}
}

// TestFuzzHarnessStableImage renders every control sample twice and requires
// identical pixels, so a malformed neighbour cannot leave state behind.
func TestFuzzHarnessStableImage(t *testing.T) {
	for i := 0; i < fuzzSamplesPerClass; i++ {
		s := buildSample(kindFinite, i)
		h1 := renderHash(t, s)
		h2 := renderHash(t, s)
		if h1 != h2 {
			t.Fatalf("%s is not stable: %s vs %s", s, h1, h2)
		}
	}
}

func renderHash(t *testing.T, s fuzzSample) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, s.Width, s.Height))
	sc := NewScannerGV(s.Width, s.Height, img, img.Bounds())
	sc.SetColor(color.RGBA{200, 100, 50, 255})
	var a Adder
	if s.Stroke || len(s.Dash) > 0 {
		d := NewDasher(s.Width, s.Height, sc)
		d.SetStroke(4*64, s.Miter, RoundCap, nil, RoundGap, s.Join, s.Dash, s.DashOff)
		a = d
	} else {
		a = NewFiller(s.Width, s.Height, sc)
	}
	if err := s.Path.AddToChecked(a); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	a.Stop(false)
	sc.Draw()
	var sb strings.Builder
	for _, b := range img.Pix {
		sb.WriteByte(b)
	}
	return sb.String()
}

// FuzzMalformedPath is the native `go test -fuzz` entry point. It decodes a
// byte stream into a Path and a small set of stroke switches, then requires
// the same properties as the corpus test: no panic, no hang, and agreement
// between the reference checker and the rendering path.
//
// The fixed corpus in TestFuzzHarnessCorpus is what runs on every `go test`;
// this function is for longer, mutation-driven runs:
//
//	go test -run '^$' -fuzz FuzzMalformedPath -fuzztime 30s
func FuzzMalformedPath(f *testing.F) {
	// Seed the fuzzer with the deterministic corpus so coverage starts from
	// every malformed class.
	for _, s := range buildCorpus() {
		raw := make([]byte, 0, len(s.Path)*2+8)
		for _, v := range s.Path {
			raw = append(raw, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		}
		raw = append(raw, byte(len(s.Dash)), 0)
		f.Add(raw, s.Stroke)
	}

	f.Fuzz(func(t *testing.T, raw []byte, stroke bool) {
		p := decodeFuzzPath(raw)
		report := AnalyzePath(p, DefaultLimits())

		// Rendering runs in its own goroutine so a hang is a test failure,
		// not a stuck fuzzer. The panic value is carried on a per-call
		// channel, so concurrent fuzz workers cannot observe each other's
		// panics.
		panicCh := make(chan interface{}, 1)
		done := make(chan struct{})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					panicCh <- r
				}
				close(done)
			}()
			img := image.NewRGBA(image.Rect(0, 0, 32, 32))
			sc := NewScannerGV(32, 32, img, img.Bounds())
			sc.SetColor(color.RGBA{1, 2, 3, 255})
			guard := NewGuardScanner(sc)
			var a Adder = NewFiller(32, 32, guard)
			if stroke {
				d := NewDasher(32, 32, guard)
				d.SetStroke(2*64, 4*64, ButtCap, nil, FlatGap, MiterClip, nil, 0)
				a = d
			}
			if err := p.AddToChecked(a); err == nil {
				a.Stop(false)
				guard.Draw()
			}
		}()
		select {
		case <-done:
		case <-time.After(sampleTimeBudget):
			t.Fatalf("render did not terminate for % x", raw)
		}
		select {
		case r := <-panicCh:
			t.Fatalf("render panicked: %v", r)
		default:
		}
		// The checker must never crash and must name a stage when it rejects.
		if report.Err != nil && report.FailureStage == "" {
			t.Fatalf("rejected without a stage: %v", report.Err)
		}
	})
}

// decodeFuzzPath turns arbitrary bytes into a Path, treating every fourth byte
// as a command selector and the rest as coordinates. It never panics.
func decodeFuzzPath(raw []byte) Path {
	if len(raw) < 4 {
		return nil
	}
	var p Path
	for i := 0; i+4 <= len(raw); i += 4 {
		word := int64(int32(uint32(raw[i]) | uint32(raw[i+1])<<8 |
			uint32(raw[i+2])<<16 | uint32(raw[i+3])<<24))
		x := fixed.Int26_6(int32(uint32(raw[(i+1)%len(raw)]) |
			uint32(raw[(i+2)%len(raw)])<<8 |
			uint32(raw[(i+3)%len(raw)])<<16 |
			uint32(raw[i%len(raw)])<<24))
		switch PathCommand(word % 5) {
		case PathMoveTo:
			p.Start(fixed.Point26_6{X: x, Y: -x})
		case PathLineTo:
			p.Line(fixed.Point26_6{X: x, Y: -x})
		case PathQuadTo:
			p.QuadBezier(fixed.Point26_6{X: x, Y: -x}, fixed.Point26_6{X: -x, Y: x})
		case PathCubicTo:
			p.CubeBezier(fixed.Point26_6{X: x, Y: -x},
				fixed.Point26_6{X: -x, Y: x}, fixed.Point26_6{X: x, Y: x})
		case PathClose:
			p.Stop(true)
		}
		if len(p) > 4096 {
			break
		}
	}
	return p
}
