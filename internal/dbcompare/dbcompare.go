// Package dbcompare compares the engine's results with PostgreSQL and SQLite
// for the subset of window semantics each database supports.
package dbcompare

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// Column types used when materialising a table in a database.
type dbType int

const (
	dbInt dbType = iota
	dbFloat
	dbText
	dbTimestamp
	dbTimestamptz
	dbDate
	dbInterval
)

// PostgresClause returns the CREATE TABLE column definitions and the
// INSERT placeholder list for a PostgreSQL database.
func pgType(k value.Kind) (string, dbType) {
	switch k {
	case value.KindInt:
		return "bigint", dbInt
	case value.KindFloat:
		return "double precision", dbFloat
	case value.KindString:
		return "text", dbText
	case value.KindBool:
		return "boolean", dbInt
	case value.KindTimestamp:
		return "timestamp", dbTimestamp
	case value.KindTimestampTZ:
		return "timestamptz", dbTimestamptz
	case value.KindDate:
		return "date", dbDate
	case value.KindInterval:
		return "interval", dbInterval
	case value.KindNumeric:
		return "numeric", dbFloat
	}
	return "text", dbText
}

func sqliteType(k value.Kind) string {
	switch k {
	case value.KindInt:
		return "INTEGER"
	case value.KindFloat:
		return "REAL"
	case value.KindNumeric:
		return "NUMERIC"
	case value.KindBool:
		return "INTEGER"
	case value.KindTimestamp, value.KindTimestampTZ, value.KindDate, value.KindString, value.KindInterval:
		return "TEXT"
	}
	return "TEXT"
}

// quoteIdent quotes a SQL identifier.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// badColumnNames maps column names to SQL-safe names, keeping a reverse map.
func safeNames(schema *table.Schema) ([]string, map[string]string) {
	names := make([]string, schema.Len())
	rev := map[string]string{}
	for i, c := range schema.Columns {
		n := fmt.Sprintf("c%d_%s", i, sanitize(c.Name))
		names[i] = n
		rev[n] = c.Name
	}
	return names, rev
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "col"
	}
	return b.String()
}

// RewriteQueryText rewrites a query written against the original table name
// ("races") and original column names so that it can run in the database,
// where the table is named "races" but columns are named c0_heat etc.
//
// done by simple word substitution on token boundaries.
func RewriteQueryText(sqlText string, schema *table.Schema, names []string) (string, error) {
	out := sqlText
	for i, c := range schema.Columns {
		out = replaceWord(out, c.Name, names[i])
	}
	return out, nil
}

func replaceWord(s, word, repl string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if hasWordAt(s, i, word) {
			// Check boundaries: before must be non-identifier, after must be
			// non-identifier.
			beforeOK := i == 0 || !isIdentByte(s[i-1])
			after := i + len(word)
			afterOK := after >= len(s) || !isIdentByte(s[after])
			if beforeOK && afterOK {
				b.WriteString(repl)
				i = after
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func hasWordAt(s string, i int, word string) bool {
	if i+len(word) > len(s) {
		return false
	}
	return strings.EqualFold(s[i:i+len(word)], word)
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// LoadPostgres creates a table and inserts rows in one transaction.
func LoadPostgres(ctx context.Context, db *sql.DB, tableName string, t *table.Table) error {
	names, _ := safeNames(t.Schema)
	var defs []string
	for i, c := range t.Schema.Columns {
		typ, _ := pgType(c.Kind)
		defs = append(defs, quoteIdent(names[i])+" "+typ)
	}
	ddl := fmt.Sprintf("DROP TABLE IF EXISTS %s; CREATE TABLE %s (%s)", quoteIdent(tableName), quoteIdent(tableName), strings.Join(defs, ", "))
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	placeholders := make([]string, len(names))
	for i := range names {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%s)", quoteIdent(tableName), strings.Join(placeholders, ",")))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range t.Rows {
		args := make([]any, len(row))
		for i, v := range row {
			args[i] = pgArg(v)
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func pgArg(v value.Value) any {
	if v.IsNull() {
		return nil
	}
	switch v.Kind {
	case value.KindInt:
		return v.I
	case value.KindFloat:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) {
			return nil
		}
		return v.F
	case value.KindNumeric:
		return v.S
	case value.KindString:
		return v.S
	case value.KindBool:
		return v.B
	case value.KindTimestamp, value.KindTimestampTZ:
		return v.T
	case value.KindDate:
		return v.T.Format("2006-01-02")
	case value.KindInterval:
		return intervalText(v.D)
	}
	return v.Display()
}

func intervalText(iv value.Interval) string {
	// PostgreSQL interval input: "X mons Y days Z seconds"
	secs := float64(iv.Nanos) / 1e9
	return fmt.Sprintf("%d mons %d days %.9f seconds", iv.Months, iv.Days, secs)
}

// QueryPostgres runs a query and returns the result rows as strings (NULL is
// the empty string), matching the engine's display formatting where possible.
func QueryPostgres(ctx context.Context, db *sql.DB, query string) ([][]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		line := make([]string, len(cols))
		for i, v := range raw {
			line[i] = pgValueString(v)
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

func pgValueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case time.Time:
		return formatTimestamp(x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return formatFloat(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return x
	}
	return fmt.Sprintf("%v", v)
}

// formatTimestamp renders a timestamp the way value.Value.String does.
func formatTimestamp(t time.Time) string {
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		// could be a date; caller compares by type, so keep full form
	}
	return t.Format("2006-01-02 15:04:05.999999")
}

func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.10f", f), "0"), ".")
}

// LoadSQLite creates and fills a table in SQLite.
func LoadSQLite(ctx context.Context, db *sql.DB, tableName string, t *table.Table) error {
	names, _ := safeNames(t.Schema)
	var defs []string
	for i, c := range t.Schema.Columns {
		defs = append(defs, quoteIdent(names[i])+" "+sqliteType(c.Kind))
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", quoteIdent(tableName))); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s (%s)", quoteIdent(tableName), strings.Join(defs, ", "))); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ph := make([]string, len(names))
	for i := range names {
		ph[i] = "?"
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%s)", quoteIdent(tableName), strings.Join(ph, ",")))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range t.Rows {
		args := make([]any, len(row))
		for i, v := range row {
			args[i] = sqliteArg(v)
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sqliteArg(v value.Value) any {
	if v.IsNull() {
		return nil
	}
	switch v.Kind {
	case value.KindInt:
		return v.I
	case value.KindFloat:
		return v.F
	case value.KindNumeric:
		return v.S
	case value.KindString:
		return v.S
	case value.KindBool:
		if v.B {
			return 1
		}
		return 0
	case value.KindTimestamp, value.KindTimestampTZ:
		return v.T.Format("2006-01-02 15:04:05")
	case value.KindDate:
		return v.T.Format("2006-01-02")
	case value.KindInterval:
		return v.D.String()
	}
	return v.Display()
}

// QuerySQLite runs a query and returns string rows.
func QuerySQLite(ctx context.Context, db *sql.DB, query string) ([][]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		line := make([]string, len(cols))
		for i, v := range raw {
			line[i] = sqliteValueString(v)
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

func sqliteValueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return formatFloat(x)
	case string:
		return x
	}
	return fmt.Sprintf("%v", v)
}
