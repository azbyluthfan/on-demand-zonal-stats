package prep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"on-demand-zonal-stats/internal/release"
)

// ErrNoGDAL is returned when gdal_translate/gdalinfo are not on PATH.
var ErrNoGDAL = errors.New("prep: GDAL tools not found (run prep in its Docker image)")

// RasterInfo is the companion <id>.json (see internal/release).
type RasterInfo = release.RasterInfo

type gdalInfo struct {
	Size         [2]int    `json:"size"`
	GeoTransform []float64 `json:"geoTransform"`
	Metadata     map[string]map[string]string
	Bands        []struct {
		Block       [2]int   `json:"block"`
		Type        string   `json:"type"`
		NoDataValue *float64 `json:"noDataValue"`
		Overviews   []any    `json:"overviews"`
	} `json:"bands"`
}

func runGDAL(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, ErrNoGDAL
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GDAL_PAM_ENABLED=NO") // no .aux.xml side files
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s: %w: %s", name, err, ee.Stderr)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func readGDALInfo(ctx context.Context, path string) (*gdalInfo, error) {
	out, err := runGDAL(ctx, "gdalinfo", "-json", path)
	if err != nil {
		return nil, err
	}
	var gi gdalInfo
	if err := json.Unmarshal(out, &gi); err != nil {
		return nil, err
	}
	if len(gi.GeoTransform) != 6 || len(gi.Bands) == 0 {
		return nil, fmt.Errorf("gdalinfo %s: missing geotransform or bands", path)
	}
	return &gi, nil
}

func gdalPath(p string) string {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return "/vsicurl/" + p
	}
	return p
}

// buildRaster converts one source raster to a COG plus companion JSON.
func buildRaster(ctx context.Context, d *db, cfg *Config, rs RasterSource, out string, qc *qcLog) (*RasterInfo, error) {
	src := gdalPath(rs.Path)
	rel := filepath.Join("rasters", rs.ID+".tif")
	dst := filepath.Join(out, rel)
	if err := mkdirFor(dst); err != nil {
		return nil, err
	}
	args := []string{"-of", "COG",
		"-co", "BLOCKSIZE=512", "-co", "COMPRESS=DEFLATE", "-co", "PREDICTOR=YES",
		"-co", "OVERVIEWS=AUTO", "-co", "RESAMPLING=AVERAGE", "-co", "BIGTIFF=IF_SAFER"}
	if rs.SRS != "" {
		args = append(args, "-a_srs", rs.SRS)
	}
	if rs.ClipToVillages {
		srcInfo, err := readGDALInfo(ctx, src)
		if err != nil {
			return nil, err
		}
		pad := 2 * math.Abs(srcInfo.GeoTransform[1])
		var xmin, ymin, xmax, ymax float64
		if err := d.queryRow(ctx, `SELECT min(bbox.xmin), min(bbox.ymin), max(bbox.xmax), max(bbox.ymax) FROM villages`).
			Scan(&xmin, &ymin, &xmax, &ymax); err != nil {
			return nil, err
		}
		args = append(args, "-projwin",
			fmt.Sprint(xmin-pad), fmt.Sprint(ymax+pad), fmt.Sprint(xmax+pad), fmt.Sprint(ymin-pad))
	}
	args = append(args, src, dst)
	logf("raster %s: gdal_translate %s", rs.ID, strings.Join(args, " "))
	if _, err := runGDAL(ctx, "gdal_translate", args...); err != nil {
		return nil, err
	}

	gi, err := readGDALInfo(ctx, dst)
	if err != nil {
		return nil, err
	}
	gt, b := gi.GeoTransform, gi.Bands[0]
	if gt[2] != 0 || gt[4] != 0 || gt[5] >= 0 {
		return nil, fmt.Errorf("raster %s: rotated or south-up grids are not supported (geotransform %v)", rs.ID, gt)
	}
	info := &RasterInfo{
		ID: rs.ID, Path: rel, CRS: "EPSG:4326",
		Width: gi.Size[0], Height: gi.Size[1],
		OriginX: gt[0], OriginY: gt[3], PixelWidth: gt[1], PixelHeight: -gt[5],
		DataType: b.Type, Nodata: b.NoDataValue, Units: rs.Units, Year: rs.Year,
		BlockSize: b.Block, Overviews: len(b.Overviews), Source: rs.Source,
	}
	info.Bounds = [4]float64{gt[0], gt[3] + gt[5]*float64(gi.Size[1]), gt[0] + gt[1]*float64(gi.Size[0]), gt[3]}

	// QC: COG validity (blocking).
	layout := gi.Metadata["IMAGE_STRUCTURE"]["LAYOUT"]
	cogOK := layout == "COG" && b.Block == [2]int{512, 512} && (info.Overviews > 0 || max(info.Width, info.Height) <= 512)
	qc.add(QCResult{Name: "cog_validity:" + rs.ID, Blocking: true, Passed: cogOK,
		Value: map[string]any{"layout": layout, "block": b.Block, "overviews": info.Overviews,
			"compression": gi.Metadata["IMAGE_STRUCTURE"]["COMPRESSION"]}})

	// Exact sum by reading the COG back through the cog extension, in
	// pixel-aligned tiles. This also exercises the serving read path.
	sum, valid, tileErr := cogSum(ctx, d, dst, info)
	info.Sum, info.ValidPixels = sum, valid
	r := QCResult{Name: "raster_sanity:" + rs.ID, Blocking: false, Passed: tileErr == nil,
		Value: map[string]any{"sum": sum, "valid_pixels": valid}, Error: errStr(tileErr)}
	if rs.ExpectedTotal != nil && tileErr == nil {
		rel := math.Abs(sum-*rs.ExpectedTotal) / *rs.ExpectedTotal
		r.Passed = rel <= cfg.QC.RasterSumTolerance
		r.Value = map[string]any{"sum": sum, "expected": *rs.ExpectedTotal, "rel_diff": rel, "valid_pixels": valid}
	}
	qc.add(r)

	if err := writeJSON(filepath.Join(out, "rasters", rs.ID+".json"), info); err != nil {
		return nil, err
	}
	return info, nil
}

