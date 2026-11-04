// Package testfixtures loads the synthetic fixtures in testdata/fixtures.
package testfixtures

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
)

// Village is one fixture village.
type Village struct {
	ID   string
	Name string
	Geom orb.Geometry // orb.Polygon or orb.MultiPolygon
}

// Dir returns the absolute path of testdata/fixtures.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixtures")
}

// Villages loads villages.geojson, sorted by ID.
func Villages(t testing.TB) []Village {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(Dir(), "villages.geojson"))
	if err != nil {
		t.Fatal(err)
	}
	fc, err := geojson.UnmarshalFeatureCollection(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]Village, 0, len(fc.Features))
	for _, f := range fc.Features {
		out = append(out, Village{
			ID:   f.Properties.MustString("village_id"),
			Name: f.Properties.MustString("village_name"),
			Geom: f.Geometry,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// VillageByID returns the fixture village with the given ID.
func VillageByID(t testing.TB, id string) Village {
	t.Helper()
	for _, v := range Villages(t) {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("fixture village %q not found", id)
	return Village{}
}
