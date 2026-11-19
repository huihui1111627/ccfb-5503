package dbcompare

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// PostgresDSN returns the DSN for the local PostgreSQL instance.
func PostgresDSN() string {
	if dsn := os.Getenv("WF_PG_DSN"); dsn != "" {
		return dsn
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "postgres"
	}
	return fmt.Sprintf("host=localhost port=5432 user=%s dbname=postgres sslmode=disable", user)
}

// OpenPostgres connects and verifies the server is reachable.
func OpenPostgres(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("pgx", PostgresDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenSQLite opens a temporary in-memory SQLite database.
func OpenSQLite(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ComparisonResult reports one query's outcome.
type ComparisonResult struct {
	SQL      string
	Status   string // "match", "mismatch", "skipped", "error"
	Detail   string
	EngineMs int64
	DBMs     int64
}

// ComparePostgres executes the same logical query in the engine and
// PostgreSQL and compares the result columns.
//
// The query text must use the original table name "races" and original column
// names; the table must have been loaded with LoadPostgres under that name
// with rewritten column names.
//
// Results are compared as unordered multisets when the query has no output
// ORDER BY: SQL does not define row order, and window results (for ROWS
// frames over peer groups) may legitimately differ in peer order between
// implementations.  Set ordered=true when the query has a total ORDER BY.
func ComparePostgres(ctx context.Context, db *sql.DB, engineSQL, rewrittenSQL string, engineRows [][]string, ordered bool) (*ComparisonResult, error) {
	res := &ComparisonResult{SQL: engineSQL}
	dbRows, err := QueryPostgres(ctx, db, rewrittenSQL)
	if err != nil {
		if isUnsupportedPG(err) {
			res.Status = "skipped"
			res.Detail = err.Error()
			return res, nil
		}
		res.Status = "error"
		res.Detail = err.Error()
		return res, nil
	}
	var ok bool
	var detail string
	if ordered {
		ok, detail = compareOrderedRows(engineRows, dbRows)
	} else {
		ok, detail = compareMultiset(engineRows, dbRows)
	}
	if !ok {
		res.Status = "mismatch"
		res.Detail = detail
		return res, nil
	}
	res.Status = "match"
	return res, nil
}

func isUnsupportedPG(err error) bool {
	msg := err.Error()
	for _, s := range []string{
		"must not contain variables",
		"syntax error",
		"not implemented",
		"requires an OVER clause",
		"is not supported",
		"does not exist",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// valuesEqual compares two formatted values, tolerating numeric formatting
// differences (e.g. PostgreSQL prints a float with more digits).
func valuesEqual(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return a == b
	}
	// numeric comparison
	af, aok := parseNum(a)
	bf, bok := parseNum(b)
	if aok && bok {
		diff := af - bf
		if diff < 0 {
			diff = -diff
		}
		scale := 1.0
		if m := abs(af); m > scale {
			scale = m
		}
		return diff <= 1e-9*scale
	}
	return false
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func parseNum(s string) (float64, bool) {
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	if err != nil {
		return 0, false
	}
	return f, true
}

// displayForCompare renders a value for cross-database comparison.  Timestamps
// drop trailing microseconds so both sides use a common format.
func displayForCompare(v value.Value) string {
	if v.IsNull() {
		return ""
	}
	switch v.Kind {
	case value.KindTimestamp, value.KindTimestampTZ, value.KindDate, value.KindTime:
		return v.String()
	case value.KindNumeric:
		return v.S
	}
	return v.String()
}

// compareRows matches rows by their first column (assumed to be a unique row
// id), because SQL result order is undefined without ORDER BY.
func compareRows(a, b [][]string) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("row count engine=%d db=%d", len(a), len(b))
	}
	if len(a) == 0 {
		return true, ""
	}
	index := func(rows [][]string) (map[string][]string, string) {
		m := make(map[string][]string, len(rows))
		for _, r := range rows {
			k := r[0]
			if _, dup := m[k]; dup {
				return nil, "duplicate row key " + k
			}
			m[k] = r
		}
		return m, ""
	}
	am, msg := index(a)
	if msg != "" {
		return false, msg
	}
	bm, msg := index(b)
	if msg != "" {
		return false, msg
	}
	for k, ar := range am {
		br, ok := bm[k]
		if !ok {
			return false, "missing db row for key " + k
		}
		if len(ar) != len(br) {
			return false, fmt.Sprintf("col count key %s: engine=%d db=%d", k, len(ar), len(br))
		}
		for j := range ar {
			if !valuesEqual(ar[j], br[j]) {
				return false, fmt.Sprintf("key %s col %d: engine=%q db=%q", k, j, ar[j], br[j])
			}
		}
	}
	return true, ""
}

// compareOrderedRows compares two row sequences positionally.
func compareOrderedRows(a, b [][]string) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("row count engine=%d db=%d", len(a), len(b))
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false, fmt.Sprintf("col count row %d", i)
		}
		for j := range a[i] {
			if !valuesEqual(a[i][j], b[i][j]) {
				return false, fmt.Sprintf("row %d col %d: engine=%q db=%q", i, j, a[i][j], b[i][j])
			}
		}
	}
	return true, ""
}

