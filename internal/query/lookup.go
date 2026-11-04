package query

import (
	"context"
	"encoding/json"
	"fmt"

	"on-demand-zonal-stats/internal/release"
)

// Geometries returns each village's geometry as GeoJSON.
func (e *Engine) Geometries(ctx context.Context, ids []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// ST_AsGeoJSON returns DuckDB's JSON type, which the driver decodes to a
	// map; keep it as text.
	rows, err := e.db.QueryContext(ctx, fmt.Sprintf(`SELECT village_id, ST_AsGeoJSON(geometry)::VARCHAR
		FROM read_parquet(%s) WHERE village_id IN (SELECT unnest(%s))`,
		lit(e.path(release.VillagesPath)), listLit(ids)))
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, g string
		if err := rows.Scan(&id, &g); err != nil {
			return nil, err
		}
		out[id] = json.RawMessage(g)
	}
	return out, classify(ctx, rows.Err())
}

// AdminArea is one province, regency or district with its village count.
type AdminArea struct {
	Level    string `json:"level"` // province | regency | district
	Code     string `json:"code"`
	Name     string `json:"name"`
	Parent   string `json:"parent,omitempty"` // parent code
	Villages int64  `json:"villages"`
}

// AdminAreas lists the admin hierarchy present in the release (for the
// frontend's area picker).
func (e *Engine) AdminAreas(ctx context.Context) ([]AdminArea, error) {
	rows, err := e.db.QueryContext(ctx, fmt.Sprintf(`WITH v AS (SELECT * FROM read_parquet(%s)),
		u AS (
			SELECT 0 AS lvl, 'province' AS level, province_code AS code, any_value(province_name) AS name, NULL::VARCHAR AS parent, count(*) AS n FROM v GROUP BY province_code
			UNION ALL
			SELECT 1, 'regency', regency_code, any_value(regency_name), any_value(province_code), count(*) FROM v GROUP BY regency_code
			UNION ALL
			SELECT 2, 'district', district_code, any_value(district_name), any_value(regency_code), count(*) FROM v GROUP BY district_code)
		SELECT level, code, name, parent, n FROM u ORDER BY lvl, code`, lit(e.path(release.VillagesPath))))
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer rows.Close()
	var out []AdminArea
	for rows.Next() {
		var a AdminArea
		var parent *string
		if err := rows.Scan(&a.Level, &a.Code, &a.Name, &parent, &a.Villages); err != nil {
			return nil, err
		}
		if parent != nil {
			a.Parent = *parent
		}
		out = append(out, a)
	}
	return out, classify(ctx, rows.Err())
}
