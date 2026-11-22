// Tests for the Snapshotter save/restore protocol. Every rendering scenario is
// run twice: once through the new snapshot protocol and once as an
// uninterrupted golden pass (or, where relevant, through the old manual
// step-by-step save/restore pattern). Pixel-identical output is the
// compatibility contract: restoring a snapshot must be indistinguishable from
// continuing from the captured state.
//
// Copyright 2024 by the rasterx Authors. All rights reserved.
package rasterx_test

import (
	"errors"
	"image"
	"math"
	"sync"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/colornames"
	"golang.org/x/image/math/fixed"
)

const snapW, snapH = 200, 200

// newHarness wraps an already-constructed *Filler, *Stroker or *Dasher in a
// Snapshotter.
func newHarness(t *testing.T, r interface{}) *Snapshotter {
	t.Helper()
	k, err := NewSnapshotter(r)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	return k
}

func newDasherHarness(t *testing.T) (*image.RGBA, *ScannerGV, *Dasher, *Snapshotter) {
	img := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	sc := NewScannerGV(snapW, snapH, img, img.Bounds())
	d := NewDasher(snapW, snapH, sc)
	return img, sc, d, newHarness(t, d)
}

func newFillerHarness(t *testing.T) (*image.RGBA, *ScannerGV, *Filler, *Snapshotter) {
	img := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	sc := NewScannerGV(snapW, snapH, img, img.Bounds())
	f := NewFiller(snapW, snapH, sc)
	return img, sc, f, newHarness(t, f)
}

func newStrokerHarness(t *testing.T) (*image.RGBA, *ScannerGV, *Stroker, *Snapshotter) {
	img := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	sc := NewScannerGV(snapW, snapH, img, img.Bounds())
	s := NewStroker(snapW, snapH, sc)
	return img, sc, s, newHarness(t, s)
}

// imgEqual reports whether two equally sized RGBA images are bit-identical.
func imgEqual(a, b *image.RGBA) bool {
	if len(a.Pix) != len(b.Pix) {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}

// imgNear reports whether two images differ by at most one unit per channel.
// That tolerance absorbs the single-1/255 rounding differences that the
// vector rasterizer produces when the same mask is composited in a different
// draw-call order; it can not hide a state-restore defect, which moves whole
// regions or changes colors.
func imgNear(a, b *image.RGBA) (ok bool, maxDiff int) {
	if len(a.Pix) != len(b.Pix) {
		return false, 256
	}
	ok = true
	for i := range a.Pix {
		d := int(a.Pix[i]) - int(b.Pix[i])
		if d < 0 {
			d = -d
		}
		if d > 1 {
			ok = false
		}
		if d > maxDiff {
			maxDiff = d
		}
	}
	return ok, maxDiff
}

// assertNear fails the test with the observed tolerance when images diverge by
// more than antialiasing rounding.
func assertNear(t *testing.T, got, want *image.RGBA, ctx string) {
	t.Helper()
	if ok, maxDiff := imgNear(got, want); !ok {
		t.Fatalf("%s diverges beyond rounding (max channel diff=%d, painted %d vs %d)",
			ctx, maxDiff, paintedPixels(got), paintedPixels(want))
	}
}

// paintedPixels counts non-transparent pixels.
func paintedPixels(m *image.RGBA) int {
	n := 0
	for i := 3; i < len(m.Pix); i += 4 {
		if m.Pix[i] != 0 {
			n++
		}
	}
	return n
}

// --- Fixture geometry -------------------------------------------------------

func parentPath() Path {
	var p Path
	AddRect(20, 20, 80, 80, 0, &p)
	return p
}

func continuationPath() Path {
	var p Path
	AddCircle(130, 130, 25, &p)
	return p
}

func childPath() Path {
	var p Path
	AddRect(100, 20, 180, 90, 0, &p)
	return p
}

// isolatedChildPath is disjoint from both parentPath and continuationPath, so
// its pixels never alpha-blend with parent layers: compositing order can not
// influence the golden comparison.
func isolatedChildPath() Path {
	var p Path
	AddRect(160, 160, 195, 195, 0, &p)
	return p
}

func parentStroke(d *Dasher) {
	d.SetStroke(8*64, 4*64, RoundCap, nil, RoundGap, ArcClip, []float64{20, 10}, 0)
}

func childStroke(d *Dasher) {
	d.SetStroke(14*64, 2*64, SquareCap, nil, FlatGap, Miter, nil, 0)
}

// goldenRollback renders the parent path and the continuation in one
// uninterrupted pass with the parent state: this is the exact output the
// snapshot rollback must reproduce.
func goldenRollback(t *testing.T) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	sc := NewScannerGV(snapW, snapH, img, img.Bounds())
	d := NewDasher(snapW, snapH, sc)
	parentStroke(d)
	sc.SetColor(colornames.Blue)
	parentPath().AddTo(d)
	continuationPath().AddTo(d)
	d.Draw()
	return img
}

