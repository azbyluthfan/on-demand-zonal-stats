package geom

import (
	"fmt"
	"math"

	"github.com/paulmach/orb"
)

// Path is an open ring: the closing point is not repeated.
type Path []orb.Point

// OpenRing converts an orb.Ring (closed or not) to a Path.
func OpenRing(r orb.Ring) Path {
	n := len(r)
	if n > 1 && r[0] == r[n-1] {
		n--
	}
	return Path(r[:n])
}

// Area is the absolute shoelace area of an open ring.
func Area(p Path) float64 {
	n := len(p)
	if n < 3 {
		return 0
	}
	var s float64
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		s += (p[j][0] - p[i][0]) * (p[j][1] + p[i][1])
	}
	return math.Abs(s) / 2
}

// signedArea is positive for counter-clockwise rings.
func signedArea(p Path) float64 {
	n := len(p)
	var s float64
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		s += p[j][0]*p[i][1] - p[i][0]*p[j][1]
	}
	return s / 2
}

// Axis selects a coordinate for axis-aligned clipping.
type Axis int

const (
	X Axis = 0
	Y Axis = 1
)

// ClipAxis keeps the part of ring p with coord >= bound (keepAbove) or
// coord <= bound (!keepAbove), using Sutherland–Hodgman. Points created on
// the clip line get that coordinate exactly, so adjacent pixels share edges
// bit-for-bit. Concave input may yield zero-width "bridges" along the clip
// line; they contribute zero area, so areas stay exact.
//
// out is reused if it has capacity.
func ClipAxis(p Path, axis Axis, bound float64, keepAbove bool, out Path) Path {
	out = out[:0]
	n := len(p)
	if n == 0 {
		return out
	}
	inside := func(q orb.Point) bool {
		if keepAbove {
			return q[axis] >= bound
		}
		return q[axis] <= bound
	}
	prev := p[n-1]
	prevIn := inside(prev)
	for _, cur := range p {
		curIn := inside(cur)
		if curIn != prevIn {
			out = append(out, axisIntersect(prev, cur, axis, bound))
		}
		if curIn {
			out = append(out, cur)
		}
		prev, prevIn = cur, curIn
	}
	return out
}

func axisIntersect(a, b orb.Point, axis Axis, bound float64) orb.Point {
	other := 1 - axis
	t := (bound - a[axis]) / (b[axis] - a[axis])
	var q orb.Point
	q[axis] = bound
	q[other] = a[other] + t*(b[other]-a[other])
	return q
}

// ClipRect clips ring p to the rectangle b.
func ClipRect(p Path, b orb.Bound) Path {
	var tmp Path
	r := ClipAxis(p, X, b.Min[0], true, nil)
	tmp = ClipAxis(r, X, b.Max[0], false, tmp)
	r = ClipAxis(tmp, Y, b.Min[1], true, r)
	tmp = ClipAxis(r, Y, b.Max[1], false, tmp)
	return tmp
}

// ClipConvex clips ring p to a convex ring (any orientation).
func ClipConvex(p Path, convex Path) Path {
	c := convex
	if signedArea(c) < 0 {
		c = make(Path, len(convex))
		for i := range convex {
			c[i] = convex[len(convex)-1-i]
		}
	}
	out := append(Path(nil), p...)
	n := len(c)
	for i := 0; i < n && len(out) > 0; i++ {
		a, b := c[i], c[(i+1)%n]
		in := func(q orb.Point) bool { return cross(a, b, q) >= 0 }
		src := out
		out = nil
		prev := src[len(src)-1]
		prevIn := in(prev)
		for _, cur := range src {
			curIn := in(cur)
			if curIn != prevIn {
				out = append(out, lineIntersect(prev, cur, a, b))
			}
			if curIn {
				out = append(out, cur)
			}
			prev, prevIn = cur, curIn
		}
	}
	return out
}

func cross(a, b, q orb.Point) float64 {
	return (b[0]-a[0])*(q[1]-a[1]) - (b[1]-a[1])*(q[0]-a[0])
}

// lineIntersect intersects segment pq with the infinite line ab.
func lineIntersect(p, q, a, b orb.Point) orb.Point {
	d1 := cross(a, b, p)
	d2 := cross(a, b, q)
	t := d1 / (d1 - d2)
	return orb.Point{p[0] + t*(q[0]-p[0]), p[1] + t*(q[1]-p[1])}
}

// PolygonArea is the area of a Polygon or MultiPolygon (exterior minus holes).
func PolygonArea(g orb.Geometry) float64 {
	var s float64
	for _, poly := range Polygons(g) {
		for i, r := range poly {
			a := Area(OpenRing(r))
			if i == 0 {
				s += a
			} else {
				s -= a
			}
		}
	}
	return s
}

// IntersectionAreaConvex is area(g ∩ convex), for a convex clip ring.
func IntersectionAreaConvex(g orb.Geometry, convex Path) float64 {
	var s float64
	for _, poly := range Polygons(g) {
		for i, r := range poly {
			a := Area(ClipConvex(OpenRing(r), convex))
			if i == 0 {
				s += a
			} else {
				s -= a
			}
		}
	}
	return s
}

// Polygons flattens a Polygon or MultiPolygon into its parts.
func Polygons(g orb.Geometry) []orb.Polygon {
	switch g := g.(type) {
	case orb.Polygon:
		return []orb.Polygon{g}
	case orb.MultiPolygon:
		return g
	default:
		panic(fmt.Sprintf("geom: unsupported geometry %T", g))
	}
}
