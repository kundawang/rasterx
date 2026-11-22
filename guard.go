// Copyright 2018 by the rasterx Authors. All rights reserved.
package rasterx

import (
	"errors"
	"fmt"
	"image"

	"golang.org/x/image/math/fixed"
)

// ErrPanic wraps a panic recovered from a downstream component. A guarded
// rasterization turns a crash in the rasterizer into a reported failure, so a
// malformed input cannot take the process down.
var ErrPanic = errors.New("rasterx: recovered panic")

// Limits are the resource budgets applied by AnalyzePath and GuardAdder.
// The zero value is not valid; use DefaultLimits.
type Limits struct {
	// MaxInputSegments bounds the number of line/quad/cubic commands.
	MaxInputSegments int
	// MaxLinesPerCurve bounds the flattened segments one Bezier may produce.
	MaxLinesPerCurve int
	// MaxOutputLines bounds the total flattened lines of one path.
	MaxOutputLines int
	// MaxDashEntries bounds the length of a dash pattern.
	MaxDashEntries int
	// MaxPixels bounds the destination image area.
	MaxPixels int
}

// DefaultLimits returns the package limits, which are chosen so that a single
// render stays inside a few tens of megabytes of working set.
func DefaultLimits() Limits {
	return Limits{
		MaxInputSegments: MaxInputSegments,
		MaxLinesPerCurve: MaxFlattenedLines,
		MaxOutputLines:   maxOutputLines,
		MaxDashEntries:   MaxDashEntries,
		MaxPixels:        rasterizeMaxPixels,
	}
}

func (l Limits) lineLimit() int {
	switch {
	case l.MaxLinesPerCurve <= 0:
		return 1
	case l.MaxLinesPerCurve > MaxFlattenedLinesCeiling:
		return MaxFlattenedLinesCeiling
	default:
		return l.MaxLinesPerCurve
	}
}

func (l Limits) inputLimit() int {
	if l.MaxInputSegments <= 0 {
		return MaxInputSegments
	}
	return l.MaxInputSegments
}

func (l Limits) outputLimit() int {
	if l.MaxOutputLines <= 0 {
		return maxOutputLines
	}
	return l.MaxOutputLines
}

// EstimatedLines returns the number of flattened segments the same deviation
// value would produce under these limits, and whether it fits.
func (l Limits) EstimatedLines(devsq float64) (int, bool) {
	return flatteningSegments(devsq, l.lineLimit())
}

// PathReport is the result of statically analyzing a Path without invoking a
// Filler, Stroker or Dasher. It records the command sequence, per-command
// counts, the path extent and the estimated flattened line count.
type PathReport struct {
	Commands   []PathCommand
	Segments   int
	MoveTo     int
	LineTo     int
	QuadTo     int
	CubicTo    int
	Close      int
	EstLines   int
	Min, Max   fixed.Point26_6
	HaveExtent bool

	// Err is the first structural problem found, or nil.
	Err error
	// FailureStage names the phase and command where Err was found.
	FailureStage string
}

