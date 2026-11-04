# Verification spikes: findings and decisions

Before building, five open questions about data sources and tooling were checked with a throwaway program (`cmd/spike`) and desk research. Run date: 2026-11-04. Host: macOS arm64, Docker Desktop 29.9.2, Go 1.27.3.

Reproduce: `make tidy && make spike` (code in `cmd/spike`, stack in `deploy/docker-compose.yml`, profile `spike`).

| # | Question | Answer | Decision |
|---|---|---|---|
| 1 | Village boundary source: coverage and license | HDX has national admin 4 | HDX COD-AB Indonesia, admin 4, CC BY-IGO (§1) |
| 2 | Can the `cog` extension return pixel windows? | **Yes** | Use `cog` for pixel reads; compute fractional coverage in Go. No custom COG reader. |
| 3 | Do `duckdb-go` and community extensions load in Docker? | **Yes** | Pin DuckDB **1.5.5** / duckdb-go **v2.10505.0** |
| 4 | Does `httpfs` do path-style range reads against RustFS? | **Yes** | Pin RustFS **1.0.4** |
| 5 | What does DuckDB profiling expose for diagnostics? | Timings, rows, memory, row groups | See §5 |

## Pinned versions

| Component | Version | Notes |
|---|---|---|
| DuckDB | 1.5.5 | Only version with a `cog` community build (§3) |
| duckdb-go | `github.com/duckdb/duckdb-go/v2 v2.10505.0` | Bundles libduckdb 1.5.5 (`v2.10504.0` = 1.5.4) |
| spatial | `eb1e57c` | core repo |
| httpfs | `827222f` | core repo |
| h3 | `v1.5.5` | community repo |
| cog | `1d210fd` | community repo |
| RustFS | `rustfs/rustfs:1.0.4` | digest `sha256:ed9210d6…93dcc2` (the tag `v1.0.4` does not exist) |
| GDAL (prep only) | `ghcr.io/osgeo/gdal:ubuntu-small-3.10.3` | |
| Go build image | `golang:1.26-bookworm`, runtime `debian:bookworm-slim` | Go 1.26 because minio-go requires it |

## 1. Village boundary source

Checked 2026-11-04 against primary APIs where reachable.

| Source | Village level? | License | Usable? |
|---|---|---|---|
| **HDX COD-AB Indonesia** (`cod-ab-idn`, source BPS, via WFP/OCHA ROAP) | **Yes: admin 0–4, 81,912 admin-4 units, national** | **CC BY-IGO** (`cc-by-igo`) | **Yes** |
| BIG geodatabase *batas wilayah administrasi desa/kelurahan* (ed. Sep 2023) on tanahair.indonesia.go.id | Yes, most authoritative and newest | **No explicit license found.** The portal requires registration and has access classes; its CKAN API returned no data from here. | Not without written terms from BIG |
| BPS WebGIS / Satu Data | Yes (2014/2019 copies circulate) | No official redistribution terms found, only third-party mirrors | No |
| geoBoundaries IDN | **No**: API has ADM0–ADM2 only (ADM3/ADM4 → 404). ADM2 is CC BY 3.0 IGO (BPS/WFP), ADM0–1 ODbL (OSM) | n/a | No |
| GADM-derived | Incomplete; GADM forbids redistribution | Restrictive | No |

Some mirrors still describe the HDX dataset as "admin 0–3 only"; the HDX record itself was updated on 2026-06-23 to levels 0–4.

**HDX COD-AB details** (`package_show?id=cod-ab-idn`):
- Counts: 34 provinces, 522 regencies/cities, 7,069 districts, 81,912 villages.
- Boundaries created by the source on 2020-04-08; reviewed by OCHA 2025-10-30; HDX resources updated 2026-06-23; yearly update cadence.
- Files: GDB zip (219 MB), SHP zip (498 MB), GeoJSON zip (456 MB), XLSX attribute table (14 MB).
- Attribution required (CC BY-IGO). This allows bundling a one-province sample in the repo and downloading on demand in prep.

