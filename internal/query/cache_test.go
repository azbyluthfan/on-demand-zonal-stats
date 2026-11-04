package query_test

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/paulmach/orb"

	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/testfixtures"
)

type answer struct {
	Counts     map[string]map[string]int64
	Unassigned map[string]int64
	ByVillage  map[string]map[string]int64
	Raster     map[string]map[string]float64
}

func ask(t *testing.T, e *query.Engine, a query.Area, pm []query.POIMetric, rm []query.RasterMetric, tr *query.Trace) answer {
	t.Helper()
	ctx := context.Background()
	r, err := e.ResolveArea(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.CountPOIs(ctx, r, pm, tr)
	if err != nil {
		t.Fatal(err)
	}
	out := answer{Counts: p.Counts, Unassigned: p.Unassigned, ByVillage: p.UnassignedByVillage}
	if len(rm) > 0 {
		rr, err := e.RasterSums(ctx, r, rm, tr)
		if err != nil {
			t.Fatal(err)
		}
		out.Raster = rr.Values
	}
	return out
}

func areas(rng *rand.Rand, n int) []query.Area {
	as := []query.Area{{ProvinceCode: "34"}, {DistrictCode: "340401"}, {DistrictCode: "340403"}}
	for i := 0; i < n; i++ {
		x0, y0 := 110.29+rng.Float64()*0.18, -7.87+rng.Float64()*0.07
		w, h := 0.002+rng.Float64()*0.06, 0.002+rng.Float64()*0.04
		as = append(as, query.Area{Geometry: orb.Polygon{{{x0, y0}, {x0 + w, y0}, {x0 + w, y0 + h}, {x0, y0 + h}, {x0, y0}}}})
	}
	return as
}

// Results with cache warm equal results with cache cold, and
// equal an engine with caching disabled. Overlapping random areas and
// changing category sets mix hits and misses within single requests.
func TestCacheWarmEqualsCold(t *testing.T) {
	base := testfixtures.Release(t)
	var rm []query.RasterMetric
	if dir := tryRasterRelease(t); dir != "" {
		base = dir
		rm = rasterMetrics
	}
	cached, err := query.Open(context.Background(), query.Options{Base: base, AllowInstall: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	plain, err := query.Open(context.Background(), query.Options{Base: base, AllowInstall: true, Cache: query.CacheConfig{Disabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()

	rng := rand.New(rand.NewSource(11))
	catSets := [][]query.POIMetric{
		metrics,
		{{ID: "cafes", Categories: []string{"cafe", "coffee_shop"}}},
		{{ID: "x", Categories: []string{"mosque", "school"}}, {ID: "y", Categories: []string{"cafe"}}},
	}
	var hits int64
	for i, a := range areas(rng, 30) {
		pm := catSets[i%len(catSets)]
		cold := ask(t, cached, a, pm, rm, &query.Trace{NoCache: true})
		tr := &query.Trace{}
		warm := ask(t, cached, a, pm, rm, tr)
		again := ask(t, cached, a, pm, rm, &query.Trace{})
		off := ask(t, plain, a, pm, rm, nil)
		for name, got := range map[string]answer{"warm": warm, "again": again, "disabled": off} {
			if !reflect.DeepEqual(got, cold) {
				t.Fatalf("area %d: %s != cold\n%s: %+v\ncold: %+v", i, name, name, got, cold)
			}
		}
		hits += tr.Cache.ResultHits
	}
	if hits == 0 {
		t.Error("no result-cache hits across repeated, overlapping requests")
	}
}

// Concurrent requests (shared reference-table loads, LRU churn) give the
// same answers as sequential cold runs. Run with -race.
func TestCacheConcurrent(t *testing.T) {
	base := testfixtures.Release(t)
	var rm []query.RasterMetric
	if dir := tryRasterRelease(t); dir != "" {
		base = dir
		rm = rasterMetrics
	}
	// Tiny caches force evictions while requests are in flight.
	e, err := query.Open(context.Background(), query.Options{Base: base, AllowInstall: true,
		Cache: query.CacheConfig{ResultEntries: 50, BlockMB: 2, RefProvinces: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	as := areas(rand.New(rand.NewSource(5)), 12)
	want := make([]answer, len(as))
	for i, a := range as {
		want[i] = ask(t, e, a, metrics, rm, &query.Trace{NoCache: true})
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for k := 0; k < 3*len(as); k++ {
				i := (g*7 + k) % len(as)
				if got := ask(t, e, as[i], metrics, rm, &query.Trace{}); !reflect.DeepEqual(got, want[i]) {
					errs <- fmt.Sprintf("goroutine %d area %d differs", g, i)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

func tryRasterRelease(t *testing.T) (dir string) {
	t.Helper()
	ok := t.Run("raster-release", func(t *testing.T) { dir = testfixtures.ReleaseWithRasters(t) })
	if !ok {
		return ""
	}
	return dir
}
