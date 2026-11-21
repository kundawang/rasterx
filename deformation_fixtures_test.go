// Copyright 2026 The rasterx Authors. All rights reserved.
//
// Fixture-driven deformation tests. Fixtures live in
// testdata/deformation_fixtures.json and are regenerated with:
//
//	go test -run '^TestDeformationFixturesMatchGenerator$' -update
//
// Each fixture stores only inputs (paths, matrix, stroke/dash parameters) and
// the digest of the independently computed coverage oracle. It never stores
// values produced by the code under test, so the expected result cannot drift
// along with a renderer bug.
package rasterx_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	. "github.com/srwiley/rasterx"
)

const deformFixturePath = "testdata/deformation_fixtures.json"

var deformUpdate = flag.Bool("update", false, "regenerate testdata/deformation_fixtures.json")

// deformCase is one fixture input. Coordinates are in the local coordinate
// system; Matrix is applied to them before filling/stroking/dashing, which is
// exactly what MatrixAdder does in the renderer.
type deformCase struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"` // fill | stroke | dash
	Width       int             `json:"width"`
	Height      int             `json:"height"`
	DestMin     [2]int          `json:"destMin"` // non-zero destination Bounds
	Paths       [][][2]float64  `json:"paths"`
	Matrix      [6]float64      `json:"matrix"`
	Closed      bool            `json:"closed"`
	StrokeWidth float64         `json:"strokeWidth,omitempty"`
	Join        string          `json:"join,omitempty"` // bevel | miter | round
	Cap         string          `json:"cap,omitempty"`  // "" (butt) | square | round
	Pattern     []float64       `json:"pattern,omitempty"`
	DashOffset  float64         `json:"dashOffset,omitempty"`
	Tolerance   deformTolerance `json:"tolerance"`

	// Analytic expectations. These are geometry facts independent of the
	// rasterizer, used when a closed form exists.
	ExpectedMass    float64 `json:"expectedMass,omitempty"`
	ExpectedMassTol float64 `json:"expectedMassTol,omitempty"` // absolute
	// ExpectedBBox is optional and holds [minX minY maxX maxY] relative to the
	// destination image origin.
	ExpectedBBox []int `json:"expectedBBox,omitempty"`

	// oracleDigest is written by the generator and checked by the test. It is
	// a hash of the independent coverage oracle, so a change in the oracle
	// implementation (rather than in rasterx) is visible as a fixture diff.
	OracleDigest string `json:"oracleDigest"`
}

type deformFixture struct {
	Version int          `json:"version"`
	Seed    int64        `json:"seed"`
	SS      int          `json:"ss"`
	Cases   []deformCase `json:"cases"`
}

const deformSeed int64 = 0x5EED_F00D

func deformMatrix(c deformCase) Matrix2D {
	return Matrix2D{c.Matrix[0], c.Matrix[1], c.Matrix[2], c.Matrix[3], c.Matrix[4], c.Matrix[5]}
}

func deformRefMatrix(c deformCase) refAffine {
	return refAffine{c.Matrix[0], c.Matrix[1], c.Matrix[2], c.Matrix[3], c.Matrix[4], c.Matrix[5]}
}

func deformPolyFromPairs(ps [][2]float64) refPoly {
	return refPolyFromPairs(ps)
}

func deformDevicePaths(c deformCase) []refPoly {
	m := deformRefMatrix(c)
	out := make([]refPoly, len(c.Paths))
	for i, p := range c.Paths {
		out[i] = m.transformPoly(deformPolyFromPairs(p))
	}
	return out
}

