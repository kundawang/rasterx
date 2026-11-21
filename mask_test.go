// Copyright 2026 by the rasterx Authors. All rights reserved.
// Tests for the ScannerGV pixel mask support.
package rasterx_test

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"testing"

	. "github.com/srwiley/rasterx"
)

// chDiff returns the absolute difference of two uint32 channel values.
func chDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// checkPix verifies that the 16 bit premultiplied channels of the image
// at (x, y) are within tol of the expected values.
func checkPix(t *testing.T, img image.Image, x, y int, er, eg, eb, ea, tol uint32) {
	t.Helper()
	r, g, b, a := img.At(x, y).RGBA()
	if chDiff(r, er) > tol || chDiff(g, eg) > tol || chDiff(b, eb) > tol || chDiff(a, ea) > tol {
		t.Errorf("pixel (%d, %d): got (%#04x, %#04x, %#04x, %#04x), want (%#04x, %#04x, %#04x, %#04x)",
			x, y, r, g, b, a, er, eg, eb, ea)
	}
}

// TestMaskGrayGradient checks a luminance mask made of a horizontal gray
// gradient against per-pixel expectations: an opaque red source on a
// transparent destination must come out as red premultiplied by the
// gradient value.
func TestMaskGrayGradient(t *testing.T) {
	const w, h = 64, 64
	var (
		img       = image.NewRGBA(image.Rect(0, 0, w, h))
		scannerGV = NewScannerGV(w, h, img, img.Bounds())
		f         = NewFiller(w, h, scannerGV)
		mask      = image.NewGray(image.Rect(0, 0, w, h))
	)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			mask.SetGray(x, y, color.Gray{Y: uint8(x * 4)})
		}
	}
	scannerGV.SetColor(color.RGBA{255, 0, 0, 255})
	scannerGV.SetMask(mask)
	AddRect(8, 8, 56, 56, 0, f)
	f.Draw()
	f.Clear()

	// Interior pixels: full path coverage, so the result is the source
	// scaled by the mask luminance alone.
	for x := 12; x < 52; x++ {
		exp := uint32(x*4) * 0x101
		for y := 12; y < 52; y++ {
			checkPix(t, img, x, y, exp, 0, 0, exp, 0x101)
		}
	}
	// Pixels outside the filled path must be untouched.
	for _, p := range []image.Point{{2, 2}, {60, 60}, {2, 60}, {60, 2}} {
		checkPix(t, img, p.X, p.Y, 0, 0, 0, 0, 0)
	}
}

// TestMaskAlphaCheckerboard checks an *image.Alpha checkerboard mask,
// the reuse of one mask by several scanners (layers), mask shifting via
// SetMaskOffset, and that the caller's mask is never modified.
func TestMaskAlphaCheckerboard(t *testing.T) {
	const w, h = 64, 64
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x/8+y/8)%2 == 0 {
				mask.SetAlpha(x, y, color.Alpha{A: 255})
			}
		}
	}
	snapshot := append([]byte(nil), mask.Pix...)

	green := color.RGBA{0, 200, 0, 255}
	drawLayer := func(offset image.Point) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		scannerGV := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, scannerGV)
		scannerGV.SetColor(green)
		scannerGV.SetMask(mask)
		scannerGV.SetMaskOffset(offset)
		AddRect(0, 0, w, h, 0, f)
		f.Draw()
		f.Clear()
		return img
	}

	// First layer, no offset: cell centers alternate full green / zero.
	img1 := drawLayer(image.Point{})
	for _, p := range []image.Point{{4, 4}, {20, 20}, {12, 4}, {4, 12}, {36, 44}} {
		mx, my := p.X, p.Y
		if (mx/8+my/8)%2 == 0 {
			checkPix(t, img1, p.X, p.Y, 0, 200*0x101, 0, 0xffff, 0x101)
		} else {
			checkPix(t, img1, p.X, p.Y, 0, 0, 0, 0, 0)
		}
	}

	// Second layer reusing the same mask, shifted by (4, 4): the
	// checkerboard pattern must shift with the offset.
	img2 := drawLayer(image.Point{4, 4})
	for y := 6; y < h; y += 8 {
		for x := 6; x < w; x += 8 {
			mx, my := x-4, y-4
			if (mx/8+my/8)%2 == 0 {
				checkPix(t, img2, x, y, 0, 200*0x101, 0, 0xffff, 0x101)
			} else {
				checkPix(t, img2, x, y, 0, 0, 0, 0, 0)
			}
		}
	}

	// The caller's mask must be byte-for-byte unchanged.
	if !bytes.Equal(snapshot, mask.Pix) {
		t.Error("mask image was modified by Draw")
	}
}

