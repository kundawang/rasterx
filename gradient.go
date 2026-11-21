// Copyright 2018 The oksvg Authors. All rights reserved.
//
// created: 5/12/2018 by S.R.Wiley
package rasterx

import (
	"errors"
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

const epsilonF = 1e-5

// ErrDegenerateRadius is returned when a radial gradient has a center or
// focus that cannot define a usable normalized radius, so gradient
// coordinates would divide by zero or map to a NaN/Inf parameter.
var ErrDegenerateRadius = errors.New("rasterx: radial gradient has a degenerate radius")

type (
	SpreadMethod  byte
	GradientUnits byte

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
	}
)

func ApplyOpacity(c color.Color, opacity float64) color.NRGBA {
	r, g, b, _ := c.RGBA()
	return color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(opacity * 0xFF)}
}

// tColor takes the paramaterized value along the gradient's stops and
// returns a color depending on the spreadMethod value of the gradient and
// the gradient's slice of stop values.
func (g *Gradient) tColor(t, opacity float64) color.Color {
	// The parameter is derived from sampled coordinates. If an extreme but
	// finite transform pushes the arithmetic outside float64, clamp to a
	// deterministic endpoint rather than letting NaN reach the stop
	// comparisons and produce an unpredictable color. Positive infinity pads
	// to the last stop and negative infinity (or NaN) to the first, which is
	// the same result the pad cases below would give for finite endpoints.
	switch {
	case math.IsNaN(t):
		t = 0
	case math.IsInf(t, 1):
		t = 1
	case math.IsInf(t, -1):
		t = 0
	}
	d := len(g.Stops)
	// These cases can be taken care of early on
	if t >= 1.0 && g.Spread == PadSpread {
		s := g.Stops[d-1]
		return ApplyOpacity(s.StopColor, s.Opacity*opacity)
	}
	if t <= 0.0 && g.Spread == PadSpread {
		return ApplyOpacity(g.Stops[0].StopColor, g.Stops[0].Opacity*opacity)
	}

	var modRange float64 = 1.0
	if g.Spread == ReflectSpread {
		modRange = 2.0
	}
	mod := math.Mod(t, modRange)
	if mod < 0 {
		mod += modRange
	}

	place := 0 // Advance to place where mod is greater than the indicated stop
	for place != len(g.Stops) && mod > g.Stops[place].Offset {
		place++
	}
	switch g.Spread {
	case RepeatSpread:
		var s1, s2 GradStop
		switch place {
		case 0, d:
			s1, s2 = g.Stops[d-1], g.Stops[0]
		default:
			s1, s2 = g.Stops[place-1], g.Stops[place]
		}
		return g.blendStops(mod, opacity, s1, s2, false)
	case ReflectSpread:
		switch place {
		case 0:
			return ApplyOpacity(g.Stops[0].StopColor, g.Stops[0].Opacity*opacity)
		case d:
			// Advance to place where mod-1 is greater than the stop indicated by place in reverse of the stop slice.
			// Since this is the reflect spead mode, the mod interval is two, allowing the stop list to be
			// iterated in reverse before repeating the sequence.
			for place != d*2 && mod-1 > (1-g.Stops[d*2-place-1].Offset) {
				place++
			}
			switch place {
			case d:
				s := g.Stops[d-1]
				return ApplyOpacity(s.StopColor, s.Opacity*opacity)
			case d * 2:
				return ApplyOpacity(g.Stops[0].StopColor, g.Stops[0].Opacity*opacity)
			default:
				return g.blendStops(mod-1, opacity,
					g.Stops[d*2-place], g.Stops[d*2-place-1], true)
			}
		default:
			return g.blendStops(mod, opacity,
				g.Stops[place-1], g.Stops[place], false)
		}
	default: // PadSpread
		switch place {
		case 0:
			return ApplyOpacity(g.Stops[0].StopColor, g.Stops[0].Opacity*opacity)
		case len(g.Stops):
			s := g.Stops[len(g.Stops)-1]
			return ApplyOpacity(s.StopColor, s.Opacity*opacity)
		default:
			return g.blendStops(mod, opacity, g.Stops[place-1], g.Stops[place], false)
		}
	}
}

