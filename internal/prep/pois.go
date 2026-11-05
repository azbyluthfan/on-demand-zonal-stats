package prep

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/paulmach/orb"
	"github.com/uber/h3-go/v4"

	"on-demand-zonal-stats/internal/geom"
	"on-demand-zonal-stats/internal/h3index"
	"on-demand-zonal-stats/internal/release"
)

// NoProvince is the partition for POIs that touch no village at all.
const NoProvince = release.NoProvince

func poiAdapterSQL(ctx context.Context, d *db, src POISource) (string, error) {
	switch src.Adapter {
	case "geojson":
		return fmt.Sprintf(`SELECT poi_id::VARCHAR AS poi_id, name::VARCHAR AS name,
			category::VARCHAR AS category, geom FROM ST_Read(%s)`, lit(src.Path)), nil
	case "overture":
		// Public bucket, anonymous access. Path is e.g.
		// s3://overturemaps-us-west-2/release/2026-10-22.0
		// Explicit empty keys: otherwise the AWS_* env vars set for the cog
		// extension's RustFS access leak into this secret.
		if err := d.exec(ctx, `CREATE OR REPLACE SECRET overture (TYPE s3, PROVIDER config,
			KEY_ID '', SECRET '', REGION 'us-west-2', SCOPE 's3://overturemaps-us-west-2')`); err != nil {
			return "", err
		}
		var xmin, ymin, xmax, ymax float64
		if err := d.queryRow(ctx, `SELECT min(bbox.xmin), min(bbox.ymin), max(bbox.xmax), max(bbox.ymax) FROM villages`).
			Scan(&xmin, &ymin, &xmax, &ymax); err != nil {
			return "", err
		}
		const pad = 0.01
		// Since 2026 releases the primary category is taxonomy.primary (the
		// old categories.primary column is gone); basic_category is coarser.
		return fmt.Sprintf(`SELECT id AS poi_id, names.primary AS name, taxonomy.primary AS category, geometry AS geom
			FROM read_parquet(%s, hive_partitioning = true)
			WHERE bbox.xmin >= %v AND bbox.xmax <= %v AND bbox.ymin >= %v AND bbox.ymax <= %v
				AND operating_status IS DISTINCT FROM 'permanently_closed'`,
			lit(src.Path+"/theme=places/type=place/*"), xmin-pad, xmax+pad, ymin-pad, ymax+pad), nil
	}
	return "", fmt.Errorf("unknown poi adapter %q", src.Adapter)
}

// loadPOIs creates tables pois, poi_assign (exact village per POI via the
// H3 split) and poi_province (partition key), and runs POI QC.
func loadPOIs(ctx context.Context, d *db, cfg *Config, qc *qcLog) error {
	adapter, err := poiAdapterSQL(ctx, d, cfg.POIs)
	if err != nil {
		return err
	}
	if err := d.exec(ctx, "CREATE TABLE src_pois AS "+adapter); err != nil {
		return err
	}
	var nSrc, nDropped int64
	if err := d.queryRow(ctx, `SELECT count(*), count(*) FILTER (geom IS NULL OR ST_GeometryType(geom)::VARCHAR <> 'POINT'
		OR coalesce(category, '') = '' OR poi_id IS NULL) FROM src_pois`).Scan(&nSrc, &nDropped); err != nil {
		return err
	}
	logf("pois: %d source rows, %d dropped (no point geometry, id or category)", nSrc, nDropped)

	if err := d.exec(ctx, fmt.Sprintf(`CREATE TABLE pois AS
		SELECT poi_id, name, category, ST_X(geom) AS lon, ST_Y(geom) AS lat,
			h3_latlng_to_cell(ST_Y(geom), ST_X(geom), %d)::UBIGINT AS h3_cell,
			ST_SetCRS(geom, 'EPSG:4326') AS geometry -- see loadBoundaries
		FROM src_pois
		WHERE geom IS NOT NULL AND ST_GeometryType(geom)::VARCHAR = 'POINT'
			AND coalesce(category, '') <> '' AND poi_id IS NOT NULL`, cfg.H3Resolution)); err != nil {
		return err
	}
	dups, err := d.count(ctx, `SELECT count(*) FROM (SELECT poi_id FROM pois GROUP BY 1 HAVING count(*) > 1)`)
	qc.add(QCResult{Name: "unique_poi_id", Blocking: true, Passed: err == nil && dups == 0, Value: dups, Error: errStr(err)})

	if err := h3ConsistencyQC(ctx, d, cfg, qc); err != nil {
		return err
	}

	if err := assignPoints(ctx, d, "pois", "poi_id", "poi_assign"); err != nil {
		return err
	}
	// Partition key: the assigned village's province; for unassigned POIs in
	// a boundary cell, the lowest candidate province; else NoProvince.
	if err := d.exec(ctx, fmt.Sprintf(`CREATE TABLE poi_province AS
		SELECT a.poi_id, coalesce(va.province_code,
			(SELECT min(v.province_code) FROM pois p JOIN village_h3 h USING (h3_cell) JOIN villages v ON v.village_id = h.village_id
			 WHERE p.poi_id = a.poi_id), %s) AS province_code
		FROM poi_assign a LEFT JOIN villages va ON va.village_id = a.village_id`, lit(NoProvince))); err != nil {
		return err
	}

	var total, interior, boundary, none, tested int64
	err = d.queryRow(ctx, `SELECT count(*), count(*) FILTER (path = 'interior'), count(*) FILTER (path = 'boundary'),
		count(*) FILTER (path = 'none'), sum(n_tested) FROM poi_assign`).Scan(&total, &interior, &boundary, &none, &tested)
	var noPartition int64
	if err == nil {
		noPartition, err = d.count(ctx, `SELECT count(*) FROM poi_province WHERE province_code IS NULL`)
	}
	qc.add(QCResult{Name: "poi_assignment", Blocking: true, Passed: err == nil && noPartition == 0,
		Value: map[string]int64{"pois": total, "interior_path": interior, "boundary_path": boundary,
			"unassigned": none, "exact_tests": tested, "without_partition": noPartition},
		Error: errStr(err)})

	if cfg.QC.CrossCheckPOIs {
		poiCrossCheck(ctx, d, qc)
	}
	return calibrationQC(ctx, d, qc)
}

