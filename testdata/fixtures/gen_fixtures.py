"""Regenerate pois.geojson, population.asc, landcover.asc and buildings.geojson
(deterministic).

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

# --- Land cover raster (categorical, same grid) -------------------------------
# Class by column band, so village class fractions have closed forms:
# lon < 110.32 -> 10, < 110.36 -> 40, < 110.40 -> 50, else 80. The
# top-left 10x10 block is nodata (0), like population.asc.
def lc_class(c):
    if c < 40:
        return 10
    if c < 80:
        return 40
    if c < 120:
        return 50
    return 80


lines = [f"ncols {ncols}", f"nrows {nrows}", f"xllcorner {xll}", f"yllcorner {yll}",
         f"cellsize {cell}", "NODATA_value 0"]
for r in range(nrows):
    lines.append(" ".join("0" if (r < 10 and c < 10) else str(lc_class(c)) for c in range(ncols)))
with open(os.path.join(HERE, "landcover.asc"), "w") as f:
    f.write("\n".join(lines) + "\n")

# --- Buildings (axis-aligned rectangles) ------------------------------------
# Centres and half-sizes are on the 1e-7 degree grid, so each rectangle's
# point on surface (its centre) is exact after prep's 1e-7 rounding.
buildings = []


def building(cx_e7, cy_e7, hw_e7, hh_e7, note=""):
    i = len(buildings) + 1
    x0, x1 = (cx_e7 - hw_e7) / 1e7, (cx_e7 + hw_e7) / 1e7
    y0, y1 = (cy_e7 - hh_e7) / 1e7, (cy_e7 + hh_e7) / 1e7
    buildings.append({
        "type": "Feature",
        "properties": {"building_id": f"B{i:04d}", "note": note},
        "geometry": {"type": "Polygon", "coordinates": [[[x0, y0], [x1, y0], [x1, y1], [x0, y1], [x0, y0]]]},
    })


building(1103200000, -78100000, 1000, 800, "centre on V001|V002 border -> V001")
building(1103201000, -78150000, 2000, 500, "straddles the border, centre in V002")
building(1103800000, -78200000, 1500, 1500, "inside V004 hole -> unassigned")
building(1104201000, -78004000, 300, 300, "inside tiny V005")
building(1104650000, -78150000, 1000, 1000, "second island of V006")
building(1106000000, -79000000, 1000, 1000, "far outside everything -> dropped")
for _ in range(400):
    building(rng.randrange(1102900000, 1104800000), rng.randrange(-78700000, -77900000),
             rng.randrange(300, 2000), rng.randrange(300, 2000))
with open(os.path.join(HERE, "buildings.geojson"), "w") as f:
    json.dump({"type": "FeatureCollection", "features": buildings}, f, indent=0)
    f.write("\n")

print(f"pois: {len(pois)}, rasters: {ncols}x{nrows}, buildings: {len(buildings)}")
