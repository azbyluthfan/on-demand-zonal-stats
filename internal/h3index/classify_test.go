package h3index_test

import (
	"math"
	"math/rand"
	"testing"

	"github.com/paulmach/orb"
	"github.com/uber/h3-go/v4"

	"on-demand-zonal-stats/internal/geom"
	"on-demand-zonal-stats/internal/h3index"
	"on-demand-zonal-stats/internal/testfixtures"
)

const res = 9

// oracle computes, independently of the classifier, the fraction of each
// nearby cell's hexagon covered by g (convex clipping of g against the hex).
func oracle(t *testing.T, g orb.Geometry) map[h3.Cell]float64 {
	t.Helper()
	b := g.Bound()
	c := b.Center()
	center, err := h3.LatLngToCell(h3.LatLng{Lat: c[1], Lng: c[0]}, res)
	if err != nil {
		t.Fatal(err)
	}
	span := math.Max(b.Max[0]-b.Min[0], b.Max[1]-b.Min[1])
	k := int(math.Ceil(span/0.0025)) + 3 // res-9 centre spacing ≈ 0.0027°
	disk, err := center.GridDisk(k)
	if err != nil {
		t.Fatal(err)
	}
	out := map[h3.Cell]float64{}
	for _, cell := range disk {
		hex, err := h3index.CellPath(cell)
		if err != nil {
			t.Fatal(err)
		}
		// "exterior minus holes" leaves ~1e-13 float residue for cells
		// inside a hole, so require a real (relative) overlap.
		if f := geom.IntersectionAreaConvex(g, hex) / geom.Area(hex); f > 1e-9 {
			out[cell] = f
		}
	}
	return out
}

func TestClassifyAgainstOracle(t *testing.T) {
	for _, v := range testfixtures.Villages(t) {
		t.Run(v.ID, func(t *testing.T) {
			rows, err := h3index.ClassifyVillage(v.ID, v.Geom, h3index.Options{Resolution: res})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				t.Fatal("village has no H3 rows")
			}
			got := map[h3.Cell]h3index.CellType{}
			for _, r := range rows {
				if r.VillageID != v.ID {
					t.Fatalf("row for wrong village %q", r.VillageID)
				}
				got[r.Cell] = r.Type
			}
			want := oracle(t, v.Geom)
			var nInterior int
			for cell, frac := range want {
				typ, ok := got[cell]
				if !ok {
					t.Errorf("cell %v covers %.3g of its area but is missing", cell, frac)
					continue
				}
				if typ == h3index.Interior {
					nInterior++
					if frac < 1-1e-9 {
						t.Errorf("interior cell %v is only %.12f covered", cell, frac)
					}
				}
				if frac < 1-1e-9 && typ != h3index.Boundary {
					t.Errorf("partially covered cell %v (%.6f) classified %s", cell, frac, typ)
				}
			}
			for cell := range got {
				if _, ok := want[cell]; !ok {
					// Allowed only for boundary cells merely touching within margin.
					if got[cell] != h3index.Boundary {
						t.Errorf("cell %v classified %s but does not intersect", cell, got[cell])
					}
				}
			}
			t.Logf("%s: %d cells, %d interior", v.ID, len(rows), nInterior)
		})
	}
}

func TestTinyVillageIsAllBoundary(t *testing.T) {
	v := testfixtures.VillageByID(t, "V005")
	rows, err := h3index.ClassifyVillage(v.ID, v.Geom, h3index.Options{Resolution: res})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("tiny village has no rows")
	}
	for _, r := range rows {
		if r.Type != h3index.Interior {
			continue
		}
		t.Errorf("tiny village has interior cell %v", r.Cell)
	}
}

func TestHoleCellsExcluded(t *testing.T) {
	v := testfixtures.VillageByID(t, "V004")
	rows, _ := h3index.ClassifyVillage(v.ID, v.Geom, h3index.Options{Resolution: res})
	hole, _ := h3.LatLngToCell(h3.LatLng{Lat: -7.82, Lng: 110.38}, res) // middle of the hole
	for _, r := range rows {
		if r.Cell == hole {
			t.Fatalf("cell in the middle of the hole is listed as %s", r.Type)
		}
	}
}

