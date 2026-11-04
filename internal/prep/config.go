// Package prep builds a data release (docs/prep.md): villages, village_h3,
// POIs, rasters, categories.json and manifest.json, with the QC checks of
// docs/prep.md. It runs offline; GDAL is used only for the raster conversion step.
package prep

import (
	"encoding/json"
	"fmt"
	"os"

	"on-demand-zonal-stats/internal/release"
)

// Config describes one release. Paths are relative to the working directory.
type Config struct {
	ReleaseID    string `json:"release_id"`
	H3Resolution int    `json:"h3_resolution"`

	// ProvinceCodes limits the release to these provinces (empty = all).
	ProvinceCodes []string `json:"province_codes,omitempty"`

	Boundaries BoundarySource `json:"boundaries"`
	POIs       POISource      `json:"pois"`
	Rasters    []RasterSource `json:"rasters"`
	QC         QCConfig       `json:"qc"`
}

// Source is provenance recorded in the manifest.
type Source = release.Source

// BoundarySource is the village boundary input.
type BoundarySource struct {
	// Adapter maps the source schema to ours: "geojson" (columns already
	// named like villages.parquet) or "hdx_cod_ab" (HDX COD-AB admin 4).
	Adapter         string `json:"adapter"`
	Path            string `json:"path"`
	Layer           string `json:"layer,omitempty"`
	BoundaryVersion string `json:"boundary_version"`
	Source          Source `json:"source"`
}

// POISource is the point input.
type POISource struct {
	// Adapter: "geojson" (poi_id, name, category, point geometry) or
	// "overture" (Overture Places GeoParquet; Path is the release prefix).
	Adapter string `json:"adapter"`
	Path    string `json:"path"`
	Release string `json:"release"` // used in the partition path
	Source  Source `json:"source"`
}

// RasterSource is one raster layer.
type RasterSource struct {
	ID    string `json:"id"` // e.g. population_2020
	Path  string `json:"path"`
	SRS   string `json:"srs,omitempty"` // assign if the source lacks one (e.g. .asc)
	Units string `json:"units"`
	Year  int    `json:"year,omitempty"`
	// ClipToVillages crops the raster to the villages' extent (plus a pad),
	// for province-sized releases.
	ClipToVillages bool `json:"clip_to_villages,omitempty"`
	// ExpectedTotal, when set, is the source-stated sum checked within
	// QC.RasterSumTolerance.
	ExpectedTotal *float64 `json:"expected_total,omitempty"`
	Source        Source   `json:"source"`
}

// QCConfig tunes the release QC checks.
type QCConfig struct {
	MaxOverlapFraction float64 `json:"max_overlap_fraction"` // default 0.001 (0.1%)
	RasterSumTolerance float64 `json:"raster_sum_tolerance"` // default 0.01 (1%)
	// CrossCheckPOIs re-assigns every POI with a naive ST_Covers join and
	// requires an exact match with the H3 split (the POI golden check).
	// Cheap for a province, skip nationally.
	CrossCheckPOIs bool `json:"cross_check_pois"`
}

// LoadConfig reads and validates a JSON config.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if c.H3Resolution == 0 {
		c.H3Resolution = 9
	}
	if c.QC.MaxOverlapFraction == 0 {
		c.QC.MaxOverlapFraction = 0.001
	}
	if c.QC.RasterSumTolerance == 0 {
		c.QC.RasterSumTolerance = 0.01
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	switch {
	case c.ReleaseID == "":
		return fmt.Errorf("config: release_id is required")
	case c.H3Resolution < 0 || c.H3Resolution > 15:
		return fmt.Errorf("config: h3_resolution %d out of range", c.H3Resolution)
	case c.Boundaries.Path == "" || c.Boundaries.BoundaryVersion == "":
		return fmt.Errorf("config: boundaries.path and boundaries.boundary_version are required")
	case c.POIs.Path == "" || c.POIs.Release == "":
		return fmt.Errorf("config: pois.path and pois.release are required")
	}
	switch c.Boundaries.Adapter {
	case "geojson", "hdx_cod_ab":
	default:
		return fmt.Errorf("config: unknown boundaries.adapter %q", c.Boundaries.Adapter)
	}
	switch c.POIs.Adapter {
	case "geojson", "overture":
	default:
		return fmt.Errorf("config: unknown pois.adapter %q", c.POIs.Adapter)
	}
	seen := map[string]bool{}
	for _, r := range c.Rasters {
		if r.ID == "" || r.Path == "" || seen[r.ID] {
			return fmt.Errorf("config: raster needs a unique id and a path (%q)", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}
