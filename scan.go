// Copyright 2018 by the rasterx Authors. All rights reserved.
//_
// Created 2017 by S.R.Wiley
// This version of Scanner wraps the golang.org/x/image/vector
// rasterizer.

package rasterx

import (
	"image"
	"math"

	"image/color"

	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

func (c *ColorFuncImage) At(x, y int) color.Color {
	return c.colorFunc(x, y)
}

type (
	ColorFuncImage struct {
		image.Uniform
		colorFunc ColorFunc
	}

	// Rasterizer converts a path to a raster using the grainless algorithm.
	ScannerGV struct {
		r vector.Rasterizer
		//a, first fixed.Point26_6
		Dest                   *image.RGBA
		Targ                   image.Rectangle
		clipImage              *ClipImage
		Source                 image.Image
		Offset                 image.Point
		minX, minY, maxX, maxY fixed.Int26_6 // keep track of bounds
	}
)

type ClipImage struct {
	ColorFuncImage
	clip image.Rectangle
}

var noApha = color.RGBA{0, 0, 0, 0}

func (s *ScannerGV) GetPathExtent() fixed.Rectangle26_6 {
	return fixed.Rectangle26_6{fixed.Point26_6{s.minX, s.minY}, fixed.Point26_6{s.maxX, s.maxY}}
}

func (c *ClipImage) At(x, y int) color.Color {
	p := image.Point{x, y}
	if p.In(c.clip) {
		return c.ColorFuncImage.At(x, y)
	} else {
		return noApha
	}
}

func (s *ScannerGV) SetWinding(useNonZeroWinding bool) {
	// no-op as scanner gv does not support even-odd winding
}

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

// clampCoord converts a 26.6 coordinate to the float32 the backing
// rasterizer consumes, saturating at SafeCoordLimit. The rasterizer's fixed
// point path scales by 512 internally, so values beyond the guard's limit
// would overflow its int32 accumulator.
func clampCoord(v fixed.Int26_6) float32 {
	return float32(clampSafeInt26(float64(v))) / 64
}

// clampPoint saturates both coordinates at SafeCoordLimit.
func clampPoint(p fixed.Point26_6) fixed.Point26_6 {
	return fixed.Point26_6{
		X: clampSafeInt26(float64(p.X)),
		Y: clampSafeInt26(float64(p.Y))}
}

// Start starts a new path at the given point.
func (s *ScannerGV) Start(a fixed.Point26_6) {
	a = clampPoint(a)
	s.set(a)
	s.r.MoveTo(clampCoord(a.X), clampCoord(a.Y))
}

// Line adds a linear segment to the current curve.
func (s *ScannerGV) Line(b fixed.Point26_6) {
	b = clampPoint(b)
	s.set(b)
	s.r.LineTo(clampCoord(b.X), clampCoord(b.Y))
}

func (s *ScannerGV) Draw() {
	if s.Dest == nil {
		// Nothing to draw into. Reporting through the scanner is not
		// possible here (Draw has no error return), so simply do nothing
		// rather than panicking in the vector rasterizer.
		return
	}
	// This draws the entire bounds of the image, because
	// at this point the alpha mask does not shift with the
	// placement of the target rectangle in the vector rasterizer.
	//
	// The alpha mask has exactly size.X*size.Y entries, indexed from zero
	// for the rectangle handed to Draw. A destination larger than the
	// rasterizer would therefore index past the mask. Clamp the rectangle to
	// the mask size so an oversized or mismatched destination cannot write
	// out of bounds.
	r := s.Dest.Bounds()
	sz := s.r.Size()
	if r.Dx() > sz.X {
		r.Max.X = r.Min.X + sz.X
	}
	if r.Dy() > sz.Y {
		r.Max.Y = r.Min.Y + sz.Y
	}
	if r.Empty() {
		return
	}
	s.r.Draw(s.Dest, r, s.Source, s.Offset)

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

// Clear cancels any previous accumulated scans
func (s *ScannerGV) Clear() {
	p := s.r.Size()
	s.r.Reset(p.X, p.Y)
	const mxfi = fixed.Int26_6(math.MaxInt32)
	s.minX, s.minY, s.maxX, s.maxY = mxfi, mxfi, -mxfi, -mxfi
}

// SetBounds sets the maximum width and height of the rasterized image and
// calls Clear. The width and height are in pixels, not fixed.Int26_6 units.
// Bounds the backing rasterizer cannot address are discarded (the scanner
// keeps its previous size) instead of allocating an impossible buffer.
func (s *ScannerGV) SetBounds(width, height int) {
	if !ValidRasterizerBounds(width, height) {
		return
	}
	s.r.Reset(width, height)
}

// NewScanner creates a new Scanner with the given bounds.
func NewScannerGV(width, height int, dest *image.RGBA,
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
