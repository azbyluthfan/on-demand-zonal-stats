// Command seed uploads bundled releases (sample/releases/<id>) to the object
// store so `docker compose up` serves data with no prep run. It is
// idempotent: a release whose manifest is already in the bucket with the
// same content is skipped.
//
//	seed [-dir /sample/releases]
package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"on-demand-zonal-stats/internal/prep"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dir := flag.String("dir", "/sample/releases", "directory of release directories")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	uc := prep.UploadConfig{
		Endpoint:  env("S3_ENDPOINT", "localhost:9000"),
		AccessKey: env("S3_ACCESS_KEY", "rustfsadmin"),
		SecretKey: env("S3_SECRET_KEY", "rustfsadmin"),
		UseSSL:    env("S3_USE_SSL", "false") == "true",
		Bucket:    env("S3_BUCKET", "villagestat"),
	}
	cl, err := minio.New(uc.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(uc.AccessKey, uc.SecretKey, ""),
		Secure: uc.UseSSL, BucketLookup: minio.BucketLookupPath})
	if err != nil {
		log.Fatal(err)
	}
	// Wait for the object store.
	for i := 0; ; i++ {
		if _, err := cl.BucketExists(ctx, uc.Bucket); err == nil {
			break
		} else if i == 60 {
			log.Fatalf("object store not reachable: %v", err)
		}
		time.Sleep(time.Second)
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rel := filepath.Join(*dir, e.Name())
		local, err := os.ReadFile(filepath.Join(rel, "manifest.json"))
		if err != nil {
			log.Printf("skip %s: %v", rel, err)
			continue
		}
		if remote := get(ctx, cl, uc.Bucket, path.Join("releases", e.Name(), "manifest.json")); bytes.Equal(remote, local) {
			log.Printf("release %s already present", e.Name())
			continue
		}
		if err := prep.Upload(ctx, rel, uc); err != nil {
			log.Fatalf("upload %s: %v", e.Name(), err)
		}
		log.Printf("release %s seeded", e.Name())
	}
}

func get(ctx context.Context, cl *minio.Client, bucket, key string) []byte {
	o, err := cl.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil
	}
	defer o.Close()
	b, err := io.ReadAll(o)
	if err != nil {
		return nil
	}
	return b
}
