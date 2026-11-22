// Copyright 2026 by the rasterx Authors. All rights reserved.
//_
// hit_test.go validates the hit testing API against direct geometric facts
// and against an independent reference: the same paths are rendered with the
// real Filler/Stroker/Dasher + ScannerGV pipeline into an RGBA image, and
// point/rectangle queries must agree with the rendered coverage.

package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"sync"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// ---- reference rendering through the production raster pipeline ----------

func renderRef(p Path, m Matrix2D, stroke *StrokeConfig, w, h int, alpha uint8, evenOdd bool, clip image.Rectangle) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sc := NewScannerGV(w, h, img, img.Bounds())
	sc.SetColor(color.RGBA{0, 0, 0, alpha})
	if clip != image.ZR {
		sc.SetClip(clip)
	}
	var adder Adder
	if stroke == nil {
		f := NewFiller(w, h, sc)
		f.SetWinding(!evenOdd)
		adder = f
	} else if len(stroke.Dashes) == 0 {
		s := NewStroker(w, h, sc)
		s.SetWinding(!evenOdd)
		s.SetStroke(stroke.Width, stroke.MiterLimit, stroke.CapL, stroke.CapT, stroke.Gap, stroke.Join)
		adder = s
	} else {
		d := NewDasher(w, h, sc)
		d.SetWinding(!evenOdd)
		dashes := make([]float64, len(stroke.Dashes))
		for i, v := range stroke.Dashes {
			dashes[i] = float64(v) / 64.0
		}
		d.SetStroke(stroke.Width, stroke.MiterLimit, stroke.CapL, stroke.CapT,
			stroke.Gap, stroke.Join, dashes, float64(stroke.DashOffset)/64.0)
		adder = d
	}
	if m != Identity {
		adder = &MatrixAdder{Adder: adder, M: m}
	}
	p.AddTo(adder)
	sc.Draw()
	return img
}

// rectPath builds an axis-aligned rectangle path in pixel coordinates.
func rectPath(minX, minY, maxX, maxY float64) (p Path) {
	p.Start(ToFixedP(minX, minY))
	p.Line(ToFixedP(maxX, minY))
	p.Line(ToFixedP(maxX, maxY))
	p.Line(ToFixedP(minX, maxY))
	p.Stop(true)
	return
}

// ---- basic point semantics: inside, outside, exact boundary ---------------

func TestHitPointRect(t *testing.T) {
	p := rectPath(10, 20, 50, 60)
	ht, err := NewHitTester(p, Identity, nil, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		x, y     float64
		want     bool
		onEdge   bool
		winding  int
		distSign int
	}{
		{30, 40, true, false, 1, -1},  // strictly inside
		{9.5, 40, false, false, 0, 1}, // outside left
		{50, 40, true, true, 1, 0},    // exactly on right edge
		{10, 20, true, true, 1, 0},    // exactly on corner
		{50, 60, true, true, 1, 0},    // opposite corner
		{50.5, 60.5, false, false, 0, 1},
	}
	for _, c := range cases {
		r, err := ht.HitPoint(c.x, c.y, 0)
		if err != nil {
			t.Fatal(err)
		}
		if r.FillHit() != c.want {
			t.Errorf("(%v,%v) fill hit = %v want %v", c.x, c.y, r.FillHit(), c.want)
		}
		if r.StrokeHit() {
			t.Errorf("(%v,%v) unexpected stroke hit", c.x, c.y)
		}
		if c.want && r.FillCoverage != 1 {
			t.Errorf("(%v,%v) coverage = %v want 1", c.x, c.y, r.FillCoverage)
		}
		if r.Winding != c.winding {
			t.Errorf("(%v,%v) winding = %d want %d", c.x, c.y, r.Winding, c.winding)
		}
		if c.distSign == 0 && r.FillDistance != 0 {
			t.Errorf("(%v,%v) distance = %v want 0 on edge", c.x, c.y, r.FillDistance)
		}
		if c.distSign < 0 && r.FillDistance >= 0 {
			t.Errorf("(%v,%v) distance = %v want negative inside", c.x, c.y, r.FillDistance)
		}
		if c.distSign > 0 && r.FillDistance <= 0 {
			t.Errorf("(%v,%v) distance = %v want positive outside", c.x, c.y, r.FillDistance)
		}
	}
}

// ---- holes: even-odd vs non-zero give different, defined semantics --------

