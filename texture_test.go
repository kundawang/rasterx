// Tests for the texture pattern (texture.go)
package rasterx_test

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/srwiley/rasterx"
)

var (
	texRed    = color.RGBA{255, 0, 0, 255}
	texGreen  = color.RGBA{0, 255, 0, 255}
	texBlue   = color.RGBA{0, 0, 255, 255}
	texYellow = color.RGBA{255, 255, 0, 255}
	texBlack  = color.RGBA{0, 0, 0, 255}
	texWhite  = color.RGBA{255, 255, 255, 255}
)

// quadTexture returns a 2x2 texture: TL red, TR green, BL blue, BR yellow.
func quadTexture() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, texRed)
	img.SetRGBA(1, 0, texGreen)
	img.SetRGBA(0, 1, texBlue)
	img.SetRGBA(1, 1, texYellow)
	return img
}

// checkerTexture returns a wxh checkerboard of c1 and c2.
func checkerTexture(w, h int, c1, c2 color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x+y)%2 == 0 {
				img.SetRGBA(x, y, c1)
			} else {
				img.SetRGBA(x, y, c2)
			}
		}
	}
	return img
}

// stubImage is an image.Image with no backing store, used to present
// extreme bounds without allocating memory.
type stubImage struct{ r image.Rectangle }

func (s stubImage) ColorModel() color.Model { return color.RGBAModel }
func (s stubImage) Bounds() image.Rectangle { return s.r }
func (s stubImage) At(x, y int) color.Color { return color.RGBA{} }

// mustColorFunc extracts a ColorFunc from a GetColorFunctionUS result.
func mustColorFunc(t *testing.T, p *Pattern, opacity float64, m Matrix2D) ColorFunc {
	t.Helper()
	cf, ok := p.GetColorFunctionUS(opacity, m).(ColorFunc)
	if !ok {
		t.Fatalf("expected ColorFunc from pattern, got %T", p.GetColorFunctionUS(opacity, m))
	}
	return cf
}

func checkColor(t *testing.T, cf ColorFunc, x, y int, want color.RGBA) {
	t.Helper()
	got := cf(x, y)
	gr, gg, gb, ga := got.RGBA()
	wr, wg, wb, wa := want.RGBA()
	if gr != wr || gg != wg || gb != wb || ga != wa {
		t.Errorf("at (%d,%d): got %v, want %v", x, y, got, want)
	}
}

func TestNewPatternErrors(t *testing.T) {
	if _, err := NewPattern(nil, Identity, TileRepeat, TileRepeat, NearestFilter); err != ErrPatternNilSrc {
		t.Error("nil source should return ErrPatternNilSrc, got", err)
	}
	empty := image.NewRGBA(image.Rect(0, 0, 0, 0))
	if _, err := NewPattern(empty, Identity, TileRepeat, TileRepeat, NearestFilter); err != ErrPatternEmptySrc {
		t.Error("empty source should return ErrPatternEmptySrc, got", err)
	}
	// Empty image with non-zero origin
	empty2 := image.NewRGBA(image.Rect(5, 5, 5, 9))
	if _, err := NewPattern(empty2, Identity, TileRepeat, TileRepeat, NearestFilter); err != ErrPatternEmptySrc {
		t.Error("zero-width source should return ErrPatternEmptySrc, got", err)
	}
	huge := stubImage{image.Rect(0, 0, 1<<20, 1<<20)}
	if _, err := NewPattern(huge, Identity, TileRepeat, TileRepeat, NearestFilter); err != ErrPatternTooLarge {
		t.Error("huge source should return ErrPatternTooLarge, got", err)
	}
	singular := Matrix2D{0, 0, 0, 0, 0, 0}
	if _, err := NewPattern(quadTexture(), singular, TileRepeat, TileRepeat, NearestFilter); err != ErrPatternBadMatrix {
		t.Error("singular matrix should return ErrPatternBadMatrix, got", err)
	}
	nan := Matrix2D{math.NaN(), 0, 0, 1, 0, 0}
	if _, err := NewPattern(quadTexture(), nan, TileRepeat, TileRepeat, NearestFilter); err != ErrPatternBadMatrix {
		t.Error("NaN matrix should return ErrPatternBadMatrix, got", err)
	}
}

