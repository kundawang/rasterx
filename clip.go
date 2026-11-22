// Copyright 2026 by the rasterx Authors. All rights reserved.
//
// Path based clipping support for ScannerGV.
//
// ScannerGV has always supported a single rectangular clip through SetClip.
// That is enough for simple blitting, but a scene graph needs nested clips:
// a round avatar mask followed by a UI rectangle, for example. This file adds
// a resumable clip stack on top of the existing rectangle clip.
//
// The clipper folds coverage into the alpha of the source image handed to the
// vector rasterizer. Every clip layer contributes an 8 bit coverage value for
// a pixel and nested layers multiply, so the result is identical to drawing
// through the intersection of all clip paths. Destination pixels with zero
// coverage are left untouched, including a fully transparent destination.
package rasterx

import (
	"errors"
	"image"
	"image/color"
	"image/draw"

	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

const (
	// DefaultMaxClipDepth is the default maximum number of nested clip
	// layers. Pushing beyond the limit fails without modifying the stack.
	DefaultMaxClipDepth = 64
	// DefaultMaxClipPixels is the default maximum number of pixels in one
	// clip layer (or, for an external mask, in the part of the mask that
	// overlaps the scanner).
	DefaultMaxClipPixels = 1 << 26
	// DefaultMaxClipMemory is the default maximum number of bytes retained
	// for internally rasterized clip coverage buffers.
	DefaultMaxClipMemory = 64 << 20
)

// Errors returned by the clip stack. They are sentinel values, so callers
// can test them with errors.Is.
var (
	// ErrClipStackEmpty is returned by PopClip when no clip is pushed.
	ErrClipStackEmpty = errors.New("rasterx: clip stack is empty")
	// ErrClipDepthLimit is returned when pushing would exceed MaxDepth.
	ErrClipDepthLimit = errors.New("rasterx: clip stack depth limit exceeded")
	// ErrClipPixelLimit is returned when a clip layer is larger than
	// MaxPixels.
	ErrClipPixelLimit = errors.New("rasterx: clip layer exceeds pixel limit")
	// ErrClipMemoryLimit is returned when retaining the coverage for a clip
	// path would exceed MaxMemory.
	ErrClipMemoryLimit = errors.New("rasterx: clip stack exceeds memory limit")
	// ErrEmptyClipPath is returned when a clip callback records no fillable
	// geometry. The push is rejected and the stack is unchanged.
	ErrEmptyClipPath = errors.New("rasterx: empty clip path")
	// ErrNilClipMask is returned when PushClipMask is passed a nil image.
	ErrNilClipMask = errors.New("rasterx: nil clip mask")
	// ErrNilClipAdder is returned when PushClipPath is passed a nil callback.
	ErrNilClipAdder = errors.New("rasterx: nil clip path callback")
)

// ClipLimits bounds the resources a clip stack may use. A field that is zero
// or negative selects the corresponding DefaultMaxClip* value.
type ClipLimits struct {
	// MaxDepth is the maximum number of simultaneously pushed clip layers.
	MaxDepth int
	// MaxPixels is the maximum area, in pixels, of one clip layer.
	MaxPixels int
	// MaxMemory is the maximum total number of bytes of internally
	// rasterized coverage retained by the stack.
	MaxMemory int
}

func (l ClipLimits) withDefaults() ClipLimits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxClipDepth
	}
	if l.MaxPixels <= 0 {
		l.MaxPixels = DefaultMaxClipPixels
	}
	if l.MaxMemory <= 0 {
		l.MaxMemory = DefaultMaxClipMemory
	}
	return l
}