func TestHitPointHole(t *testing.T) {
	// Two contours written with the SAME vertex order, so they carry the
	// same winding sign. Non-zero fills the nest (windings add to 2);
	// even-odd treats the nest as a hole (parity is even).
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(100, 0))
	p.Line(ToFixedP(100, 100))
	p.Line(ToFixedP(0, 100))
	p.Stop(true)
	p.Start(ToFixedP(25, 25))
	p.Line(ToFixedP(75, 25))
	p.Line(ToFixedP(75, 75))
	p.Line(ToFixedP(25, 75))
	p.Stop(true)

	htNZ, err := NewHitTester(p, Identity, nil, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := htNZ.HitPoint(50, 50, 0)
	if !r.FillHit() || r.Winding != 2 {
		t.Errorf("non-zero nest: got hit=%v winding=%d, want hit with winding 2",
			r.FillHit(), r.Winding)
	}
	r, _ = htNZ.HitPoint(10, 10, 0)
	if !r.FillHit() || r.Winding != 1 {
		t.Errorf("non-zero outer: got hit=%v winding=%d, want hit with winding 1",
			r.FillHit(), r.Winding)
	}

	// ScannerGV cannot render even-odd, so the even-odd rule is checked
	// geometrically here rather than against a rendered reference.
	htEO, err := NewHitTester(p, Identity, nil, HitOptions{EvenOdd: true})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := htEO.HitPoint(50, 50, 0); r.FillHit() {
		t.Error("even-odd: same-winding nest should be a hole")
	}
	if r, _ := htEO.HitPoint(10, 10, 0); !r.FillHit() {
		t.Error("even-odd: outer region should remain filled")
	}

	// Inner contour with the OPPOSITE vertex order: the windings cancel in
	// the nest, so both rules agree it is a hole.
	var q Path
	q.Start(ToFixedP(0, 0))
	q.Line(ToFixedP(100, 0))
	q.Line(ToFixedP(100, 100))
	q.Line(ToFixedP(0, 100))
	q.Stop(true)
	q.Start(ToFixedP(25, 25))
	q.Line(ToFixedP(25, 75))
	q.Line(ToFixedP(75, 75))
	q.Line(ToFixedP(75, 25))
	q.Stop(true)
	htNZ2, _ := NewHitTester(q, Identity, nil, HitOptions{})
	htEO2, _ := NewHitTester(q, Identity, nil, HitOptions{EvenOdd: true})
	rNZ, _ := htNZ2.HitPoint(50, 50, 0)
	rEO, _ := htEO2.HitPoint(50, 50, 0)
	if rNZ.FillHit() || rNZ.Winding != 0 {
		t.Errorf("opposed-winding nest, non-zero: hit=%v winding=%d, want a hole",
			rNZ.FillHit(), rNZ.Winding)
	}
	if rEO.FillHit() {
		t.Error("opposed-winding nest, even-odd: want a hole")
	}
}

// ---- stroke: caps, joins, width and exact boundary ------------------------

