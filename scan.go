// Package rasterx implements a rasterizer in go.
// By default rasterx uses ScannerGV to render images
// which uses the rasterizer in the golang.org/x/image/vector package.
// The freetype rasterizer under the GNU license can also be used, by
// downloading the scanFT package.
//
// Copyright 2018 All rights reserved.
// Created: 5/12/2018 by S.R.Wiley
package rasterx

import (
	"image"
	"math"

	"image/color"
	"image/draw"

	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// At returns the color at the point x,y
func (c *ColorFuncImage) At(x, y int) color.Color {
	return c.colorFunc(x, y)
}

type (
	// ColorFuncImage implements and image
	// using the provided colorFunc
	ColorFuncImage struct {
		image.Uniform
		colorFunc ColorFunc
	}

	// ScannerGV uses the google vector rasterizer
	ScannerGV struct {
		r vector.Rasterizer
		//a, first fixed.Point26_6
		Dest                   draw.Image
		Targ                   image.Rectangle
		clipImage              *ClipImage
		Source                 image.Image
		Offset                 image.Point
		minX, minY, maxX, maxY fixed.Int26_6 // keep track of bounds
		// mask is an optional per-pixel mask modulating the path
		// coverage; see SetMask. maskOffset places the mask relative
		// to the destination origin, and maskBuf is a reusable scratch
		// buffer holding coverage premultiplied by the mask.
		mask       image.Image
		maskOffset image.Point
		maskBuf    *image.Alpha
	}
)

// ClipImage is a clipable ColorFuncImage
type ClipImage struct {
	ColorFuncImage
	clip image.Rectangle
}

var noApha = color.RGBA{0, 0, 0, 0}

// GetPathExtent returns the extent of the path
func (s *ScannerGV) GetPathExtent() fixed.Rectangle26_6 {
	return fixed.Rectangle26_6{Min: fixed.Point26_6{X: s.minX, Y: s.minY}, Max: fixed.Point26_6{X: s.maxX, Y: s.maxY}}
}

// At returns the color of the ClipImage at the point x,y
func (c *ClipImage) At(x, y int) color.Color {
	p := image.Point{x, y}
	if p.In(c.clip) {
		return c.ColorFuncImage.At(x, y)
	}
	return noApha
}

// SetWinding set the winding rule for the scanner
func (s *ScannerGV) SetWinding(useNonZeroWinding bool) {
	// no-op as scanner gv does not support even-odd winding
}

// SetColor set the color type for the scanner
func (s *ScannerGV) SetColor(clr interface{}) {
	switch c := clr.(type) {
	case color.Color:
		s.clipImage.ColorFuncImage.Uniform.C = c
		if s.clipImage.clip == image.ZR {
			s.Source = &s.clipImage.ColorFuncImage.Uniform
		} else {
			s.clipImage.ColorFuncImage.colorFunc = func(x, y int) color.Color {
				return c
			}
			s.Source = s.clipImage
		}
	case ColorFunc:
		s.clipImage.ColorFuncImage.colorFunc = c
		if s.clipImage.clip == image.ZR {
			s.Source = &s.clipImage.ColorFuncImage
		} else {
			s.Source = s.clipImage
		}
	}
}

// SetClip sets an optional clipping rectangle to restrict rendering only to
// that region -- if size is 0 then ignored (set to image.ZR to clear)
func (s *ScannerGV) SetClip(rect image.Rectangle) {
	s.clipImage.clip = rect
	if s.Source == &s.clipImage.ColorFuncImage.Uniform {
		s.SetColor(s.clipImage.ColorFuncImage.Uniform.C)
	} else {
		s.SetColor(s.clipImage.ColorFuncImage.colorFunc)
	}
}

func (s *ScannerGV) set(a fixed.Point26_6) {
	if s.maxX < a.X {
		s.maxX = a.X
	}
	if s.maxY < a.Y {
		s.maxY = a.Y
	}
	if s.minX > a.X {
		s.minX = a.X
	}
	if s.minY > a.Y {
		s.minY = a.Y
	}
}

// Start starts a new path at the given point.
func (s *ScannerGV) Start(a fixed.Point26_6) {
	s.set(a)
	s.r.MoveTo(float32(a.X)/64, float32(a.Y)/64)
}

// Line adds a linear segment to the current curve.
func (s *ScannerGV) Line(b fixed.Point26_6) {
	s.set(b)
	s.r.LineTo(float32(b.X)/64, float32(b.Y)/64)
}

// Draw renders the accumulate scan to the desination
func (s *ScannerGV) Draw() {
	if s.mask != nil {
		s.drawMasked()
		return
	}
	// This draws the entire bounds of the image, because
	// at this point the alpha mask does not shift with the
	// placement of the target rectangle in the vector rasterizer
	s.r.Draw(s.Dest, s.Dest.Bounds(), s.Source, s.Offset)

	// Remove the line above and uncomment the lines below if you
	// are using a version of the vector rasterizer that shifts the alpha
	// mask with the placement of the target

	//	s.Targ.Min.X = int(s.minX >> 6)
	//	s.Targ.Min.Y = int(s.minY >> 6)
	//	s.Targ.Max.X = int(s.maxX>>6) + 1
	//	s.Targ.Max.Y = int(s.maxY>>6) + 1
	//	s.Targ = s.Targ.Intersect(s.Dest.Bounds())  // This check should be done by the rasterizer?
	//	s.r.Draw(s.Dest, s.Targ, s.Source, s.Offset)
}

// SetMask sets an optional mask image that modulates the path coverage
// when Draw is called: the coverage of each destination pixel is
// multiplied by the value of the mask at the corresponding point before
// the source is composited onto the destination with the usual
// premultiplied src-over rules. Passing nil restores the unmasked
// drawing behavior.
//
// *image.Alpha and *image.Alpha16 masks contribute their alpha channel
// directly; any other image is treated as a luminance mask, contributing
// the luminance of each pixel scaled by its alpha. Mask samples falling
// outside the mask bounds are zero, so nothing is drawn where the mask
// does not reach; a mask whose bounds (shifted by the mask offset) do
// not intersect the destination results in no drawing at all.
//
// The mask is only read, never modified, so a single mask image can be
// shared by any number of scanners and reused across any number of Draw
// and Clear calls. The mask setting itself is not affected by Clear or
// SetBounds.
func (s *ScannerGV) SetMask(mask image.Image) {
	s.mask = mask
}

// SetMaskOffset sets the offset of the mask image relative to the
// destination origin: mask pixel (x, y) applies to destination pixel
// (x+offset.X, y+offset.Y). The zero offset aligns the mask bounds with
// the destination coordinates. The offset may be changed between draws
// to shift the mask without touching the mask image.
func (s *ScannerGV) SetMaskOffset(offset image.Point) {
	s.maskOffset = offset
}

// MaskOffset returns the current mask offset; see SetMaskOffset.
func (s *ScannerGV) MaskOffset() image.Point {
	return s.maskOffset
}

// drawMasked renders the accumulated scan modulated by the mask image.
func (s *ScannerGV) drawMasked() {
	size := s.r.Size()
	if size.X <= 0 || size.Y <= 0 {
		return
	}
	cb := image.Rectangle{Max: size}
	if s.maskBuf == nil || s.maskBuf.Bounds() != cb {
		s.maskBuf = image.NewAlpha(cb)
	} else {
		// Reuse the scratch buffer between draws and layers.
		pix := s.maskBuf.Pix
		for i := range pix {
			pix[i] = 0
		}
	}
	// Render the path coverage into the scratch buffer. The buffer
	// bounds equal the rasterizer bounds, so buffer pixel (x, y) holds
	// the coverage for destination pixel (x, y) regardless of the
	// destination image origin.
	s.r.Draw(s.maskBuf, cb, image.Opaque, image.Point{})

	db := s.Dest.Bounds()
	dr := db.Intersect(cb)
	if dr.Empty() {
		return
	}
	// Modulate the coverage by the mask. Samples outside the mask
	// bounds are zero.
	mb := s.mask.Bounds()
	mo := s.maskOffset
	pix := s.maskBuf.Pix
	stride := s.maskBuf.Stride
	for y := dr.Min.Y; y < dr.Max.Y; y++ {
		i := y*stride + dr.Min.X
		my := y - mo.Y
		inY := my >= mb.Min.Y && my < mb.Max.Y
		for x := dr.Min.X; x < dr.Max.X; x++ {
			if mx := x - mo.X; !inY || mx < mb.Min.X || mx >= mb.Max.X {
				pix[i] = 0
			} else {
				mv := maskValueAt(s.mask, mx, my)
				pix[i] = uint8((uint32(pix[i])*0x101*mv/0xffff) >> 8)
			}
			i++
		}
	}
	// Composite through the modulated coverage. The source sample point
	// is adjusted so the source aligns exactly as in an unmasked Draw,
	// even when the destination origin is not (0, 0).
	sp := s.Offset.Add(dr.Min).Sub(db.Min)
	draw.DrawMask(s.Dest, dr, s.Source, sp, s.maskBuf, dr.Min, draw.Over)
}

// maskValueAt returns the value of the mask at (x, y) as a 16-bit alpha
// value in the range [0, 0xffff]. *image.Alpha and *image.Alpha16 masks
// contribute their alpha channel directly; all other images are treated
// as luminance masks, contributing their luminance scaled by their alpha.
func maskValueAt(m image.Image, x, y int) uint32 {
	switch mm := m.(type) {
	case *image.Alpha:
		return uint32(mm.AlphaAt(x, y).A) * 0x101
	case *image.Alpha16:
		return uint32(mm.Alpha16At(x, y).A)
	}
	r, g, b, a := m.At(x, y).RGBA()
	lum := (19595*r + 38470*g + 7471*b + 1<<15) >> 16
	return lum * a / 0xffff
}

// Clear cancels any previous accumulated scans
func (s *ScannerGV) Clear() {
	p := s.r.Size()
	s.r.Reset(p.X, p.Y)
	const mxfi = fixed.Int26_6(math.MaxInt32)
	s.minX, s.minY, s.maxX, s.maxY = mxfi, mxfi, -mxfi, -mxfi
}

// SetBounds sets the maximum width and height of the rasterized image and
// calls Clear. The width and height are in pixels, not fixed.Int26_6 units.
func (s *ScannerGV) SetBounds(width, height int) {
	s.r.Reset(width, height)
}

// NewScannerGV creates a new Scanner with the given bounds.
func NewScannerGV(width, height int, dest draw.Image,
	targ image.Rectangle) *ScannerGV {
	s := new(ScannerGV)
	s.SetBounds(width, height)
	s.Dest = dest
	s.Targ = targ
	s.clipImage = &ClipImage{}
	s.clipImage.ColorFuncImage.Uniform.C = &color.RGBA{255, 0, 0, 255}
	s.Source = &s.clipImage.ColorFuncImage.Uniform
	s.Offset = image.Point{0, 0}
	return s
}
