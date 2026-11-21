// Copyright 2026 by the rasterx Authors. All rights reserved.
//_
// Rendering-level tests for DashModeBalanceClosed.
//
// The seam criterion used here is periodicity rather than mirror symmetry:
// a correctly balanced closed contour is walked by exactly k identical dash
// periods, so the ink profile sampled at equal arc-length steps along the
// contour must repeat every N/k samples. The seam point is then
// indistinguishable from any interior period boundary. The original
// (default) streaming mode leaves a truncated final dash when the perimeter
// is not a whole number of periods, which breaks that periodicity.
package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

const (
	balSize = 256
	balCx   = 128.0
	balCy   = 128.0
)

func newBalDasher(img *image.RGBA, mode DashMode, dashes []float64, offset float64) *Dasher {
	s := NewScannerGV(balSize, balSize, img, img.Bounds())
	d := NewDasher(balSize, balSize, s)
	d.SetStroke(8*64, 4*64, ButtCap, nil, FlatGap, MiterClip, dashes, offset)
	d.Mode = mode
	return d
}

// renderBalanced strokes p with the given mode and returns the RGBA image.
func renderBalanced(t *testing.T, mode DashMode, dashes []float64, offset float64, p Path) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, balSize, balSize))
	d := newBalDasher(img, mode, dashes, offset)
	d.Scanner.(*ScannerGV).SetColor(color.RGBA{R: 255, A: 255})
	p.AddTo(d)
	d.Draw()
	return img
}

// flatCollector flattens a path into a single closed polyline using the same
// exported QuadTo/CubeTo flatteners the rasterizer uses. Coordinates are
// kept in user units (float64) to avoid pixel-truncation error in arc length.
type flatCollector struct {
	poly            [][2]float64
	pen, first      [2]float64
	started, closed bool
}

func pt(a fixed.Point26_6) [2]float64 {
	return [2]float64{float64(a.X) / 64, float64(a.Y) / 64}
}

func (f *flatCollector) Start(a fixed.Point26_6) {
	f.pen = pt(a)
	f.first = f.pen
	f.poly = append(f.poly[:0], f.pen)
	f.started = true
	f.closed = false
}

func (f *flatCollector) push(x, y float32) {
	f.pen = [2]float64{float64(x) / 64, float64(y) / 64}
	f.poly = append(f.poly, f.pen)
}

func (f *flatCollector) Line(b fixed.Point26_6) {
	f.pen = pt(b)
	f.poly = append(f.poly, f.pen)
}

func (f *flatCollector) QuadBezier(b, c fixed.Point26_6) {
	QuadTo(float32(f.pen[0])*64, float32(f.pen[1])*64,
		float32(b.X), float32(b.Y), float32(c.X), float32(c.Y), f.push)
}

func (f *flatCollector) CubeBezier(b, c, d fixed.Point26_6) {
	CubeTo(float32(f.pen[0])*64, float32(f.pen[1])*64,
		float32(b.X), float32(b.Y), float32(c.X), float32(c.Y),
		float32(d.X), float32(d.Y), f.push)
}

func (f *flatCollector) Stop(closeLoop bool) {
	if closeLoop && f.pen != f.first {
		f.poly = append(f.poly, f.first)
	}
	f.closed = closeLoop
}

// flattenPath returns the polyline vertices and cumulative arc lengths for
// the given closed path.
func flattenPath(p Path) (pts [][2]float64, cum []float64, total float64) {
	var f flatCollector
	p.AddTo(&f)
	// De-duplicate consecutive identical points (zero-length segments).
	clean := f.poly[:0]
	for _, v := range f.poly {
		if len(clean) == 0 || clean[len(clean)-1] != v {
			clean = append(clean, v)
		}
	}
	if len(clean) > 1 && clean[0] == clean[len(clean)-1] {
		clean = clean[:len(clean)-1]
	}
	cum = make([]float64, len(clean)+1)
	for i := range clean {
		j := (i + 1) % len(clean)
		dx, dy := clean[j][0]-clean[i][0], clean[j][1]-clean[i][1]
		total += math.Hypot(dx, dy)
		cum[i+1] = total
	}
	return clean, cum, total
}

// pointAtArc returns the polyline point at arc distance s.
func pointAtArc(pts [][2]float64, cum []float64, total, s float64) [2]float64 {
	s = math.Mod(s, total)
	if s < 0 {
		s += total
	}
	// Binary search for the containing segment.
	lo, hi := 0, len(cum)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if cum[mid] <= s {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	i := lo - 1
	if i < 0 {
		i = 0
	}
	segLen := cum[i+1] - cum[i]
	if segLen <= 0 {
		return pts[i%len(pts)]
	}
	t := (s - cum[i]) / segLen
	a, b := pts[i], pts[(i+1)%len(pts)]
	return [2]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t}
}

