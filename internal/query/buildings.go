package query

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"on-demand-zonal-stats/internal/release"
)

// BuildingStats explains building work for diagnostics. Counts cover the
// villages computed for this request (cache misses).
type BuildingStats struct {
	CellRows      int64 // pre-summed interior cells read
	CellBuildings int64 // buildings counted through those cells, no geometry test
	Points        int64 // boundary-cell building points read
	PointTests    int64 // exact ST_Covers tests
	Unassigned    int64 // points in a selected village's cells covered by no village
	OtherVillage  int64 // points won by (or attributed to) a village outside the area
	ComputedFor   int   // villages computed
	Stages        map[string]time.Duration
}

// BuildingResult holds per-village building counts and footprint areas
// (villages with no buildings omitted).
type BuildingResult struct {
	Count map[string]int64   // village ID -> buildings
	Area  map[string]float64 // village ID -> Σ footprint area, m²
	// UnassignedByVillage: buildings in no village, attributed like POIs to
	// the lowest candidate village of their cell; not counted in Count.
	UnassignedByVillage map[string]int64
	Unassigned          int64
	Stats               BuildingStats
}

// buildingVal is one village's result. Area is summed as an integer
// (areaUnit m²) so it is exact and independent of DuckDB's parallel
// summation order: replicas and repeated runs return identical values.
type buildingVal struct {
	count, unassigned int64
	area              int64 // in areaUnit
}

const areaUnit = 1e-4 // m² (1 cm²)

// Buildings counts buildings and sums their footprint area per village
// (docs/buildings.md). A building belongs to the village containing its
// point on surface, with the POI edge rule.
func (e *Engine) Buildings(ctx context.Context, r *Resolved, tr *Trace) (*BuildingResult, error) {
	res := &BuildingResult{Count: map[string]int64{}, Area: map[string]float64{}, UnassignedByVillage: map[string]int64{},
		Stats: BuildingStats{Stages: map[string]time.Duration{}}}
	if !e.Manifest.HasBuildings() || len(r.Villages) == 0 {
		return res, nil
	}
	useCache := e.cache != nil && (tr == nil || !tr.NoCache)
	vals := make(map[string]buildingVal, len(r.Villages))
	missing := map[string]bool{}
	var hits, misses int64
	for _, v := range r.Villages {
		if useCache {
			if x, ok := e.cache.buildings.Get(v.ID); ok {
				vals[v.ID] = x
				hits++
				continue
			}
		}
		misses++
		missing[v.ID] = true
	}
	tr.add(func(c *CacheCounts) { c.ResultHits += hits; c.ResultMisses += misses })
	if len(missing) > 0 {
		sub := r.subset(missing)
		computed, err := e.computeBuildings(ctx, sub, tr, &res.Stats)
		if err != nil {
			return nil, err
		}
		for _, v := range sub.Villages {
			x := computed[v.ID]
			vals[v.ID] = x
			if useCache {
				e.cache.buildings.Add(v.ID, x)
			}
		}
		res.Stats.ComputedFor = len(sub.Villages)
	}
	for id, x := range vals {
		if x.count > 0 {
			res.Count[id], res.Area[id] = x.count, float64(x.area)*areaUnit
		}
		if x.unassigned > 0 {
			res.UnassignedByVillage[id] = x.unassigned
			res.Unassigned += x.unassigned
		}
	}
	return res, nil
}

