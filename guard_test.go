// Copyright 2018 by the rasterx Authors. All rights reserved.
package rasterx_test

// guard_test.go pins two properties of the protective layer:
//
//  1. It does not change normal rendering. Every control scene is rendered
//     once without the guard and once with it, and the pixel buffers must be
//     byte-identical. The expected hashes were recorded from the unmodified
//     package, so they also catch an accidental change to the normal path.
//
//  2. It records the resource counts the requirement asks for: path commands,
//     failure phase, flattened line count and extent.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"testing"

	. "github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// scene renders img with an optional guard layer and returns it.
type scene struct {
	name string
	// wantHash is the SHA-256 of the 256x256 RGBA buffer produced by the
	// unmodified package. A change here means the normal path moved, which
	// the requirement forbids.
	wantHash string
	draw     func(sc Scanner)
}

func testScenes() []scene {
	g := &Gradient{
		Points: [5]float64{0, 0, 1, 1, 0},
		Bounds: struct{ X, Y, W, H float64 }{X: 0, Y: 0, W: 200, H: 200},
		Matrix: Identity,
	}
	g.Stops = []GradStop{
		{StopColor: color.RGBA{255, 0, 0, 255}, Offset: 0, Opacity: 1},
		{StopColor: color.RGBA{0, 0, 255, 255}, Offset: 1, Opacity: 1},
	}
	return []scene{
		{"fill", "6f520debadc017946d0676c7d99073ed4b9dc2c93f9989f55441a6d78827cffd",
			func(sc Scanner) {
				sc.SetColor(color.RGBA{10, 200, 30, 255})
				f := NewFiller(256, 256, sc)
				GetTestPath().AddTo(f)
				f.Draw()
			}},
		{"stroke-arcclip", "23b24674e13c13e8275e2efb0df0028731004c98d4dadf495467c9300c5bce90",
			func(sc Scanner) {
				sc.SetColor(color.RGBA{200, 10, 30, 255})
				s := NewStroker(256, 256, sc)
				s.SetStroke(10*64, 4*64, RoundCap, nil, RoundGap, ArcClip)
				GetTestPath().AddTo(s)
				s.Draw()
			}},
		{"dash-arcclip", "7f689feb82de7e0bb0bb205bd68a7c375f2e4824b9bd18d218ffa91ac97d7fc0",
			func(sc Scanner) {
				sc.SetColor(color.RGBA{30, 10, 200, 255})
				d := NewDasher(256, 256, sc)
				d.SetStroke(10*64, 4*64, RoundCap, nil, RoundGap, ArcClip,
					[]float64{33, 12}, 0)
				GetTestPath().AddTo(d)
				d.Draw()
			}},
		{"roundrect-miterclip", "d4035dc09b6da9b79ddf2fe9d2ee2b943f3afb67e7734fb225523a5cc9536490",
			func(sc Scanner) {
				sc.SetColor(color.RGBA{1, 2, 3, 255})
				s := NewStroker(256, 256, sc)
				s.SetStroke(6*64, 4*64, ButtCap, nil, FlatGap, MiterClip)
				AddRoundRect(30, 30, 200, 200, 20, 20, 17, RoundGap, s)
				s.Draw()
			}},
		{"linear-gradient", "bce84a1c81c26e7733b3f44c3bce23cee07d7796229226de9d7076d3e1483e83",
			func(sc Scanner) {
				f := NewFiller(256, 256, sc)
				sc.SetColor(g.GetColorFunction(1.0))
				AddCircle(128, 128, 90, f)
				f.Draw()
			}},
		{"clip-rect", "2d24e3436821a7b2cfc97f9e613b28a4755e1252112f990d0f8f799b56c443ff",
			func(sc Scanner) {
				sc.SetColor(color.RGBA{0, 255, 0, 255})
				f := NewFiller(256, 256, sc)
				sc.SetClip(image.Rect(60, 60, 160, 160))
				AddCircle(128, 128, 100, f)
				f.Draw()
			}},
	}
}

