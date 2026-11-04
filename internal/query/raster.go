package query

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"

	"on-demand-zonal-stats/internal/raster"
	"on-demand-zonal-stats/internal/release"
)

// RasterMetric is a raster_sum metric.
type RasterMetric struct {
	ID     string
	Layer  string
	Method string // "fractional" or "center"
}

// RasterStats explains raster work for diagnostics.
type RasterStats struct {
	Windows        int     // village windows (one per village per layer)
	Blocks         int     // COG blocks read (each decoded once per request)
	PixelsRead     int64   // pixels returned by the cog extension
	PixelsWeighted int64   // pixels with weight > 0 (summed over metrics)
	NodataWeight   float64 // pixel-equivalents of nodata inside villages (summed over metrics)
	Stages         map[string]time.Duration
}

// RasterResult holds per-village sums per metric.
type RasterResult struct {
	Values map[string]map[string]float64 // metric ID -> village ID -> sum
	Stats  RasterStats
}

// RasterSums computes Σ pixel × weight per village (docs/raster.md). Pixel
// windows are read with the cog extension's RS_BandAsArray using a bbox
// snapped to pixel edges (cog rounds bbox edges to the nearest pixel edge,
// returns rows north to south, nodata as NULL, and clips to the raster
// extent: docs/raster.md). Coverage and sums are computed in Go
// (internal/raster).
func (e *Engine) RasterSums(ctx context.Context, r *Resolved, metrics []RasterMetric, tr *Trace) (*RasterResult, error) {
	res := &RasterResult{Values: map[string]map[string]float64{}, Stats: RasterStats{Stages: map[string]time.Duration{}}}
	for _, m := range metrics {
		res.Values[m.ID] = map[string]float64{}
	}
	if len(metrics) == 0 || len(r.Villages) == 0 {
		return res, nil
	}
	useCache := e.cache != nil && (tr == nil || !tr.NoCache)

	// Result cache: (village, layer, method). Villages missing any metric
	// are recomputed for the layers they miss.
	missing := map[string]map[string]bool{} // layer -> village set
	var hits, misses int64
	for _, m := range metrics {
		for _, v := range r.Villages {
			if useCache {
				if x, ok := e.cache.raster.Get(rasterKey{v.ID, m.Layer, m.Method}); ok {
					res.Values[m.ID][v.ID] = x.Sum
					hits++
					continue
				}
			}
			misses++
			if missing[m.Layer] == nil {
				missing[m.Layer] = map[string]bool{}
			}
			missing[m.Layer][v.ID] = true
		}
	}
	tr.add(func(c *CacheCounts) { c.ResultHits += hits; c.ResultMisses += misses })
	if len(missing) == 0 {
		return res, nil
	}

	all := map[string]bool{}
	for _, vs := range missing {
		for v := range vs {
			all[v] = true
		}
	}
	sub := r.subset(all)
	t := time.Now()
	var geom func(string) (orb.Geometry, bool)
	if e.refs != nil && useCache {
		refs, err := e.acquireRefs(ctx, sub.Provinces, tr)
		if err != nil {
			return nil, err
		}
		defer refs.release()
		geom = refs.geom
	} else {
		geoms, err := e.villageGeoms(ctx, sub.IDs())
		if err != nil {
			return nil, err
		}
		geom = func(id string) (orb.Geometry, bool) { g, ok := geoms[id]; return g, ok }
	}
	res.Stats.Stages["raster_geometry"] += time.Since(t)

	var layers []string
	byLayer := map[string][]RasterMetric{}
	for _, m := range metrics {
		if missing[m.Layer] == nil {
			continue
		}
		if _, ok := byLayer[m.Layer]; !ok {
			layers = append(layers, m.Layer)
		}
		byLayer[m.Layer] = append(byLayer[m.Layer], m)
	}
	for _, layer := range layers {
		info, ok := e.Manifest.Raster(layer)
		if !ok {
			return nil, fmt.Errorf("query: unknown raster layer %q", layer)
		}
		if err := e.rasterLayer(ctx, sub.subset(missing[layer]), geom, info, byLayer[layer], res, tr, useCache); err != nil {
			return nil, err
		}
	}
	return res, nil
}

type villageWindow struct {
	id     string
	geom   orb.Geometry
	window raster.Window
	values []float64
}

