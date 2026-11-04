// Package h3index builds the village_h3 table: every H3
// cell that intersects a village, tagged "interior" (the cell lies wholly
// inside exactly one village, so points in it need no geometry test) or
// "boundary" (points in it need an exact ST_Covers test).
//
// Cells are treated as planar hexagons in lon/lat. Real H3 edges are
// great-circle arcs, which deviate from the straight lon/lat chord by
// ~1e-9° at res 9 near the equator. Margin (default 1e-7°, ~1 cm) absorbs
// that: a cell within Margin of a village edge is always "boundary".
// Indonesia does not cross the antimeridian, so no wrapping is handled.
package h3index

import (
	"fmt"
	"sort"

	"github.com/paulmach/orb"
	"github.com/uber/h3-go/v4"

	"on-demand-zonal-stats/internal/geom"
)

// CellType is the classification of a (cell, village) pair.
type CellType string

const (
	Interior CellType = "interior"
	Boundary CellType = "boundary"
)

// Row is one village_h3 row.
type Row struct {
	Cell      h3.Cell
	VillageID string
	Type      CellType
}

// Options controls classification.
type Options struct {
	Resolution int     // H3 resolution (releases use 10; see docs/prep.md)
	Margin     float64 // degrees; 0 means DefaultMargin
	MaxCells   int     // safety cap per village; 0 means DefaultMaxCells
}

const (
	DefaultMargin   = 1e-7
	DefaultMaxCells = 5_000_000
)

type verdict int

const (
	outside verdict = iota
	boundary
	interior
)

// ClassifyVillage returns every cell intersecting g, classified, sorted by cell.
func ClassifyVillage(id string, g orb.Geometry, opt Options) ([]Row, error) {
	if opt.Margin == 0 {
		opt.Margin = DefaultMargin
	}
	if opt.MaxCells == 0 {
		opt.MaxCells = DefaultMaxCells
	}
	c := &classifier{g: g, idx: geom.NewSegmentIndex(g), margin: opt.Margin}

	// Seeds: the cell of one exterior vertex per part. That vertex is on the
	// boundary, so its cell intersects the village. The cells intersecting a
	// connected polygon form a connected set, so a flood fill from one seed
	// per part reaches all of them.
	visited := map[h3.Cell]verdict{}
	var queue []h3.Cell
	for _, poly := range geom.Polygons(g) {
		if len(poly) == 0 || len(poly[0]) == 0 {
			continue
		}
		v := poly[0][0]
		cell, err := h3.LatLngToCell(h3.LatLng{Lat: v[1], Lng: v[0]}, opt.Resolution)
		if err != nil {
			return nil, fmt.Errorf("h3index: village %s: %w", id, err)
		}
		if _, seen := visited[cell]; !seen {
			visited[cell] = outside // placeholder, classified when popped
			queue = append(queue, cell)
		}
	}

	var rows []Row
	for len(queue) > 0 {
		cell := queue[0]
		queue = queue[1:]
		v, err := c.classify(cell)
		if err != nil {
			return nil, fmt.Errorf("h3index: village %s: %w", id, err)
		}
		visited[cell] = v
		if v == outside {
			continue
		}
		t := Boundary
		if v == interior {
			t = Interior
		}
		rows = append(rows, Row{Cell: cell, VillageID: id, Type: t})
		if len(rows) > opt.MaxCells {
			return nil, fmt.Errorf("h3index: village %s exceeds %d cells at res %d", id, opt.MaxCells, opt.Resolution)
		}
		nbrs, err := cell.GridDisk(1)
		if err != nil {
			return nil, err
		}
		for _, n := range nbrs {
			if _, seen := visited[n]; !seen {
				visited[n] = outside
				queue = append(queue, n)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Cell < rows[j].Cell })
	return rows, nil
}

type classifier struct {
	g      orb.Geometry
	idx    *geom.SegmentIndex
	margin float64
}

// CellPath returns the cell's hexagon as an open lon/lat ring.
func CellPath(cell h3.Cell) (geom.Path, error) {
	b, err := cell.Boundary()
	if err != nil {
		return nil, err
	}
	p := make(geom.Path, len(b))
	for i, ll := range b {
		p[i] = orb.Point{ll.Lng, ll.Lat}
	}
	return p, nil
}

func (c *classifier) classify(cell h3.Cell) (verdict, error) {
	hex, err := CellPath(cell)
	if err != nil {
		return outside, err
	}
	var hb orb.Bound
	for i, p := range hex {
		if i == 0 {
			hb = orb.Bound{Min: p, Max: p}
		} else {
			hb = hb.Extend(p)
		}
	}
	hb = hb.Pad(c.margin)

	// Any village edge within margin of the hexagon outline, or any village
	// vertex inside the hexagon, means the outline is not cleanly on one side.
	near := false
	c.idx.Query(hb, func(s geom.Segment) bool {
		if geom.RingContains(hex, s.A) || geom.RingContains(hex, s.B) {
			near = true
			return false
		}
		for i := range hex {
			if geom.SegmentDistance(s.A, s.B, hex[i], hex[(i+1)%len(hex)]) <= c.margin {
				near = true
				return false
			}
		}
		return true
	})
	if near {
		return boundary, nil
	}
	// No edge comes near the cell: it is wholly inside or wholly outside.
	if geom.Covers(c.g, hex[0]) {
		return interior, nil
	}
	return outside, nil
}

// Merge combines per-village rows into the final table and enforces the
// invariant "every interior cell maps to exactly one village": a cell that
// any other village also touches (overlapping source polygons) is demoted
// to boundary for all of them, so the exact path settles it.
// Output is sorted by (cell, village_id).
func Merge(perVillage ...[]Row) []Row {
	var all []Row
	for _, r := range perVillage {
		all = append(all, r...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Cell != all[j].Cell {
			return all[i].Cell < all[j].Cell
		}
		return all[i].VillageID < all[j].VillageID
	})
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && all[j].Cell == all[i].Cell {
			j++
		}
		if j-i > 1 {
			for k := i; k < j; k++ {
				all[k].Type = Boundary
			}
		}
		i = j
	}
	return all
}