// ClipScanner is implemented by scanners that support nested path and alpha
// mask clipping in addition to the rectangle SetClip method of Scanner.
type ClipScanner interface {
	Scanner

	// PushClipPath appends a clip layer defined by the paths added to add.
	// The callback receives an Adder in the same device coordinate space as
	// paths added to the scanner; wrap it in a MatrixAdder to apply a
	// Matrix2D transform. Curves are flattened and filled with the same
	// rules (implicitly closed subpaths, non-zero winding) as Filler.
	//
	// Returning an error from add cancels the push. The clip stack is left
	// exactly as it was before the call. An add that records no fillable
	// geometry returns ErrEmptyClipPath with the same guarantee.
	PushClipPath(add func(Adder) error) error

	// PushClipMask appends a clip layer from the alpha channel of mask.
	// Mask coordinates are canvas coordinates: the same space clip and fill
	// paths use, so a mask offset relative to the drawn geometry is applied
	// by giving it non-zero Bounds. (The legacy SetClip rectangle stays in
	// source-image space, as before; the two agree whenever Source Offset
	// is zero.) mask is never modified and may be shared by any number of
	// scanners.
	PushClipMask(mask image.Image) error

	// PopClip removes the most recently pushed clip layer. Popping an empty
	// stack returns ErrClipStackEmpty and leaves the stack unchanged.
	PopClip() error

	// ClipDepth returns the number of pushed clip layers.
	ClipDepth() int

	// SetClipLimits changes the resource limits used by subsequent pushes.
	// Existing layers are unaffected.
	SetClipLimits(limits ClipLimits)
}

// clipLayer is one entry of the clip stack. Exactly one of pix and mask is
// used.
type clipLayer struct {
	// pix holds 8 bit coverage for an internally rasterized path. It lives
	// in rasterizer (path) coordinates. When owned the scanner owns the
	// memory and accounts for it; when not owned pix aliases a shared
	// *image.Alpha mask.
	pix    []uint8
	stride int
	rect   image.Rectangle
	owned  bool

	// mask is a read only external alpha source sampled in scanner (canvas)
	// coordinates through At. It is used when pix is nil, and for shared
	// masks of types other than *image.Alpha.
	mask image.Image
}

// coverage returns the 8 bit coverage of the layer for the source pixel at
// (x, y). Every layer lives in scanner (canvas) coordinates, which are the
// source coordinates shifted by the scanner's Offset. Keeping masks and paths
// in the same space means a mask stays glued to the geometry it clips even
// when a non-zero Source Offset is used.
func (l *clipLayer) coverage(x, y int, offset image.Point) uint32 {
	x -= offset.X
	y -= offset.Y
	if l.pix == nil {
		if !(image.Point{x, y}).In(l.mask.Bounds()) {
			return 0
		}
		_, _, _, a := l.mask.At(x, y).RGBA()
		return a >> 8
	}
	if x < l.rect.Min.X || x >= l.rect.Max.X ||
		y < l.rect.Min.Y || y >= l.rect.Max.Y {
		return 0
	}
	i := (y-l.rect.Min.Y)*l.stride + (x - l.rect.Min.X)
	if i < 0 || i >= len(l.pix) {
		return 0
	}
	return uint32(l.pix[i])
}

// clipLevel is a snapshot of the stack taken when a Draw begins. Taking a
// copy keeps a Draw deterministic even if the stack changes later.
type clipLevel struct {
	layers []*clipLayer
	offset image.Point
}

// coverageAt returns the product of all layer coverages at (x, y), rounded
// once per layer. The result is 255 when there are no layers.
func (c *clipLevel) coverageAt(x, y int) uint32 {
	cov := uint32(255)
	for _, l := range c.layers {
		lc := l.coverage(x, y, c.offset)
		if lc >= 255 {
			continue
		}
		cov = (cov*lc + 127) / 255
		if cov == 0 {
			return 0
		}
	}
	return cov
}

// clippedImage wraps a source image with the coverage of a clip stack. It
// multiplies every color channel by the combined coverage, which is correct
// for the premultiplied colors of image.Image. It never modifies the wrapped
// source or any clip mask.
type clippedImage struct {
	src   image.Image
	level clipLevel
}

// ColorModel returns the color model of the clipped image.
func (c *clippedImage) ColorModel() color.Model { return color.RGBA64Model }

// Bounds returns the bounds of the wrapped source.
func (c *clippedImage) Bounds() image.Rectangle { return c.src.Bounds() }

// At returns the source color at x,y scaled by the combined clip coverage.
func (c *clippedImage) At(x, y int) color.Color {
	cov := c.level.coverageAt(x, y)
	if cov == 0 {
		return color.RGBA64{}
	}
	base := c.src.At(x, y)
	if cov >= 255 {
		return base
	}
	r, g, b, a := base.RGBA()
	scale := cov * 257 // 0..0xffff
	return color.RGBA64{
		R: uint16(r * scale / 0xffff),
		G: uint16(g * scale / 0xffff),
		B: uint16(b * scale / 0xffff),
		A: uint16(a * scale / 0xffff),
	}
}

