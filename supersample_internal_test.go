// Copyright 2024 by the rasterx Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rasterx

import (
	"image"
	"math"
	"testing"

	"golang.org/x/image/math/fixed"
)

// TestScaleFixedSaturation verifies the coordinate scaling used to map path
// geometry onto the high-resolution grid saturates at the fixed.Int26_6 range
// instead of overflowing on extreme coordinates.
func TestScaleFixedSaturation(t *testing.T) {
	cases := []struct {
		name string
		in   fixed.Int26_6
		q    int
		want fixed.Int26_6
	}{
		{"ordinary", 64, 4, 256}, // 1.0 * 4 = 4.0
		{"fraction", 32, 8, 256}, // 0.5 * 8 = 4.0
		// Inputs whose product exceeds the int32 fixed-point range must clamp.
		{"positive_saturates", fixed.Int26_6(math.MaxInt32 / 4), 8, fixed.Int26_6(math.MaxInt32)},
		{"negative_saturates", fixed.Int26_6(math.MinInt32 / 4), 8, fixed.Int26_6(math.MinInt32)},
		{"zero", 0, MaxQuality, 0},
	}
	for _, tc := range cases {
		if got := scaleFixed(tc.in, tc.q); got != tc.want {
			t.Errorf("%s: scaleFixed(%d,%d) = %d, want %d", tc.name, tc.in, tc.q, got, tc.want)
		}
	}
}

// TestLoMaskBoundsFallback covers the branch where no destination image is
// attached yet, so the low-resolution mask uses an origin-zero rectangle of
// the rasterizer size.
func TestLoMaskBoundsFallback(t *testing.T) {
	s := &ScannerSS{ScannerGV: NewScannerGV(24, 16, nil, image.Rect(0, 0, 24, 16))}
	got := s.loMaskBounds(image.Point{X: 24, Y: 16})
	want := image.Rect(0, 0, 24, 16)
	if got != want {
		t.Errorf("loMaskBounds with nil dest = %v, want %v", got, want)
	}
}

// TestLoMaskBoundsOrigin verifies the mask keeps rasterizer dimensions while
// adopting a non-zero destination origin.
func TestLoMaskBoundsOrigin(t *testing.T) {
	dest := image.NewRGBA(image.Rect(100, 100, 180, 190))
	s := &ScannerSS{ScannerGV: NewScannerGV(40, 50, dest, dest.Bounds())}
	got := s.loMaskBounds(image.Point{X: 40, Y: 50})
	want := image.Rect(100, 100, 140, 150)
	if got != want {
		t.Errorf("loMaskBounds = %v, want %v", got, want)
	}
}

// TestEffectiveQualityBudget exercises the resource-budget clamping loop at
// its boundaries, including the integer overflow-safe 64-bit products.
func TestEffectiveQualityBudget(t *testing.T) {
	s := &ScannerSS{quality: MaxQuality, ScannerGV: &ScannerGV{}}
	// A small canvas allows the full quality.
	if q := s.effectiveQualityFor(64, 64); q != MaxQuality {
		t.Errorf("64x64 effective quality = %d, want %d", q, MaxQuality)
	}
	// A canvas just over the budget at q=8 must step down at least once while
	// staying within the limit at the returned quality.
	const w, h = 2048, 2048
	q := s.effectiveQualityFor(w, h)
	if q >= MaxQuality {
		t.Errorf("expected clamping below MaxQuality, got %d", q)
	}
	if int64(q)*int64(q)*w*h > MaxHiResPixels {
		t.Errorf("returned quality %d exceeds the high-resolution pixel budget", q)
	}
	if q < MinQuality {
		t.Errorf("effective quality below minimum: %d", q)
	}
	// Zero dimensions always fall back to the minimum.
	if q := s.effectiveQualityFor(0, 100); q != MinQuality {
		t.Errorf("zero-width effective quality = %d, want %d", q, MinQuality)
	}
}