// TestMaskPremultiplied checks compositing of a semi-transparent source
// over a destination that already holds alpha, through a half-value
// luminance mask, against the standard premultiplied src-over formula.
func TestMaskPremultiplied(t *testing.T) {
	const w, h = 32, 32
	var (
		dstC = color.NRGBA{0, 0, 200, 128}
		srcC = color.NRGBA{255, 0, 0, 128}
		img  = image.NewRGBA(image.Rect(0, 0, w, h))
	)
	draw.Draw(img, img.Bounds(), &image.Uniform{dstC}, image.Point{}, draw.Src)

	scannerGV := NewScannerGV(w, h, img, img.Bounds())
	f := NewFiller(w, h, scannerGV)
	scannerGV.SetColor(srcC)
	scannerGV.SetMask(&image.Uniform{color.Gray{Y: 128}})
	AddRect(4, 4, 28, 28, 0, f)
	f.Draw()
	f.Clear()

	// Expected value from the same formula image/draw uses: with full
	// path coverage the effective mask alpha is the gray value 0x8080.
	sr, sg, sb, sa := srcC.RGBA()
	dr, dg, db, da := dstC.RGBA()
	ma := uint32(0x8080)
	a := 0xffff - (sa * ma / 0xffff)
	er := (dr*a + sr*ma) / 0xffff
	eg := (dg*a + sg*ma) / 0xffff
	eb := (db*a + sb*ma) / 0xffff
	ea := (da*a + sa*ma) / 0xffff

	for y := 10; y < 22; y++ {
		for x := 10; x < 22; x++ {
			checkPix(t, img, x, y, er, eg, eb, ea, 0x202)
		}
	}
	// Outside the path the destination must keep its original content.
	checkPix(t, img, 1, 1, 0, 0, 200*128/255*0x101, 128*0x101, 0x101)
}

// TestMaskAlignment checks mask and destination images with non-zero
// origins plus a mask offset: the mask must land at exactly
// mask.Bounds()+offset in destination coordinates, coverage outside the
// mask must be zero, and drawing must not shift with the destination
// origin.
func TestMaskAlignment(t *testing.T) {
	var (
		img       = image.NewRGBA(image.Rect(10, 10, 74, 74)) // non-zero origin
		scannerGV = NewScannerGV(64, 64, img, img.Bounds())
		f         = NewFiller(64, 64, scannerGV)
		mask      = image.NewAlpha(image.Rect(5, 7, 45, 47)) // non-zero origin
		offset    = image.Point{25, 25}
	)
	draw.Draw(mask, mask.Bounds(), &image.Uniform{color.Alpha{255}}, image.Point{}, draw.Src)

	scannerGV.SetColor(color.RGBA{255, 255, 255, 255})
	scannerGV.SetMask(mask)
	scannerGV.SetMaskOffset(offset)
	AddRect(20, 20, 40, 40, 0, f)
	f.Draw()
	f.Clear()

	// The mask occupies destination rect (30, 32)-(70, 72); the path
	// occupies (20, 20)-(40, 40). Their intersection is (30, 32)-(40, 40).
	inside := []image.Point{{35, 35}, {30, 32}, {39, 39}, {30, 39}, {39, 32}}
	for _, p := range inside {
		checkPix(t, img, p.X, p.Y, 0xffff, 0xffff, 0xffff, 0xffff, 0x101)
	}
	// Inside the path but outside the shifted mask: nothing drawn.
	outside := []image.Point{{25, 35}, {29, 35}, {35, 25}, {35, 31}, {22, 22}}
	for _, p := range outside {
		checkPix(t, img, p.X, p.Y, 0, 0, 0, 0, 0)
	}
	// The exact mask boundary column: mask x=30 is in, x=29 is out.
	checkPix(t, img, 30, 35, 0xffff, 0xffff, 0xffff, 0xffff, 0x101)
	checkPix(t, img, 29, 35, 0, 0, 0, 0, 0)
	// Destination pixels beyond the rasterizer size must be untouched.
	checkPix(t, img, 70, 70, 0, 0, 0, 0, 0)
}

