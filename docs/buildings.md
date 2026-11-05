# Buildings

`building_count` and `building_area` metrics: `Engine.Buildings` in `internal/query/buildings.go`, built by `internal/prep/buildings.go`. Source: Overture Maps Buildings (OpenStreetMap, Microsoft and Google Open Buildings footprints), ODbL.

```json
{ "id": "n", "type": "building_count" }
{ "id": "footprint", "type": "building_area" }
```

## Definition

- **Which village a building is in:** the village containing its **point on surface** (`ST_PointOnSurface`), with the POI edge rule (lowest `village_id` on a shared border). Unlike the centroid, that point is always inside the footprint, so an L-shaped or courtyard building is never placed outside itself.
- **Footprint area:** `ST_Area` in EPSG:6933 (equal-area, WGS84), as for village areas. `building_area` sums whole footprints of the village's buildings; a building straddling a border counts entirely towards one village. Share of village area built over = `building_area / area_m2` (the frontend's "Building footprint %").
- The point is rounded to 1e-7° (~1 cm) in prep, and prep assigns the **rounded** point, so serving reproduces prep exactly.
- Buildings whose point lies in no village (holes, gaps) are unassigned. Like POIs, they are attributed to the lowest candidate village of their H3 cell and reported in diagnostics (`buildings.unassigned`), not counted.

## Storage: the H3 split applied at prep time

Buildings are dense (3.67 M in DIY's bounding box, against 105 k POIs), and most of them sit in **interior** H3 cells, where the village is known without any geometry. So prep stores them in two files per province:

| File | Rows | Columns |
|---|---|---|
| `buildings/release=<r>/province_code=<p>/cells.parquet` | one per **interior** cell that has buildings | `h3_cell, buildings, area_m2` (pre-summed) |
| `buildings/release=<r>/province_code=<p>/points.parquet` | one per building in a **boundary** cell | `h3_cell, x_e7, y_e7` (int32, 1e-7°), `area_m2` (float32) |

Pre-summing interior cells is exact: an interior cell lies wholly inside one village (docs/design.md §3), so every building in it belongs to that village whatever area is asked. Buildings in no village's cell can never count and are dropped (QC reports how many). Partition keys follow the POI rules: the cell's village's province, else the assigned village's, else the lowest candidate's.

## Query

| Stage | What |
|---|---|
| `building_cells` | the selected villages' `village_h3` rows and boundary candidates (as for POIs), then Σ `buildings`, `area_m2` of the pre-summed cells joined on interior cells. No geometry. |
| `building_boundary` | points in the selected boundary cells, `ST_Covers` against each candidate village, lowest `village_id` wins; winners outside the area are dropped (`points_other_village`), uncovered points are unassigned. |

Results are cached per village (count, area, unassigned); building metrics in one request share one computation.

## QC (prep)

- `unique_building_id`, `building_assignment` (blocking): every building is in exactly one of cells / points / outside.
- `building_assignment_crosscheck` (blocking, with `cross_check_pois`): the H3-split assignment against a naive `ST_Covers` join of every building over all villages, 0 mismatches required.

## Tests

`TestBuildingsAgainstNaive` builds the fixture buildings (axis-aligned rectangles from `gen_fixtures.py`, centres on the 1e-7° grid, with edge cases: centre on the V001|V002 border, straddling it, in V004's hole, in tiny V005, far away) and checks, for code areas and a drawn polygon, cold, computed and cached:

- counts equal an independent Go assignment (`geom.Assign`, edge rule over all villages) of each centre;
- areas equal the exact WGS84 ellipsoidal area of each rectangle to 1e-6 (EPSG:6933 is equal-area on that ellipsoid).

`TestBuildingsContract` checks the HTTP response against the engine and the JSON Schema, and the cache on the second request.

## On DIY (`diy-2026-11-b`, local files, Apple Silicon)

| Request | Cold (`no-cache`) | Warm |
|---|---|---|
| Province 34 (438 villages): buildings + night lights + 8 land-cover classes | 2.2–2.3 s, of which `building_boundary` 1.8 s, `building_cells` 35–45 ms, raster read + coverage ~0.35 s | 12 ms |
| Kota Yogyakarta (45 villages): buildings + built-up share + night lights | 264 ms | |

Province totals: 2,068,649 buildings, 193 km² of footprint (6.1% of DIY's 3,172 km²), 6,309 unassigned. `TestDIYBuildingsAndLayers` checks that district-by-district counts equal the province-wide ones and that counted + unassigned = what prep stored for the province.

The cold province cost is the 634,786 `ST_Covers` tests of boundary points against detailed village polygons. If it matters, the next step is testing those points in Go with `internal/geom` (prepared per village, parallel), or a finer H3 resolution for the building split only.
