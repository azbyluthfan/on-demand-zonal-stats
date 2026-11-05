# Query engine

`internal/query` answers aggregation queries live from a release's files; `internal/release` holds the release layout and manifest types, shared with prep. This page covers the engine, area resolution and POI counting. Rasters are in docs/raster.md, caches and diagnostics in docs/caching.md.

## Engine (`query.Open`)

- One in-memory DuckDB per replica, no database file. Extensions `spatial, httpfs, h3, cog` are LOADed from `ExtensionDir` (baked into the image). `AllowInstall` is for dev and tests only. Then `autoinstall/autoload_known_extensions = false`, and `memory_limit` / `threads` are set globally.
- Storage: a DuckDB S3 secret scoped to `s3://<bucket>` (path-style, SSL as configured). The engine also sets the `AWS_*` env vars that the `cog` extension's own S3 client reads (docs/spikes.md §2), unless they are already set. Local release directories work too (dev and tests).
- At startup it reads `manifest.json` and `categories.json` and refuses a manifest whose status is not `ok`. Only partitions listed in the manifest are ever read.
- Errors: `ErrInvalidArea` → 400, `ErrTooManyVillages` → 413, `ErrStorage` (HTTP/IO errors from DuckDB, missing manifest) → 503, `ErrTimeout` (context deadline) → 504. The context cancels the DuckDB query.

## Area resolution (`ResolveArea`)

- Codes: `province_code`, `regency_code` or `district_code` equality on `villages.parquet`.
- Drawn geometry: rejected unless it is a valid Polygon/MultiPolygon. It is labelled `EPSG:4326` like the release (docs/prep.md), pruned on the `bbox` struct, then tested with `ST_Intersects`.
- `limit` returns `ErrTooManyVillages` after reading only the village list (`LIMIT limit+1`).

## POI counting (`CountPOIs`)

Results are cached per (village, category), so only missing pairs are computed (docs/caching.md). The computation runs in stages on one connection with temp tables, so each stage is timed. `village_h3` rows and village geometries come from the per-province **reference cache**, or straight from the files when caches are bypassed.

| Stage | What |
|---|---|
| `h3_cells` | `q_h` = the selected villages' `village_h3` rows; `q_hb` = **every** village's rows for the selected boundary cells (the candidates) |
| `poi_scan` | `q_p` = POIs from the selected provinces' partitions where `category IN (union of all metrics)` and `h3_cell IN q_h`. One scan serves all metrics. |
| `poi_interior` | `q_p ⋈ q_h` on interior cells, grouped by village and category. No geometry. |
| `poi_boundary` | `ST_Covers(village, poi)` for each (POI, candidate village) in boundary cells; `min(village_id)` among covering villages wins (edge rule). Winners outside the area are dropped (`OtherVillage`). A POI that no village covers is **unassigned and attributed to the lowest candidate village of its cell** (the same rule prep uses for partitioning), so unassigned counts are per-village and additive across areas; it counts for the area only if that village is selected. |

**Why it is area-independent.** Candidates include unselected villages, so a POI on the border of a selected and an unselected village goes to the lowest ID whatever was asked. Partitions are keyed by the province of the village prep assigned each POI to, so reading the selected provinces sees every POI that can belong to a selected village; a POI won by a village in another province is stored in that province's partition.

## Tests

- **Fixture golden** (`query_test.go`): the reference is Go `geom.Assign` over all fixture villages, independent of DuckDB, H3 and the release files. Compared per village per metric for province/regency/district areas, hand-drawn edge cases (V002 alone must not get the border POIs, a polygon inside V004's hole selects nothing, the tiny V005) and 40 random rectangles. Also: errors (no or two selectors, limit, bowtie, LineString) and a missing manifest → `ErrStorage`.
- **DIY invariants** (`diy_test.go`, on `out/releases/diy-2026-11-b` or the bundled `sample/` copy, and on RustFS when `ZS_S3_ENDPOINT` is set):
  - Σ villages + unassigned = rows in the province partition (67,742).
  - Additivity: 5 regencies and 78 districts requested one by one give exactly the province-wide per-village counts.
  - Determinism: a repeat gives the same answer.
  - A naive `ST_Covers` join of all POIs × all villages matches every village.
- **Cache equality and concurrency** (`cache_test.go`): docs/caching.md.

## Cold-path timings (DIY, all ~820 categories, laptop, caches bypassed)

| Request | Local files | RustFS |
|---|---|---|
| Province (438 villages, 67.7k POIs) | 0.63 s | 0.42 s |
| Slowest regency | 0.25 s | 0.16 s |
| Slowest district | 74 ms | 85 ms |

Province breakdown (RustFS): h3_cells 71 ms, poi_scan 93 ms, poi_interior 19 ms, **poi_boundary 210 ms**. 51,638 POIs took the interior path; 16,104 boundary POIs needed 33,611 `ST_Covers` tests, against 67.7k × 438 for a naive join. Warm timings are in docs/caching.md.
