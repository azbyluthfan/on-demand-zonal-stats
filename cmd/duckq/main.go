// Command duckq is a dev tool: run SQL on the project's pinned DuckDB with
// spatial, httpfs, h3 and cog loaded, printing rows as tab-separated text.
//
//	go run ./cmd/duckq "SELECT ..." ["SELECT ..." ...]
//
// Statements run in order on one connection, so SET/CREATE SECRET persist.
// Optional env: S3_ENDPOINT etc. create a "rustfs" secret for s3://villagestat.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, `usage: duckq "SQL" ["SQL" ...]`)
		os.Exit(1)
	}
	db, err := sql.Open("duckdb", "")
	check(err)
	db.SetMaxOpenConns(1)
	for _, s := range []string{"INSTALL spatial", "LOAD spatial", "INSTALL httpfs", "LOAD httpfs",
		"INSTALL h3 FROM community", "LOAD h3", "INSTALL cog FROM community", "LOAD cog"} {
		_, err := db.Exec(s)
		check(err)
	}
	if ep := os.Getenv("S3_ENDPOINT"); ep != "" {
		_, err := db.Exec(fmt.Sprintf(`CREATE SECRET rustfs (TYPE s3, KEY_ID '%s', SECRET '%s', ENDPOINT '%s',
			URL_STYLE 'path', USE_SSL false, REGION 'us-east-1', SCOPE 's3://villagestat')`,
			getenv("S3_ACCESS_KEY", "rustfsadmin"), getenv("S3_SECRET_KEY", "rustfsadmin"), ep))
		check(err)
	}
	for _, q := range os.Args[1:] {
		rows, err := db.Query(q)
		check(err)
		cols, _ := rows.Columns()
		fmt.Println(strings.Join(cols, "\t"))
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			check(rows.Scan(ptrs...))
			out := make([]string, len(cols))
			for i, v := range vals {
				switch v := v.(type) {
				case []byte:
					out[i] = fmt.Sprintf("<%d bytes>", len(v))
				default:
					out[i] = fmt.Sprint(v)
				}
			}
			fmt.Println(strings.Join(out, "\t"))
		}
		check(rows.Err())
		rows.Close()
		fmt.Println()
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "duckq:", err)
		os.Exit(1)
	}
}