func (e *Engine) rasterLayer(ctx context.Context, r *Resolved, geom func(string) (orb.Geometry, bool), info *release.RasterInfo,
	metrics []RasterMetric, res *RasterResult, tr *Trace, useCache bool) error {
	grid := raster.Grid{OriginX: info.OriginX, OriginY: info.OriginY, PixelW: info.PixelWidth, PixelH: info.PixelHeight,
		Cols: info.Width, Rows: info.Height}
	if err := grid.Validate(); err != nil {
		return err
	}
	var wins []*villageWindow
	for _, v := range r.Villages {
		g, ok := geom(v.ID)
		if !ok {
			return fmt.Errorf("query: no geometry for village %s", v.ID)
		}
		if w, ok := grid.WindowFor(g.Bound()); ok {
			wins = append(wins, &villageWindow{id: v.ID, geom: g, window: w})
		} else if useCache { // entirely outside the raster: sum 0
			for _, m := range metrics {
				e.cache.raster.Add(rasterKey{v.ID, info.ID, m.Method}, raster.Result{})
			}
		}
	}

	// Read each COG block (BlockSize, normally 512x512) touched by any
	// village window exactly once, then cut village windows out of the
	// blocks in Go. Overlapping village windows never re-decode a tile, on
	// any platform (the linux_arm64 cog build has no tile cache: docs/raster.md).
	t := time.Now()
	blocks, err := e.readBlocks(ctx, info.ID, e.path(info.Path), grid, info.BlockSize, wins, tr, useCache)
	res.Stats.Stages["raster_read"] += time.Since(t)
	if err != nil {
		return err
	}
	res.Stats.Windows += len(wins)
	res.Stats.Blocks += len(blocks)
	for _, b := range blocks {
		res.Stats.PixelsRead += int64(len(b.values))
	}
	for _, w := range wins {
		w.values = cutWindow(w.window, blocks, blockSize(info.BlockSize))
	}

	// Coverage + weighted sums, in parallel across villages.
	t = time.Now()
	nd := raster.Nodata{}
	if info.Nodata != nil {
		nd = raster.Nodata{Value: *info.Nodata, Set: true}
	}
	type out struct {
		metric, village string
		r               raster.Result
	}
	results := make([][]out, len(wins))
	errs := make([]error, len(wins))
	var wg sync.WaitGroup
	next := make(chan int)
	for k := 0; k < runtime.NumCPU(); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				w := wins[i]
				covs := map[string]raster.Coverage{}
				for _, m := range metrics {
					cov, ok := covs[m.Method]
					if !ok {
						if m.Method == "center" {
							cov = raster.Center(grid, w.geom)
						} else {
							cov = raster.Fractional(grid, w.geom)
						}
						covs[m.Method] = cov
					}
					if cov.Window != w.window {
						errs[i] = fmt.Errorf("query: coverage window %+v != read window %+v for %s", cov.Window, w.window, w.id)
						return
					}
					rr, err := cov.WeightedSum(w.values, nd)
					if err != nil {
						errs[i] = fmt.Errorf("village %s: %w", w.id, err)
						return
					}
					results[i] = append(results[i], out{m.ID, w.id, rr})
				}
			}
		}()
	}
	for i := range wins {
		next <- i
	}
	close(next)
	wg.Wait()
	res.Stats.Stages["raster_coverage"] += time.Since(t)
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	method := map[string]string{}
	for _, m := range metrics {
		method[m.ID] = m.Method
	}
	for _, rs := range results {
		for _, o := range rs {
			res.Values[o.metric][o.village] = o.r.Sum
			if useCache {
				e.cache.raster.Add(rasterKey{o.village, info.ID, method[o.metric]}, o.r)
			}
			res.Stats.PixelsWeighted += int64(o.r.PixelsTouched)
			res.Stats.NodataWeight += o.r.NodataWeight
		}
	}
	return nil
}

type blockKey struct{ bx, by int }

type block struct {
	window raster.Window
	values []float64
}

func blockSize(bs [2]int) [2]int {
	if bs[0] <= 0 || bs[1] <= 0 {
		return [2]int{512, 512}
	}
	return bs
}

