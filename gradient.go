// Copyright 2018 The oksvg Authors. All rights reserved.
//
// created: 5/12/2018 by S.R.Wiley
package rasterx

import (
	"image/color"
	"math"
	"sort"
)

const (
	ObjectBoundingBox GradientUnits = iota
	UserSpaceOnUse
)

const (
	PadSpread SpreadMethod = iota
	ReflectSpread
	RepeatSpread
)

const (
	// SRGBInterpolation interpolates stop colors directly in sRGB space.
	// This is the zero value and reproduces the historical rasterx output
	// pixel for pixel.
	SRGBInterpolation GradientInterpolation = iota
	// LinearLightInterpolation interpolates stop colors in linear-light
	// space, decoding each stop with the sRGB EOTF (or Gradient.Gamma when
	// it is a positive finite value) and re-encoding the blended result.
	// This avoids the dark midtones produced by sRGB-space interpolation,
	// e.g. when a translucent gradient is composited over a dark UI.
	LinearLightInterpolation
	// PremulAlphaInterpolation interpolates alpha-premultiplied sRGB
	// values, so the hue of a (partially) transparent stop does not bleed
	// into neighboring pixels; only its opacity contributes.
	PremulAlphaInterpolation
)

const epsilonF = 1e-5

type (
	SpreadMethod  byte
	GradientUnits byte

	// GradientInterpolation selects the color space in which gradient
	// stops are blended. See the SRGBInterpolation etc. constants.
	GradientInterpolation byte

	GradStop struct {
		StopColor color.Color
		Offset    float64
		Opacity   float64
	}

	Gradient struct {
		Points   [5]float64
		Stops    []GradStop
		Bounds   struct{ X, Y, W, H float64 }
		Matrix   Matrix2D
		Spread   SpreadMethod
		Units    GradientUnits
		IsRadial bool
		// Interpolation selects how stop colors are blended. The zero
		// value (SRGBInterpolation) preserves the historical output.
		Interpolation GradientInterpolation
		// Gamma optionally replaces the exact sRGB transfer function used
		// by LinearLightInterpolation with a simple power law of the given
		// exponent (e.g. 2.2). Values that are not positive and finite
		// (zero, negative, NaN, Inf) are ignored and the exact sRGB
		// transfer function is used. Gamma has no effect in the other
		// interpolation modes.
		Gamma float64
	}
)

