// Texture pattern implementation for the rasterx package.
// A Pattern paints fills and strokes by sampling an image.Image through
// an affine transform, with tiling, mirroring or edge clamping, and
// optional bilinear filtering. Like Gradient, a Pattern only produces a
// color source for a Scanner; it never resizes or otherwise modifies the
// destination image.
// Copyright 2026 All rights reserved.

package rasterx

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	"math"
)

type (
	// TileMode determines how sample coordinates outside the source
	// image bounds are mapped back into the image.
	TileMode byte

	// FilterMode selects how texels are sampled at fractional
	// coordinates.
	FilterMode byte

	// Pattern is a texture paint source. It is constructed from an
	// image.Image with NewPattern and yields a ColorFunc (or a fallback
	// color.Color) through GetColorFunction and GetColorFunctionUS,
	// mirroring the Gradient API, so it can be passed to
	// Scanner.SetColor for both filling and stroking.
	Pattern struct {
		// Src is the source image. Its bounds need not start at (0,0);
		// the image is snapshotted at construction time.
		Src image.Image
		// Matrix maps pattern (texture) space into user space, like the
		// Matrix field of Gradient. Use Identity for a 1 texel = 1 user
		// unit mapping anchored at the origin.
		Matrix Matrix2D
		// TileX and TileY select the out-of-range behavior along each
		// axis independently.
		TileX, TileY TileMode
		// Filter selects nearest neighbor or bilinear sampling.
		Filter FilterMode

		// img is a premultiplied alpha snapshot of Src with a zero
		// origin, so sampling never reads out of bounds regardless of
		// the source image's bounds.
		img *image.RGBA
		err error
	}
)

// TileMode constants.
const (
	// TileRepeat tiles the source image periodically.
	TileRepeat TileMode = iota
	// TileReflect tiles the source image, mirroring every other copy.
	TileReflect
	// TileClamp extends the edge texels of the source image indefinitely.
	TileClamp
)

// FilterMode constants.
const (
	// NearestFilter samples the nearest texel without interpolation.
	NearestFilter FilterMode = iota
	// BilinearFilter interpolates the four nearest texels in
	// premultiplied alpha space, so transparent edges do not produce
	// color fringes.
	BilinearFilter
)

// maxPatternTexels bounds the pixel count of a pattern source so that
// absurdly large images fail with an error instead of exhausting memory.
const maxPatternTexels = 1 << 24

// Pattern construction errors.
var (
	ErrPatternNilSrc    = errors.New("rasterx: pattern source image is nil")
	ErrPatternEmptySrc  = errors.New("rasterx: pattern source image is empty")
	ErrPatternTooLarge  = errors.New("rasterx: pattern source image is too large")
	ErrPatternBadMatrix = errors.New("rasterx: pattern matrix is not invertible")
)

// patternFallback is the deterministic color produced by a Pattern that
// could not be constructed or whose combined transform is singular.
var patternFallback = color.RGBA{0, 0, 0, 0}

// isFinite reports whether all matrix entries are finite.
func (a Matrix2D) isFinite() bool {
	for _, v := range [6]float64{a.A, a.B, a.C, a.D, a.E, a.F} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// isSingular reports whether the matrix has no usable inverse.
func (a Matrix2D) isSingular() bool {
	if !a.isFinite() {
		return true
	}
	det := a.A*a.D - a.B*a.C
	return det == 0 || math.IsNaN(det) || math.IsInf(det, 0)
}

// NewPattern creates a texture pattern from src. The src image is copied
// into a premultiplied alpha snapshot, so later changes to src do not
// affect the pattern, and sources with a non-zero bounds origin are
// handled transparently. An error is returned for a nil or empty source,
// a source exceeding maxPatternTexels pixels, or a singular or
// non-finite matrix.
func NewPattern(src image.Image, m Matrix2D, tileX, tileY TileMode, filter FilterMode) (*Pattern, error) {
	p := &Pattern{Src: src, Matrix: m, TileX: tileX, TileY: tileY, Filter: filter}
	if src == nil {
		p.err = ErrPatternNilSrc
		return nil, p.err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		p.err = ErrPatternEmptySrc
		return nil, p.err
	}
	if w > maxPatternTexels || h > maxPatternTexels || w*h > maxPatternTexels {
		p.err = ErrPatternTooLarge
		return nil, p.err
	}
	if m.isSingular() {
		p.err = ErrPatternBadMatrix
		return nil, p.err
	}
	// Snapshot into a zero-origin premultiplied image. draw.Draw converts
	// any color model (including non-premultiplied NRGBA) correctly.
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), src, b.Min, draw.Src)
	p.img = img
	return p, nil
}

// Err returns the construction error of the pattern, if any.
func (p *Pattern) Err() error {
	return p.err
}