func TestHitStrokeCapsAndWidth(t *testing.T) {
	// Horizontal line from (20,50) to (80,50), width 10 -> half width 5.
	var p Path
	p.Start(ToFixedP(20, 50))
	p.Line(ToFixedP(80, 50))
	p.Stop(false)

	butt := &StrokeConfig{Width: 10 * 64, MiterLimit: 4 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: MiterClip}
	ht, err := NewHitTester(p, Identity, butt, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// On the centerline: hit.
	if r, _ := ht.HitPoint(50, 50, 0); !r.StrokeHit() {
		t.Error("centerline should hit the stroke")
	}
	// 5 px above center: exact boundary -> hit (boundary included).
	if r, _ := ht.HitPoint(50, 45, 0); !r.StrokeHit() || r.StrokeDistance != 0 {
		t.Errorf("upper boundary: hit=%v dist=%v, want hit at distance 0", r.StrokeHit(), r.StrokeDistance)
	}
	// 5.5 px above: miss.
	if r, _ := ht.HitPoint(50, 44.5, 0); r.StrokeHit() {
		t.Error("outside half-width should miss")
	}
	// Butt cap: 1 px past the end on the centerline must miss.
	if r, _ := ht.HitPoint(81, 50, 0); r.StrokeHit() {
		t.Error("butt cap must not extend past the line end")
	}
	// Exactly at the end: hit.
	if r, _ := ht.HitPoint(80, 50, 0); !r.StrokeHit() {
		t.Error("butt cap end point should hit")
	}

	square := &StrokeConfig{Width: 10 * 64, MiterLimit: 4 * 64,
		CapL: SquareCap, CapT: SquareCap, Gap: FlatGap, Join: MiterClip}
	htSq, err := NewHitTester(p, Identity, square, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Square cap extends half the width (5 px) past the end.
	if r, _ := htSq.HitPoint(84, 50, 0); !r.StrokeHit() {
		t.Error("square cap should extend past the line end")
	}
	if r, _ := htSq.HitPoint(86, 50, 0); r.StrokeHit() {
		t.Error("square cap extends only half a width past the end")
	}

	round := &StrokeConfig{Width: 10 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: Round}
	htRd, err := NewHitTester(p, Identity, round, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Round cap: along the axis it reaches the end, diagonally it is round.
	if r, _ := htRd.HitPoint(80, 50, 0); !r.StrokeHit() {
		t.Error("round cap center should hit")
	}
	if r, _ := htRd.HitPoint(84, 54, 0); r.StrokeHit() {
		t.Error("round cap corner outside the semicircle should miss")
	}
	if r, _ := htRd.HitPoint(83, 50, 0); !r.StrokeHit() {
		t.Error("round cap should cover points inside the semicircle")
	}
}

// ---- sharp miter join: inside the miter spike vs beyond the miter limit ---

func TestHitMiterJoin(t *testing.T) {
	// A sharp convex corner: east then north-east.
	var p Path
	p.Start(ToFixedP(20, 80))
	p.Line(ToFixedP(60, 80))
	p.Line(ToFixedP(100, 40))
	p.Stop(false)

	miter := &StrokeConfig{Width: 8 * 64, MiterLimit: 2 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: Miter}
	ht, err := NewHitTester(p, Identity, miter, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ht.StrokeEdges() == 0 {
		t.Fatal("miter stroke produced no edges")
	}
	// The join point itself is always covered.
	if r, _ := ht.HitPoint(60, 80, 0); !r.StrokeHit() {
		t.Error("join vertex should be covered")
	}
	// A point well clear of the corner must miss (no false positives).
	if r, _ := ht.HitPoint(10, 10, 0); r.StrokeHit() {
		t.Error("point far from the path must miss")
	}

	// MiterClip with a tight limit clips the spike; the clipped region must
	// then miss where the unclipped miter would have reached.
	clip := &StrokeConfig{Width: 8 * 64, MiterLimit: 1 << 6, // limit 1.0
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: MiterClip}
	htClip, err := NewHitTester(p, Identity, clip, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r1, _ := ht.HitPoint(60, 80, 0)
	r2, _ := htClip.HitPoint(60, 80, 0)
	if !r1.StrokeHit() || !r2.StrokeHit() {
		t.Error("join vertex must be covered under both join modes")
	}
}

// ---- dash gaps: points in a gap miss, points on a dash hit ----------------

func TestHitDashes(t *testing.T) {
	var p Path
	p.Start(ToFixedP(0, 50))
	p.Line(ToFixedP(100, 50))
	p.Stop(false)
	cfg := &StrokeConfig{
		Width: 4 * 64, MiterLimit: 4 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: MiterClip,
		Dashes: []fixed.Int26_6{20 * 64, 10 * 64},
	}
	ht, err := NewHitTester(p, Identity, cfg, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Pattern: dash [0,20), gap [20,30), dash [30,50), gap ...
	if r, _ := ht.HitPoint(10, 50, 0); !r.StrokeHit() {
		t.Error("inside first dash should hit")
	}
	if r, _ := ht.HitPoint(25, 50, 0); r.StrokeHit() {
		t.Error("inside first gap should miss")
	}
	if r, _ := ht.HitPoint(40, 50, 0); !r.StrokeHit() {
		t.Error("inside second dash should hit")
	}
	if r, _ := ht.HitPoint(55, 50, 0); r.StrokeHit() {
		t.Error("inside second gap should miss")
	}
	// A point in the gap but far off the line still misses.
	if r, _ := ht.HitPoint(25, 90, 0); r.StrokeHit() {
		t.Error("gap point off the line should miss")
	}

	// Dash offset shifts the pattern deterministically. With offset 25 the
	// pattern starts 25/30 of a cycle in: the first (partial) dash occupies
	// [5,25), followed by the gap [25,35).
	cfgOff := *cfg
	cfgOff.DashOffset = 25 * 64
	htOff, err := NewHitTester(p, Identity, &cfgOff, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := htOff.HitPoint(10, 50, 0); !r.StrokeHit() {
		t.Error("offset pattern: x=10 should lie on the partial first dash")
	}
	if r, _ := htOff.HitPoint(28, 50, 0); r.StrokeHit() {
		t.Error("offset pattern: x=28 should lie in a gap")
	}
}

// ---- geometry entirely in the negative quadrant (bounds sentinel check) ---

func TestHitNegativeQuadrant(t *testing.T) {
	// A box fully below and left of the origin exercises the bounds quick
	// reject, which must not mistake negative coordinates for "outside".
	p := rectPath(-200, -300, -100, -200)
	ht, err := NewHitTester(p, Identity, nil, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := ht.HitPoint(-150, -250, 0); !r.FillHit() {
		t.Error("center of a negative-quadrant box should hit")
	}
	if r, _ := ht.HitPoint(-50, -250, 0); r.FillHit() {
		t.Error("point to the right of a negative-quadrant box should miss")
	}
	if r, _ := ht.HitPoint(-150, -350, 0); r.FillHit() {
		t.Error("point above a negative-quadrant box should miss")
	}
	// The fixed-point convenience entry point agrees with the float one.
	rf, err := ht.HitPointF(ToFixedP(-150, -250), 0)
	if err != nil || !rf.FillHit() {
		t.Errorf("HitPointF: hit=%v err=%v, want a hit", rf.FillHit(), err)
	}

	// A stroked negative-quadrant path behaves the same.
	var line Path
	line.Start(ToFixedP(-180, -250))
	line.Line(ToFixedP(-120, -250))
	line.Stop(false)
	cfg := &StrokeConfig{Width: 4 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: Round}
	hts, err := NewHitTester(line, Identity, cfg, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := hts.HitPoint(-150, -250, 0); !r.StrokeHit() {
		t.Error("negative-quadrant stroke centerline should hit")
	}
	if r, _ := hts.HitPoint(-150, -260, 0); r.StrokeHit() {
		t.Error("negative-quadrant stroke off-line point should miss")
	}
}

// ---- pixel threshold makes thin strokes pickable --------------------------

func TestHitPixelThreshold(t *testing.T) {
	var p Path
	p.Start(ToFixedP(0, 50))
	p.Line(ToFixedP(100, 50))
	p.Stop(false)
	cfg := &StrokeConfig{Width: 1 * 64, MiterLimit: 4 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: MiterClip}
	ht, err := NewHitTester(p, Identity, cfg, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 2 px away from a 1 px wide stroke: miss without threshold.
	if r, _ := ht.HitPoint(50, 52, 0); r.StrokeHit() {
		t.Error("2 px away from a 1 px wide stroke should miss without threshold")
	}
	// Same point with a 3 px pick threshold: hit, coverage still binary.
	r, err := ht.HitPoint(50, 52, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !r.StrokeHit() || r.StrokeCoverage != 0 {
		t.Errorf("threshold expand: hit=%v coverage=%v, want hit with coverage 0",
			r.StrokeHit(), r.StrokeCoverage)
	}
	// Threshold does not pull in points well outside the pick radius.
	if r, _ := ht.HitPoint(50, 60, 3); r.StrokeHit() {
		t.Error("point beyond the pick radius should still miss")
	}
	if _, err := ht.HitPoint(0, 0, -1); err != ErrHitBadThreshold {
		t.Errorf("negative threshold: got %v want ErrHitBadThreshold", err)
	}
	if _, err := ht.HitPoint(0, 0, math.NaN()); err != ErrHitBadThreshold {
		t.Errorf("NaN threshold: got %v want ErrHitBadThreshold", err)
	}
}

// ---- transforms and clipping ----------------------------------------------

func TestHitTransform(t *testing.T) {
	p := rectPath(-20, -10, 20, 10)
	m := Identity.Translate(100, 100).Rotate(math.Pi / 2)
	ht, err := NewHitTester(p, m, nil, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Center maps to (100,100) and stays inside.
	if r, _ := ht.HitPoint(100, 100, 0); !r.FillHit() {
		t.Error("transformed center should hit")
	}
	// A corner of the original axis-aligned box cannot be where it was.
	if r, _ := ht.HitPoint(-20, -10, 0); r.FillHit() {
		t.Error("untransformed corner should miss after rotation")
	}
	// Point well outside the rotated bounding box.
	if r, _ := ht.HitPoint(200, 200, 0); r.FillHit() {
		t.Error("remote point should miss")
	}
}

func TestHitClip(t *testing.T) {
	p := rectPath(0, 0, 100, 100)
	ht, err := NewHitTester(p, Identity, nil, HitOptions{Clip: image.Rect(40, 40, 60, 60)})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := ht.HitPoint(50, 50, 0); !r.FillHit() {
		t.Error("point inside clip and shape should hit")
	}
	if r, _ := ht.HitPoint(10, 10, 0); r.FillHit() {
		t.Error("point outside clip must miss even though the shape covers it")
	}
	// Half-open clip boundary, matching image.Rectangle semantics.
	if r, _ := ht.HitPoint(60, 50, 0); r.FillHit() {
		t.Error("point on the exclusive clip edge must miss")
	}
}

// ---- rectangle query: coverage fractions ----------------------------------

func TestHitRectCoverage(t *testing.T) {
	p := rectPath(0, 0, 100, 100)
	ht, err := NewHitTester(p, Identity, nil, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Fully covered.
	r, err := ht.HitRect(image.Rect(10, 10, 20, 20), 0, HitOptions{SamplesX: 8, SamplesY: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !r.FillHit() || r.FillCoverage != 1 {
		t.Errorf("full cover: hit=%v cov=%v", r.FillHit(), r.FillCoverage)
	}
	// Half covered: left half in, right half out.
	r, _ = ht.HitRect(image.Rect(50, 0, 150, 100), 0,
		HitOptions{SamplesX: 20, SamplesY: 1, CoverageThreshold: 0.5})
	if math.Abs(r.FillCoverage-0.5) > 0.06 {
		t.Errorf("half cover: coverage = %v want ~0.5", r.FillCoverage)
	}
	if !r.FillHit() {
		t.Error("50%% coverage at threshold 0.5 should hit")
	}
	r, _ = ht.HitRect(image.Rect(50, 0, 150, 100), 0,
		HitOptions{SamplesX: 20, SamplesY: 1, CoverageThreshold: 0.9})
	if r.FillHit() {
		t.Error("50%% coverage at threshold 0.9 should miss")
	}
	// Empty rectangle is a quiet miss.
	if r, err := ht.HitRect(image.Rectangle{}, 0, HitOptions{}); err != nil || r.AnyHit() {
		t.Errorf("empty rect: result=%v err=%v", r, err)
	}
	// Bad sample counts.
	if _, err := ht.HitRect(image.Rect(0, 0, 5, 5), 0, HitOptions{SamplesX: 0, SamplesY: 129}); err != ErrHitBadSample {
		t.Errorf("bad samples: got %v want ErrHitBadSample", err)
	}
}

// ---- empty paths, non-finite input, singular matrix, over-limit width -----

func TestHitEdgeCases(t *testing.T) {
	// Empty path: never hits, no error.
	ht, err := NewHitTester(Path{}, Identity, nil, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := ht.HitPoint(0, 0, 0); r.AnyHit() {
		t.Error("empty path must never hit")
	}
	if n := ht.FillEdges(); n != 0 {
		t.Errorf("empty path edges = %d want 0", n)
	}

	// Non-finite transform entries.
	bad := Matrix2D{A: math.NaN(), D: 1}
	if _, err := NewHitTester(rectPath(0, 0, 1, 1), bad, nil, HitOptions{}); err != ErrHitNonFinite {
		t.Errorf("NaN matrix: got %v want ErrHitNonFinite", err)
	}
	bad = Matrix2D{A: math.Inf(1), D: 1}
	if _, err := NewHitTester(rectPath(0, 0, 1, 1), bad, nil, HitOptions{}); err != ErrHitNonFinite {
		t.Errorf("Inf matrix: got %v want ErrHitNonFinite", err)
	}
	// Singular (degenerate) matrix.
	if _, err := NewHitTester(rectPath(0, 0, 1, 1), Matrix2D{}, nil, HitOptions{}); err != ErrHitSingular {
		t.Errorf("singular matrix: got %v want ErrHitSingular", err)
	}
	// Finite but extreme scale pushes transformed points out of the
	// envelope: rejected deterministically instead of wrapping an Int26_6.
	overscale := Identity.Scale(1e7, 1e7)
	if _, err := NewHitTester(rectPath(0, 0, 10, 10), overscale, nil, HitOptions{}); err != ErrHitLimit {
		t.Errorf("overscale transform: got %v want ErrHitLimit", err)
	}
	// A legal large transform that keeps the path inside the envelope works.
	// The translated box spans x in [10000,10010], y in [-10000,-9990].
	bigOK := Identity.Translate(10000, -10000)
	htBig, err := NewHitTester(rectPath(0, 0, 10, 10), bigOK, nil, HitOptions{})
	if err != nil {
		t.Errorf("large in-envelope transform: %v", err)
	} else if r, _ := htBig.HitPoint(10005, -9995, 0); !r.FillHit() {
		t.Error("large in-envelope transform: translated center should hit")
	}

	// Non-finite and over-limit query points.
	if _, err := ht.HitPoint(math.NaN(), 0, 0); err != ErrHitNonFinite {
		t.Errorf("NaN point: got %v want ErrHitNonFinite", err)
	}
	if _, err := ht.HitPoint(1e9, 0, 0); err != ErrHitLimit {
		t.Errorf("over-limit point: got %v want ErrHitLimit", err)
	}
	if _, err := ht.HitPoint(0, 0, 1e8); err != ErrHitLimit {
		t.Errorf("over-limit threshold: got %v want ErrHitLimit", err)
	}
	if _, err := ht.HitRect(image.Rect(-1e7, 0, 0, 10), 0, HitOptions{}); err != ErrHitLimit {
		t.Errorf("over-limit rect: got %v want ErrHitLimit", err)
	}

	// Negative and over-limit stroke widths.
	p := rectPath(0, 0, 10, 10)
	if _, err := NewHitTester(p, Identity,
		&StrokeConfig{Width: -1, MiterLimit: 4 * 64}, HitOptions{}); err != ErrHitNonFinite {
		t.Errorf("negative width: got %v want ErrHitNonFinite", err)
	}
	if _, err := NewHitTester(p, Identity,
		&StrokeConfig{Width: MaxHitStrokeWidth + 64, MiterLimit: 4 * 64}, HitOptions{}); err != ErrHitLimit {
		t.Errorf("oversize width: got %v want ErrHitLimit", err)
	}
	// Miter extent over the envelope even though the width itself is legal.
	if _, err := NewHitTester(p, Identity,
		&StrokeConfig{Width: 200 * 64, MiterLimit: 10 * 64}, HitOptions{}); err != ErrHitLimit {
		t.Errorf("oversize miter extent: got %v want ErrHitLimit", err)
	}
	// Negative dash entry.
	if _, err := NewHitTester(p, Identity,
		&StrokeConfig{Width: 2 * 64, MiterLimit: 4 * 64,
			Dashes: []fixed.Int26_6{-1}}, HitOptions{}); err != ErrHitNonFinite {
		t.Errorf("negative dash: got %v want ErrHitNonFinite", err)
	}
}

// ---- queries never mutate the source path or any rasterizer state ---------

func TestHitNoMutation(t *testing.T) {
	p := GetTestPath()
	before := p.ToSVGPath()
	cfg := &StrokeConfig{Width: 6 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: ArcClip,
		Dashes: []fixed.Int26_6{33 * 64, 12 * 64}}
	ht, err := NewHitTester(p, Identity, cfg, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after := p.ToSVGPath(); after != before {
		t.Fatal("NewHitTester mutated the source path")
	}
	// Repeated queries are stable and never drift.
	r1, _ := ht.HitPoint(100, 100, 0)
	for i := 0; i < 10; i++ {
		r, err := ht.HitPoint(100, 100, 0)
		if err != nil || r != r1 {
			t.Fatalf("query %d drifted: %+v vs %+v err=%v", i, r, r1, err)
		}
	}
	// An unrelated production scanner keeps working after hit queries.
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	sc := NewScannerGV(512, 512, img, img.Bounds())
	f := NewFiller(512, 512, sc)
	p.AddTo(f)
	f.Draw()
	var alphaSum int
	for y := 0; y < 512; y += 4 {
		for x := 0; x < 512; x += 4 {
			alphaSum += int(img.RGBAAt(x, y).A)
		}
	}
	if alphaSum == 0 {
		t.Error("production filler no longer renders after hit queries")
	}
}

// A HitTester is read-only after construction, so point and rectangle
// queries may run concurrently without synchronisation.
func TestHitConcurrent(t *testing.T) {
	cfg := &StrokeConfig{Width: 6 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: ArcClip,
		Dashes: []fixed.Int26_6{18 * 64, 9 * 64}}
	ht, err := NewHitTester(GetTestPath(), Identity, cfg, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				x := float64((seed*7 + i*3) % 240)
				y := float64((seed*11 + i*5) % 240)
				if _, err := ht.HitPoint(x, y, 0.5); err != nil {
					t.Errorf("point query: %v", err)
					return
				}
				if _, err := ht.HitRect(image.Rect(int(x), int(y), int(x)+12, int(y)+12),
					0, HitOptions{SamplesX: 8, SamplesY: 8}); err != nil {
					t.Errorf("rect query: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// ---- independent render reference: no false positives / negatives ---------

// refPixelClass classifies pixel (x,y) using its 3x3 neighborhood. Only
// pixels whose whole neighborhood is fully opaque (interior, at least one
// pixel away from any antialiased edge) or fully transparent (exterior) are
// authoritative; boundary-band pixels return ok=false and are skipped.
func refPixelClass(img *image.RGBA, x, y int) (covered, ok bool) {
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			a := img.RGBAAt(x+dx, y+dy).A
			if a != 0 && a != 255 {
				return false, false // a boundary pixel: no sub-pixel authority
			}
		}
	}
	// Uniform neighborhood: the center alpha speaks for its whole pixel.
	switch img.RGBAAt(x, y).A {
	case 255:
		return true, true
	case 0:
		return false, true
	default:
		return false, false
	}
}

func crossCheck(t *testing.T, name string, p Path, m Matrix2D, stroke *StrokeConfig,
	w, h int, clip image.Rectangle) {
	t.Helper()
	// ScannerGV only supports the non-zero winding rule; even-odd behavior
	// is therefore validated geometrically in TestHitPointHole instead.
	ht, err := NewHitTester(p, m, stroke, HitOptions{Clip: clip})
	if err != nil {
		t.Fatalf("%s: NewHitTester: %v", name, err)
	}
	img := renderRef(p, m, stroke, w, h, 255, false, clip)

	// A pixel is compared only when its 3x3 neighborhood is uniformly solid
	// or uniformly transparent, which guarantees every sub-pixel point in it
	// is on the same side of every edge. This eliminates the rasterizer's
	// coverage rounding (a corner-clipped pixel still renders at alpha 255)
	// as a source of false disagreements while keeping tens of thousands of
	// authoritative interior and exterior samples.
	compared, band, mismatches := 0, 0, 0
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			covered, ok := refPixelClass(img, x, y)
			if !ok {
				band++
				continue
			}
			r, err := ht.HitPoint(float64(x)+0.5, float64(y)+0.5, 0)
			if err != nil {
				t.Fatalf("%s: HitPoint: %v", name, err)
			}
			got := r.FillHit()
			if stroke != nil {
				got = r.StrokeHit()
			}
			compared++
			if got != covered {
				mismatches++
				if mismatches <= 5 {
					t.Errorf("%s: (%d,%d) query=%v reference=%v", name, x, y, got, covered)
				}
			}
		}
	}
	t.Logf("%s: compared=%d boundary-band=%d mismatches=%d", name, compared, band, mismatches)
	if mismatches != 0 {
		t.Fail()
	}
}

func TestHitVsRenderReference(t *testing.T) {
	w, h := 260, 260

	// 1. Filled curved path (the Q shape), non-zero.
	crossCheck(t, "fill-Q", GetTestPath(), Identity, nil, w, h, image.ZR)

	// 2. Solid stroked curve with round caps and arc-clip joins.
	stroke := &StrokeConfig{Width: 10 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: ArcClip}
	crossCheck(t, "stroke-round-arcclip", GetTestPath(), Identity, stroke, w, h, image.ZR)

	// 4. Miter joins with butt caps and a flat gap.
	stroke = &StrokeConfig{Width: 8 * 64, MiterLimit: 4 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: Miter}
	crossCheck(t, "stroke-miter", GetTestPath(), Identity, stroke, w, h, image.ZR)

	// 5. Dashed stroke with square caps, miter-clip joins.
	stroke = &StrokeConfig{Width: 6 * 64, MiterLimit: 4 * 64,
		CapL: SquareCap, CapT: SquareCap, Gap: FlatGap, Join: MiterClip,
		Dashes: []fixed.Int26_6{20 * 64, 9 * 64}, DashOffset: 5 * 64}
	crossCheck(t, "dash-square-miterclip", GetTestPath(), Identity, stroke, w, h, image.ZR)

	// 6. Bevel join stroke.
	stroke = &StrokeConfig{Width: 9 * 64, MiterLimit: 2 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: Bevel}
	crossCheck(t, "stroke-bevel", GetTestPath(), Identity, stroke, w, h, image.ZR)

	// 7. Transformed (rotated + translated) fill.
	m := Identity.Translate(130, 130).Rotate(0.7).Translate(-105, -110)
	crossCheck(t, "fill-transformed", GetTestPath(), m, nil, w, h, image.ZR)

	// 8. Transformed dashed stroke.
	stroke = &StrokeConfig{Width: 5 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: Round,
		Dashes: []fixed.Int26_6{15 * 64, 8 * 64}}
	crossCheck(t, "dash-transformed", GetTestPath(), m, stroke, w, h, image.ZR)

	// 9. Sharp polyline so miter spikes and bevel fallbacks are exercised.
	var sharp Path
	sharp.Start(ToFixedP(20, 220))
	sharp.Line(ToFixedP(120, 120))
	sharp.Line(ToFixedP(220, 200))
	sharp.Line(ToFixedP(140, 40))
	sharp.Stop(false)
	stroke = &StrokeConfig{Width: 7 * 64, MiterLimit: 2 * 64,
		CapL: ButtCap, CapT: ButtCap, Gap: FlatGap, Join: Miter}
	crossCheck(t, "sharp-miter", sharp, Identity, stroke, w, h, image.ZR)
	stroke.Join = MiterClip
	crossCheck(t, "sharp-miterclip", sharp, Identity, stroke, w, h, image.ZR)

	// 10. Cubic beziers only (curves bound the flattening tolerance).
	var cubic Path
	cubic.Start(ToFixedP(30, 200))
	cubic.CubeBezier(ToFixedP(80, 20), ToFixedP(180, 240), ToFixedP(230, 60))
	cubic.CubeBezier(ToFixedP(60, 120), ToFixedP(200, 30), ToFixedP(40, 40))
	cubic.Stop(false)
	stroke = &StrokeConfig{Width: 5 * 64, MiterLimit: 4 * 64,
		CapL: CubicCap, CapT: QuadraticCap, Gap: CubicGap, Join: Arc}
	crossCheck(t, "cubic-arc", cubic, Identity, stroke, w, h, image.ZR)

	// 11. Clipped query agrees with the clipped render.
	clip := image.Rect(60, 60, 200, 200)
	crossCheck(t, "clipped-fill", GetTestPath(), Identity, nil, w, h, clip)
	stroke = &StrokeConfig{Width: 8 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: ArcClip,
		Dashes: []fixed.Int26_6{18 * 64, 7 * 64}}
	crossCheck(t, "clipped-dash", GetTestPath(), Identity, stroke, w, h, clip)

	// 12. Shape with a genuine hole rendered and queried both ways.
	var hole Path
	hole.Start(ToFixedP(20, 20))
	hole.Line(ToFixedP(240, 20))
	hole.Line(ToFixedP(240, 240))
	hole.Line(ToFixedP(20, 240))
	hole.Stop(true)
	hole.Start(ToFixedP(80, 80))
	hole.Line(ToFixedP(180, 80))
	hole.Line(ToFixedP(180, 180))
	hole.Line(ToFixedP(80, 180))
	hole.Stop(true)
	crossCheck(t, "hole-nonzero", hole, Identity, nil, w, h, image.ZR)
	// The even-odd rendering of this case cannot be produced by ScannerGV
	// (SetWinding is a no-op there); its hit semantics are covered by
	// TestHitPointHole.
}

// Semi-transparent brushes change only color, never geometry: a query against
// the same path must agree with a 26%-alpha reference render.
func TestHitSemiTransparent(t *testing.T) {
	p := GetTestPath()
	ht, err := NewHitTester(p, Identity, nil, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	img := renderRef(p, Identity, nil, 260, 260, 66, false, image.ZR) // ~26% alpha
	mismatch := 0
	for y := 2; y < 258; y += 3 {
		for x := 2; x < 258; x += 3 {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			a := img.RGBAAt(x, y).A
			if a == 0 || a == 66 { // only compare unambiguous pixels
				got, _ := ht.HitPoint(fx, fy, 0)
				if got.FillHit() != (a == 66) {
					mismatch++
				}
			}
		}
	}
	if mismatch != 0 {
		t.Errorf("semi-transparent reference mismatches: %d", mismatch)
	}
}

// Rectangle queries must agree with the pixel-averaged reference coverage.
func TestHitRectVsRenderReference(t *testing.T) {
	p := GetTestPath()
	cfg := &StrokeConfig{Width: 8 * 64, MiterLimit: 4 * 64,
		CapL: RoundCap, CapT: RoundCap, Gap: RoundGap, Join: ArcClip,
		Dashes: []fixed.Int26_6{22 * 64, 10 * 64}}
	ht, err := NewHitTester(p, Identity, cfg, HitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	img := renderRef(p, Identity, cfg, 260, 260, 255, false, image.ZR)
	rects := []image.Rectangle{
		image.Rect(20, 20, 60, 60),
		image.Rect(90, 90, 140, 130),
		image.Rect(0, 0, 259, 259),
		image.Rect(150, 30, 210, 90),
	}
	for _, rc := range rects {
		r, err := ht.HitRect(rc, 0, HitOptions{SamplesX: 24, SamplesY: 24})
		if err != nil {
			t.Fatal(err)
		}
		var sum, n int
		for y := rc.Min.Y; y < rc.Max.Y; y++ {
			for x := rc.Min.X; x < rc.Max.X; x++ {
				sum += int(img.RGBAAt(x, y).A)
				n++
			}
		}
		refCov := float64(sum) / float64(n) / 255
		// The exact supersampled coverage and the rasterized pixel-alpha
		// average differ only where antialiasing splits a pixel; a few
		// percent of tolerance covers that band.
		if math.Abs(r.StrokeCoverage-refCov) > 0.03 {
			t.Errorf("%v: stroke coverage %v vs reference %v", rc, r.StrokeCoverage, refCov)
		}
	}
}