// AnalyzePath parses p with bounds checks, without touching any rasterizer.
// It is the reference checker: the rendering path is validated separately and
// the two reports are compared by the test harness.
func AnalyzePath(p Path, lim Limits) *PathReport {
	rep := &PathReport{}
	fail := func(stage string, err error) *PathReport {
		if rep.Err == nil {
			rep.Err = err
			rep.FailureStage = stage
		}
		return rep
	}
	pt := func(i int) fixed.Point26_6 {
		return fixed.Point26_6{X: p[i], Y: p[i+1]}
	}
	checkPt := func(stage string, q fixed.Point26_6) bool {
		if !IsSafePoint(q) {
			fail(stage, fmt.Errorf("%w: %v", ErrNonFinite, q))
			return false
		}
		return true
	}
	addExtent := func(q fixed.Point26_6) {
		if !rep.HaveExtent {
			rep.Min, rep.Max, rep.HaveExtent = q, q, true
			return
		}
		if q.X < rep.Min.X {
			rep.Min.X = q.X
		}
		if q.Y < rep.Min.Y {
			rep.Min.Y = q.Y
		}
		if q.X > rep.Max.X {
			rep.Max.X = q.X
		}
		if q.Y > rep.Max.Y {
			rep.Max.Y = q.Y
		}
	}
	devsq := devsqFixed

	var cur, first fixed.Point26_6
	started := false
	for i := 0; i < len(p); {
		cmd := PathCommand(p[i])
		stage := fmt.Sprintf("cmd[%d]", i)
		switch cmd {
		case PathMoveTo:
			if i+3 > len(p) {
				return fail(stage, fmt.Errorf("%w: truncated moveto", ErrBadPath))
			}
			q := pt(i + 1)
			if !checkPt(stage, q) {
				return rep
			}
			rep.Commands = append(rep.Commands, cmd)
			rep.MoveTo++
			cur, first, started = q, q, true
			addExtent(q)
			i += 3
		case PathLineTo:
			if i+3 > len(p) {
				return fail(stage, fmt.Errorf("%w: truncated lineto", ErrBadPath))
			}
			if !started {
				return fail(stage, fmt.Errorf("%w: lineto before moveto", ErrBadPath))
			}
			q := pt(i + 1)
			if !checkPt(stage, q) {
				return rep
			}
			rep.Commands = append(rep.Commands, cmd)
			rep.LineTo++
			rep.Segments++
			if rep.Segments > lim.inputLimit() {
				return fail(stage, ErrTooManySegments)
			}
			cur = q
			addExtent(q)
			i += 3
		case PathQuadTo:
			if i+5 > len(p) {
				return fail(stage, fmt.Errorf("%w: truncated quadto", ErrBadPath))
			}
			if !started {
				return fail(stage, fmt.Errorf("%w: quadto before moveto", ErrBadPath))
			}
			b, c := pt(i+1), pt(i+3)
			if !checkPt(stage, b) || !checkPt(stage, c) {
				return rep
			}
			rep.Commands = append(rep.Commands, cmd)
			rep.QuadTo++
			rep.Segments++
			if rep.Segments > lim.inputLimit() {
				return fail(stage, ErrTooManySegments)
			}
			n, ok := lim.EstimatedLines(devsq(cur, b, c))
			if !ok {
				return fail(stage, ErrTooComplex)
			}
			rep.EstLines += n
			if rep.EstLines > lim.outputLimit() {
				return fail(stage, ErrTooManyOutputLines)
			}
			cur = c
			addExtent(b)
			addExtent(c)
			i += 5
		case PathCubicTo:
			if i+7 > len(p) {
				return fail(stage, fmt.Errorf("%w: truncated cubicto", ErrBadPath))
			}
			if !started {
				return fail(stage, fmt.Errorf("%w: cubicto before moveto", ErrBadPath))
			}
			b, c, d := pt(i+1), pt(i+3), pt(i+5)
			if !checkPt(stage, b) || !checkPt(stage, c) || !checkPt(stage, d) {
				return rep
			}
			rep.Commands = append(rep.Commands, cmd)
			rep.CubicTo++
			rep.Segments++
			if rep.Segments > lim.inputLimit() {
				return fail(stage, ErrTooManySegments)
			}
			dv := devsq(cur, b, d)
			if alt := devsq(cur, c, d); alt > dv {
				dv = alt
			}
			n, ok := lim.EstimatedLines(dv)
			if !ok {
				return fail(stage, ErrTooComplex)
			}
			rep.EstLines += n
			if rep.EstLines > lim.outputLimit() {
				return fail(stage, ErrTooManyOutputLines)
			}
			cur = d
			addExtent(b)
			addExtent(c)
			addExtent(d)
			i += 7
		case PathClose:
			rep.Commands = append(rep.Commands, cmd)
			rep.Close++
			cur = first
			i++
		default:
			return fail(stage, fmt.Errorf("%w: command %d", ErrBadPath, int64(cmd)))
		}
	}
	return rep
}

// GuardAdder validates input paths before forwarding them to an Adder. It is
// independent of Filler, Stroker and Dasher: it implements Adder itself and
// only relies on the bounds in this package. On the first problem it records
// the error, the failing stage and stops forwarding, so the wrapped rasterizer
// never sees the malformed part.
type GuardAdder struct {
	Inner  Adder
	Limits Limits

	Err          error
	FailureStage string
	Segments     int // input segments forwarded
	Curves       int // quad+cubic count
	EstLines     int // estimated flattened lines charged to the budget
	Moves        int
	Min, Max     fixed.Point26_6
	HaveExtent   bool

	cur, first fixed.Point26_6
	started    bool
}

