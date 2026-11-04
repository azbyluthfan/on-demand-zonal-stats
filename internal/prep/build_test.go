package prep_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"

	"on-demand-zonal-stats/internal/prep"
	"on-demand-zonal-stats/internal/testfixtures"
)

func repoRoot() string { return filepath.Join(testfixtures.Dir(), "..", "..") }

func buildFixture(t *testing.T) (string, *prep.Manifest) {
	t.Helper()
	root := repoRoot()
	wd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil { // config paths are repo-relative
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	cfg, err := prep.LoadConfig("configs/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	m, err := prep.Build(context.Background(), cfg, out, prep.Options{SkipRasters: true})
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(out, "releases", cfg.ReleaseID), m
}

func TestBuildFixtureRelease(t *testing.T) {
	dir, m := buildFixture(t)

	if m.Status != "ok" {
		t.Fatalf("status %q", m.Status)
	}
	wantArtifacts := map[string]int64{
		"boundaries/villages.parquet":                               6,
		"village_h3/province_code=34/part-0.parquet":                -1,
		"pois/release=fixture-1/province_code=34/part-0.parquet":    -1,
		"pois/release=fixture-1/province_code=_none/part-0.parquet": -1,
		"categories.json": -1,
	}
	got := map[string]prep.Artifact{}
	for _, a := range m.Artifacts {
		got[a.Path] = a
	}
	for p, rows := range wantArtifacts {
		a, ok := got[p]
		if !ok {
			t.Errorf("missing artifact %s (have %v)", p, keys(got))
			continue
		}
		if rows >= 0 && (a.Rows == nil || *a.Rows != rows) {
			t.Errorf("%s rows = %v, want %d", p, a.Rows, rows)
		}
		if a.SHA256 == "" || a.Bytes == 0 {
			t.Errorf("%s missing checksum/size", p)
		}
	}
	// manifest.json on disk round-trips.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk prep.Manifest
	if err := json.Unmarshal(raw, &disk); err != nil || disk.ReleaseID != "fixture-1" {
		t.Fatalf("manifest.json: %v %+v", err, disk.ReleaseID)
	}
	for _, q := range m.QC {
		if q.Blocking && !q.Passed {
			t.Errorf("blocking QC %s failed: %v %s", q.Name, q.Value, q.Error)
		}
	}
}

// Reads the artifacts back the way serving will (H3 interior/boundary
// split) and checks every hand-placed edge case from gen_fixtures.py.
func TestFixtureEdgeCasesFromArtifacts(t *testing.T) {
	dir, _ := buildFixture(t)
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{"INSTALL spatial", "LOAD spatial"} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	p := func(rel string) string { return "'" + filepath.Join(dir, rel) + "'" }
	q := `WITH pois AS (SELECT * FROM read_parquet(` + p("pois/release=fixture-1/*/part-0.parquet") + `)),
		h AS (SELECT * FROM read_parquet(` + p("village_h3/*/part-0.parquet") + `)),
		v AS (SELECT * FROM read_parquet(` + p("boundaries/villages.parquet") + `)),
		cand AS (SELECT pois.poi_id, h.village_id, h.cell_type, pois.geometry AS pg FROM pois JOIN h USING (h3_cell))
		SELECT pois.poi_id, coalesce(
			(SELECT any_value(village_id) FROM cand WHERE cand.poi_id = pois.poi_id AND cell_type = 'interior'),
			(SELECT min(cand.village_id) FROM cand JOIN v USING (village_id)
			 WHERE cand.poi_id = pois.poi_id AND cell_type = 'boundary' AND ST_Covers(v.geometry, cand.pg)),
			'none')
		FROM pois WHERE poi_id <= 'P0012' ORDER BY 1`
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]string{
		"P0001": "V001", "P0002": "V001", "P0003": "V001", "P0004": "V001", "P0005": "V001", // on V001|V002 border
		"P0006": "V001", // shared corner
		"P0007": "none", // in V004 hole
		"P0008": "V004", // on hole edge
		"P0009": "V005", // tiny village
		"P0010": "V006", // second island
		"P0011": "none", // between islands
		"P0012": "none", // far away
	}
	n := 0
	for rows.Next() {
		var id, village string
		if err := rows.Scan(&id, &village); err != nil {
			t.Fatal(err)
		}
		n++
		if want[id] != village {
			t.Errorf("%s assigned to %s, want %s", id, village, want[id])
		}
	}
	if n != len(want) {
		t.Errorf("checked %d edge-case POIs, want %d", n, len(want))
	}
}

func keys(m map[string]prep.Artifact) []string {
	var k []string
	for p := range m {
		k = append(k, p)
	}
	return k
}
