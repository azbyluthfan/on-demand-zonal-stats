# Fixtures

Tiny synthetic data for unit and golden tests. Everything here is hand-made; no real data.

- `villages.geojson`: six villages near Yogyakarta (lon ~110.3–110.5, lat ~-7.8), EPSG:4326, all in province `34`, regency `3404`.
  - `V001`/`V002`: adjacent squares sharing the `x = 110.32` border (edge rule).
  - `V003`: concave L-shape.
  - `V004`: square with a square hole.
  - `V005`: triangle smaller than one H3 res-9 cell (no interior cells possible).
  - `V006`: multipolygon of two disjoint squares.
- `pois.geojson`: 512 points. The first 12 are hand-placed edge cases (on the V001|V002 border and corner, in V004's hole and on its edge, inside V005, between V006's parts, far away; see the `note` property); the rest are random, with five categories.
- `population.asc`: 200 × 100 ESRI ASCII grid of 0.001° pixels over lon 110.28–110.48, lat −7.88 to −7.78. Values vary per pixel by a known formula (so window position and orientation can be checked exactly); the top-left 10 × 10 block is nodata.
- `gen_fixtures.py`: regenerates `pois.geojson` and `population.asc` deterministically (`python3 -I testdata/fixtures/gen_fixtures.py`).

`configs/fixture.json` builds a release from these files (`make prep-fixture`). Reference results from exactextract and rasterstats are in `../golden/`.