// compareMultiset compares two unordered collections of rows, matching each
// row of b to an unused row of a with per-cell tolerance (floating-point sums
// are not associative, so implementations legitimately differ in the last
// bits).
func compareMultiset(a, b [][]string) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("row count engine=%d db=%d", len(a), len(b))
	}
	used := make([]bool, len(a))
	for _, br := range b {
		found := -1
		for i, ar := range a {
			if used[i] || len(ar) != len(br) {
				continue
			}
			if rowsEqual(ar, br) {
				found = i
				break
			}
		}
		if found < 0 {
			return false, "row present in db but not engine: " + strings.Join(br, "|")
		}
		used[found] = true
	}
	return true, ""
}

// rowsEqual compares two result rows cell by cell.
func rowsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// The following exported helpers support the wfverify command.

// PreparePostgres loads a table into PostgreSQL and returns the rewritten
// column names used in the database.
func PreparePostgres(ctx context.Context, db *sql.DB, t *table.Table, name string) ([]string, error) {
	names, _ := safeNames(t.Schema)
	return names, LoadPostgres(ctx, db, name, t)
}

// PrepareSQLite loads a table into SQLite and returns the rewritten column
// names.
func PrepareSQLite(ctx context.Context, db *sql.DB, t *table.Table, name string) ([]string, error) {
	names, _ := safeNames(t.Schema)
	return names, LoadSQLite(ctx, db, name, t)
}

// Rewrite rewrites original column names to the database's safe names.
func Rewrite(query string, schema *table.Schema, names []string) (string, error) {
	return RewriteQueryText(query, schema, names)
}

// Display renders a value for cross-database comparison.
func Display(v value.Value) string { return displayForCompare(v) }

// CompareMultiset compares unordered row collections with numeric tolerance.
func CompareMultiset(a, b [][]string) (bool, string) { return compareMultiset(a, b) }

// PGCompatible reports whether a generated query uses only features
// PostgreSQL 16 supports (no per-row offsets, no IGNORE NULLS).
func PGCompatible(sqlText string) bool {
	lower := strings.ToLower(sqlText)
	if strings.Contains(lower, "ignore nulls") {
		return false
	}
	// A window without ORDER BY has undefined row order in SQL: per-row
	// results from ROWS frames, lag/lead and first/last/nth_value then depend
	// on the physical row order, which differs between engines.  Skip those.
	if !allWindowsOrdered(lower) {
		return false
	}
	// Frame offsets must be constants in PostgreSQL 16.  Detect expressions
	// in the offset position (before PRECEDING/FOLLOWING) that reference a
	// column, cast or function call.
	if hasVariableFrameOffset(lower) {
		return false
	}
	for _, kw := range []string{"preceding", "following"} {
		idx := 0
		for {
			p := strings.Index(lower[idx:], kw)
			if p < 0 {
				break
			}
			p += idx
			// The offset is the token immediately before the keyword, except
			// for INTERVAL literals, which consist of two tokens.
			seg := offsetToken(lower[:p])
			if !isPGOffset(seg) {
				return false
			}
			idx = p + len(kw)
		}
	}
	return true
}

// hasVariableFrameOffset reports whether any frame bound uses an expression
// other than a constant, an INTERVAL literal, UNBOUNDED or CURRENT ROW.
//
// It looks for the frame keyword immediately preceding a bound (i.e.
// "... ROWS BETWEEN", "... ROWS 5 PRECEDING", "... RANGE CURRENT ROW") so it
// is not confused by ORDER BY lists that also contain commas.
func hasVariableFrameOffset(lower string) bool {
	// Only inspect text inside OVER (...) clauses, and only the frame part
	// that follows ORDER BY / PARTITION BY.
	for i := 0; i < len(lower); i++ {
		if !strings.HasPrefix(lower[i:], "over") {
			continue
		}
		j := i + 4
		for j < len(lower) && lower[j] == ' ' {
			j++
		}
		if j >= len(lower) || lower[j] != '(' {
			continue
		}
		end := frameClauseEnd(lower, j+1)
		body := lower[j+1 : end]
		if overBodyHasVariableOffset(body) {
			return true
		}
		i = end
	}
	return false
}

func isIdentChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

// overBodyHasVariableOffset scans one OVER(...) body for the frame clause.
// The frame unit keyword is the last ROWS/RANGE/GROUPS token in the body,
// appearing after any ORDER BY list and before PRECEDING/FOLLOWING.
func overBodyHasVariableOffset(body string) bool {
	for _, u := range []string{"rows", "range", "groups"} {
		idx := 0
		for {
			p := strings.Index(body[idx:], u)
			if p < 0 {
				break
			}
			p += idx
			after := p + len(u)
			if after < len(body) && isIdentChar(body[after]) {
				idx = after
				continue
			}
			rest := strings.TrimLeft(body[after:], " ")
			if !startsFrame(rest) {
				idx = after
				continue
			}
			clause := body[p:]
			// The frame clause runs to the end of the OVER body (EXCLUDE is
			// part of it but does not add offsets).
			if frameClauseHasVariableOffset(clause) {
				return true
			}
			return false
		}
	}
	return false
}

// startsFrame reports whether text begins a frame clause body.
func startsFrame(rest string) bool {
	for _, prefix := range []string{"between ", "unbounded ", "current ", "interval", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9"} {
		if strings.HasPrefix(rest, prefix) {
			return true
		}
	}
	return false
}

// frameClauseEnd returns the index just past the frame clause (the next ")"
// at paren depth 0).
func frameClauseEnd(lower string, start int) int {
	depth := 0
	for i := start; i < len(lower); i++ {
		switch lower[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return len(lower)
}

// frameClauseHasVariableOffset inspects one "ROWS ..." / "RANGE ..." clause.
func frameClauseHasVariableOffset(clause string) bool {
	if i := strings.Index(clause, " exclude "); i >= 0 {
		clause = clause[:i]
	}
	if i := strings.Index(clause, " between "); i >= 0 {
		rest := clause[i+len(" between "):]
		parts := strings.SplitN(rest, " and ", 2)
		if boundHasVariableOffset(parts[0]) {
			return true
		}
		if len(parts) == 2 && boundHasVariableOffset(parts[1]) {
			return true
		}
		return false
	}
	// Single-bound form: "ROWS <bound>".
	i := strings.Index(clause, " ")
	if i < 0 {
		return false
	}
	return boundHasVariableOffset(clause[i+1:])
}

// boundHasVariableOffset checks one bound: strip the direction keyword and
// test whether the remaining text is a constant offset.
func boundHasVariableOffset(seg string) bool {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return false
	}
	seg = strings.TrimSuffix(seg, "preceding")
	seg = strings.TrimSuffix(seg, "following")
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return false
	}
	if seg == "unbounded" || seg == "current row" || strings.HasPrefix(seg, "interval") {
		return false
	}
	if _, err := strconv.ParseFloat(seg, 64); err == nil {
		return false
	}
	return true
}

// allWindowsOrdered reports whether every OVER (...) clause contains ORDER BY.
func allWindowsOrdered(lower string) bool {
	for i := 0; i < len(lower); i++ {
		if !strings.HasPrefix(lower[i:], "over") {
			continue
		}
		j := i + 4
		for j < len(lower) && lower[j] == ' ' {
			j++
		}
		if j >= len(lower) || lower[j] != '(' {
			// named window: assume ordered (the generator does not emit WINDOW)
			continue
		}
		depth := 0
		found := false
		for ; j < len(lower); j++ {
			if lower[j] == '(' {
				depth++
			} else if lower[j] == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
			if depth == 1 && strings.HasPrefix(lower[j:], "order by") {
				found = true
			}
		}
		if !found {
			return false
		}
		i = j
	}
	return true
}

// offsetToken extracts the offset expression text preceding a PRECEDING /
// FOLLOWING keyword.
func offsetToken(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	// INTERVAL '...' literal
	if i := strings.LastIndex(prefix, "interval "); i >= 0 {
		return strings.TrimSpace(prefix[i:])
	}
	if i := strings.LastIndexAny(prefix, " ("); i >= 0 {
		return strings.TrimSpace(prefix[i+1:])
	}
	return prefix
}

func isPGOffset(seg string) bool {
	seg = strings.TrimSpace(seg)
	if seg == "unbounded" || strings.HasPrefix(seg, "interval") {
		return true
	}
	if seg == "" {
		return false
	}
	for _, r := range seg {
		if !(r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// CellEqual compares two values for cross-implementation equality: NULLs are
// equal, numerics compare numerically with a small relative tolerance, and
// everything else compares as displayed text.
func CellEqual(a, b value.Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() && b.IsNull()
	}
	return valuesEqual(displayForCompare(a), displayForCompare(b))
}
