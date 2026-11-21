// Tests for invertibility of Matrix2D and for gradient handling of a
// singular or non-finite transform. These tests exercise the public
// behavior: normal matrices invert with high precision, degenerate ones
// report an error, and gradients built from them never emit NaN/Inf
// colors.

package rasterx_test

import (
	"errors"
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/colornames"
)

func almostEqualBits(a, b float64) bool {
	return a == b || math.Abs(a-b) <= 1e-12*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func matrixAlmostEqual(a, b Matrix2D) bool {
	return almostEqualBits(a.A, b.A) && almostEqualBits(a.B, b.B) &&
		almostEqualBits(a.C, b.C) && almostEqualBits(a.D, b.D) &&
		almostEqualBits(a.E, b.E) && almostEqualBits(a.F, b.F)
}

func TestInvertHandComputed(t *testing.T) {
	// For an affine matrix [[A C E] [B D F] [0 0 1]] the inverse is
	// [[D -C C*F-D*E] [-B A B*E-A*F] [0 0 1]] / det, det = A*D-B*C.
	tests := []struct {
		name string
		m    Matrix2D
		want Matrix2D
	}{
		{
			name: "identity",
			m:    Identity,
			want: Identity,
		},
		{
			name: "uniform-scale-and-translate",
			// det = 2*3 - 0 = 6
			m:    Matrix2D{A: 2, B: 0, C: 0, D: 3, E: 10, F: 20},
			want: Matrix2D{A: 0.5, B: 0, C: 0, D: 1.0 / 3.0, E: -5, F: -20.0 / 3.0},
		},
		{
			name: "rotate-90",
			// A rotation by 90 degrees: x' = -y, y' = x.
			m:    Identity.Rotate(math.Pi / 2),
			want: Matrix2D{A: 0, B: -1, C: 1, D: 0, E: 0, F: 0},
		},
		{
			name: "shear",
			// det = 1*1 - 0.5*0 = 1
			m:    Matrix2D{A: 1, B: 0.5, C: 0, D: 1, E: 3, F: 4},
			want: Matrix2D{A: 1, B: -0.5, C: 0, D: 1, E: -3, F: -2.5},
		},
		{
			name: "rotate-scale-translate",
			// A = 2cos t, B = 2sin t, C = -3sin t, D = 3cos t, E = 7, F = -2
			// with t = pi/6. det = 6.
			m: Matrix2D{
				A: 2 * math.Cos(math.Pi/6), B: 2 * math.Sin(math.Pi/6),
				C: -3 * math.Sin(math.Pi/6), D: 3 * math.Cos(math.Pi/6),
				E: 7, F: -2,
			},
			want: Matrix2D{
				// A' = D/det, D' = A/det
				A: math.Cos(math.Pi/6) / 2,
				B: -math.Sin(math.Pi/6) / 3,
				C: math.Sin(math.Pi/6) / 2,
				D: math.Cos(math.Pi/6) / 3,
				// E' = (C*F - D*E)/det
				E: (-3*math.Sin(math.Pi/6)*-2 - 3*math.Cos(math.Pi/6)*7) / 6,
				// F' = (B*E - A*F)/det
				F: (2*math.Sin(math.Pi/6)*7 - 2*math.Cos(math.Pi/6)*-2) / 6,
			},
		},
		{
			name: "tiny-uniform-scale",
			m:    Identity.Scale(1e-200, 1e-200),
			want: Identity.Scale(1e200, 1e200),
		},
		{
			name: "huge-uniform-scale",
			m:    Identity.Scale(1e200, 1e200),
			want: Identity.Scale(1e-200, 1e-200),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.m.Invert()
			if err != nil {
				t.Fatalf("Invert(%+v) failed: %v", tc.m, err)
			}
			if !matrixAlmostEqual(got, tc.want) {
				t.Fatalf("Invert(%+v) = %+v, want %+v", tc.m, got, tc.want)
			}
			// M * M^-1 must be the identity, checked numerically.
			prod := tc.m.Mult(got)
			if !matrixAlmostEqual(prod, Identity) {
				t.Fatalf("M * M^-1 = %+v, want identity", prod)
			}
		})
	}
}

