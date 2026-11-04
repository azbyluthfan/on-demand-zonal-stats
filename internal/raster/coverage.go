// Package raster computes per-village pixel coverage and weighted sums.
//
// Two methods:
//   - Fractional: each pixel weighs area(pixel ∩ polygon) / area(pixel),
//     computed exactly by polygon clipping (same definition as exactextract).
//   - Center: a pixel counts fully if its centre is inside the polygon
//     (half-open rule, so adjacent villages never share a pixel).
//
// Pixel I/O is not done here: callers read the pixel values for
// Grid.WindowBound(w) (e.g. via the cog extension's RS_BandAsArray, with the
// bbox snapped to pixel edges, see docs/spikes.md §2) and pass them in.
package raster

import (
	"fmt"
	"math"

	"github.com/paulmach/orb"

	"on-demand-zonal-stats/internal/geom"
)

// Grid is a north-up raster grid (no rotation/skew).
type Grid struct {
	OriginX, OriginY float64 // upper-left corner
	PixelW, PixelH   float64 // positive; rows go down (y decreases)
	Cols, Rows       int
}

// Validate rejects grids this package cannot handle.
func (g Grid) Validate() error {
	if !(g.PixelW > 0) || !(g.PixelH > 0) || g.Cols <= 0 || g.Rows <= 0 {
		return fmt.Errorf("raster: invalid grid %+v", g)
	}
	return nil
}

// Window is a pixel rectangle [Col0, Col0+Cols) x [Row0, Row0+Rows).
type Window struct{ Col0, Row0, Cols, Rows int }

// Len is the number of pixels in the window.
func (w Window) Len() int { return w.Cols * w.Rows }

// PixelBound returns the world bounds of pixel (col, row).
func (g Grid) PixelBound(col, row int) orb.Bound {
	x0 := g.OriginX + float64(col)*g.PixelW
	x1 := g.OriginX + float64(col+1)*g.PixelW
	y1 := g.OriginY - float64(row)*g.PixelH
	y0 := g.OriginY - float64(row+1)*g.PixelH
	return orb.Bound{Min: orb.Point{x0, y0}, Max: orb.Point{x1, y1}}
}

// PixelCenter returns the world coordinates of pixel (col, row)'s centre.
func (g Grid) PixelCenter(col, row int) orb.Point {
	return orb.Point{
		g.OriginX + (float64(col)+0.5)*g.PixelW,
		g.OriginY - (float64(row)+0.5)*g.PixelH,
	}
}

// WindowFor returns the smallest pixel window covering b, snapped outward
// and clipped to the grid. ok is false if b misses the grid.
func (g Grid) WindowFor(b orb.Bound) (w Window, ok bool) {
	c0 := int(math.Floor((b.Min[0] - g.OriginX) / g.PixelW))
	c1 := int(math.Ceil((b.Max[0] - g.OriginX) / g.PixelW))
	r0 := int(math.Floor((g.OriginY - b.Max[1]) / g.PixelH))
	r1 := int(math.Ceil((g.OriginY - b.Min[1]) / g.PixelH))
	c0, r0 = max(c0, 0), max(r0, 0)
	c1, r1 = min(c1, g.Cols), min(r1, g.Rows)
	if c1 <= c0 || r1 <= r0 {
		return Window{}, false
	}
	return Window{Col0: c0, Row0: r0, Cols: c1 - c0, Rows: r1 - r0}, true
}

// WindowBound is the world bounds of a window (exactly on pixel edges).
func (g Grid) WindowBound(w Window) orb.Bound {
	tl := g.PixelBound(w.Col0, w.Row0)
	br := g.PixelBound(w.Col0+w.Cols-1, w.Row0+w.Rows-1)
	return orb.Bound{Min: orb.Point{tl.Min[0], br.Min[1]}, Max: orb.Point{br.Max[0], tl.Max[1]}}
}

// Coverage holds one weight per pixel of Window, row-major.
type Coverage struct {
	Window
	Weight []float64
}

// At returns the weight of the absolute pixel (col, row), 0 outside.
func (c Coverage) At(col, row int) float64 {
	x, y := col-c.Col0, row-c.Row0
	if x < 0 || y < 0 || x >= c.Cols || y >= c.Rows {
		return 0
	}
	return c.Weight[y*c.Cols+x]
}

