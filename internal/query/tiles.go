package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmach/orb"

	"on-demand-zonal-stats/internal/raster"
	"on-demand-zonal-stats/internal/release"
)

// Live map tiles (docs/tiles.md). Nothing is pre-rendered: every vector
// tile is one DuckDB query that reads the release's Parquet files and
// encodes Mapbox Vector Tiles with ST_AsMVT; every raster tile reads COG
// blocks through the cog extension (sharing the aggregation block cache)
// and is coloured in Go.

// ErrBadTile is a malformed tile address or unknown tile layer (400/404).
var ErrBadTile = errors.New("bad tile request")

// Tile is a z/x/y address in the Web Mercator (XYZ) scheme.
type Tile struct{ Z, X, Y int }

// ParseTile parses z, x and y ("12", "3304", "2137.mvt"); the suffix of y
// must be ext.
func ParseTile(z, x, y, ext string) (Tile, error) {
	y, ok := strings.CutSuffix(y, ext)
	if !ok {
		return Tile{}, fmt.Errorf("%w: tile must end in %s", ErrBadTile, ext)
	}
	var t Tile
	var err error
	for _, p := range []struct {
		s string
		v *int
	}{{z, &t.Z}, {x, &t.X}, {y, &t.Y}} {
		if *p.v, err = strconv.Atoi(p.s); err != nil {
			return Tile{}, fmt.Errorf("%w: %q is not an integer", ErrBadTile, p.s)
		}
	}
	n := 1 << max(t.Z, 0)
	if t.Z < 0 || t.Z > 22 || t.X < 0 || t.X >= n || t.Y < 0 || t.Y >= n {
		return Tile{}, fmt.Errorf("%w: tile %d/%d/%d out of range", ErrBadTile, t.Z, t.X, t.Y)
	}
	return t, nil
}

// Bound is the tile's lon/lat bounds, grown by pad tile widths on each side.
func (t Tile) Bound(pad float64) orb.Bound {
	n := float64(int(1) << t.Z)
	lon := func(x float64) float64 { return x/n*360 - 180 }
	lat := func(y float64) float64 { return math.Atan(math.Sinh(math.Pi*(1-2*y/n))) * 180 / math.Pi }
	x0, x1 := float64(t.X)-pad, float64(t.X+1)+pad
	y0, y1 := float64(t.Y)-pad, float64(t.Y+1)+pad
	return orb.Bound{Min: orb.Point{lon(x0), lat(y1)}, Max: orb.Point{lon(x1), lat(y0)}}
}

// VectorLayer is one live vector tile layer.
type VectorLayer struct {
	ID          string `json:"id"`
	MinZoom     int    `json:"minzoom"`
	Description string `json:"description"`
}

// VectorLayers lists the vector tile layers this release can serve.
func (e *Engine) VectorLayers() []VectorLayer {
	out := []VectorLayer{
		{"villages", 8, "village boundaries (village_id, village_name, district_name, area_m2)"},
		{"h3", 13, "village_h3 cells: the interior/boundary split (h3_cell, village_id, cell_type)"},
		{"pois", 13, "POIs (name, category); ?categories=a,b filters"},
	}
	if e.Manifest.HasBuildings() {
		out = append(out, VectorLayer{"buildings", 14,
			"layer building_cells: interior H3 cells as hexagons (buildings, area_m2); layer building_points: boundary-cell buildings (area_m2)"})
	}
	return out
}

func (e *Engine) vectorLayer(id string) (VectorLayer, bool) {
	for _, l := range e.VectorLayers() {
		if l.ID == id {
			return l, true
		}
	}
	return VectorLayer{}, false
}

// TileResult is an encoded tile and how it was made.
type TileResult struct {
	Data   []byte
	Cached bool // served from the tile cache
	// BlockCached: a raster tile drawn only from already-decoded COG blocks
	// (the block cache shared with aggregation), so DuckDB did no work.
	BlockCached bool
	DuckDB      time.Duration // time inside DuckDB (queries or cog reads)
	Features    int64         // vector tiles: features encoded
}