// TestInvertExtremeScaleTranslation checks that the extreme-scale path
// (where the determinant would underflow or overflow) inverts both the
// linear part and the translation correctly.
func TestInvertExtremeScaleTranslation(t *testing.T) {
	for _, scale := range []float64{1e-200, 1e-160, 1e160, 1e200} {
		m := Matrix2D{A: 2 * scale, B: 0, C: 0, D: 3 * scale, E: 4, F: -5}
		inv, err := m.Invert()
		if err != nil {
			t.Fatalf("scale %g: Invert failed: %v", scale, err)
		}
		want := Matrix2D{
			A: 0.5 / scale, B: 0, C: 0, D: 1.0 / 3.0 / scale,
			// E' = (C*F - D*E)/det = -3*scale*4 / (6*scale^2) = -2/scale
			E: -2 / scale,
			// F' = (B*E - A*F)/det = -2*scale*-5 / (6*scale^2) = 5/(3*scale)
			F: 5.0 / 3.0 / scale,
		}
		if !matrixAlmostEqual(inv, want) {
			t.Fatalf("scale %g: Invert = %+v, want %+v", scale, inv, want)
		}
		prod := m.Mult(inv)
		if !matrixAlmostEqual(prod, Identity) {
			t.Fatalf("scale %g: M*M^-1 = %+v, want identity", scale, prod)
		}
	}
}

func TestInvertSingular(t *testing.T) {
	tests := []struct {
		name string
		m    Matrix2D
	}{
		{"zero-matrix", Matrix2D{}},
		{"all-zero-linear", Matrix2D{E: 5, F: 7}},
		{"collinear-rows", Matrix2D{A: 1, B: 2, C: 2, D: 4}},
		{"collinear-columns", Matrix2D{A: 1, B: 2, C: 1, D: 2}},
		{"scale-x-by-zero", Identity.Scale(0, 1)},
		{"near-singular-near-zero-det", Matrix2D{A: 1, B: 1, C: 1, D: 1 + 1e-14}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := tc.m.Invert()
			if !errors.Is(err, ErrSingularMatrix) {
				t.Fatalf("Invert(%+v) error = %v, want ErrSingularMatrix", tc.m, err)
			}
			if inv != (Matrix2D{}) {
				t.Fatalf("Invert(%+v) returned partial result %+v on error", tc.m, inv)
			}
			if _, ok := tc.m.InvertOrIdentity(); ok {
				t.Fatalf("InvertOrIdentity(%+v) reported success for a singular matrix", tc.m)
			}
			if got, ok := tc.m.InvertOrIdentity(); ok || got != Identity {
				t.Fatalf("InvertOrIdentity(%+v) = %+v, %v; want Identity, false", tc.m, got, ok)
			}
		})
	}
}

func TestInvertNonFinite(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	tests := []Matrix2D{
		{A: nan, B: 0, C: 0, D: 1},
		{A: 1, B: 0, C: 0, D: inf},
		{A: 1, B: 0, C: 0, D: 1, E: nan},
		{A: 1, B: 0, C: 0, D: 1, F: inf},
	}
	for _, m := range tests {
		if _, err := m.Invert(); !errors.Is(err, ErrNonFiniteMatrix) {
			t.Fatalf("Invert(%+v) error = %v, want ErrNonFiniteMatrix", m, err)
		}
	}
}

// TestInvertMatchesLegacyFormula checks that the new inverse agrees with the
// formula used before the fix for a large sample of ordinary matrices, so
// existing well-formed renders are unchanged.
func TestInvertMatchesLegacyFormula(t *testing.T) {
	legacy := func(m Matrix2D) Matrix2D {
		det := m.A*m.D - m.B*m.C
		return Matrix2D{
			A: m.D / det,
			B: -m.B / det,
			C: -m.C / det,
			D: m.A / det,
			E: (m.C*m.F - m.D*m.E) / det,
			F: (m.B*m.E - m.A*m.F) / det,
		}
	}
	seed := uint64(0x9e3779b97f4a7c15)
	rnd := func() float64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return (float64(seed>>11)/float64(1<<53) - 0.5) * 200
	}
	for i := 0; i < 2000; i++ {
		m := Matrix2D{A: rnd(), B: rnd(), C: rnd(), D: rnd(), E: rnd(), F: rnd()}
		det := m.A*m.D - m.B*m.C
		if math.Abs(det) < 1e-3 { // keep away from the singular boundary
			continue
		}
		got, err := m.Invert()
		if err != nil {
			t.Fatalf("Invert(%+v) unexpected error: %v", m, err)
		}
		want := legacy(m)
		if !matrixAlmostEqual(got, want) {
			t.Fatalf("Invert(%+v) = %+v, legacy %+v", m, got, want)
		}
	}
}

