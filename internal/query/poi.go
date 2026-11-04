package query

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"on-demand-zonal-stats/internal/release"
)

// POIMetric counts POIs whose category is in Categories.
type POIMetric struct {
	ID         string
	Categories []string
}

// POIStats explains where work went (the diagnostics block). Counts
// cover the part computed for this request (cache misses), not cache hits.
type POIStats struct {
	Scanned        int64 // POIs read (category + cell filtered)
	Interior       int64 // assigned on the interior path, no geometry test
	BoundaryPOIs   int64 // POIs in boundary cells
	BoundaryTests  int64 // exact ST_Covers tests performed
	Unassigned     int64 // POIs in a selected village's cells covered by no village
	OtherVillage   int64 // boundary POIs won by (or attributed to) a village outside the area
	Stages         map[string]time.Duration
	Partitions     []string
	SelectedCells  int64
	CandidateCells int64
	ComputedFor    int // villages computed (cache misses)
}

// POIResult holds counts per metric per village (villages with 0 omitted).
type POIResult struct {
	Counts map[string]map[string]int64 // metric ID -> village ID -> count
	// Unassigned is, per metric, the number of POIs inside the area's cells
	// that no village covers. Each such POI is attributed to the lowest-ID
	// candidate village of its H3 cell (the same rule prep uses for
	// partitioning), so the figure is per-village cacheable and additive
	// across areas.
	Unassigned          map[string]int64
	UnassignedByVillage map[string]map[string]int64
	Stats               POIStats
}

type poiKey struct{ village, category string }
type poiVal struct{ count, unassigned int64 }

// CountPOIs counts POIs per village for each metric (docs/query.md), serving
// (village, category) results from the result cache where possible and
// computing the rest. tr may be nil.
func (e *Engine) CountPOIs(ctx context.Context, r *Resolved, metrics []POIMetric, tr *Trace) (*POIResult, error) {
	res := &POIResult{Counts: map[string]map[string]int64{}, Unassigned: map[string]int64{},
		UnassignedByVillage: map[string]map[string]int64{}}
	res.Stats.Stages = map[string]time.Duration{}
	for _, m := range metrics {
		res.Counts[m.ID], res.UnassignedByVillage[m.ID] = map[string]int64{}, map[string]int64{}
		res.Unassigned[m.ID] = 0
	}
	if len(r.Villages) == 0 || len(metrics) == 0 {
		return res, nil
	}
	cats := categoryUnion(metrics)
	useCache := e.cache != nil && (tr == nil || !tr.NoCache)

	// Look up every (village, category); collect what is missing.
	vals := make(map[poiKey]poiVal, len(r.Villages)*len(cats))
	missingV := map[string]bool{}
	missingC := map[string]bool{}
	var hits, misses int64
	for _, v := range r.Villages {
		for _, c := range cats {
			k := poiKey{v.ID, c}
			if useCache {
				if x, ok := e.cache.poi.Get(k); ok {
					vals[k] = x
					hits++
					continue
				}
			}
			misses++
			missingV[v.ID], missingC[c] = true, true
		}
	}
	tr.add(func(c *CacheCounts) { c.ResultHits += hits; c.ResultMisses += misses })

	if len(missingV) > 0 {
		sub := r.subset(missingV)
		mc := make([]string, 0, len(missingC))
		for c := range missingC {
			mc = append(mc, c)
		}
		sort.Strings(mc)
		counts, unassigned, err := e.computePOIs(ctx, sub, mc, tr, &res.Stats)
		if err != nil {
			return nil, err
		}
		for _, v := range sub.Villages {
			for _, c := range mc {
				k := poiKey{v.ID, c}
				x := poiVal{counts[k], unassigned[k]}
				vals[k] = x
				if useCache {
					e.cache.poi.Add(k, x)
				}
			}
		}
		res.Stats.ComputedFor = len(sub.Villages)
	}

	for _, m := range metrics {
		for _, v := range r.Villages {
			var n, u int64
			for _, c := range m.Categories {
				x := vals[poiKey{v.ID, c}]
				n += x.count
				u += x.unassigned
			}
			if n > 0 {
				res.Counts[m.ID][v.ID] = n
			}
			if u > 0 {
				res.UnassignedByVillage[m.ID][v.ID] = u
				res.Unassigned[m.ID] += u
			}
		}
	}
	return res, nil
}