const (
	mvtExtent = 4096
	mvtBuffer = 64
)

// VectorTile renders one vector tile. categories filters the pois layer.
// Below the layer's minzoom the tile is empty.
func (e *Engine) VectorTile(ctx context.Context, layer string, t Tile, categories []string, noCache bool) (*TileResult, error) {
	l, ok := e.vectorLayer(layer)
	if !ok {
		return nil, fmt.Errorf("%w: unknown vector layer %q", ErrBadTile, layer)
	}
	for _, c := range categories {
		if !e.HasCategory(c) {
			return nil, fmt.Errorf("%w: unknown category %q", ErrBadTile, c)
		}
	}
	if t.Z < l.MinZoom {
		return &TileResult{}, nil
	}
	sort.Strings(categories)
	key := fmt.Sprintf("v/%s/%d/%d/%d/%s", layer, t.Z, t.X, t.Y, strings.Join(categories, ","))
	if b, ok := e.tileGet(key, noCache); ok {
		return &TileResult{Data: b, Cached: true}, nil
	}

	b := t.Bound(float64(mvtBuffer) / mvtExtent)
	bbox := func(xmin, ymin, xmax, ymax string) string { // overlap test on a bbox
		return fmt.Sprintf("%s <= %v AND %s >= %v AND %s <= %v AND %s >= %v",
			xmin, b.Max[0], xmax, b.Min[0], ymin, b.Max[1], ymax, b.Min[1])
	}
	in := func(x, y string) string { return bbox(x, y, x, y) }
	// Hexagons are selected by centre, so grow the box by a cell's radius
	// (or a cell crossing the tile edge would leave a seam): the H3 average
	// edge length at the release resolution, in degrees at the equator, x2.
	cellPad := 2 * h3EdgeDeg(e.Manifest.H3Resolution)
	inCell := func(x, y string) string {
		return fmt.Sprintf("%s BETWEEN %v AND %v AND %s BETWEEN %v AND %v",
			x, b.Min[0]-cellPad, b.Max[0]+cellPad, y, b.Min[1]-cellPad, b.Max[1]+cellPad)
	}
	geom := func(g string) string { // to tile coordinates, clipped with a buffer
		return fmt.Sprintf("ST_AsMVTGeom(ST_Transform(%s, 'EPSG:4326', 'EPSG:3857', true), ST_Extent(ST_TileEnvelope(%d, %d, %d)), %d, %d, true)",
			g, t.Z, t.X, t.Y, mvtExtent, mvtBuffer)
	}
	// mvt encodes rows of q (with a geom column) as one MVT layer. The
	// ordered aggregate makes the bytes deterministic: a parallel scan
	// would otherwise feed features in a different order on every run.
	mvt := func(name, q string) string {
		return fmt.Sprintf(`SELECT ST_AsMVT(f, %s, %d, 'geom' ORDER BY f::VARCHAR), count(*) FROM (SELECT * FROM (%s) WHERE geom IS NOT NULL) f`,
			lit(name), mvtExtent, q)
	}
	// all lists every province partition of a layer in the manifest
	// (province_codes may be empty, meaning all provinces).
	all := func(rel func(string) string) []string {
		var rels []string
		for _, a := range e.Manifest.Artifacts {
			if ok, _ := path.Match(rel("*"), a.Path); ok {
				rels = append(rels, a.Path)
			}
		}
		return e.paths(rels)
	}

	var queries []string
	switch layer {
	case "villages":
		// Simplify to half a tile pixel (in degrees) before projecting: at
		// low zoom most vertices would collapse onto one MVT grid cell anyway.
		tol := 360 / float64(int(1)<<t.Z) / mvtExtent / 2
		queries = append(queries, mvt("villages", fmt.Sprintf(`SELECT village_id, village_name, district_name, regency_name, area_m2, %s AS geom
			FROM read_parquet(%s) WHERE %s`, geom(fmt.Sprintf("ST_SimplifyPreserveTopology(geometry, %g)", tol)),
			lit(e.path(release.VillagesPath)), bbox("bbox.xmin", "bbox.ymin", "bbox.xmax", "bbox.ymax"))))
	case "h3":
		if ps := all(release.VillageH3Path); len(ps) > 0 {
			queries = append(queries, mvt("h3", fmt.Sprintf(`SELECT h3_cell::VARCHAR AS h3_cell, village_id, cell_type,
					%s AS geom
				FROM (SELECT *, h3_cell_to_lat(h3_cell) AS lat, h3_cell_to_lng(h3_cell) AS lng FROM read_parquet(%s))
				WHERE %s`, geom("ST_GeomFromWKB(h3_cell_to_boundary_wkb(h3_cell))"), listLit(ps), inCell("lng", "lat"))))
		}
	case "pois":
		if ps := all(func(p string) string { return release.POIPath(e.Manifest.POIRelease, p) }); len(ps) > 0 {
			where := in("lon", "lat")
			if len(categories) > 0 {
				where += " AND category IN (SELECT unnest(" + listLit(categories) + "))"
			}
			queries = append(queries, mvt("pois", fmt.Sprintf(`SELECT name, category, %s AS geom
				FROM read_parquet(%s) WHERE %s`, geom("geometry"), listLit(ps), where)))
		}
	case "buildings":
		rel := e.Manifest.BuildingRelease
		if ps := all(func(p string) string { return release.BuildingCellsPath(rel, p) }); len(ps) > 0 {
			queries = append(queries, mvt("building_cells", fmt.Sprintf(`SELECT buildings, area_m2,
					%s AS geom
				FROM (SELECT *, h3_cell_to_lat(h3_cell) AS lat, h3_cell_to_lng(h3_cell) AS lng FROM read_parquet(%s))
				WHERE %s`, geom("ST_GeomFromWKB(h3_cell_to_boundary_wkb(h3_cell))"), listLit(ps), inCell("lng", "lat"))))
		}
		if ps := all(func(p string) string { return release.BuildingPointsPath(rel, p) }); len(ps) > 0 {
			queries = append(queries, mvt("building_points", fmt.Sprintf(`SELECT area_m2, %s AS geom
				FROM read_parquet(%s) WHERE %s`, geom("ST_Point(x_e7 / 1e7, y_e7 / 1e7)"), listLit(ps),
				// integer bounds on x_e7/y_e7 prune row groups on their min/max stats
				fmt.Sprintf("x_e7 BETWEEN %d AND %d AND y_e7 BETWEEN %d AND %d",
					int64(math.Floor(b.Min[0]*1e7)), int64(math.Ceil(b.Max[0]*1e7)),
					int64(math.Floor(b.Min[1]*1e7)), int64(math.Ceil(b.Max[1]*1e7))))))
		}
	}

	res := &TileResult{}
	start := time.Now()
	var out bytes.Buffer
	for _, q := range queries {
		var data []byte
		var n int64
		if err := e.db.QueryRowContext(ctx, q).Scan(&data, &n); err != nil {
			return nil, classify(ctx, err)
		}
		out.Write(data) // MVT layers are protobuf fields: tiles concatenate
		res.Features += n
	}
	res.DuckDB = time.Since(start)
	res.Data = out.Bytes()
	e.tileAdd(key, res.Data, noCache)
	return res, nil
}

