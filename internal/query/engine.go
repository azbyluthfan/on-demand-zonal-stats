// Package query answers aggregation queries live from a release's files via
// embedded DuckDB (docs/query.md). No serving database: every
// query reads Parquet from the release root (RustFS or a local directory).
package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"

	"on-demand-zonal-stats/internal/release"
)

// Errors the API maps to status codes (docs/api.md).
var (
	ErrStorage         = errors.New("release storage unavailable")        // 503
	ErrTimeout         = errors.New("query timed out")                    // 504
	ErrTooManyVillages = errors.New("area resolves to too many villages") // 413
	ErrInvalidArea     = errors.New("invalid area")                       // 400
)

// S3Config points DuckDB's httpfs at the object store.
type S3Config struct {
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
	Bucket    string // secret scope
}

// Options configures an Engine.
type Options struct {
	// Base is the release root: "s3://<bucket>/releases/<id>" or a local dir.
	Base string
	S3   *S3Config // required when Base is s3://

	MemoryLimit string // DuckDB memory_limit, e.g. "2GB"
	Threads     int    // DuckDB threads
	// ExtensionDir is where extensions were pre-installed (image build).
	ExtensionDir string
	// AllowInstall lets the engine INSTALL missing extensions (dev/tests).
	// Serving images pre-install them and leave this false.
	AllowInstall bool

	Cache CacheConfig
}

// Engine is one replica's DuckDB instance plus the loaded release.
type Engine struct {
	db         *sql.DB
	base       string
	cache      *resultCaches // nil when caching is disabled
	refs       *refCache     // nil when caching is disabled
	Manifest   *release.Manifest
	Categories map[string]int64
}

var extensions = []struct{ name, repo string }{
	{"spatial", ""}, {"httpfs", ""}, {"h3", "community"}, {"cog", "community"},
}

// Open starts DuckDB, loads extensions, configures storage and reads the
// release manifest. It fails if the manifest is missing or not servable.
func Open(ctx context.Context, opt Options) (*Engine, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	e := &Engine{db: db, base: strings.TrimRight(opt.Base, "/")}
	if !strings.HasPrefix(e.base, "s3://") {
		// The cog extension resolves paths itself; give it absolute ones.
		if abs, err := filepath.Abs(e.base); err == nil {
			e.base = abs
		}
	}
	if opt.S3 != nil {
		cogEnv(opt.S3)
	}
	if e.cache, e.refs, err = newCaches(opt.Cache); err != nil {
		db.Close()
		return nil, err
	}
	if err := e.setup(ctx, opt); err != nil {
		db.Close()
		return nil, err
	}
	if err := e.loadRelease(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) setup(ctx context.Context, opt Options) error {
	c, err := e.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	exec := func(q string) error {
		if _, err := c.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("duckdb setup %q: %w", q, err)
		}
		return nil
	}
	if opt.ExtensionDir != "" {
		if err := exec("SET extension_directory = " + lit(opt.ExtensionDir)); err != nil {
			return err
		}
	}
	for _, x := range extensions {
		if opt.AllowInstall {
			stmt := "INSTALL " + x.name
			if x.repo != "" {
				stmt += " FROM " + x.repo
			}
			if err := exec(stmt); err != nil {
				return err
			}
		}
		if err := exec("LOAD " + x.name); err != nil {
			return err
		}
	}
	stmts := []string{
		"SET GLOBAL autoinstall_known_extensions = false",
		"SET GLOBAL autoload_known_extensions = false",
	}
	if opt.MemoryLimit != "" {
		stmts = append(stmts, "SET GLOBAL memory_limit = "+lit(opt.MemoryLimit))
	}
	if opt.Threads > 0 {
		stmts = append(stmts, fmt.Sprintf("SET GLOBAL threads = %d", opt.Threads))
	}
	if s := opt.S3; s != nil {
		region := s.Region
		if region == "" {
			region = "us-east-1"
		}
		scope := "s3://"
		if s.Bucket != "" {
			scope += s.Bucket
		}
		stmts = append(stmts, fmt.Sprintf(`CREATE OR REPLACE SECRET release_store (TYPE s3,
			KEY_ID %s, SECRET %s, ENDPOINT %s, REGION %s, URL_STYLE 'path', USE_SSL %t, SCOPE %s)`,
			lit(s.AccessKey), lit(s.SecretKey), lit(s.Endpoint), lit(region), s.UseSSL, lit(scope)))
	} else if strings.HasPrefix(e.base, "s3://") {
		return fmt.Errorf("query: base %s needs S3 config", e.base)
	}
	for _, s := range stmts {
		if err := exec(s); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) loadRelease(ctx context.Context) error {
	var m release.Manifest
	if err := e.readJSON(ctx, release.ManifestPath, &m); err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrStorage, err)
	}
	if err := m.Validate(); err != nil {
		return err
	}
	e.Manifest = &m
	var cats struct {
		Categories []struct {
			Category string `json:"category"`
			Count    int64  `json:"count"`
		} `json:"categories"`
	}
	if err := e.readJSON(ctx, release.CategoriesPath, &cats); err != nil {
		return fmt.Errorf("%w: categories: %v", ErrStorage, err)
	}
	e.Categories = make(map[string]int64, len(cats.Categories))
	for _, c := range cats.Categories {
		e.Categories[c.Category] = c.Count
	}
	return nil
}

