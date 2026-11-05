package prep

import (
	"context"
	"fmt"
	"path/filepath"

	"on-demand-zonal-stats/internal/release"
)

// Buildings (docs/buildings.md). Each footprint becomes one point (its
// point on surface, so always inside the footprint) plus its footprint area.
// The point is rounded to 1e-7° (~1 cm) and stored as int32 x_e7/y_e7; prep
// assigns the rounded point, so serving reproduces prep exactly.
//
// Storage follows the H3 split: buildings in interior cells are pre-summed
// per cell (cells.parquet), which is exact because an interior cell lies
// wholly inside one village; only buildings in boundary cells are kept as
// points (points.parquet) for the exact ST_Covers test. Buildings in no
// village's cell can never count towards a village and are dropped
// (reported by QC).

func buildingAdapterSQL(ctx context.Context, d *db, src BuildingSource) (string, error) {
	switch src.Adapter {
	case "geojson":
		return fmt.Sprintf(`SELECT building_id::VARCHAR AS building_id, geom FROM ST_Read(%s)`, lit(src.Path)), nil
	case "overture":
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
		// Underground structures are not buildings on the ground.
		return fmt.Sprintf(`SELECT id AS building_id, geometry AS geom
			FROM read_parquet(%s, hive_partitioning = true)
			WHERE bbox.xmin >= %v AND bbox.xmax <= %v AND bbox.ymin >= %v AND bbox.ymax <= %v
				AND is_underground IS NOT TRUE`,
			lit(src.Path+"/theme=buildings/type=building/*"), xmin-pad, xmax+pad, ymin-pad, ymax+pad), nil
	}
	return "", fmt.Errorf("unknown buildings adapter %q", src.Adapter)
}

