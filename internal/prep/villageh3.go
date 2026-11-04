package prep

import (
	"context"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"

	"on-demand-zonal-stats/internal/h3index"
)

type villageGeom struct {
	id   string
	geom orb.Geometry
}

func readVillageGeoms(ctx context.Context, d *db) ([]villageGeom, error) {
	rows, err := d.query(ctx, `SELECT village_id, ST_AsWKB(geometry) FROM villages ORDER BY village_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []villageGeom
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		g, err := wkb.Unmarshal(b)
		if err != nil {
			return nil, fmt.Errorf("village %s: %w", id, err)
		}
		switch g.(type) {
		case orb.Polygon, orb.MultiPolygon:
		default:
			return nil, fmt.Errorf("village %s: unexpected geometry %T", id, g)
		}
		out = append(out, villageGeom{id, g})
	}
	return out, rows.Err()
}

// buildVillageH3 classifies every village's H3 cells (in parallel) and loads
// the merged rows into table village_h3.
func buildVillageH3(ctx context.Context, d *db, cfg *Config, qc *qcLog) error {
	vs, err := readVillageGeoms(ctx, d)
	if err != nil {
		return err
	}
	per := make([][]h3index.Row, len(vs))
	errs := make([]error, len(vs))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				per[i], errs[i] = h3index.ClassifyVillage(vs[i].id, vs[i].geom, h3index.Options{Resolution: cfg.H3Resolution})
			}
		}()
	}
	for i := range vs {
		next <- i
	}
	close(next)
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	merged := h3index.Merge(per...)
	logf("village_h3: %d rows for %d villages", len(merged), len(vs))

	if err := d.exec(ctx, `CREATE TABLE village_h3 (h3_cell UBIGINT, village_id VARCHAR, cell_type VARCHAR)`); err != nil {
		return err
	}
	err = d.conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", "village_h3")
		if err != nil {
			return err
		}
		for _, r := range merged {
			if err := a.AppendRow(uint64(r.Cell), r.VillageID, string(r.Type)); err != nil {
				a.Close()
				return err
			}
		}
		return a.Close()
	})
	if err != nil {
		return fmt.Errorf("village_h3 append: %w", err)
	}

	// QC: every village has a row; every interior cell maps to one village.
	var noRows, badInterior int64
	err = d.queryRow(ctx, `SELECT
		(SELECT count(*) FROM villages v WHERE NOT EXISTS (SELECT 1 FROM village_h3 h WHERE h.village_id = v.village_id)),
		(SELECT count(*) FROM (SELECT h3_cell FROM village_h3 GROUP BY h3_cell
			HAVING count(*) FILTER (cell_type = 'interior') > 0 AND count(*) > 1))`).Scan(&noRows, &badInterior)
	var nInterior, nTotal int64
	if err == nil {
		err = d.queryRow(ctx, `SELECT count(*) FILTER (cell_type = 'interior'), count(*) FROM village_h3`).Scan(&nInterior, &nTotal)
	}
	qc.add(QCResult{Name: "h3_coverage", Blocking: true, Passed: err == nil && noRows == 0 && badInterior == 0,
		Value: map[string]int64{"villages_without_cells": noRows, "interior_cells_shared": badInterior,
			"interior_rows": nInterior, "total_rows": nTotal},
		Error: errStr(err)})
	return nil
}

func provinces(ctx context.Context, d *db) ([]string, error) {
	return d.strings(ctx, `SELECT DISTINCT province_code FROM villages ORDER BY 1`)
}

func writeVillageH3(ctx context.Context, d *db, out string) ([]string, error) {
	ps, err := provinces(ctx, d)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range ps {
		path := filepath.Join(out, "village_h3", "province_code="+p, "part-0.parquet")
		if err := mkdirFor(path); err != nil {
			return nil, err
		}
		if err := d.exec(ctx, fmt.Sprintf(`COPY (
			SELECT h.h3_cell, h.village_id, h.cell_type
			FROM village_h3 h JOIN villages v USING (village_id)
			WHERE v.province_code = %s
			ORDER BY h.h3_cell, h.village_id
		) TO %s (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 100000)`, lit(p), lit(path))); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}
