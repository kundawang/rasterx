// Copyright 2026 by the rasterx Authors. All rights reserved.
//_
// Closed-subpath dash balancing. In DashModeBalanceClosed each subpath is
// buffered until it is closed, so its perimeter is known before any dash
// state machine is run. The dash pattern is then scaled so that an integer
// number of equal dash periods exactly covers the perimeter, which makes the
// pattern join symmetrically at the closing seam. Open subpaths are replayed
// unchanged and therefore keep the original streaming behavior.
package rasterx

import (
	"golang.org/x/image/math/fixed"
)

// maxBalancedEntries bounds the number of dash entries in an expanded
// balanced pattern. It guards against pathological inputs (a very short
// pattern period on a very large contour) allocating excessive memory.
const maxBalancedEntries = 1 << 16

// dashBuffering reports whether incoming path commands must be captured
// instead of being streamed straight into the stroking state machine.
func (r *Dasher) dashBuffering() bool {
	return r.Mode == DashModeBalanceClosed && len(r.Dashes) > 0 && !r.replaying
}

// Start opens a new subpath. In balancing mode the previous subpath (if any)
// is flushed first, mirroring the implicit Stop(false) performed by
// Path.AddTo before every MoveTo.
func (r *Dasher) Start(a fixed.Point26_6) {
	if r.bufOpen {
		r.Stop(false)
	}
	if r.dashBuffering() {
		r.buf = r.buf[:0]
		r.buf.Start(a)
		r.bufOpen = true
		return
	}
	r.streamStart(a)
}

// Stop closes or ends the current subpath. A buffered closed subpath is
// measured and balanced before replay; a buffered open subpath is replayed
// with the original pattern untouched.
func (r *Dasher) Stop(isClosed bool) {
	if !(r.dashBuffering() && r.bufOpen) {
		r.streamStop(isClosed)
		return
	}
	if isClosed {
		r.buf.Stop(true)
	}
	perim := measurePathLength(r.buf, isClosed)
	if isClosed && perim > 0 {
		if dashes, off, ok := balanceDashes(r.Dashes, r.DashOffset, perim); ok {
			r.playPath(r.buf, isClosed, dashes, off)
		} else {
			// Degenerate pattern (all zero/negative entries): solid stroke.
			r.playPath(r.buf, isClosed, nil, 0)
		}
	} else {
		// Open subpath, or a closed subpath with zero perimeter: keep the
		// original streaming behavior exactly.
		r.playPath(r.buf, isClosed, r.Dashes, r.DashOffset)
	}
	r.buf = r.buf[:0]
	r.bufOpen = false
}

// Clear resets the rasterizer and discards any buffered subpath. The caller's
// dash pattern slice and the Mode setting are left untouched.
func (r *Dasher) Clear() {
	r.buf = r.buf[:0]
	r.bufOpen = false
	r.replaying = false
	r.Filler.Clear()
}

// playPath replays the buffered subpath into the stroking/dashing state
// machine using dashes as the active pattern. A nil or empty dash slice
// produces a solid stroke. The Dasher's stored pattern and offset are
// restored afterwards, so the caller-visible configuration never changes.
func (r *Dasher) playPath(p Path, isClosed bool, dashes []fixed.Int26_6, offset fixed.Int26_6) {
	origDashes, origOffset, origSgm, origReplaying := r.Dashes, r.DashOffset, r.sgm, r.replaying
	r.replaying = true
	if len(dashes) > 0 {
		if cap(r.balDashes) < len(dashes) {
			r.balDashes = make([]fixed.Int26_6, len(dashes))
		} else {
			r.balDashes = r.balDashes[:len(dashes)]
		}
		copy(r.balDashes, dashes)
		r.Dashes = r.balDashes
		r.sgm = r
	} else {
		r.Dashes = r.Dashes[:0]
		r.sgm = &r.Stroker
	}
	r.DashOffset = offset

	var started bool
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			r.streamStart(fixed.Point26_6{X: p[i+1], Y: p[i+2]})
			started = true
			i += 3
		case PathLineTo:
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			r.LineSeg(r.sgm, b)
			i += 3
		case PathQuadTo:
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			r.quadBezierf(r.sgm, b, c)
			i += 5
		case PathCubicTo:
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			d := fixed.Point26_6{X: p[i+5], Y: p[i+6]}
			r.cubeBezierf(r.sgm, b, c, d)
			i += 7
		case PathClose:
			r.streamStop(true)
			started = false
			i++
		default:
			// Restore before panicking so the Dasher is not left half-swapped.
			r.Dashes, r.DashOffset, r.sgm, r.replaying = origDashes, origOffset, origSgm, origReplaying
			panic("rasterx: bad path in dash balancing replay")
		}
	}
	if started {
		// Open subpath: a PathClose command was not emitted.
		r.streamStop(isClosed)
	}
	r.Dashes, r.DashOffset, r.sgm, r.replaying = origDashes, origOffset, origSgm, origReplaying
}

