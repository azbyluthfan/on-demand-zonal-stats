package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/release"
)

// Limits bound request cost.
type Limits struct {
	MaxVillages         int           // 413 above this (default 10,000)
	QueryTimeout        time.Duration // 504 above this (default 30 s)
	MaxBodyBytes        int64         // default 2 MB
	MaxMetrics          int           // default 20
	MaxCategories       int           // per metric, default 500
	MaxGeometryVertices int           // default 20,000
}

// DefaultLimits are the documented defaults (docs/api.md).
func DefaultLimits() Limits {
	return Limits{MaxVillages: 10000, QueryTimeout: 30 * time.Second, MaxBodyBytes: 2 << 20,
		MaxMetrics: 20, MaxCategories: 500, MaxGeometryVertices: 20000}
}

// Server serves the API. The engine is attached once storage is reachable;
// until then every endpoint but /healthz answers 503.
type Server struct {
	limits  Limits
	log     *slog.Logger
	engine  atomic.Pointer[query.Engine]
	startup atomic.Pointer[string] // last startup error, for /healthz
	webDir  string
}

// New creates a server without an engine.
func New(limits Limits, log *slog.Logger, webDir string) *Server {
	return &Server{limits: limits, log: log, webDir: webDir}
}

// SetEngine attaches a ready engine.
func (s *Server) SetEngine(e *query.Engine) { s.engine.Store(e) }

// SetStartupError records why the engine is not ready yet.
func (s *Server) SetStartupError(err error) {
	msg := err.Error()
	s.startup.Store(&msg)
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/layers", s.withEngine(s.layers))
	mux.HandleFunc("GET /v1/areas", s.withEngine(s.areas))
	mux.HandleFunc("POST /v1/aggregate", s.withEngine(s.aggregate))
	mux.HandleFunc("GET /v1/tiles/{layer}/{z}/{x}/{y}", s.withEngine(s.vectorTile))
	mux.HandleFunc("GET /v1/tiles/raster/{layer}/{z}/{x}/{y}", s.withEngine(s.rasterTile))
	mux.HandleFunc("GET /v1/tiles/raster/{layer}/legend", s.withEngine(s.rasterLegend))
	if s.webDir != "" {
		if st, err := os.Stat(s.webDir); err == nil && st.IsDir() {
			mux.Handle("GET /", http.FileServer(http.Dir(s.webDir)))
		}
	}
	return s.logRequests(withGzip(mux))
}