// TestMaskEdgeCases checks the defined results for nil, empty and
// non-intersecting masks, out-of-bounds sampling, repeated Draw/Clear,
// and that clearing the mask restores the exact unmasked behavior.
func TestMaskEdgeCases(t *testing.T) {
	const w, h = 48, 48
	drawRect := func(setMask func(*ScannerGV)) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		scannerGV := NewScannerGV(w, h, img, img.Bounds())
		f := NewFiller(w, h, scannerGV)
		scannerGV.SetColor(color.RGBA{255, 0, 0, 255})
		if setMask != nil {
			setMask(scannerGV)
		}
		AddRect(8, 8, 40, 40, 0, f)
		f.Draw()
		f.Clear()
		return img
	}
	imagesEqual := func(a, b *image.RGBA) bool {
		return bytes.Equal(a.Pix, b.Pix)
	}

	full := image.NewAlpha(image.Rect(0, 0, w, h))
	draw.Draw(full, full.Bounds(), &image.Uniform{color.Alpha{255}}, image.Point{}, draw.Src)

	// A fully opaque mask must give exactly the unmasked result, and so
	// must an explicitly nil mask.
	plain := drawRect(nil)
	if got := drawRect(func(s *ScannerGV) { s.SetMask(full) }); !imagesEqual(plain, got) {
		t.Error("opaque mask result differs from unmasked result")
	}
	if got := drawRect(func(s *ScannerGV) { s.SetMask(nil) }); !imagesEqual(plain, got) {
		t.Error("nil mask result differs from unmasked result")
	}

	// An empty mask and a mask that does not intersect the destination
	// must both leave the destination untouched.
	blank := image.NewRGBA(image.Rect(0, 0, w, h))
	if got := drawRect(func(s *ScannerGV) {
		s.SetMask(image.NewAlpha(image.Rectangle{}))
	}); !imagesEqual(blank, got) {
		t.Error("empty mask did not leave destination untouched")
	}
	if got := drawRect(func(s *ScannerGV) {
		s.SetMask(image.NewAlpha(image.Rect(100, 100, 120, 120)))
	}); !imagesEqual(blank, got) {
		t.Error("non-intersecting mask did not leave destination untouched")
	}

	// A mask smaller than the path: out-of-bounds samples are zero, so
	// only the mask rect is drawn.
	half := image.NewAlpha(image.Rect(0, 0, 24, 48))
	draw.Draw(half, half.Bounds(), &image.Uniform{color.Alpha{255}}, image.Point{}, draw.Src)
	got := drawRect(func(s *ScannerGV) { s.SetMask(half) })
	checkPix(t, got, 20, 24, 0xffff, 0, 0, 0xffff, 0x101)
	checkPix(t, got, 30, 24, 0, 0, 0, 0, 0)

	// Draw, Clear, Draw must give the same result as a single Draw, and
	// the mask must survive Clear.
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	scannerGV := NewScannerGV(w, h, img, img.Bounds())
	f := NewFiller(w, h, scannerGV)
	scannerGV.SetColor(color.RGBA{255, 0, 0, 255})
	scannerGV.SetMask(half)
	AddRect(8, 8, 40, 40, 0, f)
	f.Draw()
	f.Clear()
	AddRect(8, 8, 40, 40, 0, f)
	f.Draw()
	f.Clear()
	if !imagesEqual(got, img) {
		t.Error("Draw/Clear/Draw with mask differs from a single Draw")
	}

	// Drawing twice without Clear must equal two sequential masked
	// draws (the defined accumulation behavior).
	img2 := image.NewRGBA(image.Rect(0, 0, w, h))
	sc2 := NewScannerGV(w, h, img2, img2.Bounds())
	f2 := NewFiller(w, h, sc2)
	sc2.SetColor(color.RGBA{255, 0, 0, 255})
	sc2.SetMask(half)
	AddRect(8, 8, 40, 40, 0, f2)
	f2.Draw()
	f2.Draw()
	f2.Clear()
	img3 := image.NewRGBA(image.Rect(0, 0, w, h))
	sc3 := NewScannerGV(w, h, img3, img3.Bounds())
	f3 := NewFiller(w, h, sc3)
	sc3.SetColor(color.RGBA{255, 0, 0, 255})
	sc3.SetMask(half)
	AddRect(8, 8, 40, 40, 0, f3)
	f3.Draw()
	f3.Clear()
	AddRect(8, 8, 40, 40, 0, f3)
	f3.Draw()
	f3.Clear()
	if !imagesEqual(img2, img3) {
		t.Error("double Draw without Clear differs from two sequential draws")
	}

	// After removing the mask the scanner must behave exactly like a
	// fresh unmasked scanner.
	scannerGV.SetMask(nil)
	AddRect(8, 8, 40, 40, 0, f)
	img4 := image.NewRGBA(image.Rect(0, 0, w, h))
	scannerGV.Dest = img4
	f.Draw()
	f.Clear()
	if !imagesEqual(plain, img4) {
		t.Error("clearing the mask did not restore unmasked behavior")
	}
}

// TestMaskOffsetReporting is a small sanity check on the offset getter.
func TestMaskOffsetReporting(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	scannerGV := NewScannerGV(8, 8, img, img.Bounds())
	if got := scannerGV.MaskOffset(); got != (image.Point{}) {
		t.Error("default mask offset not zero:", got)
	}
	scannerGV.SetMaskOffset(image.Point{3, -7})
	if got := scannerGV.MaskOffset(); got != (image.Point{3, -7}) {
		t.Error("mask offset not reported back:", got)
	}
}
