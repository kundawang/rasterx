// Copyright 2026 by the rasterx Authors. All rights reserved.
//
// Internal unit tests for the variable width stroker profile handling.
package rasterx

import (
	"testing"

	"golang.org/x/image/math/fixed"
)

func TestSetWidthsNormalization(t *testing.T) {
	r := new(VarStroker)
	r.SetWidths(
		WidthPoint{Pos: 1.5, Width: 30},   // pos clamped to 1
		WidthPoint{Pos: -0.5, Width: -4},  // pos clamped to 0, width clamped to 0
		WidthPoint{Pos: 0.5, Width: 20},   // kept
		WidthPoint{Pos: 0.25, Width: -10}, // width clamped to 0
	)
	want := []WidthPoint{
		{Pos: 0, Width: 0},
		{Pos: 0.25, Width: 0},
		{Pos: 0.5, Width: 20},
		{Pos: 1, Width: 30},
	}
	if len(r.widths) != len(want) {
		t.Fatalf("got %d control points, want %d", len(r.widths), len(want))
	}
	for i, w := range want {
		if r.widths[i] != w {
			t.Errorf("control point %d: got %+v, want %+v", i, r.widths[i], w)
		}
	}
}

func TestHalfWidthAt(t *testing.T) {
	const total = fixed.Int26_6(400 << 6)
	r := new(VarStroker)

	// No profile: constant width from SetStroke.
	r.SetStroke(10<<6, 4<<6, nil, nil, Miter)
	for _, s := range []fixed.Int26_6{0, total / 3, total} {
		if h := r.halfWidthAt(s, total); h != 5<<6 {
			t.Errorf("constant mode at %v: got half width %v, want %v", s, h, fixed.Int26_6(5<<6))
		}
	}

	// Linear profile 10 -> 30 over the whole path.
	r.SetWidths(WidthPoint{Pos: 0, Width: 10}, WidthPoint{Pos: 1, Width: 30})
	cases := []struct {
		pos  float64
		want float64 // full width
	}{
		{0, 10}, {0.25, 15}, {0.5, 20}, {0.75, 25}, {1, 30},
	}
	for _, c := range cases {
		s := fixed.Int26_6(c.pos * float64(total))
		got := float64(r.halfWidthAt(s, total)) / 32 // back to full width
		if d := got - c.want; d < -0.1 || d > 0.1 {
			t.Errorf("pos %v: got width %v, want %v", c.pos, got, c.want)
		}
	}

	// Intermediate control point and constant extrapolation outside the
	// first/last positions.
	r.SetWidths(WidthPoint{Pos: 0.5, Width: 40}, WidthPoint{Pos: 1, Width: 20})
	if h := r.halfWidthAt(0, total); h != 20<<6 {
		t.Errorf("before first control point: got %v, want %v", h, fixed.Int26_6(20<<6))
	}
	if h := r.halfWidthAt(total/2, total); h != 20<<6 {
		t.Errorf("at middle control point: got %v, want %v", h, fixed.Int26_6(20<<6))
	}

	// Zero total length must not divide by zero.
	if h := r.halfWidthAt(0, 0); h != 0 {
		t.Errorf("zero total length: got %v, want 0", h)
	}
}

func TestInsertWidthPoints(t *testing.T) {
	// A single 400 unit line with control points at 0.25, 0.5, 0.75 must
	// be split into four 100 unit segments.
	pts := []fixed.Point26_6{{X: 0, Y: 0}, {X: 400 << 6, Y: 0}}
	cum := []fixed.Int26_6{0, 400 << 6}
	ws := []WidthPoint{{Pos: 0, Width: 1}, {Pos: 0.25, Width: 2},
		{Pos: 0.5, Width: 3}, {Pos: 0.75, Width: 4}, {Pos: 1, Width: 5}}
	out, outCum := insertWidthPoints(pts, cum, ws, 400<<6)
	if len(out) != 5 {
		t.Fatalf("got %d points, want 5", len(out))
	}
	for i := 1; i < len(out); i++ {
		if d := outCum[i] - outCum[i-1]; d != 100<<6 {
			t.Errorf("segment %d: got length %v, want %v", i, d, fixed.Int26_6(100<<6))
		}
		if want := fixed.Int26_6(i * 100 << 6); out[i].X != want {
			t.Errorf("point %d: got x %v, want %v", i, out[i].X, want)
		}
	}
}