func (s *Server) withEngine(h func(http.ResponseWriter, *http.Request, *query.Engine)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		e := s.engine.Load()
		if e == nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "release not loaded yet; see /healthz", nil)
			return
		}
		h(w, r, e)
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	e := s.engine.Load()
	if e == nil {
		body := map[string]any{"status": "starting"}
		if msg := s.startup.Load(); msg != nil {
			body["error"] = *msg
		}
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	body := map[string]any{"status": "ok", "release_id": e.Manifest.ReleaseID}
	if r.URL.Query().Get("deep") == "1" { // also check storage is reachable now
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := e.Ping(ctx); err != nil {
			body["status"], body["error"] = "degraded", err.Error()
			writeJSON(w, http.StatusServiceUnavailable, body)
			return
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) layers(w http.ResponseWriter, r *http.Request, e *query.Engine) {
	m := e.Manifest
	type cat struct {
		Category string `json:"category"`
		Count    int64  `json:"count"`
	}
	cats := make([]cat, 0, len(e.Categories))
	for _, c := range e.SortedCategories() {
		cats = append(cats, cat{c, e.Categories[c]})
	}
	type raster struct {
		ID          string                `json:"id"`
		Kind        string                `json:"kind"`
		Units       string                `json:"units"`
		Year        int                   `json:"year,omitempty"`
		PixelWidth  float64               `json:"pixel_width"`
		PixelHeight float64               `json:"pixel_height"`
		Classes     []release.RasterClass `json:"classes,omitempty"`
		Source      any                   `json:"source"`
	}
	rasters := []raster{}
	for _, ri := range m.Rasters {
		kind := release.RasterContinuous
		if ri.IsCategorical() {
			kind = release.RasterCategorical
		}
		rasters = append(rasters, raster{ri.ID, kind, ri.Units, ri.Year, ri.PixelWidth, ri.PixelHeight, ri.Classes, ri.Source})
	}
	var buildings any
	if m.HasBuildings() {
		buildings = map[string]any{"release": m.BuildingRelease}
	}
	sources := []any{}
	for _, sr := range m.Sources {
		sources = append(sources, map[string]any{"kind": sr.Kind, "name": sr.Name, "url": sr.URL,
			"version": sr.Version, "license": sr.License, "attribution": sr.Attribution})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"release_id":       m.ReleaseID,
		"created_at":       m.CreatedAt,
		"boundary_version": m.BoundaryVersion,
		"h3_resolution":    m.H3Resolution,
		"poi_release":      m.POIRelease,
		"metric_types":     MetricTypes,
		"buildings":        buildings,
		"tiles": map[string]any{
			"vector": map[string]any{"url": "/v1/tiles/{layer}/{z}/{x}/{y}.mvt", "layers": e.VectorLayers()},
			"raster": map[string]any{"url": "/v1/tiles/raster/{layer}/{z}/{x}/{y}.png",
				"legend": "/v1/tiles/raster/{layer}/legend", "minzoom": query.RasterTileMinZoom},
		},
		"raster_methods": []string{"fractional", "center"},
		"categories":     cats,
		"rasters":        rasters,
		"sources":        sources,
	})
}

func (s *Server) areas(w http.ResponseWriter, r *http.Request, e *query.Engine) {
	ctx, cancel := context.WithTimeout(r.Context(), s.limits.QueryTimeout)
	defer cancel()
	as, err := e.AdminAreas(ctx)
	if err != nil {
		s.queryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"release_id": e.Manifest.ReleaseID, "areas": as})
}