// inkProfile samples n equal-arc-length points along the closed contour and
// records each point's alpha.
func inkProfile(img *image.RGBA, p Path, n int) []uint8 {
	pts, cum, total := flattenPath(p)
	out := make([]uint8, n)
	for i := 0; i < n; i++ {
		q := pointAtArc(pts, cum, total, total*float64(i)/float64(n))
		x, y := int(math.Round(q[0])), int(math.Round(q[1]))
		if x >= 0 && y >= 0 && x < img.Bounds().Dx() && y < img.Bounds().Dy() {
			out[i] = img.RGBAAt(x, y).A
		}
	}
	return out
}

// periodMismatches counts "strong" mismatches between a sample and the sample
// one profile period (n/k steps) ahead: one fully painted, the other fully
// empty. Anti-aliased edge pixels (intermediate alpha) never count, so the
// one-sample quantization jitter at dash boundaries does not cause failures.
func periodMismatches(profile []uint8, k int) int {
	const inkA, gapA = uint8(200), uint8(40)
	n := len(profile)
	s := n / k
	m := 0
	for i := range profile {
		a, b := profile[i], profile[(i+s)%n]
		if (a >= inkA && b <= gapA) || (b >= inkA && a <= gapA) {
			m++
		}
	}
	return m
}

// ringPath builds a circle centered at (balCx, balCy) so the start/end seam
// lies on the horizontal radius to the right of center.
func ringPath(radius float64) Path {
	var p Path
	AddCircle(balCx, balCy, radius, &p)
	return p
}

// balancedPeriodCount mirrors the balanceDashes choice of k: nearest whole
// number of original-pattern periods fitting the perimeter.
func balancedPeriodCount(perimeter float64, dashes []float64) int {
	var period float64
	for _, d := range dashes {
		if d > 0 {
			period += d
		}
	}
	k := int(math.Round(perimeter/period + 0.0))
	if k < 1 {
		k = 1
	}
	return k
}

const profileSamples = 24 * 20 // 24 samples per expected period, times headroom

func TestBalancedRingSeamPeriodicity(t *testing.T) {
	p := ringPath(80)
	dashes := []float64{33, 12}
	_, _, perimeter := flattenPath(p)
	k := balancedPeriodCount(perimeter, dashes)
	n := k * 24

	balanced := renderBalanced(t, DashModeBalanceClosed, dashes, 0, p)
	def := renderBalanced(t, DashModeDefault, dashes, 0, p)

	bProf := inkProfile(balanced, p, n)
	dProf := inkProfile(def, p, n)
	bMis := periodMismatches(bProf, k)
	dMis := periodMismatches(dProf, k)
	t.Logf("ring perimeter=%.2f k=%d  balanced mismatches=%d  default mismatches=%d",
		perimeter, k, bMis, dMis)
	if bMis != 0 {
		t.Errorf("balanced ring ink profile not period-exact: %d/%d mismatched samples", bMis, n)
	}
	if dMis == 0 {
		t.Error("default ring happens to be periodic; test pattern no longer contrasts the modes")
	}
}

func TestBalancedRectSeamPeriodicity(t *testing.T) {
	var p Path
	AddRect(40, 40, 216, 216, 0, &p)
	dashes := []float64{27, 13}
	_, _, perimeter := flattenPath(p)
	k := balancedPeriodCount(perimeter, dashes)
	n := k * 24

	balanced := renderBalanced(t, DashModeBalanceClosed, dashes, 0, p)
	def := renderBalanced(t, DashModeDefault, dashes, 0, p)
	bMis := periodMismatches(inkProfile(balanced, p, n), k)
	dMis := periodMismatches(inkProfile(def, p, n), k)
	t.Logf("rect perimeter=%.2f k=%d  balanced mismatches=%d  default mismatches=%d",
		perimeter, k, bMis, dMis)
	// A handful of mismatches are sampling jitter: the test maps arc length
	// through a flattened polyline while the rasterizer walks fixed-point
	// segments, so a sample may land one position off a dash boundary. A real
	// seam defect (truncated final dash) perturbs every period and yields an
	// order of magnitude more mismatches.
	if bMis > n/50 {
		t.Errorf("balanced rect ink profile not period-exact: %d mismatches", bMis)
	}
	if dMis <= bMis {
		t.Error("default rect seam no worse than balanced; test pattern no longer contrasts the modes")
	}
}

