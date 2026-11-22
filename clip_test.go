// Copyright 2026 by the rasterx Authors. All rights reserved.

package rasterx_test

import (
	"errors"
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
)

var _ ClipScanner = (*ScannerGV)(nil)

// refShape is an axis aligned ellipse used as an independent, analytic alpha
// reference. Coverage is integrated exactly along x and with midpoint
// sampling along y, so it shares no code or algorithm with the vector
// rasterizer under test.
type refShape struct {
	cx, cy, rx, ry float64
}

const refSubRows = 512

// cov returns the fractional area of pixel (px, py) inside the shape.
func (s refShape) cov(px, py int) float64 {
	var sum float64
	for k := 0; k < refSubRows; k++ {
		sy := float64(py) + (float64(k)+0.5)/refSubRows
		t := (sy - s.cy) / s.ry
		if t <= -1 || t >= 1 {
			continue
		}
		hw := s.rx * math.Sqrt(1-t*t)
		lo := math.Max(s.cx-hw, float64(px))
		hi := math.Min(s.cx+hw, float64(px+1))
		if hi > lo {
			sum += hi - lo
		}
	}
	return sum / refSubRows
}

// u8 quantizes coverage the way an 8 bit alpha buffer does.
func u8(c float64) int {
	v := int(c * 65535 / 256)
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func newScene(w, h int) (*image.RGBA, *ScannerGV, *Filler) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := NewScannerGV(w, h, img, img.Bounds())
	f := NewFiller(w, h, s)
	s.SetColor(color.RGBA{255, 0, 0, 255})
	return img, s, f
}

// renderClip draws a full-scanner rectangle through the given pushes on a
// fresh destination and returns the image.
func renderClip(w, h int, pushes ...func(*ScannerGV) error) *image.RGBA {
	img, s, f := newScene(w, h)
	for _, p := range pushes {
		if err := p(s); err != nil {
			panic(err)
		}
	}
	addFullRect(w, h, f)
	f.Draw()
	return img
}

// addFullRect records a rectangle covering the whole rasterizer.
func addFullRect(w, h int, a Adder) {
	var p Path
	p.Start(ToFixedP(0, 0))
	p.Line(ToFixedP(float64(w), 0))
	p.Line(ToFixedP(float64(w), float64(h)))
	p.Line(ToFixedP(0, float64(h)))
	p.Stop(true)
	p.AddTo(a)
}

func addEllipse(cx, cy, rx, ry float64, a Adder) {
	AddEllipse(cx, cy, rx, ry, 0, a)
}

func circlePush(cx, cy, rx, ry float64) func(*ScannerGV) error {
	return func(s *ScannerGV) error {
		return s.PushClipPath(func(a Adder) error {
			addEllipse(cx, cy, rx, ry, a)
			return nil
		})
	}
}

// checkAgainstReference compares the alpha and red channels of the rendered
// image with the analytic reference, failing on differences above tol.
// Structural channels must be zero because the source is pure red.
func checkAgainstReference(t *testing.T, img *image.RGBA, tol int, shape refShape) {
	t.Helper()
	for y := 0; y < img.Rect.Dy(); y++ {
		for x := 0; x < img.Rect.Dx(); x++ {
			want := u8(shape.cov(x, y))
			got := img.RGBAAt(x, y)
			if abs(int(got.R)-want) > tol || abs(int(got.A)-want) > tol ||
				got.G != 0 || got.B != 0 {
				t.Fatalf("pixel (%d,%d) = %v, analytic reference alpha %d (tol %d)",
					x, y, got, want, tol)
			}
		}
	}
}