func (s *Server) aggregate(w http.ResponseWriter, r *http.Request, e *query.Engine) {
	start := time.Now()
	var req AggregateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.limits.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error(), nil)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint: "+err.Error(), nil)
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body has trailing data", nil)
		return
	}
	v, ferrs := validate(&req, e.Manifest, e.HasCategory, s.limits)
	if len(ferrs) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "request validation failed", ferrs)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.limits.QueryTimeout)
	defer cancel()
	// "Cache-Control: no-cache" bypasses every cache: the cold path, for
	// comparisons. Results are identical either way.
	tr := &query.Trace{NoCache: strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache")}
	stages := map[string]float64{}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

	t := time.Now()
	res, err := e.ResolveArea(ctx, v.area, s.limits.MaxVillages)
	stages["resolve_area"] = ms(time.Since(t))
	if err != nil {
		s.queryError(w, err)
		return
	}
	poi, err := e.CountPOIs(ctx, res, v.poi, tr)
	if err != nil {
		s.queryError(w, err)
		return
	}
	for k, d := range poi.Stats.Stages {
		stages[k] = ms(d)
	}
	var bld *query.BuildingResult
	if len(v.buildings) > 0 {
		if bld, err = e.Buildings(ctx, res, tr); err != nil {
			s.queryError(w, err)
			return
		}
		for k, d := range bld.Stats.Stages {
			stages[k] += ms(d)
		}
	}
	var rast *query.RasterResult
	if len(v.raster) > 0 {
		if rast, err = e.RasterSums(ctx, res, v.raster, tr); err != nil {
			s.queryError(w, err)
			return
		}
		for k, d := range rast.Stats.Stages {
			stages[k] = ms(d)
		}
	}

	t = time.Now()
	villages := make([]VillageOut, len(res.Villages))
	totals := map[string]Value{}
	for _, id := range v.order {
		totals[id] = 0
	}
	for i, vv := range res.Villages {
		out := VillageOut{VillageID: vv.ID, VillageName: vv.Name, DistrictCode: vv.DistrictCode, DistrictName: vv.DistrictName,
			RegencyCode: vv.RegencyCode, RegencyName: vv.RegencyName, AreaM2: vv.AreaM2, Metrics: map[string]Value{}}
		for _, m := range v.poi {
			n := Value(poi.Counts[m.ID][vv.ID])
			out.Metrics[m.ID] = n
			totals[m.ID] += n
		}
		for _, m := range v.buildings {
			x := Value(bld.Count[vv.ID])
			if m.Type == MetricBuildingArea {
				x = Value(bld.Area[vv.ID])
			}
			out.Metrics[m.ID] = x
			totals[m.ID] += x
		}
		for _, m := range v.raster {
			out.Metrics[m.ID] = Value(rast.Values[m.ID][vv.ID])
		}
		villages[i] = out
	}
	for _, m := range v.raster { // means and fractions are not sums over villages
		totals[m.ID] = Value(rast.Totals[m.ID])
	}
	summary := Summary{VillageCount: len(villages), Totals: totals}

	var geoms map[string]json.RawMessage
	if req.IncludeGeometry {
		if geoms, err = e.Geometries(ctx, res.IDs()); err != nil {
			s.queryError(w, err)
			return
		}
	}
	stages["assemble"] = ms(time.Since(t))

	var diag *Diagnostics
	if req.Diagnostics {
		st := poi.Stats
		diag = &Diagnostics{
			TotalMS: ms(time.Since(start)), StagesMS: stages, Partitions: st.Partitions,
			SelectedCells: st.SelectedCells, CandidateCellRows: st.CandidateCells,
			PointsScanned: st.Scanned, PointsInterior: st.Interior, PointsBoundary: st.BoundaryPOIs,
			PointsBoundaryTested: st.BoundaryTests, PointsOtherVillage: st.OtherVillage,
			UnassignedPOIs: poi.Unassigned, POIVillagesComputed: st.ComputedFor,
			Cache: tr.Cache, DuckDBProfile: tr.Profile,
		}
		if diag.DuckDBProfile == nil {
			diag.DuckDBProfile = map[string]map[string]float64{}
		}
		if rast != nil {
			diag.RasterWindows, diag.RasterBlocks, diag.RasterPixelsRead = rast.Stats.Windows, rast.Stats.Blocks, rast.Stats.PixelsRead
			diag.RasterPixelsWeighted, diag.RasterNodataWeight = rast.Stats.PixelsWeighted, rast.Stats.NodataWeight
		}
		if bld != nil {
			bs := bld.Stats
			diag.Buildings = &BuildingDiagnostics{VillagesComputed: bs.ComputedFor, CellRows: bs.CellRows,
				CellBuildings: bs.CellBuildings, BoundaryPoints: bs.Points, BoundaryTests: bs.PointTests,
				PointsOtherVillage: bs.OtherVillage, Unassigned: bld.Unassigned}
		}
		if diag.Partitions == nil {
			diag.Partitions = []string{}
		}
	}

	if !req.IncludeGeometry {
		writeJSON(w, http.StatusOK, AggregateResponse{ReleaseID: e.Manifest.ReleaseID, BoundaryVersion: e.Manifest.BoundaryVersion,
			Villages: villages, Summary: summary, Diagnostics: diag})
		return
	}
	fc := FeatureCollection{Type: "FeatureCollection", Features: make([]Feature, len(villages)),
		ReleaseID: e.Manifest.ReleaseID, BoundaryVersion: e.Manifest.BoundaryVersion, Summary: summary, Diagnostics: diag}
	for i, vo := range villages {
		fc.Features[i] = Feature{Type: "Feature", ID: vo.VillageID, Geometry: geoms[vo.VillageID], Properties: vo}
	}
	writeJSON(w, http.StatusOK, fc)
}

