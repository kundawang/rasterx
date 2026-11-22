// Copyright 2018 by the rasterx Authors. All rights reserved.
package rasterx

import (
	"errors"
	"fmt"
	"math"

	"golang.org/x/image/math/fixed"
)

// Rasterx normally receives geometry from a trusted parser. When the input
// crosses a trust boundary (an SVG uploaded by a user, for example) the
// coordinates, control points, dash patterns and stroke parameters can be
// arbitrary. The limits in this file bound the work and allocation a single
// path can cause, and the accompanying checks turn malformed input into a
// reported error instead of a panic, an endless loop or a silently corrupt
// raster.
const (
	// MaxInputSegments is the maximum number of path commands accepted for a
	// single checked path.
	MaxInputSegments = 1 << 16

	// MaxFlattenedLines is the default maximum number of line segments a
	// single Bezier may flatten to. Callers may lower it; the ceiling below
	// is the largest value the guard honours.
	MaxFlattenedLines = 1 << 16

	// MaxFlattenedLinesCeiling is the hard upper bound for the per-curve
	// flattening limit.
	MaxFlattenedLinesCeiling = 1 << 20

	// maxOutputLines is the internal cap on the total flattened line count of
	// one checked path. It stops many individually-legal curves from
	// multiplying into unbounded work.
	maxOutputLines = 1 << 21

	// MaxDashEntries is the maximum number of entries accepted in a dash
	// pattern. Longer patterns are rejected and the stroke falls back to a
	// solid line.
	MaxDashEntries = 4096

	// MaxDashIterations bounds the number of dash transitions one Dasher may
	// perform between Clear calls. It stops sub-pixel dash patterns from
	// looping for an unbounded time on long segments.
	MaxDashIterations = 1 << 20

	// SafeCoordLimit is the largest coordinate magnitude (in 26.6 fixed
	// units) the guard accepts: 2^24 fixed units is 262144 pixels. Beyond
	// that, intermediate products of two coordinates stop fitting the fixed
	// point types the rasterizer uses, so such input is rejected rather than
	// silently wrapped.
	SafeCoordLimit = fixed.Int26_6(1 << 24)

	// rasterizeMaxPixels bounds the destination size a Scanner will allocate.
	// Larger images are almost certainly an attempt to force allocation.
	rasterizeMaxPixels = 1 << 27
)

var (
	// ErrNonFinite reports a coordinate that is NaN, infinite, or outside
	// SafeCoordLimit.
	ErrNonFinite = errors.New("rasterx: non-finite or out-of-range coordinate")
	// ErrBadPath reports a malformed path stream: unknown command, truncated
	// operands, or geometry before the first MoveTo.
	ErrBadPath = errors.New("rasterx: malformed path")
	// ErrTooManySegments reports input beyond MaxInputSegments.
	ErrTooManySegments = errors.New("rasterx: too many path segments")
	// ErrTooComplex reports a curve whose flattening would exceed the
	// per-curve limit.
	ErrTooComplex = errors.New("rasterx: curve exceeds flattening limit")
	// ErrTooManyOutputLines reports total flattened output beyond the
	// internal cap.
	ErrTooManyOutputLines = errors.New("rasterx: flattened path exceeds output limit")
	// ErrBadStrokeParams reports a stroke width, miter limit, dash array or
	// dash offset that is non-finite, non-positive where a positive value is
	// required, or too large.
	ErrBadStrokeParams = errors.New("rasterx: invalid stroke parameters")
	// ErrEmptyGradient reports a gradient without usable color stops.
	ErrEmptyGradient = errors.New("rasterx: gradient has no color stops")
	// ErrNilImage reports a scanner drawing with no destination image.
	ErrNilImage = errors.New("rasterx: scanner has no destination image")
	// ErrBadBounds reports scanner bounds the backing rasterizer cannot
	// address.
	ErrBadBounds = errors.New("rasterx: invalid rasterizer bounds")
	// ErrTooManyDashIterations reports a dash pattern that needed more
	// transitions than MaxDashIterations.
	ErrTooManyDashIterations = errors.New("rasterx: dash pattern exceeds iteration limit")
)

// IsSafeCoord reports whether v lies within SafeCoordLimit.
func IsSafeCoord(v fixed.Int26_6) bool {
	return v >= -SafeCoordLimit && v <= SafeCoordLimit
}

// IsSafePoint reports whether both coordinates of p are within SafeCoordLimit.
func IsSafePoint(p fixed.Point26_6) bool {
	return IsSafeCoord(p.X) && IsSafeCoord(p.Y)
}

// IsSafeFloatCoord reports whether v is finite and lies within SafeCoordLimit
// (v is in pixels, not 26.6 fixed units).
func IsSafeFloatCoord(v float64) bool {
	return isFinite(v) && v >= float64(-SafeCoordLimit)/64 && v <= float64(SafeCoordLimit)/64
}

