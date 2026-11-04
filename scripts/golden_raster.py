"""Independent raster references for the golden tests (docs/raster.md).

    fractional: exactextract "sum" (exact coverage-weighted sum)
    center:     rasterstats zonal_stats(all_touched=False) "sum"
    union:      exactextract sum over the dissolved union of all villages

Run in a throwaway container (see Makefile target `golden-raster`):
    python golden_raster.py <villages.geojson> <raster.tif> <out.csv>

Never used in serving.
"""
import csv
import sys
from importlib.metadata import version

import fiona
import rasterio
from exactextract import exact_extract
from rasterstats import zonal_stats
from shapely.geometry import mapping, shape
from shapely.ops import unary_union

villages_path, raster_path, out_path = sys.argv[1:4]

with fiona.open(villages_path) as src:
    feats = [(f["properties"]["village_id"], shape(f["geometry"])) for f in src]

with rasterio.open(raster_path) as r:
    nodata = r.nodata

# exactextract on the vector file directly.
ee = exact_extract(raster_path, villages_path, "sum", include_cols=["village_id"], output="pandas")
frac = dict(zip(ee["village_id"], ee["sum"]))

# rasterstats with pixel-centre rule; float64 accumulation for a fair comparison.
zs = zonal_stats([mapping(g) for _, g in feats], raster_path, stats=["sum"], all_touched=False,
                 nodata=nodata, add_stats=None)
center = {vid: (z["sum"] or 0.0) for (vid, _), z in zip(feats, zs)}

union = unary_union([g for _, g in feats])
ue = exact_extract(raster_path, [{"type": "Feature", "properties": {"id": "union"}, "geometry": mapping(union)}],
                   "sum", output="pandas")
union_sum = float(ue["sum"][0])

with open(out_path, "w", newline="") as f:
    f.write(f"# exactextract {version('exactextract')}, rasterstats {version('rasterstats')}, "
            f"rasterio {version('rasterio')}, gdal {rasterio.__gdal_version__}\n")
    f.write(f"# union_fractional_sum {union_sum!r}\n")
    w = csv.writer(f)
    w.writerow(["village_id", "fractional_sum", "center_sum"])
    for vid, _ in sorted(feats):
        w.writerow([vid, repr(float(frac.get(vid, 0.0) or 0.0)), repr(float(center[vid]))])
print(f"wrote {out_path}: {len(feats)} villages, union sum {union_sum:.3f}")