func renderScene(t *testing.T, s scene, guarded bool) *image.RGBA {
	t.Helper()
	const w = 256
	img := image.NewRGBA(image.Rect(0, 0, w, w))
	sc := NewScannerGV(w, w, img, img.Bounds())
	var sink Scanner = sc
	if guarded {
		sink = NewGuardScanner(sc)
	}
	s.draw(sink)
	return img
}

// TestGuardPreservesPixels is the byte-for-byte regression for the normal
// path: the guard must be transparent, and the recorded hashes must still
// match.
func TestGuardPreservesPixels(t *testing.T) {
	for _, s := range testScenes() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			plain := renderScene(t, s, false)
			guarded := renderScene(t, s, true)
			if string(plain.Pix) != string(guarded.Pix) {
				t.Fatal("GuardScanner changed the rendered pixels")
			}
			sum := sha256.Sum256(plain.Pix)
			if got := hex.EncodeToString(sum[:]); got != s.wantHash {
				t.Fatalf("normal-path hash changed: got %s, want %s", got, s.wantHash)
			}
		})
	}
}

// TestGuardRecordsFailurePhase checks that a rejected path reports the phase
// it failed in and the command index, which is what a diagnostic needs.
func TestGuardRecordsFailurePhase(t *testing.T) {
	p := Path{
		fixed.Int26_6(PathMoveTo), 1, 1,
		fixed.Int26_6(PathLineTo), 2, 2,
		fixed.Int26_6(PathCubicTo), 3, 3, // truncated: needs six operands
	}
	rep := AnalyzePath(p, DefaultLimits())
	if !errors.Is(rep.Err, ErrBadPath) {
		t.Fatalf("Err = %v, want ErrBadPath", rep.Err)
	}
	if rep.FailureStage == "" {
		t.Fatal("failure stage is empty")
	}
	// The command counts recorded before the failure must be visible.
	if rep.MoveTo != 1 || rep.LineTo != 1 {
		t.Fatalf("counts before failure are wrong: %+v", rep)
	}
}

// TestGuardRecordsResources checks the resource accounting on a valid path.
func TestGuardRecordsResources(t *testing.T) {
	g := NewGuardAdder(&Path{})
	p := GetTestPath()
	if err := p.AddToChecked(g); err != nil {
		t.Fatalf("AddToChecked: %v", err)
	}
	if g.Err != nil {
		t.Fatalf("GuardAdder rejected a valid path: %v at %q", g.Err, g.FailureStage)
	}
	if g.Segments == 0 || g.Curves == 0 || g.Moves == 0 {
		t.Fatalf("counts not recorded: %+v", g)
	}
	if g.EstLines == 0 {
		t.Fatal("estimated line count not recorded")
	}
	if !g.HaveExtent || g.Min == g.Max {
		t.Fatalf("extent not recorded: %v..%v", g.Min, g.Max)
	}
}

// TestValidateDashes checks the dash validator directly.
func TestValidateDashes(t *testing.T) {
	ok := [][]float64{nil, {}, {1}, {3, 2}, {0.5, 0.5, 4}}
	for _, d := range ok {
		if err := ValidateDashes(d, 0); err != nil {
			t.Errorf("ValidateDashes(%v) = %v, want nil", d, err)
		}
	}
	bad := []struct {
		dashes []float64
		offset float64
	}{
		{[]float64{0, 1}, 0},
		{[]float64{1, -1}, 0},
		{[]float64{1, 1}, -1e300},
		{[]float64{1, 1}, 1e300},
		{make([]float64, MaxDashEntries+1), 0},
	}
	for _, c := range bad {
		if err := ValidateDashes(c.dashes, c.offset); !errors.Is(err, ErrBadStrokeParams) {
			t.Errorf("ValidateDashes(len=%d, off=%v) = %v, want ErrBadStrokeParams",
				len(c.dashes), c.offset, err)
		}
	}
}

