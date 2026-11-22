// Matrix decomposition and key-frame interpolation for Matrix2D.
//
// A Matrix2D is an SVG style affine transform using column vectors:
//
//	[x']   [A C E] [x]
//	[y'] = [B D F] [y]
//	[1 ]   [0 0 1] [1]
//
// Linearly interpolating the six raw coefficients between two key frames
// takes the wrong path whenever the rotational difference is close to pi
// (the matrix can spin the long way around, or shrink through a near
// singular state). The functions in this file decompose a matrix into
// canonical geometric components and interpolate those instead.
//
// Copyright 2025 by the rasterx Authors. All rights reserved.

package rasterx

import (
	"errors"
	"math"
)

// Tolerances governing the decomposition. They are scaled by the
// magnitudes of the matrix coefficients, see matrixTol.
const (
	// decompEps is the relative threshold below which a column of the
	// linear part is treated as having zero length.
	decompEps = 1e-12
	// maxCoeff bounds coefficients that can be processed without an
	// intermediate overflow (math.Hypot, math.Log, ...). Coefficients
	// larger than this are rejected.
	maxCoeff = 1e100
)

// Sentinel errors returned by the decomposition and interpolation API.
// Callers may test for these with errors.Is.
var (
	// ErrNonFiniteMatrix is returned when a matrix contains a NaN or
	// infinite coefficient.
	ErrNonFiniteMatrix = errors.New("rasterx: matrix contains non-finite coefficients")
	// ErrMatrixTooLarge is returned when a matrix coefficient exceeds
	// maxCoeff, so decomposing it could overflow.
	ErrMatrixTooLarge = errors.New("rasterx: matrix coefficients exceed the supported range")
	// ErrNonFiniteT is returned when an interpolation parameter t is NaN
	// or infinite.
	ErrNonFiniteT = errors.New("rasterx: interpolation parameter t must be finite")
	// ErrSingularSpace is returned when a change-of-basis matrix used by
	// InterpolateInSpace cannot be inverted.
	ErrSingularSpace = errors.New("rasterx: change-of-basis matrix is singular")
)

// Decomposition holds the canonical geometric components of a Matrix2D.
//
// Every Matrix2D, including singular (rank deficient) ones, can be written
// uniquely (under the conventions below) as
//
//	M = Translate(TX, TY) * R * F * Sx * (Skew * Sy),
//
// where the linear part factors, in this order, as
//
//	L = R * F * Sx * (Skew * Sy)
//	  = R(Theta) * diag(Reflect, 1) * diag(ScaleX, ScaleY) * Skew(SkewAngle)
//
// with
//
//	R(theta)    = [cos -sin],  Skew(phi) = [1 tan(phi)]
//	              [sin  cos ]               [0   1     ]
//
// and diag(Reflect, 1) * diag(ScaleX, ScaleY) = diag(f*sx, sy).
//
// Conventions and non-unique solution choices:
//   - Theta (rotation) is wrapped to the half-open interval (-pi, pi].
//   - ScaleY is always non-negative; a reflection is never represented by a
//     negative ScaleY.
//   - Reflect is -1 when det(L) < 0 and +1 otherwise. The signed x scale
//     f*ScaleX therefore carries the determinant sign and ScaleX itself is
//     non-negative.
//   - SkewAngle (skew along x, matching Matrix2D.SkewX) is wrapped to the
//     half-open interval (-pi/2, pi/2]; a matrix cannot be represented with
//     a skew angle outside this range, so the wrapping is unique.
//   - For a singular matrix whose first column vanishes, the rotation is
//     recovered from the second column and ScaleX is zero; when both
//     columns vanish the rotation and skew are defined as zero and ScaleY
//     as zero as well. These singular conventions still reconstruct the
//     original matrix exactly.
type Decomposition struct {
	TX, TY float64 // translation
	Theta  float64 // rotation angle in radians, (-pi, pi]
	// Reflect is +1 for a direct transform and -1 when the determinant of
	// the linear part is negative.
	Reflect float64
	ScaleX  float64 // non-negative x scale
	ScaleY  float64 // non-negative y scale
	// SkewAngle is the SkewX angle in radians, (-pi/2, pi/2].
	SkewAngle float64
}

// isFinite reports whether f is neither NaN nor infinite.
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// validateMatrix checks that all coefficients are finite and within the
// supported range. It does not modify m.
func validateMatrix(m Matrix2D) error {
	for _, v := range [6]float64{m.A, m.B, m.C, m.D, m.E, m.F} {
		if !isFinite(v) {
			return ErrNonFiniteMatrix
		}
		if math.Abs(v) > maxCoeff {
			return ErrMatrixTooLarge
		}
	}
	return nil
}

