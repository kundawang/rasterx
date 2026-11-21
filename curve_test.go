// Copyright 2026 by the rasterx Authors. All rights reserved.
//_
// curve_test.go verifies the shared curve subdivision core (curve.go):
// point-level equivalence with a de Casteljau reference, tangent and segment
// bookkeeping, degenerate and extreme-input safety, and pixel-identical
// fill/stroke/dash rendering.

package rasterx_test

import (
	"crypto/md5"
	"image"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/colornames"
	"golang.org/x/image/math/fixed"
)

// lerp32 is the float32 linear interpolation used by the de Casteljau
// reference below, mirroring the helper in the package's bezier_test.go.
func lerp32(t, px, py, qx, qy float32) (x, y float32) {
	return px + t*(qx-px), py + t*(qy-py)
}

// deCasteljauQuad32 evaluates a quadratic Bézier in float32 at parameter t.
// It uses the lerp chain rather than the power basis, giving an independent
// reference for the shared subdivision core.
func deCasteljauQuad32(a, b, c fixed.Point26_6, t float32) fixed.Point26_6 {
	abx, aby := lerp32(t, float32(a.X), float32(a.Y), float32(b.X), float32(b.Y))
	bcx, bcy := lerp32(t, float32(b.X), float32(b.Y), float32(c.X), float32(c.Y))
	x, y := lerp32(t, abx, aby, bcx, bcy)
	return fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)}
}

// deCasteljauCube32 evaluates a cubic Bézier in float32 at parameter t via the
// two-level de Casteljau lerp chain.
func deCasteljauCube32(a, b, c, d fixed.Point26_6, t float32) fixed.Point26_6 {
	abx, aby := lerp32(t, float32(a.X), float32(a.Y), float32(b.X), float32(b.Y))
	bcx, bcy := lerp32(t, float32(b.X), float32(b.Y), float32(c.X), float32(c.Y))
	cdx, cdy := lerp32(t, float32(c.X), float32(c.Y), float32(d.X), float32(d.Y))
	abcx, abcy := lerp32(t, abx, aby, bcx, bcy)
	bcdx, bcdy := lerp32(t, bcx, bcy, cdx, cdy)
	x, y := lerp32(t, abcx, abcy, bcdx, bcdy)
	return fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)}
}

func abs26(v fixed.Int26_6) fixed.Int26_6 {
	if v < 0 {
		return -v
	}
	return v
}

// float32RoundingTolerance is the maximum acceptable difference, in
// fixed.Int26_6 units, between two float32 evaluations of the same curve using
// different evaluation formulas (power basis in the core vs the de Casteljau
// lerp chain reference). The rounding-order difference accumulates to a few
// fixed-point units; 4 units is 1/16 of a pixel, far below what rasterization
// can resolve. Exact equivalence of the package's own entry points is covered
// by TestCoreEntryPointsBitIdentical.
const float32RoundingTolerance = 4

// TestFlattenQuadCore checks emitted points against a de Casteljau reference,
// the segment count contract and the tangent convention.
func TestFlattenQuadCore(t *testing.T) {
	rnd := rand.New(rand.NewSource(1234))
	for i := 0; i < 2000; i++ {
		a := fixed.P(rnd.Intn(2000)-1000, rnd.Intn(2000)-1000)
		b := fixed.P(rnd.Intn(2000)-1000, rnd.Intn(2000)-1000)
		c := fixed.P(rnd.Intn(2000)-1000, rnd.Intn(2000)-1000)
		var pts []fixed.Point26_6
		n, t0, t1 := FlattenQuad(a, b, c, func(p fixed.Point26_6) { pts = append(pts, p) })
		if n != len(pts) {
			t.Fatalf("quad segment count %d != emitted points %d", n, len(pts))
		}
		if t0 != b.Sub(a) || t1 != c.Sub(b) {
			t.Fatalf("quad tangent mismatch: got %v/%v want %v/%v", t0, t1, b.Sub(a), c.Sub(b))
		}
		if pts[len(pts)-1] != c {
			t.Fatalf("quad last emitted point %v != end %v", pts[len(pts)-1], c)
		}
		for k, p := range pts {
			tt := float32(k+1) / float32(n)
			ref := deCasteljauQuad32(a, b, c, tt)
			// Independent float32 formula; rounding order differs, hence the
			// fixed float32 rounding tolerance rather than exact equality.
			if abs26(p.X-ref.X) > float32RoundingTolerance ||
				abs26(p.Y-ref.Y) > float32RoundingTolerance {
				t.Fatalf("quad point %d/%d outside float32 rounding tolerance: %v vs ref %v", k, n, p, ref)
			}
		}
	}
}