func TestPatternFallbacks(t *testing.T) {
	// Zero value pattern yields the transparent fallback, not a panic.
	var p Pattern
	c, ok := p.GetColorFunction(1.0).(color.RGBA)
	if !ok || c != (color.RGBA{}) {
		t.Error("zero pattern should return transparent fallback, got", c)
	}
	// Singular user space matrix also falls back.
	p2, err := NewPattern(quadTexture(), Identity, TileRepeat, TileRepeat, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	c2, ok := p2.GetColorFunctionUS(1.0, Matrix2D{0, 0, 0, 0, 0, 0}).(color.RGBA)
	if !ok || c2 != (color.RGBA{}) {
		t.Error("singular objMatrix should return transparent fallback, got", c2)
	}
	// Zero opacity falls back too.
	if c3, ok := p2.GetColorFunction(0).(color.RGBA); !ok || c3 != (color.RGBA{}) {
		t.Error("zero opacity should return transparent fallback, got", c3)
	}
}

func TestPatternRepeatNearest(t *testing.T) {
	p, err := NewPattern(checkerTexture(2, 2, texBlack, texWhite),
		Identity, TileRepeat, TileRepeat, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	posMod := func(i int) int {
		i %= 2
		if i < 0 {
			i += 2
		}
		return i
	}
	// Includes negative device coordinates.
	for y := -5; y < 8; y++ {
		for x := -5; x < 8; x++ {
			want := texWhite
			if (posMod(x)+posMod(y))%2 == 0 {
				want = texBlack
			}
			checkColor(t, cf, x, y, want)
		}
	}
}

func TestPatternReflect(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 1))
	src.SetRGBA(0, 0, texRed)
	src.SetRGBA(1, 0, texGreen)
	src.SetRGBA(2, 0, texBlue)
	p, err := NewPattern(src, Identity, TileReflect, TileClamp, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	// Mirrored copies include the edge texels, like gradient reflect spread.
	wantSeq := []color.RGBA{texRed, texGreen, texBlue, texBlue, texGreen, texRed, texRed, texGreen}
	for i, want := range wantSeq {
		checkColor(t, cf, i, 0, want)
	}
	// Negative side mirrors too: x=-1 -> texel 0, x=-2 -> texel 1, x=-3 -> texel 2
	checkColor(t, cf, -1, 0, texRed)
	checkColor(t, cf, -2, 0, texGreen)
	checkColor(t, cf, -3, 0, texBlue)
}

func TestPatternClamp(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 1))
	src.SetRGBA(0, 0, texRed)
	src.SetRGBA(1, 0, texGreen)
	src.SetRGBA(2, 0, texBlue)
	p, err := NewPattern(src, Identity, TileClamp, TileClamp, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	checkColor(t, cf, -100, 0, texRed)
	checkColor(t, cf, -1, 0, texRed)
	checkColor(t, cf, 0, 0, texRed)
	checkColor(t, cf, 1, 0, texGreen)
	checkColor(t, cf, 2, 0, texBlue)
	checkColor(t, cf, 3, 0, texBlue)
	checkColor(t, cf, 100, 0, texBlue)
}

func TestPatternBilinear(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.SetRGBA(0, 0, texBlack)
	src.SetRGBA(1, 0, texWhite)
	p, err := NewPattern(src, Identity, TileRepeat, TileClamp, BilinearFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	// At texel centers the result is exact.
	checkColor(t, cf, 0, 0, texBlack)
	checkColor(t, cf, 1, 0, texWhite)

	// Shift the pattern by half a texel so that device pixel (0,0)
	// samples exactly halfway between the black and white texels.
	pt, err := NewPattern(src, Identity.Translate(-0.5, 0), TileRepeat, TileClamp, BilinearFilter)
	if err != nil {
		t.Fatal(err)
	}
	cft := mustColorFunc(t, pt, 1.0, Identity)
	got := cft(0, 0).(color.RGBA)
	if got.R < 126 || got.R > 130 || got.G != got.R || got.B != got.R || got.A != 255 {
		t.Errorf("bilinear midpoint should be ~128 gray, got %v", got)
	}
}

func TestPatternBilinearPremultiplied(t *testing.T) {
	// NRGBA source: opaque red next to fully transparent "red". The
	// transparent texel's color channels must not leak into the blend.
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	src.SetNRGBA(1, 0, color.NRGBA{255, 0, 0, 0})
	p, err := NewPattern(src, Identity.Translate(-0.5, 0), TileRepeat, TileClamp, BilinearFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	got := cf(0, 0).(color.RGBA)
	// Premultiplied blend of (255,0,0,255) and (0,0,0,0) at t=0.5.
	if got.R < 126 || got.R > 130 || got.A < 126 || got.A > 130 || got.G != 0 || got.B != 0 {
		t.Errorf("premultiplied midpoint should be ~(128,0,0,128), got %v", got)
	}
}

func TestPatternNonZeroOrigin(t *testing.T) {
	src := image.NewRGBA(image.Rect(3, 5, 5, 7))
	src.SetRGBA(3, 5, texRed)
	src.SetRGBA(4, 5, texGreen)
	src.SetRGBA(3, 6, texBlue)
	src.SetRGBA(4, 6, texYellow)
	p, err := NewPattern(src, Identity, TileClamp, TileClamp, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	checkColor(t, cf, 0, 0, texRed)
	checkColor(t, cf, 1, 0, texGreen)
	checkColor(t, cf, 0, 1, texBlue)
	checkColor(t, cf, 1, 1, texYellow)
}

func TestPatternTranslate(t *testing.T) {
	p, err := NewPattern(quadTexture(), Identity.Translate(1, 0),
		TileRepeat, TileRepeat, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	// Shifting the pattern right by one texel swaps the columns.
	checkColor(t, cf, 0, 0, texGreen)
	checkColor(t, cf, 1, 0, texRed)
	checkColor(t, cf, 0, 1, texYellow)
	checkColor(t, cf, 1, 1, texBlue)
}

func TestPatternRotate(t *testing.T) {
	// Rotate the pattern 90 degrees about texel-space point (1,1).
	p, err := NewPattern(quadTexture(),
		Identity.Translate(1, 1).Rotate(math.Pi/2).Translate(-1, -1),
		TileClamp, TileClamp, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 1.0, Identity)
	// A +90 degree rotation (clockwise in y-down device space) moves
	// BL->TL, TL->TR, BR->BL, TR->BR.
	checkColor(t, cf, 0, 0, texBlue)
	checkColor(t, cf, 1, 0, texRed)
	checkColor(t, cf, 0, 1, texYellow)
	checkColor(t, cf, 1, 1, texGreen)
}

func TestPatternOpacity(t *testing.T) {
	p, err := NewPattern(quadTexture(), Identity, TileClamp, TileClamp, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustColorFunc(t, p, 0.5, Identity)
	got := cf(0, 0).(color.RGBA)
	// Opaque red at 50% opacity in premultiplied form.
	if got.R < 126 || got.R > 130 || got.A < 126 || got.A > 130 || got.G != 0 || got.B != 0 {
		t.Errorf("50%% opacity red should be ~(128,0,0,128), got %v", got)
	}
}

func TestPatternSingleTexel(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1, 1))
	src.SetRGBA(0, 0, texGreen)
	for _, mode := range []TileMode{TileRepeat, TileReflect, TileClamp} {
		p, err := NewPattern(src, Identity, mode, mode, BilinearFilter)
		if err != nil {
			t.Fatal(err)
		}
		cf := mustColorFunc(t, p, 1.0, Identity)
		for _, pt := range [][2]int{{-7, 3}, {0, 0}, {5, -2}, {100, 100}} {
			checkColor(t, cf, pt[0], pt[1], texGreen)
		}
	}
}

// fillRect fills the whole image with the given color source and returns
// the image and its bounds before drawing.
func renderFill(t *testing.T, w, h int, clr interface{}, bg *color.RGBA) (*image.RGBA, image.Rectangle) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if bg != nil {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetRGBA(x, y, *bg)
			}
		}
	}
	before := img.Bounds()
	scanner := NewScannerGV(w, h, img, img.Bounds())
	f := NewFiller(w, h, scanner)
	scanner.SetColor(clr)
	AddRect(0, 0, float64(w), float64(h), 0, f)
	f.Draw()
	return img, before
}