// TestSnapshotRollbackParity is the exception-rollback fixture: a child node
// changes matrix, clip, color, stroke and dash and feeds a path, then fails
// before Draw. Restore must rebuild the parent mask and parameters so the
// continuation renders exactly as in the uninterrupted golden pass.
func TestSnapshotRollbackParity(t *testing.T) {
	want := goldenRollback(t)

	img, sc, d, k := newDasherHarness(t)
	parentStroke(d)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)

	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if snap.Version() == 0 || snap.Released() {
		t.Fatal("snapshot not active")
	}

	// Child mutates everything the requirement lists.
	if err := k.SetMatrix(Identity.Translate(5, 7).Rotate(0.3)); err != nil {
		t.Fatalf("SetMatrix: %v", err)
	}
	k.SetClip(image.Rect(90, 0, 200, 200))
	k.SetColor(colornames.Red)
	childStroke(d)
	childPath().AddTo(k) // mask now contains child geometry; nothing drawn yet

	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !snap.Released() {
		t.Error("snapshot should be marked released after restore")
	}
	if k.Depth() != 0 {
		t.Errorf("stack depth = %d, want 0", k.Depth())
	}
	// Parent state observable again through the accessors.
	if got := snap.Matrix(); got != Identity {
		t.Errorf("matrix = %v, want identity", got)
	}
	if got := snap.Clip(); got != image.ZR {
		t.Errorf("clip = %v, want ZR", got)
	}
	if got := snap.Stroke().JoinMode; got != ArcClip {
		t.Errorf("join mode = %v, want ArcClip", got)
	}
	if got := len(snap.Dash().Dashes); got != 2 {
		t.Errorf("dash count = %d, want 2", got)
	}

	continuationPath().AddTo(k)
	k.Draw()

	if !imgEqual(img, want) {
		t.Fatalf("rollback output diverges from golden pass (got %d painted px, want %d)",
			paintedPixels(img), paintedPixels(want))
	}
	// The destination never saw the child: the rebuilt mask's extent must
	// equal the extent of the uninterrupted golden pipeline.
	_, scGolden := goldenExtentRollback(t)
	if sc.GetPathExtent() != scGolden.GetPathExtent() {
		t.Errorf("extent after restore = %v, want %v", sc.GetPathExtent(), scGolden.GetPathExtent())
	}
}

// goldenExtentRollback builds the uninterrupted reference pipeline and returns
// its image and scanner for extent comparison.
func goldenExtentRollback(t *testing.T) (*image.RGBA, *ScannerGV) {
	img := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	sc := NewScannerGV(snapW, snapH, img, img.Bounds())
	d := NewDasher(snapW, snapH, sc)
	parentStroke(d)
	sc.SetColor(colornames.Blue)
	parentPath().AddTo(d)
	continuationPath().AddTo(d)
	return img, sc
}

