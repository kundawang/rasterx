// Drawing state management for rasterx: immutable, read-only snapshots with
// ordered restore.
//
// Editors that render child nodes typically mutate the transform matrix, clip
// rectangle, paint (solid color, ColorFunc or gradient), stroke and dash
// parameters around a shared rasterizer. On failure those mutations used to
// leak back into the parent node because every parameter had to be saved and
// reset by hand. The Snapshotter in this file replaces that pattern with an
// explicit save/restore protocol:
//
//	s, err := keeper.Save()
//	if err != nil { return err }
//	keeper.SetMatrix(childMatrix)
//	if err := renderChild(keeper); err != nil {
//	    // Roll back every captured parameter, the accumulated path and the
//	    // scanner mask in one ordered step; no partial state survives.
//	    return keeper.Restore()
//	}
//	return keeper.Restore()
//
// A DrawState is fully immutable: every slice it references is an independent
// deep copy, so snapshots are safe to retain and read from other goroutines
// and can never alias the caller's slices or the live rasterizer.
//
// Copyright 2024 by the rasterx Authors. All rights reserved.

package rasterx

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"math"
	"sync"

	"golang.org/x/image/math/fixed"
)

// snapshotVersion is bumped whenever the on-disk/in-memory layout of DrawState
// changes incompatibly. A DrawState produced by a different Snapshotter
// generation is rejected on restore instead of being silently misapplied.
const snapshotVersion uint32 = 1

// Error values returned by the snapshot save/restore protocol. Callers can
// use errors.Is to distinguish the failure class; wrapped errors preserve the
// underlying cause.
var (
	// ErrEmptyState is returned when a Snapshotter is constructed without a
	// rasterizer or asked to restore a snapshot that carries no paint.
	ErrEmptyState = errors.New("rasterx: empty or uninitialized drawing state")
	// ErrEmptySnapshotStack is returned by Restore when no snapshot is active.
	ErrEmptySnapshotStack = errors.New("rasterx: snapshot stack is empty")
	// ErrSnapshotVersion is returned when a DrawState was produced by an
	// incompatible version of this package.
	ErrSnapshotVersion = errors.New("rasterx: incompatible snapshot version")
	// ErrSnapshotForeign is returned when a DrawState is handed to a
	// Snapshotter other than the one that created it.
	ErrSnapshotForeign = errors.New("rasterx: snapshot belongs to another Snapshotter")
	// ErrSnapshotReleased is returned when a DrawState has already been
	// restored or discarded and is therefore no longer active.
	ErrSnapshotReleased = errors.New("rasterx: snapshot has already been released")
	// ErrNonFiniteMatrix is returned when a Matrix2D contains a NaN or
	// infinite component. Such a matrix can not be applied to fixed-point
	// coordinates.
	ErrNonFiniteMatrix = errors.New("rasterx: matrix contains a non-finite component")
	// ErrRestoreOrder is returned when RestoreTo/Discard is asked for a
	// snapshot that is not on top of the stack. Snapshots must be released in
	// last-in-first-out order.
	ErrRestoreOrder = errors.New("rasterx: snapshots must be restored in LIFO order")
	// ErrSnapshotterClosed is returned by any mutating call after Close.
	ErrSnapshotterClosed = errors.New("rasterx: Snapshotter has been closed")
	// ErrRestoreFailed wraps a panic raised while replaying a captured path so
	// that a failing cap, gap or color function can never leave a partial
	// path in the scanner.
	ErrRestoreFailed = errors.New("rasterx: state restore failed")
)

// StrokeState is the read-only view of the stroking parameters captured in a
// DrawState. Function values are copied as values; the functions themselves
// are treated as immutable code, like the package-level CapFunc/GapFunc
// variables.
type StrokeState struct {
	U, MLimit  fixed.Int26_6
	CapL, CapT CapFunc
	JoinGap    GapFunc
	JoinMode   JoinMode
}

