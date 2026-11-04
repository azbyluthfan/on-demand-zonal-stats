package raster_test

import (
	"math"
	"math/rand"
	"testing"

	"github.com/paulmach/orb"

	"on-demand-zonal-stats/internal/geom"
	"on-demand-zonal-stats/internal/raster"
	"on-demand-zonal-stats/internal/testfixtures"
)

const tol = 1e-9 // absolute error allowed per pixel fraction

// unit is a 4x4 grid of 1x1 pixels covering x 0..4, y 0..4 (origin top-left).
var unit = raster.Grid{OriginX: 0, OriginY: 4, PixelW: 1, PixelH: 1, Cols: 4, Rows: 4}

func poly(pts ...orb.Point) orb.Polygon {
	r := orb.Ring(append(pts, pts[0]))
	return orb.Polygon{r}
}

// px addresses pixels by their lower-left world corner, which reads more
// naturally in tests than (col,row).
func fracAt(t *testing.T, g raster.Grid, c raster.Coverage, x, y float64) float64 {
	t.Helper()
	col := int(math.Floor((x - g.OriginX) / g.PixelW))
	row := int(math.Floor((g.OriginY-y)/g.PixelH)) - 1
	return c.At(col, row)
}

func total(c raster.Coverage) float64 {
	var s float64
	for _, w := range c.Weight {
		s += w
	}
	return s
}

func TestFractionalPixelEqualPolygon(t *testing.T) {
	c := raster.Fractional(unit, poly(orb.Point{1, 1}, orb.Point{2, 1}, orb.Point{2, 2}, orb.Point{1, 2}))
	if f := fracAt(t, unit, c, 1, 1); math.Abs(f-1) > tol {
		t.Errorf("fraction = %v, want 1", f)
	}
	if s := total(c); math.Abs(s-1) > tol {
		t.Errorf("total = %v, want 1 (no bleed into neighbours)", s)
	}
}

func TestFractionalLeftHalf(t *testing.T) {
	c := raster.Fractional(unit, poly(orb.Point{1, 1}, orb.Point{1.5, 1}, orb.Point{1.5, 2}, orb.Point{1, 2}))
	if f := fracAt(t, unit, c, 1, 1); math.Abs(f-0.5) > tol {
		t.Errorf("fraction = %v, want 0.5", f)
	}
}

func TestFractionalTriangle2x2(t *testing.T) {
	// Right triangle (0,0),(2,0),(0,2) over the 2x2 block at the origin.
	c := raster.Fractional(unit, poly(orb.Point{0, 0}, orb.Point{2, 0}, orb.Point{0, 2}))
	want := map[[2]float64]float64{
		{0, 0}: 1,   // bottom-left: fully under the hypotenuse
		{1, 0}: 0.5, // bottom-right
		{0, 1}: 0.5, // top-left
		{1, 1}: 0,   // top-right: touches at a single point
	}
	for xy, w := range want {
		if f := fracAt(t, unit, c, xy[0], xy[1]); math.Abs(f-w) > tol {
			t.Errorf("pixel at %v: fraction = %v, want %v", xy, f, w)
		}
	}
}

func TestFractionalHole(t *testing.T) {
	p := orb.Polygon{
		{{0, 0}, {2, 0}, {2, 2}, {0, 2}, {0, 0}},
		{{0.5, 0.5}, {0.5, 1.5}, {1.5, 1.5}, {1.5, 0.5}, {0.5, 0.5}},
	}
	c := raster.Fractional(unit, p)
	for _, xy := range [][2]float64{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		if f := fracAt(t, unit, c, xy[0], xy[1]); math.Abs(f-0.75) > tol {
			t.Errorf("pixel at %v: fraction = %v, want 0.75", xy, f)
		}
	}
}

func TestFractionalGeographicPixel(t *testing.T) {
	// Real-world-looking grid: 0.001° pixels near Yogyakarta, coordinates
	// that are not exactly representable in binary.
	g := raster.Grid{OriginX: 110.0, OriginY: -7.0, PixelW: 0.001, PixelH: 0.001, Cols: 500, Rows: 500}
	b := g.PixelBound(123, 45)
	c := raster.Fractional(g, poly(b.Min, orb.Point{b.Max[0], b.Min[1]}, b.Max, orb.Point{b.Min[0], b.Max[1]}))
	if f := c.At(123, 45); math.Abs(f-1) > tol {
		t.Errorf("fraction = %v, want 1", f)
	}
	if s := total(c); math.Abs(s-1) > tol {
		t.Errorf("total = %v, want 1", s)
	}
}