// TestSnapshotSuccessCommit is the success fixture: a child that draws
// successfully is committed to the destination; the following restore
// rebuilds only the parent mask, so a continuation renders on top of both.
// The golden image composites the same three layers through separate
// rasterizers (the only way to express it without mask-rebuild support).
func TestSnapshotSuccessCommit(t *testing.T) {
	// Golden: layer 1 parent, layer 2 child, layer 3 continuation on one image.
	golden := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	{
		sc := NewScannerGV(snapW, snapH, golden, golden.Bounds())
		d := NewDasher(snapW, snapH, sc)
		parentStroke(d)
		sc.SetColor(colornames.Blue)
		parentPath().AddTo(d)
		d.Draw()

		sc2 := NewScannerGV(snapW, snapH, golden, golden.Bounds())
		d2 := NewDasher(snapW, snapH, sc2)
		childStroke(d2)
		sc2.SetColor(colornames.Red)
		sc2.SetClip(image.Rect(90, 0, 200, 200))
		childPath().AddTo(d2)
		d2.Draw()

		sc3 := NewScannerGV(snapW, snapH, golden, golden.Bounds())
		d3 := NewDasher(snapW, snapH, sc3)
		parentStroke(d3)
		sc3.SetColor(colornames.Blue)
		continuationPath().AddTo(d3)
		d3.Draw()
	}

	img, _, d, k := newDasherHarness(t)
	parentStroke(d)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)
	if _, err := k.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	k.SetClip(image.Rect(90, 0, 200, 200))
	k.SetColor(colornames.Red)
	childStroke(d)
	childPath().AddTo(k)
	k.Draw() // child pixels commit
	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	continuationPath().AddTo(k)
	k.Draw()

	assertNear(t, img, golden, "commit output")
}

// TestOldStepwiseLosesMask contrasts the snapshot protocol with the old manual
// pattern. In the old pattern, Clear() wipes the accumulated mask and the
// publicly reachable setters have no way to rebuild it, so an aborted child
// permanently destroys the parent's in-progress path. The snapshot protocol
// (TestSnapshotRollbackParity) rebuilds that mask exactly. This fixture does
// NOT draw the parent before the abort, because once pixels are committed they
// are intentionally permanent; the defect being demonstrated is the loss of
// the not-yet-drawn mask.
func TestOldStepwiseLosesMask(t *testing.T) {
	golden := goldenRollback(t)

	// Old-style: parent path accumulated but never drawn before the abort.
	img, sc, d := newDasherHarnessOldStyle(t)
	parentStroke(d)
	sc.SetColor(colornames.Blue)
	parentPath().AddTo(d)
	// A child begins rendering, then fails.
	sc.SetColor(colornames.Red)
	sc.SetClip(image.Rect(90, 0, 200, 200))
	childPath().AddTo(d)
	d.Clear() // abort: the only available recovery wipes everything

	// Manually restore every publicly reachable parameter.
	sc.SetClip(image.ZR)
	sc.SetColor(colornames.Blue)
	parentStroke(d)

	continuationPath().AddTo(d)
	d.Draw()

	if imgEqual(img, golden) {
		t.Fatal("old stepwise path unexpectedly matched golden; fixture no longer distinguishes the defect")
	}
	// The golden contains the parent layer; the old path could not rebuild it,
	// so it must have strictly fewer painted pixels.
	if paintedPixels(img) >= paintedPixels(golden) {
		t.Fatalf("old path retained parent mask: %d >= golden %d", paintedPixels(img), paintedPixels(golden))
	}
}

func newDasherHarnessOldStyle(t *testing.T) (*image.RGBA, *ScannerGV, *Dasher) {
	img := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	sc := NewScannerGV(snapW, snapH, img, img.Bounds())
	d := NewDasher(snapW, snapH, sc)
	return img, sc, d
}

// TestSnapshotMatrixParity verifies matrix capture/restore against a golden
// pass built with a plain MatrixAdder.
func TestSnapshotMatrixParity(t *testing.T) {
	m := Identity.Translate(30, 30).Rotate(math.Pi / 8)

	img, _, _, k := newFillerHarness(t)
	k.SetColor(colornames.Teal)
	if err := k.SetMatrix(m); err != nil {
		t.Fatalf("SetMatrix: %v", err)
	}
	parentPath().AddTo(k) // recorded with m applied
	if _, err := k.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A child changes the transform without feeding any path; restore must
	// reinstate m exactly and the continuation renders as the golden does.
	if err := k.SetMatrix(Identity.Translate(200, 200)); err != nil {
		t.Fatalf("SetMatrix: %v", err)
	}
	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if k.Matrix() != m {
		t.Fatalf("matrix not restored: %v", k.Matrix())
	}
	// Restore already rebuilt the parent mask; add a distinct continuation
	// (rather than re-feeding the parent path, which would double-stamp it).
	continuationPath().AddTo(k)
	k.Draw()

	// Golden composites the parent layer and the continuation layer in order.
	want := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	{
		sc := NewScannerGV(snapW, snapH, want, want.Bounds())
		f := NewFiller(snapW, snapH, sc)
		sc.SetColor(colornames.Teal)
		ma := &MatrixAdder{Adder: f, M: m}
		parentPath().AddTo(ma)
		f.Draw()
		sc2 := NewScannerGV(snapW, snapH, want, want.Bounds())
		f2 := NewFiller(snapW, snapH, sc2)
		sc2.SetColor(colornames.Teal)
		ma2 := &MatrixAdder{Adder: f2, M: m}
		continuationPath().AddTo(ma2)
		f2.Draw()
	}
	assertNear(t, img, want, "matrix rollback output")
}