// NewGuardAdder wraps inner with the default limits.
func NewGuardAdder(inner Adder) *GuardAdder {
	return &GuardAdder{Inner: inner, Limits: DefaultLimits()}
}

func (g *GuardAdder) fail(stage string, err error) {
	if g.Err == nil {
		g.Err = err
		g.FailureStage = stage
	}
}

func (g *GuardAdder) addExtent(q fixed.Point26_6) {
	if !g.HaveExtent {
		g.Min, g.Max, g.HaveExtent = q, q, true
		return
	}
	if q.X < g.Min.X {
		g.Min.X = q.X
	}
	if q.Y < g.Min.Y {
		g.Min.Y = q.Y
	}
	if q.X > g.Max.X {
		g.Max.X = q.X
	}
	if q.Y > g.Max.Y {
		g.Max.Y = q.Y
	}
}

func (g *GuardAdder) check(stage string, pts ...fixed.Point26_6) bool {
	if g.Err != nil {
		return false
	}
	for _, p := range pts {
		if !IsSafePoint(p) {
			g.fail(stage, fmt.Errorf("%w: %v", ErrNonFinite, p))
			return false
		}
	}
	return true
}

func (g *GuardAdder) countSegment(stage string) bool {
	g.Segments++
	if g.Segments > g.Limits.inputLimit() {
		g.fail(stage, ErrTooManySegments)
		return false
	}
	return true
}

// addLines charges n flattened lines to the path-wide budget. It stops a
// long run of individually-legal curves from multiplying into unbounded work.
func (g *GuardAdder) addLines(stage string, n int) bool {
	g.EstLines += n
	if g.EstLines > g.Limits.outputLimit() {
		g.fail(stage, ErrTooManyOutputLines)
		return false
	}
	return true
}

// Start starts a new subpath.
func (g *GuardAdder) Start(a fixed.Point26_6) {
	if g.Err != nil || g.Inner == nil {
		return
	}
	if !g.check("start", a) {
		return
	}
	g.cur, g.first, g.started = a, a, true
	g.Moves++
	g.addExtent(a)
	g.Inner.Start(a)
}

// Line adds a line segment.
func (g *GuardAdder) Line(b fixed.Point26_6) {
	if g.Err != nil || g.Inner == nil {
		return
	}
	if !g.started {
		g.fail("line", fmt.Errorf("%w: line before start", ErrBadPath))
		return
	}
	if !g.check("line", b) || !g.countSegment("line") {
		return
	}
	g.cur = b
	g.addExtent(b)
	g.Inner.Line(b)
}

// QuadBezier adds a quadratic curve.
func (g *GuardAdder) QuadBezier(b, c fixed.Point26_6) {
	if g.Err != nil || g.Inner == nil {
		return
	}
	if !g.started {
		g.fail("quad", fmt.Errorf("%w: quad before start", ErrBadPath))
		return
	}
	if !g.check("quad", b, c) || !g.countSegment("quad") {
		return
	}
	if n, ok := g.Limits.EstimatedLines(devsqFixed(g.cur, b, c)); !ok {
		g.fail("quad", ErrTooComplex)
		return
	} else if !g.addLines("quad", n) {
		return
	}
	g.Curves++
	g.cur = c
	g.addExtent(b)
	g.addExtent(c)
	g.Inner.QuadBezier(b, c)
}

// CubeBezier adds a cubic curve.
func (g *GuardAdder) CubeBezier(b, c, d fixed.Point26_6) {
	if g.Err != nil || g.Inner == nil {
		return
	}
	if !g.started {
		g.fail("cubic", fmt.Errorf("%w: cubic before start", ErrBadPath))
		return
	}
	if !g.check("cubic", b, c, d) || !g.countSegment("cubic") {
		return
	}
	dv := devsqFixed(g.cur, b, d)
	if alt := devsqFixed(g.cur, c, d); alt > dv {
		dv = alt
	}
	if n, ok := g.Limits.EstimatedLines(dv); !ok {
		g.fail("cubic", ErrTooComplex)
		return
	} else if !g.addLines("cubic", n) {
		return
	}
	g.Curves++
	g.cur = d
	g.addExtent(b)
	g.addExtent(c)
	g.addExtent(d)
	g.Inner.CubeBezier(b, c, d)
}