func (g *Gradient) blendStops(t, opacity float64, s1, s2 GradStop, flip bool) color.Color {
	s1off := s1.Offset
	if s1.Offset > s2.Offset && !flip { // happens in repeat spread mode
		s1off -= 1
		if t > 1 {
			t -= 1
		}
	}
	if s2.Offset == s1off {
		return ApplyOpacity(s2.StopColor, s2.Opacity)
	}
	if flip {
		t = 1 - t
	}
	tp := (t - s1off) / (s2.Offset - s1off)
	if math.IsNaN(tp) { // a degenerate spread; never let NaN reach a pixel
		return ApplyOpacity(s2.StopColor, s2.Opacity*opacity)
	}
	r1, g1, b1, _ := s1.StopColor.RGBA()
	r2, g2, b2, _ := s2.StopColor.RGBA()

	return ApplyOpacity(color.RGBA{
		uint8((float64(r1)*(1-tp) + float64(r2)*tp) / 256),
		uint8((float64(g1)*(1-tp) + float64(g2)*tp) / 256),
		uint8((float64(b1)*(1-tp) + float64(b2)*tp) / 256),
		0xFF}, (s1.Opacity*(1-tp)+s2.Opacity*tp)*opacity)
}

// GetColorFunction returns a color function for the gradient in object
// bounding box space. It never returns an unusable value: if the gradient
// cannot be sampled, the returned ColorFunc paints the first stop and the
// render degrades to a flat fill. Use GetColorFunctionChecked to learn
// whether that fallback happened.
func (g *Gradient) GetColorFunction(opacity float64) interface{} {
	cf, _ := g.GetColorFunctionChecked(opacity)
	return cf
}

// GetColorFunctionUS is GetColorFunction with an explicit object matrix.
// See GetColorFunction for the fallback behavior.
func (g *Gradient) GetColorFunctionUS(opacity float64, objMatrix Matrix2D) interface{} {
	cf, _ := g.GetColorFunctionUSChecked(opacity, objMatrix)
	return cf
}

// GetColorFunctionChecked is the error-reporting form of GetColorFunction.
// On success the returned value is a ColorFunc. If the gradient cannot be
// sampled (its transform is singular or non-finite, or its geometry is
// degenerate), the returned value is still a usable ColorFunc that paints
// the first stop, and the error says why. Callers that can react should
// prefer this over the unchecked GetColorFunction.
func (g *Gradient) GetColorFunctionChecked(opacity float64) (interface{}, error) {
	return g.GetColorFunctionUSChecked(opacity, Identity)
}

// fallbackColorFunction returns a color function that paints a single,
// deterministic color — the gradient's first stop scaled by opacity. It is
// used when the gradient transform is singular, so that a bad transform
// degrades to a flat fill rather than undefined pixels.
func (g *Gradient) fallbackColorFunction(opacity float64) ColorFunc {
	s := g.Stops[0]
	c := ApplyOpacity(s.StopColor, s.Opacity*opacity)
	return func(x, y int) color.Color { return c }
}