// h3EdgeDeg is H3's average hexagon edge length at res, in degrees of arc
// (edge km / 111.32), from the H3 resolution table (res 0: 1281.256 km,
// each resolution about 1/sqrt(7) of the previous).
func h3EdgeDeg(res int) float64 {
	return 1281.256 / math.Pow(math.Sqrt(7), float64(res)) / 111.32
}

// RasterLegend describes how raster tiles are coloured.
type RasterLegend struct {
	Layer string `json:"layer"`
	Kind  string `json:"kind"`
	Units string `json:"units"`
	// Continuous: Breaks (in Units) split valid values > 0 into deciles,
	// coloured Colors[0..len(Breaks)]; values ≤ 0 and nodata are transparent.
	Breaks []float64 `json:"breaks,omitempty"`
	Colors []string  `json:"colors"`
	// Categorical: one colour per class; unknown classes and nodata are transparent.
	Classes []release.RasterClass `json:"classes,omitempty"`
}

// rampColors is a 10-step sequential ramp (dark purple to yellow, after
// matplotlib's magma/viridis family) that reads on light and dark basemaps.
var rampColors = []string{"#2c105c", "#4a1486", "#6a1b9a", "#8e2a8c", "#b4367a", "#d7466a", "#ef6351", "#f98e3c", "#fbbf2f", "#f7f05a"}