// TestCompositeNested is the independent alpha reference for a nested stack:
// the coverage of a stack of layers must equal the rounded product of the
// coverage each layer has on its own. The single layers are themselves
// checked against the analytic ellipse reference, so this validates the
// multiplication rule without sharing any code with the implementation.
func TestCompositeNested(t *testing.T) {
	const w, h = 128, 128
	outer := refShape{cx: 64, cy: 64, rx: 52, ry: 52}
	inner := refShape{cx: 72, cy: 56, rx: 38, ry: 30}

	imgA := renderClip(w, h, circlePush(outer.cx, outer.cy, outer.rx, outer.ry))
	imgB := renderClip(w, h, circlePush(inner.cx, inner.cy, inner.rx, inner.ry))
	imgAB := renderClip(w, h,
		circlePush(outer.cx, outer.cy, outer.rx, outer.ry),
		circlePush(inner.cx, inner.cy, inner.rx, inner.ry))

	checkAgainstReference(t, imgA, 9, outer)
	checkAgainstReference(t, imgB, 9, inner)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := int(imgA.RGBAAt(x, y).A)
			b := int(imgB.RGBAAt(x, y).A)
			want := (a*b + 127) / 255
			got := int(imgAB.RGBAAt(x, y).A)
			if got != want {
				t.Fatalf("nested coverage at (%d,%d) = %d, product of independent layers = %d (%d*%d)",
					x, y, got, want, a, b)
			}
		}
	}

	// The clipped red channel must equal the clipped alpha because the
	// source is a premultiplied opaque red.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := imgAB.RGBAAt(x, y)
			if c.R != c.A || c.G != 0 || c.B != 0 {
				t.Fatalf("nested pixel (%d,%d) = %v, want premultiplied red", x, y, c)
			}
		}
	}
}

// TestClipMatrix2D verifies clip paths pushed through a MatrixAdder. A 90
// degree rotation maps the (rx,ry) ellipse onto (ry,rx), which keeps the
// analytic reference independent of the matrix code.
func TestClipMatrix2D(t *testing.T) {
	const w, h = 128, 128
	cx, cy, rx, ry := 64.0, 60.0, 44.0, 24.0
	rotated := refShape{cx: cx, cy: cy, rx: ry, ry: rx}

	img := renderClip(w, h, func(s *ScannerGV) error {
		return s.PushClipPath(func(a Adder) error {
			m := Identity.Translate(cx, cy).Rotate(math.Pi/2).Translate(-cx, -cy)
			addEllipse(cx, cy, rx, ry, &MatrixAdder{Adder: a, M: m})
			return nil
		})
	})
	checkAgainstReference(t, img, 9, rotated)

	// Translate and scale: (cx,cy,rx,ry) -> (cx/2+40, cy/2+..., ...).
	img2 := renderClip(w, h, func(s *ScannerGV) error {
		return s.PushClipPath(func(a Adder) error {
			m := Identity.Translate(40-35, 44-50).Scale(0.5, 0.5)
			addEllipse(70, 100, 40, 24, &MatrixAdder{Adder: a, M: m})
			return nil
		})
	})
	checkAgainstReference(t, img2, 9, refShape{cx: 40, cy: 44, rx: 20, ry: 12})
}

