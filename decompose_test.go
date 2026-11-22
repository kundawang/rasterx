// Tests for Matrix2D decomposition and key-frame interpolation.
//
// The "hand computed" cases below build matrices forward from known
// geometric parameters and assert the decomposition recovers those exact
// parameters, so both directions of the round trip are checked against
// independently derived numbers rather than against the implementation.
//
// Copyright 2025 by the rasterx Authors. All rights reserved.

package rasterx_test

import (
	"errors"
	"math"
	"sync"
	"testing"

	. "github.com/srwiley/rasterx"
)

const decompEpsTest = 1e-10

func approx(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %v, want %v (diff %g)", name, got, want, got-want)
	}
}

// TestDecomposeHandComputed checks decomposition parameters against
// matrices constructed and evaluated by hand.
func TestDecomposeHandComputed(t *testing.T) {
	// Case 1: identity.
	d, err := Identity.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "identity theta", d.Theta, 0, 0)
	approx(t, "identity reflect", d.Reflect, 1, 0)
	approx(t, "identity sx", d.ScaleX, 1, 0)
	approx(t, "identity sy", d.ScaleY, 1, 0)
	approx(t, "identity skew", d.SkewAngle, 0, 0)

	// Case 2: pure rotation of 30 degrees.
	// R = [c -s; s c], c=0.8660254037844386, s=0.49999999999999994.
	theta := math.Pi / 6
	rot := Identity.Rotate(theta)
	d, err = rot.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "rot theta", d.Theta, theta, decompEpsTest)
	approx(t, "rot sx", d.ScaleX, 1, decompEpsTest)
	approx(t, "rot sy", d.ScaleY, 1, decompEpsTest)
	approx(t, "rot skew", d.SkewAngle, 0, decompEpsTest)
	approx(t, "rot reflect", d.Reflect, 1, 0)

	// Case 3: pure reflection across the y axis: diag(-1, 1), no rotation.
	refl := Matrix2D{A: -1, B: 0, C: 0, D: 1}
	d, err = refl.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "refl theta", d.Theta, 0, decompEpsTest)
	approx(t, "refl reflect", d.Reflect, -1, 0)
	approx(t, "refl sx", d.ScaleX, 1, decompEpsTest)
	approx(t, "refl sy", d.ScaleY, 1, decompEpsTest)
	approx(t, "refl skew", d.SkewAngle, 0, decompEpsTest)

	// Case 4: pure SkewX(phi), tan(phi)=2, phi=atan(2).
	phi := math.Atan(2)
	skew := Identity.SkewX(phi)
	d, err = skew.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "skew theta", d.Theta, 0, decompEpsTest)
	approx(t, "skew sx", d.ScaleX, 1, decompEpsTest)
	approx(t, "skew sy", d.ScaleY, 1, decompEpsTest)
	approx(t, "skew angle", d.SkewAngle, phi, decompEpsTest)

	// Case 5: composite built forward from geometric primitives (the
	// primitives are independently covered by TestGeom), with expected
	// scalar values evaluated by hand and cross-checked in Python:
	// theta=pi/3, f=-1, sx=3, sy=2, tan(phi)=2.
	// L = R*diag(-3,2)*Skew gives
	// A=-1.5, B=-3sqrt(3)/2, C=-3-sqrt(3) ~ -4.7321, D=1-3sqrt(3).
	comp := Identity.Translate(7.25, -4.5).
		Rotate(math.Pi/3).
		Scale(-3, 2).
		SkewX(math.Atan(2))
	wantComp := Matrix2D{
		A: -1.5,
		B: -3 * math.Sqrt(3) / 2,
		C: -3 - math.Sqrt(3),
		D: 1 - 3*math.Sqrt(3),
		E: 7.25,
		F: -4.5,
	}
	if !isClose(comp, wantComp, 1e-12) {
		t.Fatalf("forward construction mismatch:\n got %v\nwant %v", comp, wantComp)
	}
	d, err = comp.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "comp tx", d.TX, 7.25, decompEpsTest)
	approx(t, "comp ty", d.TY, -4.5, decompEpsTest)
	approx(t, "comp theta", d.Theta, math.Pi/3, decompEpsTest)
	approx(t, "comp reflect", d.Reflect, -1, 0)
	approx(t, "comp sx", d.ScaleX, 3, decompEpsTest)
	approx(t, "comp sy", d.ScaleY, 2, decompEpsTest)
	approx(t, "comp skew", d.SkewAngle, math.Atan(2), decompEpsTest)
}