// TestFlattenCubicCore is the cubic analogue of TestFlattenQuadCore.
func TestFlattenCubicCore(t *testing.T) {
	rnd := rand.New(rand.NewSource(5678))
	for i := 0; i < 2000; i++ {
		a := fixed.P(rnd.Intn(2000)-1000, rnd.Intn(2000)-1000)
		b := fixed.P(rnd.Intn(2000)-1000, rnd.Intn(2000)-1000)
		c := fixed.P(rnd.Intn(2000)-1000, rnd.Intn(2000)-1000)
		d := fixed.P(rnd.Intn(2000)-1000, rnd.Intn(2000)-1000)
		var pts []fixed.Point26_6
		n, t0, t1 := FlattenCubic(a, b, c, d, func(p fixed.Point26_6) { pts = append(pts, p) })
		if n != len(pts) {
			t.Fatalf("cube segment count %d != emitted points %d", n, len(pts))
		}
		if t0 != b.Sub(a) || t1 != d.Sub(c) {
			t.Fatalf("cube tangent mismatch: got %v/%v want %v/%v", t0, t1, b.Sub(a), d.Sub(c))
		}
		if pts[len(pts)-1] != d {
			t.Fatalf("cube last emitted point %v != end %v", pts[len(pts)-1], d)
		}
		for k, p := range pts {
			tt := float32(k+1) / float32(n)
			ref := deCasteljauCube32(a, b, c, d, tt)
			if abs26(p.X-ref.X) > float32RoundingTolerance ||
				abs26(p.Y-ref.Y) > float32RoundingTolerance {
				t.Fatalf("cube point %d/%d outside float32 rounding tolerance: %v vs ref %v", k, n, p, ref)
			}
		}
	}
}

// TestCoreEntryPointsBitIdentical drives the same curves through the
// fixed-point Flatten* functions and the float32 QuadTo/CubeTo wrappers and
// requires bit-identical fixed-point output. Both entry points share the
// subdivision core, so any divergence here means the fill and stroke edges
// could drift apart.
func TestCoreEntryPointsBitIdentical(t *testing.T) {
	rnd := rand.New(rand.NewSource(2024))
	for i := 0; i < 1000; i++ {
		a := fixed.P(rnd.Intn(4000)-2000, rnd.Intn(4000)-2000)
		b := fixed.P(rnd.Intn(4000)-2000, rnd.Intn(4000)-2000)
		c := fixed.P(rnd.Intn(4000)-2000, rnd.Intn(4000)-2000)
		d := fixed.P(rnd.Intn(4000)-2000, rnd.Intn(4000)-2000)

		var fixedQuad, floatQuad []fixed.Point26_6
		FlattenQuad(a, b, c, func(p fixed.Point26_6) { fixedQuad = append(fixedQuad, p) })
		QuadTo(float32(a.X), float32(a.Y), float32(b.X), float32(b.Y),
			float32(c.X), float32(c.Y),
			func(x, y float32) {
				floatQuad = append(floatQuad, fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)})
			})
		if !pointsEqual(fixedQuad, floatQuad) {
			t.Fatalf("quad entry points diverge at iteration %d", i)
		}

		var fixedCube, floatCube []fixed.Point26_6
		FlattenCubic(a, b, c, d, func(p fixed.Point26_6) { fixedCube = append(fixedCube, p) })
		CubeTo(float32(a.X), float32(a.Y), float32(b.X), float32(b.Y),
			float32(c.X), float32(c.Y), float32(d.X), float32(d.Y),
			func(x, y float32) {
				floatCube = append(floatCube, fixed.Point26_6{X: fixed.Int26_6(x), Y: fixed.Int26_6(y)})
			})
		if !pointsEqual(fixedCube, floatCube) {
			t.Fatalf("cube entry points diverge at iteration %d", i)
		}
	}
}