// lengthMeter flattens a subpath the same way the Filler does (same
// QuadTo/CubeTo subdivision heuristics) and accumulates the length of the
// resulting polyline, so the measured perimeter matches the geometry the
// dashing state machine actually walks.
type lengthMeter struct {
	pen, first fixed.Point26_6
	started    bool
	length     fixed.Int26_6
}

func (m *lengthMeter) Start(a fixed.Point26_6) {
	m.pen, m.first, m.started = a, a, true
}

func (m *lengthMeter) Line(b fixed.Point26_6) {
	if m.started {
		m.length += Length(b.Sub(m.pen))
	}
	m.pen = b
}

func (m *lengthMeter) QuadBezier(b, c fixed.Point26_6) {
	if !m.started {
		return
	}
	if m.pen == b || b == c {
		m.Line(c)
		return
	}
	QuadTo(float32(m.pen.X), float32(m.pen.Y),
		float32(b.X), float32(b.Y),
		float32(c.X), float32(c.Y),
		func(dx, dy float32) {
			m.Line(fixed.Point26_6{fixed.Int26_6(dx), fixed.Int26_6(dy)})
		})
}

func (m *lengthMeter) CubeBezier(b, c, d fixed.Point26_6) {
	if !m.started {
		return
	}
	if (m.pen == b && c == d) || (m.pen == b && b == c) || (c == b && d == c) {
		m.Line(d)
		return
	}
	CubeTo(float32(m.pen.X), float32(m.pen.Y),
		float32(b.X), float32(b.Y),
		float32(c.X), float32(c.Y),
		float32(d.X), float32(d.Y),
		func(ex, ey float32) {
			m.Line(fixed.Point26_6{fixed.Int26_6(ex), fixed.Int26_6(ey)})
		})
}

// Stop adds the implicit closing chord for closed subpaths, matching the
// Line(firstP) that Filler.Stop and Dasher.streamStop emit.
func (m *lengthMeter) Stop(isClosed bool) {
	if isClosed && m.started && m.pen != m.first {
		m.length += Length(m.first.Sub(m.pen))
	}
	m.started = false
}

// measurePathLength walks p with a lengthMeter and returns the subpath
// perimeter in fixed.Int26_6 units.
func measurePathLength(p Path, isClosed bool) fixed.Int26_6 {
	var m lengthMeter
	for i := 0; i < len(p); {
		switch PathCommand(p[i]) {
		case PathMoveTo:
			m.Start(fixed.Point26_6{X: p[i+1], Y: p[i+2]})
			i += 3
		case PathLineTo:
			m.Line(fixed.Point26_6{X: p[i+1], Y: p[i+2]})
			i += 3
		case PathQuadTo:
			m.QuadBezier(
				fixed.Point26_6{X: p[i+1], Y: p[i+2]},
				fixed.Point26_6{X: p[i+3], Y: p[i+4]})
			i += 5
		case PathCubicTo:
			m.CubeBezier(
				fixed.Point26_6{X: p[i+1], Y: p[i+2]},
				fixed.Point26_6{X: p[i+3], Y: p[i+4]},
				fixed.Point26_6{X: p[i+5], Y: p[i+6]})
			i += 7
		case PathClose:
			m.Stop(true)
			i++
		default:
			panic("rasterx: bad path in dash length measurement")
		}
	}
	m.Stop(isClosed)
	return m.length
}

// modNonNegative returns the Euclidean (non-negative) remainder of a over n.
// n must be positive.
func modNonNegative(a, n fixed.Int26_6) fixed.Int26_6 {
	r := a % n
	if r < 0 {
		r += n
	}
	return r
}