// assignPoints creates table out (id, village_id, path, n_tested): the exact
// village of every point in table pts (columns <id>, h3_cell, geometry) via
// the H3 split, the serving algorithm (docs/query.md): interior cell -> its
// single village; boundary cell -> ST_Covers against the cell's candidate
// villages, lowest village_id wins; else none.
func assignPoints(ctx context.Context, d *db, pts, id, out string) error {
	return d.exec(ctx, fmt.Sprintf(`CREATE TABLE %[3]s AS
		WITH cand AS (
			SELECT p.%[2]s, h.village_id, h.cell_type FROM %[1]s p JOIN village_h3 h USING (h3_cell)),
		interior AS (SELECT %[2]s, village_id FROM cand WHERE cell_type = 'interior'),
		tested AS (
			SELECT c.%[2]s, count(*) AS n_tested,
				min(c.village_id) FILTER (ST_Covers(v.geometry, p.geometry)) AS village_id
			FROM cand c JOIN %[1]s p USING (%[2]s) JOIN villages v USING (village_id)
			WHERE c.cell_type = 'boundary' GROUP BY c.%[2]s)
		SELECT p.%[2]s, coalesce(i.village_id, t.village_id) AS village_id,
			CASE WHEN i.village_id IS NOT NULL THEN 'interior'
			     WHEN t.village_id IS NOT NULL THEN 'boundary' ELSE 'none' END AS path,
			coalesce(t.n_tested, 0) AS n_tested
		FROM %[1]s p LEFT JOIN interior i USING (%[2]s) LEFT JOIN tested t USING (%[2]s)`, pts, id, out))
}

// poiCrossCheck compares the H3-split assignment against a naive ST_Covers
// join over all villages with the same edge rule.
func poiCrossCheck(ctx context.Context, d *db, qc *qcLog) {
	crossCheck(ctx, d, qc, "poi_assignment_crosscheck", "pois", "poi_id", "poi_assign")
}

func crossCheck(ctx context.Context, d *db, qc *qcLog, name, pts, id, assign string) {
	mismatch, err := d.count(ctx, fmt.Sprintf(`WITH naive AS (
			SELECT p.%[2]s, min(v.village_id) AS village_id
			FROM %[1]s p JOIN villages v ON ST_Covers(v.geometry, p.geometry) GROUP BY p.%[2]s)
		SELECT count(*) FROM %[3]s a LEFT JOIN naive n USING (%[2]s)
		WHERE a.village_id IS DISTINCT FROM n.village_id`, pts, id, assign))
	qc.add(QCResult{Name: name, Blocking: true, Passed: err == nil && mismatch == 0,
		Value: map[string]int64{"mismatches": mismatch}, Error: errStr(err)})
}

// maxEdgeDisagreement is how far (degrees) from a cell edge a point may be
// when DuckDB's h3 and h3-go disagree on its cell: 1e-8° ≈ 1 mm, 10x
// inside h3index.DefaultMargin, so interior/boundary logic stays exact.
const maxEdgeDisagreement = h3index.DefaultMargin / 10