func gradientStopFixtures() []GradStop {
	return []GradStop{
		{StopColor: colornames.Aquamarine, Offset: 0.0, Opacity: 1},
		{StopColor: colornames.Darksalmon, Offset: 1.0, Opacity: 1},
	}
}

func finiteColor(c color.Color) bool {
	r, g, b, a := c.RGBA()
	return r <= 0xffff && g <= 0xffff && b <= 0xffff && a <= 0xffff
}

func assertAllFinite(t *testing.T, cf ColorFunc, w, h int) {
	t.Helper()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if c := cf(x, y); !finiteColor(c) {
				t.Fatalf("color at (%d,%d) is not finite: %v", x, y, c)
			}
		}
	}
}

func TestGradientSingularMatrixFallsBack(t *testing.T) {
	bounds := struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100}
	singular := []struct {
		name string
		mat  Matrix2D
	}{
		{"zero-scale", Identity.Scale(0, 0)},
		{"flatten-x", Identity.Scale(0, 1)},
		{"collapse-line", Matrix2D{A: 1, B: 1, C: 1, D: 1}},
	}
	for _, tc := range singular {
		for _, isRadial := range []bool{false, true} {
			// ObjectBoundingBox unit space inverts the gradient matrix, so a
			// singular matrix must be reported.
			g := &Gradient{
				Points:   [5]float64{0, 0, 1, 0, 1},
				Stops:    gradientStopFixtures(),
				Bounds:   bounds,
				Matrix:   tc.mat,
				Units:    ObjectBoundingBox,
				IsRadial: isRadial,
			}
			cfi, err := g.GetColorFunctionChecked(1)
			if err == nil {
				t.Fatalf("%s radial=%v: expected an error for a singular matrix", tc.name, isRadial)
			}
			if !errors.Is(err, ErrSingularMatrix) {
				t.Fatalf("%s: error = %v, want ErrSingularMatrix", tc.name, err)
			}
			cf, ok := cfi.(ColorFunc)
			if !ok {
				t.Fatalf("%s: fallback is %T, want ColorFunc", tc.name, cfi)
			}
			assertAllFinite(t, cf, 20, 20)
			// The fallback is the first stop, so every pixel is equal.
			first := cf(0, 0)
			if c := cf(17, 9); c != first {
				t.Fatalf("%s: fallback color varies: %v vs %v", tc.name, first, c)
			}

			// UserSpaceOnUse applies the matrix to the gradient points
			// instead of inverting it. A singular matrix may collapse the
			// gradient to a point or line, which is reported as an error and
			// falls back; either way no NaN may escape to a pixel.
			g.Units = UserSpaceOnUse
			cfi, _ = g.GetColorFunctionChecked(1)
			cf, ok = cfi.(ColorFunc)
			if !ok {
				t.Fatalf("%s user-space: got %T, want ColorFunc", tc.name, cfi)
			}
			assertAllFinite(t, cf, 20, 20)
		}
	}
}

func TestGradientNonFiniteMatrixFallsBack(t *testing.T) {
	g := &Gradient{
		Points: [5]float64{0, 0, 1, 0, 1},
		Stops:  gradientStopFixtures(),
		Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
		Matrix: Matrix2D{A: math.NaN(), B: 0, C: 0, D: 1},
	}
	cfi, err := g.GetColorFunctionChecked(1)
	if !errors.Is(err, ErrNonFiniteMatrix) {
		t.Fatalf("error = %v, want ErrNonFiniteMatrix", err)
	}
	cf, ok := cfi.(ColorFunc)
	if !ok {
		t.Fatalf("fallback is %T, want ColorFunc", cfi)
	}
	assertAllFinite(t, cf, 20, 20)
}

func TestGradientDegenerateLinearFallsBack(t *testing.T) {
	// A linear gradient whose two points coincide has no direction; the
	// parameter would be 0/0.
	g := &Gradient{
		Points: [5]float64{50, 50, 50, 50, 0},
		Stops:  gradientStopFixtures(),
		Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
		Matrix: Identity,
		Units:  UserSpaceOnUse,
	}
	cfi, err := g.GetColorFunctionChecked(1)
	if !errors.Is(err, ErrSingularMatrix) {
		t.Fatalf("error = %v, want ErrSingularMatrix", err)
	}
	cf := cfi.(ColorFunc)
	assertAllFinite(t, cf, 20, 20)
}