// TestValidateStroke checks the stroke parameter validator.
func TestValidateStroke(t *testing.T) {
	if err := ValidateStroke(10<<6, 4<<6); err != nil {
		t.Errorf("valid stroke rejected: %v", err)
	}
	if err := ValidateStroke(0, 4<<6); err != nil {
		t.Errorf("zero width (legal) rejected: %v", err)
	}
	if err := ValidateStroke(-1, 4<<6); !errors.Is(err, ErrBadStrokeParams) {
		t.Errorf("negative width: %v", err)
	}
	if err := ValidateStroke(10<<6, -1); !errors.Is(err, ErrBadStrokeParams) {
		t.Errorf("negative miter: %v", err)
	}
	if err := ValidateStroke(SafeCoordLimit, 1<<20); !errors.Is(err, ErrBadStrokeParams) {
		t.Errorf("oversized parameters: %v", err)
	}
}

// TestScannerClampsCoord proves the scanner never hands the backing
// rasterizer a coordinate outside the safe range, even when a caller bypasses
// the guard.
func TestScannerClampsCoord(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	sc := NewScannerGV(8, 8, img, img.Bounds())
	sc.Start(fixed.Point26_6{X: fixed.Int26_6(1 << 30), Y: -fixed.Int26_6(1 << 30)})
	sc.Line(fixed.Point26_6{X: -fixed.Int26_6(1 << 30), Y: fixed.Int26_6(1 << 30)})
	ext := sc.GetPathExtent()
	for _, v := range []fixed.Int26_6{ext.Min.X, ext.Min.Y, ext.Max.X, ext.Max.Y} {
		if !IsSafeCoord(v) {
			t.Fatalf("scanner extent %v exceeds SafeCoordLimit", v)
		}
	}
	sc.Draw() // must not panic or write outside the image
}

// TestScannerDoesNotWriteOutOfBounds reproduces a real defect: when the
// destination image was larger than the rasterizer's mask, Draw indexed the
// mask past its end. Every mismatched combination must complete and must leave
// pixels outside the drawable region untouched.
func TestScannerDoesNotWriteOutOfBounds(t *testing.T) {
	drawMalformed := func(w, h int, img *image.RGBA) {
		sc := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, sc)
		f.Start(fixed.Point26_6{X: -1 << 30, Y: -1 << 30})
		f.Line(fixed.Point26_6{X: 1 << 30, Y: 1 << 30})
		f.Line(fixed.Point26_6{X: 1 << 30, Y: -1 << 30})
		f.Stop(true)
		f.Draw()
	}

	t.Run("destination larger than mask", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 200, 200))
		// Sentinel outside the 50x50 region the mask can address.
		for y := 50; y < 200; y++ {
			for x := 0; x < 200; x++ {
				img.SetRGBA(x, y, color.RGBA{0xAA, 0xAA, 0xAA, 0xAA})
			}
		}
		drawMalformed(50, 50, img) // used to panic: index out of range
		for y := 50; y < 200; y++ {
			for x := 0; x < 200; x++ {
				if got := img.RGBAAt(x, y); got != (color.RGBA{0xAA, 0xAA, 0xAA, 0xAA}) {
					t.Fatalf("pixel (%d,%d) outside the mask was overwritten: %v", x, y, got)
				}
			}
		}
	})

	t.Run("destination smaller than mask", func(t *testing.T) {
		drawMalformed(100, 100, image.NewRGBA(image.Rect(0, 0, 20, 20)))
	})

	t.Run("offset subimage", func(t *testing.T) {
		parent := image.NewRGBA(image.Rect(0, 0, 200, 200))
		sub := parent.SubImage(image.Rect(50, 50, 180, 180)).(*image.RGBA)
		drawMalformed(40, 40, sub)
	})
}
