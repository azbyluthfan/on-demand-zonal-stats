# Prep pipeline

`cmd/prep` builds a data release and runs its quality checks (QC). It runs in the `prep` Docker image (Go + DuckDB 1.5.5 + GDAL 3.10.3).

```
make prep-fixture                      # synthetic release, no downloads
make prep-diy                          # DI Yogyakarta, needs data/raw (below)
make prep-upload RELEASE=diy-2026-11-a # copy to s3://villagestat/releases/<id>/
```

Configs live in `configs/`. Output goes to `out/releases/<release_id>/`. `upload` refuses a release whose manifest status is not `ok`, and it uploads `manifest.json` last. Blocking QC failures stop the release: geometry validity, unique IDs, required fields, overlaps (< 0.1% of total area), H3 coverage, POI assignment (including an exact cross-check against a naive join when `cross_check_pois` is on), h3-go/DuckDB H3 consistency and COG validity. Gaps, raster sums and H3 calibration are informational.

## Steps

1. **Boundaries**: adapter, then `ST_SetCRS(…, 'EPSG:4326')`, repair (`ST_MakeValid` + polygon extract), `area_m2` in EPSG:6933, the `bbox` struct, QC, Hilbert sort, `boundaries/villages.parquet`.
2. **village_h3**: Go `internal/h3index` classifies each village in parallel. `Merge` demotes any shared cell. Rows go into DuckDB through the appender, then out to one partition per province, sorted by `h3_cell`.
3. **POIs**: adapter, then `h3_cell` from DuckDB's `h3`. Exact assignment uses the serving algorithm (interior cell, or `ST_Covers` against the boundary-cell candidates, lowest `village_id` wins). Partition key is the province of the assigned village; unassigned points in a boundary cell use the lowest candidate province, and points with no candidate go to `_none`. Then `categories.json`.
4. **Rasters**: `gdal_translate -of COG` (512 blocks, DEFLATE, predictor, overviews), optionally cropped to the villages' extent. The exact sum comes from reading the COG back through the `cog` extension in pixel-aligned 2048² tiles. Each tile must return exactly `cols × rows` values; this guards the serving read path. Companion `<id>.json` holds the full-precision geotransform.
5. **Manifest**: tools and versions, sources (sha256 when local), artifacts (bytes, sha256, Parquet row counts), raster info, and all QC results.

## Adapters

| Adapter | Notes |
|---|---|
| `geojson` (boundaries/POIs) | Columns already named as in the output. Used by fixtures. |
| `hdx_cod_ab` | Runs `ogr2ogr` to FlatGeobuf first (filtered to `adm1_pcode IN ('ID<code>')`), because **DuckDB spatial 1.5.5 `ST_Read` sees only `OGC_FID` + `geometry` in the HDX FileGDB**. GDAL 3.10 reads it fine. Codes drop the `ID` prefix (`ID3404010001` → `3404010001`); the original is kept as `source_pcode` in the work table. Names keep the text before any `/` or `\`. Note that ST_Read on a non-existent FileGDB layer **segfaults** DuckDB 1.5.5. |
| `overture` | Reads `s3://overturemaps-us-west-2/release/<rel>/theme=places/type=place/*` anonymously, filtered by the villages' bbox ± 0.01°. **The 2026 schema has no `categories`**: we use `taxonomy.primary` (there is also a coarser `basic_category`). Places with `operating_status = 'permanently_closed'` are skipped. The secret needs explicit empty keys, or the `AWS_*` env vars set for `cog` leak in. |

## Gotchas

- **CRS-typed geometry (DuckDB 1.5).** HDX FGB loads as `GEOMETRY('EPSG:4326')` and Overture as `GEOMETRY('OGC:CRS84')`, and `ST_Covers` refuses to mix them. All inputs are WGS84 lon/lat, so prep labels everything `EPSG:4326` with `ST_SetCRS` (no transform). Serving does the same for user-drawn geometry.
- **h3-go vs DuckDB `h3` disagree on edge points.** They bundle different H3 C versions. At res 9 there were 0 mismatches in 100k points; at res 10 there was 1, and at res 11 there were 3. Every mismatching point lies **≤ 1.1e-10°** from the shared cell edge. That is safe, because interior cells keep a 1e-7° margin from village edges, so a point on an interior cell's edge is still inside the village. The QC check (`h3_go_duckdb_consistency`) fails only if a disagreement is more than 1e-8° from an edge.
- **WorldPop does not serve HTTP ranges to GDAL** (`/vsicurl/` fails with "Range downloading not supported"), so the raster is downloaded first.
- **HDX downloads** redirect (302) to a signed S3 URL. Plain `curl -L` works.