// DashState is the read-only view of the dashing parameters captured in a
// DrawState. The Dashes slice is an independent copy.
type DashState struct {
	Dashes     []fixed.Int26_6
	DashOffset fixed.Int26_6
}

// DrawState is an immutable snapshot of every drawing parameter that can be
// mutated while a node subtree is rendered. Instances are created only by
// Snapshotter.Save and must be handed back to the same Snapshotter to be
// restored. All accessors return values or defensive copies, so a DrawState is
// safe to read concurrently from multiple goroutines.
type DrawState struct {
	version  uint32
	seq      uint64
	owner    *Snapshotter
	released bool

	// Target image and scanner parameters.
	dest   draw.Image
	targ   image.Rectangle
	offset image.Point
	bounds image.Rectangle
	clip   image.Rectangle
	paint  interface{} // color.Color or ColorFunc at capture time
	// winding is remembered through Snapshotter.SetWinding; ScannerGV ignores
	// it but custom Scanner implementations may not.
	winding bool

	// Transform applied to every incoming point.
	matrix Matrix2D

	// Paint supplied as a gradient, together with the opacity it was built
	// with. hasGradient is false when a plain color/ColorFunc is in use.
	hasGradient     bool
	gradient        *Gradient
	gradientOpacity float64

	stroke StrokeState
	dash   DashState

	// Current path in user (pre-matrix) coordinates and the scanner extent it
	// produced. The path slice is owned exclusively by this DrawState.
	path   Path
	extent fixed.Rectangle26_6
}

// Version reports the snapshot layout version of this DrawState.
func (s *DrawState) Version() uint32 { return s.version }

// Released reports whether the snapshot has already been restored or
// discarded.
func (s *DrawState) Released() bool { return s.released }

// Bounds reports the rasterizer width/height captured in the snapshot.
func (s *DrawState) Bounds() image.Rectangle { return s.bounds }

// Clip reports the clipping rectangle captured in the snapshot. image.ZR means
// clipping is disabled.
func (s *DrawState) Clip() image.Rectangle { return s.clip }

// Matrix reports a copy of the transform matrix captured in the snapshot.
func (s *DrawState) Matrix() Matrix2D { return s.matrix }

// Stroke reports a copy of the stroking parameters captured in the snapshot.
func (s *DrawState) Stroke() StrokeState { return s.stroke }

// Dash reports a copy of the dash parameters. The returned slice is a
// defensive copy and may be modified freely by the caller.
func (s *DrawState) Dash() DashState {
	out := s.dash
	if s.dash.Dashes != nil {
		out.Dashes = append([]fixed.Int26_6(nil), s.dash.Dashes...)
	}
	return out
}

// Gradient returns a deep copy of the captured gradient and its opacity, or
// nil when the snapshot does not use a gradient.
func (s *DrawState) Gradient() (g *Gradient, opacity float64) {
	if !s.hasGradient {
		return nil, 0
	}
	return cloneGradient(s.gradient), s.gradientOpacity
}

// Paint returns the captured paint, which is either a color.Color or a
// ColorFunc.
func (s *DrawState) Paint() interface{} { return s.paint }

// Winding reports the captured winding rule.
func (s *DrawState) Winding() bool { return s.winding }

// Path returns a defensive copy of the captured user-space path.
func (s *DrawState) Path() Path {
	if s.path == nil {
		return nil
	}
	return append(Path(nil), s.path...)
}

// Extent reports the scanner path extent captured alongside the path.
func (s *DrawState) Extent() fixed.Rectangle26_6 { return s.extent }

// Target reports the destination image, target rectangle and draw offset
// captured in the snapshot. The image itself is returned by reference (it is
// the render target, not owned state); its pixels are never modified by
// save/restore.
func (s *DrawState) Target() (dest draw.Image, targ image.Rectangle, offset image.Point) {
	return s.dest, s.targ, s.offset
}