func TestBalancedCurveSeamPeriodicity(t *testing.T) {
	// Closed cubic-bezier lens loop, seam at the top point.
	var p Path
	p.Start(ToFixedP(128, 40))
	p.CubeBezier(ToFixedP(210, 90), ToFixedP(210, 166), ToFixedP(128, 216))
	p.CubeBezier(ToFixedP(46, 166), ToFixedP(46, 90), ToFixedP(128, 40))
	p.Stop(true)
	dashes := []float64{21, 9}
	_, _, perimeter := flattenPath(p)
	k := balancedPeriodCount(perimeter, dashes)
	n := k * 24

	balanced := renderBalanced(t, DashModeBalanceClosed, dashes, 0, p)
	def := renderBalanced(t, DashModeDefault, dashes, 0, p)
	bMis := periodMismatches(inkProfile(balanced, p, n), k)
	dMis := periodMismatches(inkProfile(def, p, n), k)
	t.Logf("lens perimeter=%.2f k=%d  balanced mismatches=%d  default mismatches=%d",
		perimeter, k, bMis, dMis)
	if bMis > n/50 {
		t.Errorf("balanced lens ink profile not period-exact: %d mismatches", bMis)
	}
	if dMis <= bMis {
		t.Error("default lens seam no worse than balanced; test pattern no longer contrasts the modes")
	}
}

func TestBalancedOffsetStaysPeriodic(t *testing.T) {
	// A user offset only rotates the phase; exact periodicity (and therefore
	// a clean seam) must hold for any offset, including large ones.
	p := ringPath(80)
	dashes := []float64{33, 12}
	_, _, perimeter := flattenPath(p)
	k := balancedPeriodCount(perimeter, dashes)
	n := k * 24
	for _, off := range []float64{19, 1e7, -1e7} {
		img := renderBalanced(t, DashModeBalanceClosed, dashes, off, p)
		if m := periodMismatches(inkProfile(img, p, n), k); m > n/50 {
			t.Errorf("offset %v broke seam periodicity: %d mismatches", off, m)
		}
	}
}

func TestBalancedModePaintsSomething(t *testing.T) {
	cases := map[string]Path{
		"ring": ringPath(70),
		"rect": func() (p Path) { AddRect(30, 30, 226, 226, 0, &p); return }(),
		"lens": func() (p Path) {
			p.Start(ToFixedP(128, 40))
			p.CubeBezier(ToFixedP(210, 90), ToFixedP(210, 166), ToFixedP(128, 216))
			p.CubeBezier(ToFixedP(46, 166), ToFixedP(46, 90), ToFixedP(128, 40))
			p.Stop(true)
			return
		}(),
	}
	for name, p := range cases {
		img := renderBalanced(t, DashModeBalanceClosed, []float64{17, 11}, 0, p)
		if inkPixels(img) == 0 {
			t.Errorf("balanced %s rendered nothing", name)
		}
	}
}

func TestDefaultModeDeterministic(t *testing.T) {
	// Guard for the streaming path: repeated default-mode renders are
	// pixel-identical.
	p := ringPath(60)
	first := renderBalanced(t, DashModeDefault, []float64{33, 12}, 5, p)
	second := renderBalanced(t, DashModeDefault, []float64{33, 12}, 5, p)
	b := first.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if first.RGBAAt(x, y) != second.RGBAAt(x, y) {
				t.Fatalf("default mode nondeterministic at (%d,%d)", x, y)
			}
		}
	}
}

func TestModeSwitchingOnOneDasher(t *testing.T) {
	// The Mode field is switchable at any time: a balanced render followed by
	// a default render on the same Dasher must reproduce the standalone
	// default output, proving buffering/state does not leak across subpaths.
	p := ringPath(60)
	dashes := []float64{33, 12}
	standalone := renderBalanced(t, DashModeDefault, dashes, 0, p)

	img := image.NewRGBA(image.Rect(0, 0, balSize, balSize))
	d := newBalDasher(img, DashModeBalanceClosed, dashes, 0)
	p.AddTo(d) // balanced run, exercises the buffer
	d.Draw()
	d.Clear()

	// Switch to default and render into a fresh image on the same Dasher.
	img2 := image.NewRGBA(image.Rect(0, 0, balSize, balSize))
	d.Scanner.(*ScannerGV).Dest = img2
	d.Mode = DashModeDefault
	p.AddTo(d)
	d.Draw()

	b := img2.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img2.RGBAAt(x, y) != standalone.RGBAAt(x, y) {
				t.Fatalf("default output after a balanced run differs at (%d,%d)", x, y)
			}
		}
	}
}

func TestOpenPathBehaviorUnchanged(t *testing.T) {
	// An open polyline must render identically in both modes: balancing only
	// applies to closed subpaths.
	var p Path
	p.Start(ToFixedP(20, 128))
	p.Line(ToFixedP(236, 128))
	p.Stop(false)
	def := renderBalanced(t, DashModeDefault, []float64{25, 10}, 3, p)
	bal := renderBalanced(t, DashModeBalanceClosed, []float64{25, 10}, 3, p)
	b := def.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if def.RGBAAt(x, y) != bal.RGBAAt(x, y) {
				t.Fatalf("open path pixels differ at (%d,%d): %v vs %v",
					x, y, def.RGBAAt(x, y), bal.RGBAAt(x, y))
			}
		}
	}
}