// wrapTexel maps an out-of-range texel index back into [0, n) according
// to the tile mode. n must be >= 1. Negative indices are handled by all
// modes, so no sample can read out of bounds.
func wrapTexel(i, n int, mode TileMode) int {
	switch mode {
	case TileReflect:
		period := n * 2
		i %= period
		if i < 0 {
			i += period
		}
		if i >= n {
			i = period - 1 - i
		}
		return i
	case TileClamp:
		if i < 0 {
			return 0
		}
		if i >= n {
			return n - 1
		}
		return i
	default: // TileRepeat
		i %= n
		if i < 0 {
			i += n
		}
		return i
	}
}

// texel returns the premultiplied texel at wrapped indices (ix, iy).
func (p *Pattern) texel(ix, iy int) color.RGBA {
	s := p.img.Rect.Dx()
	t := p.img.Rect.Dy()
	return p.img.RGBAAt(wrapTexel(ix, s, p.TileX), wrapTexel(iy, t, p.TileY))
}

// lerp8 interpolates between two premultiplied 8-bit channel values.
func lerp8(a, b uint8, t float64) float64 {
	return float64(a)*(1-t) + float64(b)*t
}

// bilinear blends four premultiplied texels. Blending in premultiplied
// space keeps transparent edges from bleeding their (invisible) color.
func bilinear(c00, c10, c01, c11 color.RGBA, tx, ty float64) color.RGBA {
	mix := func(a, b, c, d uint8) uint8 {
		v := lerp8(a, b, tx)*(1-ty) + lerp8(c, d, tx)*ty
		return uint8(v + 0.5)
	}
	return color.RGBA{
		mix(c00.R, c10.R, c01.R, c11.R),
		mix(c00.G, c10.G, c01.G, c11.G),
		mix(c00.B, c10.B, c01.B, c11.B),
		mix(c00.A, c10.A, c01.A, c11.A),
	}
}

// sample returns the premultiplied color at pattern space point (u, v).
// Non-finite coordinates yield a transparent color rather than a panic
// or an out-of-bounds read.
func (p *Pattern) sample(u, v float64) color.RGBA {
	if math.IsNaN(u) || math.IsNaN(v) || math.IsInf(u, 0) || math.IsInf(v, 0) {
		return patternFallback
	}
	if p.Filter == BilinearFilter {
		// Texel centers sit at integer + 0.5 in pattern space.
		fx, fy := u-0.5, v-0.5
		x0, y0 := math.Floor(fx), math.Floor(fy)
		ix, iy := int(x0), int(y0)
		return bilinear(
			p.texel(ix, iy), p.texel(ix+1, iy),
			p.texel(ix, iy+1), p.texel(ix+1, iy+1),
			fx-x0, fy-y0)
	}
	return p.texel(int(math.Floor(u)), int(math.Floor(v)))
}

// scalePremul multiplies a premultiplied color by opacity.
func scalePremul(c color.RGBA, opacity float64) color.RGBA {
	return color.RGBA{
		uint8(float64(c.R)*opacity + 0.5),
		uint8(float64(c.G)*opacity + 0.5),
		uint8(float64(c.B)*opacity + 0.5),
		uint8(float64(c.A)*opacity + 0.5),
	}
}

// GetColorFunction returns the color function for the pattern, using the
// identity user space matrix.
func (p *Pattern) GetColorFunction(opacity float64) interface{} {
	return p.GetColorFunctionUS(opacity, Identity)
}

// GetColorFunctionUS returns the color function for the pattern using
// the given user space objMatrix, mirroring Gradient.GetColorFunctionUS.
// The result is a ColorFunc suitable for Scanner.SetColor. If the
// pattern failed to construct, or the combined transform is singular,
// a deterministic transparent fallback color is returned instead.
func (p *Pattern) GetColorFunctionUS(opacity float64, objMatrix Matrix2D) interface{} {
	if p == nil || p.err != nil || p.img == nil {
		return patternFallback
	}
	// Device space = objMatrix * Matrix * pattern space.
	m := objMatrix.Mult(p.Matrix)
	if m.isSingular() {
		return patternFallback
	}
	inv := m.Invert()
	if !inv.isFinite() {
		return patternFallback
	}
	if opacity <= 0 {
		return patternFallback
	}
	if opacity >= 1 {
		return ColorFunc(func(xi, yi int) color.Color {
			u, v := inv.Transform(float64(xi)+0.5, float64(yi)+0.5)
			return p.sample(u, v)
		})
	}
	return ColorFunc(func(xi, yi int) color.Color {
		u, v := inv.Transform(float64(xi)+0.5, float64(yi)+0.5)
		return scalePremul(p.sample(u, v), opacity)
	})
}