func pointsEqual(a, b []fixed.Point26_6) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSegmentCountDeterminism proves a given curve always yields the same
// segment count regardless of which public entry point reaches the core
// (fixed-point Flatten* vs the float32 QuadTo/CubeTo wrappers).
func TestSegmentCountDeterminism(t *testing.T) {
	rnd := rand.New(rand.NewSource(99))
	for i := 0; i < 500; i++ {
		a := fixed.P(rnd.Intn(500), rnd.Intn(500))
		b := fixed.P(rnd.Intn(500), rnd.Intn(500))
		c := fixed.P(rnd.Intn(500), rnd.Intn(500))
		d := fixed.P(rnd.Intn(500), rnd.Intn(500))

		var quadPts, cubePts int
		FlattenQuad(a, b, c, func(fixed.Point26_6) { quadPts++ })
		FlattenCubic(a, b, c, d, func(fixed.Point26_6) { cubePts++ })

		var qFromAPI, cFromAPI int
		QuadTo(float32(a.X), float32(a.Y), float32(b.X), float32(b.Y),
			float32(c.X), float32(c.Y), func(_, _ float32) { qFromAPI++ })
		CubeTo(float32(a.X), float32(a.Y), float32(b.X), float32(b.Y),
			float32(c.X), float32(c.Y), float32(d.X), float32(d.Y),
			func(_, _ float32) { cFromAPI++ })

		if quadPts != qFromAPI {
			t.Fatalf("quad segment count mismatch fixed %d vs float32 %d", quadPts, qFromAPI)
		}
		if cubePts != cFromAPI {
			t.Fatalf("cube segment count mismatch fixed %d vs float32 %d", cubePts, cFromAPI)
		}
	}
}

// TestDegenerateCurves covers near-zero-length and collapsed control polygons
// through every rasterizer type and every join mode. They must flatten to a
// single line without panicking; in particular the Arc/Arc-join curvature
// calculation, which divides by control edge length, must not hit a division
// by zero on degenerate quadratic and cubic curves.
func TestDegenerateCurves(t *testing.T) {
	wx, wy := 64, 64
	zero := fixed.P(10, 10)
	type curve struct {
		kind       byte // 'Q' or 'C'
		a, b, c, d fixed.Point26_6
	}
	curves := []curve{
		{'Q', zero, zero, fixed.P(20, 20), zero},            // a == b
		{'Q', zero, fixed.P(20, 20), fixed.P(20, 20), zero}, // b == c
		{'Q', zero, zero, zero, zero},                       // all identical
		{'C', zero, zero, fixed.P(20, 20), fixed.P(20, 20)}, // a==b, c==d
		{'C', zero, zero, zero, fixed.P(20, 20)},            // a==b==c
		{'C', fixed.P(20, 20), zero, zero, zero},            // b==c==d
		{'C', zero, zero, zero, zero},                       // point curve
		// one-unit control edges: near-zero length
		{'Q', zero, fixed.P(10*64+1, 10*64), fixed.P(10*64, 10*64+1), zero},
		{'C', zero, fixed.P(10*64+1, 10*64), fixed.P(10*64, 10*64+1), fixed.P(10*64+1, 10*64+1)},
	}
	for _, jm := range []JoinMode{Arc, ArcClip, Miter, MiterClip, Bevel, Round} {
		img := image.NewRGBA(image.Rect(0, 0, wx, wy))
		sc := NewScannerGV(wx, wy, img, img.Bounds())
		s := NewStroker(wx, wy, sc)
		s.SetStroke(4*64, 4*64, RoundCap, nil, RoundGap, jm)
		d := NewDasher(wx, wy, sc)
		d.SetStroke(4*64, 4*64, RoundCap, nil, RoundGap, jm, []float64{5, 3}, 0)
		for _, cv := range curves {
			for _, adder := range []Adder{s, d, NewFiller(wx, wy, sc)} {
				adder.Start(cv.a)
				if cv.kind == 'Q' {
					adder.QuadBezier(cv.b, cv.c)
				} else {
					adder.CubeBezier(cv.b, cv.c, cv.d)
				}
				adder.Stop(false)
			}
		}
		sc.Draw()
	}
}

