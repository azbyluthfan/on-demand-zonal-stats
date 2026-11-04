// Command spike answers the verification questions in docs/spikes.md:
//
//  1. Do the DuckDB extensions (spatial, httpfs, h3, cog) load from duckdb-go
//     inside Docker, pre-installed at image build time (no runtime download)?
//  2. Does httpfs do range reads against RustFS with path-style URLs?
//  3. What does the cog extension expose — can it return pixel windows?
//  4. What does DuckDB profiling/logging expose (operator timings, HTTP bytes)?
//
// It prints a JSON report to stdout. It is throwaway code: findings go into
// docs/spikes.md and the real code lives in internal/.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms"`
	Detail any    `json:"detail,omitempty"`
	Err    string `json:"error,omitempty"`
}

type report struct {
	DuckDBVersion string  `json:"duckdb_version"`
	Checks        []check `json:"checks"`
}

var extensions = []struct{ name, repo string }{
	{"spatial", ""},
	{"httpfs", ""},
	{"h3", "community"},
	{"cog", "community"},
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func main() {
	installOnly := flag.Bool("install-only", false, "install extensions into DUCKDB_EXT_DIR and exit (used at image build)")
	flag.Parse()

	db, err := sql.Open("duckdb", "")
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // keep all SET/LOAD state on one connection
	ctx := context.Background()

	extDir := env("DUCKDB_EXT_DIR", "")
	if extDir != "" {
		mustExec(ctx, db, "SET extension_directory = "+q(extDir))
	}

	if *installOnly {
		for _, e := range extensions {
			stmt := "INSTALL " + e.name
			if e.repo != "" {
				stmt += " FROM " + e.repo
			}
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				// Do not fail the build: a missing community extension is a finding.
				fmt.Fprintf(os.Stderr, "install %s: %v\n", e.name, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "installed %s\n", e.name)
		}
		return
	}

	r := &report{}
	_ = db.QueryRowContext(ctx, "SELECT version()").Scan(&r.DuckDBVersion)

	// Serving must never download extensions at runtime.
	mustExec(ctx, db, "SET autoinstall_known_extensions = false")
	mustExec(ctx, db, "SET autoload_known_extensions = false")
	mustExec(ctx, db, "SET memory_limit = "+q(env("DUCKDB_MEMORY_LIMIT", "2GB")))
	mustExec(ctx, db, "SET threads = "+env("DUCKDB_THREADS", "4"))

	before := functionSet(ctx, db)
	loaded := map[string]bool{}
	for _, e := range extensions {
		r.run("load_"+e.name, func() (any, error) {
			if _, err := db.ExecContext(ctx, "LOAD "+e.name); err != nil {
				return nil, err
			}
			loaded[e.name] = true
			var ver sql.NullString
			_ = db.QueryRowContext(ctx,
				"SELECT extension_version FROM duckdb_extensions() WHERE extension_name = ?", e.name).Scan(&ver)
			return map[string]string{"version": ver.String}, nil
		})
		// Attribute newly appeared functions to this extension.
		after := functionSet(ctx, db)
		if e.name == "cog" || e.name == "h3" {
			r.Checks = append(r.Checks, check{Name: e.name + "_functions", OK: true, Detail: diff(before, after, 200)})
		}
		before = after
	}

	bucket := env("S3_BUCKET", "villagestat")
	base := "s3://" + bucket + "/spike"

	r.run("s3_secret", func() (any, error) {
		_, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE OR REPLACE SECRET rustfs (
			TYPE s3, KEY_ID %s, SECRET %s, ENDPOINT %s, REGION %s,
			URL_STYLE 'path', USE_SSL %s)`,
			q(env("S3_ACCESS_KEY", "rustfsadmin")), q(env("S3_SECRET_KEY", "rustfsadmin")),
			q(env("S3_ENDPOINT", "localhost:9000")), q(env("S3_REGION", "us-east-1")),
			env("S3_USE_SSL", "false")))
		return nil, err
	})

	r.run("s3_write_parquet", func() (any, error) {
		_, err := db.ExecContext(ctx, `COPY (
			SELECT i AS id, (i % 1000)::INTEGER AS v,
			       ST_Point(110 + (i % 1000) / 1000.0, -7 - (i // 1000) / 1000.0) AS geometry
			FROM range(1000000) t(i))
		TO `+q(base+"/range.parquet")+` (FORMAT parquet, ROW_GROUP_SIZE 100000)`)
		return nil, err
	})

	r.run("s3_full_scan", func() (any, error) {
		var n, s int64
		err := db.QueryRowContext(ctx, "SELECT count(*), sum(v) FROM read_parquet("+q(base+"/range.parquet")+")").Scan(&n, &s)
		return map[string]int64{"rows": n, "sum": s}, err
	})

	// Profiling + HTTP logging on a selective query: should touch one row group.
	selective := "SELECT count(*), sum(v) FROM read_parquet(" + q(base+"/range.parquet") + ") WHERE id BETWEEN 500000 AND 500100"
	r.run("profiling_json", func() (any, error) { return profile(ctx, db, selective) })
	r.run("http_logging", func() (any, error) { return httpLogs(ctx, db, selective) })

	r.run("spatial_covers", func() (any, error) {
		var n int64
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM read_parquet(`+q(base+"/range.parquet")+`)
			WHERE ST_Covers(ST_GeomFromText('POLYGON((110 -7, 110.1 -7, 110.1 -7.1, 110 -7.1, 110 -7))'), geometry)`).Scan(&n)
		return map[string]int64{"covered": n}, err
	})

	if loaded["h3"] {
		r.run("h3_cells", func() (any, error) {
			var cell uint64
			var nCells int64
			if err := db.QueryRowContext(ctx, "SELECT h3_latlng_to_cell(-7.8, 110.36, 9)::UBIGINT").Scan(&cell); err != nil {
				return nil, err
			}
			err := db.QueryRowContext(ctx,
				"SELECT len(h3_polygon_wkt_to_cells('POLYGON((110 -7, 110.1 -7, 110.1 -7.1, 110 -7.1, 110 -7))', 9))").Scan(&nCells)
			return map[string]any{"cell_res9": cell, "cells_in_0.1deg_square": nCells}, err
		})
	}

	if loaded["cog"] {
		r.run("cog_probe", func() (any, error) { return probeCOG(ctx, db, "s3://"+bucket+"/spike/fixture_cog.tif") })
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

func (r *report) run(name string, f func() (any, error)) {
	t := time.Now()
	d, err := f()
	c := check{Name: name, OK: err == nil, Ms: time.Since(t).Milliseconds(), Detail: d}
	if err != nil {
		c.Err = err.Error()
	}
	r.Checks = append(r.Checks, c)
	fmt.Fprintf(os.Stderr, "%-20s ok=%v %dms %s\n", name, c.OK, c.Ms, c.Err)
}

func profile(ctx context.Context, db *sql.DB, query string) (any, error) {
	out := "/tmp/spike_profile.json"
	for _, s := range []string{
		"SET enable_profiling = 'json'",
		"SET profiling_mode = 'detailed'",
		"SET profiling_output = " + q(out),
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
	}
	rows.Close()
	_, _ = db.ExecContext(ctx, "SET enable_profiling = 'no_output'")
	_, _ = db.ExecContext(ctx, "PRAGMA disable_profiling")

	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	top := map[string]any{}
	for k, v := range tree {
		if k != "children" {
			top[k] = v
		}
	}
	opKeys := map[string]bool{}
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		for k := range n {
			opKeys[k] = true
		}
		if ch, ok := n["children"].([]any); ok {
			for _, c := range ch {
				if m, ok := c.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	if ch, ok := tree["children"].([]any); ok {
		for _, c := range ch {
			if m, ok := c.(map[string]any); ok {
				walk(m)
			}
		}
	}
	return map[string]any{"query_level_metrics": top, "operator_keys": keys(opKeys)}, nil
}

func httpLogs(ctx context.Context, db *sql.DB, query string) (any, error) {
	if _, err := db.ExecContext(ctx, "CALL enable_logging('HTTP')"); err != nil {
		return nil, fmt.Errorf("enable_logging: %w", err)
	}
	defer db.ExecContext(ctx, "CALL disable_logging()")
	_, _ = db.ExecContext(ctx, "CALL truncate_duckdb_logs()")
	// Otherwise GETs are served from the external file cache filled by the full scan.
	_, _ = db.ExecContext(ctx, "SET enable_external_file_cache = false")
	defer db.ExecContext(ctx, "SET enable_external_file_cache = true")
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
	}
	rows.Close()

	var n int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM duckdb_logs WHERE type = 'HTTP'").Scan(&n); err != nil {
		return nil, err
	}
	samples := []string{}
	srows, err := db.QueryContext(ctx, "SELECT message FROM duckdb_logs WHERE type = 'HTTP' LIMIT 3")
	if err == nil {
		for srows.Next() {
			var m string
			_ = srows.Scan(&m)
			samples = append(samples, m)
		}
		srows.Close()
	}
	return map[string]any{"http_log_entries": n, "samples": samples}, nil
}

// probeCOG tries every table function added by the cog extension that takes a
// single VARCHAR, against a fixture COG, and reports the result schema.
func probeCOG(ctx context.Context, db *sql.DB, path string) (any, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT function_name, function_type, parameters::VARCHAR, parameter_types::VARCHAR
		FROM duckdb_functions()
		WHERE function_name ILIKE '%cog%' OR function_name ILIKE 'rs\_%' ESCAPE '\' OR function_name ILIKE '%stac%'
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	type fn struct{ Name, Type, Params, Types string }
	var fns []fn
	for rows.Next() {
		var f fn
		_ = rows.Scan(&f.Name, &f.Type, &f.Params, &f.Types)
		fns = append(fns, f)
	}
	rows.Close()

	attempts := []map[string]any{}
	for _, f := range fns {
		if f.Type != "table" {
			continue
		}
		stmt := fmt.Sprintf("DESCRIBE SELECT * FROM %s(%s)", f.Name, q(path))
		a := map[string]any{"function": f.Name, "sql": stmt}
		cr, err := db.QueryContext(ctx, stmt)
		if err != nil {
			a["error"] = err.Error()
		} else {
			cols := []string{}
			for cr.Next() {
				var name, typ string
				var n1, k, d, e any
				_ = cr.Scan(&name, &typ, &n1, &k, &d, &e)
				cols = append(cols, name+" "+typ)
			}
			cr.Close()
			a["columns"] = cols
		}
		attempts = append(attempts, a)
	}
	// Direct pixel-window probes. Fixture: 2048x2048 Float32 of 1.0 over
	// lon 110..110.2, lat -7..-7.2, so a 0.01 deg bbox is ~102x102 pixels.
	window := []struct{ name, sql string }{
		{"georeference", "SELECT RS_GeoReference(?)::VARCHAR"},
		{"width_height", "SELECT RS_Width(?)::VARCHAR || 'x' || RS_Height(?)::VARCHAR"},
		{"nodata", "SELECT RS_BandNoDataValue(?)::VARCHAR"},
		{"value_point", "SELECT RS_Value(?, 110.005, -7.005)::VARCHAR"},
		{"band_window", "SELECT 'len=' || len(a)::VARCHAR || ' sum=' || list_sum(a)::VARCHAR || ' type=' || typeof(a) FROM (SELECT RS_BandAsArray(?, 1, [110.0, -7.01, 110.01, -7.0]) AS a)"},
		{"band_window_raw_head", "SELECT left(RS_BandAsArray(?, 1, [110.0, -7.01, 110.01, -7.0])::VARCHAR, 300)"},
		{"values_points", "SELECT RS_Values(?, [110.001, 110.002], [-7.001, -7.002])::VARCHAR"},
		{"zonal_sum", "SELECT RS_ZonalStats(?, [110.0, -7.01, 110.01, -7.0], 1, 'sum')::VARCHAR"},
	}
	results := map[string]any{}
	for _, w := range window {
		t := time.Now()
		args := []any{path}
		if strings.Count(w.sql, "?") == 2 {
			args = append(args, path)
		}
		var s sql.NullString
		err := db.QueryRowContext(ctx, w.sql, args...).Scan(&s)
		res := map[string]any{"ms": time.Since(t).Milliseconds()}
		if err != nil {
			res["error"] = err.Error()
		} else {
			res["value"] = s.String
		}
		results[w.name] = res
	}
	return map[string]any{"functions": fns, "attempts": attempts, "window": results}, nil
}

func functionSet(ctx context.Context, db *sql.DB) map[string]bool {
	set := map[string]bool{}
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT function_name || ' [' || function_type || ']' FROM duckdb_functions()")
	if err != nil {
		return set
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		set[s] = true
	}
	return set
}

func diff(before, after map[string]bool, max int) []string {
	out := []string{}
	for k := range after {
		if !before[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustExec(ctx context.Context, db *sql.DB, s string) {
	if _, err := db.ExecContext(ctx, s); err != nil {
		fatal(fmt.Errorf("%s: %w", s, err))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "spike:", err)
	os.Exit(1)
}
