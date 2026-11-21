// Copyright 2018 The oksvg Authors. All rights reserved.
//
// created: 2018 by S.R.Wiley
//_
// Implements SVG style matrix transformations.
// https://developer.mozilla.org/en-US/docs/Web/SVG/Attribute/transform
package rasterx

import (
	"errors"
	"math"

	"golang.org/x/image/math/fixed"
)

const (
	// NearSingularEpsilon is the relative determinant threshold used to
	// classify a matrix as singular. A matrix is singular when the
	// determinant of its linear part, after each row is normalized to unit
	// length, is at most this value. That normalized determinant is the
	// sine of the angle between the rows, so the test is independent of any
	// overall (including anisotropic) scale: ordinary rotations, scalings,
	// shears and translations remain invertible, while a true collapse of
	// the transform to a line or point is detected.
	NearSingularEpsilon = 1e-12

	// minNormalFloat64 is the smallest positive normal float64 value.
	// Determinants below it are subnormal: the inverse cannot be computed
	// with full float64 precision from them.
	minNormalFloat64 = 2.2250738585072014e-308
)

var (
	// ErrNonFiniteMatrix is returned when a matrix has a NaN or infinite
	// coefficient. Such a matrix cannot describe a finite affine transform.
	ErrNonFiniteMatrix = errors.New("rasterx: matrix has non-finite coefficients")

	// ErrSingularMatrix is returned when a matrix cannot be inverted
	// reliably. This covers a zero or near-singular determinant, a
	// determinant that overflows or underflows float64, and an inverse
	// whose coefficients are not finite.
	ErrSingularMatrix = errors.New("rasterx: matrix is singular or numerically unstable")
)

// isFinite reports whether v is neither NaN nor an infinity.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

type Matrix2D struct {
	A, B, C, D, E, F float64
}

// matrix3 is a full 3x3 float64 matrix
// used for inverting
type matrix3 [9]float64

func otherPair(i int) (a, b int) {
	switch i {
	case 0:
		a, b = 1, 2
	case 1:
		a, b = 0, 2
	case 2:
		a, b = 0, 1
	}
	return
}

func (m *matrix3) coFact(i, j int) float64 {
	ai, bi := otherPair(i)
	aj, bj := otherPair(j)
	a, b, c, d := m[ai+aj*3], m[bi+bj*3], m[ai+bj*3], m[bi+aj*3]
	return a*b - c*d
}

func (m *matrix3) Invert() (*matrix3, error) {
	for _, v := range m {
		if !isFinite(v) {
			return nil, ErrNonFiniteMatrix
		}
	}

	// The determinant of an affine matrix3 ({A, C, E, B, D, F, 0, 0, 1})
	// depends only on the 2x2 linear part [[A C] [B D]], whose row vectors
	// are (A, C) and (B, D).
	a, c, e, b, d, f := m[0], m[1], m[2], m[3], m[4], m[5]

	// detN is the determinant of the linear part after each row is
	// normalized to unit length, i.e. the sine of the angle between the two
	// rows. It is zero exactly when the rows are collinear, and it measures
	// how ill-conditioned the matrix is independently of any overall scale.
	// A legitimately anisotropic scale (whose rows stay orthogonal) has
	// |detN| == 1 and is never rejected.
	row0 := math.Hypot(a, c)
	row1 := math.Hypot(b, d)
	if row0 == 0 || row1 == 0 {
		return nil, ErrSingularMatrix
	}
	detN := (a/row0)*(d/row1) - (b/row1)*(c/row0)
	if !isFinite(detN) || math.Abs(detN) <= NearSingularEpsilon {
		return nil, ErrSingularMatrix
	}

	// Compute the cofactors exactly as the original implementation did.
	var cofact matrix3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			sign := float64(1 - (i+j%2)%2*2) // "checkerboard of minuses" grid
			cofact[i+j*3] = m.coFact(i, j) * sign
		}
	}
	deteriminate := m[0]*cofact[0] + m[1]*cofact[1] + m[2]*cofact[2]

	// transpose cofact
	for i := 0; i < 2; i++ {
		for j := i + 1; j < 3; j++ {
			cofact[i+j*3], cofact[j+i*3] = cofact[j+i*3], cofact[i+j*3]
		}
	}

	// Common case: the determinant is a normal float64. Divide the cofactors
	// exactly as before, so well-formed matrices keep bit-identical results
	// and existing renders are unchanged.
	if adet := math.Abs(deteriminate); adet >= minNormalFloat64 && !math.IsInf(adet, 0) {
		for i := 0; i < 9; i++ {
			cofact[i] /= deteriminate
			if !isFinite(cofact[i]) {
				return nil, ErrSingularMatrix
			}
		}
		return &cofact, nil
	}

	// The determinant underflowed, overflowed, or is exactly zero but the
	// normalized determinant says the matrix is well conditioned: this is a
	// uniformly tiny or huge matrix, which is perfectly invertible. Compute
	// the inverse from the normalized linear part so no intermediate leaves
	// the float64 range.
	//
	// With N = diag(1/row0, 1/row1) * L we have N^-1 = adj(N)/detN, hence
	// L^-1 = N^-1 * diag(1/row0, 1/row1).
	var inv Matrix2D
	inv.A = (d / row1) / detN / row0
	inv.C = (-c / row0) / detN / row1
	inv.B = (-b / row1) / detN / row0
	inv.D = (a / row0) / detN / row1
	if !inv.IsFinite() {
		return nil, ErrSingularMatrix
	}

	// Translation: M^-1 maps p to L^-1 (p - t), so the inverse translation is
	// -L^-1 t. Computing it this way avoids the cofactor products E*C and
	// F*D, which can overflow when the translation is extreme.
	inv.E = -(inv.A*e + inv.C*f)
	inv.F = -(inv.B*e + inv.D*f)
	if !inv.IsFinite() {
		return nil, ErrSingularMatrix
	}

	// Lay the result out exactly as the legacy path did.
	cofact = matrix3{inv.A, inv.C, inv.E, inv.B, inv.D, inv.F, 0, 0, 1}
	return &cofact, nil
}