// Fractional computes exact covered fractions for every pixel g's polygon
// bbox touches. g must be an orb.Polygon or orb.MultiPolygon.
func Fractional(grid Grid, g orb.Geometry) Coverage {
	w, ok := grid.WindowFor(g.Bound())
	if !ok {
		return Coverage{}
	}
	cov := Coverage{Window: w, Weight: make([]float64, w.Len())}

	var strip, a, b geom.Path
	for _, poly := range geom.Polygons(g) {
		rings := make([]geom.Path, len(poly))
		for i, r := range poly {
			rings[i] = geom.OpenRing(r)
		}
		strips := make([]geom.Path, len(rings))
		for row := w.Row0; row < w.Row0+w.Rows; row++ {
			pb := grid.PixelBound(w.Col0, row)
			y0, y1 := pb.Min[1], pb.Max[1]
			// Stage 1: clip every ring to this row's horizontal strip.
			xmin, xmax := math.Inf(1), math.Inf(-1)
			for i, r := range rings {
				a = geom.ClipAxis(r, geom.Y, y0, true, a)
				strip = geom.ClipAxis(a, geom.Y, y1, false, strips[i][:0])
				strips[i] = strip
				if i == 0 {
					for _, p := range strip {
						xmin, xmax = min(xmin, p[0]), max(xmax, p[0])
					}
				}
			}
			if len(strips[0]) < 3 {
				continue
			}
			// Stage 2: clip the strip to each pixel's vertical slab.
			cFrom := max(w.Col0, int(math.Floor((xmin-grid.OriginX)/grid.PixelW)))
			cTo := min(w.Col0+w.Cols, int(math.Ceil((xmax-grid.OriginX)/grid.PixelW)))
			for col := cFrom; col < cTo; col++ {
				pxb := grid.PixelBound(col, row)
				pixArea := (pxb.Max[0] - pxb.Min[0]) * (y1 - y0)
				var area float64
				for i, s := range strips {
					if len(s) < 3 {
						continue
					}
					a = geom.ClipAxis(s, geom.X, pxb.Min[0], true, a)
					b = geom.ClipAxis(a, geom.X, pxb.Max[0], false, b)
					if i == 0 {
						area += geom.Area(b)
					} else {
						area -= geom.Area(b)
					}
				}
				if area > 0 {
					k := (row-w.Row0)*w.Cols + (col - w.Col0)
					cov.Weight[k] += area / pixArea
				}
			}
		}
	}
	for i, v := range cov.Weight {
		cov.Weight[i] = min(max(v, 0), 1) // float noise only; valid input stays in [0,1]
	}
	return cov
}

// Center marks pixels whose centre is inside g (weight 1), else 0.
func Center(grid Grid, g orb.Geometry) Coverage {
	w, ok := grid.WindowFor(g.Bound())
	if !ok {
		return Coverage{}
	}
	cov := Coverage{Window: w, Weight: make([]float64, w.Len())}
	for row := w.Row0; row < w.Row0+w.Rows; row++ {
		for col := w.Col0; col < w.Col0+w.Cols; col++ {
			if geom.ContainsHalfOpen(g, grid.PixelCenter(col, row)) {
				cov.Weight[(row-w.Row0)*w.Cols+(col-w.Col0)] = 1
			}
		}
	}
	return cov
}

// Result is a weighted sum over one village.
type Result struct {
	Sum           float64 // Σ value × weight over valid pixels
	Weight        float64 // Σ weight over valid pixels (pixel-equivalents covered)
	NodataWeight  float64 // Σ weight over nodata/NaN pixels (reported, not summed)
	PixelsTouched int     // pixels with weight > 0
}

// Nodata describes the raster's nodata value, if any.
type Nodata struct {
	Value float64
	Set   bool
}

// WeightedSum combines values (row-major, same window as c) with weights.
func (c Coverage) WeightedSum(values []float64, nd Nodata) (Result, error) {
	if len(values) != len(c.Weight) {
		return Result{}, fmt.Errorf("raster: %d values for window of %d pixels", len(values), len(c.Weight))
	}
	var r Result
	for i, wt := range c.Weight {
		if wt == 0 {
			continue
		}
		r.PixelsTouched++
		v := values[i]
		if math.IsNaN(v) || (nd.Set && v == nd.Value) {
			r.NodataWeight += wt
			continue
		}
		r.Sum += v * wt
		r.Weight += wt
	}
	return r, nil
}