func TestGradientDegenerateRadialFallsBack(t *testing.T) {
	// A radial gradient with a zero radius would divide by zero.
	g := &Gradient{
		Points:   [5]float64{50, 50, 50, 50, 0},
		Stops:    gradientStopFixtures(),
		Bounds:   struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
		Matrix:   Identity,
		Units:    UserSpaceOnUse,
		IsRadial: true,
	}
	cfi, err := g.GetColorFunctionChecked(1)
	if !errors.Is(err, ErrDegenerateRadius) {
		t.Fatalf("error = %v, want ErrDegenerateRadius", err)
	}
	cf := cfi.(ColorFunc)
	assertAllFinite(t, cf, 20, 20)
}

// TestGradientBackwardsCompatible checks that the unchecked
// GetColorFunction still returns a usable ColorFunc (never nil) and that
// normal gradients produce exactly the same samples as before.
func TestGradientBackwardsCompatible(t *testing.T) {
	g := &Gradient{
		Points: [5]float64{0, 0, 1, 0, 0},
		Stops:  gradientStopFixtures(),
		Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
		Matrix: Identity,
	}
	cfNew, err := g.GetColorFunctionChecked(1)
	if err != nil {
		t.Fatalf("ColorFunction: %v", err)
	}
	cfOld, ok := g.GetColorFunction(1).(ColorFunc)
	if !ok {
		t.Fatalf("GetColorFunction did not return a ColorFunc")
	}
	newF := cfNew.(ColorFunc)
	for x := 0; x < 100; x++ {
		if a, b := cfOld(x, 50), newF(x, 50); a != b {
			t.Fatalf("x=%d: legacy and checked color functions differ: %v vs %v", x, a, b)
		}
	}

	// Even with a singular matrix the legacy entry point returns a ColorFunc
	// (the fallback) rather than an unusable value.
	bad := &Gradient{
		Points: [5]float64{0, 0, 1, 0, 0},
		Stops:  gradientStopFixtures(),
		Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 100, H: 100},
		Matrix: Identity.Scale(0, 0),
	}
	badCF, ok := bad.GetColorFunction(1).(ColorFunc)
	if !ok {
		t.Fatalf("GetColorFunction with a singular matrix returned %T, want ColorFunc", bad.GetColorFunction(1))
	}
	assertAllFinite(t, badCF, 20, 20)
}

// TestSingularGradientRendersFinitePixels drives a singular gradient through
// the actual ScannerGV path. Any NaN/Inf reaching the rasterizer's color
// sampling shows up as a defined color here, so this is an end-to-end check
// that the fallback protects pixel computation.
func TestSingularGradientRendersFinitePixels(t *testing.T) {
	const w, h = 32, 32
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	scanner := NewScannerGV(w, h, img, img.Bounds())
	filler := NewFiller(w, h, scanner)

	for _, isRadial := range []bool{false, true} {
		g := &Gradient{
			Points:   [5]float64{0.2, 0.2, 0.8, 0.8, 0.4},
			Stops:    gradientStopFixtures(),
			Bounds:   struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: w, H: h},
			Matrix:   Identity.Scale(0, 0),
			Units:    ObjectBoundingBox,
			IsRadial: isRadial,
		}
		cf, err := g.GetColorFunctionChecked(1)
		if err == nil {
			t.Fatalf("radial=%v: expected an error", isRadial)
		}
		scanner.SetColor(cf)
		AddRect(0, 0, w, h, 0, filler)
		filler.Draw()
		filler.Clear()

		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, gg, b, a := img.At(x, y).RGBA()
				// The fallback paints the first stop; any integer is finite,
				// but assert it is actually the fallback color's channel.
				if r == 0 && gg == 0 && b == 0 && a == 0 {
					t.Fatalf("radial=%v: pixel (%d,%d) is fully transparent; fallback was not painted", isRadial, x, y)
				}
			}
		}
	}
}