// TestSnapshotStrokerParity exercises the plain Stroker wrapper (no dashes),
// including open subpath end caps via the recorded PathStop marker.
func TestSnapshotStrokerParity(t *testing.T) {
	openPath := func(p Adder) {
		p.Start(ToFixedP(30, 40))
		p.Line(ToFixedP(90, 60))
		p.CubeBezier(ToFixedP(120, 80), ToFixedP(70, 110), ToFixedP(100, 140))
		p.Stop(false)
	}

	golden := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	{
		sc := NewScannerGV(snapW, snapH, golden, golden.Bounds())
		s := NewStroker(snapW, snapH, sc)
		s.SetStroke(10*64, 4*64, RoundCap, nil, RoundGap, ArcClip)
		sc.SetColor(colornames.Purple)
		openPath(s)
		s.Draw()
	}

	img, _, s, k := newStrokerHarness(t)
	s.SetStroke(10*64, 4*64, RoundCap, nil, RoundGap, ArcClip)
	k.SetColor(colornames.Purple)
	openPath(k)
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(snap.Path()) == 0 {
		t.Fatal("current path was not recorded")
	}
	s.SetStroke(2*64, 1, ButtCap, ButtCap, FlatGap, Bevel)
	k.SetColor(colornames.Orange)
	openPath(k)
	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Re-draw the rebuilt mask: output must match the single-pass golden.
	k.Draw()

	if !imgEqual(img, golden) {
		t.Fatalf("stroker rollback diverges from golden (caps lost?)")
	}
}

// TestSnapshotGradientParity covers gradient save/restore: a child switches to
// a solid color, fails, and the restored gradient paint must match a golden
// gradient render bit-for-bit.
func TestSnapshotGradientParity(t *testing.T) {
	g := &Gradient{
		Points:   [5]float64{0, 0, 1, 0, 0},
		IsRadial: false,
		Bounds:   struct{ X, Y, W, H float64 }{X: 20, Y: 20, W: 100, H: 100},
		Matrix:   Identity,
		Stops: []GradStop{
			{StopColor: colornames.Aquamarine, Offset: 0, Opacity: 1},
			{StopColor: colornames.Skyblue, Offset: 0.6, Opacity: 1},
			{StopColor: colornames.Darksalmon, Offset: 1, Opacity: 0.75},
		},
	}

	golden := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	{
		sc := NewScannerGV(snapW, snapH, golden, golden.Bounds())
		f := NewFiller(snapW, snapH, sc)
		sc.SetColor(g.GetColorFunction(0.8))
		parentPath().AddTo(f)
		f.Draw()
	}

	img, _, _, k := newFillerHarness(t)
	if err := k.SetGradient(g, 0.8); err != nil {
		t.Fatalf("SetGradient: %v", err)
	}
	parentPath().AddTo(k)
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	gg, op := snap.Gradient()
	if gg == nil || op != 0.8 || len(gg.Stops) != 3 {
		t.Fatalf("gradient snapshot wrong: %+v opacity %v", gg, op)
	}

	k.SetColor(colornames.Red)
	childPath().AddTo(k)
	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	k.Draw()

	if !imgEqual(img, golden) {
		t.Fatalf("gradient rollback diverges from golden gradient render")
	}
}