// Stop closes the current subpath.
func (g *GuardAdder) Stop(closeLoop bool) {
	if g.Err != nil || g.Inner == nil {
		return
	}
	if closeLoop && g.started && g.cur != g.first {
		g.Line(g.first)
	}
	g.started = false
	g.Inner.Stop(closeLoop)
}

// devsqFixed returns the squared deviation QuadTo/CubeTo use for their
// flatness heuristic. The rasterizer's devSquared operates on 26.6 fixed
// integers directly, so the guard must use the same scale or its segment
// estimate will disagree with the real flattening.
func devsqFixed(a, b, c fixed.Point26_6) float64 {
	dx := float64(a.X) - 2*float64(b.X) + float64(c.X)
	dy := float64(a.Y) - 2*float64(b.Y) + float64(c.Y)
	return dx*dx + dy*dy
}

// GuardScanner validates and counts the flattened output of a rasterizer. It
// wraps any Scanner and applies the same coordinate bounds, so it can sit
// between a Filler and the real Scanner without either knowing about it.
type GuardScanner struct {
	Inner  Scanner
	Limits Limits

	Err          error
	FailureStage string
	StartCount   int
	LineCount    int
	Extent       fixed.Rectangle26_6
	DrawCount    int

	width, height int
	haveExtent    bool
}

// NewGuardScanner wraps inner with the default limits.
func NewGuardScanner(inner Scanner) *GuardScanner {
	return &GuardScanner{Inner: inner, Limits: DefaultLimits()}
}

func (g *GuardScanner) fail(stage string, err error) {
	if g.Err == nil {
		g.Err = err
		g.FailureStage = stage
	}
}

func (g *GuardScanner) addExtent(p fixed.Point26_6) {
	if !g.haveExtent {
		g.Extent.Min, g.Extent.Max, g.haveExtent = p, p, true
		return
	}
	if p.X < g.Extent.Min.X {
		g.Extent.Min.X = p.X
	}
	if p.Y < g.Extent.Min.Y {
		g.Extent.Min.Y = p.Y
	}
	if p.X > g.Extent.Max.X {
		g.Extent.Max.X = p.X
	}
	if p.Y > g.Extent.Max.Y {
		g.Extent.Max.Y = p.Y
	}
}

// Start records a moveto.
func (g *GuardScanner) Start(a fixed.Point26_6) {
	if g.Err != nil || g.Inner == nil {
		return
	}
	if !IsSafePoint(a) {
		g.fail("scan:start", fmt.Errorf("%w: %v", ErrNonFinite, a))
		return
	}
	g.StartCount++
	g.addExtent(a)
	g.Inner.Start(a)
}

// Line records a flattened line.
func (g *GuardScanner) Line(b fixed.Point26_6) {
	if g.Err != nil || g.Inner == nil {
		return
	}
	if !IsSafePoint(b) {
		g.fail("scan:line", fmt.Errorf("%w: %v", ErrNonFinite, b))
		return
	}
	g.LineCount++
	if g.LineCount > g.Limits.outputLimit() {
		g.fail("scan:line", ErrTooManyOutputLines)
		return
	}
	g.addExtent(b)
	g.Inner.Line(b)
}

// Draw flushes the accumulated path, converting a downstream panic into an
// error.
func (g *GuardScanner) Draw() {
	if g.Err != nil || g.Inner == nil {
		return
	}
	g.DrawCount++
	defer func() {
		if r := recover(); r != nil {
			g.fail("scan:draw", fmt.Errorf("%w: %v", ErrPanic, r))
		}
	}()
	g.Inner.Draw()
}

// GetPathExtent returns the extent recorded for the current path.
func (g *GuardScanner) GetPathExtent() fixed.Rectangle26_6 {
	if g.haveExtent {
		return g.Extent
	}
	return g.Inner.GetPathExtent()
}