// Snapshotter owns the live rasterizer and the stack of active DrawStates. It
// implements the Adder interface: feed paths through the Snapshotter rather
// than the underlying Filler/Stroker/Dasher so that the current path can be
// recorded and replayed on restore. Path feeding is single-goroutine, exactly
// as it is for the underlying Scanner; Save/Restore/stack operations are
// internally synchronized and retained DrawStates are immutable and safe to
// read from any goroutine.
type Snapshotter struct {
	mu sync.Mutex

	filler  *Filler
	stroker *Stroker
	dasher  *Dasher
	scanner Scanner

	// Live parameter cache. For ScannerGV these are also read back directly
	// from the scanner on Save; the cache keeps generic Scanner
	// implementations working even though the Scanner interface has no
	// getters.
	paint     interface{}
	clip      image.Rectangle
	winding   bool
	boundsW   int
	boundsH   int
	matrix    Matrix2D
	gradient  *Gradient
	gradAlpha float64

	// path is the live user-space recording of everything fed through the
	// Adder methods since the last restore.
	path     Path
	stack    []*DrawState
	seq      uint64
	closed   bool
	paintSet bool // SetColor/SetGradient was called explicitly
}

// NewSnapshotter wraps an existing *Filler, *Stroker or *Dasher. The concrete
// type is remembered so that stroke, dash and path state can all be captured.
// Passing nil or an unsupported rasterizer returns ErrEmptyState.
func NewSnapshotter(rasterizer interface{}) (*Snapshotter, error) {
	k := &Snapshotter{
		matrix:  Identity,
		winding: true, // matches the NewFiller/NewStroker/NewDasher defaults
	}
	switch r := rasterizer.(type) {
	case *Dasher:
		if r == nil {
			return nil, ErrEmptyState
		}
		k.dasher = r
		k.stroker = &r.Stroker
		k.filler = &r.Filler
	case *Stroker:
		if r == nil {
			return nil, ErrEmptyState
		}
		k.stroker = r
		k.filler = &r.Filler
	case *Filler:
		if r == nil {
			return nil, ErrEmptyState
		}
		k.filler = r
	default:
		return nil, fmt.Errorf("%w: unsupported rasterizer type %T", ErrEmptyState, rasterizer)
	}
	k.scanner = k.filler.Scanner
	if k.scanner == nil {
		return nil, ErrEmptyState
	}
	if gv, ok := k.scanner.(*ScannerGV); ok {
		k.boundsW, k.boundsH = gv.r.Size().X, gv.r.Size().Y
	}
	return k, nil
}

// chain returns the most-derived Adder of the wrapped rasterizer so that path
// commands reach stroking/dashing code when it is in use.
func (k *Snapshotter) chain() Adder {
	switch {
	case k.dasher != nil:
		return k.dasher
	case k.stroker != nil:
		return k.stroker
	default:
		return k.filler
	}
}

// Close releases every active snapshot. After Close the Snapshotter can not be
// used; the underlying rasterizer is left cleared so no scanner mask or
// recorded path survives.
func (k *Snapshotter) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return ErrSnapshotterClosed
	}
	k.closed = true
	for _, s := range k.stack {
		s.released = true
		s.path = nil
		s.dash.Dashes = nil
	}
	k.stack = nil
	k.path = nil
	k.gradient = nil
	k.filler.Clear()
	return nil
}

// SetMatrix stores the matrix applied to every subsequently fed point. A
// matrix containing NaN or infinite components is rejected with
// ErrNonFiniteMatrix and the live matrix is left unchanged.
func (k *Snapshotter) SetMatrix(m Matrix2D) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return ErrSnapshotterClosed
	}
	if !finiteMatrix(m) {
		return ErrNonFiniteMatrix
	}
	k.matrix = m
	return nil
}

// Matrix returns the currently active transform matrix.
func (k *Snapshotter) Matrix() Matrix2D {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.matrix
}