// TestClipOffsetMask covers an alpha mask whose Bounds are offset from the
// origin, mirroring a detached UI overlay mask, and compares exactly.
func TestClipOffsetMask(t *testing.T) {
	const w, h = 96, 80
	maskRect := image.Rect(20, 10, 60, 50)
	mask := image.NewAlpha(maskRect)
	for y := maskRect.Min.Y; y < maskRect.Max.Y; y++ {
		for x := maskRect.Min.X; x < maskRect.Max.X; x++ {
			mask.SetAlpha(x, y, color.Alpha{A: uint8((x*3 + y*5) & 0xff)})
		}
	}
	before := append([]byte(nil), mask.Pix...)

	img := renderClip(w, h, func(s *ScannerGV) error { return s.PushClipMask(mask) })
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			got := img.RGBAAt(x, y)
			var want color.RGBA
			if (image.Point{x, y}).In(maskRect) {
				a := mask.AlphaAt(x, y).A
				want = color.RGBA{R: a, A: a}
			}
			if got != want {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
	if !bytesEqual(before, mask.Pix) {
		t.Fatal("shared clip mask was modified")
	}

	// A non-*image.Alpha mask takes the generic At path and must agree.
	rgbaMask := image.NewRGBA(image.Rect(5, 7, 45, 41))
	for y := rgbaMask.Rect.Min.Y; y < rgbaMask.Rect.Max.Y; y++ {
		for x := rgbaMask.Rect.Min.X; x < rgbaMask.Rect.Max.X; x++ {
			rgbaMask.Set(x, y, color.RGBA{R: 9, G: 9, B: 9, A: uint8((x * 7) & 0xff)})
		}
	}
	img2 := renderClip(w, h, func(s *ScannerGV) error { return s.PushClipMask(rgbaMask) })
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			got := img2.RGBAAt(x, y)
			var want color.RGBA
			if (image.Point{x, y}).In(rgbaMask.Rect) {
				_, _, _, a := rgbaMask.At(x, y).RGBA()
				want = color.RGBA{R: uint8(a >> 8), A: uint8(a >> 8)}
			}
			if got != want {
				t.Fatalf("generic mask pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

// TestClipMaskOffsetCombines checks that an offset mask and a path clip
// multiply in source coordinates.
func TestClipMaskOffsetCombines(t *testing.T) {
	const w, h = 96, 96
	maskRect := image.Rect(24, 16, 80, 72)
	mask := image.NewAlpha(maskRect)
	for y := maskRect.Min.Y; y < maskRect.Max.Y; y++ {
		for x := maskRect.Min.X; x < maskRect.Max.X; x++ {
			mask.SetAlpha(x, y, color.Alpha{A: 200})
		}
	}
	circle := refShape{cx: 48, cy: 48, rx: 30, ry: 30}

	imgMask := renderClip(w, h, func(s *ScannerGV) error { return s.PushClipMask(mask) })
	imgCircle := renderClip(w, h, circlePush(circle.cx, circle.cy, circle.rx, circle.ry))
	imgBoth := renderClip(w, h,
		func(s *ScannerGV) error { return s.PushClipMask(mask) },
		circlePush(circle.cx, circle.cy, circle.rx, circle.ry))

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := int(imgMask.RGBAAt(x, y).A)
			b := int(imgCircle.RGBAAt(x, y).A)
			want := (a*b + 127) / 255
			if got := int(imgBoth.RGBAAt(x, y).A); got != want {
				t.Fatalf("mask+path coverage at (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClipCancelRestoresState verifies that every failure mode leaves the
// clip stack and later rendering exactly as they were, compared byte for byte
// with a fixture rendered through the surviving outer clip.
func TestClipCancelRestoresState(t *testing.T) {
	const w, h = 80, 80
	outer := func(s *ScannerGV) error {
		return s.PushClipPath(func(a Adder) error {
			addEllipse(40, 40, 30, 30, a)
			return nil
		})
	}

	clipFixture := renderClip(w, h, outer)
	fixture := renderClip(w, h)

	img, s, f := newScene(w, h)
	if err := outer(s); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("cancel")
	cases := []struct {
		name string
		push func() error
		want error
	}{
		{"callback error", func() error {
			return s.PushClipPath(func(a Adder) error {
				addEllipse(10, 10, 5, 5, a)
				return sentinel
			})
		}, sentinel},
		{"empty path", func() error {
			return s.PushClipPath(func(a Adder) error { return nil })
		}, ErrEmptyClipPath},
		{"move only", func() error {
			return s.PushClipPath(func(a Adder) error {
				a.Start(ToFixedP(5, 5))
				a.Stop(false)
				return nil
			})
		}, ErrEmptyClipPath},
		{"nil callback", func() error { return s.PushClipPath(nil) }, ErrNilClipAdder},
		{"nil mask", func() error { return s.PushClipMask(nil) }, ErrNilClipMask},
	}
	for _, c := range cases {
		if err := c.push(); !errors.Is(err, c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, err, c.want)
		}
		if s.ClipDepth() != 1 {
			t.Fatalf("%s: ClipDepth = %d, want 1", c.name, s.ClipDepth())
		}
	}

	addFullRect(w, h, f)
	f.Draw()
	if !bytesEqual(img.Pix, clipFixture.Pix) {
		t.Fatal("cancelled pushes polluted the subsequent draw")
	}

	// Popping the remaining layer restores the unclipped behaviour.
	if err := s.PopClip(); err != nil {
		t.Fatal(err)
	}
	if err := s.PopClip(); !errors.Is(err, ErrClipStackEmpty) {
		t.Fatalf("second pop = %v, want ErrClipStackEmpty", err)
	}
	if s.ClipDepth() != 0 {
		t.Fatal("depth changed after empty pop")
	}
	img2, _, f2 := newScene(w, h)
	f2.Scanner = s
	_ = img2
	// Rebind scanner to the blank destination used by the fixture.
	blank := image.NewRGBA(image.Rect(0, 0, w, h))
	s.Dest = blank
	f.Clear()
	addFullRect(w, h, f)
	f.Draw()
	if !bytesEqual(blank.Pix, fixture.Pix) {
		t.Fatal("state after pop does not match unclipped fixture")
	}
}

// TestClipPushPopRestoresFixtures verifies that popping B restores exactly
// the image produced by A alone, and popping A restores the unclipped image.
func TestClipPushPopRestoresFixtures(t *testing.T) {
	const w, h = 96, 96
	pushA := circlePush(48, 48, 40, 30)
	pushB := func(s *ScannerGV) error {
		return s.PushClipPath(func(a Adder) error {
			AddRect(30, 20, 70, 80, 0, a)
			return nil
		})
	}
	aOnly := renderClip(w, h, pushA)
	both := renderClip(w, h, pushA, pushB)
	none := renderClip(w, h)

	s, f := func() (*ScannerGV, *Filler) {
		_, s, f := newScene(w, h)
		return s, f
	}()
	dest := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		s.Dest = img
		f.Clear()
		return img
	}

	if err := pushA(s); err != nil {
		t.Fatal(err)
	}
	if err := pushB(s); err != nil {
		t.Fatal(err)
	}
	img := dest()
	addFullRect(w, h, f)
	f.Draw()
	if !bytesEqual(img.Pix, both.Pix) {
		t.Fatal("pushed A then B does not match fixture")
	}

	if err := s.PopClip(); err != nil {
		t.Fatal(err)
	}
	img = dest()
	addFullRect(w, h, f)
	f.Draw()
	if !bytesEqual(img.Pix, aOnly.Pix) {
		t.Fatal("after popping B the result is not the A-only fixture")
	}

	if err := s.PopClip(); err != nil {
		t.Fatal(err)
	}
	img = dest()
	addFullRect(w, h, f)
	f.Draw()
	if !bytesEqual(img.Pix, none.Pix) {
		t.Fatal("after popping A the result is not the unclipped fixture")
	}
}

// TestClipLimits verifies depth, pixel and memory limits reject a push
// without changing state or polluting later layers.
func TestClipLimits(t *testing.T) {
	const w, h = 64, 64
	img, s, f := newScene(w, h)
	circle := func(a Adder) error {
		addEllipse(32, 32, 20, 20, a)
		return nil
	}

	s.SetClipLimits(ClipLimits{MaxDepth: 2})
	if err := s.PushClipPath(circle); err != nil {
		t.Fatal(err)
	}
	if err := s.PushClipPath(circle); err != nil {
		t.Fatal(err)
	}
	if err := s.PushClipPath(circle); !errors.Is(err, ErrClipDepthLimit) {
		t.Fatalf("third push = %v, want ErrClipDepthLimit", err)
	}
	if s.ClipDepth() != 2 {
		t.Fatalf("ClipDepth = %d, want 2", s.ClipDepth())
	}
	if err := s.PopClip(); err != nil {
		t.Fatal(err)
	}
	if err := s.PopClip(); err != nil {
		t.Fatal(err)
	}

	s.SetClipLimits(ClipLimits{MaxPixels: 100})
	if err := s.PushClipPath(circle); !errors.Is(err, ErrClipPixelLimit) {
		t.Fatalf("pixel limited push = %v, want ErrClipPixelLimit", err)
	}
	if s.ClipDepth() != 0 {
		t.Fatal("rejected push changed depth")
	}
	small := image.NewAlpha(image.Rect(0, 0, 10, 10))
	if err := s.PushClipMask(small); err != nil {
		t.Fatalf("small mask rejected: %v", err)
	}
	if err := s.PopClip(); err != nil {
		t.Fatal(err)
	}

	s.SetClipLimits(ClipLimits{MaxMemory: 100})
	if err := s.PushClipPath(circle); !errors.Is(err, ErrClipMemoryLimit) {
		t.Fatalf("memory limited push = %v, want ErrClipMemoryLimit", err)
	}
	if s.ClipDepth() != 0 {
		t.Fatal("memory rejected push changed depth")
	}

	// Zero limits restore the defaults, so the push succeeds and renders.
	s.SetClipLimits(ClipLimits{})
	if err := s.PushClipPath(circle); err != nil {
		t.Fatal(err)
	}
	addFullRect(w, h, f)
	f.Draw()
	checkAgainstReference(t, img, 9, refShape{cx: 32, cy: 32, rx: 20, ry: 20})
}

// TestClipSetClipCompatibility verifies that the rectangle SetClip method is
// unchanged and composes with the stack as an intersection.
func TestClipSetClipCompatibility(t *testing.T) {
	const w, h = 96, 96
	rect := image.Rect(20, 20, 76, 76)
	circle := refShape{cx: 48, cy: 48, rx: 36, ry: 36}

	rectOnly := renderRectClip(w, h, func(s *ScannerGV) { s.SetClip(rect) })
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			got := rectOnly.RGBAAt(x, y)
			var want color.RGBA
			if (image.Point{x, y}).In(rect) {
				want = color.RGBA{R: 255, A: 255}
			}
			if got != want {
				t.Fatalf("rect clip pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}

	// SetClip(image.ZR) clears the rectangle clip, as before.
	circleOnly := renderRectClip(w, h, func(s *ScannerGV) {
		s.SetClip(rect)
		s.SetClip(image.ZR)
		if err := s.PushClipPath(func(a Adder) error {
			addEllipse(circle.cx, circle.cy, circle.rx, circle.ry, a)
			return nil
		}); err != nil {
			t.Errorf("push: %v", err)
		}
	})
	checkAgainstReference(t, circleOnly, 9, circle)

	// With both active the result is their intersection.
	combined := renderRectClip(w, h, func(s *ScannerGV) {
		s.SetClip(rect)
		if err := s.PushClipPath(func(a Adder) error {
			addEllipse(circle.cx, circle.cy, circle.rx, circle.ry, a)
			return nil
		}); err != nil {
			t.Errorf("push: %v", err)
		}
	})
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			got := combined.RGBAAt(x, y)
			want := u8(circle.cov(x, y))
			if !(image.Point{x, y}).In(rect) {
				want = 0
			}
			if abs(int(got.A)-want) > 9 {
				t.Fatalf("combined clip pixel (%d,%d) = %v, want alpha %d", x, y, got, want)
			}
		}
	}

	// An opaque mask with the same rectangle agrees with SetClip exactly.
	imgMask := renderClip(w, h, func(s *ScannerGV) error {
		m := image.NewAlpha(rect)
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				m.SetAlpha(x, y, color.Alpha{A: 255})
			}
		}
		return s.PushClipMask(m)
	})
	if !bytesEqual(imgMask.Pix, rectOnly.Pix) {
		t.Fatal("opaque mask rectangle differs from SetClip rectangle")
	}
}

func renderRectClip(w, h int, setup func(*ScannerGV)) *image.RGBA {
	img, s, f := newScene(w, h)
	setup(s)
	addFullRect(w, h, f)
	f.Draw()
	return img
}

// TestClipClearDrawOrder verifies that Clear only resets the accumulated
// path and leaves the clip stack intact, and that Draw uses the stack active
// when it is called.
func TestClipClearDrawOrder(t *testing.T) {
	const w, h = 80, 80
	circle := refShape{cx: 40, cy: 40, rx: 26, ry: 26}

	s, f := func() (*ScannerGV, *Filler) {
		_, s, f := newScene(w, h)
		return s, f
	}()
	fresh := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		s.Dest = img
		f.Clear()
		return img
	}

	if err := s.PushClipPath(func(a Adder) error {
		addEllipse(circle.cx, circle.cy, circle.rx, circle.ry, a)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	img := fresh()
	addFullRect(w, h, f)
	f.Draw()
	f.Clear() // must not pop the clip
	if s.ClipDepth() != 1 {
		t.Fatal("Clear dropped the clip stack")
	}
	checkAgainstReference(t, img, 9, circle)

	// A second draw through the still-active clip on a fresh destination
	// is identical to the first.
	img = fresh()
	addFullRect(w, h, f)
	f.Draw()
	checkAgainstReference(t, img, 9, circle)

	// Draw reads the stack as it is at Draw time. Popping after adding
	// geometry removes the clip for that draw.
	if err := s.PopClip(); err != nil {
		t.Fatal(err)
	}
	img = fresh()
	addFullRect(w, h, f) // geometry added while unclipped
	f.Draw()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if got := img.RGBAAt(x, y); got != (color.RGBA{R: 255, A: 255}) {
				t.Fatalf("unclipped pixel (%d,%d) = %v", x, y, got)
			}
		}
	}
}

// TestClipBoundaryTangency covers a clip path tangent to drawn geometry and
// degenerate clip paths, with defined results.
func TestClipBoundaryTangency(t *testing.T) {
	const w, h = 64, 64

	// A circle whose left edge is exactly tangent to x=32. Nothing left of
	// x=32 may be touched; the tangent column centre has coverage.
	img := renderClip(w, h, circlePush(48, 32, 16, 16))
	for y := 0; y < h; y++ {
		for x := 0; x < 32; x++ {
			if got := img.RGBAAt(x, y); got.A != 0 {
				t.Fatalf("tangent clip touched (%d,%d) with %v", x, y, got)
			}
		}
	}
	if img.RGBAAt(32, 32).A == 0 {
		t.Fatal("tangent pixel centre has no coverage")
	}

	// A path consisting only of a degenerate line has no area: the push
	// succeeds but affects no pixel.
	img2 := renderClip(w, h, func(s *ScannerGV) error {
		return s.PushClipPath(func(a Adder) error {
			a.Start(ToFixedP(10, 10))
			a.Line(ToFixedP(10, 10))
			a.Stop(true)
			return nil
		})
	})
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if got := img2.RGBAAt(x, y); got.A != 0 {
				t.Fatalf("degenerate clip touched (%d,%d) with %v", x, y, got)
			}
		}
	}

	// Pushing onto a rasterizer with zero area is well defined and empty.
	zero := image.NewRGBA(image.Rect(0, 0, 0, 0))
	zs := NewScannerGV(0, 0, zero, zero.Bounds())
	if err := zs.PushClipPath(func(a Adder) error {
		addEllipse(0, 0, 5, 5, a)
		return nil
	}); err != nil {
		t.Fatalf("zero sized push = %v, want nil", err)
	}
	if zs.ClipDepth() != 1 {
		t.Fatal("zero sized push not recorded")
	}
}