// clipPathBuilder records an Adder path for later rasterization. Recording
// control points instead of flattening immediately keeps a pending push
// cheap and lets an aborted push discard it with no side effects.
type clipPathBuilder struct {
	p Path
}

// Start starts a new curve at the given point.
func (c *clipPathBuilder) Start(a fixed.Point26_6) { c.p.Start(a) }

// Line adds a linear segment to the current curve.
func (c *clipPathBuilder) Line(b fixed.Point26_6) { c.p.Line(b) }

// QuadBezier adds a quadratic segment to the current curve.
func (c *clipPathBuilder) QuadBezier(b, d fixed.Point26_6) { c.p.QuadBezier(b, d) }

// CubeBezier adds a cubic segment to the current curve.
func (c *clipPathBuilder) CubeBezier(b, c2, d fixed.Point26_6) { c.p.CubeBezier(b, c2, d) }

// Stop closes the current subpath when closeLoop is true. Open subpaths are
// still filled implicitly, exactly like Filler.
func (c *clipPathBuilder) Stop(closeLoop bool) { c.p.Stop(closeLoop) }

// pathHasGeometry reports whether a recorded path contains any segment that
// can produce fill coverage. MoveTo and Close alone cannot.
func pathHasGeometry(p Path) bool {
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			i += 3
		case PathLineTo, PathQuadTo, PathCubicTo:
			return true
		case PathClose:
			i++
		default:
			return false
		}
	}
	return false
}

// clipRasterScanner adapts a vector.Rasterizer to the two Scanner methods
// Filler uses while flattening and filling a path. Embedding the interface
// keeps the type small while still satisfying Filler; the other methods are
// never reached.
type clipRasterScanner struct {
	Scanner
	r *vector.Rasterizer
}

// Start starts a new curve at the given point.
func (s *clipRasterScanner) Start(a fixed.Point26_6) {
	s.r.MoveTo(float32(a.X)/64, float32(a.Y)/64)
}

// Line adds a linear segment to the current curve.
func (s *clipRasterScanner) Line(b fixed.Point26_6) {
	s.r.LineTo(float32(b.X)/64, float32(b.Y)/64)
}

// rasterizeClipPath fills p into a new 8 bit coverage buffer at rasterizer
// resolution. Filler is used so curve flattening, winding and implicit
// closing match ordinary scanner drawing exactly.
func rasterizeClipPath(p Path, w, h int) *image.Alpha {
	dst := image.NewAlpha(image.Rect(0, 0, w, h))
	if dst.Rect.Empty() {
		return dst
	}
	rs := vector.NewRasterizer(w, h)
	f := &Filler{Scanner: &clipRasterScanner{r: rs}}
	p.AddTo(f)
	rs.DrawOp = draw.Src
	rs.Draw(dst, dst.Bounds(), image.NewUniform(color.Alpha{A: 255}), image.Point{})
	return dst
}

// SetClipLimits changes the resource limits used by subsequent clip pushes.
// Existing layers are unaffected.
func (s *ScannerGV) SetClipLimits(limits ClipLimits) {
	s.clipsLimit = limits
}

// ClipDepth returns the number of pushed clip layers.
func (s *ScannerGV) ClipDepth() int {
	return len(s.clips)
}

func (s *ScannerGV) limits() ClipLimits {
	return s.clipsLimit.withDefaults()
}

// pushLayer appends a fully prepared layer, honoring the depth limit.
func (s *ScannerGV) pushLayer(l *clipLayer) error {
	if len(s.clips) >= s.limits().MaxDepth {
		return ErrClipDepthLimit
	}
	s.clips = append(s.clips, l)
	if l.owned {
		s.clipsMemUsed += len(l.pix)
	}
	return nil
}

