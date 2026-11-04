"""Regenerate pois.geojson and population.asc (deterministic).

Run from the repo root:  python3 -I testdata/fixtures/gen_fixtures.py
The outputs are checked in; rerun only to change the fixtures.
"""
import json
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))
rng = random.Random(42)
CATS = ["cafe", "coffee_shop", "restaurant", "school", "mosque"]

# --- POIs -------------------------------------------------------------------
pois = []


def add(lon, lat, cat, note=""):
    i = len(pois) + 1
    pois.append({
        "type": "Feature",
        "properties": {"poi_id": f"P{i:04d}", "name": f"POI {i}", "category": cat, "note": note},
        "geometry": {"type": "Point", "coordinates": [round(lon, 7), round(lat, 7)]},
    })


# Hand-placed edge cases.
for y in (-7.819, -7.815, -7.81, -7.805, -7.801):
    add(110.32, y, "cafe", "on V001|V002 border -> V001")
add(110.32, -7.80, "cafe", "on V001|V002 shared corner -> V001")
add(110.38, -7.82, "cafe", "inside V004 hole -> unassigned")
add(110.37, -7.82, "cafe", "on V004 hole edge -> V004")
add(110.4201, -7.8004, "coffee_shop", "inside tiny V005")
add(110.465, -7.815, "restaurant", "second island of V006")
add(110.455, -7.815, "restaurant", "between V006 islands -> unassigned")
add(110.60, -7.90, "school", "far outside everything -> unassigned")

# Random background points over the fixture extent.
for _ in range(500):
    add(rng.uniform(110.29, 110.48), rng.uniform(-7.87, -7.79), rng.choice(CATS))

with open(os.path.join(HERE, "pois.geojson"), "w") as f:
    json.dump({"type": "FeatureCollection", "features": pois}, f, indent=0)
    f.write("\n")

# --- Population raster (ESRI ASCII grid) ------------------------------------
# 200 x 100 pixels of 0.001 deg over lon 110.28..110.48, lat -7.88..-7.78.
# Values vary per pixel (so orientation/row-major bugs show up); the
# top-left 10x10 block is nodata.
ncols, nrows, cell = 200, 100, 0.001
xll, yll, nodata = 110.28, -7.88, -9999
lines = [f"ncols {ncols}", f"nrows {nrows}", f"xllcorner {xll}", f"yllcorner {yll}",
         f"cellsize {cell}", f"NODATA_value {nodata}"]
for r in range(nrows):  # r = 0 is the top (northern) row
    row = []
    for c in range(ncols):
        if r < 10 and c < 10:
            row.append(str(nodata))
        else:
            row.append(f"{1 + ((c * 7 + r * 3) % 11) + 0.25 * (c % 4):.2f}")
    lines.append(" ".join(row))
with open(os.path.join(HERE, "population.asc"), "w") as f:
    f.write("\n".join(lines) + "\n")

print(f"pois: {len(pois)}, raster: {ncols}x{nrows}")