func TestPatternFillIntegration(t *testing.T) {
	p, err := NewPattern(checkerTexture(2, 2, texBlack, texWhite),
		Identity, TileRepeat, TileRepeat, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	img, before := renderFill(t, 8, 8, p.GetColorFunction(1.0), nil)
	if img.Bounds() != before {
		t.Fatal("pattern fill changed the destination image bounds")
	}
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			want := texWhite
			if (x+y)%2 == 0 {
				want = texBlack
			}
			if got := img.RGBAAt(x, y); got != want {
				t.Fatalf("fill at (%d,%d): got %v, want %v", x, y, got, want)
			}
		}
	}
}

func TestPatternTranslucentFillIntegration(t *testing.T) {
	// 1x1 semi-transparent red over an opaque blue background.
	src := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	src.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 128})
	p, err := NewPattern(src, Identity, TileRepeat, TileRepeat, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	bg := color.RGBA{0, 0, 255, 255}
	img, _ := renderFill(t, 4, 4, p.GetColorFunction(1.0), &bg)
	// src over dst: a = 128/255, so r ~ 128, b ~ 127, a = 255.
	got := img.RGBAAt(2, 2)
	if got.A != 255 || got.R < 126 || got.R > 130 || got.B < 125 || got.B > 129 || got.G != 0 {
		t.Errorf("translucent composite should be ~(128,0,127,255), got %v", got)
	}
}

func TestPatternStrokeIntegration(t *testing.T) {
	const w, h = 32, 16
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	scanner := NewScannerGV(w, h, img, img.Bounds())
	st := NewStroker(w, h, scanner)
	st.SetStroke(4*64, 4*64, ButtCap, nil, FlatGap, MiterClip)

	p, err := NewPattern(quadTexture(), Identity, TileRepeat, TileRepeat, NearestFilter)
	if err != nil {
		t.Fatal(err)
	}
	scanner.SetColor(p.GetColorFunction(1.0))

	st.Start(ToFixedP(2, 8))
	st.Line(ToFixedP(30, 8))
	st.Stop(false)
	st.Draw()

	// The stroke is 4px wide centered on y=8, so (10,8) is well inside
	// and must carry the texture color for texel (10%2, 8%2) = (0,0).
	if got := img.RGBAAt(10, 8); got != texRed {
		t.Errorf("stroke interior should be texture red, got %v", got)
	}
	if got := img.RGBAAt(11, 8); got != texGreen {
		t.Errorf("stroke interior should be texture green, got %v", got)
	}
	// Far from the stroke the background is untouched.
	if got := img.RGBAAt(10, 0); got != (color.RGBA{}) {
		t.Errorf("outside stroke should be untouched, got %v", got)
	}
}