// PushClipPath appends a clip layer defined by the paths added to add.
//
// Paths are in the scanner's device coordinate space, the same space used by
// Filler, Stroker and Dasher, and support MatrixAdder wrappers. The layer is
// committed only if add returns nil and records fillable geometry; otherwise
// the clip stack and the memory accounting are left untouched.
func (s *ScannerGV) PushClipPath(add func(Adder) error) error {
	if add == nil {
		return ErrNilClipAdder
	}
	limits := s.limits()
	if len(s.clips) >= limits.MaxDepth {
		return ErrClipDepthLimit
	}
	b := &clipPathBuilder{}
	if err := add(b); err != nil {
		return err
	}
	if !pathHasGeometry(b.p) {
		return ErrEmptyClipPath
	}
	size := s.r.Size()
	w, h := size.X, size.Y
	if w < 0 || h < 0 || (w != 0 && h > (1<<31-1)/w) {
		return ErrClipPixelLimit
	}
	pixels := w * h
	if pixels > limits.MaxPixels {
		return ErrClipPixelLimit
	}
	// Coverage is one byte per pixel. Check before allocating so a failed
	// push cannot affect the process or the stack.
	if pixels > 0 && (s.clipsMemUsed > limits.MaxMemory-pixels) {
		return ErrClipMemoryLimit
	}
	alpha := rasterizeClipPath(b.p, w, h)
	return s.pushLayer(&clipLayer{
		pix:    alpha.Pix,
		stride: alpha.Stride,
		rect:   alpha.Rect,
		owned:  true,
	})
}

// PushClipMask appends a clip layer from the alpha channel of mask. The mask
// is held read-only and sampled through image.Image, so any image type may be
// used. Mask coordinates are canvas coordinates: they match the coordinates of
// clip and fill paths, so a mask that should be offset from the geometry is
// built with non-zero Bounds. Pixels outside the mask bounds are fully
// clipped.
func (s *ScannerGV) PushClipMask(mask image.Image) error {
	if mask == nil {
		return ErrNilClipMask
	}
	limits := s.limits()
	if len(s.clips) >= limits.MaxDepth {
		return ErrClipDepthLimit
	}
	bounds := mask.Bounds()
	if !bounds.Empty() {
		size := s.r.Size()
		overlap := bounds.Intersect(image.Rect(0, 0, size.X, size.Y))
		ox, oy := overlap.Dx(), overlap.Dy()
		if ox > 0 && oy > 0 && (ox > (1<<31-1)/oy || ox*oy > limits.MaxPixels) {
			return ErrClipPixelLimit
		}
	}
	l := &clipLayer{mask: mask}
	// *image.Alpha already stores exactly what coverage needs and is the
	// common mask representation; read its pixels directly to avoid a
	// per-pixel interface call and color conversion. The buffer is shared
	// and never written.
	if alpha, ok := mask.(*image.Alpha); ok && !alpha.Rect.Empty() {
		l.pix = alpha.Pix
		l.stride = alpha.Stride
		l.rect = alpha.Rect
		l.mask = nil
		// A shared mask's buffer is not owned; it is exempt from the
		// retained-memory limit but still bounded by MaxPixels above.
		l.owned = false
	}
	return s.pushLayer(l)
}

// PopClip removes the most recently pushed clip layer. Popping an empty
// stack returns ErrClipStackEmpty and changes nothing.
func (s *ScannerGV) PopClip() error {
	n := len(s.clips)
	if n == 0 {
		return ErrClipStackEmpty
	}
	l := s.clips[n-1]
	s.clips[n-1] = nil
	s.clips = s.clips[:n-1]
	if l.owned {
		s.clipsMemUsed -= len(l.pix)
		if s.clipsMemUsed < 0 {
			s.clipsMemUsed = 0
		}
	}
	return nil
}

// clipSnapshot copies the active stack for one Draw. The copy prevents a
// source image handed to the rasterizer from seeing later pushes or pops.
func (s *ScannerGV) clipSnapshot() clipLevel {
	layers := make([]*clipLayer, len(s.clips))
	copy(layers, s.clips)
	return clipLevel{layers: layers, offset: s.Offset}
}

// resetClips drops every clip layer. Rasterized coverage is tied to the
// rasterizer bounds, so a bounds change invalidates it.
func (s *ScannerGV) resetClips() {
	s.clips = nil
	s.clipsMemUsed = 0
}