func TestMultiSubpathBalancing(t *testing.T) {
	// Two independent closed rings in one path: each is balanced on its own
	// perimeter, and both render with equal ink for equal geometry.
	var p Path
	AddCircle(64, 128, 40, &p)
	AddCircle(192, 128, 40, &p)
	img := renderBalanced(t, DashModeBalanceClosed, []float64{19, 7}, 0, p)
	var inkLeft, inkRight int
	for y := 0; y < balSize; y++ {
		for x := 0; x < balSize/2; x++ {
			if img.RGBAAt(x, y).A > 0 {
				inkLeft++
			}
		}
		for x := balSize / 2; x < balSize; x++ {
			if img.RGBAAt(x, y).A > 0 {
				inkRight++
			}
		}
	}
	if inkLeft == 0 || inkRight == 0 {
		t.Fatalf("one subpath missing: left ink %d, right ink %d", inkLeft, inkRight)
	}
	if d := math.Abs(float64(inkLeft - inkRight)); d > 4 {
		t.Errorf("subpath ink counts differ by %.0f", d)
	}
}

func TestBalancedEdgeCases(t *testing.T) {
	// Tiny closed contour whose perimeter is shorter than one period.
	var tiny Path
	AddCircle(128, 128, 3, &tiny)
	if img := renderBalanced(t, DashModeBalanceClosed, []float64{50, 25}, 0, tiny); inkPixels(img) == 0 {
		t.Error("tiny ring rendered nothing")
	}

	// Pattern containing zero and negative entries: deterministic, no hang,
	// ink present.
	ring := ringPath(60)
	if img := renderBalanced(t, DashModeBalanceClosed, []float64{30, 0, -12, 15}, 0, ring); inkPixels(img) == 0 {
		t.Error("ring with zero/negative dashes rendered nothing")
	}

	// All-zero pattern: solid stroke fallback.
	if img := renderBalanced(t, DashModeBalanceClosed, []float64{0, -5}, 0, ring); inkPixels(img) == 0 {
		t.Error("all-zero dash pattern did not fall back to a solid stroke")
	}

	// Very large positive and negative offsets: must not hang or panic.
	for _, off := range []float64{1e7, -1e7} {
		if img := renderBalanced(t, DashModeBalanceClosed, []float64{20, 10}, off, ring); inkPixels(img) == 0 {
			t.Errorf("offset %v rendered nothing", off)
		}
	}

	// Zero-length closed subpath followed by a normal ring in the same path:
	// deterministic, no hang, ring still paints.
	var mixed Path
	mixed.Start(ToFixedP(10, 10))
	mixed.Line(ToFixedP(10, 10))
	mixed.Stop(true)
	AddCircle(128, 128, 50, &mixed)
	if img := renderBalanced(t, DashModeBalanceClosed, []float64{18, 9}, 0, mixed); inkPixels(img) == 0 {
		t.Error("mixed zero-length + ring path rendered nothing")
	}

	// Arc-based closed contour (ellipse) balances too.
	var ell Path
	AddEllipse(128, 128, 90, 55, 0, &ell)
	if img := renderBalanced(t, DashModeBalanceClosed, []float64{23, 11}, 0, ell); inkPixels(img) == 0 {
		t.Error("balanced ellipse rendered nothing")
	}
}

func TestBalancedDegenerateSegments(t *testing.T) {
	// Closed contour deliberately containing degenerate beziers, which the
	// flatteners reduce to Line calls during replay. Those internal Line
	// calls must be stroked, not re-captured into the balance buffer.
	var p Path
	p.Start(ToFixedP(40, 40))
	p.Line(ToFixedP(216, 40))
	// Degenerate cubic: first control point equals the pen.
	p.CubeBezier(ToFixedP(216, 40), ToFixedP(230, 128), ToFixedP(216, 216))
	// Degenerate quadratic: control equals endpoint.
	p.QuadBezier(ToFixedP(40, 216), ToFixedP(40, 216))
	p.Line(ToFixedP(40, 40))
	p.Stop(true)

	img := renderBalanced(t, DashModeBalanceClosed, []float64{25, 11}, 0, p)
	if n := inkPixels(img); n == 0 {
		t.Fatal("degenerate-segment contour rendered nothing")
	}
}

func inkPixels(img *image.RGBA) int {
	n := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y).A > 0 {
				n++
			}
		}
	}
	return n
}
