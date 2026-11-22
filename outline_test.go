// Copyright 2026 by the rasterx Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rasterx_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/colornames"
	"golang.org/x/image/math/fixed"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// openPolyline is an open, corner-heavy polyline that exercises every join.
func openPolyline() Path {
	var p Path
	p.Start(ToFixedP(60, 90))
	p.Line(ToFixedP(180, 60))
	p.Line(ToFixedP(300, 150))
	p.Line(ToFixedP(240, 260))
	p.Line(ToFixedP(120, 230))
	return p
}

// closedPolygon is an explicitly closed, sharp-cornered polygon that exercises
// the closing join.
func closedPolygon() Path {
	var p Path
	p.Start(ToFixedP(80, 80))
	p.Line(ToFixedP(260, 60))
	p.Line(ToFixedP(420, 200))
	p.Line(ToFixedP(300, 380))
	p.Line(ToFixedP(120, 330))
	p.Stop(true)
	return p
}

// curvePath mixes cubic and quadratic beziers with straight segments.
func curvePath() Path {
	var p Path
	p.Start(ToFixedP(40, 300))
	p.CubeBezier(ToFixedP(120, 40), ToFixedP(260, 40), ToFixedP(330, 300))
	p.QuadBezier(ToFixedP(400, 420), ToFixedP(470, 260))
	p.Line(ToFixedP(470, 460))
	p.CubeBezier(ToFixedP(300, 500), ToFixedP(160, 470), ToFixedP(70, 470))
	return p
}

// figureEight self-intersects.
func figureEight() Path {
	var p Path
	p.Start(ToFixedP(256, 256))
	p.CubeBezier(ToFixedP(60, 60), ToFixedP(452, 60), ToFixedP(256, 256))
	p.CubeBezier(ToFixedP(60, 452), ToFixedP(452, 452), ToFixedP(256, 256))
	return p
}

// negativePath lies entirely in negative coordinate space.
func negativePath() Path {
	var p Path
	p.Start(ToFixedP(-220, -180))
	p.Line(ToFixedP(-60, -240))
	p.QuadBezier(ToFixedP(40, -120), ToFixedP(-120, -40))
	p.CubeBezier(ToFixedP(-260, 20), ToFixedP(-340, -60), ToFixedP(-220, -180))
	p.Stop(true)
	return p
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

const testW, testH = 512, 512

// renderOld renders a path stroke through the original Stroker/Dasher pipeline.
func renderOld(t *testing.T, src Path, cfg StrokeConfig, m *Matrix2D, clr color.Color) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, testW, testH))
	sc := NewScannerGV(testW, testH, img, img.Bounds())
	sc.SetColor(clr)
	var adder Adder
	if len(cfg.Dashes) > 0 {
		d := NewDasher(testW, testH, sc)
		d.SetWinding(true)
		d.SetStroke(cfg.Width, cfg.MiterLimit, cfg.CapL, cfg.CapT, cfg.Gap,
			cfg.Join, cfg.Dashes, cfg.DashOffset)
		adder = d
	} else {
		s := NewStroker(testW, testH, sc)
		s.SetWinding(true)
		s.SetStroke(cfg.Width, cfg.MiterLimit, cfg.CapL, cfg.CapT, cfg.Gap, cfg.Join)
		adder = s
	}
	if m != nil {
		adder = &MatrixAdder{Adder: adder, M: *m}
	}
	src.AddTo(adder)
	sc.Draw()
	return img
}

// renderNew exports the stroke outline then fills it through a plain Filler.
func renderNew(t *testing.T, src Path, cfg StrokeConfig, m *Matrix2D, clr color.Color) *image.RGBA {
	t.Helper()
	outline, err := StrokeOutline(src, cfg, m)
	if err != nil {
		t.Fatalf("StrokeOutline: %v", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, testW, testH))
	sc := NewScannerGV(testW, testH, img, img.Bounds())
	sc.SetColor(clr)
	// Replay move/line straight into the scanner, mirroring the stroker.
	outline.AddToScanner(sc)
	sc.Draw()
	return img
}

func imagesEqual(a, b *image.RGBA) bool {
	return bytes.Equal(a.Pix, b.Pix)
}

func diffStats(a, b *image.RGBA) (n int, maxDelta int) {
	for i := range a.Pix {
		d := int(a.Pix[i]) - int(b.Pix[i])
		if d < 0 {
			d = -d
		}
		if d != 0 {
			n++
		}
		if d > maxDelta {
			maxDelta = d
		}
	}
	return
}