// cogSum sums all valid pixels via RS_BandAsArray over pixel-aligned tiles.
// A tile whose returned length differs from its pixel count is an error:
// serving relies on exact windows.
func cogSum(ctx context.Context, d *db, path string, info *RasterInfo) (float64, int64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, 0, err
	}
	const tile = 2048
	var sum float64
	var valid int64
	for r0 := 0; r0 < info.Height; r0 += tile {
		for c0 := 0; c0 < info.Width; c0 += tile {
			nc, nr := min(tile, info.Width-c0), min(tile, info.Height-r0)
			xmin := info.OriginX + float64(c0)*info.PixelWidth
			xmax := info.OriginX + float64(c0+nc)*info.PixelWidth
			ymax := info.OriginY - float64(r0)*info.PixelHeight
			ymin := info.OriginY - float64(r0+nr)*info.PixelHeight
			nd := "NULL"
			if info.Nodata != nil {
				nd = fmt.Sprint(*info.Nodata)
			}
			var n, cnt int64
			var s float64
			err := d.queryRow(ctx, fmt.Sprintf(`WITH a AS (SELECT RS_BandAsArray(%s, 1, [%v, %v, %v, %v]) AS v)
				SELECT len(v),
					coalesce(list_sum(list_filter(v, x -> NOT isnan(x) AND x IS DISTINCT FROM %s)), 0),
					len(list_filter(v, x -> NOT isnan(x) AND x IS DISTINCT FROM %s))
				FROM a`, lit(abs), xmin, ymin, xmax, ymax, nd, nd)).Scan(&n, &s, &cnt)
			if err != nil {
				return sum, valid, fmt.Errorf("tile (%d,%d): %w", c0, r0, err)
			}
			if n != int64(nc*nr) {
				return sum, valid, fmt.Errorf("tile (%d,%d): RS_BandAsArray returned %d values for a %dx%d pixel-aligned window", c0, r0, n, nc, nr)
			}
			sum += s
			valid += cnt
		}
	}
	return sum, valid, nil
}
