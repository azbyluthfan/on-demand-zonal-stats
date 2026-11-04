package query_test

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"

	"on-demand-zonal-stats/internal/geom"
	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/testfixtures"
)

func openFixture(t *testing.T) *query.Engine {
	t.Helper()
	e, err := query.Open(context.Background(), query.Options{Base: testfixtures.Release(t), AllowInstall: true, Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

type fixturePOI struct {
	id, category string
	p            orb.Point
}

func fixturePOIs(t *testing.T) []fixturePOI {
	raw, err := os.ReadFile(filepath.Join(testfixtures.Dir(), "pois.geojson"))
	if err != nil {
		t.Fatal(err)
	}
	fc, err := geojson.UnmarshalFeatureCollection(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out []fixturePOI
	for _, f := range fc.Features {
		out = append(out, fixturePOI{f.Properties.MustString("poi_id"), f.Properties.MustString("category"), f.Geometry.(orb.Point)})
	}
	return out
}

// naive assigns every POI with the Go edge rule over ALL villages — an
// implementation independent of DuckDB, H3 and the release files.
func naive(t *testing.T) map[string]string {
	var cands []geom.Candidate
	for _, v := range testfixtures.Villages(t) {
		cands = append(cands, geom.Candidate{ID: v.ID, Geom: v.Geom})
	}
	out := map[string]string{}
	for _, p := range fixturePOIs(t) {
		if id, ok := geom.Assign(p.p, cands); ok {
			out[p.id] = id
		}
	}
	return out
}

var metrics = []query.POIMetric{
	{ID: "cafes", Categories: []string{"cafe", "coffee_shop"}},
	{ID: "schools", Categories: []string{"school"}},
	{ID: "all", Categories: []string{"cafe", "coffee_shop", "restaurant", "school", "mosque"}},
}

func expected(t *testing.T, villages []string) map[string]map[string]int64 {
	assign := naive(t)
	sel := map[string]bool{}
	for _, v := range villages {
		sel[v] = true
	}
	out := map[string]map[string]int64{}
	for _, m := range metrics {
		out[m.ID] = map[string]int64{}
		in := map[string]bool{}
		for _, c := range m.Categories {
			in[c] = true
		}
		for _, p := range fixturePOIs(t) {
			if v, ok := assign[p.id]; ok && sel[v] && in[p.category] {
				out[m.ID][v]++
			}
		}
	}
	return out
}

func checkArea(t *testing.T, e *query.Engine, name string, a query.Area, wantVillages []string) {
	t.Helper()
	ctx := context.Background()
	r, err := e.ResolveArea(ctx, a, 0)
	if err != nil {
		t.Fatalf("%s: resolve: %v", name, err)
	}
	if wantVillages != nil {
		got := r.IDs()
		if len(got) != len(wantVillages) || (len(got) > 0 && !equal(got, wantVillages)) {
			t.Fatalf("%s: villages %v, want %v", name, got, wantVillages)
		}
	}
	res, err := e.CountPOIs(ctx, r, metrics, nil)
	if err != nil {
		t.Fatalf("%s: count: %v", name, err)
	}
	want := expected(t, r.IDs())
	for _, m := range metrics {
		for _, v := range r.IDs() {
			if got, w := res.Counts[m.ID][v], want[m.ID][v]; got != w {
				t.Errorf("%s: %s/%s = %d, naive %d", name, m.ID, v, got, w)
			}
		}
		for v := range res.Counts[m.ID] {
			if !contains(r.IDs(), v) {
				t.Errorf("%s: result has unselected village %s", name, v)
			}
		}
	}
}

func TestGoldenFixtureCodeAreas(t *testing.T) {
	e := openFixture(t)
	checkArea(t, e, "province", query.Area{ProvinceCode: "34"}, []string{"V001", "V002", "V003", "V004", "V005", "V006"})
	checkArea(t, e, "regency", query.Area{RegencyCode: "3404"}, nil)
	checkArea(t, e, "district 340401", query.Area{DistrictCode: "340401"}, []string{"V001", "V002"})
	checkArea(t, e, "district 340403", query.Area{DistrictCode: "340403"}, []string{"V004", "V005", "V006"})
	checkArea(t, e, "unknown district", query.Area{DistrictCode: "999999"}, []string{})
}

func rect(x0, y0, x1, y1 float64) orb.Polygon {
	return orb.Polygon{{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}}
}

func TestGoldenFixtureDrawnAreas(t *testing.T) {
	e := openFixture(t)
	// Only V002 selected: border POIs on x=110.32 must still go to V001 (not counted here).
	checkArea(t, e, "V002 only", query.Area{Geometry: rect(110.325, -7.815, 110.335, -7.805)}, []string{"V002"})
	checkArea(t, e, "V001+V002", query.Area{Geometry: rect(110.31, -7.815, 110.33, -7.805)}, []string{"V001", "V002"})
	checkArea(t, e, "inside V004 hole", query.Area{Geometry: rect(110.375, -7.825, 110.385, -7.815)}, []string{})
	checkArea(t, e, "touching tiny V005", query.Area{Geometry: rect(110.4201, -7.8004, 110.43, -7.79)}, []string{"V005"})

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 40; i++ {
		x0, y0 := 110.29+rng.Float64()*0.18, -7.87+rng.Float64()*0.07
		w, h := 0.002+rng.Float64()*0.06, 0.002+rng.Float64()*0.04
		checkArea(t, e, "random", query.Area{Geometry: rect(x0, y0, x0+w, y0+h)}, nil)
	}
}

func TestEdgeCasesFixture(t *testing.T) {
	e := openFixture(t)
	r, _ := e.ResolveArea(context.Background(), query.Area{ProvinceCode: "34"}, 0)
	res, err := e.CountPOIs(context.Background(), r, []query.POIMetric{{ID: "c", Categories: []string{"cafe"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// From gen_fixtures.py: 6 hand-placed cafes on the V001|V002 border/corner
	// go to V001; the cafe in V004's hole is unassigned, the one on its hole
	// edge counts for V004.
	assign := naive(t)
	for _, id := range []string{"P0001", "P0002", "P0003", "P0004", "P0005", "P0006"} {
		if assign[id] != "V001" {
			t.Fatalf("naive reference disagrees on %s: %s", id, assign[id])
		}
	}
	if res.Unassigned["c"] < 1 {
		t.Errorf("expected the V004-hole cafe to be reported unassigned, got %d", res.Unassigned["c"])
	}
	if res.Stats.BoundaryTests == 0 || res.Stats.Interior == 0 {
		t.Errorf("expected both paths to be used: %+v", res.Stats)
	}
}

// Sum over villages + unassigned = POIs in the area's partition.
func TestInvariantFixture(t *testing.T) {
	e := openFixture(t)
	r, _ := e.ResolveArea(context.Background(), query.Area{ProvinceCode: "34"}, 0)
	res, err := e.CountPOIs(context.Background(), r, metrics, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, n := range res.Counts["all"] {
		sum += n
	}
	// Province-34 partition = POIs assigned to a fixture village, plus
	// unassigned ones that fall in some village's cell.
	if sum+res.Unassigned["all"] != res.Stats.Scanned || res.Stats.OtherVillage != 0 {
		t.Errorf("sum %d + unassigned %d != scanned %d (other %d)", sum, res.Unassigned["all"], res.Stats.Scanned, res.Stats.OtherVillage)
	}
	if int64(len(naive(t))) != sum {
		t.Errorf("assigned %d, naive %d", sum, len(naive(t)))
	}
}

func TestResolveErrors(t *testing.T) {
	e := openFixture(t)
	ctx := context.Background()
	if _, err := e.ResolveArea(ctx, query.Area{}, 0); !errors.Is(err, query.ErrInvalidArea) {
		t.Errorf("empty area: %v", err)
	}
	if _, err := e.ResolveArea(ctx, query.Area{ProvinceCode: "34", RegencyCode: "3404"}, 0); !errors.Is(err, query.ErrInvalidArea) {
		t.Errorf("two selectors: %v", err)
	}
	if _, err := e.ResolveArea(ctx, query.Area{ProvinceCode: "34"}, 3); !errors.Is(err, query.ErrTooManyVillages) {
		t.Errorf("limit 3: %v", err)
	}
	bowtie := orb.Polygon{{{110.30, -7.80}, {110.32, -7.82}, {110.32, -7.80}, {110.30, -7.82}, {110.30, -7.80}}}
	if _, err := e.ResolveArea(ctx, query.Area{Geometry: bowtie}, 0); !errors.Is(err, query.ErrInvalidArea) {
		t.Errorf("bowtie: %v", err)
	}
	if _, err := e.ResolveArea(ctx, query.Area{Geometry: orb.LineString{{0, 0}, {1, 1}}}, 0); !errors.Is(err, query.ErrInvalidArea) {
		t.Errorf("linestring: %v", err)
	}
}

func TestOpenMissingRelease(t *testing.T) {
	_, err := query.Open(context.Background(), query.Options{Base: t.TempDir(), AllowInstall: true})
	if !errors.Is(err, query.ErrStorage) {
		t.Errorf("missing manifest: %v", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
