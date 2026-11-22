// Copyright 2026 by the rasterx Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rasterx

import (
	"errors"
	"image"
	"math"

	"golang.org/x/image/math/fixed"
)

// Errors returned by the stroke-geometry export functions. Callers should use
// errors.Is to test for them.
var (
	// ErrInvalidParam is returned when a stroke parameter is non-finite or
	// outside of its valid domain (negative width, zero/negative dash length,
	// etc.).
	ErrInvalidParam = errors.New("rasterx: invalid stroke parameter")
	// ErrDashOffset is returned when a dash offset cannot be reduced into the
	// range of the dash pattern.
	ErrDashOffset = errors.New("rasterx: dash offset is out of range")
	// ErrMiterOverflow is returned when the miter limit is so large that the
	// internally scaled limit would overflow fixed.Int26_6.
	ErrMiterOverflow = errors.New("rasterx: miter limit overflows fixed point range")
)

// OutlineScanner is a Scanner that does not rasterize anything. Instead it
// captures the Start/Line stream emitted by a Stroker or Dasher and records it
// as a Path.
//
// The recorded Path uses PathMoveTo to delimit contours and PathLineTo for
// every (flattened) edge. Contours are closed implicitly by the following
// PathMoveTo, exactly as the stroker feeds the rasterizer: the vector
// scanner closes an open contour automatically on the next pen move. Emitting
// explicit PathClose commands instead would add duplicate closing edges that
// change the accumulated coverage, so they are deliberately not written.
// SubPaths reports every contour with Closed == true for callers that need the
// closure and winding semantics spelled out.
//
// The contours carry the stroker's edge orientations so that non-zero-winding
// filling reproduces the stroke exactly. Because the path relies on the
// rasterizer's implicit-on-move closure, it must be replayed straight into a
// Scanner via Path.AddToScanner; do not use Path.AddTo, whose Filler inserts
// explicit closing edges that alter coverage. FillStrokeOutline performs the
// faithful replay for you.
//
// The zero value is not usable; create one with NewOutlineScanner.
type OutlineScanner struct {
	path     Path
	bounds   fixed.Rectangle26_6
	hasPt    bool
	hasLimit bool // bounds have been initialized with at least one point
}

// NewOutlineScanner returns a ready-to-use OutlineScanner with the given
// bounds. The bounds only mirror the Scanner contract and never clip the
// captured geometry; pass zero when they are unknown.
func NewOutlineScanner(width, height int) *OutlineScanner {
	o := new(OutlineScanner)
	o.SetBounds(width, height)
	return o
}

// Start begins a new contour. The previous contour, if any, is implicitly
// closed by the pen move in the same way the rasterizer closes it.
func (o *OutlineScanner) Start(a fixed.Point26_6) {
	o.path.Start(a)
	o.hasPt = true
	o.accumulate(a)
}

// Line extends the current contour with a straight segment.
func (o *OutlineScanner) Line(b fixed.Point26_6) {
	o.path.Line(b)
	o.accumulate(b)
}

// Draw marks the end of a stroke capture. The final open contour is treated as
// closed, matching the rasterizer. Draw is safe to call repeatedly and emits
// no extra path commands.
func (o *OutlineScanner) Draw() {}

// Path returns the captured stroke outline. The returned slice is owned by the
// scanner and is reset by Clear; copy it if it must outlive the next Clear.
func (o *OutlineScanner) Path() Path { return o.path }

// accumulate expands the recorded extent with p.
func (o *OutlineScanner) accumulate(p fixed.Point26_6) {
	if !o.hasLimit {
		o.bounds = fixed.Rectangle26_6{Min: p, Max: p}
		o.hasLimit = true
		return
	}
	if p.X < o.bounds.Min.X {
		o.bounds.Min.X = p.X
	}
	if p.Y < o.bounds.Min.Y {
		o.bounds.Min.Y = p.Y
	}
	if p.X > o.bounds.Max.X {
		o.bounds.Max.X = p.X
	}
	if p.Y > o.bounds.Max.Y {
		o.bounds.Max.Y = p.Y
	}
}

