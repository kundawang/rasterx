// Gradient implementation fo rasterx package
// Copyright 2018 All rights reserved.
// Created: 5/12/2018 by S.R.Wiley

package rasterx

import (
	"image/color"
	"math"
	"sort"
)

// SVG bounds paremater constants
const (
	ObjectBoundingBox GradientUnits = iota
	UserSpaceOnUse
)

// SVG spread parameter constants
const (
	PadSpread SpreadMethod = iota
	ReflectSpread
	RepeatSpread
)

const epsilonF = 1e-5

type (
	// SpreadMethod is the type for spread parameters
	SpreadMethod byte
	// GradientUnits is the type for gradient units
	GradientUnits byte
	// GradStop represents a stop in the SVG 2.0 gradient specification
	GradStop struct {
		StopColor color.Color
		Offset    float64
		Opacity   float64
	}
	// Gradient holds a description of an SVG 2.0 gradient
	Gradient struct {
		Points [5]float64
		// Stops are the gradient color stops, sorted by Offset on use.
		Stops  []GradStop
		Bounds struct{ X, Y, W, H float64 }
		Matrix Matrix2D
		Spread SpreadMethod
		Units  GradientUnits
		// IsRadial selects the radial gradient path when true.
		IsRadial bool
		// FR is the inner (focal) circle radius of a radial gradient,
		// analogous to the SVG 2 fr attribute. It is interpreted in the
		// same units as the outer radius Points[4]. When FR is zero the
		// gradient renders exactly as the original single-point radial
		// gradient. Negative values are clamped to zero; for a radial
		// gradient the inner circle is the t=0 circle and the outer
		// circle (center cx,cy = Points[0:2], radius r = Points[4]) is
		// the t=1 circle.
		FR float64
	}
)

// transparent is the fully transparent color used as the deterministic
// result of invalid gradients (e.g. a negative outer radius, non-finite
// geometry or a singular gradient transform).
var transparent = color.NRGBA{0, 0, 0, 0}

// isFinite reports whether v is neither an infinity nor a NaN.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// ApplyOpacity sets the color's alpha channel to the given value
func ApplyOpacity(c color.Color, opacity float64) color.NRGBA {
	r, g, b, _ := c.RGBA()
	return color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(opacity * 0xFF)}
}

// tColor takes the paramaterized value along the gradient's stops and
// returns a color depending on the spreadMethod value of the gradient and
// the gradient's slice of stop values.
func (g *Gradient) tColor(t, opacity float64) color.Color {
	d := len(g.Stops)
	// These cases can be taken care of early on
	if t >= 1.0 && g.Spread == PadSpread {
		s := g.Stops[d-1]
		return ApplyOpacity(s.StopColor, s.Opacity*opacity)
	}
	if t <= 0.0 && g.Spread == PadSpread {
		return ApplyOpacity(g.Stops[0].StopColor, g.Stops[0].Opacity*opacity)
	}

	var modRange = 1.0
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
		s1off--
		if t > 1 {
			t--
		}
	}
	if s2.Offset == s1off {
		return ApplyOpacity(s2.StopColor, s2.Opacity)
	}
	if flip {
		t = 1 - t
	}
	tp := (t - s1off) / (s2.Offset - s1off)
	r1, g1, b1, _ := s1.StopColor.RGBA()
	r2, g2, b2, _ := s2.StopColor.RGBA()

	return ApplyOpacity(color.RGBA{
		uint8((float64(r1)*(1-tp) + float64(r2)*tp) / 256),
		uint8((float64(g1)*(1-tp) + float64(g2)*tp) / 256),
		uint8((float64(b1)*(1-tp) + float64(b2)*tp) / 256),
		0xFF}, (s1.Opacity*(1-tp)+s2.Opacity*tp)*opacity)
}

// GetColorFunction returns the color function
func (g *Gradient) GetColorFunction(opacity float64) interface{} {
	return g.GetColorFunctionUS(opacity, Identity)
}

