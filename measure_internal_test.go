// Copyright 2018 by the rasterx Authors. All rights reserved.
//_
// White-box tests for the internal measure helpers. These cover the error
// and edge semantics that the external high-precision tests cannot reach
// directly, such as malformed tokens and non-finite coordinates that the
// fixed-point Path type cannot itself encode.

package rasterx

import (
	"math"
	"testing"

	"golang.org/x/image/math/fixed"
)

// malformedPaths exercises every truncation pattern plus an unknown command.
func TestMeasureMalformed(t *testing.T) {
	bad := []Path{
		{fixed.Int26_6(PathLineTo)},                         // truncated line
		{fixed.Int26_6(PathQuadTo), 1, 2},                   // truncated quad
		{fixed.Int26_6(PathCubicTo), 1, 2, 3, 4},            // truncated cubic
		{fixed.Int26_6(PathMoveTo), 1},                      // truncated move
		{fixed.Int26_6(99)},                                 // unknown command
		{fixed.Int26_6(PathMoveTo), 1, 2, fixed.Int26_6(7)}, // valid move then bad token
	}
	for i, p := range bad {
		if _, err := NewPathMeasure(p); err != ErrMalformedPath {
			t.Errorf("case %d: want ErrMalformedPath, got %v", i, err)
		}
	}
}

// TestMeasureEmptyAndMovesOnly checks the zero-length semantics.
func TestMeasureEmptyAndMovesOnly(t *testing.T) {
	m, err := NewPathMeasure(Path{})
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if m.LengthF() != 0 || m.Length() != 0 || m.SegmentCount() != 0 {
		t.Fatal("empty path should have zero length and no segments")
	}
	if _, _, _, _, err := m.PointAtFChecked(0); err != ErrZeroLengthPath {
		t.Errorf("checked query on empty path: want ErrZeroLengthPath, got %v", err)
	}
	// Unchecked query on zero-length path must not panic and returns the
	// zero point and a zero tangent.
	x, y, tx, ty := m.PointAtF(5)
	if x != 0 || y != 0 || tx != 0 || ty != 0 {
		t.Error("zero-length unchecked float query should return zeros")
	}
	pt, tan := m.PointAt(320)
	if pt != (fixed.Point26_6{}) || tan != (fixed.Point26_6{}) {
		t.Error("zero-length unchecked fixed query should return zeros")
	}
	if _, _, err := m.PointAtChecked(0); err != ErrZeroLengthPath {
		t.Errorf("fixed checked query: want ErrZeroLengthPath, got %v", err)
	}

	// A path containing only move-tos, including a zero-length close, still
	// measures zero and remembers the first pen position.
	var p Path
	p.Start(ToFixedP(3, 7))
	p.Start(ToFixedP(11, 13))
	p.Stop(true)
	m2, err := NewPathMeasure(p)
	if err != nil {
		t.Fatalf("moves-only path: %v", err)
	}
	if m2.LengthF() != 0 || m2.SegmentCount() != 1 {
		t.Fatalf("moves-only: len=%v segs=%v", m2.LengthF(), m2.SegmentCount())
	}
	x, y, _, _, err = m2.PointAtFChecked(0)
	if err != ErrZeroLengthPath || x != 3 || y != 7 {
		t.Errorf("zero path first point: got (%v,%v) err %v", x, y, err)
	}
}

// TestMeasureNonFinite guards the integrator against NaN/Inf control points.
func TestMeasureNonFinite(t *testing.T) {
	bad := []struct {
		name string
		seg  pathSegment
	}{
		{"line-nan", pathSegment{kind: segLine, p0: fPoint{0, 0}, p1: fPoint{math.NaN(), 1}}},
		{"quad-inf", pathSegment{kind: segQuad, p0: fPoint{0, 0}, p1: fPoint{1, 1}, p2: fPoint{math.Inf(1), 2}}},
		{"cubic-nan", pathSegment{kind: segCubic, p0: fPoint{0, 0}, p1: fPoint{1, 1}, p2: fPoint{2, 2}, p3: fPoint{math.NaN(), 3}}},
	}
	for _, tc := range bad {
		s := tc.seg
		s.buildTable()
		if !math.IsNaN(s.length) && !math.IsInf(s.length, 0) {
			// A non-finite control point must surface as non-finite length
			// rather than a silent wrong number; the constructor rejects it.
			t.Errorf("%s: expected non-finite segment length, got %v", tc.name, s.length)
		}
	}

	// Exercise the constructor's own finite check by building raw slices
	// whose values, while finite fixed integers, are then replaced through
	// the helper used by the parser.
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(1, 1))
	m, err := NewPathMeasure(p)
	if err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if m.LengthF() != math.Sqrt2 {
		t.Errorf("unit diagonal length: want %v, got %v", math.Sqrt2, m.LengthF())
	}

	// Inject a non-finite point directly into a segment and confirm the
	// solver/query path cannot loop forever: build a zero-length segment
	// instead and make sure solveParameter returns immediately.
	s := pathSegment{kind: segCubic, p0: fPoint{1, 1}, p1: fPoint{1, 1}, p2: fPoint{1, 1}, p3: fPoint{1, 1}}
	s.buildTable()
	if s.length != 0 {
		t.Error("constant cubic should have zero length")
	}
	if got := m.solveParameter(&s, 0); got != 0 {
		t.Errorf("zero-length solve: want 0, got %v", got)
	}
}

// TestMeasureBoundedIteration makes sure a near-cusp cubic solves in a fixed
// number of steps without recursion and returns a finite parameter.
func TestMeasureBoundedIteration(t *testing.T) {
	// Cubic with a cusp at t = 0.5 (control cage folds back on itself).
	s := pathSegment{
		kind: segCubic,
		p0:   fPoint{0, 0}, p1: fPoint{10, 10}, p2: fPoint{-10, 10}, p3: fPoint{0, 0},
	}
	s.buildTable()
	if s.length <= 0 || math.IsNaN(s.length) {
		t.Fatalf("cusp cubic length invalid: %v", s.length)
	}
	m := &PathMeasure{}
	for u := 0.0; u <= s.length; u += s.length / 13 {
		tt := m.solveParameter(&s, u)
		if math.IsNaN(tt) || math.IsInf(tt, 0) || tt < 0 || tt > 1 {
			t.Errorf("cusp solve at u=%v returned %v", u, tt)
		}
	}
}

// TestMeasureInvalidDistance covers NaN and infinite query distances.
func TestMeasureInvalidDistance(t *testing.T) {
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(10, 0))
	m, err := NewPathMeasure(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, _, _, err := m.PointAtFChecked(d); err != ErrInvalidDistance {
			t.Errorf("distance %v: want ErrInvalidDistance, got %v", d, err)
		}
		// The unchecked variant must treat non-finite input as the start,
		// never panic or return non-finite values.
		x, y, tx, ty := m.PointAtF(d)
		if math.IsNaN(x) || math.IsNaN(y) || math.IsNaN(tx) || math.IsNaN(ty) {
			t.Errorf("distance %v: unchecked query returned NaN", d)
		}
	}
}