func TestFractionalMultiPolygon(t *testing.T) {
	mp := orb.MultiPolygon{
		poly(orb.Point{0, 0}, orb.Point{0.5, 0}, orb.Point{0.5, 1}, orb.Point{0, 1}),
		poly(orb.Point{2, 2}, orb.Point{3, 2}, orb.Point{3, 3}, orb.Point{2, 3}),
	}
	c := raster.Fractional(unit, mp)
	if f := fracAt(t, unit, c, 0, 0); math.Abs(f-0.5) > tol {
		t.Errorf("first part fraction = %v, want 0.5", f)
	}
	if f := fracAt(t, unit, c, 2, 2); math.Abs(f-1) > tol {
		t.Errorf("second part fraction = %v, want 1", f)
	}
}

// Σ fraction × pixel area must equal the polygon's area, for any polygon
// inside the grid. Random star-shaped polygons exercise concave clipping.
func TestFractionalConservesArea(t *testing.T) {
	g := raster.Grid{OriginX: 110.0, OriginY: -7.0, PixelW: 0.00083333, PixelH: 0.00083333, Cols: 400, Rows: 400}
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 50; n++ {
		cx, cy := 110.1+rng.Float64()*0.1, -7.1-rng.Float64()*0.1
		k := 5 + rng.Intn(40)
		pts := make([]orb.Point, k)
		for i := range pts {
			ang := 2 * math.Pi * float64(i) / float64(k)
			r := 0.002 + rng.Float64()*0.02
			pts[i] = orb.Point{cx + r*math.Cos(ang), cy + r*math.Sin(ang)}
		}
		p := poly(pts...)
		c := raster.Fractional(g, p)
		got := total(c) * g.PixelW * g.PixelH
		want := geom.PolygonArea(p)
		if rel := math.Abs(got-want) / want; rel > 1e-9 {
			t.Fatalf("polygon %d: covered area %v vs polygon area %v (rel err %g)", n, got, want, rel)
		}
	}
}

// Adjacent villages split shared pixels: fractions on the border sum to 1.
func TestFractionalAdjacentSumToOne(t *testing.T) {
	a := poly(orb.Point{0, 0}, orb.Point{1.5, 0}, orb.Point{1.5, 2}, orb.Point{0, 2})
	b := poly(orb.Point{1.5, 0}, orb.Point{3, 0}, orb.Point{3, 2}, orb.Point{1.5, 2})
	ca, cb := raster.Fractional(unit, a), raster.Fractional(unit, b)
	for _, y := range []float64{0, 1} {
		if s := fracAt(t, unit, ca, 1, y) + fracAt(t, unit, cb, 1, y); math.Abs(s-1) > tol {
			t.Errorf("border pixel at y=%v: fractions sum to %v, want 1", y, s)
		}
	}
}

func TestCenterHalfOpenNoDoubleCount(t *testing.T) {
	// Border at x=1.5 passes exactly through pixel centres of column 1.
	a := poly(orb.Point{0, 0}, orb.Point{1.5, 0}, orb.Point{1.5, 4}, orb.Point{0, 4})
	b := poly(orb.Point{1.5, 0}, orb.Point{4, 0}, orb.Point{4, 4}, orb.Point{1.5, 4})
	ca, cb := raster.Center(unit, a), raster.Center(unit, b)
	for row := 0; row < 4; row++ {
		for col := 0; col < 4; col++ {
			if s := ca.At(col, row) + cb.At(col, row); s != 1 {
				t.Errorf("pixel (%d,%d) counted %v times", col, row, s)
			}
		}
	}
}

func TestCenterTriangle(t *testing.T) {
	// Triangle (0,0),(3,0),(0,3): centres strictly below x+y=3 are in.
	c := raster.Center(unit, poly(orb.Point{0, 0}, orb.Point{3, 0}, orb.Point{0, 3}))
	// Centres (0.5,0.5),(1.5,0.5),(0.5,1.5) are inside; (1.5,1.5) is on the
	// hypotenuse; (2.5,0.5),(0.5,2.5) are on it too. Only the 3 strict ones
	// are guaranteed; check those plus a clearly-outside one.
	for _, xy := range [][2]float64{{0, 0}, {1, 0}, {0, 1}} {
		if fracAt(t, unit, c, xy[0], xy[1]) != 1 {
			t.Errorf("pixel at %v should be in", xy)
		}
	}
	if fracAt(t, unit, c, 2, 2) != 0 {
		t.Error("pixel at (2,2) should be out")
	}
}

