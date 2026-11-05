package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"on-demand-zonal-stats/internal/api"
	"on-demand-zonal-stats/internal/query"
	"on-demand-zonal-stats/internal/testfixtures"
)

var schemas = map[string]*jsonschema.Schema{}

func schema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	if s, ok := schemas[name]; ok {
		return s
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile(filepath.Join(testfixtures.RepoRoot(), "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	schemas[name] = s
	return s
}

func validateSchema(t *testing.T, name string, body []byte) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if err := schema(t, name).Validate(inst); err != nil {
		t.Fatalf("response violates %s: %v\n%s", name, err, abbreviate(body))
	}
}

func abbreviate(b []byte) string {
	if len(b) > 1500 {
		return string(b[:1500]) + "…"
	}
	return string(b)
}

func newServer(t *testing.T, base string, mod func(*api.Limits)) (*httptest.Server, *query.Engine) {
	t.Helper()
	e, err := query.Open(context.Background(), query.Options{Base: base, AllowInstall: true, Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	lim := api.DefaultLimits()
	if mod != nil {
		mod(&lim)
	}
	var logOut io.Writer = io.Discard
	if os.Getenv("ZS_TEST_LOG") == "1" {
		logOut = os.Stderr
	}
	s := api.New(lim, slog.New(slog.NewTextHandler(logOut, nil)), "")
	s.SetEngine(e)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, e
}

func post(t *testing.T, ts *httptest.Server, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case string:
		r = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		r = bytes.NewReader(raw)
	}
	resp, err := http.Post(ts.URL+"/v1/aggregate", "application/json", r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func get(t *testing.T, ts *httptest.Server, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func req(area map[string]any, diag, geom bool) map[string]any {
	return map[string]any{
		"area": area,
		"metrics": []map[string]any{
			{"id": "cafes", "type": "poi_count", "categories": []string{"cafe", "coffee_shop"}},
			{"id": "schools", "type": "poi_count", "categories": []string{"school"}},
		},
		"diagnostics": diag, "include_geometry": geom,
	}
}

func TestAggregateContractAndValues(t *testing.T) {
	ts, e := newServer(t, testfixtures.Release(t), nil)
	status, body := post(t, ts, req(map[string]any{"province_code": "34"}, true, false))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	validateSchema(t, "aggregate_response.schema.json", body)

	var got api.AggregateResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	// Values equal the engine's own answer.
	r, _ := e.ResolveArea(context.Background(), query.Area{ProvinceCode: "34"}, 0)
	want, _ := e.CountPOIs(context.Background(), r, []query.POIMetric{
		{ID: "cafes", Categories: []string{"cafe", "coffee_shop"}}, {ID: "schools", Categories: []string{"school"}}}, nil)
	if got.Summary.VillageCount != 6 || len(got.Villages) != 6 {
		t.Fatalf("village count %d/%d", got.Summary.VillageCount, len(got.Villages))
	}
	var total float64
	for _, v := range got.Villages {
		if float64(v.Metrics["cafes"]) != float64(want.Counts["cafes"][v.VillageID]) {
			t.Errorf("%s cafes %v, engine %d", v.VillageID, v.Metrics["cafes"], want.Counts["cafes"][v.VillageID])
		}
		if _, ok := v.Metrics["schools"]; !ok {
			t.Errorf("%s: zero-valued metric omitted", v.VillageID)
		}
		total += float64(v.Metrics["cafes"])
	}
	if total != float64(got.Summary.Totals["cafes"]) {
		t.Errorf("totals %v vs sum %v", got.Summary.Totals["cafes"], total)
	}
	if d := got.Diagnostics; d == nil || d.PointsInterior == 0 || d.PointsBoundaryTested == 0 || d.StagesMS["poi_boundary"] < 0 {
		t.Errorf("diagnostics incomplete: %+v", d)
	}
}

func TestGeoJSONContract(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), nil)
	status, body := post(t, ts, req(map[string]any{"district_code": "340401"}, true, true))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	validateSchema(t, "aggregate_geojson.schema.json", body)
	var fc api.FeatureCollection
	_ = json.Unmarshal(body, &fc)
	if len(fc.Features) != 2 || fc.Features[0].ID != "V001" || fc.Features[0].Properties.VillageID != "V001" {
		t.Fatalf("features: %+v", fc.Features)
	}
}

// include_geometry and diagnostics change only what they should.
func TestTogglesChangeOnlyTheirParts(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), nil)
	area := map[string]any{"regency_code": "3404"}
	_, plain := post(t, ts, req(area, false, false))
	_, diag := post(t, ts, req(area, true, false))
	_, geo := post(t, ts, req(area, false, true))

	var p, d api.AggregateResponse
	_ = json.Unmarshal(plain, &p)
	_ = json.Unmarshal(diag, &d)
	if p.Diagnostics != nil || d.Diagnostics == nil {
		t.Fatal("diagnostics toggle not respected")
	}
	d.Diagnostics = nil
	if !reflect.DeepEqual(p, d) {
		t.Error("diagnostics=true changed more than the diagnostics block")
	}
	var g api.FeatureCollection
	_ = json.Unmarshal(geo, &g)
	if !reflect.DeepEqual(g.Summary, p.Summary) || g.ReleaseID != p.ReleaseID || len(g.Features) != len(p.Villages) {
		t.Fatal("include_geometry changed summary or village set")
	}
	for i, f := range g.Features {
		if !reflect.DeepEqual(f.Properties, p.Villages[i]) {
			t.Errorf("feature %d properties differ from village row", i)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), nil)
	poi := []map[string]any{{"id": "c", "type": "poi_count", "categories": []string{"cafe"}}}
	cases := []struct {
		name  string
		body  any
		field string // expected field error ("" = no fields expected)
		code  string
	}{
		{"not json", `{"area":`, "", "invalid_json"},
		{"unknown field", `{"area":{"province_code":"34"},"metrics":[],"bogus":1}`, "", "invalid_json"},
		{"trailing data", `{"area":{"province_code":"34"},"metrics":[{"id":"c","type":"poi_count","categories":["cafe"]}]} {}`, "", "invalid_json"},
		{"no area", map[string]any{"area": map[string]any{}, "metrics": poi}, "area", "invalid_request"},
		{"two selectors", map[string]any{"area": map[string]any{"province_code": "34", "regency_code": "3404"}, "metrics": poi}, "area", "invalid_request"},
		{"bad code", map[string]any{"area": map[string]any{"province_code": "34; DROP"}, "metrics": poi}, "area.province_code", "invalid_request"},
		{"geometry not polygon", map[string]any{"area": map[string]any{"geometry": map[string]any{"type": "Point", "coordinates": []float64{110, -7}}}, "metrics": poi}, "area.geometry", "invalid_request"},
		{"geometry open ring", map[string]any{"area": map[string]any{"geometry": map[string]any{"type": "Polygon",
			"coordinates": [][][]float64{{{110, -7}, {111, -7}, {111, -8}, {110, -8}}}}}, "metrics": poi}, "area.geometry", "invalid_request"},
		{"geometry lat/lon swapped", map[string]any{"area": map[string]any{"geometry": map[string]any{"type": "Polygon",
			"coordinates": [][][]float64{{{-7, 110}, {-7, 111}, {-8, 111}, {-7, 110}}}}}, "metrics": poi}, "area.geometry", "invalid_request"},
		{"geometry bowtie", map[string]any{"area": map[string]any{"geometry": map[string]any{"type": "Polygon",
			"coordinates": [][][]float64{{{110.30, -7.80}, {110.32, -7.82}, {110.32, -7.80}, {110.30, -7.82}, {110.30, -7.80}}}}}, "metrics": poi}, "area", "invalid_request"},
		{"no metrics", map[string]any{"area": map[string]any{"province_code": "34"}, "metrics": []any{}}, "metrics", "invalid_request"},
		{"bad metric id", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "a b", "type": "poi_count", "categories": []string{"cafe"}}}}, "metrics[0].id", "invalid_request"},
		{"duplicate metric id", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{poi[0], poi[0]}}, "metrics[1].id", "invalid_request"},
		{"unknown type", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "x", "type": "median"}}}, "metrics[0].type", "invalid_request"},
		{"unknown category", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "x", "type": "poi_count", "categories": []string{"cafe", "spaceport"}}}}, "metrics[0].categories[1]", "invalid_request"},
		{"no categories", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "x", "type": "poi_count"}}}, "metrics[0].categories", "invalid_request"},
		{"unknown raster layer", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "p", "type": "raster_sum", "layer": "population_1900", "method": "fractional"}}}, "metrics[0].layer", "invalid_request"},
		{"bad method", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "p", "type": "raster_sum", "layer": "population_2020", "method": "nearest"}}}, "metrics[0].method", "invalid_request"},
		{"classes on poi_count", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "x", "type": "poi_count", "categories": []string{"cafe"}, "classes": []int{10}}}}, "metrics[0].classes", "invalid_request"},
		{"building metric with layer", map[string]any{"area": map[string]any{"province_code": "34"},
			"metrics": []map[string]any{{"id": "b", "type": "building_count", "layer": "population_2020"}}}, "metrics[0]", "invalid_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := post(t, ts, c.body)
			if status != 400 {
				t.Fatalf("status %d, want 400: %s", status, body)
			}
			validateSchema(t, "error.schema.json", body)
			var er api.ErrorResponse
			_ = json.Unmarshal(body, &er)
			if er.Error != c.code {
				t.Errorf("error code %q, want %q", er.Error, c.code)
			}
			if c.field != "" {
				found := false
				for _, f := range er.Fields {
					found = found || f.Field == c.field
				}
				if !found {
					t.Errorf("no field error for %q: %+v", c.field, er.Fields)
				}
			}
		})
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	validateSchema(t, "error.schema.json", body)
	var er api.ErrorResponse
	_ = json.Unmarshal(body, &er)
	return er.Error
}

