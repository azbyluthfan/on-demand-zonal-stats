# Design notes: trade-offs

This project answers village-level (desa/kelurahan) aggregation queries live from files in object storage, with DuckDB embedded in a Go API and no serving database. This note explains the choices, what they cost, and where a different design would win. Numbers come from the DI Yogyakarta release (438 villages, 67.7k POIs, WorldPop 2020 at 3″) on a laptop; details are in the linked docs.

## 1. On-demand aggregation vs precomputation

**Choice:** compute per request from Parquet/COG files, then cache per-village results.

The honest comparison: every answer this API gives is a sum over **villages**. A precomputed long table, (village, category) → count plus (village, layer, method) → sum, would answer any area query, including drawn polygons (which select villages), by filtering and summing. Nationally that table is about 82k villages × ~820 categories ≈ 67M rows, which is small. On latency alone, precomputation would win cold requests.

What on-demand buys:

- **New questions without a rebuild.** A new category grouping, raster layer, coverage method, or a POI filter (confidence, operating status, date) is a query change, not a prep run over the whole country.
- **One code path for correctness.** The serving algorithm is also prep's QC (prep assigns every POI with the same H3 split and cross-checks it against a naive join), so there is no separate precompute pipeline to keep consistent.
- **Precompute-like latency anyway, after first touch.** The per-village result cache turns a 608 ms cold province request into 9 ms warm (docs/caching.md). Caching per **category** rather than per category set means any combination of seen categories is a cache hit.

What it costs: cold latency (province with POIs + both raster methods: ~0.5–0.6 s in Docker), CPU per replica, and DuckDB memory. If the question set were fixed, precompute the long table at release time and serve it from the same Parquet files. The design allows both: a precomputed table would be one more artifact in the manifest.

## 2. File layout and object storage

- **Immutable releases** under `releases/<release_id>/`. The manifest is written and uploaded **last**, so a reader that finds a manifest finds every artifact it lists. Serving reads only what the manifest lists. Rolling forward is a new prefix plus a config change.
- **Sort orders chosen for pruning:** villages by Hilbert curve (spatially compact row groups for bbox filters); `village_h3` and POIs by `h3_cell`, so the `h3_cell IN (…)` semi-join prunes row groups (1 of 10 row groups scanned in the verification spike). Partitioned by province, so a regency request reads one province's files.
- **Partition key = province of the village prep assigned each POI to.** That makes "read the selected provinces' partitions" sufficient even for POIs in cells shared across a province border. Unassigned POIs go to the lowest candidate province, or `_none`.
- **COG, 512×512 DEFLATE tiles with overviews.** Read with the `cog` extension, whose own S3 client is configured from `AWS_*` env vars rather than DuckDB secrets (docs/spikes.md §2).
- **Path-style S3 on RustFS** works for both httpfs and cog. Nothing here is RustFS-specific.

## 3. H3 interior/boundary split

**Idea:** most points lie well inside a village. Classify each village's H3 cells as *interior* (the whole hexagon is inside one village) or *boundary*. Interior points join on `h3_cell` with no geometry; only boundary points get an exact `ST_Covers` against that cell's candidate villages.

