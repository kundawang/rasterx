// Copyright 2024 by the rasterx Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rasterx

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Supersampling quality limits.
//
// Quality is the number of samples taken per pixel along each axis, so a
// quality of N uses N*N coverage samples per output pixel. The values are
// bounded so that a large canvas combined with an extreme quality setting can
// not exhaust memory.
const (
	// MinQuality is the lowest selectable sampling quality. At MinQuality the
	// scanner renders exactly like ScannerGV (the default scanner), preserving
	// the original output bit for bit.
	MinQuality = 1

	// MaxQuality is the highest selectable sampling quality along each axis.
	// MaxQuality of 8 means at most 64 coverage samples per output pixel.
	MaxQuality = 8

	// MaxHiResPixels bounds the number of pixels allocated for the
	// supersampled mask (quality*width * quality*height). When a request would
	// exceed this budget the quality is clamped down rather than failing, so
	// rendering always completes with bounded memory. At roughly one byte per
	// high-resolution sample this caps the coverage mask at about 16 MiB,
	// before the fixed-size accumulator buffer of the underlying rasterizer.
	MaxHiResPixels = 1 << 24 // 16,777,216 high-resolution pixels
)

// whiteOpaque is the uniform source used while accumulating the coverage
// mask; only the resulting alpha channel is consumed.
var whiteOpaque = image.NewUniform(image.Opaque)

// ScannerSS is a Scanner providing optional, configurable high-quality
// antialiasing through grid (box) supersampling.
//
// The default output of the package (ScannerGV) samples coverage once per
// pixel, which can leave thin stroked lines looking broken or "dashed" after
// scaling. ScannerSS optionally takes Quality*Quality samples per pixel,
// applies the same coverage rasterizer as ScannerGV on the finer grid, and
// averages the sample areas back down to the destination resolution. Because
// the down-sampling step is an exact area average, filled area and blended
// alpha are conserved: a half-opaque color over a fixed coverage keeps the
// same effective coverage, and integer translations of a path produce a
// translated image rather than flickering edge pixels.
//
// A Quality of MinQuality (1) is handled entirely by the embedded ScannerGV
// and is therefore bit-identical to the pre-existing default output. Quality
// above 1 is clamped to MaxQuality and, independently, to the largest quality
// whose high-resolution mask fits within MaxHiResPixels. The effective quality
// can be queried through EffectiveQuality.
//
// ScannerSS satisfies the Scanner interface and can be passed to NewFiller,
// NewStroker and NewDasher anywhere a ScannerGV is used. Like ScannerGV it
// implements only the non-zero winding rule.
type ScannerSS struct {
	*ScannerGV

	// quality is the requested number of samples per pixel along each axis.
	quality int

	// r accumulates the path geometry on the quality-scaled grid.
	r vector.Rasterizer

	// hiMask is the quality-scaled coverage mask.
	hiMask *image.Alpha
	// loMask is the box-averaged, destination-resolution coverage mask.
	loMask *image.Alpha

	// started reports whether any path segment was added since the last Clear.
	started bool
	// hiW and hiH are the current high-resolution mask dimensions.
	hiW, hiH int
}

// Compile-time assertion that ScannerSS is a Scanner.
var _ Scanner = (*ScannerSS)(nil)

// NewScannerSS creates a supersampling Scanner with the given destination
// bounds and sampling quality.
//
// A quality of 1 reproduces the standard ScannerGV output exactly. Higher
// values (up to MaxQuality) trade time and memory for smoother edge coverage.
// The quality is clamped to the resource limits described by MaxQuality and
// MaxHiResPixels; use EffectiveQuality to inspect the value in effect.
func NewScannerSS(width, height, quality int, dest *image.RGBA,
	targ image.Rectangle) *ScannerSS {
	s := &ScannerSS{
		ScannerGV: NewScannerGV(width, height, dest, targ),
		quality:   clampQuality(quality),
	}
	s.initHiRes()
	return s
}

