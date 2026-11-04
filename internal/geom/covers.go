// Package geom holds the small planar geometry kernel shared by the raster
// and H3 code. Coordinates are lon/lat degrees treated as planar, which is
// what DuckDB spatial and exactextract also do for EPSG:4326 inputs.
//
// Supported geometries are orb.Polygon and orb.MultiPolygon. Polygons are
// assumed valid (prep runs ST_MakeValid). Ring orientation does not matter.
package geom

import (
	"fmt"
	"sort"

	"github.com/paulmach/orb"
)

// Covers reports whether p lies inside g or on its boundary, matching
// ST_Covers. Boundary tests are exact on the float64 inputs.
func Covers(g orb.Geometry, p orb.Point) bool {
	switch g := g.(type) {
	case orb.Polygon:
		return polygonCovers(g, p)
	case orb.MultiPolygon:
		for _, poly := range g {
			if polygonCovers(poly, p) {
				return true
			}
		}
		return false
	default:
		panic(fmt.Sprintf("geom: unsupported geometry %T", g))
	}
}

func polygonCovers(poly orb.Polygon, p orb.Point) bool {
	if len(poly) == 0 || !poly.Bound().Contains(p) {
		return false
	}
	if onRing(poly[0], p) {
		return true
	}
	if !ringContains(poly[0], p) {
		return false
	}
	for _, hole := range poly[1:] {
		if onRing(hole, p) {
			return true // the hole's edge is part of the polygon boundary
		}
		if ringContains(hole, p) {
			return false
		}
	}
	return true
}

// ContainsHalfOpen reports whether p is inside g using the half-open
// crossing rule: a point exactly on an edge belongs to exactly one of two
// polygons sharing that edge. This is the rule for pixel centres in the
// "center" raster method, so adjacent villages never count a pixel twice.
func ContainsHalfOpen(g orb.Geometry, p orb.Point) bool {
	switch g := g.(type) {
	case orb.Polygon:
		return polygonContainsHalfOpen(g, p)
	case orb.MultiPolygon:
		for _, poly := range g {
			if polygonContainsHalfOpen(poly, p) {
				return true
			}
		}
		return false
	default:
		panic(fmt.Sprintf("geom: unsupported geometry %T", g))
	}
}

func polygonContainsHalfOpen(poly orb.Polygon, p orb.Point) bool {
	in := false
	for _, r := range poly {
		if ringContains(r, p) {
			in = !in // even-odd over exterior and holes
		}
	}
	return in
}

// ringContains is the classic crossing-number test with a half-open rule
// in y and a strict comparison in x.
func ringContains(r orb.Ring, p orb.Point) bool {
	in := false
	n := len(r)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		a, b := r[i], r[j]
		if (a[1] > p[1]) != (b[1] > p[1]) {
			x := (b[0]-a[0])*(p[1]-a[1])/(b[1]-a[1]) + a[0]
			if p[0] < x {
				in = !in
			}
		}
	}
	return in
}

func onRing(r orb.Ring, p orb.Point) bool {
	n := len(r)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		if onSegment(r[j], r[i], p) {
			return true
		}
	}
	return false
}

// onSegment is exact: zero cross product and inside the segment's box.
func onSegment(a, b, p orb.Point) bool {
	if p[0] < min(a[0], b[0]) || p[0] > max(a[0], b[0]) ||
		p[1] < min(a[1], b[1]) || p[1] > max(a[1], b[1]) {
		return false
	}
	return (b[0]-a[0])*(p[1]-a[1])-(b[1]-a[1])*(p[0]-a[0]) == 0
}

// Candidate is a village that might contain a point.
type Candidate struct {
	ID   string
	Geom orb.Geometry
}

// Assign applies the edge rule: among candidates that cover p, return the
// lowest ID (byte-wise string order). ok is false when no candidate covers p,
// i.e. the point is unassigned and must be counted, not dropped.
func Assign(p orb.Point, cands []Candidate) (id string, ok bool) {
	for _, c := range cands {
		if (!ok || c.ID < id) && Covers(c.Geom, p) {
			id, ok = c.ID, true
		}
	}
	return id, ok
}

// SortCandidates orders candidates by ID; handy for deterministic output.
func SortCandidates(c []Candidate) {
	sort.Slice(c, func(i, j int) bool { return c[i].ID < c[j].ID })
}