// clamp01 clamps v to [0, 1]; NaN maps to 0. This gives non-finite
// opacities and offsets a deterministic result instead of the
// implementation-defined float-to-int conversion.
func clamp01(v float64) float64 {
	switch {
	case math.IsNaN(v) || v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// sanitizeOffset maps non-finite stop offsets to deterministic values so
// that sorting and blending stay well defined. Finite offsets — including
// offsets outside [0, 1] — are passed through unchanged.
func sanitizeOffset(o float64) float64 {
	switch {
	case math.IsNaN(o) || math.IsInf(o, -1):
		return 0
	case math.IsInf(o, 1):
		return 1
	}
	return o
}

// to8 converts a [0, 1] float to a uint8 with round-to-nearest, clamped to
// [0, 0xFF]; NaN maps to 0.
func to8(v float64) uint8 {
	switch {
	case math.IsNaN(v) || v <= 0:
		return 0
	case v >= 1:
		return 0xFF
	}
	return uint8(math.Round(v * 0xFF))
}

// srgbDecode is the sRGB EOTF; input is clamped to [0, 1].
func srgbDecode(c float64) float64 {
	c = clamp01(c)
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// srgbEncode is the inverse sRGB OETF; input is clamped to [0, 1].
func srgbEncode(c float64) float64 {
	c = clamp01(c)
	if c <= 0.0031308 {
		return 12.92 * c
	}
	return 1.055*math.Pow(c, 1.0/2.4) - 0.055
}

// transferFuncs returns the decode/encode pair for LinearLightInterpolation.
// A positive finite gamma selects a simple power law; anything else
// (including NaN, Inf, zero, negative, or absurdly large exponents) falls
// back to the exact sRGB transfer functions, so no input can produce NaN.
func transferFuncs(gamma float64) (dec, enc func(float64) float64) {
	if gamma > 0 && !math.IsInf(gamma, 0) && !math.IsNaN(gamma) {
		inv := 1 / gamma
		return func(c float64) float64 { return math.Pow(clamp01(c), gamma) },
			func(c float64) float64 { return clamp01(math.Pow(clamp01(c), inv)) }
	}
	return srgbDecode, srgbEncode
}

func ApplyOpacity(c color.Color, opacity float64) color.NRGBA {
	r, g, b, _ := c.RGBA()
	return color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(clamp01(opacity) * 0xFF)}
}

// stopVals holds the precomputed per-stop channel values used by the
// non-legacy interpolation modes. All values are in [0, 1]; a includes the
// stop color's own alpha, the stop opacity and the overall opacity.
type stopVals struct {
	r, g, b, a float64 // non-premultiplied sRGB
	lr, lg, lb float64 // linear-light
	pr, pg, pb float64 // alpha-premultiplied sRGB
}

// stopColorizer evaluates the gradient's color for a parameterized
// position t. It is immutable after construction, so the color functions
// returned by GetColorFunctionUS are safe for concurrent use and are
// unaffected by later changes to the Gradient.
type stopColorizer struct {
	stops   []GradStop // sorted, sanitized copy of the gradient's stops
	vals    []stopVals // nil in SRGBInterpolation (legacy) mode
	spread  SpreadMethod
	mode    GradientInterpolation
	opacity float64
	encode  func(float64) float64
}

// newStopColorizer builds a colorizer from stops, which must already be
// copied, sanitized and sorted by offset. Unrecognized mode values fall
// back to the legacy sRGB behavior.
func newStopColorizer(stops []GradStop, spread SpreadMethod,
	mode GradientInterpolation, opacity, gamma float64) *stopColorizer {
	c := &stopColorizer{stops: stops, spread: spread, mode: mode, opacity: opacity}
	if mode != LinearLightInterpolation && mode != PremulAlphaInterpolation {
		c.mode = SRGBInterpolation
		return c
	}
	dec, enc := transferFuncs(gamma)
	c.encode = enc
	c.vals = make([]stopVals, len(stops))
	for i, s := range stops {
		nrgba := color.NRGBAModel.Convert(s.StopColor).(color.NRGBA)
		v := stopVals{
			r: float64(nrgba.R) / 0xFF,
			g: float64(nrgba.G) / 0xFF,
			b: float64(nrgba.B) / 0xFF,
			a: float64(nrgba.A) / 0xFF * s.Opacity * opacity,
		}
		v.lr, v.lg, v.lb = dec(v.r), dec(v.g), dec(v.b)
		v.pr, v.pg, v.pb = v.r*v.a, v.g*v.a, v.b*v.a
		c.vals[i] = v
	}
	return c
}

// pad returns the color of a single stop, used where no blending occurs.
func (c *stopColorizer) pad(i int) color.Color {
	s := c.stops[i]
	if c.mode == SRGBInterpolation {
		return ApplyOpacity(s.StopColor, s.Opacity*c.opacity)
	}
	v := c.vals[i]
	return color.NRGBA{to8(v.r), to8(v.g), to8(v.b), to8(v.a)}
}

// blend interpolates between stops i1 and i2 at parameterized position t.
// The SRGBInterpolation path reproduces the historical rasterx arithmetic
// exactly; the other modes blend in float64 and round to nearest.
func (c *stopColorizer) blend(t float64, i1, i2 int, flip bool) color.Color {
	s1, s2 := c.stops[i1], c.stops[i2]
	s1off := s1.Offset
	if s1.Offset > s2.Offset && !flip { // happens in repeat spread mode
		s1off -= 1
		if t > 1 {
			t -= 1
		}
	}
	if s2.Offset == s1off {
		if c.mode == SRGBInterpolation {
			// Historical behavior: the overall opacity is not applied here.
			return ApplyOpacity(s2.StopColor, s2.Opacity)
		}
		return c.pad(i2)
	}
	if flip {
		t = 1 - t
	}
	tp := (t - s1off) / (s2.Offset - s1off)

	if c.mode == SRGBInterpolation {
		r1, g1, b1, _ := s1.StopColor.RGBA()
		r2, g2, b2, _ := s2.StopColor.RGBA()
		return ApplyOpacity(color.RGBA{
			uint8((float64(r1)*(1-tp) + float64(r2)*tp) / 256),
			uint8((float64(g1)*(1-tp) + float64(g2)*tp) / 256),
			uint8((float64(b1)*(1-tp) + float64(b2)*tp) / 256),
			0xFF}, (s1.Opacity*(1-tp)+s2.Opacity*tp)*c.opacity)
	}

	tp = clamp01(tp)
	v1, v2 := c.vals[i1], c.vals[i2]
	a := v1.a*(1-tp) + v2.a*tp
	if c.mode == LinearLightInterpolation {
		lr := v1.lr*(1-tp) + v2.lr*tp
		lg := v1.lg*(1-tp) + v2.lg*tp
		lb := v1.lb*(1-tp) + v2.lb*tp
		return color.NRGBA{to8(c.encode(lr)), to8(c.encode(lg)), to8(c.encode(lb)), to8(a)}
	}
	// PremulAlphaInterpolation
	pr := v1.pr*(1-tp) + v2.pr*tp
	pg := v1.pg*(1-tp) + v2.pg*tp
	pb := v1.pb*(1-tp) + v2.pb*tp
	if a <= 0 {
		return color.NRGBA{0, 0, 0, 0}
	}
	return color.NRGBA{to8(pr / a), to8(pg / a), to8(pb / a), to8(a)}
}

// colorAt takes the parameterized value along the gradient's stops and
// returns a color depending on the spread method and the stop list.
// Non-finite t is mapped to a deterministic result and never produces NaN.
func (c *stopColorizer) colorAt(t float64) color.Color {
	if math.IsNaN(t) {
		t = 0
	}
	d := len(c.stops)
	// These cases can be taken care of early on
	if t >= 1.0 && c.spread == PadSpread {
		return c.pad(d - 1)
	}
	if t <= 0.0 && c.spread == PadSpread {
		return c.pad(0)
	}

	var modRange float64 = 1.0
	if c.spread == ReflectSpread {
		modRange = 2.0
	}
	mod := math.Mod(t, modRange)
	if mod < 0 {
		mod += modRange
	}
	if math.IsNaN(mod) { // t was infinite
		mod = 0
	}

	place := 0 // Advance to place where mod is greater than the indicated stop
	for place != d && mod > c.stops[place].Offset {
		place++
	}
	switch c.spread {
	case RepeatSpread:
		switch place {
		case 0, d:
			return c.blend(mod, d-1, 0, false)
		default:
			return c.blend(mod, place-1, place, false)
		}
	case ReflectSpread:
		switch place {
		case 0:
			return c.pad(0)
		case d:
			// Advance to place where mod-1 is greater than the stop indicated by place in reverse of the stop slice.
			// Since this is the reflect spead mode, the mod interval is two, allowing the stop list to be
			// iterated in reverse before repeating the sequence.
			for place != d*2 && mod-1 > (1-c.stops[d*2-place-1].Offset) {
				place++
			}
			switch place {
			case d:
				return c.pad(d - 1)
			case d * 2:
				return c.pad(0)
			default:
				return c.blend(mod-1, d*2-place, d*2-place-1, true)
			}
		default:
			return c.blend(mod, place-1, place, false)
		}
	default: // PadSpread
		switch place {
		case 0:
			return c.pad(0)
		case d:
			return c.pad(d - 1)
		default:
			return c.blend(mod, place-1, place, false)
		}
	}
}

func (g *Gradient) GetColorFunction(opacity float64) interface{} {
	return g.GetColorFunctionUS(opacity, Identity)
}

func (g *Gradient) GetColorFunctionUS(opacity float64, objMatrix Matrix2D) interface{} {
	opacity = clamp01(opacity)
	switch len(g.Stops) {
	case 0:
		return ApplyOpacity(color.RGBA{255, 0, 255, 255}, opacity) // default error color for gradient w/o stops.
	case 1:
		return ApplyOpacity(g.Stops[0].StopColor, opacity) // Illegal, I think, should really should not happen.
	}

	// Sort a private copy of the stops by offset in ascending order;
	// the gradient's own Stops slice is never modified, so the returned
	// color function is safe to use concurrently and the caller's slice
	// keeps its original order. The sort is stable so that duplicate
	// offsets give deterministic results. Non-finite offsets and
	// out-of-range opacities are sanitized on the copy.
	stops := make([]GradStop, len(g.Stops))
	copy(stops, g.Stops)
	for i := range stops {
		stops[i].Offset = sanitizeOffset(stops[i].Offset)
		stops[i].Opacity = clamp01(stops[i].Opacity)
	}
	sort.SliceStable(stops, func(i, j int) bool {
		return stops[i].Offset < stops[j].Offset
	})

	tcz := newStopColorizer(stops, g.Spread, g.Interpolation, opacity, g.Gamma)
	tc := tcz.colorAt
	// lastStopColor reproduces the historical fallback used by the radial
	// intersection code paths below.
	lastStopColor := func() color.Color { return tcz.pad(len(stops) - 1) }

	w, h := float64(g.Bounds.W), float64(g.Bounds.H)
	oriX, oriY := float64(g.Bounds.X), float64(g.Bounds.Y)
	gradT := Identity.Translate(oriX, oriY).Scale(w, h).
		Mult(g.Matrix).Scale(1/w, 1/h).Translate(-oriX, -oriY).Invert()

	if g.IsRadial {
		cx, cy, fx, fy, rx, ry := g.Points[0], g.Points[1], g.Points[2], g.Points[3], g.Points[4], g.Points[4]
		if g.Units == ObjectBoundingBox {
			cx = g.Bounds.X + g.Bounds.W*cx
			cy = g.Bounds.Y + g.Bounds.H*cy
			fx = g.Bounds.X + g.Bounds.W*fx
			fy = g.Bounds.Y + g.Bounds.H*fy
			rx *= g.Bounds.W
			ry *= g.Bounds.H
		} else {
			cx, cy = g.Matrix.Transform(cx, cy)
			rx, ry = g.Matrix.TransformVector(rx, ry)
			cx, cy = objMatrix.Transform(cx, cy)
			rx, ry = objMatrix.TransformVector(rx, ry)
		}

		if cx == fx && cy == fy {
			// When the focus and center are the same things are much simpler;
			// t is just distance from center
			// scaled by the bounds aspect ratio times r
			if g.Units == ObjectBoundingBox {
				return ColorFunc(func(xi, yi int) color.Color {
					x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
					dx := float64(x) - cx
					dy := float64(y) - cy
					return tc(math.Sqrt(dx*dx/(rx*rx) + dy*dy/(ry*ry)))
				})
			} else {
				return ColorFunc(func(xi, yi int) color.Color {
					x := float64(xi) + 0.5
					y := float64(yi) + 0.5
					dx := x - cx
					dy := y - cy
					return tc(math.Sqrt(dx*dx/(rx*rx) + dy*dy/(ry*ry)))
				})
			}
		} else {
			fx /= rx
			fy /= ry
			cx /= rx
			cy /= ry

			dfx := fx - cx
			dfy := fy - cy

			if dfx*dfx+dfy*dfy > 1 { // Focus outside of circle; use intersection
				// point of line from center to focus and circle as per SVG specs.
				nfx, nfy, intersects := RayCircleIntersectionF(fx, fy, cx, cy, cx, cy, 1.0-epsilonF)
				fx, fy = nfx, nfy
				if intersects == false {
					return color.RGBA{255, 255, 0, 255} // should not happen
				}
			}
			if g.Units == ObjectBoundingBox {
				return ColorFunc(func(xi, yi int) color.Color {
					x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
					ex := x / rx
					ey := y / ry

					t1x, t1y, intersects := RayCircleIntersectionF(ex, ey, fx, fy, cx, cy, 1.0)
					if intersects == false { //In this case, use the last stop color
						return lastStopColor()
					}
					tdx, tdy := t1x-fx, t1y-fy
					dx, dy := ex-fx, ey-fy
					if tdx*tdx+tdy*tdy < epsilonF {
						return lastStopColor()
					}
					return tc(math.Sqrt(dx*dx+dy*dy) / math.Sqrt(tdx*tdx+tdy*tdy))
				})
			} else {
				return ColorFunc(func(xi, yi int) color.Color {
					x := float64(xi) + 0.5
					y := float64(yi) + 0.5
					ex := x / rx
					ey := y / ry

					t1x, t1y, intersects := RayCircleIntersectionF(ex, ey, fx, fy, cx, cy, 1.0)
					if intersects == false { //In this case, use the last stop color
						return lastStopColor()
					}
					tdx, tdy := t1x-fx, t1y-fy
					dx, dy := ex-fx, ey-fy
					if tdx*tdx+tdy*tdy < epsilonF {
						return lastStopColor()
					}
					return tc(math.Sqrt(dx*dx+dy*dy) / math.Sqrt(tdx*tdx+tdy*tdy))
				})
			}
		}
	} else {
		p1x, p1y, p2x, p2y := g.Points[0], g.Points[1], g.Points[2], g.Points[3]
		if g.Units == ObjectBoundingBox {
			p1x = g.Bounds.X + g.Bounds.W*p1x
			p1y = g.Bounds.Y + g.Bounds.H*p1y
			p2x = g.Bounds.X + g.Bounds.W*p2x
			p2y = g.Bounds.Y + g.Bounds.H*p2y

			dx := p2x - p1x
			dy := p2y - p1y
			d := (dx*dx + dy*dy) // self inner prod
			return ColorFunc(func(xi, yi int) color.Color {
				x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
				dfx := x - p1x
				dfy := y - p1y
				return tc((dx*dfx + dy*dfy) / d)
			})
		} else {
			p1x, p1y = g.Matrix.Transform(p1x, p1y)
			p2x, p2y = g.Matrix.Transform(p2x, p2y)
			p1x, p1y = objMatrix.Transform(p1x, p1y)
			p2x, p2y = objMatrix.Transform(p2x, p2y)
			dx := p2x - p1x
			dy := p2y - p1y
			d := (dx*dx + dy*dy) // self inner prod
			return ColorFunc(func(xi, yi int) color.Color {
				x := float64(xi) + 0.5
				y := float64(yi) + 0.5
				dfx := x - p1x
				dfy := y - p1y
				return tc((dx*dfx + dy*dfy) / d)
			})
		}
	}
}
