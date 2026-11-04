# HTTP API

`cmd/api` + `internal/api`. JSON in, JSON out. Response contracts live in `schemas/` (JSON Schema 2020-12), and the tests validate every response against them.

```
docker compose up -d   # RustFS + seed + api1 (:8081) + api2 (:8082) + load balancer (:8080)
make api-local         # one API on :8080 reading sample/releases/diy-2026-11-a directly
```

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | `200 {"status":"ok","release_id"}` once the release is loaded. `503 {"status":"starting","error"}` before that. `?deep=1` also checks storage is reachable now (`503 "degraded"` if not). |
| `GET /v1/layers` | Release ID, boundary version, H3 resolution, POI release, categories with counts, raster layers, sources and licenses (`schemas/layers.schema.json`). |
| `GET /v1/areas` | Province/regency/district codes and names with village counts, for the frontend's area picker. |
| `POST /v1/aggregate` | Per-village aggregates for an area (`schemas/aggregate_response.schema.json`, or `aggregate_geojson.schema.json` with `include_geometry`). |

### `POST /v1/aggregate`

```json
{
  "area": { "regency_code": "3471" },
  "metrics": [
    { "id": "cafes", "type": "poi_count", "categories": ["cafe", "coffee_shop"] },
    { "id": "pop", "type": "raster_sum", "layer": "population_2020", "method": "fractional" }
  ],
  "include_geometry": false,
  "diagnostics": true
}
```

- `area`: exactly one of `province_code`, `regency_code`, `district_code`, or `geometry` (GeoJSON Polygon/MultiPolygon in lon/lat; villages intersecting it).
- Each village row: `village_id, village_name, district_code, district_name, regency_code, regency_name, area_m2, metrics`. Every requested metric is present, zeros included. Villages are sorted by `village_id`.
- `summary`: `village_count` and `totals` per metric.
- `include_geometry: true` returns a GeoJSON FeatureCollection (`application/geo+json`). Each feature has `id = village_id` and the same properties; `release_id`, `boundary_version`, `summary` and `diagnostics` are foreign members.
- `diagnostics: true` adds a block explaining where time and work went:
  - `total_ms` and `stages_ms`: `resolve_area, reference, h3_cells, poi_scan, poi_interior, poi_boundary, raster_geometry, raster_read, raster_coverage, assemble` (absent when not needed, e.g. on cache hits)
  - POI split: `partitions`, `selected_cells`, `candidate_cell_rows`, `poi_villages_computed`, `points_scanned`, `points_interior`, `points_boundary`, `points_boundary_tested`, `points_other_village` (boundary POIs won by a neighbour outside the area), `unassigned_pois` per metric
  - raster: `raster_windows`, `raster_blocks`, `raster_pixels_read`, `raster_pixels_weighted`, `raster_nodata_weight`
  - `cache`: `result_hits/misses`, `block_hits/misses`, `reference_hits/misses`
  - `duckdb_profile`: DuckDB's own profiler per stage (latency, CPU, rows scanned, peak buffer memory, queries)

  Details: docs/caching.md (caches, profile) and docs/raster.md (raster path).
- `Cache-Control: no-cache` bypasses every cache (the cold path); results are identical.
- Responses are gzip-compressed when the client sends `Accept-Encoding: gzip` (province GeoJSON: 6.2 MB → 1.7 MB).

### Errors

Every non-2xx body is `{"error": <code>, "message": ..., "fields": [{"field", "message"}]?}` (`schemas/error.schema.json`).

| Condition | Status | `error` |
|---|---|---|
| Malformed JSON, unknown field, trailing data | 400 | `invalid_json` |
| Validation (area selectors, codes, geometry type/closure/range/validity, metric ids, unknown category or layer, bad method) | 400 | `invalid_request`, with field paths such as `metrics[0].categories[1]` |
| Body > 2 MB | 413 | `body_too_large` |
| Area > `MAX_VILLAGES` (10,000) | 413 | `too_many_villages`; returned after reading only the village list |
| Query > `QUERY_TIMEOUT` (30 s) | 504 | `timeout`; the context cancels the DuckDB query |
| Release storage unreachable / artifact missing | 503 | `storage_unavailable`; no partial results |
| Release not loaded yet | 503 | `not_ready` |

## Operation

- Configuration is via env vars (see the `cmd/api/main.go` header): `RELEASE_ID` or `RELEASE_BASE`, `S3_*`, `DUCKDB_MEMORY_LIMIT` (2GB), `DUCKDB_THREADS` (4), `MAX_VILLAGES` (10,000), `QUERY_TIMEOUT` (30s), `WEB_DIR`, and cache sizes `RESULT_CACHE_ENTRIES`, `BLOCK_CACHE_MB`, `REF_CACHE_PROVINCES`, `CACHE_DISABLED`.
- The HTTP server starts immediately. The release loads in the background with exponential backoff (1 s → 30 s) and each failure is logged, so a replica started without storage reports `starting` instead of crash-looping.
- Image: `deploy/Dockerfile.api` (Debian slim, non-root, extensions baked into `/opt/duckdb_ext`, Docker `HEALTHCHECK` via `api -healthcheck`). Compose runs `api1`/`api2` with `mem_limit: 3g` behind nginx round-robin (`deploy/nginx.conf`, port 8080), which also serves the frontend.
- Logs: JSON (slog), one line per request (healthy `/healthz` probes are skipped).

## Resilience and end-to-end checks (2026-11-04, Docker Desktop, release `diy-2026-11-a` on RustFS)

- Kota Yogyakarta (`regency_code 3471`, 45 villages), cafes + schools with diagnostics: **276 ms** cold (before caching; warm is ~5 ms, docs/caching.md). api1 and api2 return identical bodies apart from diagnostics.
- Drawn polygon around Tugu: 11 villages as GeoJSON.
- RustFS stopped: `POST /v1/aggregate` → 503 `storage_unavailable` (2.5 s, httpfs retries) and `/healthz?deep=1` → 503 `degraded`. A replica restarted with RustFS down → `/healthz` 503 `starting`, retries logged. RustFS restarted → both replicas serve 200 within ~2 s.
