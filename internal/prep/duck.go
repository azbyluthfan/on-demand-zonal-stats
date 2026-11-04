package prep

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
)

// db wraps one DuckDB connection; all statements run on it so session
// settings, secrets and temp tables are shared.
type db struct {
	sql  *sql.DB
	conn *sql.Conn
}

func openDB(ctx context.Context) (*db, error) {
	s, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	c, err := s.Conn(ctx)
	if err != nil {
		s.Close()
		return nil, err
	}
	d := &db{sql: s, conn: c}
	if dir := os.Getenv("DUCKDB_EXT_DIR"); dir != "" {
		if err := d.exec(ctx, "SET extension_directory = "+lit(dir)); err != nil {
			return nil, err
		}
	}
	for _, e := range []string{"spatial", "httpfs", "h3 FROM community", "cog FROM community"} {
		name := strings.Fields(e)[0]
		// INSTALL is a no-op when already present (e.g. baked into the image).
		if err := d.exec(ctx, "INSTALL "+e); err != nil {
			return nil, fmt.Errorf("install %s: %w", name, err)
		}
		if err := d.exec(ctx, "LOAD "+name); err != nil {
			return nil, fmt.Errorf("load %s: %w", name, err)
		}
	}
	if v := os.Getenv("DUCKDB_MEMORY_LIMIT"); v != "" {
		if err := d.exec(ctx, "SET memory_limit = "+lit(v)); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// InstallExtensions installs extensions into DUCKDB_EXT_DIR (image build).
func InstallExtensions(ctx context.Context) error {
	d, err := openDB(ctx)
	if err != nil {
		return err
	}
	return d.close()
}

func (d *db) close() error {
	d.conn.Close()
	return d.sql.Close()
}

func (d *db) exec(ctx context.Context, q string, args ...any) error {
	if _, err := d.conn.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("%w\nSQL: %s", err, abbreviate(q))
	}
	return nil
}

func (d *db) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return d.conn.QueryRowContext(ctx, q, args...)
}

func (d *db) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	r, err := d.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%w\nSQL: %s", err, abbreviate(q))
	}
	return r, nil
}

func (d *db) count(ctx context.Context, q string, args ...any) (int64, error) {
	var n int64
	err := d.queryRow(ctx, q, args...).Scan(&n)
	if err != nil {
		err = fmt.Errorf("%w\nSQL: %s", err, abbreviate(q))
	}
	return n, err
}

func (d *db) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// lit quotes a SQL string literal.
func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func litList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = lit(s)
	}
	return strings.Join(q, ", ")
}

func abbreviate(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 400 {
		return q[:400] + "…"
	}
	return q
}