// Invert returns the inverse of the matrix.
//
// Invert reports failure rather than producing non-finite coefficients:
// it returns ErrNonFiniteMatrix if any coefficient is NaN or infinite, and
// ErrSingularMatrix if the linear part is singular or so close to singular
// that the inverse would not be numerically meaningful. For matrices that
// pass those checks the result is exact to normal float64 rounding, so
// existing well-formed renders are unchanged.
func (a Matrix2D) Invert() (Matrix2D, error) {
	n := &matrix3{a.A, a.C, a.E, a.B, a.D, a.F, 0, 0, 1}
	inv, err := n.Invert()
	if err != nil {
		return Matrix2D{}, err
	}
	return Matrix2D{A: inv[0], C: inv[1], E: inv[2], B: inv[3], D: inv[4], F: inv[5]}, nil
}

// InvertOrIdentity returns the inverse of the matrix, or Identity when the
// matrix cannot be inverted. It is convenient for callers that prefer a
// stable fallback over propagating an error; the second return value
// reports whether the inversion succeeded.
func (a Matrix2D) InvertOrIdentity() (Matrix2D, bool) {
	inv, err := a.Invert()
	if err != nil {
		return Identity, false
	}
	return inv, true
}

// IsFinite reports whether every coefficient of the matrix is a finite
// float64 (neither NaN nor an infinity).
func (a Matrix2D) IsFinite() bool {
	return isFinite(a.A) && isFinite(a.B) && isFinite(a.C) &&
		isFinite(a.D) && isFinite(a.E) && isFinite(a.F)
}

// Mult returns the product a*b.
func (a Matrix2D) Mult(b Matrix2D) Matrix2D {
	return Matrix2D{
		A: a.A*b.A + a.C*b.B,
		B: a.B*b.A + a.D*b.B,
		C: a.A*b.C + a.C*b.D,
		D: a.B*b.C + a.D*b.D,
		E: a.A*b.E + a.C*b.F + a.E,
		F: a.B*b.E + a.D*b.F + a.F}
}

var Identity = Matrix2D{1, 0, 0, 1, 0, 0}

// TFixed transforms a fixed.Point26_6 by the matrix
func (m Matrix2D) TFixed(a fixed.Point26_6) (b fixed.Point26_6) {
	b.X = fixed.Int26_6((float64(a.X)*m.A + float64(a.Y)*m.C) + m.E*64)
	b.Y = fixed.Int26_6((float64(a.X)*m.B + float64(a.Y)*m.D) + m.F*64)
	return
}

// Transform multiples the input vector by matrix m and outputs the results vector
// components.
func (m Matrix2D) Transform(x1, y1 float64) (x2, y2 float64) {
	x2 = x1*m.A + y1*m.C + m.E
	y2 = x1*m.B + y1*m.D + m.F
	return
}

// TransformVector is a modidifed version of Transform that ignores the
// translation components.
func (m Matrix2D) TransformVector(x1, y1 float64) (x2, y2 float64) {
	x2 = x1*m.A + y1*m.C
	y2 = x1*m.B + y1*m.D
	return
}

func (a Matrix2D) Scale(x, y float64) Matrix2D {
	return a.Mult(Matrix2D{
		A: x,
		B: 0,
		C: 0,
		D: y,
		E: 0,
		F: 0})
}

func (a Matrix2D) SkewY(theta float64) Matrix2D {
	return a.Mult(Matrix2D{
		A: 1,
		B: math.Tan(theta),
		C: 0,
		D: 1,
		E: 0,
		F: 0})
}

func (a Matrix2D) SkewX(theta float64) Matrix2D {
	return a.Mult(Matrix2D{
		A: 1,
		B: 0,
		C: math.Tan(theta),
		D: 1,
		E: 0,
		F: 0})
}

func (a Matrix2D) Translate(x, y float64) Matrix2D {
	return a.Mult(Matrix2D{
		A: 1,
		B: 0,
		C: 0,
		D: 1,
		E: x,
		F: y})
}

func (a Matrix2D) Rotate(theta float64) Matrix2D {
	return a.Mult(Matrix2D{
		A: math.Cos(theta),
		B: math.Sin(theta),
		C: -math.Sin(theta),
		D: math.Cos(theta),
		E: 0,
		F: 0})
}

type MatrixAdder struct {
	Adder
	M Matrix2D
}

func (t *MatrixAdder) Reset() {
	t.M = Identity
}

func (t *MatrixAdder) Start(a fixed.Point26_6) {
	t.Adder.Start(t.M.TFixed(a))
}

// Line adds a linear segment to the current curve.
func (t *MatrixAdder) Line(b fixed.Point26_6) {
	t.Adder.Line(t.M.TFixed(b))
}

// QuadBezier adds a quadratic segment to the current curve.
func (t *MatrixAdder) QuadBezier(b, c fixed.Point26_6) {
	t.Adder.QuadBezier(t.M.TFixed(b), t.M.TFixed(c))
}

// CubeBezier adds a cubic segment to the current curve.
func (t *MatrixAdder) CubeBezier(b, c, d fixed.Point26_6) {
	t.Adder.CubeBezier(t.M.TFixed(b), t.M.TFixed(c), t.M.TFixed(d))
}
