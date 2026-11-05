// Package api is the HTTP layer (docs/api.md): request validation, error
// mapping and response assembly on top of internal/query.
package api

import (
	"encoding/json"
	"math"
	"strconv"

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

// Metric types.
const (
	MetricPOICount            = "poi_count"
	MetricRasterSum           = "raster_sum"
	MetricRasterMean          = "raster_mean"
	MetricRasterClassFraction = "raster_class_fraction"
	MetricBuildingCount       = "building_count"
	MetricBuildingArea        = "building_area"
)

// MetricTypes lists every metric type (GET /v1/layers).
var MetricTypes = []string{MetricPOICount, MetricRasterSum, MetricRasterMean, MetricRasterClassFraction,
	MetricBuildingCount, MetricBuildingArea}

// MetricRequest is one requested metric.
type MetricRequest struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`                 // see MetricTypes
	Categories []string `json:"categories,omitempty"` // poi_count
	Layer      string   `json:"layer,omitempty"`      // raster_*
	Method     string   `json:"method,omitempty"`     // raster_*: fractional | center
	Classes    []int64  `json:"classes,omitempty"`    // raster_class_fraction
}

// Value is a metric value; NaN (an undefined mean or fraction, e.g. a
// village with no valid pixels) encodes as null.
type Value float64

// MarshalJSON implements json.Marshaler.
func (v Value) MarshalJSON() ([]byte, error) {
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, f, 'g', -1, 64), nil
}

// UnmarshalJSON implements json.Unmarshaler (null -> NaN).
func (v *Value) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*v = Value(math.NaN())
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	*v = Value(f)
	return err
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
	VillageID    string           `json:"village_id"`
	VillageName  string           `json:"village_name"`
	DistrictCode string           `json:"district_code"`
	DistrictName string           `json:"district_name"`
	RegencyCode  string           `json:"regency_code"`
	RegencyName  string           `json:"regency_name"`
	AreaM2       float64          `json:"area_m2"`
	Metrics      map[string]Value `json:"metrics"`
}

// Summary totals over the returned villages.
type Summary struct {
	VillageCount int              `json:"village_count"`
	Totals       map[string]Value `json:"totals"`
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
	Buildings            *BuildingDiagnostics          `json:"buildings,omitempty"`
}

// BuildingDiagnostics explains the building path (docs/buildings.md).
type BuildingDiagnostics struct {
	VillagesComputed   int   `json:"villages_computed"`
	CellRows           int64 `json:"cell_rows"`
	CellBuildings      int64 `json:"cell_buildings"`
	BoundaryPoints     int64 `json:"boundary_points"`
	BoundaryTests      int64 `json:"boundary_tests"`
	PointsOtherVillage int64 `json:"points_other_village"`
	Unassigned         int64 `json:"unassigned"`
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
