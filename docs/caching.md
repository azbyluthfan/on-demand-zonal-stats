# Caching and diagnostics

Code: `internal/query/{cache,refcache,trace}.go`. Every replica has its own caches; nothing is shared, and nothing persists across restarts.

## Caches

| Cache | Key → value | Sizing (env) | Notes |
|---|---|---|---|
| **Result: POI** | (village, category) → (count, unassigned) | `RESULT_CACHE_ENTRIES` (2,000,000) | Per **category**, not per category set: any combination of cached categories is a sum, so `["cafe"]` then `["cafe","coffee_shop"]` reuses cafe. A request computes only the villages × categories it misses. |
| **Result: buildings** | village → (count, footprint area, unassigned) | ENTRIES / 10 | `building_count` and `building_area` share one entry. |
| **Result: raster** | (village, layer, method) → sum, weights, per-class weights (categorical) | ENTRIES / 10 | Deterministic, so reused by any area containing the village. Sum, mean and every class fraction of a layer reduce the same entry, so asking for another statistic or class set is a hit. |
| **Decoded blocks** | (layer, block x, block y) → 512×512 float64 (any stored type) | `BLOCK_CACHE_MB` (256) | Cache misses for new villages or another method skip the cog read and decode. |
| **Reference** | province → DuckDB tables `ref_h3_*` (village_h3, sorted by cell) and `ref_v_*` (village geometries), plus decoded Go geometries | `REF_CACHE_PROVINCES` (4) | Loaded on first use, shared by concurrent first requests, LRU-evicted only when idle (never under a running query). |

The engine is per release, so the release ID is implicit in every key. `CACHE_DISABLED=1` turns all caches off. A request with **`Cache-Control: no-cache`** bypasses every cache, reading files and computing everything; this is how "cold" is measured.

### Unassigned POIs are attributed to a village

A POI inside some village's H3 cell that no village covers (a gap or a hole) is **attributed to the lowest-ID candidate village of its cell**, the same rule prep uses to choose its partition. That makes unassigned counts cacheable per village and additive across areas, like the village counts.

## Diagnostics (`"diagnostics": true`)

- `stages_ms`: `resolve_area, reference, h3_cells, poi_scan, poi_interior, poi_boundary, raster_geometry, raster_read, raster_coverage, assemble`. Absent stages were not needed (e.g. all cache hits).
- `cache`: `result_hits/misses` (village × category and village × layer × method lookups), `block_hits/misses`, `reference_hits/misses` (provinces).
- `poi_villages_computed`: villages actually computed (cache misses). The `points_*` counters cover the computed part only.
- `duckdb_profile`: DuckDB's own profiler (`PRAGMA enable_profiling='no_output'` per connection, read with `duckdb.GetProfilingInfo`), summed per stage: `latency_s, cpu_time_s, rows_scanned, blocked_thread_s, peak_buffer_bytes` (max), `queries`, and scan row-group counters when present.
  - **Limitation:** `bytes_read` counts only DuckDB's local file I/O. It is 0 for S3/httpfs reads and for the cog extension (its own client). Per-query HTTP bytes are not exposed. Request counts are available globally from `duckdb_logs`, but not per query.

## Correctness

- `TestCacheWarmEqualsCold`: 33 areas (codes plus overlapping random boxes) × rotating category sets, POI and raster. Cold (`NoCache`), warm, warm again, and a cache-disabled engine all give **identical** results; hits occur.
- `TestCacheConcurrent` (with `-race`): 8 goroutines × 36 requests on deliberately tiny caches (50 entries, one 2 MB block, one province) give the same answers as sequential cold runs.
- All golden and invariant tests (fixture, DIY local and RustFS, exactextract/rasterstats) run with caches on.

## Performance baseline

`make perf-baseline`: k6 on the compose network, round-robin over api1/api2, release `diy-2026-11-a` on RustFS, Docker Desktop on an Apple Silicon laptop (8 CPUs to the VM). `DUCKDB_THREADS=4`, `DUCKDB_MEMORY_LIMIT=2GB`, container limit 3 GB. Run 2026-11-04.

| Scenario | Initial target (p95) | p50 | **p95** | p99 |
|---|---|---|---|---|
| Regency (Sleman, 86 villages), POI count, warm | < 300 ms | 3.3 ms | **5.2 ms** | 8.6 ms |
| Regency, fractional population, **cold** (`no-cache`) | < 3 s | 82 ms | **92 ms** | 105 ms |
| Province (438 villages), cafes + fractional + center, warm | < 1 s | 5.0 ms | **5.9 ms** | 6.9 ms |
| 20 concurrent users, mixed, 5 min | no errors; memory < limit + 20% | | **0 errors / 49,021 requests**, 121 req/s; peak RSS **499 MiB** (api1), 474 MiB (api2) | |

The mixed load was 15% province (POI + fractional), 10% cold regency with both raster methods, 20% random boxes, and 55% regency with a random category and method. Overall p95 was 655 ms, dominated by the cold and province requests. No restarts or OOM kills.

Single requests, cold vs warm (province, cafes + fractional + center): **608 ms cold → 9 ms warm** (1,752 result hits). On a first cached run the reference load costs ~70 ms once per province per replica.

### Comparisons between this project's own techniques

| Comparison | Measured |
|---|---|
| H3 split vs naive join (DIY, all categories) | 33,611 exact `ST_Covers` tests vs 67,742 × 438 ≈ 29.7 M for the naive join; interior path takes 76% of POIs with no geometry test (docs/query.md, docs/prep.md) |
| Cache cold vs warm | 608 ms → 9 ms (province, cafes + fractional + center) |
| Fractional vs center | Same read cost (shared blocks). Coverage stage ~145 ms for both methods on the province, cached afterwards. Accuracy: up to 12.8% per village, 0.007% province-wide (docs/raster.md) |

The initial targets were rough guesses set before any measurement, and DIY is small (one province). A national build would test the reference cache's memory per province (Jawa Barat has ~14× DIY's villages).