// matrixTol returns an absolute tolerance scaled by the largest coefficient
// of the supplied matrices, with a floor of decompEps.
func matrixTol(ms ...Matrix2D) float64 {
	maxAbs := 1.0
	for _, m := range ms {
		for _, v := range [6]float64{m.A, m.B, m.C, m.D, m.E, m.F} {
			if a := math.Abs(v); a > maxAbs {
				maxAbs = a
			}
		}
	}
	return decompEps * maxAbs
}

// wrapAngle wraps a to the half-open interval (-pi, pi].
func wrapAngle(a float64) float64 {
	a = math.Mod(a, 2*math.Pi)
	if a <= -math.Pi {
		a += 2 * math.Pi
	} else if a > math.Pi {
		a -= 2 * math.Pi
	}
	return a
}

// Decompose breaks the matrix down into translation, rotation, reflection,
// scale and skew components. The receiver is read only.
//
// Singular (zero or near-zero scale) matrices are supported: the
// decomposition degenerates in a well defined way and Decomposition.Matrix
// reconstructs the original matrix within floating point round off. An
// error is only returned for non-finite or excessively large coefficients,
// in which case the returned decomposition is the zero value.
func (m Matrix2D) Decompose() (Decomposition, error) {
	var d Decomposition
	if err := validateMatrix(m); err != nil {
		return d, err
	}
	tol := matrixTol(m)

	d.TX, d.TY = m.E, m.F
	d.Reflect = 1

	// First column r0 = (A, B) has length sx; second column r1 = (C, D).
	sx := math.Hypot(m.A, m.B)
	switch {
	case sx > tol:
		// General, non-degenerate case: QR style factorization
		// L = R * diag(f, 1) * diag(sx, sy) * Skew(phi).
		det := m.A*m.D - m.B*m.C
		if det < 0 {
			d.Reflect = -1
		}
		// First column of R, oriented so that Q's diagonal is non-negative.
		r00, r10 := d.Reflect*m.A/sx, d.Reflect*m.B/sx
		d.Theta = wrapAngle(math.Atan2(r10, r00))
		// Second column of R is r0 rotated 90 degrees counter-clockwise.
		// Q = R^T * L: its entries are dot products of R's columns with
		// L's second column (C, D).
		q01 := r00*m.C + r10*m.D
		q11 := -r10*m.C + r00*m.D
		if q11 < 0 && q11 > -tol { // guard against sign noise at det ~ 0
			q11 = 0
		}
		d.ScaleX = sx
		d.ScaleY = q11
		// Q = diag(f*sx, sy) * Skew(phi), so q01 = f*sx*tan(phi).
		// sx > tol in this branch, so the divisor is safe.
		if q01 != 0 {
			d.SkewAngle = math.Atan(q01 / (d.Reflect * sx))
		}
	case math.Hypot(m.C, m.D) > tol:
		// Singular first column: recover the frame from the second column.
		// R is the rotation whose y basis is (C, D)/sy, and the reflection
		// sign is chosen to keep ScaleY non-negative (folding any sign
		// flip into the signed x scale).
		sy := math.Hypot(m.C, m.D)
		r01, r11 := m.C/sy, m.D/sy
		// R's first column is its second column rotated -90 degrees.
		r00, r10 := r11, -r01
		d.Theta = wrapAngle(math.Atan2(r10, r00))
		d.ScaleX = 0
		d.ScaleY = sy
		d.SkewAngle = 0
	default:
		// Both columns vanish: a pure translation (or the zero matrix).
		d.Theta = 0
		d.ScaleX = 0
		d.ScaleY = 0
		d.SkewAngle = 0
	}
	return d, nil
}

// Matrix rebuilds a Matrix2D from its decomposition. It is the inverse of
// Decompose: for every finite matrix m,
//
//	d, _ := m.Decompose()
//	d.Matrix()
//
// equals m within floating point round off, including singular matrices.
func (d Decomposition) Matrix() Matrix2D {
	cos, sin := math.Cos(d.Theta), math.Sin(d.Theta)
	tan := math.Tan(d.SkewAngle)
	sx := d.Reflect * d.ScaleX // signed x scale
	sy := d.ScaleY

	// Linear part L = R * diag(sx, sy) * Skew(phi):
	//   [cos -sin] [sx sx*tan]   [cos*sx  cos*sx*tan - sin*sy]
	//   [sin  cos] [0  sy     ] = [sin*sx  sin*sx*tan + cos*sy]
	return Matrix2D{
		A: cos * sx,
		B: sin * sx,
		C: cos*sx*tan - sin*sy,
		D: sin*sx*tan + cos*sy,
		E: d.TX,
		F: d.TY,
	}
}

// shortestAngle unwraps b relative to a so the returned angle differs from a
// by at most pi, choosing -pi for an exact antipodal pair (both angles are
// geometrically identical there; -pi keeps the path deterministic).
func shortestAngle(a, b float64) float64 {
	delta := wrapAngle(b - a)
	return a + delta
}