// TestClipNonZeroBounds verifies clip paths map through a destination whose
// Bounds do not start at the origin.
func TestClipNonZeroBounds(t *testing.T) {
	const w, h = 80, 80
	origin := image.Pt(100, 60)
	img := image.NewRGBA(image.Rect(origin.X, origin.Y, origin.X+w, origin.Y+h))
	s := NewScannerGV(w, h, img, img.Bounds())
	f := NewFiller(w, h, s)
	s.SetColor(color.RGBA{255, 0, 0, 255})

	circle := refShape{cx: 30, cy: 34, rx: 18, ry: 18}
	if err := s.PushClipPath(func(a Adder) error {
		addEllipse(circle.cx, circle.cy, circle.rx, circle.ry, a)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	addFullRect(w, h, f)
	f.Draw()

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			want := u8(circle.cov(x, y))
			got := img.RGBAAt(origin.X+x, origin.Y+y)
			if abs(int(got.A)-want) > 9 {
				t.Fatalf("offset bounds pixel (%d,%d) = %v, want alpha %d", x, y, got, want)
			}
		}
	}
}

// TestClipSourceOffset verifies that a non-zero Source Offset shifts only the
// source lookup while path clip coverage stays in scanner (canvas)
// coordinates, aligned with the drawn geometry.
func TestClipSourceOffset(t *testing.T) {
	const w, h = 64, 64
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := NewScannerGV(w, h, img, img.Bounds())
	f := NewFiller(w, h, s)
	s.SetColor(color.RGBA{255, 0, 0, 255})
	s.Offset = image.Pt(10, 6)

	circle := refShape{cx: 32, cy: 32, rx: 20, ry: 20}
	if err := s.PushClipPath(func(a Adder) error {
		addEllipse(circle.cx, circle.cy, circle.rx, circle.ry, a)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	addFullRect(w, h, f)
	f.Draw()

	// The uniform red source is identical everywhere, so only the clip shows:
	// the result is the unshifted circle coverage.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			want := u8(circle.cov(x, y))
			got := img.RGBAAt(x, y)
			if abs(int(got.A)-want) > 9 {
				t.Fatalf("offset source pixel (%d,%d) = %v, want alpha %d", x, y, got, want)
			}
		}
	}
}