- **Exactness.** Hexagons are treated as planar lon/lat polygons. Real H3 edges are great-circle arcs, ~1e-9° off at res 10, so a cell is interior only if no village edge comes within **1e-7° (~1 cm)**. A further `Merge` step demotes any cell claimed by two villages (overlapping source polygons). The result is identical to a naive join, verified on every DIY POI (0 mismatches) and in fixture golden tests (docs/query.md).
- **Edge rule.** A point on a shared border goes to the **lowest `village_id`** (byte order). Boundary candidates include villages *outside* the requested area, so a village's count never depends on what was asked; regency-by-regency and district-by-district totals equal the province's exactly.
- **Two H3 implementations.** POI cells come from DuckDB's `h3` extension, classification from `h3-go`. They bundle different H3 C versions and disagree on ~1–3 points per 100k at res 10–11, all within 1.1e-10° of a cell edge. That is safe because of the 1 cm margin; prep QC measures it every build.
- **Resolution 10, calibrated** (docs/prep.md). Against res 9, exact tests drop from 77k to 34k for DIY, and small urban villages (<1 km²) go from 17% to 56% of POIs on the interior path. Province partitions stay small (375 KB for DIY; ~70 MB nationally).
- **Unassigned POIs** (in a village's cell but inside no village: holes, gaps) are attributed to the lowest candidate village of their cell. That keeps them cacheable per village and additive across areas.

## 4. Fractional pixel coverage

- **Definition:** Σ pixel × area(pixel ∩ village) / area(pixel), computed exactly by polygon clipping in planar lon/lat, the same definition exactextract uses. Implemented in Go (`internal/raster`) as two-stage Sutherland–Hodgman: each ring is clipped once per pixel row, then per pixel. A 100 km² village with 2,000 vertices takes ~3 ms.
- **Verified** against exactextract on all 438 DIY villages (max relative difference 1.8e-8), and against a closed-form fixture. Σ villages = exactextract on the dissolved union (docs/raster.md).
- **Why it matters:** pixel-centre counting is only 0.007% off for the province total but **up to 12.8% off for individual villages**, mostly small dense urban ones (Semaki +7.6%, Baciro −3.7%). The frontend's accuracy view maps this.
- **Read path:** each 512×512 COG **block** touched by any village is read once per request (`RS_BandAsArray` on exact pixel-edge bounds), then village windows are cut out in Go. The linux_arm64 build of `cog` at DuckDB 1.5.5 has no tile cache, and per-village windows took 4.5 s in Docker against ~110–146 ms by block. Decoded blocks are also the block-cache unit.
- **Tie rule:** pixel centres exactly on a boundary use a half-open rule (never double-counted between neighbours). GDAL's rasterizer differs on exact ties; one contrived fixture case is pinned in tests, and DIY has none.

## 5. Replicas, memory and caching

- **Stateless, read-only replicas**, each with its own in-memory DuckDB (`memory_limit` 2 GB, 4 threads), behind nginx round-robin. They start serving immediately and load the release in the background with backoff (`/healthz` says why they are not ready). If storage disappears they answer 503, never partial results.
- **Per-replica caches** (result, decoded block, per-province reference tables) are sized by env vars. With round-robin each replica warms separately, so the hit rate halves with two replicas. Consistent hashing by area or province at the load balancer would fix that if it mattered.
- **Measured** (k6, 20 users, 5 min, mixed): 0 errors in 49,021 requests, peak RSS ~500 MB per replica. The initial p95 targets are met by 50–150×: warm regency POIs 5.2 ms, cold regency population 92 ms, warm province 5.9 ms (docs/caching.md).
- **Nationally**, the reference cache holds per-province `village_h3` and geometries. Jawa Barat has ~14× DIY's villages, so a few hundred MB per province at most; `REF_CACHE_PROVINCES` bounds it. This is the main thing the (not yet run) national build would test.
- **Diagnostics** expose per-stage timings, cache hits and DuckDB's own profile. Limitation: DuckDB reports 0 bytes read for S3/httpfs, so per-request network bytes are not available.

## 6. When PostGIS would be the better choice

Choose PostGIS (or another spatial database) when:

- **Data changes continuously**: individual POI edits, streaming updates, user-contributed geometries. This design is release-based; any change means a new release.
- **Queries are open-ended spatial SQL**: joins across many layers, nearest-neighbour or buffers over raw geometries, topology. Here the question shape is fixed (village aggregates); a database planner with GiST indexes handles ad-hoc queries far better.
- **Many small, highly concurrent lookups over a hot working set** where a long-lived indexed database beats re-reading files (though the caches here narrow that gap).
- **Transactions, row-level security, multi-tenant writes**, or teams with existing PostGIS tooling (QGIS editing, `ST_AsMVT` vector tiles, pg_tileserv).

This design fits when data arrives in releases, storage should be cheap and shared (one bucket, any number of stateless replicas), the questions are aggregate and well-defined, and auditability matters: every number traces to a manifest with checksums and QC.

## 7. Known limitations

- **DIY only.** The national build (82k villages) is untested; the overlap and gap QC (`ST_Union_Agg`) and the reference-cache memory are the risks.
- **Boundaries date from 2020** (34 provinces, before the 2022 Papua split; ~82k vs ~84k villages today).
- **WorldPop constrained 2020** totals ~3.85 M for DIY against ~3.67 M in the BPS 2020 census. That is a property of the source.
- **Platform-specific extension builds** (`cog` linux_arm64 lags), and DuckDB pinned at 1.5.5, the only version with a `cog` build.
- **No authentication or multi-tenancy** (non-goals for v1).