// SetColor proxies Scanner.SetColor and remembers the paint so it can be
// captured for generic Scanner implementations.
func (k *Snapshotter) SetColor(clr interface{}) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.paintSet = true
	k.paint = clr
	k.gradient = nil
	k.scanner.SetColor(clr)
}

// SetGradient installs a gradient paint. The gradient is deep-copied, so later
// mutations of the caller's value do not affect the Snapshotter. A non-finite
// gradient matrix is rejected.
func (k *Snapshotter) SetGradient(g *Gradient, opacity float64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return ErrSnapshotterClosed
	}
	if g == nil {
		return ErrEmptyState
	}
	if !finiteMatrix(g.Matrix) {
		return ErrNonFiniteMatrix
	}
	if math.IsNaN(opacity) || math.IsInf(opacity, 0) {
		return fmt.Errorf("%w: gradient opacity is not finite", ErrNonFiniteMatrix)
	}
	clone := cloneGradient(g)
	k.gradient = clone
	k.gradAlpha = opacity
	k.paintSet = true
	k.paint = clone.GetColorFunction(opacity)
	k.scanner.SetColor(k.paint)
	return nil
}

// SetClip proxies Scanner.SetClip and remembers the clip rectangle.
func (k *Snapshotter) SetClip(rect image.Rectangle) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.clip = rect
	k.scanner.SetClip(rect)
}

// SetWinding proxies Scanner.SetWinding and remembers the rule.
func (k *Snapshotter) SetWinding(useNonZeroWinding bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.winding = useNonZeroWinding
	k.scanner.SetWinding(useNonZeroWinding)
}

// SetBounds proxies Filler.SetBounds and remembers the clamped dimensions.
func (k *Snapshotter) SetBounds(width, height int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	k.boundsW, k.boundsH = width, height
	k.filler.SetBounds(width, height)
}

// Draw proxies the scanner's Draw call.
func (k *Snapshotter) Draw() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.scanner.Draw()
}

// Clear proxies Filler.Clear, dropping both the scanner mask and the live
// path recording so the two can never diverge.
func (k *Snapshotter) Clear() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.path = nil
	k.filler.Clear()
}

// GetPathExtent proxies the scanner's path extent.
func (k *Snapshotter) GetPathExtent() fixed.Rectangle26_6 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.scanner.GetPathExtent()
}

// PathRecording returns a defensive copy of the live user-space path recorded
// since the last restore. It is intended for tests and diagnostics.
func (k *Snapshotter) PathRecording() Path {
	k.mu.Lock()
	defer k.mu.Unlock()
	return clonePath(k.path)
}

// Clip returns the currently active clip rectangle (image.ZR if disabled).
func (k *Snapshotter) Clip() image.Rectangle {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.clip
}

// Adder returns the Adder that path commands should be sent to. It is currently
// the Snapshotter itself.
func (k *Snapshotter) Adder() Adder { return k }

// Start records a raw MoveTo and forwards the matrix-transformed point.
func (k *Snapshotter) Start(a fixed.Point26_6) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.path.Start(a)
	k.chain().Start(k.matrix.TFixed(a))
}

// Line records a raw LineTo and forwards the matrix-transformed point.
func (k *Snapshotter) Line(b fixed.Point26_6) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.path.Line(b)
	k.chain().Line(k.matrix.TFixed(b))
}

// QuadBezier records a raw quadratic segment and forwards the transformed
// control points.
func (k *Snapshotter) QuadBezier(b, c fixed.Point26_6) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.path.QuadBezier(b, c)
	k.chain().QuadBezier(k.matrix.TFixed(b), k.matrix.TFixed(c))
}

// CubeBezier records a raw cubic segment and forwards the transformed control
// points.
func (k *Snapshotter) CubeBezier(b, c, d fixed.Point26_6) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.path.CubeBezier(b, c, d)
	k.chain().CubeBezier(k.matrix.TFixed(b), k.matrix.TFixed(c), k.matrix.TFixed(d))
}