func TestWindowForSnapsOutwardAndClamps(t *testing.T) {
	g := raster.Grid{OriginX: 0, OriginY: 10, PixelW: 1, PixelH: 1, Cols: 10, Rows: 10}
	w, ok := g.WindowFor(orb.Bound{Min: orb.Point{1.2, 3.5}, Max: orb.Point{3.0, 5.1}})
	want := raster.Window{Col0: 1, Row0: 4, Cols: 2, Rows: 3}
	if !ok || w != want {
		t.Errorf("WindowFor = %+v %v, want %+v", w, ok, want)
	}
	wb := g.WindowBound(w)
	if wb.Min != (orb.Point{1, 3}) || wb.Max != (orb.Point{3, 6}) {
		t.Errorf("WindowBound = %v", wb)
	}
	w, ok = g.WindowFor(orb.Bound{Min: orb.Point{-5, -5}, Max: orb.Point{0.5, 20}})
	if !ok || w != (raster.Window{Col0: 0, Row0: 0, Cols: 1, Rows: 10}) {
		t.Errorf("clamped WindowFor = %+v %v", w, ok)
	}
	if _, ok := g.WindowFor(orb.Bound{Min: orb.Point{20, 20}, Max: orb.Point{21, 21}}); ok {
		t.Error("bbox outside grid should give ok=false")
	}
}

func TestWeightedSumNodata(t *testing.T) {
	c := raster.Fractional(unit, poly(orb.Point{0, 3}, orb.Point{2, 3}, orb.Point{2, 3.5}, orb.Point{0, 3.5}))
	// Window is cols 0-1, row 0. Fractions 0.5 each.
	vals := []float64{10, -99999}
	r, err := c.WeightedSum(vals, raster.Nodata{Value: -99999, Set: true})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Sum-5) > tol || math.Abs(r.Weight-0.5) > tol || math.Abs(r.NodataWeight-0.5) > tol || r.PixelsTouched != 2 {
		t.Errorf("WeightedSum = %+v", r)
	}
	r, _ = c.WeightedSum([]float64{math.NaN(), 4}, raster.Nodata{})
	if math.Abs(r.Sum-2) > tol || math.Abs(r.NodataWeight-0.5) > tol {
		t.Errorf("NaN handling: %+v", r)
	}
	if _, err := c.WeightedSum([]float64{1}, raster.Nodata{}); err == nil {
		t.Error("expected length mismatch error")
	}
}

// On the fixture villages, fractional covered area equals polygon area.
func TestFractionalFixtureVillages(t *testing.T) {
	g := raster.Grid{OriginX: 110.29, OriginY: -7.79, PixelW: 0.00083333333, PixelH: 0.00083333333, Cols: 250, Rows: 100}
	for _, v := range testfixtures.Villages(t) {
		c := raster.Fractional(g, v.Geom)
		got := total(c) * g.PixelW * g.PixelH
		want := geom.PolygonArea(v.Geom)
		if rel := math.Abs(got-want) / want; rel > 1e-9 {
			t.Errorf("%s: covered area %v vs %v (rel %g)", v.ID, got, want, rel)
		}
	}
}

func BenchmarkFractionalLargeVillage(b *testing.B) {
	// ~100 km² village (0.09° square-ish, 2000 vertices) on a 3" (~90 m) grid.
	g := raster.Grid{OriginX: 110, OriginY: -7, PixelW: 1.0 / 1200, PixelH: 1.0 / 1200, Cols: 1200, Rows: 1200}
	k := 2000
	pts := make([]orb.Point, k)
	rng := rand.New(rand.NewSource(2))
	for i := range pts {
		ang := 2 * math.Pi * float64(i) / float64(k)
		r := 0.045 + rng.Float64()*0.002
		pts[i] = orb.Point{110.5 + r*math.Cos(ang), -7.5 + r*math.Sin(ang)}
	}
	p := poly(pts...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raster.Fractional(g, p)
	}
}