// RasterTileMinZoom keeps a tile's window to a few million pixels at
// province scale; lower zooms are empty.
const RasterTileMinZoom = 7

// Legend computes (once per layer) the colouring of raster tiles. Continuous
// breaks are deciles of the valid values > 0, computed by DuckDB over the
// whole raster: quantile_cont over unnest(RS_BandAsArray(…)).
func (e *Engine) Legend(ctx context.Context, layer string) (*RasterLegend, error) {
	info, ok := e.Manifest.Raster(layer)
	if !ok {
		return nil, fmt.Errorf("%w: unknown raster layer %q", ErrBadTile, layer)
	}
	if v, ok := e.legends.Load(layer); ok {
		return v.(*RasterLegend), nil
	}
	lg := &RasterLegend{Layer: layer, Kind: release.RasterContinuous, Units: info.Units}
	if info.IsCategorical() {
		lg.Kind, lg.Classes = release.RasterCategorical, info.Classes
		for _, c := range info.Classes {
			lg.Colors = append(lg.Colors, c.Color)
		}
	} else {
		nd := "NULL"
		if info.Nodata != nil {
			nd = strconv.FormatFloat(*info.Nodata, 'g', -1, 64)
		}
		qs := make([]string, 9)
		for i := range qs {
			qs[i] = fmt.Sprintf("%.1f", float64(i+1)/10)
		}
		f := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
		var breaks []any
		err := e.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT quantile_cont(v, [%s])
			FROM (SELECT unnest(RS_BandAsArray(%s, 1, [%s::DOUBLE, %s::DOUBLE, %s::DOUBLE, %s::DOUBLE]))::DOUBLE AS v)
			WHERE v IS NOT NULL AND NOT isnan(v) AND v IS DISTINCT FROM %s AND v > 0`,
			strings.Join(qs, ", "), lit(e.path(info.Path)),
			f(info.Bounds[0]), f(info.Bounds[1]), f(info.Bounds[2]), f(info.Bounds[3]), nd)).Scan(&breaks)
		if err != nil {
			return nil, classify(ctx, err)
		}
		for _, b := range breaks {
			if x, ok := b.(float64); ok {
				x *= info.ScaleFactor()
				if len(lg.Breaks) == 0 || x > lg.Breaks[len(lg.Breaks)-1] {
					lg.Breaks = append(lg.Breaks, x) // drop repeated quantiles (integer data)
				}
			}
		}
		lg.Colors = rampColors[len(rampColors)-len(lg.Breaks)-1:]
	}
	e.legends.Store(layer, lg)
	return lg, nil
}

// RasterTile renders one 256×256 PNG tile: COG blocks under the tile are
// read through the cog extension (shared block cache), then sampled at each
// output pixel's centre (nearest pixel) and coloured with the layer legend.
func (e *Engine) RasterTile(ctx context.Context, layer string, t Tile, noCache bool) (*TileResult, error) {
	info, ok := e.Manifest.Raster(layer)
	if !ok {
		return nil, fmt.Errorf("%w: unknown raster layer %q", ErrBadTile, layer)
	}
	key := fmt.Sprintf("r/%s/%d/%d/%d", layer, t.Z, t.X, t.Y)
	if b, ok := e.tileGet(key, noCache); ok {
		return &TileResult{Data: b, Cached: true}, nil
	}
	lg, err := e.Legend(ctx, layer)
	if err != nil {
		return nil, err
	}
	const size = 256
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	res := &TileResult{}
	grid := raster.Grid{OriginX: info.OriginX, OriginY: info.OriginY, PixelW: info.PixelWidth, PixelH: info.PixelHeight,
		Cols: info.Width, Rows: info.Height}
	if w, ok := grid.WindowFor(t.Bound(0)); ok && t.Z >= RasterTileMinZoom {
		start := time.Now()
		useCache := e.cache != nil && !noCache
		tr := &Trace{NoCache: noCache}
		blocks, err := e.readBlocks(ctx, info.ID, e.path(info.Path), grid, info.BlockSize, []*villageWindow{{window: w}}, tr, useCache)
		if err != nil {
			return nil, err
		}
		if tr.Cache.BlockMisses == 0 {
			res.BlockCached = true // every block was already decoded: no cog read ran
		} else {
			res.DuckDB = time.Since(start)
		}
		vals := cutWindow(w, blocks, blockSize(info.BlockSize))
		paint := e.painter(info, lg)
		n := float64(int(1) << t.Z)
		for py := 0; py < size; py++ {
			// Web Mercator row -> latitude (pixel centre).
			lat := math.Atan(math.Sinh(math.Pi*(1-2*(float64(t.Y)+(float64(py)+0.5)/size)/n))) * 180 / math.Pi
			row := int(math.Floor((info.OriginY-lat)/info.PixelHeight)) - w.Row0
			if row < 0 || row >= w.Rows {
				continue
			}
			for px := 0; px < size; px++ {
				lon := (float64(t.X)+(float64(px)+0.5)/size)/n*360 - 180
				col := int(math.Floor((lon-info.OriginX)/info.PixelWidth)) - w.Col0
				if col < 0 || col >= w.Cols {
					continue
				}
				if c, ok := paint(vals[row*w.Cols+col]); ok {
					img.SetNRGBA(px, py, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	res.Data = buf.Bytes()
	e.tileAdd(key, res.Data, noCache)
	return res, nil
}

// painter maps a stored pixel value to a colour (false: transparent).
func (e *Engine) painter(info *release.RasterInfo, lg *RasterLegend) func(float64) (color.NRGBA, bool) {
	nodata := func(v float64) bool { return math.IsNaN(v) || (info.Nodata != nil && v == *info.Nodata) }
	if info.IsCategorical() {
		cols := map[int64]color.NRGBA{}
		for _, c := range lg.Classes {
			cols[c.Value] = hexColor(c.Color)
		}
		return func(v float64) (color.NRGBA, bool) {
			if nodata(v) {
				return color.NRGBA{}, false
			}
			c, ok := cols[int64(v)]
			return c, ok
		}
	}
	ramp := make([]color.NRGBA, len(lg.Colors))
	for i, c := range lg.Colors {
		ramp[i] = hexColor(c)
	}
	scale := info.ScaleFactor()
	return func(v float64) (color.NRGBA, bool) {
		if nodata(v) || v <= 0 {
			return color.NRGBA{}, false
		}
		return ramp[sort.SearchFloat64s(lg.Breaks, v*scale)], true
	}
}

func hexColor(s string) color.NRGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(strings.TrimPrefix(s, "#"), "%02x%02x%02x", &r, &g, &b); err != nil {
		return color.NRGBA{128, 128, 128, 255}
	}
	return color.NRGBA{r, g, b, 255}
}

// tileGet / tileAdd use the tile cache (bytes, LRU by count).
func (e *Engine) tileGet(key string, noCache bool) ([]byte, bool) {
	if e.cache == nil || noCache {
		return nil, false
	}
	return e.cache.tiles.Get(key)
}

func (e *Engine) tileAdd(key string, b []byte, noCache bool) {
	if e.cache != nil && !noCache {
		e.cache.tiles.Add(key, b)
	}
}

// legends caches RasterLegend per layer (computed on first use).
type legendCache = sync.Map
