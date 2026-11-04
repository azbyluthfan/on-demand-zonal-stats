package query

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"

	"github.com/duckdb/duckdb-go/v2"
)

// Trace carries per-request cache policy and collects diagnostics: cache
// hits/misses and DuckDB profiling metrics per stage. Safe for concurrent use.
type Trace struct {
	// NoCache bypasses every cache (results, blocks, reference tables) so a
	// request measures the cold path. Results are identical either way.
	NoCache bool

	mu      sync.Mutex
	Cache   CacheCounts
	Profile map[string]map[string]float64 // stage -> metric -> summed value
}

// CacheCounts are per-request cache statistics.
type CacheCounts struct {
	ResultHits   int64 `json:"result_hits"`   // (village, metric part) lookups served from cache
	ResultMisses int64 `json:"result_misses"` // ... computed
	BlockHits    int64 `json:"block_hits"`    // decoded COG blocks from cache
	BlockMisses  int64 `json:"block_misses"`  // ... read via cog
	RefHits      int64 `json:"reference_hits"`
	RefMisses    int64 `json:"reference_misses"` // provinces loaded into the reference cache
}

func (t *Trace) add(f func(c *CacheCounts)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	f(&t.Cache)
	t.mu.Unlock()
}

// profiled metrics: root-level, summed over a stage's queries.
var rootMetrics = map[string]string{
	"LATENCY": "latency_s", "CPU_TIME": "cpu_time_s", "TOTAL_BYTES_READ": "bytes_read",
	"CUMULATIVE_ROWS_SCANNED": "rows_scanned", "BLOCKED_THREAD_TIME": "blocked_thread_s",
	"SYSTEM_PEAK_BUFFER_MEMORY": "peak_buffer_bytes",
}

// enableProfiling turns on in-memory profiling for this connection.
func enableProfiling(ctx context.Context, c *sql.Conn) {
	_, _ = c.ExecContext(ctx, "PRAGMA enable_profiling = 'no_output'")
	_, _ = c.ExecContext(ctx, "PRAGMA profiling_mode = 'detailed'")
}

// disableProfiling resets the pooled connection.
func disableProfiling(c *sql.Conn) {
	_, _ = c.ExecContext(context.Background(), "PRAGMA disable_profiling")
}

// collect adds the last query's profile on c to stage.
func (t *Trace) collect(c *sql.Conn, stage string) {
	if t == nil {
		return
	}
	info, err := duckdb.GetProfilingInfo(c)
	if err != nil {
		return
	}
	vals := map[string]float64{}
	for k, name := range rootMetrics {
		if v, err := strconv.ParseFloat(info.Metrics[k], 64); err == nil {
			vals[name] = v
		}
	}
	// Row-group pruning lives on the scan operators.
	var walk func(n duckdb.ProfilingInfo)
	walk = func(n duckdb.ProfilingInfo) {
		for k, v := range n.Metrics {
			if strings.Contains(k, "ROW_GROUPS") {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					vals[strings.ToLower(k)] += f
				}
			}
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(info)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Profile == nil {
		t.Profile = map[string]map[string]float64{}
	}
	st := t.Profile[stage]
	if st == nil {
		st = map[string]float64{}
		t.Profile[stage] = st
	}
	for k, v := range vals {
		if k == "peak_buffer_bytes" {
			st[k] = max(st[k], v)
		} else {
			st[k] += v
		}
	}
	st["queries"]++
}
