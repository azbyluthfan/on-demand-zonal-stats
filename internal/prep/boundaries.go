package prep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Columns of villages.parquet, in order.
const villageCols = `village_id, village_name, district_code, district_name,
	regency_code, regency_name, province_code, province_name,
	boundary_version, area_m2, bbox, geometry`

// convertHDX extracts the admin-4 layer to FlatGeobuf with ogr2ogr. DuckDB
// spatial 1.5.5's ST_Read sees only OGC_FID + geometry in the HDX FileGDB
// (attribute columns are missing), while GDAL 3.10 reads it correctly.
func convertHDX(ctx context.Context, cfg *Config, workDir string) (string, error) {
	dst := filepath.Join(workDir, "hdx_admin4.fgb")
	args := []string{"-f", "FlatGeobuf", "-overwrite"}
	if len(cfg.ProvinceCodes) > 0 {
		pc := make([]string, len(cfg.ProvinceCodes))
		for i, c := range cfg.ProvinceCodes {
			pc[i] = "ID" + c
		}
		args = append(args, "-where", "adm1_pcode IN ("+litList(pc)+")")
	}
	args = append(args, dst, cfg.Boundaries.Path, cfg.Boundaries.Layer)
	logf("boundaries: ogr2ogr %v", args)
	if _, err := runGDAL(ctx, "ogr2ogr", args...); err != nil {
		return "", err
	}
	return dst, nil
}

func boundaryAdapterSQL(src BoundarySource) (string, error) {
	switch src.Adapter {
	case "geojson":
		return fmt.Sprintf(`SELECT village_id::VARCHAR AS village_id, village_name::VARCHAR AS village_name,
			district_code::VARCHAR AS district_code, district_name::VARCHAR AS district_name,
			regency_code::VARCHAR AS regency_code, regency_name::VARCHAR AS regency_name,
			province_code::VARCHAR AS province_code, province_name::VARCHAR AS province_name,
			geom FROM ST_Read(%s)`, lit(src.Path)), nil
	case "hdx_cod_ab":
		return hdxAdapterSQL(src)
	}
	return "", fmt.Errorf("unknown boundary adapter %q", src.Adapter)
}

// hdxAdapterSQL maps HDX COD-AB Indonesia admin 4. P-codes look like
// "ID3404010001"; village_id etc. drop the "ID" prefix to get BPS codes,
// and alternate names after "/" or "\" are kept only in *_name_full.
func hdxAdapterSQL(src BoundarySource) (string, error) {
	// src.Path here is the FlatGeobuf produced by convertHDX.
	strip := func(col string) string { return fmt.Sprintf("regexp_replace(%s::VARCHAR, '^ID', '')", col) }
	// Alternate names follow "/" or "\": keep the first as the name.
	name := func(col string) string {
		return fmt.Sprintf(`trim(regexp_extract(%s::VARCHAR, '^[^/\\]*'))`, col)
	}
	return fmt.Sprintf(`SELECT %s AS village_id, %s AS village_name,
			%s AS district_code, %s AS district_name,
			%s AS regency_code, %s AS regency_name,
			%s AS province_code, %s AS province_name,
			adm4_pcode::VARCHAR AS source_pcode, adm4_name::VARCHAR AS source_name,
			geom FROM ST_Read(%s)`,
		strip("adm4_pcode"), name("adm4_name"), strip("adm3_pcode"), name("adm3_name"),
		strip("adm2_pcode"), name("adm2_name"), strip("adm1_pcode"), name("adm1_name"),
		lit(src.Path)), nil
}