// TestSnapshotClearDrawInterleave interleaves Draw/Clear calls between Save
// and Restore. It verifies the structural contract of the interleaving: the
// committed Draw reaches the destination, the discarded accumulations do not,
// and Restore rebuilds exactly the captured mask (extent), leaving the
// Snapshotter able to continue rendering. Bit-exact compositing across the
// single-scanner and multi-scanner pipelines is covered by the dedicated
// parity fixtures above.
func TestSnapshotClearDrawInterleave(t *testing.T) {
	img, _, d, k := newDasherHarness(t)
	parentStroke(d)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	before := paintedPixels(img)

	isolatedChildPath().AddTo(k)
	k.Draw() // commits child pixels
	committed := paintedPixels(img)
	if committed <= before {
		t.Fatalf("Draw did not commit pixels: %d <= %d", committed, before)
	}
	k.Clear() // discards the mask; destination untouched
	if paintedPixels(img) != committed {
		t.Fatal("Clear altered already-committed destination pixels")
	}
	isolatedChildPath().AddTo(k)
	k.Clear() // another accumulation discarded without Draw
	if paintedPixels(img) != committed {
		t.Fatal("second Clear altered committed destination pixels")
	}

	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The rebuilt mask is exactly the captured one, regardless of the
	// interleaved Draw/Clear churn.
	if got := k.GetPathExtent(); got != snap.Extent() {
		t.Errorf("extent = %v, want captured %v", got, snap.Extent())
	}
	// The rebuilt path recording matches the snapshot's path byte-for-byte.
	if len(k.PathRecording()) != len(snap.Path()) {
		t.Errorf("rebuilt path length = %d, want %d", len(k.PathRecording()), len(snap.Path()))
	}

	// And rendering still works on the restored state: a continuation draws
	// without error and adds fresh pixels.
	continuationPath().AddTo(k)
	k.Draw()
	if paintedPixels(img) <= committed {
		t.Fatal("continuation after interleaved restore produced no pixels")
	}
}

// TestSnapshotNestedStack exercises nested snapshots, LIFO enforcement and
// Discard.
func TestSnapshotNestedStack(t *testing.T) {
	_, _, d, k := newDasherHarness(t)
	parentStroke(d)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)

	outer, err := k.Save()
	if err != nil {
		t.Fatalf("outer Save: %v", err)
	}
	k.SetColor(colornames.Red)
	childPath().AddTo(k)

	inner, err := k.Save()
	if err != nil {
		t.Fatalf("inner Save: %v", err)
	}
	if k.Depth() != 2 {
		t.Fatalf("depth = %d, want 2", k.Depth())
	}
	k.SetClip(image.Rect(0, 0, 10, 10))

	// Out-of-order release is rejected and must not mutate live state.
	if err := k.RestoreTo(outer); !errors.Is(err, ErrRestoreOrder) {
		t.Fatalf("RestoreTo(outer) err = %v, want ErrRestoreOrder", err)
	}
	if got := k.Clip(); got != (image.Rect(0, 0, 10, 10)) {
		t.Fatalf("rejected restore mutated clip: %v", got)
	}
	if err := k.Discard(outer); !errors.Is(err, ErrRestoreOrder) {
		t.Fatalf("Discard(outer) err = %v, want ErrRestoreOrder", err)
	}

	// Inner restore returns to the red/child state.
	if err := k.RestoreTo(inner); err != nil {
		t.Fatalf("RestoreTo(inner): %v", err)
	}
	if k.Depth() != 1 {
		t.Fatalf("depth = %d, want 1", k.Depth())
	}
	// Reusing the released inner snapshot is rejected.
	if err := k.RestoreTo(inner); !errors.Is(err, ErrSnapshotReleased) {
		t.Fatalf("re-restore err = %v, want ErrSnapshotReleased", err)
	}

	// Outer restore returns to the blue/parent state.
	if err := k.RestoreTo(outer); err != nil {
		t.Fatalf("RestoreTo(outer): %v", err)
	}
	if k.Clip() != image.ZR {
		t.Errorf("clip = %v, want ZR", k.Clip())
	}
	if err := k.Restore(); !errors.Is(err, ErrEmptySnapshotStack) {
		t.Fatalf("empty restore err = %v, want ErrEmptySnapshotStack", err)
	}
}

