# Raster aggregation

`raster_sum` metrics: `Engine.RasterSums` in `internal/query/raster.go`, with the coverage maths in `internal/raster`.

```json
{ "id": "pop", "type": "raster_sum", "layer": "population_2020", "method": "fractional" }
```

- **fractional**: Σ pixel × (area(pixel ∩ village) / area(pixel)), computed exactly by polygon clipping in planar lon/lat, the same definition exactextract uses.
- **center**: Σ pixels whose centre is inside the village (half-open rule, so adjacent villages never count a pixel twice). A baseline for the accuracy view.
- Nodata (and NaN) pixels are excluded; their covered weight is reported as `raster_nodata_weight`.

## Read path

1. Results already in the result cache are reused (docs/caching.md). For the rest, village geometries come from the reference cache (or `villages.parquet` when bypassing caches).
2. Each village's window = its bbox snapped outward to pixel edges and clipped to the raster (`raster.Grid.WindowFor`).
3. **Each COG block (512×512) touched by any window is read once** with the `cog` extension: `RS_BandAsArray(path, 1, [xmin, ymin, xmax, ymax])` on the block's exact pixel-edge bounds, batched 16 blocks per statement, unless the decoded block is already cached. Village windows are cut out of the blocks in Go.
4. Coverage and weighted sums run in parallel across villages.

Diagnostics: `stages_ms.raster_geometry / raster_read / raster_coverage`, `raster_windows`, `raster_blocks`, `raster_pixels_read`, `raster_pixels_weighted`, `raster_nodata_weight`.

### `cog` extension behaviour (probed on the fixture COG, whose values follow a known formula)

| Question | Answer |
|---|---|
| Pixel order | Row-major, north to south, west to east: exactly the source values |
| bbox → pixels | **Edges round to the nearest pixel edge** (insets/outsets up to 0.49 px give the same window; a 0.6 px outset adds a pixel). That is why a 0.01° box over 0.0009765625° pixels gave 102, not 103, in the verification spike (docs/spikes.md). We always request exact pixel-edge bounds, so float noise cannot shift a window. |
| Outside the raster | Clipped to the extent; fully outside → empty array |
| Nodata | Returned as **NULL** (mapped to NaN, then excluded) |
| S3 config | Its own `object_store` client from `AWS_*` env vars, not DuckDB secrets. `query.Open` sets them from `S3Config` unless already set. |
| Local paths | Need to be absolute; the engine makes local release roots absolute. |

### Why blocks, not per-village windows

The `cog` build differs by platform at DuckDB 1.5.5. **osx_arm64 gets v0.3.0, which has a tile cache (`cog_cache_stats`); the linux_arm64 build (published 2026-07-28) is older, with no cache.** linux_amd64 (2026-08-03) appears current. Reading one window per village made the Docker (linux_arm64) API re-fetch and re-decode overlapping tiles: **4.5 s** of raster read for DIY even when warm, against 50 ms warm on macOS. Reading each block once per request brings Docker down to **~110–146 ms** (4 blocks, 800k pixels) on any platform, and makes decoded blocks the natural cache unit. Local-file COGs also bypass `cog`'s cache entirely, which only matters for dev runs.

## Correctness

| Test | Reference | Result |
|---|---|---|
| `TestRasterWindowAgainstFormula` | Closed-form fixture values (`gen_fixtures.py`) for V001, a square aligned to pixel edges | fractional = center = formula (|Δ| < 1e-6 over 400 px) |
| `TestRasterGoldenFixture` | exactextract 0.3.0 / rasterstats 0.21.0 (`testdata/golden/fixture_population_2020.csv`) | all villages pass; one documented tie (below) |
| `TestRasterGoldenDIY` (local + RustFS) | same tools on the real DIY release (`testdata/golden/diy_population_2020.csv`) | **all 438 villages**: fractional within 0.1% / 0.5 people (actual max 1.8e-8 relative); center matches rasterstats |
| Invariant: Σ villages = union | exactextract on the dissolved union of all villages | Σ fractional = 3,848,726 = union reference |
| `TestAggregateRasterContract` | API vs engine, schema-validated | pass |

**Known tie (fixture V005).** A pixel centre lies exactly on the tiny triangle's vertex (110.4205, −7.8005). GDAL's rasterizer (rasterstats) counts that pixel (sum 7.0); our half-open rule does not (0). Fractional agrees (0.875 = 7 × 0.125). Matching GDAL here would mean adopting its scanline tie rules. Exact ties have measure zero on real data (none in DIY's 438 villages), so the test pins this case as a known difference.

Golden CSVs are regenerated with `make golden-raster` (python:3.12-slim, linux/amd64, because exactextract has no linux/arm64 wheel). Tool versions are recorded in each CSV's header.

## Results on DIY (`diy-2026-11-a`, WorldPop 2020 constrained, 3″)

- DIY population, fractional: **3,848,726** (center: 3,848,983). BPS 2020 census: ~3.67 M. WorldPop's modelled total is ~5% higher, which is a property of the source.
- Fractional and center agree to 0.007% province-wide but **differ by up to 12.8% per village**. Largest absolute gaps are in dense, small urban villages: Semaki (Kota Yogyakarta, 0.61 km²) center +7.6%, Baciro −3.7%, Brontokusuman +4.3%. The frontend's accuracy view maps these gaps.

| Request (Docker, RustFS, both replicas, caches cold) | Total | raster_read | coverage |
|---|---|---|---|
| Province 34, pop fractional + center + cafes | 480–630 ms | 104–146 ms | ~145 ms |
| Kota Yogyakarta (45 villages), pop fractional + center | 52–58 ms | 35–37 ms | 1–2 ms |

Replicas return identical bodies (diagnostics aside).