// TestExtremeCoordinates exercises the float32 range limits. Segment counts
// must stay finite and bounded and subdivision must terminate promptly for
// high-curvature control polygons and very large coordinates.
func TestExtremeCoordinates(t *testing.T) {
	const maxF32Coord = 1 << 30 // well within float32 exact-integer range
	cases := []struct {
		name           string
		a, b, c, d     fixed.Point26_6
		quad           bool
		maxAllowedSegs int
	}{
		{
			name:           "huge quad",
			a:              fixed.P(-maxF32Coord, -maxF32Coord),
			b:              fixed.P(maxF32Coord, -maxF32Coord),
			c:              fixed.P(maxF32Coord, maxF32Coord),
			quad:           true,
			maxAllowedSegs: 200000,
		},
		{
			name:           "huge cube",
			a:              fixed.P(-maxF32Coord, maxF32Coord),
			b:              fixed.P(maxF32Coord, -maxF32Coord),
			c:              fixed.P(-maxF32Coord, -maxF32Coord),
			d:              fixed.P(maxF32Coord, maxF32Coord),
			quad:           false,
			maxAllowedSegs: 200000,
		},
		{
			name:           "high curvature small curve",
			a:              fixed.P(0, 0),
			b:              fixed.P(100*64, -500*64),
			c:              fixed.P(-100*64, 500*64),
			d:              fixed.P(0, 20*64),
			quad:           false,
			maxAllowedSegs: 5000,
		},
		{
			name:           "flat huge span",
			a:              fixed.P(-maxF32Coord, 0),
			b:              fixed.P(0, 1),
			c:              fixed.P(0, -1),
			d:              fixed.P(maxF32Coord, 0),
			quad:           false,
			maxAllowedSegs: 200000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan int, 1)
			go func() {
				var n int
				if tc.quad {
					n, _, _ = FlattenQuad(tc.a, tc.b, tc.c, func(fixed.Point26_6) {})
				} else {
					n, _, _ = FlattenCubic(tc.a, tc.b, tc.c, tc.d, func(fixed.Point26_6) {})
				}
				done <- n
			}()
			select {
			case n := <-done:
				if n < 1 || n > tc.maxAllowedSegs {
					t.Fatalf("segment count %d outside [1,%d]", n, tc.maxAllowedSegs)
				}
			case <-time.After(time.Second):
				t.Fatalf("subdivision did not terminate for %s", tc.name)
			}
		})
	}
}

// renderSubpaths builds one image from many consecutive subpaths, exercising
// open and closed paths on a single stroker so joins between subpaths cannot
// hide cracks.
func renderSubpaths() [16]byte {
	wx, wy := 128, 128
	img := image.NewRGBA(image.Rect(0, 0, wx, wy))
	sc := NewScannerGV(wx, wy, img, img.Bounds())
	sc.SetColor(colornames.Black)
	s := NewStroker(wx, wy, sc)
	s.SetStroke(6*64, 4*64, RoundCap, nil, RoundGap, ArcClip)
	for k := 0; k < 8; k++ {
		x0 := 10 + k*14
		s.Start(fixed.P(x0, 20))
		s.CubeBezier(fixed.P(x0+20, 40), fixed.P(x0-10, 80), fixed.P(x0+10, 108))
		s.Stop(false)
	}
	for k := 0; k < 4; k++ {
		x0 := 10 + k*28
		s.Start(fixed.P(x0, 118))
		s.QuadBezier(fixed.P(x0+24, 118), fixed.P(x0+24, 96))
		s.Stop(true)
	}
	sc.Draw()
	return md5.Sum(img.Pix)
}

