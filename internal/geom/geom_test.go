package geom_test

import (
	"math"
	"testing"

	"github.com/paulmach/orb"

	"on-demand-zonal-stats/internal/geom"
	"on-demand-zonal-stats/internal/testfixtures"
)

func square(x0, y0, x1, y1 float64) orb.Polygon {
	return orb.Polygon{{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}}
}

func TestCovers(t *testing.T) {
	ring := testfixtures.VillageByID(t, "V004").Geom // square 110.36..110.40 with hole 110.37..110.39
	cases := []struct {
		name string
		p    orb.Point
		want bool
	}{
		{"inside solid part", orb.Point{110.365, -7.82}, true},
		{"on exterior edge", orb.Point{110.36, -7.82}, true},
		{"on exterior vertex", orb.Point{110.40, -7.84}, true},
		{"inside hole", orb.Point{110.38, -7.82}, false},
		{"on hole edge", orb.Point{110.37, -7.82}, true},
		{"on hole vertex", orb.Point{110.39, -7.81}, true},
		{"outside", orb.Point{110.41, -7.82}, false},
	}
	for _, c := range cases {
		if got := geom.Covers(ring, c.p); got != c.want {
			t.Errorf("%s: Covers(%v) = %v, want %v", c.name, c.p, got, c.want)
		}
	}
}

func TestCoversMultiPolygon(t *testing.T) {
	islands := testfixtures.VillageByID(t, "V006").Geom
	if !geom.Covers(islands, orb.Point{110.465, -7.815}) {
		t.Error("point in second island not covered")
	}
	if geom.Covers(islands, orb.Point{110.455, -7.815}) {
		t.Error("point between islands covered")
	}
}

// Edge rule: a point on a shared border counts once, for the
// lowest village_id.
func TestAssignEdgeRule(t *testing.T) {
	vs := testfixtures.Villages(t)
	var cands []geom.Candidate
	for _, v := range vs {
		cands = append(cands, geom.Candidate{ID: v.ID, Geom: v.Geom})
	}
	// Reverse order so the result can't come from iteration order.
	for i, j := 0, len(cands)-1; i < j; i, j = i+1, j-1 {
		cands[i], cands[j] = cands[j], cands[i]
	}
	cases := []struct {
		name   string
		p      orb.Point
		wantID string
		wantOK bool
	}{
		{"shared border V001|V002", orb.Point{110.32, -7.81}, "V001", true},
		{"shared corner V001|V002", orb.Point{110.32, -7.80}, "V001", true},
		{"interior of V002", orb.Point{110.33, -7.81}, "V002", true},
		{"in V004 hole: unassigned", orb.Point{110.38, -7.82}, "", false},
		{"offshore: unassigned", orb.Point{111, -8}, "", false},
	}
	for _, c := range cases {
		id, ok := geom.Assign(c.p, cands)
		if id != c.wantID || ok != c.wantOK {
			t.Errorf("%s: Assign = (%q, %v), want (%q, %v)", c.name, id, ok, c.wantID, c.wantOK)
		}
	}
}

func TestAssignFourWayCorner(t *testing.T) {
	cands := []geom.Candidate{
		{ID: "D", Geom: square(1, 1, 2, 2)},
		{ID: "B", Geom: square(0, 1, 1, 2)},
		{ID: "C", Geom: square(1, 0, 2, 1)},
		{ID: "A", Geom: square(0, 0, 1, 1)},
	}
	if id, _ := geom.Assign(orb.Point{1, 1}, cands); id != "A" {
		t.Errorf("corner shared by 4 squares assigned to %q, want A", id)
	}
	if id, _ := geom.Assign(orb.Point{1.5, 1}, cands); id != "C" {
		t.Errorf("border C|D assigned to %q, want C", id)
	}
}

// The half-open rule partitions: each point on a shared border belongs to
// exactly one of the two neighbours.
func TestContainsHalfOpenPartitions(t *testing.T) {
	a := testfixtures.VillageByID(t, "V001").Geom
	b := testfixtures.VillageByID(t, "V002").Geom
	for i := 1; i < 100; i++ {
		p := orb.Point{110.32, -7.82 + float64(i)*0.0002}
		n := 0
		if geom.ContainsHalfOpen(a, p) {
			n++
		}
		if geom.ContainsHalfOpen(b, p) {
			n++
		}
		if n != 1 {
			t.Fatalf("point %v on shared border counted %d times", p, n)
		}
	}
}

func TestPolygonArea(t *testing.T) {
	got := geom.PolygonArea(testfixtures.VillageByID(t, "V004").Geom)
	want := 0.04*0.04 - 0.02*0.02
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("V004 area = %v, want %v", got, want)
	}
	got = geom.PolygonArea(testfixtures.VillageByID(t, "V006").Geom)
	if math.Abs(got-2*0.01*0.01) > 1e-12 {
		t.Errorf("V006 area = %v, want 0.0002", got)
	}
}

func TestClipRectConcave(t *testing.T) {
	// L-shape clipped to a box that cuts both arms: the bridge along the clip
	// edge must add no area.
	l := geom.OpenRing(orb.Ring{{0, 0}, {3, 0}, {3, 1}, {1, 1}, {1, 3}, {0, 3}, {0, 0}})
	got := geom.Area(geom.ClipRect(l, orb.Bound{Min: orb.Point{0.5, 0.5}, Max: orb.Point{2, 2}}))
	// Box 1.5x1.5 = 2.25 minus the notch [1,2]x[1,2] = 1 → 1.25
	if math.Abs(got-1.25) > 1e-12 {
		t.Errorf("clipped L area = %v, want 1.25", got)
	}
}

func TestSegmentDistance(t *testing.T) {
	d := geom.SegmentDistance(orb.Point{0, 0}, orb.Point{1, 0}, orb.Point{0.5, 1}, orb.Point{0.5, 2})
	if math.Abs(d-1) > 1e-15 {
		t.Errorf("distance = %v, want 1", d)
	}
	if d := geom.SegmentDistance(orb.Point{0, 0}, orb.Point{1, 1}, orb.Point{0, 1}, orb.Point{1, 0}); d != 0 {
		t.Errorf("crossing segments distance = %v, want 0", d)
	}
}
