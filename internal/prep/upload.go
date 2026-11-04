package prep

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// UploadConfig points at the object store.
type UploadConfig struct {
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	UseSSL    bool
	Bucket    string
}

// Upload copies a built release to s3://<bucket>/releases/<id>/. It refuses
// releases whose manifest status is not "ok". The manifest is uploaded last,
// so a reader that finds a manifest always finds every artifact it lists.
func Upload(ctx context.Context, releaseDir string, uc UploadConfig) error {
	raw, err := os.ReadFile(filepath.Join(releaseDir, "manifest.json"))
	if err != nil {
		return err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.Status != "ok" {
		return fmt.Errorf("upload: release %s has status %q; fix QC before uploading", m.ReleaseID, m.Status)
	}
	cl, err := minio.New(uc.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(uc.AccessKey, uc.SecretKey, ""),
		Secure:       uc.UseSSL,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return err
	}
	ok, err := cl.BucketExists(ctx, uc.Bucket)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if !ok {
		if err := cl.MakeBucket(ctx, uc.Bucket, minio.MakeBucketOptions{}); err != nil {
			return err
		}
	}
	prefix := path.Join("releases", m.ReleaseID)
	put := func(rel string) error {
		ct := mime.TypeByExtension(path.Ext(rel))
		switch path.Ext(rel) {
		case ".parquet":
			ct = "application/vnd.apache.parquet"
		case ".tif":
			ct = "image/tiff; application=geotiff; profile=cloud-optimized"
		}
		_, err := cl.FPutObject(ctx, uc.Bucket, path.Join(prefix, rel), filepath.Join(releaseDir, filepath.FromSlash(rel)),
			minio.PutObjectOptions{ContentType: ct})
		if err == nil {
			logf("uploaded s3://%s/%s", uc.Bucket, path.Join(prefix, rel))
		}
		return err
	}
	for _, a := range m.Artifacts {
		if err := put(a.Path); err != nil {
			return err
		}
	}
	return put("manifest.json")
}
