package query_test

import (
	"bufio"
	"context"
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/testfixtures"
)

var rasterMetrics = []query.RasterMetric{
	{ID: "frac", Layer: "population_2020", Method: "fractional"},
	{ID: "center", Layer: "population_2020", Method: "center"},
}

type golden struct {
	frac, center map[string]float64
	unionFrac    float64
}

// loadGolden reads a CSV written by scripts/golden_raster.py
// (exactextract = fractional, rasterstats all_touched=False = center).
func loadGolden(t *testing.T, name string) golden {
	t.Helper()
	f, err := os.Open(filepath.Join(testfixtures.RepoRoot(), "testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g := golden{frac: map[string]float64{}, center: map[string]float64{}}
	var body []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "# union_fractional_sum ") {
			g.unionFrac, _ = strconv.ParseFloat(strings.TrimPrefix(line, "# union_fractional_sum "), 64)
			continue
		}
		if !strings.HasPrefix(line, "#") {
			body = append(body, line)
		}
	}
	recs, err := csv.NewReader(strings.NewReader(strings.Join(body, "\n"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs[1:] {
		g.frac[r[0]], _ = strconv.ParseFloat(r[1], 64)
		g.center[r[0]], _ = strconv.ParseFloat(r[2], 64)
	}
	return g
}

func rasterSums(t *testing.T, e *query.Engine, a query.Area) (*query.Resolved, *query.RasterResult) {
	t.Helper()
	r, err := e.ResolveArea(context.Background(), a, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.RasterSums(context.Background(), r, rasterMetrics, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r, res
}

// fracOK is the golden-test criterion: relative error < 0.1%, or absolute
// < 0.5 people for tiny villages.
func fracOK(got, want float64) bool {
	d := math.Abs(got - want)
	return d < 0.5 || d <= 1e-3*math.Abs(want)
}

// centerOK: "exact match", allowing float32 accumulation in rasterstats.
func centerOK(got, want float64) bool { return math.Abs(got-want) <= 1e-5*max(1, math.Abs(want)) }

// The fixture raster's values are known in closed form (gen_fixtures.py), and
// V001 is a square aligned with pixel edges, so its sum can be computed with
// no geometry at all. This checks window placement, row order and values.
func TestRasterWindowAgainstFormula(t *testing.T) {
	e, err := query.Open(context.Background(), query.Options{Base: testfixtures.ReleaseWithRasters(t), AllowInstall: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	_, res := rasterSums(t, e, query.Area{DistrictCode: "340401"})
	// V001: lon 110.30..110.32 -> cols 20..39; lat -7.80..-7.82 -> rows 20..39.
	var want float64
	for r := 20; r < 40; r++ {
		for c := 20; c < 40; c++ {
			want += 1 + float64((c*7+r*3)%11) + 0.25*float64(c%4)
		}
	}
	for _, m := range []string{"frac", "center"} {
		// 1e-9 per pixel fraction, summed over 400 pixels of ~6.
		if got := res.Values[m]["V001"]; math.Abs(got-want) > 1e-6 {
			t.Errorf("%s V001 = %v, formula %v", m, got, want)
		}
	}
	if res.Stats.Windows != 2 || res.Stats.PixelsRead == 0 {
		t.Errorf("stats %+v", res.Stats)
	}
}

func TestRasterGoldenFixture(t *testing.T) {
	e, err := query.Open(context.Background(), query.Options{Base: testfixtures.ReleaseWithRasters(t), AllowInstall: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	g := loadGolden(t, "fixture_population_2020.csv")
	r, res := rasterSums(t, e, query.Area{ProvinceCode: "34"})
	for _, v := range r.IDs() {
		if got := res.Values["frac"][v]; !fracOK(got, g.frac[v]) {
			t.Errorf("fractional %s = %v, exactextract %v", v, got, g.frac[v])
		}
		got, want := res.Values["center"][v], g.center[v]
		if v == "V005" {
			// Known tie: a pixel centre lies exactly on V005's vertex
			// (110.4205, -7.8005). GDAL's rasterizer counts it; our half-open
			// rule (no double counting between neighbours) does not.
			if got != 0 || want != 7 {
				t.Errorf("V005 tie case changed: ours %v, rasterstats %v", got, want)
			}
			continue
		}
		if !centerOK(got, want) {
			t.Errorf("center %s = %v, rasterstats %v", v, got, want)
		}
	}
}

func TestRasterGoldenDIY(t *testing.T) {
	g := loadGolden(t, "diy_population_2020.csv")
	for name, e := range diyEngines(t) {
		t.Run(name, func(t *testing.T) {
			r, res := rasterSums(t, e, query.Area{ProvinceCode: "34"})
			var sumFrac, sumCenter, maxRel float64
			var worst string
			for _, v := range r.IDs() {
				f, c := res.Values["frac"][v], res.Values["center"][v]
				sumFrac += f
				sumCenter += c
				if !fracOK(f, g.frac[v]) {
					t.Errorf("fractional %s = %v, exactextract %v", v, f, g.frac[v])
				}
				if !centerOK(c, g.center[v]) {
					t.Errorf("center %s = %v, rasterstats %v", v, c, g.center[v])
				}
				if f > 100 {
					if rel := math.Abs(f-c) / f; rel > maxRel {
						maxRel, worst = rel, v
					}
				}
			}
			// Invariant: Σ fractional over villages = raster sum over
			// the union of villages (exactextract on the dissolved union).
			if rel := math.Abs(sumFrac-g.unionFrac) / g.unionFrac; rel > 1e-3 {
				t.Errorf("Σ fractional %v vs union %v (rel %g)", sumFrac, g.unionFrac, rel)
			}
			t.Logf("DIY population: fractional %.0f, center %.0f (union ref %.0f); largest center error %.1f%% in %s; windows %d, pixels %d, stages %v",
				sumFrac, sumCenter, g.unionFrac, 100*maxRel, worst, res.Stats.Windows, res.Stats.PixelsRead, res.Stats.Stages)
		})
	}
}