// GetColorFunctionUS returns the color function using the User Space objMatrix
func (g *Gradient) GetColorFunctionUS(opacity float64, objMatrix Matrix2D) interface{} {
	switch len(g.Stops) {
	case 0:
		return ApplyOpacity(color.RGBA{0, 0, 0, 255}, opacity) // default error color for gradient w/o stops.
	case 1:
		return ApplyOpacity(g.Stops[0].StopColor, opacity) // Illegal, I think, should really should not happen.
	}

	// sort by offset in ascending order
	sort.Slice(g.Stops, func(i, j int) bool {
		return g.Stops[i].Offset < g.Stops[j].Offset
	})
	// Clamp out-of-range and non-finite offsets into [0,1] so the stop
	// walk in tColor always has deterministic bounds.
	for i := range g.Stops {
		switch {
		case !isFinite(g.Stops[i].Offset) || g.Stops[i].Offset < 0:
			g.Stops[i].Offset = 0
		case g.Stops[i].Offset > 1:
			g.Stops[i].Offset = 1
		}
	}

	w, h := float64(g.Bounds.W), float64(g.Bounds.H)
	oriX, oriY := float64(g.Bounds.X), float64(g.Bounds.Y)
	gradT := Identity.Translate(oriX, oriY).Scale(w, h).
		Mult(g.Matrix).Scale(1/w, 1/h).Translate(-oriX, -oriY).Invert()

	if g.IsRadial {
		// A radial gradient interpolates between an inner (focal) circle
		// (fx,fy) with focal radius FR and an outer circle (cx,cy) with
		// radius r. With FR == 0 the inner circle collapses to the focal
		// point and the original single-point radial behavior is used.
		cx, cy, fx, fy, r := g.Points[0], g.Points[1], g.Points[2], g.Points[3], g.Points[4]
		fr := g.FR
		if !isFinite(cx) || !isFinite(cy) || !isFinite(fx) || !isFinite(fy) ||
			!isFinite(r) || !isFinite(fr) {
			// Non-finite geometry can never produce a defined color.
			return ColorFunc(func(xi, yi int) color.Color { return transparent })
		}
		if g.Units == ObjectBoundingBox {
			cx = g.Bounds.X + g.Bounds.W*cx
			cy = g.Bounds.Y + g.Bounds.H*cy
			fx = g.Bounds.X + g.Bounds.W*fx
			fy = g.Bounds.Y + g.Bounds.H*fy
			fr *= g.Bounds.W
			rx, ry := r*g.Bounds.W, r*g.Bounds.H
			if !isFinite(gradT.A) || !isFinite(gradT.B) || !isFinite(gradT.C) ||
				!isFinite(gradT.D) || !isFinite(gradT.E) || !isFinite(gradT.F) ||
				!isFinite(cx) || !isFinite(cy) || !isFinite(fx) || !isFinite(fy) ||
				!isFinite(rx) || !isFinite(ry) || !isFinite(fr) {
				return ColorFunc(func(xi, yi int) color.Color { return transparent })
			}
			if rx <= 0 || ry <= 0 {
				// A zero outer radius paints the last stop everywhere;
				// a negative radius renders nothing (SVG invalid geometry).
				if rx == 0 && ry == 0 {
					return ColorFunc(func(xi, yi int) color.Color {
						s := g.Stops[len(g.Stops)-1]
						return ApplyOpacity(s.StopColor, s.Opacity*opacity)
					})
				}
				return ColorFunc(func(xi, yi int) color.Color { return transparent })
			}
			// Normalize the geometry by the outer ellipse: both circles
			// become unit-radius circles and the focal radius becomes the
			// ratio fr/r (the W/H scaling cancels in the ratio).
			frNorm := clampFocalRadius(fr / rx)
			if frNorm == 0 {
				return g.legacyRadialOBB(cx, cy, fx, fy, rx, ry, gradT, opacity)
			}
			nfx, nfy, nfr := constrainFocalCircle(fx/rx, fy/ry, cx/rx, cy/ry, frNorm)
			ncx, ncy := cx/rx, cy/ry
			return ColorFunc(func(xi, yi int) color.Color {
				x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
				ex, ey := x/rx, y/ry
				if !isFinite(ex) || !isFinite(ey) {
					return transparent
				}
				t, ok := radialTwoCircleT(ex, ey, nfx, nfy, nfr, ncx, ncy)
				if !ok {
					return g.lastStopColor(opacity)
				}
				return g.tColor(t, opacity)
			})
		}

		// UserSpaceOnUse: the gradient matrix and the object matrix map
		// the gradient coordinates into the user space of the pixels.
		cx, cy = g.Matrix.Transform(cx, cy)
		fx, fy = g.Matrix.Transform(fx, fy)
		rx, ry := g.Matrix.TransformVector(r, r)
		frx, _ := g.Matrix.TransformVector(fr, fr)
		cx, cy = objMatrix.Transform(cx, cy)
		fx, fy = objMatrix.Transform(fx, fy)
		rx, ry = objMatrix.TransformVector(rx, ry)
		frx, _ = objMatrix.TransformVector(frx, frx)
		if !isFinite(cx) || !isFinite(cy) || !isFinite(fx) || !isFinite(fy) ||
			!isFinite(rx) || !isFinite(ry) || !isFinite(frx) {
			return ColorFunc(func(xi, yi int) color.Color { return transparent })
		}
		if rx <= 0 || ry <= 0 {
			if rx == 0 && ry == 0 {
				return ColorFunc(func(xi, yi int) color.Color {
					s := g.Stops[len(g.Stops)-1]
					return ApplyOpacity(s.StopColor, s.Opacity*opacity)
				})
			}
			return ColorFunc(func(xi, yi int) color.Color { return transparent })
		}
		// |fr|/|r| is invariant under the shared linear transform, so the
		// normalized focal radius is fr/r; negatives are clamped to zero.
		frNorm := clampFocalRadius(fr / r)
		if frNorm == 0 {
			return g.legacyRadialUS(cx, cy, fx, fy, rx, ry, opacity)
		}
		nfx, nfy, nfr := constrainFocalCircle(fx/rx, fy/ry, cx/rx, cy/ry, frNorm)
		ncx, ncy := cx/rx, cy/ry
		return ColorFunc(func(xi, yi int) color.Color {
			x := float64(xi) + 0.5
			y := float64(yi) + 0.5
			ex, ey := x/rx, y/ry
			if !isFinite(ex) || !isFinite(ey) {
				return transparent
			}
			t, ok := radialTwoCircleT(ex, ey, nfx, nfy, nfr, ncx, ncy)
			if !ok {
				return g.lastStopColor(opacity)
			}
			return g.tColor(t, opacity)
		})
	}
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
			return g.tColor((dx*dfx+dy*dfy)/d, opacity)
		})
	}

	p1x, p1y = g.Matrix.Transform(p1x, p1y)
	p2x, p2y = g.Matrix.Transform(p2x, p2y)
	p1x, p1y = objMatrix.Transform(p1x, p1y)
	p2x, p2y = objMatrix.Transform(p2x, p2y)
	dx := p2x - p1x
	dy := p2y - p1y
	d := (dx*dx + dy*dy)
	// if d == 0.0 {
	// 	fmt.Println("zero delta")
	// }
	return ColorFunc(func(xi, yi int) color.Color {
		x := float64(xi) + 0.5
		y := float64(yi) + 0.5
		dfx := x - p1x
		dfy := y - p1y
		return g.tColor((dx*dfx+dy*dfy)/d, opacity)
	})
}