// Stop records the explicit end-of-subpath marker (open or closed) and
// forwards it. Recording it separately is what allows open stroked subpaths and
// their end caps to be replayed identically.
func (k *Snapshotter) Stop(closeLoop bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.path = append(k.path, fixed.Int26_6(PathStop), b2i(closeLoop))
	k.chain().Stop(closeLoop)
}

// b2i converts the close flag to the fixed-int encoding used by Path.
func b2i(b bool) fixed.Int26_6 {
	if b {
		return 1
	}
	return 0
}

// Save captures the complete current drawing state as an immutable DrawState
// and pushes it on the snapshot stack. The live rasterizer is left untouched.
func (k *Snapshotter) Save() (*DrawState, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return nil, ErrSnapshotterClosed
	}
	if !finiteMatrix(k.matrix) {
		return nil, ErrNonFiniteMatrix
	}
	if k.gradient != nil && !finiteMatrix(k.gradient.Matrix) {
		return nil, fmt.Errorf("%w: gradient matrix", ErrNonFiniteMatrix)
	}

	s := &DrawState{
		version: snapshotVersion,
		seq:     k.seq,
		owner:   k,
		matrix:  k.matrix,
		winding: k.winding,
		clip:    k.clip,
		paint:   k.paint,
		path:    clonePath(k.path),
		extent:  k.scanner.GetPathExtent(),
	}
	k.seq++

	// Target/bounds/paint are read back from the concrete scanner when
	// possible so that callers mutating ScannerGV directly are still covered.
	switch sc := k.scanner.(type) {
	case *ScannerGV:
		s.dest = sc.Dest
		s.targ = sc.Targ
		s.offset = sc.Offset
		s.clip = sc.clipImage.clip
		size := sc.r.Size()
		s.bounds = image.Rect(0, 0, size.X, size.Y)
		if sc.Source == &sc.clipImage.ColorFuncImage.Uniform {
			s.paint = sc.clipImage.ColorFuncImage.Uniform.C
		} else {
			s.paint = sc.clipImage.ColorFuncImage.colorFunc
		}
	default:
		s.bounds = image.Rect(0, 0, k.boundsW, k.boundsH)
	}
	if s.paint == nil || !k.paintSet {
		return nil, fmt.Errorf("%w: no paint has been set on the scanner", ErrEmptyState)
	}

	if k.gradient != nil {
		s.hasGradient = true
		s.gradient = cloneGradient(k.gradient)
		s.gradientOpacity = k.gradAlpha
	}

	k.captureStroke(s)
	k.stack = append(k.stack, s)
	return s, nil
}

// captureStroke fills the stroke/dash portions of a DrawState from the live
// rasterizer. All fields are value-copied; the dash slice is deep-copied.
func (k *Snapshotter) captureStroke(s *DrawState) {
	if k.stroker != nil {
		s.stroke = StrokeState{
			U:        k.stroker.u,
			MLimit:   k.stroker.mLimit,
			CapL:     k.stroker.CapL,
			CapT:     k.stroker.CapT,
			JoinGap:  k.stroker.JoinGap,
			JoinMode: k.stroker.JoinMode,
		}
	}
	if k.dasher != nil {
		s.dash = DashState{DashOffset: k.dasher.DashOffset}
		if len(k.dasher.Dashes) > 0 {
			s.dash.Dashes = append([]fixed.Int26_6(nil), k.dasher.Dashes...)
		}
	}
}

// Restore pops the most recently saved snapshot and reapplies it. The
// rasterizer mask is rebuilt from the captured path, so pixels rendered after
// Restore are identical to continuing from the captured state.
func (k *Snapshotter) Restore() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return ErrSnapshotterClosed
	}
	if len(k.stack) == 0 {
		return ErrEmptySnapshotStack
	}
	return k.applyLocked(k.stack[len(k.stack)-1])
}

