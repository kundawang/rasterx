package rasterx

import (
	"image/color"
	"testing"
)

// benchGradient builds a linear gradient with n stops, resembling the
// shared gradients of a multi-layer poster.
func benchGradient(n int, spread SpreadMethod, m Matrix2D) *Gradient {
	g := &Gradient{Points: [5]float64{0, 0, 1, 0, 0},
		IsRadial: false, Matrix: m, Spread: spread}
	g.Bounds.X, g.Bounds.Y, g.Bounds.W, g.Bounds.H = 0, 0, 512, 512
	g.Stops = make([]*GradStop, n)
	for i := 0; i < n; i++ {
		g.Stops[i] = &GradStop{
			StopColor: color.NRGBA{uint8(i * 37), uint8(i * 91), uint8(i * 53), 0xFF},
			Offset:    float64(i) / float64(n-1),
			Opacity:   1.0 - float64(i%3)*0.1,
		}
	}
	return g
}

func benchRadialGradient(n int, m Matrix2D) *Gradient {
	g := benchGradient(n, ReflectSpread, m)
	g.IsRadial = true
	g.Points = [5]float64{0.5, 0.5, 0.5, 0.5, 0.5}
	return g
}

// fillPixels evaluates cf over a w*h block, the per-pixel hot path of a
// poster layer.
func fillPixels(cf ColorFunc, w, h int) {
	var acc color.NRGBA
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			acc = nrgbaOf(cf(x, y))
		}
	}
	sink = acc
}

var sink color.NRGBA

// --- Per-pixel cost: uncached stop interpolation vs LUT ---

func BenchmarkGradientLinearUncached(b *testing.B) {
	g := benchGradient(16, PadSpread, Identity)
	cf := g.getColorFunctionUncached(1.0).(ColorFunc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillPixels(cf, 512, 512)
	}
}

func BenchmarkGradientLinearLUT(b *testing.B) {
	ClearGradientLUTCache()
	g := benchGradient(16, PadSpread, Identity)
	cf := g.GetColorFunction(1.0).(ColorFunc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillPixels(cf, 512, 512)
	}
}

func BenchmarkGradientRadialUncached(b *testing.B) {
	g := benchRadialGradient(16, Identity)
	cf := g.getColorFunctionUncached(1.0).(ColorFunc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillPixels(cf, 512, 512)
	}
}

func BenchmarkGradientRadialLUT(b *testing.B) {
	ClearGradientLUTCache()
	g := benchRadialGradient(16, Identity)
	cf := g.GetColorFunction(1.0).(ColorFunc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillPixels(cf, 512, 512)
	}
}

// --- Multi-layer poster: 32 layers reusing one gradient ---

const benchLayers = 32

func BenchmarkGradientMultiLayerUncached(b *testing.B) {
	g := benchGradient(16, PadSpread, Identity)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for l := 0; l < benchLayers; l++ {
			cf := g.getColorFunctionUncached(1.0).(ColorFunc)
			fillPixels(cf, 64, 64)
		}
	}
}

func BenchmarkGradientMultiLayerLUT(b *testing.B) {
	ClearGradientLUTCache()
	g := benchGradient(16, PadSpread, Identity)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for l := 0; l < benchLayers; l++ {
			cf := g.GetColorFunction(1.0).(ColorFunc)
			fillPixels(cf, 64, 64)
		}
	}
	b.StopTimer()
	_, _, hits, misses := GradientLUTCacheStats()
	b.ReportMetric(float64(hits)/float64(hits+misses)*100, "hit%")
}

// --- Gradient matrix composition: same stops, per-layer transforms ---

func BenchmarkGradientMatrixVariantsLUT(b *testing.B) {
	ClearGradientLUTCache()
	grads := make([]*Gradient, 8)
	for i := range grads {
		m := Identity.Translate(float64(i*10), 0).Rotate(float64(i)*0.1).Scale(1+float64(i)*0.05, 1)
		grads[i] = benchGradient(16, PadSpread, m)
		grads[i].Stops = benchGradient(16, PadSpread, Identity).Stops // shared stops
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, g := range grads {
			cf := g.GetColorFunction(1.0).(ColorFunc)
			fillPixels(cf, 64, 64)
		}
	}
	b.StopTimer()
	entries, bytes, hits, misses := GradientLUTCacheStats()
	b.ReportMetric(float64(hits)/float64(hits+misses)*100, "hit%")
	b.ReportMetric(float64(entries), "entries")
	b.ReportMetric(float64(bytes), "lut-bytes")
}

// --- Concurrent readers, as when layers render on worker goroutines ---

func BenchmarkGradientLUTConcurrent(b *testing.B) {
	ClearGradientLUTCache()
	g := benchGradient(16, PadSpread, Identity)
	cf := g.GetColorFunction(1.0).(ColorFunc)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			fillPixels(cf, 64, 64)
		}
	})
}
