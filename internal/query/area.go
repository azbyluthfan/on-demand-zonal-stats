package query

import (
	"context"
	"fmt"
	"sort"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"

	"on-demand-zonal-stats/internal/release"
)

// Area selects villages: exactly one field is set.
type Area struct {
	ProvinceCode string
	RegencyCode  string
	DistrictCode string
	Geometry     orb.Geometry // orb.Polygon or orb.MultiPolygon, lon/lat
}

// Validate checks exactly one selector is set.
func (a Area) Validate() error {
	n := 0
	for _, s := range []string{a.ProvinceCode, a.RegencyCode, a.DistrictCode} {
		if s != "" {
			n++
		}
	}
	if a.Geometry != nil {
		n++
		switch a.Geometry.(type) {
		case orb.Polygon, orb.MultiPolygon:
		default:
			return fmt.Errorf("%w: geometry must be a Polygon or MultiPolygon", ErrInvalidArea)
		}
	}
	if n != 1 {
		return fmt.Errorf("%w: exactly one of province_code, regency_code, district_code, geometry is required", ErrInvalidArea)
	}
	return nil
}

// Village is one selected village's attributes.
type Village struct {
	ID           string
	Name         string
	DistrictCode string
	DistrictName string
	RegencyCode  string
	RegencyName  string
	ProvinceCode string
	ProvinceName string
	AreaM2       float64
	Bbox         orb.Bound
}

// Resolved is an area resolved to villages.
type Resolved struct {
	Villages  []Village // sorted by ID
	Provinces []string  // partitions to read, sorted
}

// IDs returns the village IDs in order.
func (r *Resolved) IDs() []string {
	ids := make([]string, len(r.Villages))
	for i, v := range r.Villages {
		ids[i] = v.ID
	}
	return ids
}

// ResolveArea lists the villages in the area. For a geometry, villages whose
// polygon intersects it are returned (bbox pruning, then exact test). It
// returns ErrTooManyVillages when more than limit villages match (limit<=0:
// no limit), without reading anything else.
func (e *Engine) ResolveArea(ctx context.Context, a Area, limit int) (*Resolved, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	src := "read_parquet(" + lit(e.path(release.VillagesPath)) + ")"
	var where string
	var args []any
	switch {
	case a.ProvinceCode != "":
		where, args = "province_code = ?", []any{a.ProvinceCode}
	case a.RegencyCode != "":
		where, args = "regency_code = ?", []any{a.RegencyCode}
	case a.DistrictCode != "":
		where, args = "district_code = ?", []any{a.DistrictCode}
	default:
		b := a.Geometry.Bound()
		w, err := wkb.Marshal(a.Geometry)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidArea, err)
		}
		// Same CRS label as the release (see docs/prep.md).
		g := "ST_SetCRS(ST_GeomFromWKB(?::BLOB), 'EPSG:4326')"
		var valid bool
		if err := e.db.QueryRowContext(ctx, "SELECT ST_IsValid("+g+")", w).Scan(&valid); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidArea, err)
		}
		if !valid {
			return nil, fmt.Errorf("%w: geometry is not valid (self-intersecting or malformed)", ErrInvalidArea)
		}
		where = fmt.Sprintf(`bbox.xmax >= %v AND bbox.xmin <= %v AND bbox.ymax >= %v AND bbox.ymin <= %v
			AND ST_Intersects(geometry, %s)`, b.Min[0], b.Max[0], b.Min[1], b.Max[1], g)
		args = []any{w}
	}
	q := fmt.Sprintf(`SELECT village_id, village_name, district_code, district_name, regency_code, regency_name,
			province_code, province_name, area_m2, bbox.xmin, bbox.ymin, bbox.xmax, bbox.ymax
		FROM %s WHERE %s ORDER BY village_id`, src, where)
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit+1)
	}
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer rows.Close()
	r := &Resolved{}
	provs := map[string]bool{}
	for rows.Next() {
		var v Village
		if err := rows.Scan(&v.ID, &v.Name, &v.DistrictCode, &v.DistrictName, &v.RegencyCode, &v.RegencyName,
			&v.ProvinceCode, &v.ProvinceName, &v.AreaM2, &v.Bbox.Min[0], &v.Bbox.Min[1], &v.Bbox.Max[0], &v.Bbox.Max[1]); err != nil {
			return nil, err
		}
		r.Villages = append(r.Villages, v)
		provs[v.ProvinceCode] = true
	}
	if err := rows.Err(); err != nil {
		return nil, classify(ctx, err)
	}
	if limit > 0 && len(r.Villages) > limit {
		return nil, fmt.Errorf("%w: more than %d", ErrTooManyVillages, limit)
	}
	for p := range provs {
		r.Provinces = append(r.Provinces, p)
	}
	sort.Strings(r.Provinces)
	return r, nil
}