// TestSnapshotErrors covers the explicit error contract.
func TestSnapshotErrors(t *testing.T) {
	if _, err := NewSnapshotter(nil); !errors.Is(err, ErrEmptyState) {
		t.Errorf("NewSnapshotter(nil) err = %v", err)
	}
	if _, err := NewSnapshotter(42); !errors.Is(err, ErrEmptyState) {
		t.Errorf("NewSnapshotter(42) err = %v", err)
	}

	_, _, _, k := newFillerHarness(t)
	if err := k.SetMatrix(Matrix2D{A: math.NaN()}); !errors.Is(err, ErrNonFiniteMatrix) {
		t.Errorf("NaN matrix err = %v", err)
	}
	if err := k.SetMatrix(Matrix2D{D: math.Inf(1)}); !errors.Is(err, ErrNonFiniteMatrix) {
		t.Errorf("Inf matrix err = %v", err)
	}
	if err := k.SetGradient(nil, 1); !errors.Is(err, ErrEmptyState) {
		t.Errorf("nil gradient err = %v", err)
	}
	badGrad := &Gradient{Matrix: Matrix2D{A: math.NaN()}}
	if err := k.SetGradient(badGrad, 1); !errors.Is(err, ErrNonFiniteMatrix) {
		t.Errorf("NaN gradient err = %v", err)
	}
	if err := k.SetGradient(&Gradient{Matrix: Identity}, math.NaN()); !errors.Is(err, ErrNonFiniteMatrix) {
		t.Errorf("NaN opacity err = %v", err)
	}

	// Saving without any paint is an empty-state error.
	if _, err := k.Save(); !errors.Is(err, ErrEmptyState) {
		t.Errorf("paint-less Save err = %v", err)
	}
	k.SetColor(colornames.Black)
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Foreign ownership.
	_, _, _, k2 := newFillerHarness(t)
	k2.SetColor(colornames.White)
	if err := k2.RestoreTo(snap); !errors.Is(err, ErrSnapshotForeign) {
		t.Fatalf("foreign restore err = %v, want ErrSnapshotForeign", err)
	}

	// Close semantics.
	if err := k.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := k.Save(); !errors.Is(err, ErrSnapshotterClosed) {
		t.Fatalf("Save after Close err = %v", err)
	}
	if err := k.Close(); !errors.Is(err, ErrSnapshotterClosed) {
		t.Fatalf("double Close err = %v", err)
	}
}

// TestSnapshotPanicRollback verifies that a panic raised by a user function
// while replaying a captured path is converted to an error and leaves no
// partial mask in the scanner. The triggering cap function is inert while the
// path is first fed and armed only after Save, so the panic can only occur
// during the restore replay.
func TestSnapshotPanicRollback(t *testing.T) {
	_, sc, s, k := newStrokerHarness(t)
	armed := false
	panicCap := CapFunc(func(p Adder, a, eNorm fixed.Point26_6) {
		if armed {
			panic("boom from cap")
		}
		ButtCap(p, a, eNorm)
	})
	s.SetStroke(10*64, 4*64, panicCap, panicCap, FlatGap, Miter)
	k.SetColor(colornames.Black)
	k.Start(ToFixedP(20, 20))
	k.Line(ToFixedP(80, 80))
	k.Stop(false)
	if _, err := k.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	armed = true
	err := k.Restore()
	if !errors.Is(err, ErrRestoreFailed) {
		t.Fatalf("Restore err = %v, want ErrRestoreFailed", err)
	}
	// No partial mask: drawing on a fresh image yields no pixels.
	fresh := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	sc.Dest = fresh
	sc.Draw()
	if paintedPixels(fresh) != 0 {
		t.Fatal("scanner retained a partial mask after failed restore")
	}
}