// GetPathExtent returns the bounding box of every point recorded so far. The
// returned rectangle is the zero value when nothing has been captured.
func (o *OutlineScanner) GetPathExtent() fixed.Rectangle26_6 { return o.bounds }

// SetBounds mirrors the Scanner contract. It does not clip or discard any
// captured geometry and, unlike a rasterizing scanner, does not Clear the
// accumulated path.
func (o *OutlineScanner) SetBounds(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
}

// SetColor accepts any value and retains nothing; the capture is independent of
// paint attributes.
func (o *OutlineScanner) SetColor(color interface{}) {}

// SetWinding is a no-op. The exported contours always use non-zero winding.
func (o *OutlineScanner) SetWinding(useNonZeroWinding bool) {}

// SetClip is a no-op; stroke geometry export is not clipped.
func (o *OutlineScanner) SetClip(rect image.Rectangle) {}

// Clear drops all captured geometry and resets the extent.
func (o *OutlineScanner) Clear() {
	o.path.Clear()
	o.bounds = fixed.Rectangle26_6{}
	o.hasPt = false
	o.hasLimit = false
}

// StrokeConfig holds the parameters of a stroke-to-outline conversion. It
// mirrors the arguments of Stroker.SetStroke and Dasher.SetStroke.
type StrokeConfig struct {
	// Width is the stroke width in fixed.Int26_6 units (1<<6 is one pixel).
	// A width of zero degrades stably to an empty outline.
	Width fixed.Int26_6
	// MiterLimit is the SVG miter limit in fixed.Int26_6 units, measured
	// relative to the stroke width as in SetStroke.
	MiterLimit fixed.Int26_6
	// CapL and CapT are the leading and trailing cap functions. A nil value
	// follows the same defaulting rules as SetStroke.
	CapL, CapT CapFunc
	// Gap is the join gap function; nil defaults according to JoinMode.
	Gap GapFunc
	// Join is the join mode.
	Join JoinMode
	// Dashes is the dash pattern in pixel units, nil or empty for a solid
	// stroke. The slice is never modified.
	Dashes []float64
	// DashOffset is the starting dash offset in pixel units.
	DashOffset float64
}

// captureW/captureH size the internal vector rasterizer's accumulation buffer
// while a stroke is being captured. They do not clip valid geometry: the
// rasterizer accepts off-canvas coordinates, and the buffer only has to exist.
// 4096 comfortably covers the fixed.Int26_6 coordinate range used in practice;
// it exceeds the rasterizer's 512px floating-point threshold, but geometry
// capture only ever calls Start/Line, so the slower curve math is never used.
const (
	captureW = 4096
	captureH = 4096
)