func (e *Engine) readJSON(ctx context.Context, rel string, v any) error {
	var content string
	if err := e.db.QueryRowContext(ctx, "SELECT content FROM read_text("+lit(e.path(rel))+")").Scan(&content); err != nil {
		return err
	}
	return json.Unmarshal([]byte(content), v)
}

// cogEnv configures the cog extension's own S3 client, which ignores DuckDB
// secrets and reads AWS_* env vars (docs/spikes.md §2). Values already set
// in the environment win.
func cogEnv(s *S3Config) {
	scheme := "http://"
	if s.UseSSL {
		scheme = "https://"
	}
	region := s.Region
	if region == "" {
		region = "us-east-1"
	}
	for k, v := range map[string]string{
		"AWS_ENDPOINT": scheme + s.Endpoint, "AWS_ACCESS_KEY_ID": s.AccessKey, "AWS_SECRET_ACCESS_KEY": s.SecretKey,
		"AWS_REGION": region, "AWS_ALLOW_HTTP": strconv.FormatBool(!s.UseSSL),
		"AWS_VIRTUAL_HOSTED_STYLE_REQUEST": "false", "AWS_EC2_METADATA_DISABLED": "true",
	} {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// InstallExtensions installs the extensions into dir (image build step).
func InstallExtensions(ctx context.Context, dir string) error {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return err
	}
	defer db.Close()
	e := &Engine{db: db}
	return e.setup(ctx, Options{ExtensionDir: dir, AllowInstall: true})
}

// Close releases DuckDB.
func (e *Engine) Close() error { return e.db.Close() }

// Ping checks the release root is still reachable (for health checks).
func (e *Engine) Ping(ctx context.Context) error {
	// read_text on a missing local file yields zero rows, not an error.
	var n int64
	err := e.db.QueryRowContext(ctx, "SELECT count(*) FROM read_text("+lit(e.path(release.ManifestPath))+")").Scan(&n)
	if err == nil && n != 1 {
		err = fmt.Errorf("%w: %s not found", ErrStorage, release.ManifestPath)
	}
	return classify(ctx, err)
}

func (e *Engine) path(rel string) string {
	if strings.HasPrefix(e.base, "s3://") {
		return e.base + "/" + rel
	}
	return filepath.Join(e.base, filepath.FromSlash(rel))
}

// paths returns the full paths of the artifacts that exist in the manifest.
func (e *Engine) paths(rels []string) []string {
	var out []string
	for _, r := range rels {
		if e.Manifest.Has(r) {
			out = append(out, e.path(r))
		}
	}
	return out
}

// classify maps DuckDB/storage errors onto the API's error kinds.
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := err.Error()
	for _, s := range []string{"HTTP Error", "HTTP GET error", "IO Error", "No files found", "Could not establish connection", "Connection refused"} {
		if strings.Contains(msg, s) {
			return fmt.Errorf("%w: %v", ErrStorage, err)
		}
	}
	return err
}

func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func listLit(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = lit(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}
