package prep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"on-demand-zonal-stats/internal/release"
)

var logf = log.Printf

// Manifest types live in internal/release (shared with serving).
type (
	Manifest     = release.Manifest
	SourceRecord = release.SourceRecord
	Artifact     = release.Artifact
)

// Options for Build.
type Options struct {
	SkipRasters bool // for environments without GDAL (unit tests)
}

// Build produces a release under outRoot/releases/<release_id>/ and returns
// its manifest. A *BlockingError is returned (with the manifest still
// written, status "failed") when blocking QC checks fail.
func Build(ctx context.Context, cfg *Config, outRoot string, opt Options) (*Manifest, error) {
	out := filepath.Join(outRoot, "releases", cfg.ReleaseID)
	if err := os.RemoveAll(out); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	start := time.Now()
	d, err := openDB(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()

	qc := &qcLog{}
	m := &Manifest{
		ReleaseID: cfg.ReleaseID, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		H3Resolution: cfg.H3Resolution, BoundaryVersion: cfg.Boundaries.BoundaryVersion,
		POIRelease: cfg.POIs.Release, ProvinceCodes: cfg.ProvinceCodes,
		Tools: tools(ctx, d),
	}

	step := func(name string, f func() error) error {
		t := time.Now()
		if err := f(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		logf("step %-12s %s", name, time.Since(t).Round(time.Millisecond))
		return nil
	}
	if err := step("boundaries", func() error { return loadBoundaries(ctx, d, cfg, qc) }); err != nil {
		return nil, err
	}
	if err := step("village_h3", func() error { return buildVillageH3(ctx, d, cfg, qc) }); err != nil {
		return nil, err
	}
	if err := step("pois", func() error { return loadPOIs(ctx, d, cfg, qc) }); err != nil {
		return nil, err
	}
	if err := step("write", func() error {
		if _, err := writeVillages(ctx, d, out); err != nil {
			return err
		}
		if _, err := writeVillageH3(ctx, d, out); err != nil {
			return err
		}
		if _, err := writePOIs(ctx, d, cfg, out); err != nil {
			return err
		}
		_, err := writeCategories(ctx, d, out)
		return err
	}); err != nil {
		return nil, err
	}
	if !opt.SkipRasters {
		for _, rs := range cfg.Rasters {
			if err := step("raster:"+rs.ID, func() error {
				info, err := buildRaster(ctx, d, cfg, rs, out, qc)
				if err == nil {
					m.Rasters = append(m.Rasters, *info)
				}
				return err
			}); err != nil {
				return nil, err
			}
		}
	}

	m.Sources = sourceRecords(cfg, opt)
	if m.Artifacts, err = listArtifacts(ctx, d, out); err != nil {
		return nil, err
	}
	m.QC = qc.results
	m.Status = "ok"
	failed := qc.failures()
	if len(failed) > 0 {
		m.Status = "failed"
	}
	if err := writeJSON(filepath.Join(out, "manifest.json"), m); err != nil {
		return nil, err
	}
	logf("release %s: status=%s in %s -> %s", cfg.ReleaseID, m.Status, time.Since(start).Round(time.Millisecond), out)
	if len(failed) > 0 {
		return m, &BlockingError{Checks: failed}
	}
	return m, nil
}

func tools(ctx context.Context, d *db) map[string]any {
	t := map[string]any{}
	var v string
	if d.queryRow(ctx, "SELECT version()").Scan(&v) == nil {
		t["duckdb"] = v
	}
	if rows, err := d.query(ctx, `SELECT extension_name, extension_version FROM duckdb_extensions()
		WHERE loaded AND extension_name IN ('spatial', 'httpfs', 'h3', 'cog') ORDER BY 1`); err == nil {
		for rows.Next() {
			var n, ver string
			_ = rows.Scan(&n, &ver)
			t["ext_"+n] = ver
		}
		rows.Close()
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			switch dep.Path {
			case "github.com/duckdb/duckdb-go/v2", "github.com/uber/h3-go/v4", "github.com/paulmach/orb":
				t[dep.Path] = dep.Version
			}
		}
	}
	if out, err := runGDAL(ctx, "gdalinfo", "--version"); err == nil {
		t["gdal"] = strings.TrimSpace(string(out))
	}
	return t
}

func sourceRecords(cfg *Config, opt Options) []SourceRecord {
	recs := []SourceRecord{
		{Kind: "boundaries", Source: cfg.Boundaries.Source, Path: cfg.Boundaries.Path},
		{Kind: "pois", Source: cfg.POIs.Source, Path: cfg.POIs.Path},
	}
	if !opt.SkipRasters {
		for _, r := range cfg.Rasters {
			recs = append(recs, SourceRecord{Kind: "raster:" + r.ID, Source: r.Source, Path: r.Path})
		}
	}
	for i := range recs {
		if sum, err := fileSHA256(recs[i].Path); err == nil {
			recs[i].SHA256 = sum
		}
	}
	return recs
}

func listArtifacts(ctx context.Context, d *db, out string) ([]Artifact, error) {
	var arts []Artifact
	err := filepath.WalkDir(out, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Name() == "manifest.json" {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		sum, err := fileSHA256(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		a := Artifact{Path: filepath.ToSlash(rel), Bytes: fi.Size(), SHA256: sum}
		if strings.HasSuffix(p, ".parquet") {
			n, err := d.count(ctx, fmt.Sprintf("SELECT count(*) FROM read_parquet(%s)", lit(p)))
			if err != nil {
				return err
			}
			a.Rows = &n
		}
		arts = append(arts, a)
		return nil
	})
	sort.Slice(arts, func(i, j int) bool { return arts[i].Path < arts[j].Path })
	return arts, err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return "", errors.Join(err, errors.New("not a file"))
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