// RestoreTo restores a specific snapshot. Snapshots must be released in LIFO
// order: a snapshot buried under another active snapshot is rejected with
// ErrRestoreOrder instead of corrupting the nested state.
func (k *Snapshotter) RestoreTo(s *DrawState) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return ErrSnapshotterClosed
	}
	if err := k.checkUsable(s); err != nil {
		return err
	}
	if s != k.stack[len(k.stack)-1] {
		return fmt.Errorf("%w: snapshot seq=%d is not on top of a %d-deep stack",
			ErrRestoreOrder, s.seq, len(k.stack))
	}
	return k.applyLocked(s)
}

// Discard drops a snapshot without reapplying it. The same LIFO and ownership
// rules as RestoreTo apply; the live rasterizer is cleared.
func (k *Snapshotter) Discard(s *DrawState) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return ErrSnapshotterClosed
	}
	if err := k.checkUsable(s); err != nil {
		return err
	}
	top := k.stack[len(k.stack)-1]
	if s != top {
		return fmt.Errorf("%w: snapshot seq=%d is not on top", ErrRestoreOrder, s.seq)
	}
	k.pop(top)
	k.filler.Clear()
	k.path = nil
	return nil
}

// Depth reports the number of currently active snapshots.
func (k *Snapshotter) Depth() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.stack)
}

// checkUsable validates everything that can fail before any rasterizer state
// is touched, so a rejected restore never mutates the live state.
func (k *Snapshotter) checkUsable(s *DrawState) error {
	if s == nil {
		return fmt.Errorf("%w: nil snapshot", ErrEmptyState)
	}
	if s.version != snapshotVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrSnapshotVersion, s.version, snapshotVersion)
	}
	if s.owner != k {
		return ErrSnapshotForeign
	}
	if s.released {
		return ErrSnapshotReleased
	}
	if !finiteMatrix(s.matrix) {
		return fmt.Errorf("%w: transform", ErrNonFiniteMatrix)
	}
	if s.hasGradient && (s.gradient == nil || !finiteMatrix(s.gradient.Matrix)) {
		return fmt.Errorf("%w: gradient transform", ErrNonFiniteMatrix)
	}
	return nil
}

// applyLocked re-applies a validated snapshot. A panic raised by a user
// function while replaying the path is converted to ErrRestoreFailed and the
// scanner is cleared, guaranteeing that no partial mask can survive.
func (k *Snapshotter) applyLocked(s *DrawState) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			k.filler.Clear()
			k.path = nil
			err = fmt.Errorf("%w: %v", ErrRestoreFailed, rec)
		}
	}()

	// 1. Transform and rasterizer geometry. SetBounds resets the vector
	//    rasterizer and clears any mask accumulated since the snapshot.
	k.matrix = s.matrix
	k.scanner.SetWinding(s.winding)
	k.filler.SetBounds(s.bounds.Dx(), s.bounds.Dy())
	k.boundsW, k.boundsH = s.bounds.Dx(), s.bounds.Dy()

	// 2. Target image (ScannerGV only; generic scanners manage their own
	//    destination).
	if gv, ok := k.scanner.(*ScannerGV); ok {
		gv.Dest = s.dest
		gv.Targ = s.targ
		gv.Offset = s.offset
	}

	// 3. Paint and clip. The clip is first disabled so SetColor builds the
	//    un-clipped source exactly as it was captured; SetClip then re-wraps
	//    it into a ClipImage when the captured clip is active (mirroring
	//    ScannerGV.SetColor/SetClip).
	k.scanner.SetClip(image.ZR)
	k.clip = image.ZR
	k.scanner.SetColor(s.paint)
	k.paint = s.paint
	if s.clip != image.ZR {
		k.scanner.SetClip(s.clip)
		k.clip = s.clip
	}
	if s.hasGradient {
		k.gradient = cloneGradient(s.gradient)
		k.gradAlpha = s.gradientOpacity
	} else {
		k.gradient = nil
	}

	// 4. Stroke parameters (exact field values, bypassing SetStroke
	//    normalization) and dash pattern.
	k.applyStroke(s)

	// 5. Rebuild the recorded path from user coordinates, re-applying the
	//    restored matrix through the same chain. This deterministically
	//    regenerates Filler/Stroker/Dasher progress state and the scanner mask
	//    (including path extent), giving pixel-identical output.
	k.path = clonePath(s.path)
	k.replay(s.path)

	k.pop(s)
	return nil
}