// deformRender renders the fixture through rasterx's public API. The matrix is
// applied by MatrixAdder, i.e. the rasterizer receives device-space points, so
// stroking and dashing happen in device space.
func deformRender(c deformCase) *image.RGBA {
	rect := image.Rect(c.DestMin[0], c.DestMin[1], c.DestMin[0]+c.Width, c.DestMin[1]+c.Height)
	img := image.NewRGBA(rect)
	sc := NewScannerGV(c.Width, c.Height, img, img.Bounds())
	sc.SetColor(color.White)
	m := deformMatrix(c)

	addPaths := func(a Adder) {
		ma := &MatrixAdder{Adder: a, M: m}
		for _, p := range c.Paths {
			ma.Start(ToFixedP(p[0][0], p[0][1]))
			for _, q := range p[1:] {
				ma.Line(ToFixedP(q[0], q[1]))
			}
			ma.Stop(c.Closed)
		}
	}

	switch c.Kind {
	case "fill":
		f := NewFiller(c.Width, c.Height, sc)
		addPaths(f)
		f.Draw()
	case "stroke":
		s := NewStroker(c.Width, c.Height, sc)
		s.SetStroke(ToFixedP(c.StrokeWidth, 0).X, 4*64, deformCap(c.Cap), nil,
			deformGap(c.Join), deformJoinMode(c.Join))
		addPaths(s)
		s.Draw()
	case "dash":
		d := NewDasher(c.Width, c.Height, sc)
		d.SetStroke(ToFixedP(c.StrokeWidth, 0).X, 4*64, deformCap(c.Cap), nil,
			deformGap(c.Join), deformJoinMode(c.Join), c.Pattern, c.DashOffset)
		addPaths(d)
		d.Draw()
	default:
		panic("deformRender: unknown kind " + c.Kind)
	}
	return img
}

func deformCap(name string) CapFunc {
	switch name {
	case "round":
		return RoundCap
	case "square":
		return SquareCap
	default:
		return ButtCap
	}
}

func deformJoinMode(name string) JoinMode {
	switch name {
	case "miter":
		return Miter
	case "round":
		return Round
	default:
		return Bevel
	}
}

func deformGap(name string) GapFunc {
	if name == "round" {
		return RoundGap
	}
	return FlatGap
}

// deformOracleFor builds the independent expected geometry for a case.
func deformOracleFor(c deformCase) *deformOracle {
	o := &deformOracle{W: c.Width, H: c.Height}
	dev := deformDevicePaths(c)
	switch c.Kind {
	case "fill":
		for _, p := range dev {
			o.addFill(p)
		}
	case "stroke":
		var outline []refPoly
		for _, p := range dev {
			outline = append(outline, deformStrokeOutline(p, c.StrokeWidth, c.Join, c.Cap, c.Closed)...)
		}
		for _, p := range outline {
			o.addUnion(p)
		}
	case "dash":
		for _, p := range dev {
			for _, seg := range deformDashRanges(p, c.Pattern, c.DashOffset) {
				for _, capsule := range deformDashCapsule(seg, c.StrokeWidth, c.Cap) {
					o.addUnion(capsule)
				}
			}
		}
	}
	return o
}

// ---------------------------------------------------------------------------
// Deterministic fixture generation
// ---------------------------------------------------------------------------

func deformFillTol() deformTolerance {
	return deformTolerance{EdgeBand: 1.5, EdgeTol: 0.55, FlatTol: 0.03, MinIoU: 0.99}
}

func deformStrokeTol() deformTolerance {
	return deformTolerance{EdgeBand: 1.75, EdgeTol: 0.55, FlatTol: 0.03, MinIoU: 0.98}
}

func deformDashTol() deformTolerance {
	return deformTolerance{EdgeBand: 1.75, EdgeTol: 0.55, FlatTol: 0.03, MinIoU: 0.98}
}