// SetBounds validates and forwards the rasterizer bounds.
func (g *GuardScanner) SetBounds(w, h int) {
	if !ValidRasterizerBounds(w, h) {
		g.fail("scan:setbounds", boundsError(w, h))
		if g.Inner != nil {
			g.Inner.SetBounds(0, 0)
		}
		g.width, g.height = 0, 0
		return
	}
	g.width, g.height = w, h
	if g.Inner != nil {
		g.Inner.SetBounds(w, h)
	}
}

// SetColor forwards the paint.
func (g *GuardScanner) SetColor(c interface{}) {
	if g.Inner != nil {
		g.Inner.SetColor(c)
	}
}

// SetWinding forwards the fill rule.
func (g *GuardScanner) SetWinding(nonZero bool) {
	if g.Inner != nil {
		g.Inner.SetWinding(nonZero)
	}
}

// SetClip forwards the clip rectangle.
func (g *GuardScanner) SetClip(r image.Rectangle) {
	if g.Inner != nil {
		g.Inner.SetClip(r)
	}
}

// Clear resets the accumulated state, including the error.
func (g *GuardScanner) Clear() {
	g.Err = nil
	g.FailureStage = ""
	g.StartCount, g.LineCount, g.DrawCount = 0, 0, 0
	g.haveExtent = false
	g.Extent = fixed.Rectangle26_6{}
	if g.Inner != nil {
		g.Inner.Clear()
	}
}

// AddToChecked adds p to q with bounds and budget checks. Unlike AddTo it
// reports malformed input instead of forwarding it. The forwarding sequence is
// identical to AddTo for well-formed paths, so rendering is unchanged.
func (p Path) AddToChecked(q Adder) error {
	segments := 0
	for i := 0; i < len(p); {
		cmd := PathCommand(p[i])
		stage := fmt.Sprintf("cmd[%d]", i)
		switch cmd {
		case PathMoveTo:
			if i+3 > len(p) {
				return fmt.Errorf("%w: truncated moveto at %s", ErrBadPath, stage)
			}
			pt := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			if !IsSafePoint(pt) {
				return fmt.Errorf("%w: moveto %v", ErrNonFinite, pt)
			}
			q.Stop(false)
			q.Start(pt)
			i += 3
		case PathLineTo:
			if i+3 > len(p) {
				return fmt.Errorf("%w: truncated lineto at %s", ErrBadPath, stage)
			}
			pt := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			if !IsSafePoint(pt) {
				return fmt.Errorf("%w: lineto %v", ErrNonFinite, pt)
			}
			segments++
			if segments > MaxInputSegments {
				return ErrTooManySegments
			}
			q.Line(pt)
			i += 3
		case PathQuadTo:
			if i+5 > len(p) {
				return fmt.Errorf("%w: truncated quadto at %s", ErrBadPath, stage)
			}
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			if !IsSafePoint(b) || !IsSafePoint(c) {
				return fmt.Errorf("%w: quadto controls", ErrNonFinite)
			}
			segments++
			if segments > MaxInputSegments {
				return ErrTooManySegments
			}
			q.QuadBezier(b, c)
			i += 5
		case PathCubicTo:
			if i+7 > len(p) {
				return fmt.Errorf("%w: truncated cubicto at %s", ErrBadPath, stage)
			}
			b := fixed.Point26_6{X: p[i+1], Y: p[i+2]}
			c := fixed.Point26_6{X: p[i+3], Y: p[i+4]}
			d := fixed.Point26_6{X: p[i+5], Y: p[i+6]}
			if !IsSafePoint(b) || !IsSafePoint(c) || !IsSafePoint(d) {
				return fmt.Errorf("%w: cubicto controls", ErrNonFinite)
			}
			segments++
			if segments > MaxInputSegments {
				return ErrTooManySegments
			}
			q.CubeBezier(b, c, d)
			i += 7
		case PathClose:
			q.Stop(true)
			i++
		default:
			return fmt.Errorf("%w: command %d at %s", ErrBadPath, int64(cmd), stage)
		}
	}
	q.Stop(false)
	return nil
}
