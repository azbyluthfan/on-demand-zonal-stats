// Package release defines a data release's layout and manifest, shared by
// prep (writer) and serving (reader). Paths are relative to the release
// root: s3://<bucket>/releases/<release_id>/ or a local directory.
package release

import (
	"fmt"
	"path"
)

// NoProvince is the POI partition for points that touch no village.
const NoProvince = "_none"

// Layout paths.
const (
	VillagesPath   = "boundaries/villages.parquet"
	CategoriesPath = "categories.json"
	ManifestPath   = "manifest.json"
)

// VillageH3Path is the village_h3 partition for one province.
func VillageH3Path(province string) string {
	return path.Join("village_h3", "province_code="+province, "part-0.parquet")
}

// POIPath is the POI partition for one province.
func POIPath(poiRelease, province string) string {
	return path.Join("pois", "release="+poiRelease, "province_code="+province, "part-0.parquet")
}

// BuildingCellsPath holds, for one province, buildings in interior H3
// cells pre-summed per cell (h3_cell, buildings, area_m2): exact, because an
// interior cell lies wholly inside one village (docs/buildings.md).
func BuildingCellsPath(buildingRelease, province string) string {
	return path.Join("buildings", "release="+buildingRelease, "province_code="+province, "cells.parquet")
}

// BuildingPointsPath holds, for one province, the buildings in boundary H3
// cells as points (h3_cell, x_e7, y_e7, area_m2) for the exact ST_Covers test.
func BuildingPointsPath(buildingRelease, province string) string {
	return path.Join("buildings", "release="+buildingRelease, "province_code="+province, "points.parquet")
}

// RasterPath is a raster layer's COG.
func RasterPath(id string) string { return path.Join("rasters", id+".tif") }

// RasterInfoPath is a raster layer's companion JSON.
func RasterInfoPath(id string) string { return path.Join("rasters", id+".json") }

// Source is provenance recorded in the manifest.
type Source struct {
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
	Version     string `json:"version,omitempty"`
	License     string `json:"license"`
	Attribution string `json:"attribution,omitempty"`
}

// QCResult is one prep QC check.
type QCResult struct {
	Name     string `json:"name"`
	Blocking bool   `json:"blocking"`
	Passed   bool   `json:"passed"`
	Value    any    `json:"value,omitempty"`
	Detail   any    `json:"detail,omitempty"`
	Error    string `json:"error,omitempty"`
}

// SourceRecord is a source with its local checksum when available.
type SourceRecord struct {
	Kind string `json:"kind"` // boundaries | pois | buildings | raster:<id>
	Source
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

// Artifact is one file in the release.
type Artifact struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Rows   *int64 `json:"rows,omitempty"`
}

// Raster layer kinds.
const (
	RasterContinuous  = "continuous"  // values are quantities: sum, mean
	RasterCategorical = "categorical" // values are class codes: class fractions
)

// RasterClass is one class of a categorical raster.
type RasterClass struct {
	Value int64  `json:"value"`
	Label string `json:"label"`
	Color string `json:"color,omitempty"` // #rrggbb, for legends
}

// RasterInfo is the companion <id>.json of a raster layer.
type RasterInfo struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	CRS         string   `json:"crs"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	OriginX     float64  `json:"origin_x"` // upper-left corner, full precision
	OriginY     float64  `json:"origin_y"`
	PixelWidth  float64  `json:"pixel_width"`
	PixelHeight float64  `json:"pixel_height"` // positive; rows go south
	DataType    string   `json:"data_type"`
	Nodata      *float64 `json:"nodata"`
	Units       string   `json:"units"`
	Year        int      `json:"year,omitempty"`
	// Kind is RasterContinuous (also when empty, for older releases) or
	// RasterCategorical.
	Kind    string        `json:"kind,omitempty"`
	Classes []RasterClass `json:"classes,omitempty"` // categorical only
	// Scale multiplies stored pixel values to get Units (0 or absent = 1),
	// e.g. 0.1 for night lights stored as integer radiance x 10.
	Scale       float64    `json:"scale,omitempty"`
	BlockSize   [2]int     `json:"block_size"`
	Overviews   int        `json:"overviews"`
	Sum         float64    `json:"sum"` // stored values (before Scale)
	ValidPixels int64      `json:"valid_pixels"`
	Bounds      [4]float64 `json:"bounds"` // xmin, ymin, xmax, ymax
	Source      Source     `json:"source"`
}

// Manifest is manifest.json.
type Manifest struct {
	ReleaseID       string `json:"release_id"`
	CreatedAt       string `json:"created_at"`
	Status          string `json:"status"` // "ok" or "failed"
	H3Resolution    int    `json:"h3_resolution"`
	BoundaryVersion string `json:"boundary_version"`
	POIRelease      string `json:"poi_release"`
	// BuildingRelease is the building partition release; empty when the
	// release has no building layer.
	BuildingRelease string         `json:"building_release,omitempty"`
	ProvinceCodes   []string       `json:"province_codes"`
	Tools           map[string]any `json:"tools"`
	Sources         []SourceRecord `json:"sources"`
	Artifacts       []Artifact     `json:"artifacts"`
	Rasters         []RasterInfo   `json:"rasters"`
	QC              []QCResult     `json:"qc"`
}

// Has reports whether the manifest lists the artifact path.
func (m *Manifest) Has(p string) bool {
	for _, a := range m.Artifacts {
		if a.Path == p {
			return true
		}
	}
	return false
}

// IsCategorical reports whether the layer holds class codes.
func (r *RasterInfo) IsCategorical() bool { return r.Kind == RasterCategorical }

// ScaleFactor is Scale, defaulting to 1.
func (r *RasterInfo) ScaleFactor() float64 {
	if r.Scale == 0 {
		return 1
	}
	return r.Scale
}

// Class returns the class with the given value.
func (r *RasterInfo) Class(v int64) (RasterClass, bool) {
	for _, c := range r.Classes {
		if c.Value == v {
			return c, true
		}
	}
	return RasterClass{}, false
}

// HasBuildings reports whether the release has the building layer.
func (m *Manifest) HasBuildings() bool { return m.BuildingRelease != "" }

// Raster returns the raster layer with the given ID.
func (m *Manifest) Raster(id string) (*RasterInfo, bool) {
	for i := range m.Rasters {
		if m.Rasters[i].ID == id {
			return &m.Rasters[i], true
		}
	}
	return nil, false
}

// Validate checks the manifest is servable.
func (m *Manifest) Validate() error {
	switch {
	case m.Status != "ok":
		return fmt.Errorf("release %s has status %q", m.ReleaseID, m.Status)
	case m.H3Resolution < 0 || m.H3Resolution > 15:
		return fmt.Errorf("release %s: bad h3_resolution %d", m.ReleaseID, m.H3Resolution)
	case !m.Has(VillagesPath):
		return fmt.Errorf("release %s: manifest lists no %s", m.ReleaseID, VillagesPath)
	}
	return nil
}