// readBlocks reads every block touched by the windows, in batched statements.
func (e *Engine) readBlocks(ctx context.Context, layer, path string, grid raster.Grid, bsRaw [2]int, wins []*villageWindow,
	tr *Trace, useCache bool) (map[blockKey]*block, error) {
	bs := blockSize(bsRaw)
	blocks := map[blockKey]*block{}
	var order []blockKey
	for _, w := range wins {
		for by := w.window.Row0 / bs[1]; by <= (w.window.Row0+w.window.Rows-1)/bs[1]; by++ {
			for bx := w.window.Col0 / bs[0]; bx <= (w.window.Col0+w.window.Cols-1)/bs[0]; bx++ {
				k := blockKey{bx, by}
				if _, ok := blocks[k]; ok {
					continue
				}
				c0, r0 := bx*bs[0], by*bs[1]
				b := &block{window: raster.Window{Col0: c0, Row0: r0,
					Cols: min(bs[0], grid.Cols-c0), Rows: min(bs[1], grid.Rows-r0)}}
				blocks[k] = b
				if useCache {
					if vals, ok := e.cache.blocks.Get(blockCacheKey{layer, bx, by}); ok {
						b.values = vals
						tr.add(func(c *CacheCounts) { c.BlockHits++ })
						continue
					}
				}
				tr.add(func(c *CacheCounts) { c.BlockMisses++ })
				order = append(order, k)
			}
		}
	}
	const batch = 16 // blocks per statement (16 x 512^2 doubles = 32 MB)
	for i := 0; i < len(order); i += batch {
		bl := make([]*block, 0, batch)
		for _, k := range order[i:min(i+batch, len(order))] {
			bl = append(bl, blocks[k])
		}
		if err := e.readWindows(ctx, path, grid, bl, tr); err != nil {
			return nil, err
		}
	}
	if useCache {
		for _, k := range order {
			e.cache.blocks.Add(blockCacheKey{layer, k.bx, k.by}, blocks[k].values)
		}
	}
	return blocks, nil
}

// cutWindow assembles a window's values (row-major) from blocks.
func cutWindow(w raster.Window, blocks map[blockKey]*block, bs [2]int) []float64 {
	out := make([]float64, w.Len())
	for r := 0; r < w.Rows; r++ {
		gr := w.Row0 + r
		for c := 0; c < w.Cols; {
			gc := w.Col0 + c
			b := blocks[blockKey{gc / bs[0], gr / bs[1]}]
			// Copy the run of this row that lies inside block b.
			n := min(w.Cols-c, b.window.Col0+b.window.Cols-gc)
			src := (gr-b.window.Row0)*b.window.Cols + (gc - b.window.Col0)
			copy(out[r*w.Cols+c:r*w.Cols+c+n], b.values[src:src+n])
			c += n
		}
	}
	return out
}

// readWindows fetches pixel windows in one statement via RS_BandAsArray.
func (e *Engine) readWindows(ctx context.Context, path string, grid raster.Grid, wins []*block, tr *Trace) error {
	f := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) } // exact round trip
	var b strings.Builder
	b.WriteString("SELECT i, RS_BandAsArray(" + lit(path) + ", 1, [xmin, ymin, xmax, ymax]) FROM (VALUES ")
	for i, w := range wins {
		wb := grid.WindowBound(w.window)
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d, %s::DOUBLE, %s::DOUBLE, %s::DOUBLE, %s::DOUBLE)", i, f(wb.Min[0]), f(wb.Min[1]), f(wb.Max[0]), f(wb.Max[1]))
	}
	b.WriteString(") t(i, xmin, ymin, xmax, ymax)")
	c, err := e.db.Conn(ctx)
	if err != nil {
		return classify(ctx, err)
	}
	defer c.Close()
	enableProfiling(ctx, c)
	defer disableProfiling(c)
	rows, err := c.QueryContext(ctx, b.String())
	if err != nil {
		return classify(ctx, err)
	}
	defer rows.Close()
	defer func() { tr.collect(c, "raster_read") }()
	for rows.Next() {
		var i int
		var arr []any
		if err := rows.Scan(&i, &arr); err != nil {
			return err
		}
		w := wins[i]
		if len(arr) != w.window.Len() {
			return fmt.Errorf("%w: cog returned %d values for a %dx%d window at (%d,%d)",
				ErrStorage, len(arr), w.window.Cols, w.window.Rows, w.window.Col0, w.window.Row0)
		}
		w.values = make([]float64, len(arr))
		for k, v := range arr {
			switch x := v.(type) {
			case nil:
				w.values[k] = math.NaN() // cog maps nodata to NULL
			case float64:
				w.values[k] = x
			case float32:
				w.values[k] = float64(x)
			default:
				return fmt.Errorf("query: unexpected pixel type %T", v)
			}
		}
	}
	return classify(ctx, rows.Err())
}

// villageGeoms reads the villages' geometries.
func (e *Engine) villageGeoms(ctx context.Context, ids []string) (map[string]orb.Geometry, error) {
	rows, err := e.db.QueryContext(ctx, fmt.Sprintf(`SELECT village_id, ST_AsWKB(geometry)
		FROM read_parquet(%s) WHERE village_id IN (SELECT unnest(%s))`, lit(e.path(release.VillagesPath)), listLit(ids)))
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer rows.Close()
	out := make(map[string]orb.Geometry, len(ids))
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		g, err := wkb.Unmarshal(b)
		if err != nil {
			return nil, fmt.Errorf("village %s: %w", id, err)
		}
		out[id] = g
	}
	return out, classify(ctx, rows.Err())
}
