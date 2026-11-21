# rasterx

Rasterx is a golang rasterizer that implements path stroking functions capable of SVG 2.0 compliant 'arc' joins and explicit loop closing. 



* Paths can be explicity closed or left open, resulting in a line join or end caps. 
* Arc joins are supported, which causes the extending edge from a Bezier curve to follow the radius of curvature at the end point rather than a straight line miter, resulting in a more fliud looking join. 
* Not specified in the SVG2.0 spec., but supported in rasterx is the arc-clip join, which is the arc join analog of a miter-clip join, both of which end the miter at a specified distance, rather than all or nothing.
* Several cap and gap functions in addition to those specified by SVG2.0 are implemented, specifically quad and cubic caps and gaps.
* Line start and end capping functions can be different.


![rasterx example](/doc/TestShapes4.svg.png?raw=true "Rasterx Example")

The above image shows the effect of using different join modes for a stroked curving path. The top stroked path uses miter (green) or arc (red, yellow, orange) join functions with high miter limit. The middle and lower path shows the effect of using the miter-clip and arc-clip joins, repectively, with different miter-limit values. The black chevrons at the top show different cap and gap functions.

## Scanner interface

Rasterx takes the path description of lines, bezier curves, and drawing parameters, and converts them into a set of straight line segments before rasterizing the lines to an image using some method of antialiasing. Rasterx abstracts this last step through the Scanner interface. There are two different structs that satisfy the Scanner interface; ScannerGV and [ScannerFT](https://github.com/srwiley/scanFT). ScannerGV wraps the rasterizer found in the golang.org/x/image/vector package. ScannerFT contains a modified version of the antialiaser found in the [golang freetype](https://github.com/golang/freetype) translation. These use different functions to connect an image to the antialiaser. ScannerFT uses a Painter to translate the raster onto the image, and ScannerGV uses the vector's Draw method with a source image and uses the path as an alpha mask. Please see the test files for examples. At this time, the ScannerFT is a bit faster as compared to ScannerGV for larger and less complicated images, while ScannerGV can be faster for smaller and more complex images. Also ScannerGV does not allow for using the even-odd winding rule, which is something the SVG specification uses. Since ScannerFT is subject to freetype style licensing rules, it lives [here](https://github.com/srwiley/scanFT) in a separate repository and must be imported into your project seperately. ScannerGV is included in the rasterx package, and has more go-friendly licensing. 

Below are the results of some benchmarks performed on a sample shape (the letter Q ). The first test is the time it takes to scan the image after all the curves have been flattened. The second test is the time it takes to flatten, and scan a simple filled image. The last test is the time it takes to flatten a stroked and dashed outline of the shape and scan it. Results for three different image sizes are shown.


```
128x128 Image
Test                        Rep       Time
BenchmarkScanGV-16          5000      287180 ns/op
BenchmarkFillGV-16          5000      339831 ns/op
BenchmarkDashGV-16          2000      968265 ns/op

BenchmarkScanFT-16    	   20000       88118 ns/op
BenchmarkFillFT-16    	    5000      214370 ns/op
BenchmarkDashFT-16    	    1000     2063797 ns/op

256x256 Image
Test                        Rep       Time
BenchmarkScanGV-16          2000     1188452 ns/op
BenchmarkFillGV-16          1000     1277268 ns/op
BenchmarkDashGV-16          500      2238169 ns/op

BenchmarkScanFT-16    	    5000      290685 ns/op
BenchmarkFillFT-16    	    3000      446329 ns/op
BenchmarkDashFT-16    	     500     2923512 ns/op

512x512 Image
Test                        Rep       Time
BenchmarkScanGV-16           500     3341038 ns/op
BenchmarkFillGV-16           500     4032213 ns/op
BenchmarkDashGV-16           200     6003355 ns/op

BenchmarkScanFT-16    	    5000      292884 ns/op
BenchmarkFillFT-16    	    3000      449582 ns/op
BenchmarkDashFT-16    	     500     2800493 ns/op
```

The package uses an interface called Rasterx, which is satisfied by three structs, Filler, Stroker and Dasher.  The Filler flattens Bezier curves into lines and uses an anonymously composed Scanner for the antialiasing step. The Stroker embeds a Filler and adds path stroking, and the Dasher embedds a Stroker and adds the ability to create dashed stroked curves.


![rasterx Scheme](/doc/schematic.png?raw=true "Rasterx Scheme")

Eash of the Filler, Dasher, and Stroker can function on their own and each implement the Rasterx interface, so if you need just the curve filling but no stroking capability, you only need a Filler. On the other hand if you have created a Dasher and want to use it to Fill, you can just do this:

```golang
filler := &dasher.Filler
```
Now filler is a filling rasterizer. Please see rasterx_test.go for examples.


### Non-standard library dependencies
rasterx requires the following imports which are not included in the go standard library:

* golang.org/x/image/math/fixed
* golang.org/x/image/vector

These can be included in your gopath by the following 'get' commands:

* "go get golang.org/x/image/vector"
* "go get golang.org/x/image/math/fixed"

If you want to use the freetype style antialiaser, 'go get' or clone into your workspace the scanFT package:

* github.com/srwiley/scanFT 

## High-quality antialiasing (supersampling)

The default `ScannerGV` resolves coverage once per output pixel. That is fast and area-accurate for ordinary geometry, but thin stroked icons and fine lines can look broken or "dashed" after scaling, because a partly covered pixel is rounded to one of few coverage levels. `ScannerSS` adds an optional, configurable high-quality mode based on grid (box) supersampling: the path is rasterized on an N×N finer grid with the same coverage engine, then the sample areas are averaged exactly back down to the destination resolution.

The default output is unchanged. A quality of `1` delegates entirely to `ScannerGV` and is bit-identical to the pre-existing renderer; the new behavior is opt-in.

```golang
// quality is the number of samples per pixel along each axis.
// 1 = identical to ScannerGV, higher values (up to MaxQuality = 8)
// use quality*quality coverage samples per output pixel.
scanner := rasterx.NewScannerSS(width, height, quality, dest, dest.Bounds())
filler := rasterx.NewFiller(width, height, scanner)
// ... build and draw exactly as before ...
```

Properties of the mode:

* **Conserved area and alpha.** Down-sampling is an exact box average of coverage, so the total covered area is preserved and a translucent source keeps the same effective coverage regardless of quality. Integer translations move the image pixel-for-pixel rather than flickering.
* **Applies to fills and strokes alike.** Because `ScannerSS` is a `Scanner`, it works with `NewFiller`, `NewStroker` and `NewDasher` unchanged, including caps, joins, dashes, gradients and color functions.
* **Bounded resources.** Quality is clamped to `MaxQuality` (8, i.e. 64 samples/pixel) and, independently, to the largest quality whose high-resolution mask fits within `MaxHiResPixels`. On a large canvas the quality is lowered automatically instead of exhausting memory; query `EffectiveQuality()` for the value in use.
* **Well-defined edge cases.** An empty path leaves the destination untouched, clipping restricts coverage exactly as with `ScannerGV`, zero/negative bounds render nothing, and non-zero destination origins place geometry at the correct absolute coordinates.

Use `SetQuality` to change the sampling quality at runtime, and `Quality()` / `EffectiveQuality()` to inspect the requested and effective values. See `supersample_test.go` for the analytic edge-error reference, area/alpha conservation, flicker, resource-limit and origin/clip tests.

