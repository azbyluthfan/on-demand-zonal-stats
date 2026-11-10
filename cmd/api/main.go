// Command api serves the aggregation API (see docs/api.md).
//
// Configuration (env):
//
//	LISTEN_ADDR          :8080
//	RELEASE_BASE         s3://villagestat/releases/<id>  (or a local directory)
//	  or RELEASE_ID + S3_BUCKET
//	S3_ENDPOINT, S3_ACCESS_KEY, S3_SECRET_KEY, S3_REGION, S3_USE_SSL
//	DUCKDB_MEMORY_LIMIT  2GB      DUCKDB_THREADS 4     DUCKDB_EXT_DIR
//	MAX_VILLAGES         10000    QUERY_TIMEOUT  30s
//	WEB_DIR              web      (static frontend; empty disables)
//	RESULT_CACHE_ENTRIES 2000000  BLOCK_CACHE_MB 256   REF_CACHE_PROVINCES 4   TILE_CACHE_ENTRIES 4096
//	CACHE_DISABLED=1              (turn every cache off)
//	DEV_INSTALL_EXTENSIONS=1      (allow INSTALL at startup; dev only)
//
// Flags: -install-extensions (image build), -healthcheck (container probe).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"on-demand-zonal-stats/internal/api"
	"on-demand-zonal-stats/internal/query"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	install := flag.Bool("install-extensions", false, "install DuckDB extensions into DUCKDB_EXT_DIR and exit")
	health := flag.Bool("healthcheck", false, "GET /healthz on LISTEN_ADDR; exit 0 if healthy")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if *health {
		os.Exit(healthcheck())
	}
	if *install {
		if err := query.InstallExtensions(context.Background(), env("DUCKDB_EXT_DIR", "")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	limits := api.DefaultLimits()
	if v, err := strconv.Atoi(env("MAX_VILLAGES", "10000")); err == nil {
		limits.MaxVillages = v
	}
	if v, err := time.ParseDuration(env("QUERY_TIMEOUT", "30s")); err == nil {
		limits.QueryTimeout = v
	}
	srv := api.New(limits, log, env("WEB_DIR", "web"))

	opt, err := engineOptions()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go loadEngine(ctx, srv, opt, log)

	hs := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: limits.QueryTimeout + 30*time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", hs.Addr, "release", opt.Base)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func engineOptions() (query.Options, error) {
	threads, _ := strconv.Atoi(env("DUCKDB_THREADS", "4"))
	opt := query.Options{
		Base:         os.Getenv("RELEASE_BASE"),
		MemoryLimit:  env("DUCKDB_MEMORY_LIMIT", "2GB"),
		Threads:      threads,
		ExtensionDir: os.Getenv("DUCKDB_EXT_DIR"),
		AllowInstall: os.Getenv("DEV_INSTALL_EXTENSIONS") == "1",
		Cache: query.CacheConfig{
			Disabled:      os.Getenv("CACHE_DISABLED") == "1",
			ResultEntries: atoi(os.Getenv("RESULT_CACHE_ENTRIES")),
			BlockMB:       atoi(os.Getenv("BLOCK_CACHE_MB")),
			RefProvinces:  atoi(os.Getenv("REF_CACHE_PROVINCES")),
			TileEntries:   atoi(os.Getenv("TILE_CACHE_ENTRIES")),
		},
	}
	bucket := env("S3_BUCKET", "villagestat")
	if opt.Base == "" {
		id := os.Getenv("RELEASE_ID")
		if id == "" {
			return opt, errors.New("set RELEASE_BASE or RELEASE_ID")
		}
		opt.Base = "s3://" + bucket + "/releases/" + id
	}
	if strings.HasPrefix(opt.Base, "s3://") {
		opt.S3 = &query.S3Config{
			Endpoint:  env("S3_ENDPOINT", "localhost:9000"),
			AccessKey: env("S3_ACCESS_KEY", "rustfsadmin"),
			SecretKey: env("S3_SECRET_KEY", "rustfsadmin"),
			Region:    env("S3_REGION", "us-east-1"),
			UseSSL:    env("S3_USE_SSL", "false") == "true",
			Bucket:    bucket,
		}
	}
	return opt, nil
}

// loadEngine retries until the release loads; /healthz reports progress.
func loadEngine(ctx context.Context, srv *api.Server, opt query.Options, log *slog.Logger) {
	backoff := time.Second
	for {
		octx, cancel := context.WithTimeout(ctx, 30*time.Second)
		e, err := query.Open(octx, opt)
		cancel()
		if err == nil {
			srv.SetEngine(e)
			log.Info("release loaded", "release_id", e.Manifest.ReleaseID, "h3_resolution", e.Manifest.H3Resolution,
				"categories", len(e.Categories))
			return
		}
		srv.SetStartupError(err)
		log.Error("release not loaded; retrying", "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func healthcheck() int {
	addr := env("LISTEN_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