func (e *Engine) computeBuildings(ctx context.Context, r *Resolved, tr *Trace, st *BuildingStats) (map[string]buildingVal, error) {
	out := map[string]buildingVal{}
	var cellRel, pointRel []string
	for _, p := range r.Provinces {
		cellRel = append(cellRel, release.BuildingCellsPath(e.Manifest.BuildingRelease, p))
		pointRel = append(pointRel, release.BuildingPointsPath(e.Manifest.BuildingRelease, p))
	}
	cellPaths, pointPaths := e.paths(cellRel), e.paths(pointRel)

	c, err := e.db.Conn(ctx)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer c.Close()
	enableProfiling(ctx, c)
	defer func() {
		for _, t := range []string{"qb_sel", "qb_h", "qb_hb"} {
			_, _ = c.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+t)
		}
		disableProfiling(c)
	}()

	var h3Src, vSrc string
	if e.refs != nil && (tr == nil || !tr.NoCache) {
		t := time.Now()
		refs, err := e.acquireRefs(ctx, r.Provinces, tr)
		st.Stages["reference"] += time.Since(t)
		if err != nil {
			return nil, err
		}
		defer refs.release()
		h3Src, vSrc = refs.h3Src(), refs.vSrc()
	} else {
		var h3Rel []string
		for _, p := range r.Provinces {
			h3Rel = append(h3Rel, release.VillageH3Path(p))
		}
		h3Paths := e.paths(h3Rel)
		if len(h3Paths) == 0 {
			return nil, fmt.Errorf("%w: no village_h3 partitions for provinces %v", ErrStorage, r.Provinces)
		}
		h3Src = "read_parquet(" + listLit(h3Paths) + ")"
		vSrc = "read_parquet(" + lit(e.path(release.VillagesPath)) + ")"
	}

	stage := func(name string, f func() error) error {
		t := time.Now()
		err := f()
		st.Stages[name] += time.Since(t)
		if err == nil {
			tr.collect(c, name)
		}
		return classify(ctx, err)
	}
	exec := func(q string) error { _, err := c.ExecContext(ctx, q); return err }

	if err := stage("building_cells", func() error {
		if err := exec("CREATE TEMP TABLE qb_sel AS SELECT unnest(" + listLit(r.IDs()) + ") AS village_id"); err != nil {
			return err
		}
		if err := exec(fmt.Sprintf(`CREATE TEMP TABLE qb_h AS
			SELECT h3_cell, village_id, cell_type FROM %s
			WHERE village_id IN (SELECT village_id FROM qb_sel)`, h3Src)); err != nil {
			return err
		}
		if err := exec(fmt.Sprintf(`CREATE TEMP TABLE qb_hb AS
			SELECT h3_cell, village_id FROM %s
			WHERE cell_type = 'boundary'
			  AND h3_cell IN (SELECT h3_cell FROM qb_h WHERE cell_type = 'boundary')`, h3Src)); err != nil {
			return err
		}
		if len(cellPaths) == 0 {
			return nil
		}
		// Interior path: pre-summed cells, no geometry at all.
		rows, err := c.QueryContext(ctx, fmt.Sprintf(`SELECT h.village_id, count(*), sum(b.buildings), sum(round(b.area_m2::DOUBLE / %v)::BIGINT)
			FROM read_parquet(%s) b JOIN qb_h h USING (h3_cell)
			WHERE h.cell_type = 'interior' GROUP BY ALL`, areaUnit, listLit(cellPaths)))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			var cells, n int64
			var area int64
			if err := rows.Scan(&v, &cells, &n, &area); err != nil {
				return err
			}
			x := out[v]
			x.count += n
			x.area += area
			out[v] = x
			st.CellRows += cells
			st.CellBuildings += n
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	if len(pointPaths) == 0 {
		return out, nil
	}

	selected := map[string]bool{}
	for _, v := range r.Villages {
		selected[v.ID] = true
	}
	if err := stage("building_boundary", func() error {
		rows, err := c.QueryContext(ctx, fmt.Sprintf(`
			WITH p AS (
				SELECT row_number() OVER () AS pid, h3_cell, area_m2,
					ST_SetCRS(ST_Point(x_e7 / 1e7, y_e7 / 1e7), 'EPSG:4326') AS geometry
				FROM read_parquet(%s)
				WHERE h3_cell IN (SELECT h3_cell FROM qb_hb)),
			vg AS (
				SELECT village_id, geometry FROM %s
				WHERE village_id IN (SELECT DISTINCT village_id FROM qb_hb)),
			t AS (
				SELECT p.pid, any_value(round(p.area_m2::DOUBLE / %v)::BIGINT) AS area, count(*) AS n_tested,
					min(hb.village_id) FILTER (ST_Covers(vg.geometry, p.geometry)) AS village_id,
					min(hb.village_id) AS first_candidate
				FROM p JOIN qb_hb hb USING (h3_cell) JOIN vg USING (village_id)
				GROUP BY p.pid)
			SELECT village_id, first_candidate, count(*), sum(area), sum(n_tested) FROM t GROUP BY ALL`,
			listLit(pointPaths), vSrc, areaUnit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v sql.NullString
			var first string
			var n, tests int64
			var area int64
			if err := rows.Scan(&v, &first, &n, &area, &tests); err != nil {
				return err
			}
			st.Points += n
			st.PointTests += tests
			switch {
			case v.Valid && selected[v.String]:
				x := out[v.String]
				x.count += n
				x.area += area
				out[v.String] = x
			case !v.Valid && selected[first]:
				x := out[first]
				x.unassigned += n
				out[first] = x
				st.Unassigned += n
			default:
				st.OtherVillage += n
			}
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	return out, nil
}
