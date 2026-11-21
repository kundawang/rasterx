// Copyright 2018 by the rasterx Authors. All rights reserved.
//_
// Created 2017 by S.R.Wiley

package rasterx

import (
	"image"
	"image/color"

	"golang.org/x/image/math/fixed"
)

type (
	// ColorFunc maps a color to x y coordinates
	ColorFunc func(x, y int) color.Color
	// Scanner interface for path generating types
	Scanner interface {
		Start(a fixed.Point26_6)
		Line(b fixed.Point26_6)
		Draw()
		GetPathExtent() fixed.Rectangle26_6
		SetBounds(w, h int)
		SetColor(color interface{})
		SetWinding(useNonZeroWinding bool)
		Clear()

		// SetClip sets an optional clipping rectangle to restrict rendering
		// only to that region -- if size is 0 then ignored (set to image.ZR
		// to clear)
		SetClip(rect image.Rectangle)
	}
	// Adder interface for types that can accumlate path commands
	Adder interface {
		// Start starts a new curve at the given point.
		Start(a fixed.Point26_6)
		// Line adds a line segment to the path
		Line(b fixed.Point26_6)
		// QuadBezier adds a quadratic bezier curve to the path
		QuadBezier(b, c fixed.Point26_6)
		// CubeBezier adds a cubic bezier curve to the path
		CubeBezier(b, c, d fixed.Point26_6)
		// Closes the path to the start point if closeLoop is true
		Stop(closeLoop bool)
	}
	// Rasterx extends the adder interface to include lineF and joinF functions
	Rasterx interface {
		Adder
		lineF(b fixed.Point26_6)
		joinF()
	}

	// Filler satisfies Rasterx
	Filler struct {
		Scanner
		a, first fixed.Point26_6
	}
)

// Start starts a new path at the given point.
func (r *Filler) Start(a fixed.Point26_6) {
	r.a = a
	r.first = a
	r.Scanner.Start(a)
}

// Stop sends a path at the given point.
func (r *Filler) Stop(isClosed bool) {
	if r.first != r.a {
		r.Line(r.first)
	}
}

// QuadBezier adds a quadratic segment to the current curve.
func (r *Filler) QuadBezier(b, c fixed.Point26_6) {
	r.QuadBezierF(r, b, c)
}

// QuadBezierF adds a quadratic segment to the sgm Rasterizer. Every rasterizer
// type (Filler, Stroker, Dasher) reaches the subdivision through this method,
// so the curve segment core in curve.go is the single flattening path shared
// by filling, stroking and dashing.
func (r *Filler) QuadBezierF(sgm Rasterx, b, c fixed.Point26_6) {
	// check for degenerate bezier
	if IsDegenerateQuad(r.a, b, c) {
		sgm.Line(c)
		return
	}
	sgm.joinF()
	FlattenQuad(r.a, b, c, func(p fixed.Point26_6) {
		sgm.lineF(p)
	})
}

// CubeBezier adds a cubic bezier to the curve
func (r *Filler) CubeBezier(b, c, d fixed.Point26_6) {
	r.CubeBezierF(r, b, c, d)
}

// joinF is a no-op for a filling rasterizer. This is used in stroking and dashed
// stroking
func (r *Filler) joinF() {

}

// Line for a filling rasterizer is just the line call in scan
func (r *Filler) Line(b fixed.Point26_6) {
	r.lineF(b)
}

// lineF for a filling rasterizer is just the line call in scan
func (r *Filler) lineF(b fixed.Point26_6) {
	r.Scanner.Line(b)
	r.a = b
}

// CubeBezierF adds a cubic bezier to the curve, sending the line calls to the
// sgm Rasterizer. As with QuadBezierF this is the common entry point for
// filling, stroking and dashing, and the actual subdivision lives in the
// shared curve segment core in curve.go.
func (r *Filler) CubeBezierF(sgm Rasterx, b, c, d fixed.Point26_6) {
	if IsDegenerateCube(r.a, b, c, d) {
		sgm.Line(d)
		return
	}
	sgm.joinF()
	FlattenCubic(r.a, b, c, d, func(p fixed.Point26_6) {
		sgm.lineF(p)
	})
}

// Clear resets the filler
func (r *Filler) Clear() {
	r.a = fixed.Point26_6{}
	r.first = r.a
	r.Scanner.Clear()
}

// SetBounds sets the maximum width and height of the rasterized image and
// calls Clear. The width and height are in pixels, not fixed.Int26_6 units.
func (r *Filler) SetBounds(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	r.Scanner.SetBounds(width, height)
	r.Clear()
}

// NewFiller returns a Filler ptr with default values.
// A Filler in addition to rasterizing lines like a Scann,
// can also rasterize quadratic and cubic bezier curves.
// If Scanner is nil default scanner ScannerGV is used
func NewFiller(width, height int, scanner Scanner) *Filler {
	r := new(Filler)
	r.Scanner = scanner
	r.SetBounds(width, height)
	r.SetWinding(true)
	return r
}