// h3ConsistencyQC checks that DuckDB's h3 extension (POI cells) and h3-go
// (village_h3) agree on point -> cell for up to 100k POIs. The two bundle
// different H3 C versions and can round differently for points lying on a
// cell edge; that is safe when the point is within maxEdgeDisagreement of
// the edge, because interior cells keep h3index.DefaultMargin from villages.
func h3ConsistencyQC(ctx context.Context, d *db, cfg *Config, qc *qcLog) error {
	rows, err := d.query(ctx, `SELECT poi_id, lon, lat, h3_cell FROM pois ORDER BY poi_id LIMIT 100000`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type mismatch struct {
		POI        string  `json:"poi_id"`
		Lon        float64 `json:"lon"`
		Lat        float64 `json:"lat"`
		DuckDB     string  `json:"duckdb_cell"`
		H3Go       string  `json:"h3go_cell"`
		EdgeDistDg float64 `json:"distance_to_edge_deg"`
	}
	var n int64
	var bad []mismatch
	worst := 0.0
	for rows.Next() {
		var id string
		var lon, lat float64
		var cell uint64
		if err := rows.Scan(&id, &lon, &lat, &cell); err != nil {
			return err
		}
		n++
		c, err := h3.LatLngToCell(h3.LatLng{Lat: lat, Lng: lon}, cfg.H3Resolution)
		if err != nil {
			return err
		}
		if uint64(c) == cell {
			continue
		}
		// Distance from the point to the outline of the DuckDB-chosen cell.
		hex, err := h3index.CellPath(h3.Cell(cell))
		if err != nil {
			return err
		}
		p := orb.Point{lon, lat}
		dist := math.Inf(1)
		for i := range hex {
			dist = min(dist, geom.SegmentDistance(p, p, hex[i], hex[(i+1)%len(hex)]))
		}
		worst = max(worst, dist)
		if len(bad) < 20 {
			bad = append(bad, mismatch{id, lon, lat, h3.Cell(cell).String(), c.String(), dist})
		}
	}
	qc.add(QCResult{Name: "h3_go_duckdb_consistency", Blocking: true, Passed: worst <= maxEdgeDisagreement,
		Value: map[string]any{"checked": n, "mismatches": len(bad), "max_edge_distance_deg": worst,
			"allowed_deg": maxEdgeDisagreement}, Detail: bad})
	return rows.Err()
}

// calibrationQC reports the share of POIs on the interior path per village
// size class, used to calibrate the H3 resolution (docs/prep.md).
func calibrationQC(ctx context.Context, d *db, qc *qcLog) error {
	rows, err := d.query(ctx, `SELECT
			CASE WHEN v.area_m2 < 1e6 THEN 'a_<1km2' WHEN v.area_m2 < 5e6 THEN 'b_1-5km2'
			     WHEN v.area_m2 < 20e6 THEN 'c_5-20km2' ELSE 'd_>=20km2' END AS size_class,
			count(DISTINCT v.village_id) AS villages, count(*) AS pois,
			count(*) FILTER (a.path = 'interior') / count(*) AS interior_share
		FROM poi_assign a JOIN villages v USING (village_id)
		GROUP BY 1 ORDER BY 1`)
	if err != nil {
		qc.info("h3_calibration", nil, nil, err)
		return nil
	}
	defer rows.Close()
	type row struct {
		SizeClass     string  `json:"size_class"`
		Villages      int64   `json:"villages"`
		POIs          int64   `json:"pois"`
		InteriorShare float64 `json:"interior_share"`
	}
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.SizeClass, &r.Villages, &r.POIs, &r.InteriorShare); err != nil {
			return err
		}
		out = append(out, r)
	}
	qc.info("h3_calibration", nil, out, rows.Err())
	return nil
}

func writePOIs(ctx context.Context, d *db, cfg *Config, out string) ([]string, error) {
	ps, err := d.strings(ctx, `SELECT DISTINCT province_code FROM poi_province ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range ps {
		path := filepath.Join(out, "pois", "release="+cfg.POIs.Release, "province_code="+p, "part-0.parquet")
		if err := mkdirFor(path); err != nil {
			return nil, err
		}
		if err := d.exec(ctx, fmt.Sprintf(`COPY (
			SELECT p.poi_id, p.name, p.category, p.h3_cell, p.lon, p.lat, p.geometry,
				{'xmin': p.lon, 'ymin': p.lat, 'xmax': p.lon, 'ymax': p.lat} AS bbox
			FROM pois p JOIN poi_province pp USING (poi_id)
			WHERE pp.province_code = %s
			ORDER BY p.h3_cell, p.poi_id
		) TO %s (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 100000)`, lit(p), lit(path))); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

type categoryCount struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}

func writeCategories(ctx context.Context, d *db, out string) (string, error) {
	rows, err := d.query(ctx, `SELECT category, count(*) FROM pois GROUP BY 1 ORDER BY 2 DESC, 1`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cats := []categoryCount{}
	for rows.Next() {
		var c categoryCount
		if err := rows.Scan(&c.Category, &c.Count); err != nil {
			return "", err
		}
		cats = append(cats, c)
	}
	path := filepath.Join(out, "categories.json")
	return path, writeJSON(path, map[string]any{"categories": cats})
}

func writeJSON(path string, v any) error {
	if err := mkdirFor(path); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func mkdirFor(path string) error { return os.MkdirAll(filepath.Dir(path), 0o755) }
