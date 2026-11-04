package geom

import (
	"math"

	"github.com/paulmach/orb"
)

// Segment is one polygon edge.
type Segment struct{ A, B orb.Point }

// SegmentIndex is a uniform-grid index over all ring edges of a geometry,
// used to find the edges near a small query box (an H3 cell) without
// scanning every vertex of a large village.
type SegmentIndex struct {
	segs    []Segment
	bound   orb.Bound
	nx, ny  int
	cw, ch  float64
	buckets [][]int32
	stamp   []uint32
	gen     uint32
}

// NewSegmentIndex indexes every edge of g's rings.
func NewSegmentIndex(g orb.Geometry) *SegmentIndex {
	idx := &SegmentIndex{bound: g.Bound()}
	for _, poly := range Polygons(g) {
		for _, r := range poly {
			p := OpenRing(r)
			for i := range p {
				idx.segs = append(idx.segs, Segment{p[i], p[(i+1)%len(p)]})
			}
		}
	}
	// About one bucket per 4 segments, at least 1x1.
	n := int(math.Ceil(math.Sqrt(float64(len(idx.segs)) / 4)))
	idx.nx, idx.ny = max(n, 1), max(n, 1)
	w := idx.bound.Max[0] - idx.bound.Min[0]
	h := idx.bound.Max[1] - idx.bound.Min[1]
	idx.cw = max(w/float64(idx.nx), 1e-12)
	idx.ch = max(h/float64(idx.ny), 1e-12)
	idx.buckets = make([][]int32, idx.nx*idx.ny)
	for i, s := range idx.segs {
		x0, y0, x1, y1 := idx.cellRange(segBound(s))
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				k := y*idx.nx + x
				idx.buckets[k] = append(idx.buckets[k], int32(i))
			}
		}
	}
	idx.stamp = make([]uint32, len(idx.segs))
	return idx
}

func segBound(s Segment) orb.Bound {
	return orb.Bound{
		Min: orb.Point{min(s.A[0], s.B[0]), min(s.A[1], s.B[1])},
		Max: orb.Point{max(s.A[0], s.B[0]), max(s.A[1], s.B[1])},
	}
}

func (idx *SegmentIndex) cellRange(b orb.Bound) (x0, y0, x1, y1 int) {
	clamp := func(v, n int) int { return min(max(v, 0), n-1) }
	x0 = clamp(int(math.Floor((b.Min[0]-idx.bound.Min[0])/idx.cw)), idx.nx)
	x1 = clamp(int(math.Floor((b.Max[0]-idx.bound.Min[0])/idx.cw)), idx.nx)
	y0 = clamp(int(math.Floor((b.Min[1]-idx.bound.Min[1])/idx.ch)), idx.ny)
	y1 = clamp(int(math.Floor((b.Max[1]-idx.bound.Min[1])/idx.ch)), idx.ny)
	return
}

// Query calls fn for each distinct segment whose box intersects b.
// Not safe for concurrent use.
func (idx *SegmentIndex) Query(b orb.Bound, fn func(Segment) bool) {
	if !b.Intersects(idx.bound) {
		return
	}
	idx.gen++
	x0, y0, x1, y1 := idx.cellRange(b)
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			for _, i := range idx.buckets[y*idx.nx+x] {
				if idx.stamp[i] == idx.gen {
					continue
				}
				idx.stamp[i] = idx.gen
				if segBound(idx.segs[i]).Intersects(b) && !fn(idx.segs[i]) {
					return
				}
			}
		}
	}
}

// SegmentDistance is the minimum distance between segments ab and cd.
func SegmentDistance(a, b, c, d orb.Point) float64 {
	if segmentsIntersect(a, b, c, d) {
		return 0
	}
	return min(pointSegDist(a, c, d), pointSegDist(b, c, d),
		pointSegDist(c, a, b), pointSegDist(d, a, b))
}

func pointSegDist(p, a, b orb.Point) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = ((p[0]-a[0])*dx + (p[1]-a[1])*dy) / l2
		t = min(max(t, 0), 1)
	}
	ex, ey := a[0]+t*dx-p[0], a[1]+t*dy-p[1]
	return math.Hypot(ex, ey)
}

func segmentsIntersect(a, b, c, d orb.Point) bool {
	d1, d2 := cross(c, d, a), cross(c, d, b)
	d3, d4 := cross(a, b, c), cross(a, b, d)
	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) && ((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true
	}
	return (d1 == 0 && onSegment(c, d, a)) || (d2 == 0 && onSegment(c, d, b)) ||
		(d3 == 0 && onSegment(a, b, c)) || (d4 == 0 && onSegment(a, b, d))
}

// RingContains reports whether p is inside ring r using the half-open crossing rule (points exactly on an edge may go either way; use Covers for boundary-inclusive tests).
func RingContains(r Path, p orb.Point) bool { return ringContains(orb.Ring(r), p) }