// balanceDashes computes a dash pattern that covers the closed-subpath
// perimeter L with an integer number of equal periods.
//
// Inputs are the original pattern dashes (entries may be zero or negative),
// the user supplied offset and the measured perimeter L > 0. Negative
// entries are clamped to zero, matching the SVG convention that a negative
// dash length is an error treated as zero. The returned pattern is freshly
// allocated and consists of k copies of the balanced period, where k is the
// whole number of periods closest to L/period; its total length equals L
// exactly, so the dashing state machine consumes one full pattern while
// walking the contour and reaches the seam in the starting phase. The input
// slice is never modified.
//
// ok is false when every pattern entry is zero (no period length), in which
// case the caller falls back to a solid stroke.
func balanceDashes(dashes []fixed.Int26_6, offset, L fixed.Int26_6) (out []fixed.Int26_6, outOffset fixed.Int26_6, ok bool) {
	n := len(dashes)
	src := make([]fixed.Int26_6, n)
	var period fixed.Int26_6
	for i, d := range dashes {
		if d > 0 {
			src[i] = d
			period += d
		}
	}
	if period <= 0 {
		return nil, 0, false
	}

	// Nearest whole number of periods, at least one. Rounding (rather than
	// truncating) keeps the scaling factor closest to 1.0, so when the
	// perimeter already nearly fits an integer number of periods the dashes
	// are barely resized. Clamp k to L because no period can be shorter than
	// one fixed-point unit.
	kf := float64(L) / float64(period)
	k := int(kf + 0.5)
	if k < 1 {
		k = 1
	}
	if k > int(L) {
		k = int(L)
	}
	// Cap the expanded pattern size so a pathological input (tiny pattern
	// period on a huge contour) cannot allocate an unbounded slice. When the
	// cap binds, the balanced period is simply repeated fewer times; the
	// streaming state machine still closes the seam because it cycles over
	// the pattern while walking the perimeter.
	if maxK := maxBalancedEntries / n; k > maxK {
		k = maxK
	}
	targetPeriod := L / fixed.Int26_6(k) // integer floor; remainder handled below

	base := make([]fixed.Int26_6, n)
	var sum fixed.Int26_6
	for i, d := range src {
		if d <= 0 {
			continue
		}
		// Scale in float64 to avoid int64 overflow when period and the
		// dash entries are both large; clamp to a non-negative integer.
		v := fixed.Int26_6(float64(targetPeriod)*float64(d)/float64(period) + 0.5)
		if v < 0 {
			v = 0
		}
		base[i] = v
		sum += v
	}

	// Fixed-point rounding leaves a residual between k copies of the scaled
	// period and L. Every copy is corrected by the same share so the periods
	// stay as equal as possible; the indivisible remainder (always smaller
	// than n) is spread one unit at a time over the first copies' entries.
	// The expanded pattern then totals L exactly, and the dashing state
	// machine reaches the seam back in its starting phase.
	rem := L - sum*fixed.Int26_6(k)
	share := rem / fixed.Int26_6(k)
	left := rem - share*fixed.Int26_6(k) // |left| < k
	if share != 0 {
		for i := range base {
			base[i] += share
			if base[i] < 0 {
				base[i] = 0
			}
		}
	}

	out = make([]fixed.Int26_6, 0, n*k)
	var balPeriod fixed.Int26_6
	for j := 0; j < k; j++ {
		copyPeriod := append([]fixed.Int26_6(nil), base...)
		if left > 0 {
			for i := range copyPeriod {
				if left == 0 {
					break
				}
				copyPeriod[i]++
				left--
			}
		} else if left < 0 {
			for i := range copyPeriod {
				if left == 0 {
					break
				}
				if copyPeriod[i] > 0 {
					copyPeriod[i]--
					left++
				}
			}
		}
		out = append(out, copyPeriod...)
	}
	for _, d := range base {
		balPeriod += d
	}
	// balPeriod is the common (un-remaindered) period length; use the actual
	// first copy length for phase folding so it never exceeds reality.
	var firstPeriod fixed.Int26_6
	for i := 0; i < n; i++ {
		firstPeriod += out[i]
	}

	// Fold the user offset, including very large or negative values, into
	// one balanced period so the state machine starts within the period.
	// Because the expanded pattern spans the perimeter an integer number of
	// times, any phase still joins seamlessly at the seam.
	if firstPeriod > 0 {
		outOffset = modNonNegative(offset, firstPeriod)
	}
	return out, outOffset, true
}