// TestReconstructRoundTrip applies Decompose followed by Matrix over a wide
// range of generated matrices, including reflected and skewed ones, and
// checks every coefficient is recovered.
func TestReconstructRoundTrip(t *testing.T) {
	angles := []float64{0, 0.3, -0.7, math.Pi / 2, -math.Pi / 2, 2.1, -2.9, math.Pi}
	skews := []float64{0, 0.4, -1.1, 1.3}
	scales := []struct{ x, y float64 }{
		{1, 1}, {2, 3}, {0.5, 4}, {3, 0.25}, {10, 10},
	}
	for _, th := range angles {
		for _, ph := range skews {
			for _, sc := range scales {
				for _, f := range []float64{1, -1} {
					m := Identity.Translate(3.5, -2.25).
						Rotate(th).
						Scale(f*sc.x, sc.y).
						SkewX(ph)
					d, err := m.Decompose()
					if err != nil {
						t.Fatalf("decompose %v: %v", m, err)
					}
					if !isClose(d.Matrix(), m, 1e-9) {
						t.Errorf("round trip failed for th=%g ph=%g sc=%v f=%g:\n got %v\nwant %v",
							th, ph, sc, f, d.Matrix(), m)
					}
				}
			}
		}
	}
}

// TestAngleAndSkewRanges verifies the documented canonical ranges.
func TestAngleAndSkewRanges(t *testing.T) {
	checks := []Matrix2D{
		Identity.Rotate(3 * math.Pi / 2),  // input angle 3pi/2 wraps to -pi/2
		Identity.Rotate(-3 * math.Pi / 2), // wraps to pi/2
		Identity.Rotate(5 * math.Pi),      // wraps to -pi (pi equivalent)
		Identity.SkewX(1.4),
		Identity.SkewX(-1.4),
	}
	for i, m := range checks {
		d, err := m.Decompose()
		if err != nil {
			t.Fatal(err)
		}
		if d.Theta <= -math.Pi || d.Theta > math.Pi {
			t.Errorf("case %d: theta %g outside (-pi, pi]", i, d.Theta)
		}
		if d.SkewAngle <= -math.Pi/2 || d.SkewAngle > math.Pi/2 {
			t.Errorf("case %d: skew %g outside (-pi/2, pi/2]", i, d.SkewAngle)
		}
		if d.ScaleX < 0 || d.ScaleY < 0 {
			t.Errorf("case %d: negative canonical scale %+v", i, d)
		}
		if d.Reflect != 1 && d.Reflect != -1 {
			t.Errorf("case %d: reflect %g", i, d.Reflect)
		}
	}
}

// TestSingularDecomposition covers zero and near-zero scales and rank one
// matrices; reconstruction must still be exact.
func TestSingularDecomposition(t *testing.T) {
	// Rank one: first column zero, second column nonzero (sy only).
	rank1 := Matrix2D{A: 0, B: 0, C: 3, D: 4, E: 1, F: 2} // sy=5
	d, err := rank1.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "rank1 sx", d.ScaleX, 0, 0)
	approx(t, "rank1 sy", d.ScaleY, 5, decompEpsTest)
	approx(t, "rank1 theta", d.Theta, math.Atan2(-3.0/5, 4.0/5), decompEpsTest)
	if !isClose(d.Matrix(), rank1, 1e-10) {
		t.Errorf("rank1 reconstruction %v != %v", d.Matrix(), rank1)
	}

	// Rank one via Scale(0, 5) combined with a rotation.
	m := Identity.Rotate(0.9).Scale(0, 5)
	d, err = m.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	if !isClose(d.Matrix(), m, 1e-10) {
		t.Errorf("zero-sx reconstruction %v != %v", d.Matrix(), m)
	}
	if d.ScaleX != 0 {
		t.Errorf("zero-sx: ScaleX = %g, want 0", d.ScaleX)
	}

	// Near-zero first column: must not blow up and must reconstruct.
	near := Matrix2D{A: 1e-14, B: -1e-14, C: 1, D: 2}
	d, err = near.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	if !isClose(d.Matrix(), near, 1e-9) {
		t.Errorf("near-zero reconstruction %v != %v", d.Matrix(), near)
	}

	// Zero matrix and pure translation.
	zero := Matrix2D{E: 6, F: -7}
	d, err = zero.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	if !isClose(d.Matrix(), zero, 0) {
		t.Errorf("zero reconstruction %v != %v", d.Matrix(), zero)
	}
	if d.ScaleX != 0 || d.ScaleY != 0 {
		t.Errorf("zero matrix scales = %g, %g", d.ScaleX, d.ScaleY)
	}
}

