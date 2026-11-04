package testfixtures

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"on-demand-zonal-stats/internal/prep"
)

var (
	releaseOnce sync.Once
	releaseDir  string
	releaseErr  error
)

var (
	rasterOnce sync.Once
	rasterDir  string
	rasterErr  error
)

// ReleaseWithRasters returns a fixture release that includes the raster:
// built here when GDAL is on PATH (CI installs it), otherwise the one built
// by `make prep-fixture` in out/releases/fixture-1. Skips if neither exists.
func ReleaseWithRasters(t testing.TB) string {
	t.Helper()
	rasterOnce.Do(func() {
		if _, err := exec.LookPath("gdal_translate"); err == nil {
			wd, _ := os.Getwd()
			if rasterErr = os.Chdir(RepoRoot()); rasterErr != nil {
				return
			}
			defer os.Chdir(wd)
			cfg, err := prep.LoadConfig("configs/fixture.json")
			if err != nil {
				rasterErr = err
				return
			}
			out, err := os.MkdirTemp("", "fixture-release-rasters-")
			if err != nil {
				rasterErr = err
				return
			}
			if _, rasterErr = prep.Build(context.Background(), cfg, out, prep.Options{}); rasterErr == nil {
				rasterDir = filepath.Join(out, "releases", cfg.ReleaseID)
			}
			return
		}
		local := filepath.Join(RepoRoot(), "out", "releases", "fixture-1")
		if _, err := os.Stat(filepath.Join(local, "rasters", "population_2020.tif")); err == nil {
			rasterDir = local
		}
	})
	if rasterErr != nil {
		t.Fatal(rasterErr)
	}
	if rasterDir == "" {
		t.Skip("no fixture raster: install GDAL or run `make prep-fixture`")
	}
	return rasterDir
}

// RepoRoot is the repository root.
func RepoRoot() string { return filepath.Join(Dir(), "..", "..") }

// Release builds configs/fixture.json once per test binary (without
// rasters, so no GDAL is needed) and returns the release directory.
func Release(t testing.TB) string {
	t.Helper()
	releaseOnce.Do(func() {
		wd, _ := os.Getwd()
		if releaseErr = os.Chdir(RepoRoot()); releaseErr != nil { // config paths are repo-relative
			return
		}
		defer os.Chdir(wd)
		cfg, err := prep.LoadConfig("configs/fixture.json")
		if err != nil {
			releaseErr = err
			return
		}
		out, err := os.MkdirTemp("", "fixture-release-")
		if err != nil {
			releaseErr = err
			return
		}
		if _, releaseErr = prep.Build(context.Background(), cfg, out, prep.Options{SkipRasters: true}); releaseErr == nil {
			releaseDir = filepath.Join(out, "releases", cfg.ReleaseID)
		}
	})
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	return releaseDir
}