func categoryUnion(metrics []POIMetric) []string {
	set := map[string]bool{}
	for _, m := range metrics {
		for _, c := range m.Categories {
			set[c] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// subset returns the villages in keep, with their provinces.
func (r *Resolved) subset(keep map[string]bool) *Resolved {
	out := &Resolved{}
	provs := map[string]bool{}
	for _, v := range r.Villages {
		if keep[v.ID] {
			out.Villages = append(out.Villages, v)
			provs[v.ProvinceCode] = true
		}
	}
	for p := range provs {
		out.Provinces = append(out.Provinces, p)
	}
	sort.Strings(out.Provinces)
	return out
}

// computePOIs runs the H3-split assignment for the villages and categories:
//
//  1. q_h: the villages' village_h3 rows; q_hb: every village's rows for
//     the selected boundary cells (candidates, selected or not).
//  2. q_p: POIs of the needed province partitions, filtered by category and
//     by the selected cells. One scan serves all categories.
//  3. Interior path: join on h3_cell where cell_type = 'interior'.
//  4. Boundary path: ST_Covers against every candidate; lowest village_id
//     among covering villages wins (edge rule), so results do not depend on
//     the requested area. Uncovered POIs are attributed to the lowest
//     candidate (unassigned).
//
// POI partitions are keyed by the province prep assigned each POI to
// (docs/prep.md), so the selected provinces' partitions hold every POI
// that can belong, or be attributed, to a selected village.
func (e *Engine) computePOIs(ctx context.Context, r *Resolved, cats []string, tr *Trace, st *POIStats) (
	counts, unassigned map[poiKey]int64, err error) {
	counts, unassigned = map[poiKey]int64{}, map[poiKey]int64{}
	var poiRel []string
	for _, p := range r.Provinces {
		poiRel = append(poiRel, release.POIPath(e.Manifest.POIRelease, p))
	}
	poiPaths := e.paths(poiRel)
	st.Partitions = r.Provinces

	c, err := e.db.Conn(ctx)
	if err != nil {
		return nil, nil, classify(ctx, err)
	}
	defer c.Close()
	enableProfiling(ctx, c)
	defer func() {
		// Temp tables live on this pooled connection; drop them even if ctx is done.
		for _, t := range []string{"q_sel", "q_h", "q_hb", "q_p"} {
			_, _ = c.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+t)
		}
		disableProfiling(c)
	}()

	// Sources: reference-cache tables, or the release files when bypassing.
	var h3Src, vSrc string
	if e.refs != nil && (tr == nil || !tr.NoCache) {
		t := time.Now()
		refs, err := e.acquireRefs(ctx, r.Provinces, tr)
		st.Stages["reference"] += time.Since(t)
		if err != nil {
			return nil, nil, err
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
			return nil, nil, fmt.Errorf("%w: no village_h3 partitions for provinces %v", ErrStorage, r.Provinces)
		}
		h3Src = "read_parquet(" + listLit(h3Paths) + ")"
		vSrc = "read_parquet(" + lit(e.path(release.VillagesPath)) + ")"
	}

	stage := func(name string, f func() error) error {
		t := time.Now()
		err := f()
		st.Stages[name] += time.Since(t)
		return classify(ctx, err)
	}
	exec := func(stageName, q string) error {
		if _, err := c.ExecContext(ctx, q); err != nil {
			return err
		}
		tr.collect(c, stageName)
		return nil
	}

	if err := stage("h3_cells", func() error {
		if err := exec("h3_cells", "CREATE TEMP TABLE q_sel AS SELECT unnest("+listLit(r.IDs())+") AS village_id"); err != nil {
			return err
		}
		if err := exec("h3_cells", fmt.Sprintf(`CREATE TEMP TABLE q_h AS
			SELECT h3_cell, village_id, cell_type FROM %s
			WHERE village_id IN (SELECT village_id FROM q_sel)`, h3Src)); err != nil {
			return err
		}
		if err := exec("h3_cells", fmt.Sprintf(`CREATE TEMP TABLE q_hb AS
			SELECT h3_cell, village_id FROM %s
			WHERE cell_type = 'boundary'
			  AND h3_cell IN (SELECT h3_cell FROM q_h WHERE cell_type = 'boundary')`, h3Src)); err != nil {
			return err
		}
		var sel, cand int64
		err := c.QueryRowContext(ctx, `SELECT (SELECT count(DISTINCT h3_cell) FROM q_h), (SELECT count(*) FROM q_hb)`).Scan(&sel, &cand)
		st.SelectedCells += sel
		st.CandidateCells += cand
		return err
	}); err != nil {
		return nil, nil, err
	}
	if len(poiPaths) == 0 { // no POIs at all in these provinces
		return counts, unassigned, nil
	}

	if err := stage("poi_scan", func() error {
		if err := exec("poi_scan", fmt.Sprintf(`CREATE TEMP TABLE q_p AS
			SELECT poi_id, category, h3_cell, geometry FROM read_parquet(%s)
			WHERE category IN (SELECT unnest(%s))
			  AND h3_cell IN (SELECT h3_cell FROM q_h)`, listLit(poiPaths), listLit(cats))); err != nil {
			return err
		}
		var n int64
		err := c.QueryRowContext(ctx, "SELECT count(*) FROM q_p").Scan(&n)
		st.Scanned += n
		return err
	}); err != nil {
		return nil, nil, err
	}

	if err := stage("poi_interior", func() error {
		rows, err := c.QueryContext(ctx, `SELECT h.village_id, p.category, count(*)
			FROM q_p p JOIN q_h h USING (h3_cell)
			WHERE h.cell_type = 'interior' GROUP BY ALL`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v, cat string
			var n int64
			if err := rows.Scan(&v, &cat, &n); err != nil {
				return err
			}
			counts[poiKey{v, cat}] += n
			st.Interior += n
		}
		if err := rows.Err(); err != nil {
			return err
		}
		tr.collect(c, "poi_interior")
		return nil
	}); err != nil {
		return nil, nil, err
	}

	selected := map[string]bool{}
	for _, v := range r.Villages {
		selected[v.ID] = true
	}
	if err := stage("poi_boundary", func() error {
		rows, err := c.QueryContext(ctx, fmt.Sprintf(`
			WITH vg AS (
				SELECT village_id, geometry FROM %s
				WHERE village_id IN (SELECT DISTINCT village_id FROM q_hb)),
			t AS (
				SELECT p.poi_id, p.category, count(*) AS n_tested,
					min(hb.village_id) FILTER (ST_Covers(vg.geometry, p.geometry)) AS village_id,
					min(hb.village_id) AS first_candidate
				FROM q_p p JOIN q_hb hb USING (h3_cell) JOIN vg USING (village_id)
				GROUP BY p.poi_id, p.category)
			SELECT village_id, first_candidate, category, count(*), sum(n_tested) FROM t GROUP BY ALL`, vSrc))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v sql.NullString
			var first, cat string
			var n, tests int64
			if err := rows.Scan(&v, &first, &cat, &n, &tests); err != nil {
				return err
			}
			st.BoundaryPOIs += n
			st.BoundaryTests += tests
			switch {
			case v.Valid && selected[v.String]:
				counts[poiKey{v.String, cat}] += n
			case !v.Valid && selected[first]:
				unassigned[poiKey{first, cat}] += n
				st.Unassigned += n
			default:
				st.OtherVillage += n // won by, or attributed to, a village outside the area
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		tr.collect(c, "poi_boundary")
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return counts, unassigned, nil
}

// SortedCategories returns the release's categories, most frequent first.
func (e *Engine) SortedCategories() []string {
	out := make([]string, 0, len(e.Categories))
	for c := range e.Categories {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if e.Categories[out[i]] != e.Categories[out[j]] {
			return e.Categories[out[i]] > e.Categories[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// HasCategory reports whether the release lists the category.
func (e *Engine) HasCategory(c string) bool { _, ok := e.Categories[strings.TrimSpace(c)]; return ok }