func deformCases() []deformCase {
	rnd := rand.New(rand.NewSource(deformSeed))

	// Seeded random polygon generator. All values are stored in the fixture;
	// the seed merely makes regeneration reproducible.
	randPoly := func(cx, cy, rMin, rMax float64, n int) [][2]float64 {
		p := make([][2]float64, n)
		for i := range p {
			a := 2 * math.Pi * float64(i) / float64(n)
			r := rMin + rnd.Float64()*(rMax-rMin)
			p[i] = [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)}
		}
		return p
	}
	randMatrix := func(cx, cy float64) [6]float64 {
		m := refIdentity().
			translate(-cx, -cy).
			rotate(rnd.Float64()*1.2-0.6).
			scale(0.7+rnd.Float64()*0.8, 0.7+rnd.Float64()*0.8).
			translate(cx, cy)
		return [6]float64{m.A, m.B, m.C, m.D, m.E, m.F}
	}

	pentagon := func() [][2]float64 {
		return [][2]float64{{14.3, 18.7}, {58.9, 12.1}, {61.7, 47.3}, {37.1, 62.9}, {15.9, 44.2}}
	}

	rotScale := refIdentity().
		translate(-38, -38).
		rotate(0.43).
		scale(1.25, 0.9).
		translate(38, 38)
	reflect := refIdentity().about(48, 48, refIdentity().rotate(0.2).scale(-1, 1))
	aniso := refIdentity().about(75, 65, refIdentity().rotate(0.35).scale(1.2, 0.8))

	toArr := func(m refAffine) [6]float64 { return [6]float64{m.A, m.B, m.C, m.D, m.E, m.F} }

	cases := []deformCase{
		{
			Name:      "fill_pentagon_rot_scale",
			Kind:      "fill",
			Width:     96,
			Height:    96,
			Paths:     [][][2]float64{pentagon()},
			Matrix:    toArr(rotScale),
			Closed:    true,
			Tolerance: deformFillTol(),
		},
		{
			Name:      "fill_pentagon_reflect",
			Kind:      "fill",
			Width:     96,
			Height:    96,
			Paths:     [][][2]float64{pentagon()},
			Matrix:    toArr(reflect),
			Closed:    true,
			Tolerance: deformFillTol(),
		},
		{
			Name:   "fill_ring_two_loops",
			Kind:   "fill",
			Width:  100,
			Height: 100,
			Paths: [][][2]float64{
				{{10, 10}, {90, 10}, {90, 90}, {10, 90}},
				{{30, 30}, {30, 70}, {70, 70}, {70, 30}}, // opposite orientation: hole
			},
			Matrix:    toArr(refIdentity().translate(-50, -50).rotate(0.25).scale(0.95, 0.85).translate(50, 50)),
			Closed:    true,
			Tolerance: deformFillTol(),
		},
		{
			Name:      "fill_random_hex_0",
			Kind:      "fill",
			Width:     96,
			Height:    96,
			Paths:     [][][2]float64{randPoly(48, 48, 12, 30, 6)},
			Matrix:    randMatrix(48, 48),
			Closed:    true,
			Tolerance: deformFillTol(),
		},
		{
			Name:        "stroke_open_bevel",
			Kind:        "stroke",
			Width:       120,
			Height:      100,
			Paths:       [][][2]float64{{{15, 55}, {40, 20}, {60, 55}, {85, 22}}},
			Matrix:      toArr(refIdentity()),
			Closed:      false,
			StrokeWidth: 9,
			Join:        "bevel",
			Cap:         "",
			Tolerance:   deformStrokeTol(),
		},
		{
			Name:        "stroke_open_miter_aniso",
			Kind:        "stroke",
			Width:       150,
			Height:      130,
			Paths:       [][][2]float64{{{15, 75}, {45, 30}, {75, 80}, {110, 35}}},
			Matrix:      toArr(aniso),
			Closed:      false,
			StrokeWidth: 10,
			Join:        "miter",
			Cap:         "",
			Tolerance:   deformStrokeTol(),
		},
		{
			Name:        "stroke_open_round",
			Kind:        "stroke",
			Width:       120,
			Height:      100,
			Paths:       [][][2]float64{{{15, 55}, {40, 20}, {60, 55}, {85, 22}}},
			Matrix:      toArr(refIdentity().translate(-50, -38).rotate(0.23).translate(55, 48)),
			Closed:      false,
			StrokeWidth: 9,
			Join:        "round",
			Cap:         "round",
			Tolerance:   deformStrokeTol(),
		},
		{
			Name:        "stroke_closed_miter",
			Kind:        "stroke",
			Width:       150,
			Height:      150,
			Paths:       [][][2]float64{{{30, 30}, {120, 35}, {115, 120}, {35, 115}}},
			Matrix:      toArr(refIdentity().translate(-75, -75).rotate(0.18).scale(1.1, 0.9).translate(75, 75)),
			Closed:      true,
			StrokeWidth: 9,
			Join:        "miter",
			Cap:         "",
			Tolerance:   deformStrokeTol(),
		},
		{
			Name:        "stroke_closed_round",
			Kind:        "stroke",
			Width:       150,
			Height:      150,
			Paths:       [][][2]float64{{{30, 30}, {120, 35}, {115, 120}, {35, 115}}},
			Matrix:      toArr(refIdentity()),
			Closed:      true,
			StrokeWidth: 9,
			Join:        "round",
			Cap:         "",
			Tolerance:   deformStrokeTol(),
		},
		{
			Name:        "dash_open_butt",
			Kind:        "dash",
			Width:       120,
			Height:      60,
			Paths:       [][][2]float64{{{10, 30}, {100, 30}}},
			Matrix:      toArr(refIdentity()),
			Closed:      false,
			StrokeWidth: 6,
			Join:        "bevel",
			Cap:         "",
			Pattern:     []float64{10, 6},
			DashOffset:  3,
			Tolerance:   deformDashTol(),
		},
		{
			Name:        "dash_open_round_polyline",
			Kind:        "dash",
			Width:       140,
			Height:      110,
			Paths:       [][][2]float64{{{15.3, 60.7}, {65.7, 30.3}, {110.3, 70.7}}},
			Matrix:      toArr(refIdentity().translate(-62, -50).rotate(0.21).translate(70, 58)),
			Closed:      false,
			StrokeWidth: 7,
			Join:        "round",
			Cap:         "round",
			Pattern:     []float64{9, 5},
			DashOffset:  2,
			Tolerance:   deformDashTol(),
		},
		{
			Name:        "dash_open_square_aniso",
			Kind:        "dash",
			Width:       150,
			Height:      130,
			Paths:       [][][2]float64{{{15, 100}, {120, 40}}},
			Matrix:      toArr(refIdentity().about(70, 65, refIdentity().scale(1.2, 0.8).rotate(0.4))),
			Closed:      false,
			StrokeWidth: 9,
			Join:        "bevel",
			Cap:         "square",
			Pattern:     []float64{11, 7},
			DashOffset:  3,
			Tolerance:   deformDashTol(),
		},
		{
			Name:            "fill_tiny_triangle",
			Kind:            "fill",
			Width:           20,
			Height:          20,
			Paths:           [][][2]float64{{{10.3, 10.7}, {10.675, 10.7}, {10.3, 11.075}}},
			Matrix:          [6]float64{1, 0, 0, 1, 0, 0},
			Closed:          true,
			Tolerance:       deformTolerance{EdgeBand: 1.5, EdgeTol: 0.6, FlatTol: 0.03, MinIoU: 0.5},
			ExpectedMass:    0.375 * 0.375 / 2,
			ExpectedMassTol: 0.02,
			ExpectedBBox:    []int{10, 10, 10, 10},
		},
		{
			Name:            "stroke_tiny_width",
			Kind:            "stroke",
			Width:           60,
			Height:          40,
			Paths:           [][][2]float64{{{20.3, 20.7}, {50.3, 20.7}}},
			Matrix:          [6]float64{1, 0, 0, 1, 0, 0},
			Closed:          false,
			StrokeWidth:     0.75,
			Join:            "bevel",
			Cap:             "",
			Tolerance:       deformTolerance{EdgeBand: 1.5, EdgeTol: 0.6, FlatTol: 0.06, MinIoU: 0.5},
			ExpectedMass:    0.75 * 30,
			ExpectedMassTol: 0.5,
		},
		{
			Name:   "fill_nonzero_bounds",
			Kind:   "fill",
			Width:  64,
			Height: 56,
			DestMin: [2]int{
				13, 7,
			},
			Paths:     [][][2]float64{pentagon()},
			Matrix:    toArr(refIdentity().translate(-6, -6)),
			Closed:    true,
			Tolerance: deformFillTol(),
		},
		{
			Name:            "fill_zero_area",
			Kind:            "fill",
			Width:           30,
			Height:          30,
			Paths:           [][][2]float64{{{10, 10}, {10, 10}, {10, 10}}},
			Matrix:          [6]float64{1, 0, 0, 1, 0, 0},
			Closed:          true,
			Tolerance:       deformTolerance{EdgeBand: 1.5, EdgeTol: 0.6, FlatTol: 0.03, MinIoU: 0.5},
			ExpectedMass:    0,
			ExpectedMassTol: 0.001,
		},
		{
			Name:        "stroke_zero_length_round",
			Kind:        "stroke",
			Width:       60,
			Height:      60,
			Paths:       [][][2]float64{{{30, 30}, {30, 30}}},
			Matrix:      [6]float64{1, 0, 0, 1, 0, 0},
			Closed:      false,
			StrokeWidth: 8,
			Join:        "round",
			Cap:         "round",
			Tolerance:   deformTolerance{EdgeBand: 1.5, EdgeTol: 0.6, FlatTol: 0.06, MinIoU: 0.5},
			// Half disc approximation of RoundCap loses a little area.
			ExpectedMass:    math.Pi * 4 * 4,
			ExpectedMassTol: 1.2,
		},
	}
	return cases
}

