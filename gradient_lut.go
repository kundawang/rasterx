// Copyright 2018 The oksvg Authors. All rights reserved.
//
// Gradient look-up table (LUT) support.
//
// A gradient LUT is a read-only table of precomputed colors sampled along
// the gradient's stop ramp. It is built once from a snapshot of a
// Gradient's stops, spread method, opacity, transformation matrix and
// related fields, and is then shared by every pixel (and every layer)
// that evaluates the same gradient, replacing the per-pixel stop scan
// and interpolation with an O(1) table lookup.
//
// Snapshot / invalidation rules:
//
//   - A LUT captures the gradient state at the moment GetColorFunction is
//     called. Mutating the Gradient (stops, matrix, bounds, ...) afterwards
//     does NOT affect color functions already handed out.
//   - The cache key covers stops (offset, opacity and color), spread
//     method, opacity, gradient matrix, units, bounds, points and the
//     radial flag, so gradients that differ in any of these inputs never
//     share a cached LUT. (Blending is performed in the package's single
//     sRGB color space; there is no color-space parameter today, but any
//     future one must be added to lutKey.)
//   - Cache hits are verified against the full stop snapshot, so even a
//     hash collision cannot serve the wrong colors.
//
// Concurrency: a built LUT is immutable and safe for concurrent reads.
// The shared cache is guarded by a mutex, bounds its entry count and
// total memory, and can be emptied explicitly with ClearGradientLUTCache.
package rasterx

import (
	"container/list"
	"image/color"
	"math"
	"sync"
)

const (
	// DefaultGradientLUTSize is the default number of entries in a
	// gradient LUT. Each entry is 4 bytes (color.NRGBA).
	DefaultGradientLUTSize = 1024
	// MinGradientLUTSize and MaxGradientLUTSize clamp the LUT resolution
	// requested through SetGradientLUTSize.
	MinGradientLUTSize = 16
	MaxGradientLUTSize = 4096

	// DefaultMaxGradientLUTEntries bounds how many LUTs the shared
	// cache holds before evicting the least recently used ones.
	DefaultMaxGradientLUTEntries = 64
	// DefaultMaxGradientLUTBytes bounds the total memory held by the
	// shared LUT cache (64 entries * 1024 entries * 4 bytes = 256 KiB).
	DefaultMaxGradientLUTBytes = 1 << 20
)

// gradientLUT is an immutable, precomputed color ramp for one gradient
// configuration. After construction it is only read, so it may be shared
// freely across goroutines and layers.
type gradientLUT struct {
	table  []color.NRGBA // colors sampled at t = i/(len(table)-1)
	last   color.NRGBA   // final stop color, used by radial fallbacks
	spread SpreadMethod
}

// at returns the gradient color for parameter t, applying the spread
// method and interpolating between the two nearest table entries.
// Colors match Gradient.tColor at the sample points exactly and differ
// by at most one least-significant bit per channel elsewhere, except
// within one table cell of a ramp discontinuity (a repeat seam with
// mismatched endpoint stops, or duplicate stop offsets), which the
// interpolation straddles.
func (l *gradientLUT) at(t float64) color.NRGBA {
	n := len(l.table)
	switch l.spread {
	case RepeatSpread:
		t = math.Mod(t, 1.0)
		if t < 0 {
			t += 1.0
		}
		if t != t { // NaN: first table entry, matching pad's first stop.
			return l.table[0]
		}
		// The table is periodic for repeat: entry i holds t = i/n and
		// the last cell wraps to entry 0.
		x := t * float64(n)
		i := int(x)
		f := x - float64(i)
		if f <= 0 {
			return l.table[i]
		}
		j := i + 1
		if j >= n {
			j = 0
		}
		return lerpNRGBA(l.table[i], l.table[j], f)
	case ReflectSpread:
		t = math.Mod(t, 2.0)
		if t < 0 {
			t += 2.0
		}
		if t > 1.0 {
			t = 2.0 - t
		}
	default: // PadSpread
		if t >= 1.0 {
			return l.table[n-1]
		}
		if t <= 0.0 {
			return l.table[0]
		}
	}
	if t != t { // NaN: match the pad behavior of tColor, first stop color.
		return l.table[0]
	}
	x := t * float64(n-1)
	i := int(x)
	if i >= n-1 {
		return l.table[n-1]
	}
	f := x - float64(i)
	if f <= 0 {
		return l.table[i]
	}
	return lerpNRGBA(l.table[i], l.table[i+1], f)
}