// loadBuildings creates tables buildings, building_assign and
// building_province, and runs building QC.
func loadBuildings(ctx context.Context, d *db, cfg *Config, qc *qcLog) error {
	src := cfg.Buildings
	adapter, err := buildingAdapterSQL(ctx, d, *src)
	if err != nil {
		return err
	}
	if err := d.exec(ctx, "CREATE TABLE src_buildings AS "+adapter); err != nil {
		return err
	}
	// Polygon footprints only; repair invalid ones before area/point.
	if err := d.exec(ctx, fmt.Sprintf(`CREATE TABLE buildings AS
		WITH g AS (
			SELECT building_id, ST_SetCRS(geom, 'EPSG:4326') AS geom FROM src_buildings
			WHERE building_id IS NOT NULL AND geom IS NOT NULL
				AND ST_GeometryType(geom)::VARCHAR IN ('POLYGON', 'MULTIPOLYGON')),
		v AS (SELECT building_id, CASE WHEN ST_IsValid(geom) THEN geom ELSE ST_MakeValid(geom) END AS geom FROM g),
		p AS (
			SELECT building_id,
				ST_Area(ST_Transform(geom, 'EPSG:4326', 'EPSG:6933', true)) AS area_m2,
				ST_PointOnSurface(geom) AS pos
			FROM v WHERE NOT ST_IsEmpty(geom)),
		r AS (
			SELECT building_id, area_m2,
				round(ST_X(pos) * 1e7)::INTEGER AS x_e7, round(ST_Y(pos) * 1e7)::INTEGER AS y_e7
			FROM p WHERE pos IS NOT NULL AND NOT ST_IsEmpty(pos))
		SELECT building_id, area_m2, x_e7, y_e7,
			h3_latlng_to_cell(y_e7 / 1e7, x_e7 / 1e7, %d)::UBIGINT AS h3_cell,
			ST_SetCRS(ST_Point(x_e7 / 1e7, y_e7 / 1e7), 'EPSG:4326') AS geometry
		FROM r`, cfg.H3Resolution)); err != nil {
		return err
	}
	var nSrc, n int64
	if err := d.queryRow(ctx, `SELECT (SELECT count(*) FROM src_buildings), (SELECT count(*) FROM buildings)`).Scan(&nSrc, &n); err != nil {
		return err
	}
	logf("buildings: %d source rows, %d kept (polygon footprints)", nSrc, n)
	// Footprints are no longer needed (points and areas are derived); free
	// them before the assignment joins. 3.6 M DIY polygons are ~1 GB.
	if err := d.exec(ctx, "DROP TABLE src_buildings"); err != nil {
		return err
	}
	dups, err := d.count(ctx, `SELECT count(*) FROM (SELECT building_id FROM buildings GROUP BY 1 HAVING count(*) > 1)`)
	qc.add(QCResult{Name: "unique_building_id", Blocking: true, Passed: err == nil && dups == 0, Value: dups, Error: errStr(err)})

	if err := assignPoints(ctx, d, "buildings", "building_id", "building_assign"); err != nil {
		return err
	}
	// Storage class and partition key per building:
	//   cell:    interior cell, pre-summed (province of the cell's village)
	//   point:   boundary cell (assigned village's province, else lowest candidate's)
	//   outside: in no village's cell, dropped
	if err := d.exec(ctx, `CREATE TABLE building_province AS
		WITH cand AS (
			SELECT b.building_id, min(v.province_code) AS province_code
			FROM buildings b JOIN village_h3 h USING (h3_cell) JOIN villages v ON v.village_id = h.village_id
			GROUP BY b.building_id)
		SELECT a.building_id,
			CASE WHEN a.path = 'interior' THEN 'cell' WHEN c.province_code IS NULL THEN 'outside' ELSE 'point' END AS storage,
			coalesce(va.province_code, c.province_code) AS province_code
		FROM building_assign a LEFT JOIN villages va ON va.village_id = a.village_id
		LEFT JOIN cand c USING (building_id)`); err != nil {
		return err
	}

	var total, interior, boundary, none, tested, cell, point, outside int64
	err = d.queryRow(ctx, `SELECT count(*), count(*) FILTER (path = 'interior'), count(*) FILTER (path = 'boundary'),
			count(*) FILTER (path = 'none'), coalesce(sum(n_tested), 0),
			count(*) FILTER (storage = 'cell'), count(*) FILTER (storage = 'point'), count(*) FILTER (storage = 'outside')
		FROM building_assign JOIN building_province USING (building_id)`).
		Scan(&total, &interior, &boundary, &none, &tested, &cell, &point, &outside)
	qc.add(QCResult{Name: "building_assignment", Blocking: true,
		Passed: err == nil && total == n && cell+point+outside == total,
		Value: map[string]int64{"buildings": total, "interior_path": interior, "boundary_path": boundary,
			"unassigned": none, "exact_tests": tested, "stored_in_cells": cell, "stored_as_points": point,
			"outside_all_villages_dropped": outside},
		Error: errStr(err)})
	if cfg.QC.CrossCheckPOIs {
		crossCheck(ctx, d, qc, "building_assignment_crosscheck", "buildings", "building_id", "building_assign")
	}
	return nil
}

// writeBuildings writes cells.parquet and points.parquet per province.
func writeBuildings(ctx context.Context, d *db, cfg *Config, out string) error {
	ps, err := d.strings(ctx, `SELECT DISTINCT province_code FROM building_province WHERE storage <> 'outside' ORDER BY 1`)
	if err != nil {
		return err
	}
	for _, p := range ps {
		cells := filepath.Join(out, filepath.FromSlash(release.BuildingCellsPath(cfg.Buildings.Release, p)))
		points := filepath.Join(out, filepath.FromSlash(release.BuildingPointsPath(cfg.Buildings.Release, p)))
		if err := mkdirFor(cells); err != nil {
			return err
		}
		if err := d.exec(ctx, fmt.Sprintf(`COPY (
			SELECT b.h3_cell, count(*)::INTEGER AS buildings, sum(b.area_m2) AS area_m2
			FROM buildings b JOIN building_province bp USING (building_id)
			WHERE bp.storage = 'cell' AND bp.province_code = %s
			GROUP BY b.h3_cell ORDER BY b.h3_cell
		) TO %s (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 100000)`, lit(p), lit(cells))); err != nil {
			return err
		}
		if err := d.exec(ctx, fmt.Sprintf(`COPY (
			SELECT b.h3_cell, b.x_e7, b.y_e7, b.area_m2::FLOAT AS area_m2
			FROM buildings b JOIN building_province bp USING (building_id)
			WHERE bp.storage = 'point' AND bp.province_code = %s
			ORDER BY b.h3_cell, b.x_e7, b.y_e7
		) TO %s (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 100000)`, lit(p), lit(points))); err != nil {
			return err
		}
	}
	return nil
}