// applyStroke restores stroke and dash fields directly so that the captured
// values, including SetStroke-normalized defaults, survive a round trip.
func (k *Snapshotter) applyStroke(s *DrawState) {
	if k.stroker != nil {
		k.stroker.u = s.stroke.U
		k.stroker.mLimit = s.stroke.MLimit
		k.stroker.CapL = s.stroke.CapL
		k.stroker.CapT = s.stroke.CapT
		k.stroker.JoinGap = s.stroke.JoinGap
		k.stroker.JoinMode = s.stroke.JoinMode
		// Per-path stroke progress is rebuilt by replay; reset the flags that
		// Start/Stop rely on so a half-open stroke can not leak.
		k.stroker.inStroke = false
		k.filler.a = fixed.Point26_6{}
		k.filler.first = fixed.Point26_6{}
	}
	if k.dasher != nil {
		k.dasher.Dashes = k.dasher.Dashes[:0]
		if len(s.dash.Dashes) > 0 {
			k.dasher.Dashes = append(k.dasher.Dashes, s.dash.Dashes...)
		}
		k.dasher.DashOffset = s.dash.DashOffset
		if len(k.dasher.Dashes) == 0 {
			k.dasher.sgm = &k.dasher.Stroker
		} else {
			k.dasher.sgm = k.dasher
		}
	}
}

// replay feeds a recorded user-space path through the active chain. It mirrors
// Path.AddTo but also understands the PathStop marker, so open subpaths and
// their end caps are reproduced exactly. The matrix is applied at this layer,
// matching the Snapshotter's Adder methods.
func (k *Snapshotter) replay(p Path) {
	chain := k.chain()
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			chain.Stop(false)
			a := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			chain.Start(k.matrix.TFixed(a))
			i += 3
		case PathLineTo:
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			chain.Line(k.matrix.TFixed(b))
			i += 3
		case PathQuadTo:
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			chain.QuadBezier(k.matrix.TFixed(b), k.matrix.TFixed(c))
			i += 5
		case PathCubicTo:
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			d := fixed.Point26_6{X: p[i+5], Y: p[i+6]}
			chain.CubeBezier(k.matrix.TFixed(b), k.matrix.TFixed(c), k.matrix.TFixed(d))
			i += 7
		case PathClose:
			chain.Stop(true)
			i++
		case PathStop:
			chain.Stop(p[i+1] != 0)
			i += 2
		default:
			panic(fmt.Sprintf("rasterx: bad path command %d at index %d", p[i], i))
		}
	}
}

// pop marks the snapshot released and removes it from the stack.
func (k *Snapshotter) pop(s *DrawState) {
	s.released = true
	last := len(k.stack) - 1
	k.stack = k.stack[:last]
}

// clonePath returns an independent copy of a path so that snapshots can never
// alias the live recording or a caller-owned slice.
func clonePath(p Path) Path {
	if len(p) == 0 {
		return nil
	}
	out := make(Path, len(p))
	copy(out, p)
	return out
}

// cloneGradient returns an independent copy of a gradient, including its stop
// slice. Color values are immutable value types and copied by interface value.
func cloneGradient(g *Gradient) *Gradient {
	if g == nil {
		return nil
	}
	cp := *g
	if g.Stops != nil {
		cp.Stops = append([]GradStop(nil), g.Stops...)
	}
	return &cp
}

// finiteMatrix reports whether all six components of m are finite numbers.
func finiteMatrix(m Matrix2D) bool {
	for _, v := range [6]float64{m.A, m.B, m.C, m.D, m.E, m.F} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
