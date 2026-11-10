package query

import (
	lru "github.com/hashicorp/golang-lru/v2"

	"on-demand-zonal-stats/internal/raster"
)

// CacheConfig sizes the per-replica caches (docs/caching.md). Zero values take
// defaults; Disabled turns every cache off.
type CacheConfig struct {
	Disabled bool
	// ResultEntries bounds the result cache: (village, POI category) and
	// (village, layer, method) entries. Default 2,000,000 (~200 MB).
	ResultEntries int
	// BlockMB bounds the decoded COG block cache. Default 256 MB
	// (a 512x512 block of float64 is 2 MB).
	BlockMB int
	// RefProvinces is how many provinces the reference cache keeps loaded.
	// Default 4.
	RefProvinces int
	// TileEntries bounds the encoded map tile cache. Default 4,096 tiles
	// (a busy vector tile is ~50-200 KB).
	TileEntries int
}

type rasterKey struct{ village, layer, method string }

type blockCacheKey struct {
	layer  string
	bx, by int
}

// resultCaches are keyed within one engine, i.e. one release: the release
// ID is implicit in the key.
type resultCaches struct {
	poi       *lru.Cache[poiKey, poiVal]
	buildings *lru.Cache[string, buildingVal] // village ID
	raster    *lru.Cache[rasterKey, raster.Result]
	blocks    *lru.Cache[blockCacheKey, []float64]
	tiles     *lru.Cache[string, []byte] // encoded map tiles (docs/tiles.md)
}

func newCaches(cfg CacheConfig) (*resultCaches, *refCache, error) {
	if cfg.Disabled {
		return nil, nil, nil
	}
	if cfg.ResultEntries <= 0 {
		cfg.ResultEntries = 2_000_000
	}
	if cfg.BlockMB <= 0 {
		cfg.BlockMB = 256
	}
	if cfg.RefProvinces <= 0 {
		cfg.RefProvinces = 4
	}
	if cfg.TileEntries <= 0 {
		cfg.TileEntries = 4096
	}
	poi, err := lru.New[poiKey, poiVal](cfg.ResultEntries)
	if err != nil {
		return nil, nil, err
	}
	bld, err := lru.New[string, buildingVal](max(cfg.ResultEntries/10, 1000))
	if err != nil {
		return nil, nil, err
	}
	rs, err := lru.New[rasterKey, raster.Result](max(cfg.ResultEntries/10, 1000))
	if err != nil {
		return nil, nil, err
	}
	blocks, err := lru.New[blockCacheKey, []float64](max(cfg.BlockMB/2, 1))
	if err != nil {
		return nil, nil, err
	}
	tiles, err := lru.New[string, []byte](cfg.TileEntries)
	if err != nil {
		return nil, nil, err
	}
	return &resultCaches{poi: poi, buildings: bld, raster: rs, blocks: blocks, tiles: tiles},
		&refCache{m: map[string]*refEntry{}, cap: cfg.RefProvinces}, nil
}

// CacheSizes reports current cache occupancy.
type CacheSizes struct {
	POIEntries      int            `json:"poi_entries"`
	BuildingEntries int            `json:"building_entries"`
	RasterEntries   int            `json:"raster_entries"`
	Blocks          int            `json:"blocks"`
	Tiles           int            `json:"tiles"`
	Reference       *RefCacheStats `json:"reference,omitempty"`
}

// CacheSizes reports current occupancy (zero when caching is disabled).
func (e *Engine) CacheSizes() CacheSizes {
	if e.cache == nil {
		return CacheSizes{}
	}
	ref := e.ReferenceCache()
	return CacheSizes{POIEntries: e.cache.poi.Len(), BuildingEntries: e.cache.buildings.Len(), RasterEntries: e.cache.raster.Len(), Blocks: e.cache.blocks.Len(), Tiles: e.cache.tiles.Len(), Reference: &ref}
}