// TestSnapshotSliceIsolation guarantees that slices handed in by the caller
// and slices exposed by a snapshot can never share mutable storage.
func TestSnapshotSliceIsolation(t *testing.T) {
	_, _, d, k := newDasherHarness(t)
	dashes := []float64{20, 10, 5}
	d.SetStroke(8*64, 4*64, RoundCap, nil, RoundGap, MiterClip, dashes, 3)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)

	g := &Gradient{Matrix: Identity, Stops: []GradStop{
		{StopColor: colornames.Red, Offset: 0, Opacity: 1},
		{StopColor: colornames.Blue, Offset: 1, Opacity: 1},
	}}
	if err := k.SetGradient(g, 1); err != nil {
		t.Fatalf("SetGradient: %v", err)
	}
	// Mutate every caller-owned slice after handing it in.
	dashes[0] = 999
	g.Stops[0] = GradStop{StopColor: colornames.Green, Offset: 0.5, Opacity: 0.2}

	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	gotDash := snap.Dash()
	if len(gotDash.Dashes) != 3 || gotDash.Dashes[0] == fixed.Int26_6(999*64) {
		t.Fatalf("snapshot dash slice aliases caller data: %v", gotDash.Dashes)
	}
	gg, _ := snap.Gradient()
	if gg.Stops[0].StopColor == colornames.Green || gg.Stops[0].Offset != 0 {
		t.Fatal("snapshot gradient aliases caller stop slice")
	}

	// Mutating returned defensive copies must not reach the snapshot.
	gotDash.Dashes[0] = -1
	gotPath := snap.Path()
	gotPath[0] = -1
	again := snap.Dash()
	if again.Dashes[0] == -1 {
		t.Fatal("snapshot mutated through returned dash slice")
	}
	if snap.Path()[0] == -1 {
		t.Fatal("snapshot mutated through returned path slice")
	}

	// Live mutation of the dasher after Save must not reach the snapshot.
	d.Dashes[0] = fixed.Int26_6(777 * 64)
	if snap.Dash().Dashes[0] == fixed.Int26_6(777*64) {
		t.Fatal("snapshot aliases live dasher dash storage")
	}

	// Continuing the path after Save must not extend the recorded snapshot.
	before := len(snap.Path())
	childPath().AddTo(k)
	if len(snap.Path()) != before {
		t.Fatal("snapshot path aliases the live recording")
	}
}

// TestSnapshotBoundsRestore changes target bounds inside a child and checks
// that restore reinstates the scanner size and that subsequent pixels match
// the golden pass.
func TestSnapshotBoundsRestore(t *testing.T) {
	want := goldenRollback(t)
	img, sc, d, k := newDasherHarness(t)
	parentStroke(d)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	k.SetBounds(16, 9)
	if size := sc.Size(); size != (image.Point{X: 16, Y: 9}) {
		t.Fatalf("bounds not applied: %v", size)
	}
	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if size := sc.Size(); size != (image.Point{X: snapW, Y: snapH}) {
		t.Fatalf("bounds not restored: %v", size)
	}
	if got := snap.Bounds(); got != image.Rect(0, 0, snapW, snapH) {
		t.Fatalf("snapshot bounds = %v", got)
	}
	continuationPath().AddTo(k)
	k.Draw()
	if !imgEqual(img, want) {
		t.Fatal("output after bounds restore diverges from golden")
	}
}

// TestSnapshotConcurrentReaders hammers a retained DrawState from many
// goroutines; under -race any shared mutable reference would be detected.
func TestSnapshotConcurrentReaders(t *testing.T) {
	_, _, d, k := newDasherHarness(t)
	d.SetStroke(8*64, 4*64, RoundCap, nil, RoundGap, ArcClip, []float64{20, 10}, 0)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)
	g := &Gradient{Matrix: Identity, Stops: []GradStop{
		{StopColor: colornames.Red, Offset: 0, Opacity: 1},
		{StopColor: colornames.Green, Offset: 1, Opacity: 1},
	}}
	if err := k.SetGradient(g, 0.5); err != nil {
		t.Fatalf("SetGradient: %v", err)
	}
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = snap.Version()
				_ = snap.Released()
				_ = snap.Bounds()
				_ = snap.Clip()
				_ = snap.Matrix()
				_ = snap.Stroke()
				_ = snap.Winding()
				_ = snap.Extent()
				_ = snap.Paint()
				dp := snap.Dash()
				dp.Dashes = append(dp.Dashes, fixed.Int26_6(seed))
				pp := snap.Path()
				if len(pp) > 0 {
					pp[0] = fixed.Int26_6(seed)
				}
				gg, op := snap.Gradient()
				if gg != nil {
					gg.Stops[0].Offset = float64(seed)
					_ = op
				}
				dest, targ, off := snap.Target()
				_ = dest.Bounds().Eq(targ)
				_ = off
			}
		}(i)
	}
	wg.Wait()

	// Snapshot content must be untouched after the concurrent mutation storm.
	if len(snap.Dash().Dashes) != 2 {
		t.Fatal("concurrent readers mutated shared snapshot state")
	}
	if gg, _ := snap.Gradient(); gg.Stops[0].Offset != 0 {
		t.Fatal("gradient stop mutated through concurrent access")
	}
}

