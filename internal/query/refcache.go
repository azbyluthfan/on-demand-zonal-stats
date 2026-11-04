package query

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"

	"on-demand-zonal-stats/internal/release"
)

// refCache is the reference cache (docs/caching.md): per province, the
// village_h3 rows and village geometries loaded into DuckDB tables once,
// plus decoded geometries in Go. LRU by province; entries in use are never
// evicted.
type refCache struct {
	mu   sync.Mutex
	m    map[string]*refEntry
	cap  int
	seq  int
	tick int64
}

type refEntry struct {
	province string
	h3Table  string // h3_cell, village_id, cell_type (sorted by h3_cell)
	vTable   string // village_id, geometry
	geoms    map[string]orb.Geometry
	ready    chan struct{}
	err      error
	refs     int
	lastUse  int64
	loadTime time.Duration
}

var safeIdent = regexp.MustCompile(`[^A-Za-z0-9_]`)

// refs is a set of acquired entries; call release when the request is done.
type refs struct {
	entries []*refEntry
	release func()
}

func (r *refs) h3Src() string {
	return unionOf(r.entries, func(e *refEntry) string { return e.h3Table })
}
func (r *refs) vSrc() string { return unionOf(r.entries, func(e *refEntry) string { return e.vTable }) }

func unionOf(es []*refEntry, name func(*refEntry) string) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = "SELECT * FROM " + name(e)
	}
	return "(" + strings.Join(parts, " UNION ALL ") + ")"
}

func (r *refs) geom(id string) (orb.Geometry, bool) {
	for _, e := range r.entries {
		if g, ok := e.geoms[id]; ok {
			return g, true
		}
	}
	return nil, false
}

// acquireRefs returns the reference entries for the provinces, loading
// missing ones (concurrent loads of one province are shared).
func (e *Engine) acquireRefs(ctx context.Context, provinces []string, tr *Trace) (*refs, error) {
	rc := e.refs
	var mine []*refEntry
	var toLoad []*refEntry
	rc.mu.Lock()
	for _, p := range provinces {
		en, ok := rc.m[p]
		if !ok {
			rc.seq++
			id := fmt.Sprintf("%s_%d", safeIdent.ReplaceAllString(p, "_"), rc.seq)
			en = &refEntry{province: p, h3Table: "ref_h3_" + id, vTable: "ref_v_" + id, ready: make(chan struct{})}
			rc.m[p] = en
			toLoad = append(toLoad, en)
			tr.add(func(c *CacheCounts) { c.RefMisses++ })
		} else {
			tr.add(func(c *CacheCounts) { c.RefHits++ })
		}
		en.refs++
		rc.tick++
		en.lastUse = rc.tick
		mine = append(mine, en)
	}
	rc.mu.Unlock()

	release := func() {
		rc.mu.Lock()
		for _, en := range mine {
			en.refs--
		}
		evicted := rc.evictLocked()
		rc.mu.Unlock()
		e.dropRefs(evicted)
	}
	for _, en := range toLoad {
		// Loads use a background context: other requests may wait on them.
		en.err = e.loadRef(context.Background(), en)
		if en.err != nil {
			rc.mu.Lock()
			if rc.m[en.province] == en {
				delete(rc.m, en.province)
			}
			rc.mu.Unlock()
		}
		close(en.ready)
	}
	for _, en := range mine {
		select {
		case <-en.ready:
		case <-ctx.Done():
			release()
			return nil, classify(ctx, ctx.Err())
		}
		if en.err != nil {
			release()
			return nil, en.err
		}
	}
	return &refs{entries: mine, release: release}, nil
}

// evictLocked removes least-recently-used idle entries above capacity.
func (rc *refCache) evictLocked() []*refEntry {
	if len(rc.m) <= rc.cap {
		return nil
	}
	var idle []*refEntry
	for _, en := range rc.m {
		select {
		case <-en.ready:
			if en.refs == 0 {
				idle = append(idle, en)
			}
		default:
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUse < idle[j].lastUse })
	var out []*refEntry
	for _, en := range idle {
		if len(rc.m) <= rc.cap {
			break
		}
		delete(rc.m, en.province)
		out = append(out, en)
	}
	return out
}

func (e *Engine) dropRefs(es []*refEntry) {
	for _, en := range es {
		_, _ = e.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+en.h3Table)
		_, _ = e.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+en.vTable)
	}
}

func (e *Engine) loadRef(ctx context.Context, en *refEntry) error {
	start := time.Now()
	h3Path := release.VillageH3Path(en.province)
	h3Sel := "SELECT NULL::UBIGINT AS h3_cell, NULL::VARCHAR AS village_id, NULL::VARCHAR AS cell_type WHERE false"
	if e.Manifest.Has(h3Path) {
		h3Sel = fmt.Sprintf("SELECT h3_cell, village_id, cell_type FROM read_parquet(%s) ORDER BY h3_cell", lit(e.path(h3Path)))
	}
	if _, err := e.db.ExecContext(ctx, "CREATE TABLE "+en.h3Table+" AS "+h3Sel); err != nil {
		return classify(ctx, err)
	}
	if _, err := e.db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s AS SELECT village_id, geometry
		FROM read_parquet(%s) WHERE province_code = %s`, en.vTable, lit(e.path(release.VillagesPath)), lit(en.province))); err != nil {
		e.dropRefs([]*refEntry{en})
		return classify(ctx, err)
	}
	rows, err := e.db.QueryContext(ctx, "SELECT village_id, ST_AsWKB(geometry) FROM "+en.vTable)
	if err != nil {
		e.dropRefs([]*refEntry{en})
		return classify(ctx, err)
	}
	defer rows.Close()
	en.geoms = map[string]orb.Geometry{}
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		g, err := wkb.Unmarshal(b)
		if err != nil {
			return fmt.Errorf("village %s: %w", id, err)
		}
		en.geoms[id] = g
	}
	en.loadTime = time.Since(start)
	return classify(ctx, rows.Err())
}

// RefCacheStats describes the reference cache (for /healthz or logs).
type RefCacheStats struct {
	Provinces []string `json:"provinces"`
	Capacity  int      `json:"capacity"`
}

// ReferenceCache reports which provinces are loaded.
func (e *Engine) ReferenceCache() RefCacheStats {
	e.refs.mu.Lock()
	defer e.refs.mu.Unlock()
	s := RefCacheStats{Capacity: e.refs.cap, Provinces: []string{}}
	for p := range e.refs.m {
		s.Provinces = append(s.Provinces, p)
	}
	sort.Strings(s.Provinces)
	return s
}
