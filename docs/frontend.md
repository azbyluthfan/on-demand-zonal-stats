# Frontend

`web/` (`index.html`, `style.css`, `app.js`): one static page served by the API container (`WEB_DIR`, baked into `Dockerfile.api` at `/srv/web`). Plain JS, no build step.

Open http://localhost:8080 after `docker compose up -d` (load balancer; the replicas are also on :8081 and :8082), or after `make api-local`.

## Libraries (pinned, from unpkg)

| Library | Version | Why this one |
|---|---|---|
| MapLibre GL JS | 5.24.0 (UMD) | MapLibre 6 ships ES modules plus a separate worker only; the 5.x UMD build loads with a plain `<script>` |
| Terra Draw | 1.38.0 (UMD, global `terraDraw`) | polygon drawing |
| terra-draw-maplibre-gl-adapter | 1.5.0 (UMD, `TerraDrawMapLibreGLAdapter({ map })`) | |
| Basemap | OpenFreeMap Positron (`tiles.openfreemap.org/styles/positron`) | free, no key; © OpenStreetMap contributors |

## What it does

- **Area**: province → regency → district dropdowns (from `GET /v1/areas`; the most specific choice is sent), or **Draw** a polygon (Terra Draw; click to add points, click the first point to close).
- **Points of interest**: preset chips (Cafés, Schools, Places of worship, Restaurants, Health, Shops; only categories present in the release) plus search across all ~820 categories from `GET /v1/layers`. Selected categories become one `poi_count` metric.
- **Layers** (each shown only when the release has it, roles read from `GET /v1/layers`):
  - **Population** (the continuous layer in people): when on, the request asks for **both** `fractional` and `center` (`pop_fractional`, `pop_center`), so the map can switch views without refetching. The method radio picks which one "Population" uses.
  - **Night lights** (another continuous layer): `raster_mean`, fractional.
  - **Land cover** (the categorical layer): one `raster_class_fraction` per class (11 for WorldCover), so the dominant-class and per-class views need no refetch.
  - **Buildings**: `building_count` + `building_area`.
- **Map position in the URL** (`#map=zoom/lat/lng`), so a view can be shared or scripted (`make gallery` uses it; docs/gallery.md).
- **Map layers** (live tiles, docs/tiles.md): village outlines, H3 interior/boundary cells, POIs (the selected categories, else all), buildings (pre-summed interior cells as hexagons plus boundary-cell points) and every raster layer, each with a toggle and opacity, plus the choropleth's own opacity. Rasters draw under the choropleth, vector layers above it, labels on top. Raster rows show their legend. Clicking a POI, building or H3 cell opens its popup. A live line shows tiles loaded, how many came from the tile cache, DuckDB median/p95 ms and features encoded, read from each tile's `Server-Timing` header via a `duck://` MapLibre protocol.
- **Map shows**: POI count, Population, POIs per 1,000 people; night-light mean radiance and radiance × km² per 1,000 people; buildings (count, footprint % of village area, mean footprint m², people per building); land cover **dominant class** (categorical colours from the layer's classes, villages per class in the legend) and each class's % of area; and the **accuracy view**: centre − fractional in people, or in %, as a diverging blue–red choropleth. Count views have a "per km²" toggle. Sequential views use 6 quantile classes (YlGnBu); diverging views use symmetric classes at the 40/70/90th percentiles of |value| (RdBu). Villages with no value (e.g. 0 population for a %) are grey and counted in the legend.
- **Result**: totals (villages, POIs, both population figures, night-light mean, buildings and footprint share, land-cover shares ≥ 0.5%, POIs in no village), top-10 villages for the current view (largest |gap| for accuracy; click to zoom), hover outline, and a click popup with every value for the village.
- **Diagnostics panel**: stage timing bar and table, POI assignment (villages computed, points scanned, interior vs boundary, exact `ST_Covers` tests, POIs won by a neighbour), buildings (interior cells read, buildings via cells, boundary points, exact tests), raster (windows, COG blocks, pixels, nodata), cache hits/misses, DuckDB profile per stage, partitions, server vs round-trip time. Collapsible.
- **Bypass cache** checkbox sends `Cache-Control: no-cache`, to show cold vs warm.
- Errors show the API message and each field error (e.g. `metrics[0].categories[1]: unknown category …`); 413 adds a hint to pick a smaller area; 503 at load says the API is starting.
- Footer: data sources and licenses from the manifest, plus basemap attribution.

The API gzips JSON, GeoJSON and static responses (`internal/api/gzip.go`); province GeoJSON is 6.2 MB raw, 1.7 MB gzipped.

## Manual checklist (2026-11-04, desktop browser, host API and both Docker replicas, release `diy-2026-11-a`)

| Check | Result |
|---|---|
| Area picker loads codes | ✅ DIY (438) → 5 regencies → districts (e.g. 14 in Kota Yogyakarta); district disabled until a regency is chosen |
| Drawing a polygon returns results | ✅ 4-vertex box over Yogyakarta → 109 villages in 64 ms (warm); map fits; outline stays visible |
| Choropleth legend matches values | ✅ Kota Yogyakarta cafés per km²: legend max 80.5 = top village (Suryatmajan 80.5); totals 816 cafés / 378,382 people match the API |
| Diagnostics panel renders | ✅ stages, POI split (2,229 interior / 699 boundary points for the province), raster 4 blocks / 800,598 px, cache hits (180/0 on a repeat), DuckDB profile |
| Accuracy view loads | ✅ centre − fractional (people and %), diverging legend; province totals 3,848,726 vs 3,848,983 |
| Popup | ✅ e.g. Sorosutan (Umbulharjo): 23 POIs, 19,944 vs 19,826 people, gap −118 |
| No console errors | ✅ |
| Phone width (375 px) | ✅ map above controls, no horizontal scroll (basemap was still loading in the quick check; diagnostics panel hidden on phones) |

Village popups are suppressed while drawing and for 400 ms after a polygon is closed, so the closing click doesn't open one.

## Playwright smoke test

[e2e/smoke.mjs](../e2e/smoke.mjs), run by `make e2e` and in CI. It runs 17 checks covering the checklist (area picker, province → choropleth, legend max = top village, diagnostics sections, accuracy view, regency, popup, drawing a polygon, no console errors) and refreshes the screenshots in `docs/img/`. Terra Draw reads very fast automated clicks as a double-click, so the test pauses 400 ms between vertices. A result that arrives before the map style has loaded (slow GPU or network) is drawn once the map is ready.