// clampFocalRadius normalizes a focal-radius ratio fr/r. Negative focal
// radii are invalid in SVG and are clamped to zero, which selects the
// legacy single-point radial path.
func clampFocalRadius(frNorm float64) float64 {
	if !isFinite(frNorm) || frNorm <= 0 {
		return 0
	}
	return frNorm
}

// constrainFocalCircle enforces the SVG 2 rule that the focal circle
// (fx,fy,fr) must lie entirely within the unit outer circle centered at
// (cx,cy). All coordinates are normalized by the outer ellipse, so the
// outer circle has radius one. The focal point is first projected onto the
// unit circle when it lies outside (matching the SVG 1 fr=0 behavior),
// then fr is shrunk to the largest radius that still fits between the
// focal point and the outer circle. A focal circle at least as large as
// the outer circle collapses to a full-size concentric circle.
func constrainFocalCircle(fx, fy, cx, cy, fr float64) (nfx, nfy, nfr float64) {
	dx, dy := fx-cx, fy-cy
	d := math.Sqrt(dx*dx + dy*dy)
	if d > 1.0 {
		// Focus outside the outer circle: move it to the intersection of
		// the center/focus line with the unit circle.
		fx, fy = cx+dx/d, cy+dy/d
		d = 1.0
	}
	if fr >= 1.0 {
		// Inner circle is no smaller than the outer one: the annulus has
		// no positive width, so use the outer circle itself as the inner
		// circle (t is 0 inside, 1 outside) for a deterministic result.
		return cx, cy, 1.0
	}
	if maxFr := 1.0 - d; fr > maxFr {
		fr = maxFr // shrink the focal radius so the circle fits
	}
	return fx, fy, fr
}

// lastStopColor returns the last stop color with the gradient opacity
// applied; it is the deterministic pad color when t is beyond the outer
// circle without a real intersection.
func (g *Gradient) lastStopColor(opacity float64) color.Color {
	s := g.Stops[len(g.Stops)-1]
	return ApplyOpacity(s.StopColor, s.Opacity*opacity)
}