// StrokeOutline converts the stroke of src defined by cfg into a fillable
// outline Path. Straight segments, quadratic and cubic beziers, dashing, cap
// functions, gap functions and every JoinMode are handled by reusing the
// existing stroking machinery, so the result is exactly the geometry the
// Stroker would otherwise rasterize immediately.
//
// If m is non-nil it is applied to the geometry while stroking, equivalent to
// wrapping the rasterizer in a MatrixAdder.
//
// Neither src, cfg.Dashes nor m are modified. An empty src path yields an empty
// outline and a nil error. Invalid parameters (non-finite values, negative
// width or miter limit, zero/negative dash lengths, non-reducible dash offset,
// miter limits that overflow fixed point) produce a wrapped error and no path.
func StrokeOutline(src Path, cfg StrokeConfig, m *Matrix2D) (Path, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if m != nil {
		if err := validateMatrix(*m); err != nil {
			return nil, err
		}
	}

	scanner := NewOutlineScanner(0, 0)
	// The capture scanner ignores rasterizer bounds, but the embedded vector
	// rasterizer sizes its accumulation buffer to them, so use a generously
	// fixed canvas rather than the geometry bounds: coordinates are stored
	// untransformed and can legitimately be negative or off-canvas.
	w, h := captureW, captureH
	scanner.SetBounds(w, h)

	if len(cfg.Dashes) > 0 {
		d := NewDasher(w, h, scanner)
		d.SetStroke(cfg.Width, cfg.MiterLimit, cfg.CapL, cfg.CapT, cfg.Gap,
			cfg.Join, append([]float64(nil), cfg.Dashes...), cfg.DashOffset)
		var adder Adder = d
		if m != nil {
			adder = &MatrixAdder{Adder: d, M: *m}
		}
		src.AddTo(adder)
		d.Stop(false)
	} else {
		s := NewStroker(w, h, scanner)
		s.SetStroke(cfg.Width, cfg.MiterLimit, cfg.CapL, cfg.CapT, cfg.Gap, cfg.Join)
		var adder Adder = s
		if m != nil {
			adder = &MatrixAdder{Adder: s, M: *m}
		}
		src.AddTo(adder)
		s.Stop(false)
	}
	scanner.Draw()

	// Return an independent copy so later Clear/reuse of the scanner cannot
	// alias the caller's result.
	out := make(Path, len(scanner.path))
	copy(out, scanner.path)
	return out, nil
}

// AddToScanner replays a stroke outline produced by StrokeOutline directly into
// a Scanner. It is the faithful inverse of the capture: every PathMoveTo
// becomes a Scanner.Start and every PathLineTo a Scanner.Line, with no closing
// edges added. The scanner closes each contour implicitly on the following
// Start, exactly as during the original stroke, which is what makes the fill
// pixel-identical.
//
// Use this rather than Path.AddTo for stroke outlines: AddTo routes through a
// Filler whose Stop draws an explicit edge back to the contour start, doubling
// the thin connecting edges and changing the rasterized coverage.
func (p Path) AddToScanner(s Scanner) {
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			s.Start(fixed.Point26_6{p[i+1], p[i+2]})
			i += 3
		case PathLineTo:
			s.Line(fixed.Point26_6{p[i+1], p[i+2]})
			i += 3
		case PathClose:
			// No-op: stroke contours are closed implicitly by the next move;
			// tolerate an explicit close without reintroducing an edge.
			i++
		default:
			// StrokeOutline only emits move/line commands.
			return
		}
	}
}

// FillStrokeOutline exports the stroke of src and fills it through scanner in
// one step, returning the outline Path as well so callers can retain the
// editable geometry. The fill is pixel-for-pixel identical to stroking src
// with the same Scanner directly. If m is non-nil it is applied while stroking.
//
// The scanner's color, clip and winding must be configured by the caller.
func FillStrokeOutline(src Path, cfg StrokeConfig, m *Matrix2D, scanner Scanner) (Path, error) {
	outline, err := StrokeOutline(src, cfg, m)
	if err != nil {
		return nil, err
	}
	outline.AddToScanner(scanner)
	scanner.Draw()
	return outline, nil
}