// SetQuality changes the sampling quality (samples per pixel along each
// axis). Values outside [MinQuality, MaxQuality] are clamped, and the quality
// is additionally clamped so that the high-resolution mask stays within the
// MaxHiResPixels budget. The current path is cleared, matching SetBounds.
func (s *ScannerSS) SetQuality(quality int) {
	s.quality = clampQuality(quality)
	s.Clear()
	s.initHiRes()
}

// Quality returns the requested sampling quality. See EffectiveQuality for
// the value actually used after applying the resource limits.
func (s *ScannerSS) Quality() int { return s.quality }

// EffectiveQuality returns the sampling quality in effect after clamping to
// MaxQuality and the MaxHiResPixels budget for the current bounds. It is 1
// whenever the scanner is rendering through the plain ScannerGV path.
func (s *ScannerSS) EffectiveQuality() int {
	return s.effectiveQualityFor(s.ScannerGV.r.Size().X, s.ScannerGV.r.Size().Y)
}

func (s *ScannerSS) effectiveQualityFor(w, h int) int {
	q := s.quality
	if q < MinQuality {
		q = MinQuality
	}
	if q > MaxQuality {
		q = MaxQuality
	}
	if w <= 0 || h <= 0 {
		return MinQuality
	}
	// Shrink q until q*w * q*h fits the high-resolution pixel budget. The
	// intermediate products are computed in 64 bits to avoid overflow on
	// large canvases.
	for q > MinQuality && int64(q)*int64(q)*int64(w)*int64(h) > MaxHiResPixels {
		q--
	}
	return q
}

func clampQuality(quality int) int {
	if quality < MinQuality {
		return MinQuality
	}
	if quality > MaxQuality {
		return MaxQuality
	}
	return quality
}

// initHiRes (re)allocates the high- and low-resolution masks for the current
// bounds and effective quality. At effective quality 1 no supersampling
// buffers are needed.
func (s *ScannerSS) initHiRes() {
	size := s.ScannerGV.r.Size()
	q := s.effectiveQualityFor(size.X, size.Y)
	if q <= MinQuality || size.X <= 0 || size.Y <= 0 {
		s.hiW, s.hiH = 0, 0
		s.hiMask, s.loMask = nil, nil
		return
	}
	s.hiW, s.hiH = size.X*q, size.Y*q
	s.hiMask = image.NewAlpha(image.Rect(0, 0, s.hiW, s.hiH))
	// The low-resolution mask keeps the rasterizer's pixel dimensions (so the
	// box down-sampling indexing is always in range) but adopts the
	// destination's rectangle origin, so it aligns pixel-for-pixel with a
	// non-zero-origin destination when compositing. Any size mismatch between
	// the mask and the destination is intersected safely by image/draw.
	s.loMask = image.NewAlpha(s.loMaskBounds(size))
	s.r.Reset(s.hiW, s.hiH)
}

// loMaskBounds returns the low-resolution mask rectangle for the given
// rasterizer size: the rasterizer's width/height positioned at the
// destination's origin, or an origin-zero rectangle if no destination is set.
func (s *ScannerSS) loMaskBounds(size image.Point) image.Rectangle {
	if s.ScannerGV.Dest != nil {
		min := s.ScannerGV.Dest.Bounds().Min
		return image.Rectangle{
			Min: min,
			Max: image.Point{X: min.X + size.X, Y: min.Y + size.Y},
		}
	}
	return image.Rect(0, 0, size.X, size.Y)
}

// scaleFixed maps a fixed.Int26_6 coordinate from the destination grid onto
// the quality-scaled grid, rounding to the nearest high-resolution sub-pixel
// in the same 1/64 unit space.
func scaleFixed(a fixed.Int26_6, q int) fixed.Int26_6 {
	v := int64(a) * int64(q)
	// Fixed-point multiplication by an integer quality cannot overflow once
	// q is bounded by MaxQuality and coordinates by int32, but guard anyway.
	if v > math.MaxInt32 {
		return fixed.Int26_6(math.MaxInt32)
	}
	if v < math.MinInt32 {
		return fixed.Int26_6(math.MinInt32)
	}
	return fixed.Int26_6(v)
}