// lerpNRGBA interpolates between a and b with fraction f in [0,1],
// rounding to nearest.
func lerpNRGBA(a, b color.NRGBA, f float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(float64(a.R)*(1-f) + float64(b.R)*f + 0.5),
		G: uint8(float64(a.G)*(1-f) + float64(b.G)*f + 0.5),
		B: uint8(float64(a.B)*(1-f) + float64(b.B)*f + 0.5),
		A: uint8(float64(a.A)*(1-f) + float64(b.A)*f + 0.5),
	}
}

// snapStop is the canonical, comparable form of a GradStop used for
// cache-key verification.
type snapStop struct {
	offset, opacity float64
	r, g, b, a      uint32
}

func snapshotStop(s *GradStop) snapStop {
	r, g, b, a := s.StopColor.RGBA()
	return snapStop{offset: s.Offset, opacity: s.Opacity, r: r, g: g, b: b, a: a}
}

// lutKey identifies one cached LUT. It covers every input that
// influences the gradient's colors so that different inputs can never
// map to the same entry.
type lutKey struct {
	points    [5]float64
	bounds    [4]float64
	matrix    Matrix2D
	opacity   float64
	stopsHash uint64
	numStops  int
	spread    SpreadMethod
	units     GradientUnits
	isRadial  bool
}

// lutEntry is one cached LUT plus the data needed to verify a hit.
type lutEntry struct {
	key   lutKey
	stops []snapStop // canonical snapshot used to confirm hits
	lut   *gradientLUT
	bytes int
}

// gradientLUTCache is a bounded LRU cache of built LUTs.
type gradientLUTCache struct {
	mu         sync.Mutex
	entries    map[lutKey]*list.Element
	lru        *list.List // front = most recently used; stores *lutEntry
	bytes      int
	maxEntries int
	maxBytes   int
	hits       uint64
	misses     uint64
}

var lutCache = &gradientLUTCache{
	entries:    make(map[lutKey]*list.Element),
	lru:        list.New(),
	maxEntries: DefaultMaxGradientLUTEntries,
	maxBytes:   DefaultMaxGradientLUTBytes,
}

var gradientLUTSize = DefaultGradientLUTSize

// SetGradientLUTSize sets the resolution (number of color entries) of
// newly built gradient LUTs. The value is clamped to
// [MinGradientLUTSize, MaxGradientLUTSize]; each entry costs 4 bytes.
// Existing cached LUTs are unaffected.
func SetGradientLUTSize(n int) {
	if n < MinGradientLUTSize {
		n = MinGradientLUTSize
	}
	if n > MaxGradientLUTSize {
		n = MaxGradientLUTSize
	}
	lutCache.mu.Lock()
	gradientLUTSize = n
	lutCache.mu.Unlock()
}

// SetGradientLUTCacheLimit bounds the shared LUT cache: at most
// maxEntries LUTs and at most maxBytes of table memory are retained;
// least recently used entries are evicted first. Non-positive values
// disable caching (LUTs are still built, just never stored).
func SetGradientLUTCacheLimit(maxEntries, maxBytes int) {
	lutCache.mu.Lock()
	lutCache.maxEntries = maxEntries
	lutCache.maxBytes = maxBytes
	lutCache.evictLocked()
	lutCache.mu.Unlock()
}

// ClearGradientLUTCache empties the shared LUT cache and resets its
// hit/miss statistics. Built LUTs already handed out stay valid.
func ClearGradientLUTCache() {
	lutCache.mu.Lock()
	lutCache.entries = make(map[lutKey]*list.Element)
	lutCache.lru.Init()
	lutCache.bytes = 0
	lutCache.hits = 0
	lutCache.misses = 0
	lutCache.mu.Unlock()
}

// GradientLUTCacheStats reports the number of cached LUTs, their total
// table memory in bytes, and the cache hit/miss counts since the last
// ClearGradientLUTCache.
func GradientLUTCacheStats() (entries, bytes int, hits, misses uint64) {
	lutCache.mu.Lock()
	defer lutCache.mu.Unlock()
	return len(lutCache.entries), lutCache.bytes, lutCache.hits, lutCache.misses
}

// hashStops folds the canonical stop snapshot into a 64-bit FNV-1a hash.
func hashStops(stops []snapStop) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	add := func(v uint64) {
		for i := 0; i < 8; i++ {
			h ^= v & 0xFF
			h *= prime64
			v >>= 8
		}
	}
	for _, s := range stops {
		add(math.Float64bits(s.offset))
		add(math.Float64bits(s.opacity))
		add(uint64(s.r)<<32 | uint64(s.g))
		add(uint64(s.b)<<32 | uint64(s.a))
	}
	return h
}