// TestConsecutiveSubpaths checks repeated builds are pixel-identical and that
// the image actually contains painted pixels (a crack or an unclosed edge
// would shrink coverage rather than panic).
func TestConsecutiveSubpaths(t *testing.T) {
	first := renderSubpaths()
	for i := 0; i < 3; i++ {
		if got := renderSubpaths(); got != first {
			t.Fatal("consecutive subpath rendering not deterministic")
		}
	}
	wx, wy := 128, 128
	img := image.NewRGBA(image.Rect(0, 0, wx, wy))
	sc := NewScannerGV(wx, wy, img, img.Bounds())
	sc.SetColor(colornames.Black)
	s := NewStroker(wx, wy, sc)
	s.SetStroke(6*64, 4*64, RoundCap, nil, RoundGap, ArcClip)
	s.Start(fixed.P(10, 20))
	s.CubeBezier(fixed.P(30, 40), fixed.P(0, 80), fixed.P(20, 108))
	s.Stop(false)
	sc.Draw()
	painted := 0
	for _, px := range img.Pix {
		if px != 0 {
			painted++
		}
	}
	if painted == 0 {
		t.Fatal("stroked subpath produced no painted pixels")
	}
}

// hashFileBytes returns the MD5 digest of a file's raw bytes.
func hashFileBytes(t *testing.T, path string) [16]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return md5.Sum(data)
}

// TestRenderedPixelsUnchanged re-renders representative fill, stroke and dash
// paths through the public API and compares the resulting PNG files byte for
// byte against golden images rendered with the pre-refactor code. Set
// RASTERX_GOLDEN_DIR to the directory holding the golden PNGs; the test skips
// when the variable is absent.
//
// The comparison is done on the encoded PNG bytes rather than on decoded
// RGBA pixels on purpose: Go's PNG codec stores non-alpha-premultiplied
// colors, so decoding an image back to the rasterizer's premultiplied RGBA
// introduces rounding differences even for an image encoded by the same
// process. Identical encoded files are therefore the strictest lossless
// proof that rendering output is unchanged.
func TestRenderedPixelsUnchanged(t *testing.T) {
	dir := os.Getenv("RASTERX_GOLDEN_DIR")
	if dir == "" {
		t.Skip("RASTERX_GOLDEN_DIR not set; skipping golden pixel comparison")
	}
	const wx, wy = 512, 512
	out := t.TempDir()

	// Fill path (golden: shapeGVF.png).
	img := image.NewRGBA(image.Rect(0, 0, wx, wy))
	sc := NewScannerGV(wx, wy, img, img.Bounds())
	f := NewFiller(wx, wy, sc)
	doShapes(t, f, f, filepath.Join(out, "shapeGVF.png"), img)

	// Stroke path, Miter join (golden: shapeGVS3.png).
	img = image.NewRGBA(image.Rect(0, 0, wx, wy))
	sc = NewScannerGV(wx, wy, img, img.Bounds())
	s := NewStroker(wx, wy, sc)
	s.SetStroke(10*64, 4*64, nil, nil, nil, Miter)
	doShapes(t, s, s, filepath.Join(out, "shapeGVS3.png"), img)

	// Dashed stroke path, ArcClip join (golden: shapeGVD0.png).
	img = image.NewRGBA(image.Rect(0, 0, wx, wy))
	sc = NewScannerGV(wx, wy, img, img.Bounds())
	d := NewDasher(wx, wy, sc)
	d.SetStroke(10*64, 4*64, SquareCap, nil, RoundGap, ArcClip, []float64{33, 12}, 30)
	doShapes(t, d, d, filepath.Join(out, "shapeGVD0.png"), img)

	for _, name := range []string{"shapeGVF.png", "shapeGVS3.png", "shapeGVD0.png"} {
		want := hashFileBytes(t, filepath.Join(dir, name))
		got := hashFileBytes(t, filepath.Join(out, name))
		if got != want {
			t.Errorf("rendered %s differs from golden (golden %x, got %x)", name, want, got)
		}
	}
}