// radialTwoCircleT returns the gradient parameter t for the sample point
// (ex,ey) in normalized space, where the outer circle (cx,cy) has unit
// radius and the inner focal circle is (fx,fy) with radius fr
// (0 < fr <= 1). The focal circle is t=0 and the outer circle t=1.
// Points inside the focal disk plateau at t=0. ok is false when the ray
// from the focus through the sample point has no real intersection with
// the outer circle; the caller then paints the last stop color.
func radialTwoCircleT(ex, ey, fx, fy, fr, cx, cy float64) (t float64, ok bool) {
	dx, dy := ex-fx, ey-fy
	d := math.Sqrt(dx*dx + dy*dy)
	if d <= fr {
		return 0, true // inside the focal disk: first stop plateau
	}
	if cx == fx && cy == fy {
		// Concentric circles: the parameter is the normalized distance
		// between the focal radius and the unit radius.
		if 1.0-fr < epsilonF {
			// Equal radii: the annulus has zero width.
			return 1, true // d > fr == 1 means the point is outside
		}
		return (d - fr) / (1.0 - fr), true
	}
	// Eccentric circles: intersect the ray from the focus through the
	// sample point with the unit outer circle, then remap the distance
	// ratio from focal-radius geometry onto the [0,1] stop parameter.
	t1x, t1y, intersects := RayCircleIntersectionF(ex, ey, fx, fy, cx, cy, 1.0)
	if !intersects {
		return 0, false // no real intersection: last stop color
	}
	tdx, tdy := t1x-fx, t1y-fy
	L := math.Sqrt(tdx*tdx + tdy*tdy)
	if L < epsilonF || L <= fr {
		return 0, false
	}
	t = (d - fr) / (L - fr)
	if !isFinite(t) {
		return 0, false
	}
	return t, true
}

// legacyRadialOBB keeps the original ObjectBoundingBox radial behavior
// byte-for-byte for gradients whose focal radius is zero.
func (g *Gradient) legacyRadialOBB(cx, cy, fx, fy, rx, ry float64, gradT Matrix2D, opacity float64) ColorFunc {
	if cx == fx && cy == fy {
		// When the focus and center are the same things are much simpler;
		// t is just distance from center
		// scaled by the bounds aspect ratio times r
		return ColorFunc(func(xi, yi int) color.Color {
			x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
			dx := float64(x) - cx
			dy := float64(y) - cy
			return g.tColor(math.Sqrt(dx*dx/(rx*rx)+(dy*dy)/(ry*ry)), opacity)
		})
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
			return ColorFunc(func(xi, yi int) color.Color { return color.RGBA{255, 255, 0, 255} })
		}
	}
	return ColorFunc(func(xi, yi int) color.Color {
		x, y := gradT.Transform(float64(xi)+0.5, float64(yi)+0.5)
		ex := x / rx
		ey := y / ry

		t1x, t1y, intersects := RayCircleIntersectionF(ex, ey, fx, fy, cx, cy, 1.0)
		if intersects == false { //In this case, use the last stop color
			return g.lastStopColor(opacity)
		}
		tdx, tdy := t1x-fx, t1y-fy
		dx, dy := ex-fx, ey-fy
		if tdx*tdx+tdy*tdy < epsilonF {
			return g.lastStopColor(opacity)
		}
		return g.tColor(math.Sqrt(dx*dx+dy*dy)/math.Sqrt(tdx*tdx+tdy*tdy), opacity)
	})
}

// legacyRadialUS keeps the original UserSpaceOnUse radial behavior
// byte-for-byte for gradients whose focal radius is zero.
func (g *Gradient) legacyRadialUS(cx, cy, fx, fy, rx, ry float64, opacity float64) ColorFunc {
	if cx == fx && cy == fy {
		return ColorFunc(func(xi, yi int) color.Color {
			x := float64(xi) + 0.5
			y := float64(yi) + 0.5
			dx := x - cx
			dy := y - cy
			return g.tColor(math.Sqrt(dx*dx/(rx*rx)+(dy*dy)/(ry*ry)), opacity)
		})
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
			return ColorFunc(func(xi, yi int) color.Color { return color.RGBA{255, 255, 0, 255} })
		}
	}
	return ColorFunc(func(xi, yi int) color.Color {
		x := float64(xi) + 0.5
		y := float64(yi) + 0.5
		ex := x / rx
		ey := y / ry

		t1x, t1y, intersects := RayCircleIntersectionF(ex, ey, fx, fy, cx, cy, 1.0)
		if intersects == false { //In this case, use the last stop color
			return g.lastStopColor(opacity)
		}
		tdx, tdy := t1x-fx, t1y-fy
		dx, dy := ex-fx, ey-fy
		if tdx*tdx+tdy*tdy < epsilonF {
			return g.lastStopColor(opacity)
		}
		return g.tColor(math.Sqrt(dx*dx+dy*dy)/math.Sqrt(tdx*tdx+tdy*tdy), opacity)
	})
}