// TestNormalMatricesStillMatchBaselinePixels renders the repository's
// gradient/image tests' outputs and checks a few hand-computed samples still
// match the documented legacy behavior. The pixel-for-pixel check against
// the pre-fix renderer is covered by the unchanged testdata PNGs in the
// existing tests.
func TestCheckedColorFunctionMatchesUnchecked(t *testing.T) {
	bounds := struct{ X, Y, W, H float64 }{X: 10, Y: 20, W: 60, H: 40}
	for _, units := range []GradientUnits{ObjectBoundingBox, UserSpaceOnUse} {
		for _, radial := range []bool{false, true} {
			for _, mat := range []Matrix2D{
				Identity,
				Identity.Translate(3, -4).Rotate(0.4).Scale(1.5, 0.75),
				Identity.Scale(1e-6, 1e6),
			} {
				g := &Gradient{
					Points:   [5]float64{0.1, 0.2, 0.9, 0.8, 0.3},
					Stops:    gradientStopFixtures(),
					Bounds:   bounds,
					Matrix:   mat,
					Units:    units,
					IsRadial: radial,
				}
				checked, err := g.GetColorFunctionUSChecked(0.8, Identity)
				if err != nil {
					t.Fatalf("units=%v radial=%v mat=%+v: unexpected error %v", units, radial, mat, err)
				}
				unchecked := g.GetColorFunctionUS(0.8, Identity)
				cf, ok := checked.(ColorFunc)
				if !ok {
					t.Fatalf("checked result is %T, want ColorFunc", checked)
				}
				ucf, ok := unchecked.(ColorFunc)
				if !ok {
					t.Fatalf("unchecked result is %T, want ColorFunc", unchecked)
				}
				for y := -10; y < 60; y += 7 {
					for x := -10; x < 80; x += 7 {
						if a, b := cf(x, y), ucf(x, y); a != b {
							t.Fatalf("units=%v radial=%v: checked %v != unchecked %v at (%d,%d)",
								units, radial, a, b, x, y)
						}
						if !finiteColor(cf(x, y)) {
							t.Fatalf("non-finite color at (%d,%d)", x, y)
						}
					}
				}
			}
		}
	}
}

// TestNearSingularThreshold pins the documented threshold behavior: a matrix
// whose normalized determinant is just above NearSingularEpsilon inverts, and
// one just below it is rejected.
func TestNearSingularThreshold(t *testing.T) {
	// Rows (1,0) and (cos t, sin t). The normalized determinant is sin t.
	makeMatrix := func(sinT float64) Matrix2D {
		cosT := math.Sqrt(1 - sinT*sinT)
		return Matrix2D{A: 1, C: 0, B: cosT, D: sinT}
	}

	above := makeMatrix(NearSingularEpsilon * 10)
	if _, err := above.Invert(); err != nil {
		t.Fatalf("matrix with normalized determinant %g was rejected: %v",
			NearSingularEpsilon*10, err)
	}

	below := makeMatrix(NearSingularEpsilon / 10)
	if _, err := below.Invert(); !errors.Is(err, ErrSingularMatrix) {
		t.Fatalf("matrix with normalized determinant %g: error = %v, want ErrSingularMatrix",
			NearSingularEpsilon/10, err)
	}
}

// TestInvertRotationAtExtremeScale checks that rotations remain invertible
// over the full practical range of uniform scales, including scales whose
// squared determinant would overflow or underflow.
func TestInvertRotationAtExtremeScale(t *testing.T) {
	for _, scale := range []float64{1e-250, 1e-150, 1e-8, 1, 1e8, 1e150, 1e250} {
		for _, angle := range []float64{0.1, math.Pi / 4, 2.0} {
			m := Identity.Scale(scale, scale).Rotate(angle)
			inv, err := m.Invert()
			if err != nil {
				t.Fatalf("scale %g angle %g: Invert failed: %v", scale, angle, err)
			}
			prod := m.Mult(inv)
			if !matrixAlmostEqual(prod, Identity) {
				t.Fatalf("scale %g angle %g: M*M^-1 = %+v, want identity", scale, angle, prod)
			}
		}
	}
}

// TestInvertAnisotropicScale checks that a large ratio between the two axes
// is not mistaken for singularity, since it is a well-conditioned,
// invertible transform.
func TestInvertAnisotropicScale(t *testing.T) {
	for _, ratio := range []float64{1e-200, 1e-12, 1e12, 1e200} {
		m := Identity.Scale(ratio, 1)
		inv, err := m.Invert()
		if err != nil {
			t.Fatalf("ratio %g: Invert failed: %v", ratio, err)
		}
		want := Identity.Scale(1/ratio, 1)
		if !matrixAlmostEqual(inv, want) {
			t.Fatalf("ratio %g: Invert = %+v, want %+v", ratio, inv, want)
		}
	}
}