// Start begins a new path on both the embedded default scanner (used for the
// quality-1 path and bounds tracking) and the high-resolution rasterizer.
func (s *ScannerSS) Start(a fixed.Point26_6) {
	s.started = true
	s.ScannerGV.Start(a)
	if s.EffectiveQuality() <= MinQuality {
		return
	}
	q := s.EffectiveQuality()
	s.r.MoveTo(float32(scaleFixed(a.X, q))/64, float32(scaleFixed(a.Y, q))/64)
}

// Line extends the current path on both rasterizers.
func (s *ScannerSS) Line(b fixed.Point26_6) {
	s.started = true
	s.ScannerGV.Line(b)
	if s.EffectiveQuality() <= MinQuality {
		return
	}
	q := s.EffectiveQuality()
	s.r.LineTo(float32(scaleFixed(b.X, q))/64, float32(scaleFixed(b.Y, q))/64)
}

// Clear drops all accumulated path segments on both rasterizers.
func (s *ScannerSS) Clear() {
	s.started = false
	s.ScannerGV.Clear()
	if s.hiW > 0 && s.hiH > 0 {
		s.r.Reset(s.hiW, s.hiH)
	}
}

// SetBounds resizes the scanner and re-derives the supersampling buffers.
func (s *ScannerSS) SetBounds(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	s.ScannerGV.SetBounds(width, height)
	// ScannerGV.SetBounds already cleared its rasterizer; reset our state.
	s.started = false
	s.initHiRes()
}

// Draw composites the accumulated path coverage onto the destination.
//
// At effective quality 1 it delegates to ScannerGV.Draw unchanged. At higher
// quality the path is rasterized into an N*N-sized alpha mask, box-averaged
// down to the destination size, and then composited with the configured
// source using the standard source-over blend. An empty path leaves the
// destination untouched, as does a fully clipped or zero-area draw.
func (s *ScannerSS) Draw() {
	// A zero-size rasterizer (SetBounds(0,0) or negative bounds) has nothing
	// to render into; drawing would panic inside the underlying rasterizer, so
	// treat it as a no-op with a well-defined result.
	size := s.ScannerGV.r.Size()
	if size.X <= 0 || size.Y <= 0 {
		return
	}
	q := s.EffectiveQuality()
	if q <= MinQuality {
		if !s.started {
			// An empty path must leave the destination untouched.
			return
		}
		s.ScannerGV.Draw()
		return
	}
	if !s.started || s.hiMask == nil || s.loMask == nil {
		// Nothing was added since the last Clear: an empty path must not
		// modify the destination.
		return
	}

	// 1. Rasterize the fine-grid coverage into the high-resolution alpha mask.
	//    An opaque white source makes mask alpha equal to the accumulated
	//    coverage; draw.Src overwrites every pixel, so the reused mask does not
	//    need clearing first. The mask buffer is allocated once (in
	//    initHiRes/SetBounds) and reused across Draw calls to avoid per-frame
	//    allocation on large canvases.
	if s.hiMask == nil || s.hiMask.Rect.Dx() != s.hiW || s.hiMask.Rect.Dy() != s.hiH {
		s.hiMask = image.NewAlpha(image.Rect(0, 0, s.hiW, s.hiH))
	}
	s.r.DrawOp = draw.Src
	s.r.Draw(s.hiMask, s.hiMask.Bounds(), whiteOpaque, image.Point{})

	// 2. Box-average q*q samples per destination pixel. The average is an
	//    exact area average of coverage, so total covered area and the
	//    effective alpha of semi-transparent sources are conserved.
	boxDown(s.loMask, s.hiMask, q)

	dst := s.ScannerGV.Dest
	r := dst.Bounds()

	// 3. Apply the optional clip rectangle to the coverage mask directly.
	//    ScannerGV expresses clipping through a ClipImage source whose At()
	//    returns transparent outside the clip. That source cannot be reused
	//    with the standard library's draw.DrawMask: ClipImage embeds
	//    image.Uniform, and the library's *image.Uniform fast path ignores the
	//    overridden At() (and therefore the clip). Zeroing the mask outside
	//    the clip yields the identical coverage and keeps compositing simple.
	clip := s.ScannerGV.clipImage.clip
	mask := s.loMask
	if clip != image.ZR {
		applyClip(mask, clip.Intersect(r))
	}

	// 4. Composite the averaged mask with the configured source. The source is
	//    either a solid uniform color or a coordinate-based ColorFunc evaluated
	//    at absolute destination coordinates, matching ScannerGV. sourceFor
	//    hides any concrete image.Uniform type (so image/draw cannot take a fast
	//    path that bypasses a ColorFunc) and resolves the case where only a
	//    solid color was configured, leaving ColorFuncImage.colorFunc nil.
	//    Clipping was already applied to the mask in step 3.
	src := s.sourceFor()
	// Anchor source sampling at the destination minimum plus the configured
	// Offset so a ColorFunc(x, y) sees the absolute destination pixel. The
	// mask shares the destination grid and is anchored at its minimum.
	draw.DrawMask(dst, r, src, r.Min.Add(s.ScannerGV.Offset), mask, r.Min, draw.Over)
}