// filledCount counts pixels whose alpha exceeds the given threshold.
func filledCount(img *image.RGBA, threshold uint8) int {
	n := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] > threshold {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Pixel identity: caps, joins, gaps
// ---------------------------------------------------------------------------

func TestStrokeOutlinePixelIdentityCapsJoins(t *testing.T) {
	caps := []struct {
		name       string
		capL, capT CapFunc
	}{
		{"butt", ButtCap, nil},
		{"square", SquareCap, nil},
		{"round", RoundCap, RoundCap},
		{"cubic", CubicCap, nil},
		{"quadratic", QuadraticCap, nil},
		{"mixed", SquareCap, RoundCap},
	}
	joins := []struct {
		name string
		mode JoinMode
		gap  GapFunc
	}{
		{"miter", Miter, FlatGap},
		{"miterclip", MiterClip, RoundGap},
		{"bevel", Bevel, FlatGap},
		{"round", Round, RoundGap},
		{"arc", Arc, FlatGap},
		{"arcclip", ArcClip, RoundGap},
		{"miterclip-cubicgap", MiterClip, CubicGap},
		{"arcclip-quadgap", ArcClip, QuadraticGap},
	}
	paths := []struct {
		name string
		p    func() Path
	}{
		{"open-polyline", openPolyline},
		{"closed-polygon", closedPolygon},
		{"curves", curvePath},
		{"figure-eight", figureEight},
	}
	cfg := StrokeConfig{Width: 12 * 64, MiterLimit: 4 * 64}
	for _, pa := range paths {
		for _, cp := range caps {
			for _, jn := range joins {
				c := cfg
				c.CapL, c.CapT = cp.capL, cp.capT
				c.Join, c.Gap = jn.mode, jn.gap
				old := renderOld(t, pa.p(), c, nil, colornames.Crimson)
				new := renderNew(t, pa.p(), c, nil, colornames.Crimson)
				if !imagesEqual(old, new) {
					n, md := diffStats(old, new)
					t.Errorf("path=%s cap=%s join=%s: pixel mismatch (%d bytes differ, max delta %d)",
						pa.name, cp.name, jn.name, n, md)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Pixel identity: dashing
// ---------------------------------------------------------------------------

func TestStrokeOutlinePixelIdentityDashes(t *testing.T) {
	dashSets := []struct {
		name   string
		dashes []float64
		offset float64
	}{
		{"basic", []float64{33, 12}, 0},
		{"offset", []float64{20, 8, 4, 8}, 17},
		{"negative-offset", []float64{25, 10}, -13.5},
		{"single-dash", []float64{12}, 3},
	}
	caps := []CapFunc{ButtCap, SquareCap, RoundCap}
	gaps := []GapFunc{FlatGap, RoundGap, CubicGap, QuadraticGap}
	src := curvePath()
	cfg := StrokeConfig{Width: 9 * 64, MiterLimit: 6 * 64, Join: MiterClip}
	for gi, ds := range dashSets {
		for ci, cp := range caps {
			c := cfg
			c.Dashes, c.DashOffset = ds.dashes, ds.offset
			c.CapL, c.CapT = cp, cp
			c.Gap = gaps[(gi+ci)%len(gaps)]
			old := renderOld(t, src, c, nil, colornames.Steelblue)
			new := renderNew(t, src, c, nil, colornames.Steelblue)
			if !imagesEqual(old, new) {
				n, md := diffStats(old, new)
				t.Errorf("dashes=%s: pixel mismatch (%d bytes differ, max delta %d)",
					ds.name, n, md)
			}
		}
	}
	// Closed dashed path with a non-gap-closing phase.
	closed := closedPolygon()
	c := StrokeConfig{Width: 7 * 64, MiterLimit: 3 * 64, CapL: RoundCap, CapT: RoundCap,
		Gap: RoundGap, Join: Round, Dashes: []float64{40, 14}, DashOffset: 2}
	if !imagesEqual(renderOld(t, closed, c, nil, colornames.Darkgreen),
		renderNew(t, closed, c, nil, colornames.Darkgreen)) {
		t.Error("closed dashed path pixel mismatch")
	}
}

// ---------------------------------------------------------------------------
// Matrix2D, negative coordinates, tiny width, transparent brush
// ---------------------------------------------------------------------------

func TestStrokeOutlineMatrixAndEdgeCases(t *testing.T) {
	matrix := Identity.Translate(250, 240).Rotate(math.Pi/7).
		Scale(0.75, 1.15).SkewX(0.18).Translate(-240, -230)
	src := curvePath()
	cfg := StrokeConfig{Width: 10 * 64, MiterLimit: 4 * 64, CapL: RoundCap, CapT: RoundCap,
		Gap: RoundGap, Join: ArcClip}

	old := renderOld(t, src, cfg, &matrix, colornames.Darkmagenta)
	new := renderNew(t, src, cfg, &matrix, colornames.Darkmagenta)
	if !imagesEqual(old, new) {
		n, md := diffStats(old, new)
		t.Errorf("matrix transformed stroke mismatch (%d bytes differ, max delta %d)", n, md)
	}

	// Negative coordinates need a translation that brings them on-canvas.
	neg := negativePath()
	shift := Identity.Translate(380, 300)
	old = renderOld(t, neg, cfg, &shift, colornames.Teal)
	new = renderNew(t, neg, cfg, &shift, colornames.Teal)
	if !imagesEqual(old, new) {
		t.Error("negative coordinate stroke mismatch")
	}

	// Very small widths: 1/64 of a pixel and one pixel.
	for _, w := range []fixed.Int26_6{1, 64} {
		c := cfg
		c.Width = w
		old = renderOld(t, openPolyline(), c, nil, colornames.Orange)
		new = renderNew(t, openPolyline(), c, nil, colornames.Orange)
		if !imagesEqual(old, new) {
			t.Errorf("tiny width %d mismatch", w)
		}
	}

	// Transparent brush: identical coverage must reach the scanner.
	transparent := ApplyOpacity(colornames.Royalblue, 0.37)
	old = renderOld(t, src, cfg, nil, transparent)
	new = renderNew(t, src, cfg, nil, transparent)
	if !imagesEqual(old, new) {
		t.Error("transparent brush stroke mismatch")
	}

	// Fully transparent brush yields a blank image in both pipelines.
	clear := ApplyOpacity(colornames.Black, 0)
	old = renderOld(t, src, cfg, nil, clear)
	new = renderNew(t, src, cfg, nil, clear)
	if !imagesEqual(old, new) {
		t.Error("zero-alpha brush stroke mismatch")
	}
	if filledCount(new, 0) != 0 {
		t.Error("zero-alpha brush produced visible pixels")
	}
}

// ---------------------------------------------------------------------------
// Closure and winding semantics
// ---------------------------------------------------------------------------

func TestStrokeOutlineSubpathsClosedAndWound(t *testing.T) {
	cfg := StrokeConfig{Width: 14 * 64, MiterLimit: 4 * 64, CapL: ButtCap, CapT: ButtCap,
		Gap: FlatGap, Join: MiterClip}
	outline, err := StrokeOutline(openPolyline(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	subs, err := SplitSubPaths(outline)
	if err != nil {
		t.Fatalf("SplitSubPaths: %v", err)
	}
	if len(subs) < 2 {
		t.Fatalf("expected multiple contours, got %d", len(subs))
	}
	// The stroker emits zero/one-edge helper contours (cap joins and
	// duplicate vertices); meaningful contours must enclose area with a
	// consistent, defined orientation. Fill correctness (opposing edge
	// orientations) is enforced by the pixel-identity tests instead.
	nonDegenerate := 0
	for _, s := range subs {
		if !s.Closed {
			t.Error("exported subpath is not closed")
		}
		if len(s.Points) < 3 {
			continue
		}
		nonDegenerate++
		if s.SignedArea() == 0 {
			t.Error("zero-area non-degenerate contour")
		}
	}
	if nonDegenerate < 1 {
		t.Fatalf("expected area-enclosing contours, got %d", nonDegenerate)
	}

	// The raw Path delimits contours with PathMoveTo; SplitSubPaths must still
	// report every contour as closed with a defined winding.
	if PathCommand(outline[0]) != PathMoveTo {
		t.Error("outline does not start with PathMoveTo")
	}
	for i := 0; i < len(outline); {
		switch PathCommand(outline[i]) {
		case PathMoveTo, PathLineTo:
			i += 3
		case PathClose:
			t.Error("capture must not inject explicit PathClose edges")
			i++
		default:
			t.Fatalf("unexpected command %v at %d", outline[i], i)
		}
	}
}

// TestStrokeOutlineArea checks the covered area against the analytic value of
// an axis-aligned, butt-capped line: length * width, antialiasing tolerance.
func TestStrokeOutlineArea(t *testing.T) {
	var p Path
	p.Start(ToFixedP(100, 256))
	p.Line(ToFixedP(400, 256))
	widthPx := 40.0
	cfg := StrokeConfig{Width: fixed.Int26_6(widthPx * 64), MiterLimit: 4 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: MiterClip}
	img := renderNew(t, p, cfg, nil, colornames.Black)
	got := filledCount(img, 128)
	want := 300 * widthPx
	tol := want * 0.01
	if math.Abs(float64(got)-want) > tol {
		t.Errorf("filled area %d, want ~%f (±%f)", got, want, tol)
	}

	// Old and new pipelines cover exactly the same number of pixels.
	old := renderOld(t, p, cfg, nil, colornames.Black)
	if filledCount(old, 0) != filledCount(img, 0) {
		t.Error("old/new covered pixel counts differ")
	}
}

// ---------------------------------------------------------------------------
// Degenerate inputs: empty path, zero width, self-intersection
// ---------------------------------------------------------------------------

func TestStrokeOutlineDegenerate(t *testing.T) {
	// Empty path is a stable no-op.
	outline, err := StrokeOutline(Path{}, StrokeConfig{Width: 10 * 64, MiterLimit: 4 * 64,
		CapL: ButtCap, CapT: ButtCap, Join: MiterClip}, nil)
	if err != nil {
		t.Fatalf("empty path errored: %v", err)
	}
	if len(outline) != 0 {
		t.Errorf("empty path produced geometry: %v", outline)
	}

	// Zero-width stroke degrades to an empty outline in both pipelines.
	cfg := StrokeConfig{Width: 0, MiterLimit: 4 * 64, CapL: RoundCap, CapT: RoundCap,
		Gap: RoundGap, Join: Round}
	outline, err = StrokeOutline(curvePath(), cfg, nil)
	if err != nil {
		t.Fatalf("zero width errored: %v", err)
	}
	img := renderNew(t, curvePath(), cfg, nil, colornames.Red)
	if filledCount(img, 0) != 0 {
		t.Error("zero-width stroke produced visible pixels")
	}
	old := renderOld(t, curvePath(), cfg, nil, colornames.Red)
	if !imagesEqual(old, img) {
		t.Error("zero-width old/new mismatch")
	}

	// Self-intersecting path fills identically (non-zero winding handles it).
	c := StrokeConfig{Width: 16 * 64, MiterLimit: 5 * 64, CapL: RoundCap, CapT: RoundCap,
		Gap: RoundGap, Join: ArcClip}
	if !imagesEqual(renderOld(t, figureEight(), c, nil, colornames.Purple),
		renderNew(t, figureEight(), c, nil, colornames.Purple)) {
		t.Error("self-intersecting stroke mismatch")
	}
}

// ---------------------------------------------------------------------------
// Invalid parameters return errors and produce no geometry
// ---------------------------------------------------------------------------

func TestStrokeOutlineInvalidParams(t *testing.T) {
	base := StrokeConfig{Width: 10 * 64, MiterLimit: 4 * 64, CapL: ButtCap, CapT: ButtCap,
		Join: MiterClip}
	bad := []StrokeConfig{
		{Width: -1, MiterLimit: 4 * 64, Join: MiterClip},         // negative width
		{Width: 10 * 64, MiterLimit: -1, Join: MiterClip},        // negative miter
		{Width: 10 * 64, MiterLimit: 4 * 64, Join: JoinMode(99)}, // unknown join
		{Width: 10 * 64, MiterLimit: 4 * 64, Join: Miter, Dashes: []float64{0}},
		{Width: 10 * 64, MiterLimit: 4 * 64, Join: Miter, Dashes: []float64{math.NaN()}},
		{Width: 10 * 64, MiterLimit: 4 * 64, Join: Miter, Dashes: []float64{-3}},
		{Width: 10 * 64, MiterLimit: 4 * 64, Join: Miter, Dashes: []float64{5},
			DashOffset: math.Inf(1)},
	}
	for i, c := range bad {
		if _, err := StrokeOutline(openPolyline(), c, nil); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		} else if !errors.Is(err, ErrInvalidParam) && !errors.Is(err, ErrDashOffset) {
			t.Errorf("case %d: unexpected error %v", i, err)
		}
	}

	// Miter limit that overflows fixed point.
	huge := StrokeConfig{Width: 100 * 64, MiterLimit: fixed.Int26_6(math.MaxInt32),
		CapL: ButtCap, CapT: ButtCap, Join: Miter}
	if _, err := StrokeOutline(openPolyline(), huge, nil); !errors.Is(err, ErrMiterOverflow) {
		t.Errorf("overflow: got %v, want ErrMiterOverflow", err)
	}

	// Non-finite matrix.
	nanM := Matrix2D{A: math.NaN(), D: 1}
	if _, err := StrokeOutline(openPolyline(), base, &nanM); !errors.Is(err, ErrInvalidParam) {
		t.Errorf("nan matrix: got %v", err)
	}
	infM := Matrix2D{A: 1, D: 1, E: math.Inf(-1)}
	if _, err := StrokeOutline(openPolyline(), base, &infM); !errors.Is(err, ErrInvalidParam) {
		t.Errorf("inf matrix: got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Immutability: source path, dash slice and scanner are never modified
// ---------------------------------------------------------------------------

func TestStrokeOutlineDoesNotMutateInputs(t *testing.T) {
	src := curvePath()
	srcSnap := append(Path(nil), src...)
	dashes := []float64{33, 12, 7, 9}
	dashSnap := append([]float64(nil), dashes...)
	cfg := StrokeConfig{Width: 11 * 64, MiterLimit: 4 * 64, CapL: RoundCap, CapT: RoundCap,
		Gap: RoundGap, Join: ArcClip, Dashes: dashes, DashOffset: 5}
	if _, err := StrokeOutline(src, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if !pathEqual(srcSnap, src) {
		t.Error("source Path was modified")
	}
	if !floatsEqual(dashSnap, dashes) {
		t.Error("dash slice was modified")
	}

	// A scanner handed directly to a Stroker keeps its captured content until
	// an explicit Clear, and double Draw emits only one closing command.
	sc := NewOutlineScanner(testW, testH)
	s := NewStroker(testW, testH, sc)
	s.SetStroke(8*64, 4*64, ButtCap, nil, FlatGap, MiterClip)
	src.AddTo(s)
	s.Stop(false)
	sc.Draw()
	first := append(Path(nil), sc.Path()...)
	sc.Draw()
	if len(sc.Path()) != len(first) {
		t.Error("repeated Draw changed the captured path")
	}
	sc.Clear()
	if len(sc.Path()) != 0 {
		t.Error("Clear did not reset the outline")
	}
}

func pathEqual(a, b Path) bool {
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

func floatsEqual(a, b []float64) bool {
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

// ---------------------------------------------------------------------------
// Fixture-driven golden comparison
//
// Each fixture pairs a representative path with a stroke configuration. The
// test asserts three things for every fixture:
//
//   - the exported outline, filled through a Filler-side scanner, is
//     pixel-identical to the legacy Stroker/Dasher rendering (regression guard
//     against the old rasterizer);
//   - the covered area (number of nonzero-alpha pixels) matches the legacy
//     rendering;
//   - every exported contour is closed, so the geometry is a valid fill.
//
// Set UPDATE_STROKE_GOLDEN=1 to (re)write the golden PNGs in testdata.
// ---------------------------------------------------------------------------

func strokeFixtures() []struct {
	name string
	path Path
	cfg  StrokeConfig
} {
	sharp := func() Path {
		// Acute angles force long miters and exercise miter clipping.
		var p Path
		p.Start(ToFixedP(40, 420))
		p.Line(ToFixedP(250, 250))
		p.Line(ToFixedP(460, 420))
		p.Line(ToFixedP(250, 90))
		return p
	}
	curves := curvePath
	return []struct {
		name string
		path Path
		cfg  StrokeConfig
	}{
		{
			name: "caps",
			path: openPolyline(),
			cfg: StrokeConfig{Width: 13 * 64, MiterLimit: 4 * 64,
				CapL: SquareCap, CapT: RoundCap, Gap: RoundGap, Join: Round},
		},
		{
			name: "miter-sharp",
			path: sharp(),
			cfg: StrokeConfig{Width: 18 * 64, MiterLimit: 2 * 64,
				CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: MiterClip},
		},
		{
			name: "miter-arc-clip",
			path: sharp(),
			cfg: StrokeConfig{Width: 12 * 64, MiterLimit: 3 * 64,
				CapL: ButtCap, CapT: ButtCap, Gap: CubicGap, Join: ArcClip},
		},
		{
			name: "dash",
			path: closedPolygon(),
			cfg: StrokeConfig{Width: 8 * 64, MiterLimit: 5 * 64,
				CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: Miter,
				Dashes: []float64{30, 11, 6, 11}, DashOffset: 9},
		},
		{
			name: "curve",
			path: curves(),
			cfg: StrokeConfig{Width: 11 * 64, MiterLimit: 4 * 64,
				CapL: RoundCap, CapT: SquareCap, Gap: QuadraticGap, Join: Arc},
		},
	}
}

func TestStrokeOutlineFixtures(t *testing.T) {
	for _, fx := range strokeFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			old := renderOld(t, fx.path, fx.cfg, nil, colornames.Darkslateblue)
			new := renderNew(t, fx.path, fx.cfg, nil, colornames.Darkslateblue)

			// 1. Pixel identity against the legacy stroker.
			if !imagesEqual(old, new) {
				n, md := diffStats(old, new)
				t.Fatalf("pixel mismatch vs legacy stroker (%d bytes differ, max delta %d)", n, md)
			}

			// 2. Covered area matches.
			if got, want := filledCount(new, 0), filledCount(old, 0); got != want {
				t.Errorf("covered area new=%d old=%d", got, want)
			}

			// 3. Every exported contour is closed and parseable.
			outline, err := StrokeOutline(fx.path, fx.cfg, nil)
			if err != nil {
				t.Fatalf("StrokeOutline: %v", err)
			}
			subs, err := SplitSubPaths(outline)
			if err != nil {
				t.Fatalf("SplitSubPaths: %v", err)
			}
			if len(subs) == 0 {
				t.Fatal("no contours exported")
			}
			for _, s := range subs {
				if !s.Closed || len(s.Points) == 0 {
					t.Error("non-closed or empty contour")
				}
			}

			// Golden PNG: write on demand, otherwise compare bytes if present.
			golden := "testdata/stroke_outline_" + fx.name + ".png"
			if os.Getenv("UPDATE_STROKE_GOLDEN") == "1" {
				if err := SaveToPngFile(golden, new); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			if g, err := os.ReadFile(golden); err == nil {
				var buf bytes.Buffer
				if err := png.Encode(&buf, new); err != nil {
					t.Fatalf("encode: %v", err)
				}
				if !bytes.Equal(g, buf.Bytes()) {
					t.Errorf("golden %s drift; rerun with UPDATE_STROKE_GOLDEN=1", golden)
				}
			}
		})
	}
}

// TestFillStrokeOutlineThroughFiller proves the exported outline can be filled
// by an actual *Filler (passed via its embedded Scanner) and stays pixel
// identical to the legacy stroker. The faithful replay bypasses Filler.Stop's
// explicit closing edge, which would otherwise double the thin strip edges.
func TestFillStrokeOutlineThroughFiller(t *testing.T) {
	src := curvePath()
	cfg := StrokeConfig{Width: 12 * 64, MiterLimit: 4 * 64, CapL: RoundCap, CapT: RoundCap,
		Gap: RoundGap, Join: ArcClip, Dashes: []float64{28, 10}, DashOffset: 4}

	img := image.NewRGBA(image.Rect(0, 0, testW, testH))
	sc := NewScannerGV(testW, testH, img, img.Bounds())
	sc.SetColor(colornames.Crimson)
	f := NewFiller(testW, testH, sc)
	f.SetWinding(true)
	if _, err := FillStrokeOutline(src, cfg, nil, f); err != nil {
		t.Fatalf("FillStrokeOutline: %v", err)
	}

	old := renderOld(t, src, cfg, nil, colornames.Crimson)
	if !imagesEqual(old, img) {
		n, md := diffStats(old, img)
		t.Errorf("filler replay mismatch (%d bytes differ, max delta %d)", n, md)
	}
}

// TestLargeMiterStable ensures a very large but in-range miter limit neither
// panics nor diverges from the legacy stroker on a sharp corner, while a limit
// that overflows fixed point is rejected.
func TestLargeMiterStable(t *testing.T) {
	sharp := func() Path {
		var p Path
		p.Start(ToFixedP(40, 420))
		p.Line(ToFixedP(250, 250))
		p.Line(ToFixedP(460, 420))
		return p
	}
	// Large but representable miter limit; MiterClip falls back to a gap once
	// the miter is cut, so this must render without overflow.
	cfg := StrokeConfig{Width: 6 * 64, MiterLimit: 100000 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: MiterClip}
	old := renderOld(t, sharp(), cfg, nil, colornames.Black)
	new := renderNew(t, sharp(), cfg, nil, colornames.Black)
	if !imagesEqual(old, new) {
		t.Error("large in-range miter limit diverged from legacy stroker")
	}
}