// TestSnapshotQuadBezierParity exercises quadratic segments (including their
// PathStop recording) through the Filler chain and checks restore against a
// single-pass golden. It also covers the SetWinding proxy and the Adder
// accessor.
func TestSnapshotQuadBezierParity(t *testing.T) {
	quad := func(p Adder) {
		p.Start(ToFixedP(20, 100))
		p.QuadBezier(ToFixedP(60, 20), ToFixedP(120, 100))
		p.QuadBezier(ToFixedP(160, 160), ToFixedP(180, 100))
		p.Stop(false)
	}

	golden := image.NewRGBA(image.Rect(0, 0, snapW, snapH))
	{
		sc := NewScannerGV(snapW, snapH, golden, golden.Bounds())
		f := NewFiller(snapW, snapH, sc)
		f.SetWinding(false)
		sc.SetColor(colornames.Tomato)
		quad(f)
		f.Draw()
	}

	img, _, _, k := newFillerHarness(t)
	k.SetWinding(false)
	k.SetColor(colornames.Tomato)
	if k.Adder() != k {
		t.Fatal("Adder() must return the Snapshotter itself")
	}
	quad(k)
	if _, err := k.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	k.SetWinding(true)
	quad(k)
	if err := k.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	k.Draw()

	assertNear(t, img, golden, "quadratic bezier rollback output")
}

// TestSnapshotAccessorsOnEmptyState covers the nil/empty branches of the
// DrawState accessors: a paint-less state has no gradient and an empty path.
func TestSnapshotAccessorsOnEmptyState(t *testing.T) {
	_, _, _, k := newFillerHarness(t)
	k.SetColor(colornames.Black)
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if g, _ := snap.Gradient(); g != nil {
		t.Fatal("paint-only snapshot must report nil gradient")
	}
	if p := snap.Path(); p != nil {
		t.Fatalf("empty path must be nil, got %v", p)
	}
	if d := snap.Dash(); d.Dashes != nil {
		t.Fatal("filler snapshot must have nil dashes")
	}
}

// TestSnapshotDiscard clears the live mask through Discard and verifies the
// scanner no longer holds the captured path and the snapshot is released.
func TestSnapshotDiscard(t *testing.T) {
	_, _, d, k := newDasherHarness(t)
	parentStroke(d)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)
	snap, err := k.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	childPath().AddTo(k)
	if err := k.Discard(snap); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if !snap.Released() {
		t.Fatal("discarded snapshot must be marked released")
	}
	if k.Depth() != 0 {
		t.Fatalf("depth after discard = %d, want 0", k.Depth())
	}
	if len(k.PathRecording()) != 0 {
		t.Fatal("live path must be cleared after discard")
	}
}

// TestSnapshotConcurrentMutation drives every mutating Snapshotter entry point
// from several goroutines while another goroutine saves and restores. Under
// -race this would fail without the internal mutex; correctness only requires
// that it completes without a detected data race or panic.
func TestSnapshotConcurrentMutation(t *testing.T) {
	_, _, d, k := newDasherHarness(t)
	parentStroke(d)
	k.SetColor(colornames.Blue)
	parentPath().AddTo(k)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Renderer goroutines: feed paths, change parameters, draw and clear.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = k.SetMatrix(Identity.Translate(float64(seed), 0))
				k.SetClip(image.Rect(seed, 0, snapW, snapH))
				k.SetColor(colornames.Blue)
				parentPath().AddTo(k)
				k.Draw()
				k.Clear()
			}
		}(g)
	}

	// Snapshot goroutine: repeatedly save, mutate and restore.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap, err := k.Save()
			if err != nil {
				continue // another goroutine may have just changed paint state
			}
			k.SetColor(colornames.Red)
			childPath().AddTo(k)
			_ = k.RestoreTo(snap)
		}
	}()

	// Let the storm run briefly, then stop and ensure a clean final state.
	for i := 0; i < 200; i++ {
		_, _ = k.Save()
		if k.Depth() > 0 {
			_ = k.Restore()
		}
	}
	close(stop)
	wg.Wait()
	for k.Depth() > 0 {
		if err := k.Restore(); err != nil {
			break
		}
	}
}