// vectorTile serves GET /v1/tiles/{layer}/{z}/{x}/{y}.mvt (docs/tiles.md).
func (s *Server) vectorTile(w http.ResponseWriter, r *http.Request, e *query.Engine) {
	t, err := query.ParseTile(r.PathValue("z"), r.PathValue("x"), r.PathValue("y"), ".mvt")
	if err != nil {
		s.queryError(w, err)
		return
	}
	var cats []string
	if c := strings.TrimSpace(r.URL.Query().Get("categories")); c != "" {
		cats = strings.Split(c, ",")
		if len(cats) > s.limits.MaxCategories {
			writeError(w, http.StatusBadRequest, "invalid_request", "too many categories", nil)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.limits.QueryTimeout)
	defer cancel()
	res, err := e.VectorTile(ctx, r.PathValue("layer"), t, cats, noCache(r))
	if err != nil {
		s.queryError(w, err)
		return
	}
	writeTile(w, "application/vnd.mapbox-vector-tile", res)
}

// rasterTile serves GET /v1/tiles/raster/{layer}/{z}/{x}/{y}.png.
func (s *Server) rasterTile(w http.ResponseWriter, r *http.Request, e *query.Engine) {
	t, err := query.ParseTile(r.PathValue("z"), r.PathValue("x"), r.PathValue("y"), ".png")
	if err != nil {
		s.queryError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.limits.QueryTimeout)
	defer cancel()
	res, err := e.RasterTile(ctx, r.PathValue("layer"), t, noCache(r))
	if err != nil {
		s.queryError(w, err)
		return
	}
	writeTile(w, "image/png", res)
}

// rasterLegend serves GET /v1/tiles/raster/{layer}/legend.
func (s *Server) rasterLegend(w http.ResponseWriter, r *http.Request, e *query.Engine) {
	ctx, cancel := context.WithTimeout(r.Context(), s.limits.QueryTimeout)
	defer cancel()
	lg, err := e.Legend(ctx, r.PathValue("layer"))
	if err != nil {
		s.queryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lg)
}

func noCache(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache")
}

// writeTile writes a tile with Server-Timing (time inside DuckDB, cache hit
// or miss) so browsers expose per-tile cost to the frontend.
func writeTile(w http.ResponseWriter, contentType string, res *query.TileResult) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	// Map clients keep tiles in memory; the server tile cache answers repeats
	// in ~1 ms. no-cache keeps browsers from silently reusing a tile, so the
	// Server-Timing the UI shows is always this request's.
	h.Set("Cache-Control", "no-cache")
	switch {
	case res.Cached:
		h.Set("Server-Timing", `cache;desc="hit"`)
	case res.BlockCached:
		h.Set("Server-Timing", `cache;desc="blocks"`)
	case res.DuckDB == 0: // below minzoom or outside the data: no query ran
		h.Set("Server-Timing", `empty;desc="no data"`)
	default:
		h.Set("Server-Timing", fmt.Sprintf(`duckdb;dur=%.2f, features;desc="%d"`, float64(res.DuckDB.Microseconds())/1000, res.Features))
	}
	h.Set("Timing-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Data)
}

// queryError maps engine errors to the documented status codes. No partial
// results are ever written: errors happen before the body.
func (s *Server) queryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, query.ErrBadTile):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
	case errors.Is(err, query.ErrInvalidArea):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), []FieldError{{Field: "area", Message: err.Error()}})
	case errors.Is(err, query.ErrTooManyVillages):
		writeError(w, http.StatusRequestEntityTooLarge, "too_many_villages",
			"area resolves to more villages than the limit; choose a smaller area", nil)
	case errors.Is(err, query.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "query exceeded the time limit", nil)
	case errors.Is(err, query.ErrStorage):
		s.log.Error("storage error", "err", err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "release storage is unavailable", nil)
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "client_closed", "request cancelled", nil)
	default:
		s.log.Error("query error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string, fields []FieldError) {
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].Field < fields[j].Field })
	writeJSON(w, status, ErrorResponse{Error: code, Message: msg, Fields: fields})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusOK {
		if _, ok := v.(FeatureCollection); ok {
			w.Header().Set("Content-Type", "application/geo+json")
		}
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" && sw.status == http.StatusOK {
			return // keep health-check noise out of the log
		}
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds())
	})
}

var _ io.Writer = (*statusWriter)(nil)
