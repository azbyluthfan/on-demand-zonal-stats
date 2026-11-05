package query_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/release"
	"on-demand-zonal-stats/internal/testfixtures"
)

// Real-data invariant tests on the DI Yogyakarta release.
// Local: uses out/releases/diy-2026-11-b if it exists (make prep-diy).
// RustFS: set ZS_S3_ENDPOINT=localhost:9000 (make up + prep-upload).
const diyRelease = "diy-2026-11-b"

func diyEngines(t *testing.T) map[string]*query.Engine {
	t.Helper()
	engines := map[string]*query.Engine{}
	local := diyLocal()
	if local != "" {
		e, err := query.Open(context.Background(), query.Options{Base: local, AllowInstall: true})
		if err != nil {
			t.Fatal(err)
		}
		engines["local"] = e
	}
	if ep := os.Getenv("ZS_S3_ENDPOINT"); ep != "" {
		e, err := query.Open(context.Background(), query.Options{
			Base: "s3://villagestat/releases/" + diyRelease, AllowInstall: true,
			S3: &query.S3Config{Endpoint: ep, AccessKey: "rustfsadmin", SecretKey: "rustfsadmin", Bucket: "villagestat"},
		})
		if err != nil {
			t.Fatal(err)
		}
		engines["rustfs"] = e
	}
	if len(engines) == 0 {
		t.Skip("no DIY release: run `make prep-diy` (local) or set ZS_S3_ENDPOINT")
	}
	t.Cleanup(func() {
		for _, e := range engines {
			e.Close()
		}
	})
	return engines
}

// diyLocal finds the DIY release: out/releases (make prep-diy) or the
// bundled copy in sample/releases (always present in the repo, used by CI).
func diyLocal() string {
	for _, d := range []string{"out", "sample"} {
		p := filepath.Join(testfixtures.RepoRoot(), d, "releases", diyRelease)
		if _, err := os.Stat(filepath.Join(p, "manifest.json")); err == nil {
			return p
		}
	}
	return ""
}

func allCategories(e *query.Engine) []query.POIMetric {
	return []query.POIMetric{
		{ID: "all", Categories: e.SortedCategories()},
		{ID: "cafes", Categories: []string{"cafe", "coffee_shop"}},
	}
}

