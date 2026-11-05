package query_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"

	"on-demand-zonal-stats/internal/geom"
	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/testfixtures"
)

type fixtureBuilding struct {
	id   string
	p    orb.Point // centre, rounded to 1e-7° like prep
	area float64   // footprint area, m²
}

// fixtureBuildings reads the fixture rectangles. A rectangle's point on
// surface is its centre, which gen_fixtures.py puts on the 1e-7° grid.
func fixtureBuildings(t *testing.T) []fixtureBuilding {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testfixtures.Dir(), "buildings.geojson"))
	if err != nil {
		t.Fatal(err)
	}
	fc, err := geojson.UnmarshalFeatureCollection(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out []fixtureBuilding
	for _, f := range fc.Features {
		b := f.Geometry.(orb.Polygon).Bound()
		cx, cy := math.Round((b.Min[0]+b.Max[0])/2*1e7)/1e7, math.Round((b.Min[1]+b.Max[1])/2*1e7)/1e7
		out = append(out, fixtureBuilding{f.Properties.MustString("building_id"), orb.Point{cx, cy}, ellipsoidRectArea(b)})
	}
	return out
}

// ellipsoidRectArea is the exact area of a lon/lat rectangle on the WGS84
// ellipsoid. EPSG:6933 (prep's area CRS) is equal-area on that ellipsoid, so
// prep's footprint areas must agree to rounding.
func ellipsoidRectArea(b orb.Bound) float64 {
	const a, f = 6378137.0, 1 / 298.257223563
	e2 := f * (2 - f)
	e := math.Sqrt(e2)
	q := func(lat float64) float64 {
		s := math.Sin(lat * math.Pi / 180)
		return s/(1-e2*s*s) - math.Log((1-e*s)/(1+e*s))/(2*e)
	}
	return a * a * (1 - e2) / 2 * (b.Max[0] - b.Min[0]) * math.Pi / 180 * math.Abs(q(b.Max[1])-q(b.Min[1]))
}

// Buildings must match an independent Go assignment (edge rule over all
// villages) of every building's point, for areas of any shape, and be the
// same whether served from the caches or not.
func TestBuildingsAgainstNaive(t *testing.T) {
	e := openFixture(t)
	var cands []geom.Candidate
	for _, v := range testfixtures.Villages(t) {
		cands = append(cands, geom.Candidate{ID: v.ID, Geom: v.Geom})
	}
	wantN, wantA := map[string]int64{}, map[string]float64{}
	for _, b := range fixtureBuildings(t) {
		if v, ok := geom.Assign(b.p, cands); ok {
			wantN[v]++
			wantA[v] += b.area
		}
	}
	if wantN["V001"] == 0 || wantN["V004"] == 0 {
		t.Fatalf("fixture too sparse: %v", wantN)
	}
	ctx := context.Background()
	for _, a := range []query.Area{{ProvinceCode: "34"}, {DistrictCode: "340401"}, {DistrictCode: "340403"},
		{Geometry: rect(110.31, -7.83, 110.39, -7.805)}} {
		r, err := e.ResolveArea(ctx, a, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, noCache := range []bool{true, false, false} { // cold, then computed, then cached
			res, err := e.Buildings(ctx, r, &query.Trace{NoCache: noCache})
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range r.IDs() {
				if res.Count[v] != wantN[v] {
					t.Errorf("%+v: %s count %d, naive %d", a, v, res.Count[v], wantN[v])
				}
				if d := math.Abs(res.Area[v] - wantA[v]); d > 1e-6*wantA[v] {
					t.Errorf("%+v: %s area %.4f, ellipsoid %.4f", a, v, res.Area[v], wantA[v])
				}
			}
			for v := range res.Count {
				if !contains(r.IDs(), v) {
					t.Errorf("result has unselected village %s", v)
				}
			}
		}
	}
	// Edge cases (gen_fixtures.py): border centre -> V001, hole -> unassigned.
	r, _ := e.ResolveArea(ctx, query.Area{DistrictCode: "340403"}, 0)
	res, err := e.Buildings(ctx, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.UnassignedByVillage["V004"] < 1 {
		t.Errorf("building in V004's hole not reported as unassigned: %v", res.UnassignedByVillage)
	}
	if res.Count["V005"] != 1 {
		t.Errorf("V005 (smaller than an H3 cell) count %d, want 1", res.Count["V005"])
	}
}

// Land cover is in column bands (gen_fixtures.py), so class fractions of the
// fixture villages have closed forms in planar lon/lat area.
func TestLandCoverClassFractions(t *testing.T) {
	e, err := query.Open(context.Background(), query.Options{Base: testfixtures.ReleaseWithRasters(t), AllowInstall: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r, err := e.ResolveArea(context.Background(), query.Area{ProvinceCode: "34"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ms := []query.RasterMetric{
		{ID: "trees", Layer: "landcover", Method: "fractional", Stat: query.StatClassFraction, Classes: []int64{10}},
		{ID: "crop", Layer: "landcover", Method: "fractional", Stat: query.StatClassFraction, Classes: []int64{40}},
		{ID: "all", Layer: "landcover", Method: "fractional", Stat: query.StatClassFraction, Classes: []int64{10, 40, 50, 80}},
		{ID: "built_c", Layer: "landcover", Method: "center", Stat: query.StatClassFraction, Classes: []int64{50}},
		{ID: "pop_mean", Layer: "population_2020", Method: "fractional", Stat: query.StatMean},
		{ID: "pop_sum", Layer: "population_2020", Method: "fractional"},
	}
	res, err := e.RasterSums(context.Background(), r, ms, nil)
	if err != nil {
		t.Fatal(err)
	}
	near := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	near("V001 trees", res.Values["trees"]["V001"], 1)
	near("V002 crop", res.Values["crop"]["V002"], 1)
	near("V004 built (centre)", res.Values["built_c"]["V004"], 1)
	// V003: L-shape, 4e-4 deg² of 6e-4 west of 110.32.
	near("V003 trees", res.Values["trees"]["V003"], 2.0/3)
	near("V003 crop", res.Values["crop"]["V003"], 1.0/3)
	for _, v := range r.IDs() {
		near(v+" all classes", res.Values["all"][v], 1)
	}
	// Area totals are coverage-weighted, not means of village fractions.
	near("total all", res.Totals["all"], 1)

	// Mean over V001 (pixel-aligned, no nodata) is the formula's average.
	var sum float64
	for row := 20; row < 40; row++ {
		for c := 20; c < 40; c++ {
			sum += 1 + float64((c*7+row*3)%11) + 0.25*float64(c%4)
		}
	}
	if got := res.Values["pop_mean"]["V001"]; math.Abs(got-sum/400) > 1e-9 {
		t.Errorf("V001 mean %v, formula %v", got, sum/400)
	}
	if got := res.Totals["pop_sum"]; got <= 0 {
		t.Errorf("total sum %v", got)
	}
}
