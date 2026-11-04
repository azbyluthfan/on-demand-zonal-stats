// Package api is the HTTP layer (docs/api.md): request validation, error
// mapping and response assembly on top of internal/query.
package api

import (
	"encoding/json"

	"on-demand-zonal-stats/internal/query"
)

// AggregateRequest is the POST /v1/aggregate body.
type AggregateRequest struct {
	Area            AreaRequest     `json:"area"`
	Metrics         []MetricRequest `json:"metrics"`
	IncludeGeometry bool            `json:"include_geometry"`
	Diagnostics     bool            `json:"diagnostics"`
}

// AreaRequest selects villages: exactly one field.
type AreaRequest struct {
	ProvinceCode string          `json:"province_code,omitempty"`
	RegencyCode  string          `json:"regency_code,omitempty"`
	DistrictCode string          `json:"district_code,omitempty"`
	Geometry     json.RawMessage `json:"geometry,omitempty"` // GeoJSON Polygon/MultiPolygon
}

// MetricRequest is one requested metric.
type MetricRequest struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`                 // poi_count | raster_sum
	Categories []string `json:"categories,omitempty"` // poi_count
	Layer      string   `json:"layer,omitempty"`      // raster_sum
	Method     string   `json:"method,omitempty"`     // raster_sum: fractional | center
}

// AggregateResponse is the JSON response (include_geometry=false).
type AggregateResponse struct {
	ReleaseID       string       `json:"release_id"`
	BoundaryVersion string       `json:"boundary_version"`
	Villages        []VillageOut `json:"villages"`
	Summary         Summary      `json:"summary"`
	Diagnostics     *Diagnostics `json:"diagnostics,omitempty"`
}

// VillageOut is one village row.
type VillageOut struct {
	VillageID    string             `json:"village_id"`
	VillageName  string             `json:"village_name"`
	DistrictCode string             `json:"district_code"`
	DistrictName string             `json:"district_name"`
	RegencyCode  string             `json:"regency_code"`
	RegencyName  string             `json:"regency_name"`
	AreaM2       float64            `json:"area_m2"`
	Metrics      map[string]float64 `json:"metrics"`
}

// Summary totals over the returned villages.
type Summary struct {
	VillageCount int                `json:"village_count"`
	Totals       map[string]float64 `json:"totals"`
}

// Diagnostics explains where time and work went (docs/api.md, docs/caching.md).
type Diagnostics struct {
	TotalMS              float64            `json:"total_ms"`
	StagesMS             map[string]float64 `json:"stages_ms"`
	Partitions           []string           `json:"partitions"`
	SelectedCells        int64              `json:"selected_cells"`
	CandidateCellRows    int64              `json:"candidate_cell_rows"`
	PointsScanned        int64              `json:"points_scanned"`
	PointsInterior       int64              `json:"points_interior"`
	PointsBoundary       int64              `json:"points_boundary"`
	PointsBoundaryTested int64              `json:"points_boundary_tested"`
	PointsOtherVillage   int64              `json:"points_other_village"`
	UnassignedPOIs       map[string]int64   `json:"unassigned_pois"`
	POIVillagesComputed  int                `json:"poi_villages_computed"`
	Cache                query.CacheCounts  `json:"cache"`
	// DuckDBProfile sums DuckDB's own profiling per stage: latency_s,
	// cpu_time_s, bytes_read, rows_scanned, blocked_thread_s,
	// peak_buffer_bytes (max), row-group counters, queries.
	DuckDBProfile        map[string]map[string]float64 `json:"duckdb_profile"`
	RasterWindows        int                           `json:"raster_windows,omitempty"`
	RasterBlocks         int                           `json:"raster_blocks,omitempty"`
	RasterPixelsRead     int64                         `json:"raster_pixels_read,omitempty"`
	RasterPixelsWeighted int64                         `json:"raster_pixels_weighted,omitempty"`
	RasterNodataWeight   float64                       `json:"raster_nodata_weight,omitempty"`
}

// FeatureCollection is the include_geometry=true response: GeoJSON with
// the same properties plus foreign members for the response metadata.
type FeatureCollection struct {
	Type            string       `json:"type"`
	Features        []Feature    `json:"features"`
	ReleaseID       string       `json:"release_id"`
	BoundaryVersion string       `json:"boundary_version"`
	Summary         Summary      `json:"summary"`
	Diagnostics     *Diagnostics `json:"diagnostics,omitempty"`
}

// Feature is one village feature.
type Feature struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Geometry   json.RawMessage `json:"geometry"`
	Properties VillageOut      `json:"properties"`
}

// ErrorResponse is every non-2xx body.
type ErrorResponse struct {
	Error   string       `json:"error"`   // machine code: invalid_request, too_many_villages, ...
	Message string       `json:"message"` // human readable
	Fields  []FieldError `json:"fields,omitempty"`
}

// FieldError points at one invalid request field.
type FieldError struct {
	Field   string `json:"field"` // JSON path, e.g. metrics[1].categories[0]
	Message string `json:"message"`
}