func count(t *testing.T, e *query.Engine, a query.Area, ms []query.POIMetric) (*query.Resolved, *query.POIResult, time.Duration) {
	t.Helper()
	start := time.Now()
	r, err := e.ResolveArea(context.Background(), a, 10000)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.CountPOIs(context.Background(), r, ms, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r, res, time.Since(start)
}

func TestDIYInvariants(t *testing.T) {
	for name, e := range diyEngines(t) {
		t.Run(name, func(t *testing.T) {
			ms := allCategories(e)
			r, prov, dt := count(t, e, query.Area{ProvinceCode: "34"}, ms)
			t.Logf("province 34: %d villages, %v; stats %+v", len(r.Villages), dt.Round(time.Millisecond), prov.Stats)

			// 1. Sum over villages + unassigned = POIs in the province partition.
			var sum int64
			for _, n := range prov.Counts["all"] {
				sum += n
			}
			var partRows int64 = -1
			for _, a := range e.Manifest.Artifacts {
				if a.Path == release.POIPath(e.Manifest.POIRelease, "34") && a.Rows != nil {
					partRows = *a.Rows
				}
			}
			if sum+prov.Unassigned["all"] != partRows || prov.Stats.Scanned != partRows {
				t.Errorf("sum %d + unassigned %d vs partition rows %d (scanned %d)", sum, prov.Unassigned["all"], partRows, prov.Stats.Scanned)
			}

			// 2. Additivity: regencies and districts requested one by one give
			//    exactly the province's per-village counts (edge rule does not
			//    depend on the requested area).
			for _, level := range []string{"regency", "district"} {
				got := map[string]int64{}
				seen := map[string]bool{}
				var slowest time.Duration
				for _, v := range r.Villages {
					code := v.RegencyCode
					a := query.Area{RegencyCode: code}
					if level == "district" {
						code, a = v.DistrictCode, query.Area{DistrictCode: v.DistrictCode}
					}
					if seen[code] {
						continue
					}
					seen[code] = true
					_, res, dt := count(t, e, a, ms)
					slowest = max(slowest, dt)
					for vid, n := range res.Counts["all"] {
						got[vid] += n
					}
				}
				for _, v := range r.Villages {
					if got[v.ID] != prov.Counts["all"][v.ID] {
						t.Errorf("%s-wise count for %s = %d, province-wide %d", level, v.ID, got[v.ID], prov.Counts["all"][v.ID])
					}
				}
				t.Logf("%d %ss, slowest %v", len(seen), level, slowest.Round(time.Millisecond))
			}

			// 3. Determinism.
			_, again, _ := count(t, e, query.Area{ProvinceCode: "34"}, ms)
			for _, m := range ms {
				for v, n := range prov.Counts[m.ID] {
					if again.Counts[m.ID][v] != n {
						t.Fatalf("non-deterministic: %s/%s %d vs %d", m.ID, v, n, again.Counts[m.ID][v])
					}
				}
			}
		})
	}
}

// Golden check on real data: per-village counts equal a naive ST_Covers join
// of every POI against every village with the same edge rule.
func TestDIYGoldenNaive(t *testing.T) {
	engines := diyEngines(t)
	e, ok := engines["local"]
	if !ok {
		t.Skip("naive check reads local files")
	}
	_, res, _ := count(t, e, query.Area{ProvinceCode: "34"}, allCategories(e))

	local := diyLocal()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("LOAD spatial"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`WITH v AS (SELECT village_id, geometry FROM read_parquet('` + filepath.Join(local, release.VillagesPath) + `')),
		p AS (SELECT poi_id, geometry FROM read_parquet('` + filepath.Join(local, "pois", "release=*", "province_code=*", "part-0.parquet") + `')),
		a AS (SELECT p.poi_id, min(v.village_id) AS village_id FROM p JOIN v ON ST_Covers(v.geometry, p.geometry) GROUP BY p.poi_id)
		SELECT village_id, count(*) FROM a GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var v string
		var c int64
		if err := rows.Scan(&v, &c); err != nil {
			t.Fatal(err)
		}
		n++
		if res.Counts["all"][v] != c {
			t.Errorf("village %s: H3 split %d, naive %d", v, res.Counts["all"][v], c)
		}
	}
	if n < 400 {
		t.Errorf("naive join produced only %d villages", n)
	}
}

// New layers on real data: buildings are additive across districts and add
// up to what prep stored; land-cover shares of all classes sum to 1; the
// night-light mean is a sane radiance.
func TestDIYBuildingsAndLayers(t *testing.T) {
	for name, e := range diyEngines(t) {
		t.Run(name, func(t *testing.T) {
			if !e.Manifest.HasBuildings() {
				t.Skip("release has no buildings")
			}
			ctx := context.Background()
			r, err := e.ResolveArea(ctx, query.Area{ProvinceCode: "34"}, 10000)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			prov, err := e.Buildings(ctx, r, &query.Trace{NoCache: true})
			if err != nil {
				t.Fatal(err)
			}
			var n, unassigned int64
			for _, v := range r.Villages {
				n += prov.Count[v.ID]
				unassigned += prov.UnassignedByVillage[v.ID]
			}
			// Everything stored for province 34 is either counted or unassigned
			// (points won by another province's village live in its partition).
			var stored int64
			db, err := sql.Open("duckdb", "")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if local := diyLocal(); name == "local" && local != "" {
				p := func(rel string) string { return filepath.Join(local, filepath.FromSlash(rel)) }
				err := db.QueryRow(`SELECT (SELECT sum(buildings) FROM read_parquet('` + p(release.BuildingCellsPath(e.Manifest.BuildingRelease, "34")) +
					`')) + (SELECT count(*) FROM read_parquet('` + p(release.BuildingPointsPath(e.Manifest.BuildingRelease, "34")) + `'))`).Scan(&stored)
				if err != nil {
					t.Fatal(err)
				}
				if n+unassigned+prov.Stats.OtherVillage != stored {
					t.Errorf("counted %d + unassigned %d + other %d != stored %d", n, unassigned, prov.Stats.OtherVillage, stored)
				}
			}
			// Bit-identical on a second cold run (areas are summed as integers,
			// so DuckDB's parallel summation order cannot change the result).
			again, err := e.Buildings(ctx, r, &query.Trace{NoCache: true})
			if err != nil {
				t.Fatal(err)
			}
			for id, a := range prov.Area {
				if again.Area[id] != a || again.Count[id] != prov.Count[id] {
					t.Fatalf("non-deterministic buildings for %s: %v/%v vs %v/%v", id, a, prov.Count[id], again.Area[id], again.Count[id])
				}
			}
			t.Logf("province buildings %d (unassigned %d) in %v; stats %+v", n, unassigned, time.Since(start).Round(time.Millisecond), prov.Stats)

			got := map[string]int64{}
			seen := map[string]bool{}
			for _, v := range r.Villages {
				if seen[v.DistrictCode] {
					continue
				}
				seen[v.DistrictCode] = true
				dr, err := e.ResolveArea(ctx, query.Area{DistrictCode: v.DistrictCode}, 0)
				if err != nil {
					t.Fatal(err)
				}
				res, err := e.Buildings(ctx, dr, &query.Trace{NoCache: true})
				if err != nil {
					t.Fatal(err)
				}
				for id, c := range res.Count {
					got[id] += c
				}
			}
			for _, v := range r.Villages {
				if got[v.ID] != prov.Count[v.ID] {
					t.Errorf("district-wise buildings for %s = %d, province-wide %d", v.ID, got[v.ID], prov.Count[v.ID])
				}
			}

			var ms []query.RasterMetric
			for _, info := range e.Manifest.Rasters {
				switch {
				case info.IsCategorical():
					var all []int64
					for _, c := range info.Classes {
						all = append(all, c.Value)
					}
					ms = append(ms, query.RasterMetric{ID: "lc_all", Layer: info.ID, Method: "fractional", Stat: query.StatClassFraction, Classes: all})
				case info.Units != "people":
					ms = append(ms, query.RasterMetric{ID: "mean_" + info.ID, Layer: info.ID, Method: "fractional", Stat: query.StatMean})
				}
			}
			start = time.Now()
			res, err := e.RasterSums(ctx, r, ms, &query.Trace{NoCache: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				for _, v := range r.Villages {
					x := res.Values[m.ID][v.ID]
					if m.Stat == query.StatClassFraction && (x < 1-1e-9 || x > 1+1e-9) {
						t.Errorf("%s: %s all-class share %v", m.Layer, v.ID, x)
					}
					if m.Stat == query.StatMean && (x < 0 || x > 500) {
						t.Errorf("%s: %s mean %v", m.Layer, v.ID, x)
					}
				}
				t.Logf("%s: province %v", m.ID, res.Totals[m.ID])
			}
			t.Logf("rasters %v: windows %d, blocks %d, pixels %d, stages %v", time.Since(start).Round(time.Millisecond),
				res.Stats.Windows, res.Stats.Blocks, res.Stats.PixelsRead, res.Stats.Stages)
		})
	}
}