// GetColorFunctionUSChecked is the error-reporting form of GetColorFunctionUS.
//
// A gradient whose transform has a zero or near-zero determinant cannot be
// sampled: its coordinates would map to infinities and NaN. Rather than
// letting those values reach the pixel, GetColorFunctionUSChecked returns a
// stable color function plus ErrSingularMatrix (or ErrNonFiniteMatrix when
// a coefficient of the gradient transform is NaN or infinite, or
// ErrDegenerateRadius or ErrSingularMatrix for degenerate geometry). The
// fallback color function always paints the first stop, honoring opacity,
// so a bad imported transform produces a flat painted region instead of
// undefined pixels.
func (g *Gradient) GetColorFunctionUSChecked(opacity float64, objMatrix Matrix2D) (interface{}, error) {
	switch len(g.Stops) {
	case 0:
		return ApplyOpacity(color.RGBA{255, 0, 255, 255}, opacity), nil // default error color for gradient w/o stops.
	case 1:
		return ApplyOpacity(g.Stops[0].StopColor, opacity), nil // Illegal, I think, should really should not happen.
	}

	// sort by offset in ascending order
	sort.Slice(g.Stops, func(i, j int) bool {
		return g.Stops[i].Offset < g.Stops[j].Offset
	})

	// A non-finite transform cannot be sampled in either unit space.
	if !g.Matrix.IsFinite() || !objMatrix.IsFinite() {
		return g.fallbackColorFunction(opacity), ErrNonFiniteMatrix
	}

	w, h := float64(g.Bounds.W), float64(g.Bounds.H)
	oriX, oriY := float64(g.Bounds.X), float64(g.Bounds.Y)
	// gradT maps device pixels back into the coordinate space in which the
	// gradient points are meaningful: the unit bounding box for
	// ObjectBoundingBox units. UserSpaceOnUse does not need it, so there it
	// is the identity.
	//
	// The ObjectBoundingBox mapping is
	//   T(ori) * S(w,h) * g.Matrix * S(1/w,1/h) * T(-ori)
	// where the two scale/translate pairs invert exactly for any non-zero
	// bounding box. Because S(1/w,1/h) and S(w,h) are exact inverses for
	// any non-zero, finite w and h, the whole product is invertible whenever
	// g.Matrix is, and its singularity always shows up in g.Matrix. Checking
	// g.Matrix directly therefore gives the same answer without risking a
	// spurious error from an intermediate overflow.
	gradT := Identity
	if g.Units == ObjectBoundingBox {
		if w == 0 || h == 0 {
			return g.fallbackColorFunction(opacity), ErrSingularMatrix
		}
		if _, err := g.Matrix.Invert(); err != nil {
			return g.fallbackColorFunction(opacity), err
		}
		var err error
		gradT, err = Identity.Translate(oriX, oriY).Scale(w, h).
			Mult(g.Matrix).Scale(1/w, 1/h).Translate(-oriX, -oriY).Invert()
		if err != nil {
			return g.fallbackColorFunction(opacity), err
		}
	}

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
			if rx*rx == 0 || ry*ry == 0 {
				return g.fallbackColorFunction(opacity), ErrDegenerateRadius
			}
			if g.Units == ObjectBoundingBox {
				return ColorFunc(func(xi, yi int) color.Color {
					x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
					dx := float64(x) - cx
					dy := float64(y) - cy
					return g.tColor(math.Sqrt(dx*dx/(rx*rx)+dy*dy/(ry*ry)), opacity)
				}), nil
			} else {
				return ColorFunc(func(xi, yi int) color.Color {
					x := float64(xi) + 0.5
					y := float64(yi) + 0.5
					dx := x - cx
					dy := y - cy
					return g.tColor(math.Sqrt(dx*dx/(rx*rx)+dy*dy/(ry*ry)), opacity)
				}), nil
			}
		} else {
			if rx == 0 || ry == 0 {
				return g.fallbackColorFunction(opacity), ErrDegenerateRadius
			}
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
					return color.RGBA{255, 255, 0, 255}, nil // should not happen
				}
			}
			if g.Units == ObjectBoundingBox {
				return ColorFunc(func(xi, yi int) color.Color {
					x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
					ex := x / rx
					ey := y / ry

					t1x, t1y, intersects := RayCircleIntersectionF(ex, ey, fx, fy, cx, cy, 1.0)
					if intersects == false { //In this case, use the last stop color
						s := g.Stops[len(g.Stops)-1]
						return ApplyOpacity(s.StopColor, s.Opacity*opacity)
					}
					tdx, tdy := t1x-fx, t1y-fy
					dx, dy := ex-fx, ey-fy
					if tdx*tdx+tdy*tdy < epsilonF {
						s := g.Stops[len(g.Stops)-1]
						return ApplyOpacity(s.StopColor, s.Opacity*opacity)
					}
					return g.tColor(math.Sqrt(dx*dx+dy*dy)/math.Sqrt(tdx*tdx+tdy*tdy), opacity)
				}), nil
			} else {
				return ColorFunc(func(xi, yi int) color.Color {
					x := float64(xi) + 0.5
					y := float64(yi) + 0.5
					ex := x / rx
					ey := y / ry

					t1x, t1y, intersects := RayCircleIntersectionF(ex, ey, fx, fy, cx, cy, 1.0)
					if intersects == false { //In this case, use the last stop color
						s := g.Stops[len(g.Stops)-1]
						return ApplyOpacity(s.StopColor, s.Opacity*opacity)
					}
					tdx, tdy := t1x-fx, t1y-fy
					dx, dy := ex-fx, ey-fy
					if tdx*tdx+tdy*tdy < epsilonF {
						s := g.Stops[len(g.Stops)-1]
						return ApplyOpacity(s.StopColor, s.Opacity*opacity)
					}
					return g.tColor(math.Sqrt(dx*dx+dy*dy)/math.Sqrt(tdx*tdx+tdy*tdy), opacity)
				}), nil
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
			if d == 0 || !isFinite(d) {
				return g.fallbackColorFunction(opacity), ErrSingularMatrix
			}
			return ColorFunc(func(xi, yi int) color.Color {
				x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
				dfx := x - p1x
				dfy := y - p1y
				return g.tColor((dx*dfx+dy*dfy)/d, opacity)
			}), nil
		} else {
			p1x, p1y = g.Matrix.Transform(p1x, p1y)
			p2x, p2y = g.Matrix.Transform(p2x, p2y)
			p1x, p1y = objMatrix.Transform(p1x, p1y)
			p2x, p2y = objMatrix.Transform(p2x, p2y)
			dx := p2x - p1x
			dy := p2y - p1y
			d := (dx*dx + dy*dy) // self inner prod
			if d == 0 || !isFinite(d) {
				return g.fallbackColorFunction(opacity), ErrSingularMatrix
			}
			return ColorFunc(func(xi, yi int) color.Color {
				x := float64(xi) + 0.5
				y := float64(yi) + 0.5
				dfx := x - p1x
				dfy := y - p1y
				return g.tColor((dx*dfx+dy*dfy)/d, opacity)
			}), nil
		}
	}
}