// TestInterpolationEndpoints requires bit-for-bit endpoint reproduction and
// exact decomposition recovery at every sampled key frame.
func TestInterpolationEndpoints(t *testing.T) {
	a := Identity.Translate(10, -4).Rotate(0.6).Scale(2, 3).SkewX(0.5)
	b := Identity.Translate(-5, 8).Rotate(-2.4).Scale(-1.5, 0.5).SkewX(-0.7)

	got, err := a.Interpolate(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != a {
		t.Errorf("t=0 endpoint mutated: %v != %v", got, a)
	}
	got, err = a.Interpolate(b, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != b {
		t.Errorf("t=1 endpoint mutated: %v != %v", got, b)
	}

	// Every interior frame must itself round trip through the decomposition.
	for i := 1; i < 10; i++ {
		tt := float64(i) / 10
		m, err := a.Interpolate(b, tt)
		if err != nil {
			t.Fatalf("t=%g: %v", tt, err)
		}
		d, err := m.Decompose()
		if err != nil {
			t.Fatalf("t=%g frame decompose: %v", tt, err)
		}
		if !isClose(d.Matrix(), m, 1e-9) {
			t.Errorf("t=%g frame does not round trip: %v != %v", tt, d.Matrix(), m)
		}
	}
}

// TestShortestRotationPath constructs frames 170 degrees apart across the
// +/-pi branch cut and verifies the midpoint travels the 20 degree path,
// not the 340 degree one.
func TestShortestRotationPath(t *testing.T) {
	from := math.Pi * 170 / 180 // +170 degrees
	to := -math.Pi * 170 / 180  // -170 degrees
	a := Identity.Rotate(from)
	b := Identity.Rotate(to)
	m, err := a.Interpolate(b, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	d, err := m.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	// Shortest midpoint is -180 degrees (equivalently pi); the long path
	// would pass through angle 0.
	if math.Abs(math.Abs(d.Theta)-math.Pi) > 1e-9 {
		t.Errorf("midpoint theta = %g, want +/-pi (short path)", d.Theta)
	}
	// The interpolated linear part must stay close to a pure rotation,
	// i.e. not collapse through a near-singular state.
	det := m.A*m.D - m.B*m.C
	if math.Abs(det-1) > 1e-9 {
		t.Errorf("short path det = %g, want 1", det)
	}

	// Reverse direction takes the symmetric 20 degree path as well and
	// exercises the other side of the angle wrapping logic.
	mRev, err := b.Interpolate(a, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	dRev, err := mRev.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(math.Abs(dRev.Theta)-math.Pi) > 1e-9 {
		t.Errorf("reverse midpoint theta = %g, want +/-pi (short path)", dRev.Theta)
	}
}

// TestNegativeScaleInterpolation checks the signed x scale passes through
// zero smoothly and the determinant changes sign instead of flipping.
func TestNegativeScaleInterpolation(t *testing.T) {
	a := Identity.Scale(2, 2)
	b := Identity.Scale(-2, 2)
	mid, err := a.Interpolate(b, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	// Signed x scale is zero at the midpoint; y scale must remain 2.
	if math.Abs(mid.A) > 1e-12 || math.Abs(mid.B) > 1e-12 {
		t.Errorf("midpoint first column = (%g, %g), want zero", mid.A, mid.B)
	}
	approx(t, "midpoint D", mid.D, 2, 1e-9)

	// The signed x scale must cross zero linearly (no sign flip): track it
	// through the decomposition at every sampled frame.
	prevSX := math.Inf(1) // monotonically decreasing from +2 to -2
	for i := 0; i <= 10; i++ {
		tt := float64(i) / 10
		m, err := a.Interpolate(b, tt)
		if err != nil {
			t.Fatal(err)
		}
		d, err := m.Decompose()
		if err != nil {
			t.Fatal(err)
		}
		signedSX := d.Reflect * d.ScaleX // want 2 -> 0 -> -2, linearly
		want := 2 - 4*tt
		if math.Abs(signedSX-want) > 1e-9 {
			t.Errorf("t=%g signed sx = %g, want %g", tt, signedSX, want)
		}
		if signedSX > prevSX+1e-12 {
			t.Errorf("signed sx not monotonic at t=%g: %g after %g", tt, signedSX, prevSX)
		}
		prevSX = signedSX
	}
}

// TestSingularInterpolation blends to and from singular endpoints.
func TestSingularInterpolation(t *testing.T) {
	normal := Identity.Scale(4, 9)
	singular := Identity.Scale(0, 0)
	for _, pair := range [][2]Matrix2D{{normal, singular}, {singular, normal}} {
		mid, err := pair[0].Interpolate(pair[1], 0.5)
		if err != nil {
			t.Fatal(err)
		}
		// Linear fallback on both axes: (4+0)/2 = 2, (9+0)/2 = 4.5.
		approx(t, "singular mid A", mid.A, 2, 1e-9)
		approx(t, "singular mid D", mid.D, 4.5, 1e-9)
	}
	// Rank-one endpoint must still be reproduced exactly.
	rank1 := Matrix2D{A: 0, B: 0, C: 3, D: 4}
	got, err := normal.Interpolate(rank1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != rank1 {
		t.Errorf("rank1 endpoint %v != %v", got, rank1)
	}
}

// TestInterpolationErrors checks non-finite and oversized inputs are
// rejected while the original matrices stay untouched.
func TestInterpolationErrors(t *testing.T) {
	good := Identity
	nan := math.NaN()
	inf := math.Inf(1)
	bad := []Matrix2D{
		{A: nan}, {B: inf}, {C: math.Inf(-1)}, {D: nan}, {E: inf}, {F: nan},
	}
	for i, bm := range bad {
		if _, err := bm.Decompose(); !errors.Is(err, ErrNonFiniteMatrix) {
			t.Errorf("case %d: decompose err = %v, want ErrNonFiniteMatrix", i, err)
		}
		if _, err := good.Interpolate(bm, 0.5); !errors.Is(err, ErrNonFiniteMatrix) {
			t.Errorf("case %d: interpolate err = %v, want ErrNonFiniteMatrix", i, err)
		}
	}
	huge := Matrix2D{A: 1e150}
	if _, err := huge.Decompose(); !errors.Is(err, ErrMatrixTooLarge) {
		t.Errorf("huge decompose err = %v, want ErrMatrixTooLarge", err)
	}
	if _, err := huge.Interpolate(good, 0.5); !errors.Is(err, ErrMatrixTooLarge) {
		t.Errorf("huge interpolate err = %v, want ErrMatrixTooLarge", err)
	}
	if _, err := good.Interpolate(good, nan); !errors.Is(err, ErrNonFiniteT) {
		t.Errorf("nan t err")
	}
	if _, err := good.Interpolate(good, inf); !errors.Is(err, ErrNonFiniteT) {
		t.Errorf("inf t err")
	}
	// The good matrix must be unchanged after the failed calls.
	if good != Identity {
		t.Errorf("receiver mutated: %v", good)
	}
}

// TestConcurrentReadOnly hammers decomposition and interpolation from many
// goroutines with the race detector in mind (go test -race).
func TestConcurrentReadOnly(t *testing.T) {
	a := Identity.Translate(1, 2).Rotate(1.1).Scale(-2, 3).SkewX(0.3)
	b := Identity.Translate(4, -1).Rotate(-2.0).Scale(3, 0.5).SkewX(-0.4)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				tt := float64((seed+k)%11) / 10
				if _, err := a.Decompose(); err != nil {
					t.Error(err)
					return
				}
				m, err := a.Interpolate(b, tt)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := m.Decompose(); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestInterpolateInSpace verifies blending matrices given in different local
// coordinate frames, and the singular-space error path.
func TestInterpolateInSpace(t *testing.T) {
	space := Identity.Translate(100, 200).Rotate(0.8)
	// Two key frames expressed in the same local frame.
	a := Identity.Translate(10, 0).Scale(2, 2)
	b := Identity.Translate(0, 10).Scale(3, 3)
	got, err := InterpolateInSpace(space, a, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !isClose(got, a, 1e-9) {
		t.Errorf("space t=0 %v != %v", got, a)
	}
	got, err = InterpolateInSpace(space, a, b, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !isClose(got, b, 1e-9) {
		t.Errorf("space t=1 %v != %v", got, b)
	}
	// Midpoint world position should equal the world-space midpoint.
	worldMid, err := space.Mult(a).Interpolate(space.Mult(b), 0.5)
	if err != nil {
		t.Fatal(err)
	}
	got, err = InterpolateInSpace(space, a, b, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if !isClose(space.Mult(got), worldMid, 1e-9) {
		t.Errorf("space midpoint %v != world midpoint %v", space.Mult(got), worldMid)
	}

	// Singular change of basis.
	if _, err := InterpolateInSpace(Matrix2D{}, a, b, 0.5); !errors.Is(err, ErrSingularSpace) {
		t.Errorf("singular space err = %v, want ErrSingularSpace", err)
	}
	// Non-finite space.
	if _, err := InterpolateInSpace(Matrix2D{A: math.NaN()}, a, b, 0.5); !errors.Is(err, ErrNonFiniteMatrix) {
		t.Errorf("non-finite space err = %v, want ErrNonFiniteMatrix", err)
	}
	// Non-finite interpolation parameter propagates from the inner blend.
	if _, err := InterpolateInSpace(space, a, b, math.NaN()); !errors.Is(err, ErrNonFiniteT) {
		t.Errorf("non-finite t in space err = %v, want ErrNonFiniteT", err)
	}
}

// TestExtrapolation verifies t outside [0,1] extends the path predictably.
func TestExtrapolation(t *testing.T) {
	a := Identity.Translate(0, 0).Scale(1, 1)
	b := Identity.Translate(10, 20).Scale(4, 9)
	m, err := a.Interpolate(b, 2)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "extrap tx", m.E, 20, 1e-9)
	approx(t, "extrap ty", m.F, 40, 1e-9)
	// Geometric scale: sx at t=2 is 4^2 = 16, sy = 9^2 = 81.
	approx(t, "extrap sx", m.A, 16, 1e-8)
	approx(t, "extrap sy", m.D, 81, 1e-8)
}

// TestAngleWrappingLargeInput exercises wrapAngle on angles several turns
// away from the canonical interval, covering both correction branches.
func TestAngleWrappingLargeInput(t *testing.T) {
	// Inputs several turns out, chosen so the recovered canonical angle
	// is unambiguous (avoiding the +/-pi equivalence of a half turn).
	cases := []struct{ in, want float64 }{
		{4*math.Pi + 0.5, 0.5},                      // fmod inside (-pi, pi]
		{2*math.Pi + math.Pi + 0.5, -math.Pi + 0.5}, // fmod > pi, shifted down
		{-2*math.Pi - math.Pi - 0.5, math.Pi - 0.5}, // fmod < -pi, shifted up
		{6*math.Pi - 0.75, -0.75},
		{-6*math.Pi + 0.75, 0.75},
	}
	for _, c := range cases {
		d, err := Identity.Rotate(c.in).Decompose()
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(d.Theta-c.want) > 1e-9 {
			t.Errorf("wrap %g: theta = %g, want %g", c.in, d.Theta, c.want)
		}
	}
}

// TestNearSingularGeneralCase covers the guard that clamps a tiny negative
// q11 (sign noise around a zero determinant) while the first column stays
// well conditioned.
func TestNearSingularGeneralCase(t *testing.T) {
	// First column unit length; second column almost parallel so q11 ~ 0.
	m := Matrix2D{A: 1, B: 0, C: 1, D: 1e-13}
	d, err := m.Decompose()
	if err != nil {
		t.Fatal(err)
	}
	if d.ScaleY < 0 {
		t.Errorf("near-singular ScaleY = %g, want clamped to >= 0", d.ScaleY)
	}
	if !isClose(d.Matrix(), m, 1e-9) {
		t.Errorf("near-singular reconstruction %v != %v", d.Matrix(), m)
	}
}