func TestTooManyVillages413(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), func(l *api.Limits) { l.MaxVillages = 3 })
	start := time.Now()
	status, body := post(t, ts, req(map[string]any{"province_code": "34"}, false, false))
	if status != 413 || errorCode(t, body) != "too_many_villages" {
		t.Fatalf("status %d: %s", status, body)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("413 took %v; should return before querying POIs", time.Since(start))
	}
}

func TestBodyTooLarge413(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), func(l *api.Limits) { l.MaxBodyBytes = 64 })
	status, body := post(t, ts, req(map[string]any{"province_code": "34"}, false, false))
	if status != 413 || errorCode(t, body) != "body_too_large" {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestTimeout504(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), func(l *api.Limits) { l.QueryTimeout = time.Nanosecond })
	status, body := post(t, ts, req(map[string]any{"province_code": "34"}, false, false))
	if status != 504 || errorCode(t, body) != "timeout" {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestNotReady503(t *testing.T) {
	s := api.New(api.DefaultLimits(), slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	s.SetStartupError(os.ErrNotExist)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	status, body := post(t, ts, req(map[string]any{"province_code": "34"}, false, false))
	if status != 503 || errorCode(t, body) != "not_ready" {
		t.Fatalf("aggregate: %d %s", status, body)
	}
	status, body = get(t, ts, "/healthz")
	if status != 503 || !strings.Contains(string(body), `"starting"`) || !strings.Contains(string(body), "not exist") {
		t.Fatalf("healthz: %d %s", status, body)
	}
}

// Storage disappearing mid-life: 503, no partial body.
func TestStorageGone503(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, testfixtures.Release(t), dir)
	ts, _ := newServer(t, dir, nil)
	if status, body := get(t, ts, "/healthz?deep=1"); status != 200 {
		t.Fatalf("healthz before: %d %s", status, body)
	}
	if err := os.RemoveAll(filepath.Join(dir, "village_h3")); err != nil {
		t.Fatal(err)
	}
	status, body := post(t, ts, req(map[string]any{"province_code": "34"}, false, false))
	if status != 503 || errorCode(t, body) != "storage_unavailable" {
		t.Fatalf("status %d: %s", status, body)
	}
	if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if status, _ := get(t, ts, "/healthz?deep=1"); status != 503 {
		t.Errorf("deep healthz with storage gone: %d", status)
	}
}

func TestLayersAndAreas(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), nil)
	status, body := get(t, ts, "/v1/layers")
	if status != 200 {
		t.Fatalf("layers %d %s", status, body)
	}
	validateSchema(t, "layers.schema.json", body)
	if !strings.Contains(string(body), `"coffee_shop"`) || !strings.Contains(string(body), `"h3_resolution":9`) {
		t.Errorf("layers content: %s", abbreviate(body))
	}
	status, body = get(t, ts, "/v1/areas")
	var a struct {
		Areas []query.AdminArea `json:"areas"`
	}
	_ = json.Unmarshal(body, &a)
	if status != 200 || len(a.Areas) != 1+1+3 || a.Areas[0].Level != "province" || a.Areas[4].Villages != 3 {
		t.Fatalf("areas %d: %+v", status, a.Areas)
	}
	if status, body := get(t, ts, "/healthz"); status != 200 || !strings.Contains(string(body), "fixture-1") {
		t.Errorf("healthz %d %s", status, body)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), nil)
	if status, _ := get(t, ts, "/v1/aggregate"); status != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/aggregate = %d", status)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAggregateRasterContract(t *testing.T) {
	ts, e := newServer(t, testfixtures.ReleaseWithRasters(t), nil)
	status, body := post(t, ts, map[string]any{
		"area": map[string]any{"province_code": "34"},
		"metrics": []map[string]any{
			{"id": "pop", "type": "raster_sum", "layer": "population_2020", "method": "fractional"},
			{"id": "pop_center", "type": "raster_sum", "layer": "population_2020", "method": "center"},
			{"id": "cafes", "type": "poi_count", "categories": []string{"cafe"}},
		},
		"diagnostics": true,
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	validateSchema(t, "aggregate_response.schema.json", body)
	var got api.AggregateResponse
	_ = json.Unmarshal(body, &got)
	r, _ := e.ResolveArea(context.Background(), query.Area{ProvinceCode: "34"}, 0)
	want, err := e.RasterSums(context.Background(), r, []query.RasterMetric{{ID: "pop", Layer: "population_2020", Method: "fractional"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, v := range got.Villages {
		if float64(v.Metrics["pop"]) != want.Values["pop"][v.VillageID] {
			t.Errorf("%s pop %v, engine %v", v.VillageID, v.Metrics["pop"], want.Values["pop"][v.VillageID])
		}
		total += float64(v.Metrics["pop"])
	}
	if math.Abs(total-float64(got.Summary.Totals["pop"])) > 1e-6 || got.Diagnostics.RasterWindows != 6 || got.Diagnostics.StagesMS["raster_read"] <= 0 {
		t.Errorf("summary/diagnostics: %+v %+v", got.Summary, got.Diagnostics)
	}
}

func TestRasterMetricValidation(t *testing.T) {
	ts, _ := newServer(t, testfixtures.ReleaseWithRasters(t), nil)
	area := map[string]any{"province_code": "34"}
	cases := []struct {
		name   string
		metric map[string]any
		field  string
	}{
		{"fraction of continuous layer", map[string]any{"id": "x", "type": "raster_class_fraction", "layer": "population_2020",
			"method": "fractional", "classes": []int{10}}, "metrics[0].layer"},
		{"sum of categorical layer", map[string]any{"id": "x", "type": "raster_sum", "layer": "landcover", "method": "fractional"}, "metrics[0].layer"},
		{"mean of categorical layer", map[string]any{"id": "x", "type": "raster_mean", "layer": "landcover", "method": "center"}, "metrics[0].layer"},
		{"no classes", map[string]any{"id": "x", "type": "raster_class_fraction", "layer": "landcover", "method": "fractional"}, "metrics[0].classes"},
		{"unknown class", map[string]any{"id": "x", "type": "raster_class_fraction", "layer": "landcover", "method": "fractional",
			"classes": []int{10, 11}}, "metrics[0].classes[1]"},
		{"duplicate class", map[string]any{"id": "x", "type": "raster_class_fraction", "layer": "landcover", "method": "fractional",
			"classes": []int{10, 10}}, "metrics[0].classes[1]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := post(t, ts, map[string]any{"area": area, "metrics": []map[string]any{c.metric}})
			if status != 400 {
				t.Fatalf("status %d: %s", status, body)
			}
			var er api.ErrorResponse
			_ = json.Unmarshal(body, &er)
			found := false
			for _, f := range er.Fields {
				found = found || f.Field == c.field
			}
			if !found {
				t.Errorf("no field error for %q: %+v", c.field, er.Fields)
			}
		})
	}
}

func TestClassFractionAndMeanContract(t *testing.T) {
	ts, e := newServer(t, testfixtures.ReleaseWithRasters(t), nil)
	ms := []map[string]any{
		{"id": "trees", "type": "raster_class_fraction", "layer": "landcover", "method": "fractional", "classes": []int{10}},
		{"id": "pop_mean", "type": "raster_mean", "layer": "population_2020", "method": "fractional"},
	}
	status, body := post(t, ts, map[string]any{"area": map[string]any{"province_code": "34"}, "metrics": ms, "include_geometry": true})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	validateSchema(t, "aggregate_geojson.schema.json", body)
	var got api.FeatureCollection
	_ = json.Unmarshal(body, &got)
	r, _ := e.ResolveArea(context.Background(), query.Area{ProvinceCode: "34"}, 0)
	want, err := e.RasterSums(context.Background(), r, []query.RasterMetric{
		{ID: "trees", Layer: "landcover", Method: "fractional", Stat: query.StatClassFraction, Classes: []int64{10}},
		{ID: "pop_mean", Layer: "population_2020", Method: "fractional", Stat: query.StatMean}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Features {
		for _, m := range []string{"trees", "pop_mean"} {
			if float64(f.Properties.Metrics[m]) != want.Values[m][f.ID] {
				t.Errorf("%s %s = %v, engine %v", f.ID, m, f.Properties.Metrics[m], want.Values[m][f.ID])
			}
		}
	}
	// Totals of means and fractions are coverage-weighted, not sums.
	for _, m := range []string{"trees", "pop_mean"} {
		if float64(got.Summary.Totals[m]) != want.Totals[m] {
			t.Errorf("total %s = %v, engine %v", m, got.Summary.Totals[m], want.Totals[m])
		}
	}
	if tr := float64(got.Summary.Totals["trees"]); tr <= 0 || tr >= 1 {
		t.Errorf("trees total %v not a fraction", tr)
	}
}

func TestBuildingsContract(t *testing.T) {
	ts, e := newServer(t, testfixtures.Release(t), nil)
	body := map[string]any{"area": map[string]any{"district_code": "340401"}, "diagnostics": true,
		"metrics": []map[string]any{{"id": "n", "type": "building_count"}, {"id": "a", "type": "building_area"}}}
	for i, want := range []string{"computed", "cached"} {
		status, b := post(t, ts, body)
		if status != 200 {
			t.Fatalf("status %d: %s", status, b)
		}
		validateSchema(t, "aggregate_response.schema.json", b)
		var got api.AggregateResponse
		_ = json.Unmarshal(b, &got)
		r, _ := e.ResolveArea(context.Background(), query.Area{DistrictCode: "340401"}, 0)
		eng, err := e.Buildings(context.Background(), r, &query.Trace{NoCache: true})
		if err != nil {
			t.Fatal(err)
		}
		var n, a float64
		for _, v := range got.Villages {
			if float64(v.Metrics["n"]) != float64(eng.Count[v.VillageID]) || float64(v.Metrics["a"]) != eng.Area[v.VillageID] {
				t.Errorf("%s: %v/%v, engine %v/%v", v.VillageID, v.Metrics["n"], v.Metrics["a"], eng.Count[v.VillageID], eng.Area[v.VillageID])
			}
			n, a = n+float64(v.Metrics["n"]), a+float64(v.Metrics["a"])
		}
		if n == 0 || n != float64(got.Summary.Totals["n"]) || math.Abs(a-float64(got.Summary.Totals["a"])) > 1e-6 {
			t.Errorf("totals %+v, sums %v %v", got.Summary.Totals, n, a)
		}
		d := got.Diagnostics
		switch {
		case d == nil || d.Buildings == nil:
			t.Fatalf("no building diagnostics")
		case i == 0 && (d.Buildings.VillagesComputed != 2 || d.Buildings.CellBuildings+d.Buildings.BoundaryPoints == 0):
			t.Errorf("%s: %+v", want, d.Buildings)
		case i == 1 && (d.Buildings.VillagesComputed != 0 || d.Cache.ResultHits != 2):
			t.Errorf("%s: %+v cache %+v", want, d.Buildings, d.Cache)
		}
	}
}

func TestValueJSON(t *testing.T) {
	b, _ := json.Marshal(map[string]api.Value{"x": api.Value(math.NaN()), "y": 1.5})
	if string(b) != `{"x":null,"y":1.5}` {
		t.Errorf("marshal %s", b)
	}
	var v map[string]api.Value
	if err := json.Unmarshal(b, &v); err != nil || !math.IsNaN(float64(v["x"])) || v["y"] != 1.5 {
		t.Errorf("unmarshal %v %v", v, err)
	}
}

func TestGzip(t *testing.T) {
	ts, _ := newServer(t, testfixtures.Release(t), nil)
	raw, _ := json.Marshal(req(map[string]any{"province_code": "34"}, false, true))
	rq, _ := http.NewRequest("POST", ts.URL+"/v1/aggregate", bytes.NewReader(raw))
	rq.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(rq) // no transparent decompression
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("no gzip: %v", resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	validateSchema(t, "aggregate_geojson.schema.json", body)
}