// validate checks every numeric parameter before any geometry is produced.
func (cfg StrokeConfig) validate() error {
	if cfg.Width < 0 || !isFiniteFixed(cfg.Width) {
		return ErrInvalidParam
	}
	if cfg.MiterLimit < 0 || !isFiniteFixed(cfg.MiterLimit) {
		return ErrInvalidParam
	}
	// SetStroke computes mLimit = (u * miterLimit) >> 6; reject inputs that
	// would overflow the int32 fixed point range instead of wrapping silently.
	if cfg.Width > 0 && cfg.MiterLimit > 0 {
		u := cfg.Width / 2
		if int64(u)*int64(cfg.MiterLimit) > int64(math.MaxInt32) {
			return ErrMiterOverflow
		}
	}
	switch cfg.Join {
	case Arc, ArcClip, Miter, MiterClip, Bevel, Round:
	default:
		return ErrInvalidParam
	}

	if len(cfg.Dashes) == 0 {
		if math.IsNaN(cfg.DashOffset) || math.IsInf(cfg.DashOffset, 0) {
			return ErrInvalidParam
		}
		return nil
	}
	var total float64
	for _, d := range cfg.Dashes {
		if math.IsNaN(d) || math.IsInf(d, 0) || d <= 0 {
			return ErrInvalidParam
		}
		if d*64 > float64(math.MaxInt32) {
			return ErrInvalidParam
		}
		total += d
	}
	if math.IsNaN(cfg.DashOffset) || math.IsInf(cfg.DashOffset, 0) {
		return ErrInvalidParam
	}
	if math.Abs(cfg.DashOffset)*64 > float64(math.MaxInt32) ||
		total*64 > float64(math.MaxInt32) {
		return ErrDashOffset
	}
	// Reduce the offset into [0, total) exactly as a consumer of the dasher
	// must; a non-finite remainder would make the dash phase unreachable.
	off := math.Mod(cfg.DashOffset, total)
	if off < 0 {
		off += total
	}
	if math.IsNaN(off) || math.IsInf(off, 0) {
		return ErrDashOffset
	}
	return nil
}

// validateMatrix rejects non-finite matrix entries that would otherwise turn
// the captured coordinates into NaN fixed point values.
func validateMatrix(m Matrix2D) error {
	for _, v := range [6]float64{m.A, m.B, m.C, m.D, m.E, m.F} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ErrInvalidParam
		}
	}
	return nil
}

// isFiniteFixed reports whether v maps to a finite float value. Every
// fixed.Int26_6 is finite by construction, so this currently always returns
// true, but the check documents and centralizes the domain contract.
func isFiniteFixed(v fixed.Int26_6) bool {
	f := float64(v)
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// SubPath is one closed contour of an exported stroke outline.
type SubPath struct {
	// Points are the vertices of the contour in emission order.
	Points []fixed.Point26_6
	// Closed is always true for contours produced by StrokeOutline.
	Closed bool
}

// SplitSubPaths breaks a Path captured by OutlineScanner into its constituent
// contours. Stroke contours are delimited by PathMoveTo and closed implicitly;
// every returned subpath therefore reports Closed == true regardless of
// whether the path also contains explicit PathClose commands.
func SplitSubPaths(p Path) ([]SubPath, error) {
	var subs []SubPath
	var cur SubPath
	flush := func() {
		if len(cur.Points) > 0 {
			cur.Closed = true
			subs = append(subs, cur)
		}
		cur = SubPath{}
	}
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			flush()
			cur.Points = append(cur.Points, fixed.Point26_6{p[i+1], p[i+2]})
			i += 3
		case PathLineTo:
			if len(cur.Points) == 0 {
				return nil, ErrInvalidParam
			}
			cur.Points = append(cur.Points, fixed.Point26_6{p[i+1], p[i+2]})
			i += 3
		case PathClose:
			// Tolerate an explicit close; the contour stays a single one.
			flush()
			i++
		default:
			// Stroke outlines are fully flattened, so quad/cubic commands here
			// indicate a Path that was not produced by StrokeOutline.
			return nil, ErrInvalidParam
		}
	}
	flush()
	return subs, nil
}

// SignedArea returns the signed shoelace area of s in fixed point units
// (divide by 64*64 == 4096 to obtain pixel²). In the screen coordinate system
// used by fixed.Point26_6 (y grows downward) a positive value means the
// vertices run clockwise on screen, negative means counter-clockwise. The
// stroker also emits one- and two-vertex helper contours, which return zero.
func (s SubPath) SignedArea() int64 {
	n := len(s.Points)
	if n < 3 {
		return 0
	}
	var area int64
	for i := 0; i < n; i++ {
		j := (i + 1) % n
		area += int64(s.Points[i].X)*int64(s.Points[j].Y) -
			int64(s.Points[j].X)*int64(s.Points[i].Y)
	}
	return area / 2
}
