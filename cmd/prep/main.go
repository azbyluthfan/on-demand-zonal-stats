// Command prep builds and uploads data releases.
//
//	prep build  -config configs/fixture.json [-out out] [-skip-rasters]
//	prep upload -dir out/releases/<id> [-bucket villagestat]
//	prep install-extensions        (image build: bake DuckDB extensions)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"on-demand-zonal-stats/internal/prep"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch os.Args[1] {
	case "build":
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		cfgPath := fs.String("config", "", "release config JSON")
		out := fs.String("out", "out", "output root (release goes to <out>/releases/<id>)")
		skip := fs.Bool("skip-rasters", false, "skip raster steps (no GDAL needed)")
		_ = fs.Parse(os.Args[2:])
		if *cfgPath == "" {
			usage()
		}
		cfg, err := prep.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := prep.Build(ctx, cfg, *out, prep.Options{SkipRasters: *skip}); err != nil {
			var be *prep.BlockingError
			if errors.As(err, &be) {
				log.Printf("%v (see manifest.json qc section)", be)
				os.Exit(2)
			}
			log.Fatal(err)
		}
	case "upload":
		fs := flag.NewFlagSet("upload", flag.ExitOnError)
		dir := fs.String("dir", "", "built release directory")
		bucket := fs.String("bucket", env("S3_BUCKET", "villagestat"), "bucket")
		_ = fs.Parse(os.Args[2:])
		if *dir == "" {
			usage()
		}
		err := prep.Upload(ctx, *dir, prep.UploadConfig{
			Endpoint:  env("S3_ENDPOINT", "localhost:9000"),
			AccessKey: env("S3_ACCESS_KEY", "rustfsadmin"),
			SecretKey: env("S3_SECRET_KEY", "rustfsadmin"),
			UseSSL:    env("S3_USE_SSL", "false") == "true",
			Bucket:    *bucket,
		})
		if err != nil {
			log.Fatal(err)
		}
	case "install-extensions":
		if err := prep.InstallExtensions(ctx); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  prep build  -config <file> [-out out] [-skip-rasters]
  prep upload -dir out/releases/<id> [-bucket villagestat]
  prep install-extensions`)
	os.Exit(1)
}