func deformFixtureFromCases() deformFixture {
	return deformFixture{Version: 1, Seed: deformSeed, SS: 8, Cases: deformCases()}
}

func deformFixtureBytes(f deformFixture) ([]byte, error) {
	for i := range f.Cases {
		o := deformOracleFor(f.Cases[i])
		f.Cases[i].OracleDigest = deformCoverageDigest(o.coverage(f.SS))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestDeformationFixturesMatchGenerator(t *testing.T) {
	want, err := deformFixtureBytes(deformFixtureFromCases())
	if err != nil {
		t.Fatal(err)
	}
	if *deformUpdate {
		if err := os.MkdirAll(filepath.Dir(deformFixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(deformFixturePath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes); rerun without -update to verify", deformFixturePath, len(want))
		return
	}
	got, err := os.ReadFile(deformFixturePath)
	if err != nil {
		t.Fatalf("read fixture (run go test -run '^TestDeformationFixturesMatchGenerator$' -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fixture %s is stale or was edited by hand; regenerate with:\n"+
			"  go test ./... -run '^TestDeformationFixturesMatchGenerator$' -update", deformFixturePath)
	}
}

func TestDeformationFixtures(t *testing.T) {
	fixture := loadDeformFixtures(t)
	for _, c := range fixture.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			oracle := deformOracleFor(c)
			ref := oracle.coverage(fixture.SS)
			// The fixture records a digest of the oracle this case was
			// generated with. A mismatch means the *reference* changed (or
			// the fixture was hand-edited), so it is reported separately from
			// a renderer mismatch.
			if c.OracleDigest != "" {
				if got := deformCoverageDigest(ref); got != c.OracleDigest {
					t.Errorf("%s: oracle digest is %s but the fixture records %s; regenerate the fixture or revert the oracle change",
						c.Name, got, c.OracleDigest)
				}
			}
			img := deformRender(c)
			m := deformCompare(oracle, ref, img, c.Tolerance)
			if m.Bad != 0 {
				t.Fatalf("%s\n%s", c.Name, m)
			}
			// Shapes with no ink at all have no meaningful intersection to
			// measure; their zero mass is checked by ExpectedMass below.
			if m.IoU < c.Tolerance.MinIoU && (m.MassRef > 0.05 || m.MassAct > 0.05) {
				t.Errorf("%s: IoU %.5f below minimum %.3f", c.Name, m.IoU, c.Tolerance.MinIoU)
			}
			if c.ExpectedMassTol > 0 || c.ExpectedMass != 0 {
				if math.Abs(m.MassAct-c.ExpectedMass) > c.ExpectedMassTol {
					t.Errorf("%s: rendered mass %.5f differs from analytic %.5f by more than %.5f",
						c.Name, m.MassAct, c.ExpectedMass, c.ExpectedMassTol)
				}
			}
			if len(c.ExpectedBBox) == 4 {
				minX, minY, maxX, maxY := deformBBox(img)
				if [4]int{minX, minY, maxX, maxY} != [4]int{c.ExpectedBBox[0], c.ExpectedBBox[1], c.ExpectedBBox[2], c.ExpectedBBox[3]} {
					t.Errorf("%s: alpha bbox = [%d %d %d %d], want %v", c.Name, minX, minY, maxX, maxY, c.ExpectedBBox)
				}
			}
			t.Logf("%s: %s", c.Name, m)
		})
	}
}

func loadDeformFixtures(t *testing.T) deformFixture {
	t.Helper()
	b, err := os.ReadFile(deformFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f deformFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	return f
}

func (c deformCase) String() string {
	return fmt.Sprintf("%s(%s %dx%d m=%v)", c.Name, c.Kind, c.Width, c.Height, c.Matrix)
}