## Real-data inputs (`data/raw/`, gitignored)

| File | Source | Size | sha256 |
|---|---|---|---|
| `hdx/idn_admin_boundaries.gdb.zip` (unzipped next to it) | [HDX cod-ab-idn](https://data.humdata.org/dataset/cod-ab-idn), GDB resource | 218,753,706 B | `640c6239…e308` |
| `worldpop/idn_ppp_2020_constrained.tif` | `https://data.worldpop.org/GIS/Population/Global_2000_2020_Constrained/2020/BSGM/IDN/idn_ppp_2020_constrained.tif` | 103,102,616 B | `352f5094…cb0a` |

Overture is read in place (release `2026-10-21.0`).

## DI Yogyakarta release `diy-2026-11-a`

Built 2026-11-04 in 24 s. All QC passed. This release is bundled in `sample/` and seeded into RustFS by `docker compose up`.

| Artifact | Rows | Bytes |
|---|---|---|
| `boundaries/villages.parquet` | 438 | 1.35 MB |
| `village_h3/province_code=34/part-0.parquet` (res 10) | 256,673 | 375 KB |
| `pois/release=2026-10-21.0/province_code=34/part-0.parquet` | 67,742 | 5.7 MB |
| `pois/release=2026-10-21.0/province_code=_none/part-0.parquet` | 36,915 | 3.1 MB |
| `rasters/population_2020.tif` (1002×799, 3″ ≈ 92 m, Float32, nodata −99999) | | 1.26 MB |
| `categories.json`, `rasters/population_2020.json`, `manifest.json` | | |

- **Boundaries**: 438 villages, 78 districts, 5 regencies. 0 repaired, 0 overlaps, 0 interior gaps. Areas look right (Kota Yogyakarta 33 km², Gunung Kidul 1,474 km²).
- **POIs**: 108,142 in the bbox; 3,485 dropped (no category); 104,657 kept. 67,650 fall in DIY villages (51,638 via interior cells, 16,012 via 33,611 exact tests). The 37,007 unassigned are mostly the neighbouring Central Java area inside the bbox; 92 of them sit in DIY villages' cells and are kept in the province partition. The naive `ST_Covers` cross-check had **0 mismatches**.
- **Raster**: the cropped COG sums to 6,829,236 people (the crop includes part of Central Java). Over DIY's villages the fractional total is 3,848,726 (docs/raster.md); the BPS 2020 census gives ~3.67 M.

### H3 resolution calibration → **res 10**

Same DIY inputs, rebuilt per resolution:

| Res | village_h3 rows | Size | POIs on interior path | Exact `ST_Covers` tests | Interior share by village size (<1 / 1–5 / 5–20 / ≥20 km²) |
|---|---|---|---|---|---|
| 8 | 9,161 | 18 KB | 10,635 | 181,232 | 0 / 0.08 / 0.24 / 0.46 |
| 9 | 44,046 | 74 KB | 33,969 | 77,254 | 0.17 / 0.48 / 0.63 / 0.77 |
| **10** | 256,673 | 375 KB | 51,638 | 33,611 | 0.56 / 0.75 / 0.85 / 0.90 |
| 11 | 1,655,196 | 1.5 MB | 59,897 | 15,777 | 0.76 / 0.88 / 0.93 / 0.94 |

Res 10 more than halves the exact tests compared with res 9, and fixes small urban villages (17% → 56% interior). Its table stays small (~70 MB nationally, split by province). Res 11 is 4× larger for diminishing gains. The fixture release stays at res 9, which its unit tests use.

## Limitations

- **Only DI Yogyakarta has been built.** A national build has not been run. Things to watch: `ST_Union_Agg` for the gaps check over 82k villages, the overlap self-join, and memory (`PREP_MEMORY_LIMIT`, default 6 GB).
- **Source-stated raster total.** The raster `expected_total` check (sum within 1% of the source's stated total) needs the WorldPop national figure; it is not set for the DIY crop.