// sourceFor returns the image used to color the coverage mask. When a clip is
// active or a ColorFunc was set it is the per-pixel ColorFuncImage; otherwise
// it is the scanner's plain uniform source. Either value is wrapped so the
// standard library uses the general At-based compositing path.
func (s *ScannerSS) sourceFor() image.Image {
	cfi := &s.ScannerGV.clipImage.ColorFuncImage
	if cfi.colorFunc != nil {
		return nonUniformSource{Image: cfi}
	}
	// Solid color only: the embedded Uniform holds it. Wrap it to avoid the
	// *image.Uniform glyph fast path, which would ignore the mask anchoring and
	// coordinate conventions relied on above.
	return nonUniformSource{Image: &cfi.Uniform}
}

// nonUniformSource wraps an image.Image so it is never recognized as a
// concrete *image.Uniform by image/draw, forcing the general (At-based)
// compositing path that honors a per-pixel ColorFunc and the supplied source
// point. It adds no behavior of its own beyond hiding the underlying dynamic
// type.
type nonUniformSource struct {
	image.Image
}

// applyClip zeroes every alpha pixel of m outside clip. Pixels inside the
// clip are left unchanged.
func applyClip(m *image.Alpha, clip image.Rectangle) {
	for y := m.Rect.Min.Y; y < m.Rect.Max.Y; y++ {
		for x := m.Rect.Min.X; x < m.Rect.Max.X; x++ {
			if !(image.Point{X: x, Y: y}).In(clip) {
				m.SetAlpha(x, y, color.Alpha{})
			}
		}
	}
}

// boxDown averages every q by q block of hi into the matching pixel of lo.
// lo must be q times smaller than hi in each dimension. lo may carry a
// non-zero rectangle origin (a non-zero destination origin); the averaged
// value is written at the corresponding absolute coordinate. The summation
// uses a 32-bit accumulator; with q <= MaxQuality (8) the 64 sample values of
// at most 255 cannot overflow it.
func boxDown(lo, hi *image.Alpha, q int) {
	w, h := lo.Rect.Dx(), lo.Rect.Dy()
	q2 := uint32(q * q)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum uint32
			base := y*q*hi.Stride + x*q
			for dy := 0; dy < q; dy++ {
				row := base + dy*hi.Stride
				for dx := 0; dx < q; dx++ {
					sum += uint32(hi.Pix[row+dx])
				}
			}
			// Round to the nearest representable alpha instead of truncating
			// so the expected coverage is unbiased across the whole image.
			lo.SetAlpha(lo.Rect.Min.X+x, lo.Rect.Min.Y+y,
				color.Alpha{A: uint8((sum + q2/2) / q2)})
		}
	}
}