func sameStops(a, b []snapStop) bool {
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

// get returns the cached LUT for key if present and verified, moving it
// to the front of the LRU.
func (c *gradientLUTCache) get(key lutKey, stops []snapStop) *gradientLUT {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil
	}
	e := el.Value.(*lutEntry)
	if !sameStops(e.stops, stops) { // hash collision: never serve wrong colors
		c.misses++
		return nil
	}
	c.hits++
	c.lru.MoveToFront(el)
	return e.lut
}

// put inserts e, evicting least recently used entries to satisfy the
// entry and memory limits. Entries larger than the byte limit are not
// cached at all.
func (c *gradientLUTCache) put(e *lutEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxEntries <= 0 || c.maxBytes <= 0 || e.bytes > c.maxBytes {
		return
	}
	if el, ok := c.entries[e.key]; ok {
		old := el.Value.(*lutEntry)
		c.bytes -= old.bytes
		c.lru.Remove(el)
	}
	el := c.lru.PushFront(e)
	c.entries[e.key] = el
	c.bytes += e.bytes
	c.evictLocked()
}

// evictLocked drops least recently used entries until both limits hold.
// Callers must hold c.mu.
func (c *gradientLUTCache) evictLocked() {
	for (c.maxEntries >= 0 && len(c.entries) > c.maxEntries) ||
		(c.maxBytes >= 0 && c.bytes > c.maxBytes) {
		back := c.lru.Back()
		if back == nil {
			return
		}
		e := back.Value.(*lutEntry)
		delete(c.entries, e.key)
		c.lru.Remove(back)
		c.bytes -= e.bytes
	}
}

// gradientLUTFor returns the shared LUT for the gradient's current
// state, building and caching it on a miss. The gradient's stops are
// deep-copied and sorted into a canonical snapshot, so later mutation of
// the Gradient neither corrupts the cache nor affects the returned LUT.
func gradientLUTFor(g *Gradient, opacity float64) *gradientLUT {
	d := len(g.Stops)
	sorted := make([]*GradStop, d)
	for i, s := range g.Stops {
		cp := *s
		sorted[i] = &cp
	}
	sortStopsByOffset(sorted)

	snap := make([]snapStop, d)
	for i, s := range sorted {
		snap[i] = snapshotStop(s)
	}

	key := lutKey{
		points:    g.Points,
		bounds:    [4]float64{g.Bounds.X, g.Bounds.Y, g.Bounds.W, g.Bounds.H},
		matrix:    g.Matrix,
		opacity:   opacity,
		stopsHash: hashStops(snap),
		numStops:  d,
		spread:    g.Spread,
		units:     g.Units,
		isRadial:  g.IsRadial,
	}

	if lut := lutCache.get(key, snap); lut != nil {
		return lut
	}

	lutCache.mu.Lock()
	size := gradientLUTSize
	lutCache.mu.Unlock()

	lut := buildGradientLUT(sorted, g.Spread, opacity, size)
	lutCache.put(&lutEntry{
		key:   key,
		stops: snap,
		lut:   lut,
		bytes: len(lut.table) * 4,
	})
	return lut
}

// buildGradientLUT samples the gradient's stop ramp into a new table.
// The stops must already be sorted by offset. For RepeatSpread the table
// is periodic: entry i holds the color at t = i/size, so the ramp wraps
// seamlessly from the last entry back to the first. For PadSpread and
// ReflectSpread entry i holds t = i/(size-1), since reflection is
// exactly the pad ramp mirrored (see gradientLUT.at).
func buildGradientLUT(sortedStops []*GradStop, spread SpreadMethod, opacity float64, size int) *gradientLUT {
	snap := &Gradient{Stops: sortedStops, Spread: PadSpread}
	if spread == RepeatSpread {
		snap.Spread = RepeatSpread
	}
	table := make([]color.NRGBA, size)
	if spread == RepeatSpread {
		n := float64(size)
		for i := range table {
			table[i] = toNRGBA(snap.tColor(float64(i)/n, opacity))
		}
	} else {
		last := float64(size - 1)
		for i := range table {
			table[i] = toNRGBA(snap.tColor(float64(i)/last, opacity))
		}
	}
	lastStop := sortedStops[len(sortedStops)-1]
	return &gradientLUT{
		table:  table,
		last:   ApplyOpacity(lastStop.StopColor, lastStop.Opacity*opacity),
		spread: spread,
	}
}

// toNRGBA converts the colors produced by tColor/blendStops (always
// color.NRGBA via ApplyOpacity) without going through a color model.
func toNRGBA(c color.Color) color.NRGBA {
	if n, ok := c.(color.NRGBA); ok {
		return n
	}
	r, g, b, a := c.RGBA()
	return color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}