// Points that H3 itself puts in an interior cell must lie inside the
// village. This checks the planar-hexagon approximation against H3's own
// point-to-cell assignment.
func TestInteriorCellPointsAreInside(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, v := range testfixtures.Villages(t) {
		rows, _ := h3index.ClassifyVillage(v.ID, v.Geom, h3index.Options{Resolution: res})
		for _, r := range rows {
			if r.Type != h3index.Interior {
				continue
			}
			hex, _ := h3index.CellPath(r.Cell)
			c, _ := r.Cell.LatLng()
			for i := 0; i < 50; i++ {
				// Random point in a random triangle (centre, v_k, v_k+1),
				// biased toward the outline where errors would show.
				k := rng.Intn(len(hex))
				a, b := hex[k], hex[(k+1)%len(hex)]
				s, u := 0.9+0.1*rng.Float64(), rng.Float64()
				p := orb.Point{
					c.Lng + s*((1-u)*(a[0]-c.Lng)+u*(b[0]-c.Lng)),
					c.Lat + s*((1-u)*(a[1]-c.Lat)+u*(b[1]-c.Lat)),
				}
				cell, _ := h3.LatLngToCell(h3.LatLng{Lat: p[1], Lng: p[0]}, res)
				if cell != r.Cell {
					continue // fell into a neighbour per H3; not this cell's point
				}
				if !geom.Covers(v.Geom, p) {
					t.Fatalf("%s: point %v is in interior cell %v but outside the village", v.ID, p, r.Cell)
				}
			}
		}
	}
}

func TestMergeDemotesSharedCells(t *testing.T) {
	// Two overlapping squares: cells inside the overlap are interior for
	// both before merging and must become boundary after.
	a := orb.Polygon{{{110.30, -7.82}, {110.33, -7.82}, {110.33, -7.79}, {110.30, -7.79}, {110.30, -7.82}}}
	b := orb.Polygon{{{110.32, -7.82}, {110.35, -7.82}, {110.35, -7.79}, {110.32, -7.79}, {110.32, -7.82}}}
	ra, _ := h3index.ClassifyVillage("A", a, h3index.Options{Resolution: res})
	rb, _ := h3index.ClassifyVillage("B", b, h3index.Options{Resolution: res})
	merged := h3index.Merge(ra, rb)

	byCell := map[h3.Cell][]h3index.Row{}
	for _, r := range merged {
		byCell[r.Cell] = append(byCell[r.Cell], r)
	}
	overlapInterior := 0
	for _, r := range ra {
		if r.Type == h3index.Interior && len(byCell[r.Cell]) > 1 {
			overlapInterior++
		}
	}
	if overlapInterior == 0 {
		t.Fatal("test setup: expected some A-interior cells inside the overlap")
	}
	for cell, rs := range byCell {
		interior := 0
		for _, r := range rs {
			if r.Type == h3index.Interior {
				interior++
			}
		}
		if interior > 0 && len(rs) > 1 {
			t.Errorf("cell %v is interior but mapped to %d villages", cell, len(rs))
		}
	}
	for i := 1; i < len(merged); i++ {
		p, c := merged[i-1], merged[i]
		if p.Cell > c.Cell || (p.Cell == c.Cell && p.VillageID >= c.VillageID) {
			t.Fatal("merged rows not sorted by (cell, village_id)")
		}
	}
}

func TestMergeAdjacentFixturesKeepsInterior(t *testing.T) {
	// Non-overlapping fixtures: every interior cell must stay interior and
	// map to exactly one village (a prep QC rule).
	var per [][]h3index.Row
	for _, v := range testfixtures.Villages(t) {
		r, err := h3index.ClassifyVillage(v.ID, v.Geom, h3index.Options{Resolution: res})
		if err != nil {
			t.Fatal(err)
		}
		per = append(per, r)
	}
	before := 0
	for _, rs := range per {
		for _, r := range rs {
			if r.Type == h3index.Interior {
				before++
			}
		}
	}
	after := 0
	for _, r := range h3index.Merge(per...) {
		if r.Type == h3index.Interior {
			after++
		}
	}
	if before != after || before == 0 {
		t.Errorf("interior cells before merge %d, after %d", before, after)
	}
}