// IsSafeMatrix reports whether every affine coefficient of m is finite. The
// resulting geometry is still bounded on the transformed points by
// IsSafePoint.
func IsSafeMatrix(m Matrix2D) bool {
	return isFinite(m.A) && isFinite(m.B) && isFinite(m.C) &&
		isFinite(m.D) && isFinite(m.E) && isFinite(m.F)
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// clampInt26 converts a float64 to Int26_6, saturating instead of letting a
// NaN or out-of-range value become an implementation-defined integer.
func clampInt26(v float64) fixed.Int26_6 {
	if math.IsNaN(v) {
		return 0
	}
	if v >= float64(math.MaxInt32) {
		return fixed.Int26_6(math.MaxInt32)
	}
	if v <= float64(math.MinInt32) {
		return fixed.Int26_6(math.MinInt32)
	}
	return fixed.Int26_6(v)
}

// clampSafeInt26 converts a float64 to Int26_6 saturating at SafeCoordLimit.
func clampSafeInt26(v float64) fixed.Int26_6 {
	if math.IsNaN(v) {
		return 0
	}
	if v >= float64(SafeCoordLimit) {
		return SafeCoordLimit
	}
	if v <= float64(-SafeCoordLimit) {
		return -SafeCoordLimit
	}
	return fixed.Int26_6(v)
}

// validLineLimit normalizes the public MaxFlattenedLines knob.
func validLineLimit() int {
	switch {
	case MaxFlattenedLines <= 0:
		return 1
	case MaxFlattenedLines > MaxFlattenedLinesCeiling:
		return MaxFlattenedLinesCeiling
	default:
		return MaxFlattenedLines
	}
}

// flatteningSegments is the single implementation of the flatness heuristic
// shared by QuadTo, CubeTo, the guard and the reference checker. It returns
// the segment count for a squared deviation and whether that count is within
// limit. A non-finite or negative deviation is never a valid curve and is
// reported as not ok.
func flatteningSegments(devsq float64, limit int) (n int, ok bool) {
	if !isFinite(devsq) || devsq < 0 {
		return 0, false
	}
	if devsq < 0.333 { // below QuadTo's subdivision threshold: one segment
		return 1, true
	}
	const tol = 3.0
	root := math.Sqrt(math.Sqrt(tol * devsq))
	if !isFinite(root) || root > float64(limit) {
		return limit, false
	}
	n = 1 + int(root)
	if n < 1 {
		n = 1
	}
	return n, true
}

// flatteningCount returns the segment count the flatteners must use. It is
// total: an unusable deviation yields the limit, so QuadTo/CubeTo always make
// progress and never loop unboundedly.
func flatteningCount(devsq float64) int {
	if math.IsNaN(devsq) || math.IsInf(devsq, 0) || devsq <= 0 {
		return 1
	}
	n, _ := flatteningSegments(devsq, validLineLimit())
	return n
}

// SafeStrokeWidth clamps a stroke width to the largest non-negative value that
// can be halved and doubled without overflowing Int26_6.
func SafeStrokeWidth(w fixed.Int26_6) fixed.Int26_6 {
	if w < 0 {
		return 0
	}
	if w > SafeCoordLimit/2 {
		return SafeCoordLimit / 2
	}
	return w
}

// FlatteningSegments returns how many line segments QuadTo/CubeTo would emit
// for the given devsq value (the squared deviation used by their flatness
// heuristic). The count is clamped to MaxFlattenedLinesCeiling; ok is false
// when the true count exceeds the ceiling or devsq is unusable.
func FlatteningSegments(devsq float64) (n int, ok bool) {
	return flatteningSegments(devsq, MaxFlattenedLinesCeiling)
}

// ValidRasterizerBounds reports whether a w*h rasterizer can be allocated
// without overflow or excessive memory.
func ValidRasterizerBounds(w, h int) bool {
	if w < 0 || h < 0 || w > math.MaxInt32 || h > math.MaxInt32 {
		return false
	}
	if w == 0 || h == 0 {
		return true
	}
	return w <= rasterizeMaxPixels/h
}

func boundsError(w, h int) error {
	if w > math.MaxInt32 || h > math.MaxInt32 {
		return fmt.Errorf("%w: %dx%d overflows int32", ErrBadBounds, w, h)
	}
	return fmt.Errorf("%w: %dx%d exceeds pixel budget", ErrBadBounds, w, h)
}

// ValidateStroke checks the stroke parameters accepted by SetStroke. A zero
// width is legal (it produces no stroke); negative or oversized values are
// not.
func ValidateStroke(width, miterLimit fixed.Int26_6) error {
	if width < 0 || width > SafeCoordLimit/2 {
		return fmt.Errorf("%w: width %d", ErrBadStrokeParams, width)
	}
	if miterLimit < 0 || miterLimit > SafeCoordLimit {
		return fmt.Errorf("%w: miter limit %d", ErrBadStrokeParams, miterLimit)
	}
	return nil
}

// ValidateDashes checks a dash array and offset in pixels. Entries must be
// finite and positive; the array may not exceed MaxDashEntries; the offset
// must be finite. An empty array is valid (solid stroke).
func ValidateDashes(dashes []float64, dashOffset float64) error {
	if !isFinite(dashOffset) || dashOffset > float64(SafeCoordLimit)/64 ||
		dashOffset < -float64(SafeCoordLimit)/64 {
		return fmt.Errorf("%w: dash offset %v", ErrBadStrokeParams, dashOffset)
	}
	if len(dashes) == 0 {
		return nil
	}
	if len(dashes) > MaxDashEntries {
		return fmt.Errorf("%w: %d dash entries (max %d)",
			ErrBadStrokeParams, len(dashes), MaxDashEntries)
	}
	for i, v := range dashes {
		if !isFinite(v) || v <= 0 || v > float64(SafeCoordLimit)/64 {
			return fmt.Errorf("%w: dash entry %d = %v", ErrBadStrokeParams, i, v)
		}
	}
	return nil
}

// SanitizeDashValue converts one dash entry to 26.6 fixed point, clamping to
// at least one fixed unit so a caller that ignores ValidateDashes cannot make
// the dashing loops spin on a zero-length entry.
func SanitizeDashValue(v float64) fixed.Int26_6 {
	if !isFinite(v) {
		return 1
	}
	if v > float64(SafeCoordLimit)/64 {
		v = float64(SafeCoordLimit) / 64
	}
	iv := fixed.Int26_6(v * 64)
	if iv < 1 {
		iv = 1
	}
	return iv
}