// loadBoundaries creates table villages and runs boundary QC.
func loadBoundaries(ctx context.Context, d *db, cfg *Config, qc *qcLog) error {
	src := cfg.Boundaries
	if src.Adapter == "hdx_cod_ab" {
		if src.Layer == "" {
			return fmt.Errorf("hdx_cod_ab adapter needs boundaries.layer (e.g. idn_admin4)")
		}
		work, err := os.MkdirTemp("", "prep-hdx-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		if src.Path, err = convertHDX(ctx, cfg, work); err != nil {
			return err
		}
	}
	adapter, err := boundaryAdapterSQL(src)
	if err != nil {
		return err
	}
	filter := ""
	if len(cfg.ProvinceCodes) > 0 {
		filter = "WHERE province_code IN (" + litList(cfg.ProvinceCodes) + ")"
	}
	// DuckDB 1.5 geometries carry a CRS and refuse to mix them (HDX FGB is
	// EPSG:4326, Overture is OGC:CRS84). All inputs are WGS84 lon/lat, so
	// label everything EPSG:4326 without transforming.
	if err := d.exec(ctx, fmt.Sprintf(`CREATE TABLE src_villages AS
		SELECT * REPLACE (ST_SetCRS(geom, 'EPSG:4326') AS geom) FROM (%s) %s`, adapter, filter)); err != nil {
		return err
	}
	nSrc, err := d.count(ctx, "SELECT count(*) FROM src_villages")
	if err != nil {
		return err
	}
	if nSrc == 0 {
		return fmt.Errorf("boundaries: no villages loaded (check path, layer and province_codes)")
	}
	logf("boundaries: %d source rows", nSrc)

	// Repair invalid geometries; keep only polygonal parts.
	if err := d.exec(ctx, fmt.Sprintf(`CREATE TABLE villages AS
		WITH v AS (SELECT *, ST_IsValid(geom) AS valid_in FROM src_villages),
		r AS (
			SELECT * EXCLUDE (geom),
				CASE WHEN valid_in THEN geom ELSE ST_CollectionExtract(ST_MakeValid(geom), 3) END AS geometry
			FROM v)
		SELECT * EXCLUDE (valid_in), NOT valid_in AS repaired, %s AS boundary_version,
			ST_Area(ST_Transform(geometry, 'EPSG:4326', 'EPSG:6933', true)) AS area_m2,
			{'xmin': ST_XMin(geometry), 'ymin': ST_YMin(geometry),
			 'xmax': ST_XMax(geometry), 'ymax': ST_YMax(geometry)} AS bbox
		FROM r`, lit(cfg.Boundaries.BoundaryVersion))); err != nil {
		return err
	}

	// QC: geometry validity (blocking).
	var repaired, invalid, nonPolygonal int64
	err = d.queryRow(ctx, `SELECT count(*) FILTER (repaired),
		count(*) FILTER (geometry IS NULL OR ST_IsEmpty(geometry) OR NOT ST_IsValid(geometry)),
		count(*) FILTER (ST_GeometryType(geometry)::VARCHAR NOT IN ('POLYGON', 'MULTIPOLYGON'))
		FROM villages`).Scan(&repaired, &invalid, &nonPolygonal)
	qc.add(QCResult{Name: "geometry_validity", Blocking: true, Passed: err == nil && invalid == 0 && nonPolygonal == 0,
		Value: map[string]int64{"repaired": repaired, "invalid_after_repair": invalid, "non_polygonal": nonPolygonal},
		Error: errStr(err)})

	// QC: unique IDs (blocking).
	dups, err := d.strings(ctx, `SELECT village_id FROM villages GROUP BY village_id HAVING count(*) > 1 ORDER BY 1 LIMIT 20`)
	qc.add(QCResult{Name: "unique_village_id", Blocking: true, Passed: err == nil && len(dups) == 0,
		Value: len(dups), Detail: dups, Error: errStr(err)})

	// QC: required fields (blocking).
	var nulls int64
	err = d.queryRow(ctx, `SELECT count(*) FROM villages WHERE
		coalesce(village_id, '') = '' OR coalesce(village_name, '') = '' OR
		coalesce(district_code, '') = '' OR coalesce(district_name, '') = '' OR
		coalesce(regency_code, '') = '' OR coalesce(regency_name, '') = '' OR
		coalesce(province_code, '') = '' OR coalesce(province_name, '') = ''`).Scan(&nulls)
	qc.add(QCResult{Name: "required_fields", Blocking: true, Passed: err == nil && nulls == 0, Value: nulls, Error: errStr(err)})

	return boundaryOverlapQC(ctx, d, cfg, qc)
}

func boundaryOverlapQC(ctx context.Context, d *db, cfg *Config, qc *qcLog) error {
	// Planar degree² areas: only the ratio matters.
	if err := d.exec(ctx, `CREATE TABLE village_overlaps AS
		SELECT a.village_id AS a_id, b.village_id AS b_id,
			ST_Area(ST_Intersection(a.geometry, b.geometry)) AS area
		FROM villages a JOIN villages b
			ON ST_Intersects(a.geometry, b.geometry) AND a.village_id < b.village_id`); err != nil {
		qc.add(QCResult{Name: "overlaps", Blocking: true, Error: err.Error()})
		return nil
	}
	var total, overlap float64
	err := d.queryRow(ctx, `SELECT (SELECT sum(ST_Area(geometry)) FROM villages),
		(SELECT coalesce(sum(area), 0) FROM village_overlaps)`).Scan(&total, &overlap)
	frac := 0.0
	if total > 0 {
		frac = overlap / total
	}
	type offender struct {
		A, B string
		Frac float64 `json:"area_fraction_of_total"`
	}
	var top []offender
	if err == nil {
		rows, qerr := d.query(ctx, `SELECT a_id, b_id, area FROM village_overlaps WHERE area > 0 ORDER BY area DESC LIMIT 10`)
		if qerr == nil {
			for rows.Next() {
				var o offender
				var a float64
				_ = rows.Scan(&o.A, &o.B, &a)
				o.Frac = a / total
				top = append(top, o)
			}
			rows.Close()
		}
	}
	qc.add(QCResult{Name: "overlaps", Blocking: true, Passed: err == nil && frac < cfg.QC.MaxOverlapFraction,
		Value: map[string]float64{"overlap_fraction": frac, "max": cfg.QC.MaxOverlapFraction}, Detail: top, Error: errStr(err)})

	// Gaps (informational): holes inside the union of all villages.
	var gapArea, unionArea float64
	err = d.queryRow(ctx, `WITH u AS (SELECT ST_Union_Agg(geometry) AS g FROM villages),
		parts AS (SELECT unnest(ST_Dump(g)) AS d FROM u)
		SELECT sum(ST_Area(ST_MakePolygon(ST_ExteriorRing(d.geom)))) - (SELECT ST_Area(g) FROM u),
			(SELECT ST_Area(g) FROM u)
		FROM parts`).Scan(&gapArea, &unionArea)
	gf := 0.0
	if unionArea > 0 {
		gf = gapArea / unionArea
	}
	qc.info("gaps", map[string]float64{"interior_gap_fraction": gf}, "holes inside the union of villages; informational", err)
	return nil
}

func writeVillages(ctx context.Context, d *db, out string) (string, error) {
	path := filepath.Join(out, "boundaries", "villages.parquet")
	if err := mkdirFor(path); err != nil {
		return "", err
	}
	return path, d.exec(ctx, fmt.Sprintf(`COPY (
		SELECT %s FROM villages
		ORDER BY ST_Hilbert(ST_Centroid(geometry), (SELECT ST_Extent(ST_Extent_Agg(geometry)) FROM villages)), village_id
	) TO %s (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 10000)`, villageCols, lit(path)))
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