// TestClipSharedMaskAcrossScanners verifies that one mask can be shared
// read-only by independent scanners and produces identical results.
func TestClipSharedMaskAcrossScanners(t *testing.T) {
	const w, h = 64, 64
	mask := image.NewAlpha(image.Rect(8, 8, 56, 56))
	for y := 8; y < 56; y++ {
		for x := 8; x < 56; x++ {
			mask.SetAlpha(x, y, color.Alpha{A: uint8((x * y) & 0xff)})
		}
	}
	before := append([]byte(nil), mask.Pix...)

	var results [2]*image.RGBA
	for i := range results {
		results[i] = renderClip(w, h, func(s *ScannerGV) error { return s.PushClipMask(mask) })
	}
	if !bytesEqual(results[0].Pix, results[1].Pix) {
		t.Fatal("shared mask produced different results on two scanners")
	}
	if !bytesEqual(before, mask.Pix) {
		t.Fatal("shared mask was modified")
	}
}

// TestClipTransparentTarget verifies that pixels with no clip coverage stay
// fully transparent on a transparent destination.
func TestClipTransparentTarget(t *testing.T) {
	const w, h = 48, 48
	inside := image.Rect(16, 16, 32, 32)
	img := renderClip(w, h, func(s *ScannerGV) error {
		return s.PushClipMask(image.NewAlpha(inside))
	})
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			got := img.RGBAAt(x, y)
			if !(image.Point{x, y}).In(inside) && got != (color.RGBA{}) {
				t.Fatalf("outside clip pixel (%d,%d) = %v, want fully transparent", x, y, got)
			}
		}
	}
}

// TestClipNewScannerState checks the initial state and empty pop.
func TestClipNewScannerState(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	s := NewScannerGV(10, 10, img, img.Bounds())
	if s.ClipDepth() != 0 {
		t.Fatal("new scanner started with clips")
	}
	if err := s.PopClip(); !errors.Is(err, ErrClipStackEmpty) {
		t.Fatalf("pop on new scanner = %v, want ErrClipStackEmpty", err)
	}
}

// TestClipSetBoundsInvalidates verifies that changing the rasterizer bounds
// drops coverage tied to the old size instead of drawing stale clips.
func TestClipSetBoundsInvalidates(t *testing.T) {
	const w, h = 32, 32
	_, s, _ := newScene(w, h)
	if err := s.PushClipPath(func(a Adder) error {
		addEllipse(16, 16, 10, 10, a)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s.ClipDepth() != 1 {
		t.Fatal("clip not pushed")
	}
	s.SetBounds(w, h)
	if s.ClipDepth() != 0 {
		t.Fatal("SetBounds kept clips rasterized for the old bounds")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