// Interpolate returns the matrix at parameter t on the key-frame path from a
// to b. t is not clamped: t == 0 reproduces a and t == 1 reproduces b
// bit-for-bit (the original endpoints are returned directly), t in (0, 1)
// lies between them and values outside [0, 1] extrapolate.
//
// Both receivers are read only, and the method is safe for concurrent use
// by multiple goroutines.
//
// Interpolation rules:
//   - Translation and skew are linearly interpolated.
//   - Rotation takes the shortest angular path: the difference is wrapped
//     to (-pi, pi] before blending, so a 170 degree frame to a -170 degree
//     frame travels 20 degrees instead of 340.
//   - The reflection sign, carried by the signed x scale f*ScaleX, is
//     interpolated linearly, so a negative scale passes smoothly through
//     zero instead of flipping.
//   - Positive ScaleX and ScaleY use geometric (logarithmic) interpolation;
//     a scale at or near zero switches that axis to linear interpolation,
//     which makes zero/near-zero and fully singular endpoints well defined.
//
// ErrNonFiniteT, ErrNonFiniteMatrix or ErrMatrixTooLarge is returned for
// non-finite t or unsupported endpoints; the original matrices are left
// untouched.
func (a Matrix2D) Interpolate(b Matrix2D, t float64) (Matrix2D, error) {
	if !isFinite(t) {
		return Matrix2D{}, ErrNonFiniteT
	}
	if t == 0 {
		return a, validateMatrix(a)
	}
	if t == 1 {
		return b, validateMatrix(b)
	}
	da, err := a.Decompose()
	if err != nil {
		return Matrix2D{}, err
	}
	db, err := b.Decompose()
	if err != nil {
		return Matrix2D{}, err
	}
	tol := matrixTol(a, b)

	d := Decomposition{
		TX:        da.TX + (db.TX-da.TX)*t,
		TY:        da.TY + (db.TY-da.TY)*t,
		Reflect:   1,
		SkewAngle: da.SkewAngle + (db.SkewAngle-da.SkewAngle)*t,
	}
	// Shortest-path rotation.
	d.Theta = shortestAngle(da.Theta, db.Theta)
	d.Theta = da.Theta + (d.Theta-da.Theta)*t
	d.Theta = wrapAngle(d.Theta)

	// Signed x scale blends f*sx linearly so negative scales cross zero
	// smoothly; reflection is recovered from the blended sign.
	sx0, sx1 := da.Reflect*da.ScaleX, db.Reflect*db.ScaleX
	sx := sx0 + (sx1-sx0)*t
	if sx < 0 {
		d.Reflect, d.ScaleX = -1, -sx
	} else {
		d.Reflect, d.ScaleX = 1, sx
	}

	// Non-negative y scale: geometric between positive endpoints, linear
	// whenever an endpoint is at or near zero.
	switch {
	case da.ScaleY > tol && db.ScaleY > tol:
		d.ScaleY = math.Exp(math.Log(da.ScaleY) + (math.Log(db.ScaleY)-math.Log(da.ScaleY))*t)
	default:
		d.ScaleY = da.ScaleY + (db.ScaleY-da.ScaleY)*t
	}
	// Apply the same geometric rule to the non-negative ScaleX magnitude
	// when both endpoints stay on the same side of zero, i.e. there is no
	// sign flip to interpolate through.
	if da.Reflect == db.Reflect && da.ScaleX > tol && db.ScaleX > tol {
		d.ScaleX = math.Exp(math.Log(da.ScaleX) + (math.Log(db.ScaleX)-math.Log(da.ScaleX))*t)
	}
	return d.Matrix(), nil
}

// InterpolateInSpace interpolates two matrices expressed in different local
// coordinate systems. space maps each local frame into a common parent frame
// (i.e. the world matrix is space.Mult(local)); it must be invertible.
//
// The result is converted back to the local frame of a. For example, key
// frames stored relative to differently positioned group transforms can be
// blended without the translation being sheared by the differing frames:
//
//	world = space.Mult(local)  =>  local = space.Invert().Mult(world)
func InterpolateInSpace(space, a, b Matrix2D, t float64) (Matrix2D, error) {
	if err := validateMatrix(space); err != nil {
		return Matrix2D{}, err
	}
	det := space.A*space.D - space.B*space.C
	if math.Abs(det) <= matrixTol(space) {
		return Matrix2D{}, ErrSingularSpace
	}
	inv := space.Invert()
	wa := space.Mult(a)
	wb := space.Mult(b)
	m, err := wa.Interpolate(wb, t)
	if err != nil {
		return Matrix2D{}, err
	}
	return inv.Mult(m), nil
}
