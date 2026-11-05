package api

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"

	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/release"
)

var (
	metricIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	codeRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
)

// validated is a request after validation.
type validated struct {
	area      query.Area
	poi       []query.POIMetric
	raster    []query.RasterMetric
	buildings []MetricRequest
	order     []string // metric IDs in request order
}

// validate checks the request against the release and returns field errors.
func validate(req *AggregateRequest, m *release.Manifest, hasCategory func(string) bool, lim Limits) (*validated, []FieldError) {
	var errs []FieldError
	fail := func(field, format string, a ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, a...)})
	}
	v := &validated{}

	// Area: exactly one selector.
	set := 0
	for _, s := range []struct{ name, val string }{
		{"province_code", req.Area.ProvinceCode}, {"regency_code", req.Area.RegencyCode}, {"district_code", req.Area.DistrictCode},
	} {
		if s.val == "" {
			continue
		}
		set++
		if !codeRe.MatchString(s.val) {
			fail("area."+s.name, "must be 1-32 characters of letters, digits, '.', '_' or '-'")
		}
	}
	if len(req.Area.Geometry) > 0 && string(req.Area.Geometry) != "null" {
		set++
		if g, msg := parseAreaGeometry(req.Area.Geometry, lim.MaxGeometryVertices); msg != "" {
			fail("area.geometry", "%s", msg)
		} else {
			v.area.Geometry = g
		}
	}
	switch set {
	case 0:
		fail("area", "exactly one of province_code, regency_code, district_code, geometry is required")
	case 1:
		v.area.ProvinceCode, v.area.RegencyCode, v.area.DistrictCode = req.Area.ProvinceCode, req.Area.RegencyCode, req.Area.DistrictCode
	default:
		fail("area", "exactly one of province_code, regency_code, district_code, geometry may be set (got %d)", set)
	}

	// Metrics.
	if len(req.Metrics) == 0 {
		fail("metrics", "at least one metric is required")
	}
	if len(req.Metrics) > lim.MaxMetrics {
		fail("metrics", "at most %d metrics per request", lim.MaxMetrics)
	}
	seen := map[string]bool{}
	for i, mr := range req.Metrics {
		p := fmt.Sprintf("metrics[%d]", i)
		if !metricIDRe.MatchString(mr.ID) {
			fail(p+".id", "must match %s", metricIDRe)
		} else if seen[mr.ID] {
			fail(p+".id", "duplicate metric id %q", mr.ID)
		}
		seen[mr.ID] = true
		v.order = append(v.order, mr.ID)
		if mr.Type != MetricRasterClassFraction && len(mr.Classes) > 0 {
			fail(p+".classes", "classes apply to raster_class_fraction only")
		}
		switch mr.Type {
		case MetricPOICount:
			if mr.Layer != "" || mr.Method != "" {
				fail(p, "layer and method apply to raster metrics only")
			}
			if len(mr.Categories) == 0 {
				fail(p+".categories", "at least one category is required")
			}
			if len(mr.Categories) > lim.MaxCategories {
				fail(p+".categories", "at most %d categories per metric", lim.MaxCategories)
			}
			dup := map[string]bool{}
			for j, c := range mr.Categories {
				switch {
				case !hasCategory(c):
					fail(fmt.Sprintf("%s.categories[%d]", p, j), "unknown category %q (see GET /v1/layers)", c)
				case dup[c]:
					fail(fmt.Sprintf("%s.categories[%d]", p, j), "duplicate category %q", c)
				}
				dup[c] = true
			}
			v.poi = append(v.poi, query.POIMetric{ID: mr.ID, Categories: mr.Categories})
		case MetricRasterSum, MetricRasterMean, MetricRasterClassFraction:
			if len(mr.Categories) > 0 {
				fail(p+".categories", "categories apply to poi_count only")
			}
			info, ok := m.Raster(mr.Layer)
			switch {
			case !ok:
				fail(p+".layer", "unknown raster layer %q (see GET /v1/layers)", mr.Layer)
			case mr.Type == MetricRasterClassFraction && !info.IsCategorical():
				fail(p+".layer", "layer %q is continuous; use raster_sum or raster_mean", mr.Layer)
			case mr.Type != MetricRasterClassFraction && info.IsCategorical():
				fail(p+".layer", "layer %q is categorical; use raster_class_fraction", mr.Layer)
			}
			if mr.Method != "fractional" && mr.Method != "center" {
				fail(p+".method", `must be "fractional" or "center"`)
			}
			rm := query.RasterMetric{ID: mr.ID, Layer: mr.Layer, Method: mr.Method, Stat: query.StatSum}
			switch mr.Type {
			case MetricRasterMean:
				rm.Stat = query.StatMean
			case MetricRasterClassFraction:
				rm.Stat, rm.Classes = query.StatClassFraction, mr.Classes
				if len(mr.Classes) == 0 {
					fail(p+".classes", "at least one class is required")
				}
				dup := map[int64]bool{}
				for j, c := range mr.Classes {
					if ok && info.IsCategorical() {
						if _, known := info.Class(c); !known {
							fail(fmt.Sprintf("%s.classes[%d]", p, j), "unknown class %d of layer %q (see GET /v1/layers)", c, mr.Layer)
						}
					}
					if dup[c] {
						fail(fmt.Sprintf("%s.classes[%d]", p, j), "duplicate class %d", c)
					}
					dup[c] = true
				}
			}
			v.raster = append(v.raster, rm)
		case MetricBuildingCount, MetricBuildingArea:
			if len(mr.Categories) > 0 || mr.Layer != "" || mr.Method != "" {
				fail(p, "%s takes no categories, layer or method", mr.Type)
			}
			if !m.HasBuildings() {
				fail(p+".type", "this release has no building layer")
			}
			v.buildings = append(v.buildings, mr)
		default:
			fail(p+".type", "must be one of %s", strings.Join(MetricTypes, ", "))
		}
	}
	return v, errs
}

// parseAreaGeometry parses a GeoJSON Polygon/MultiPolygon in lon/lat. It
// returns a message for the field error, or "" when valid. Topological
// validity (self-intersection) is checked by DuckDB during resolution.
func parseAreaGeometry(raw []byte, maxVertices int) (orb.Geometry, string) {
	g, err := geojson.UnmarshalGeometry(raw)
	if err != nil {
		return nil, "must be a GeoJSON geometry object: " + err.Error()
	}
	var polys []orb.Polygon
	switch t := g.Geometry().(type) {
	case orb.Polygon:
		polys = []orb.Polygon{t}
	case orb.MultiPolygon:
		polys = t
	default:
		return nil, fmt.Sprintf("must be a Polygon or MultiPolygon, got %s", g.Type)
	}
	if len(polys) == 0 {
		return nil, "must have at least one polygon"
	}
	n := 0
	for _, poly := range polys {
		if len(poly) == 0 {
			return nil, "polygon has no rings"
		}
		for _, r := range poly {
			if len(r) < 4 || r[0] != r[len(r)-1] {
				return nil, "each ring needs at least 4 positions and must be closed"
			}
			for _, pt := range r {
				if math.IsNaN(pt[0]) || math.IsNaN(pt[1]) || pt[0] < -180 || pt[0] > 180 || pt[1] < -90 || pt[1] > 90 {
					return nil, "coordinates must be [lon, lat] in WGS84 (lon -180..180, lat -90..90)"
				}
			}
			n += len(r)
		}
	}
	if n > maxVertices {
		return nil, fmt.Sprintf("too many vertices (%d > %d)", n, maxVertices)
	}
	return g.Geometry(), ""
}