**Caveats, and how prep handles them** (adapter `hdx_cod_ab`, docs/prep.md):
- **2020 vintage, 34 provinces**: before the 2022 Papua split (now 38 provinces); Kemendagri currently lists ~84k villages vs 81,912 here. Releases record `boundary_version = "hdx-cod-ab-2020"`, and the README says so.
- **Not edge-matched** (no COD-EM): overlaps and gaps between villages are possible. Prep's overlap and gap QC measures them (DIY: none).
- **P-codes** carry an `ID` prefix (`ID3404010001`); prep strips it to get BPS codes for `village_id` and the admin codes.
- **Alternate names** after `/` or `\` in some admin-3/4 names: prep keeps the text before the separator as the name.

**Decision:** HDX COD-AB Indonesia admin 4 (`boundary_version = hdx-cod-ab-2020`). BIG remains a possible upgrade if BIG confirms reuse terms in writing; the code is source-agnostic through adapters and `boundary_version`.

## 2. `cog` extension: pixel windows work

Run against a 2048×2048 Float32 COG fixture (all 1.0, 512 tiles, DEFLATE, overviews) on RustFS:

| Call | Result | Time |
|---|---|---|
| `RS_BandAsArray(path, 1, [xmin, ymin, xmax, ymax])` | flat `DOUBLE[]`, 10,404 values (102×102) for a 0.01° bbox | 3 ms |
| `RS_Value(path, lon, lat)` / `RS_Values(path, lons[], lats[])` | point samples | 2–3 ms |
| `RS_ZonalStats(path, bbox, band, 'sum')` | 10404.0 (bbox only: no polygon, no fractions) | 3 ms |
| `RS_Width/Height`, `RS_BandNoDataValue`, `RS_GeoReference` | 2048×2048, -99999.0, world file | ~0 ms |
| `read_cog(path)` | tile grid: `id, level, tile_x, tile_y, cols, rows, bbox STRUCT, crs` | |

Signatures: every `RS_*` function takes the raster as a `VARCHAR` path. `RS_BandAsArray(VARCHAR, INTEGER[, DOUBLE[] bbox])`, `RS_Values(VARCHAR, DOUBLE[], DOUBLE[][, INTEGER band])`, `RS_ZonalStats(VARCHAR, DOUBLE[], INTEGER, VARCHAR)`.

Things the raster path has to handle (all handled; see docs/raster.md):
- **`cog` does not use DuckDB S3 secrets.** It has its own Rust `object_store` client configured from env vars. Without them it calls the EC2 metadata endpoint (`169.254.169.254`) and fails after ~2 s × 10 retries. Required: `AWS_ENDPOINT`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `AWS_ALLOW_HTTP=true`, `AWS_VIRTUAL_HOSTED_STYLE_REQUEST=false`. The query engine sets them from its S3 config; `cog` requests don't appear in `duckdb_logs`.
- **Windows come back flat, with no shape or origin.** A 0.01° box over 0.0009765625° pixels (102.4 px) returned 102 per side: `cog` rounds bbox edges to the nearest pixel edge. The engine therefore always requests exact pixel-edge bounds.
- `RS_GeoReference` prints 6 decimals (`0.000098`), which is lossy. Prep records the full-precision geotransform in each raster's companion JSON.

## 3. duckdb-go and extensions in Docker

- **DuckDB 2.0.0** (duckdb-go `v2.20000.0`, the latest): spatial, httpfs and h3 load, but **`cog` has no build** (HTTP 404 on linux_arm64, linux_amd64 and osx_arm64).
- `cog` builds exist **only for v1.5.5** (checked 2.0.0, 1.5.6, 1.5.5, 1.4.5 and 1.4.4). h3 and spatial also exist for 1.5.5. Before upgrading, check that `community-extensions.duckdb.org/<ver>/<platform>/cog.duckdb_extension.gz` exists.
- Image pattern: extensions are installed at build time into `DUCKDB_EXT_DIR=/opt/duckdb_ext`; at runtime `autoinstall_known_extensions=false` and `autoload_known_extensions=false`, and all four load in 7–80 ms.
- `go get duckdb-go@<older>` alone does not downgrade the bundled libduckdb (MVS keeps the newer `libduckdb`/bindings modules). Reset `go.mod` and re-resolve when changing versions.

## 4. httpfs and RustFS

- Secret: `TYPE s3, ENDPOINT 'rustfs:9000', URL_STYLE 'path', USE_SSL false, REGION 'us-east-1'`. Nothing else needed.
- `COPY … TO 's3://…' (FORMAT parquet, ROW_GROUP_SIZE 100000)`: 1M rows with point geometry, 10.4 MB, in 300–400 ms.
- Range reads confirmed: the HTTP log shows `HEAD`, then `GET` with `Range: bytes=10440619-10457002` (the footer), then row-group ranges.
- **DuckDB's external file cache hides repeat GETs**: after a full scan only `HEAD`s appear. Use `SET enable_external_file_cache=false` when measuring cold I/O.
- `ST_Covers` and H3 functions work on data read from S3.

## 5. Diagnostics: what DuckDB exposes

Per query, from profiling (`profiling_mode='detailed'`):

- total time, CPU time, rows scanned
- row groups scanned vs to scan (shows pruning: **1 of 10** for a selective query)
- bytes read and written (local file I/O only; see below)
- peak buffer memory, blocked thread time, temp directory size
- planner, physical planner and per-optimizer-rule timings
- the operator tree with per-node timing, rows, row groups and `extra_info` (filters, projections, files)

From `CALL enable_logging('HTTP')` → `duckdb_logs`: method, URL, `Range` header, status, `content-length` and duration per request (credentials redacted), for httpfs only, and per database rather than per query.

The API reads these per query with `duckdb.GetProfilingInfo` and reports them per stage (docs/caching.md). Limitation: bytes read is 0 for S3/httpfs and `cog` reads, so per-request network bytes are not available.

## Related findings elsewhere

- `cog` window semantics (rounding to pixel edges, nodata → NULL, clipping to the extent) and the **linux_arm64 build without a tile cache**: docs/raster.md.
- DuckDB 1.5 **CRS-typed geometry** (EPSG:4326 vs OGC:CRS84); DuckDB spatial's `ST_Read` missing FileGDB attributes and crashing on unknown layers; h3-go and DuckDB `h3` disagreeing on points exactly on cell edges: docs/prep.md.
