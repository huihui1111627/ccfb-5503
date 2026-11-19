package dbcompare

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/window"
)

// pgHandle bundles a loaded PostgreSQL table with its column names.
type pgHandle struct {
	db    *sql.DB
	names []string
	tbl   *table.Table
}

func loadPG(t *testing.T, tbl *table.Table) (context.Context, *pgHandle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	t.Cleanup(cancel)
	db, err := OpenPostgres(ctx)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	names, _ := safeNames(tbl.Schema)
	if err := LoadPostgres(ctx, db, "races", tbl); err != nil {
		t.Fatalf("load: %v", err)
	}
	return ctx, &pgHandle{db: db, names: names, tbl: tbl}
}

// uniqueTable builds a table whose order keys are unique, so ROWS frames
// (whose peer order is undefined in SQL) are deterministic.
func uniqueTable(rows int, seed int64) *table.Table { return gen.UniqueTable(rows, seed) }

func runPGCompare(t *testing.T, ctx context.Context, h *pgHandle, sqlText string) *ComparisonResult {
	t.Helper()
	q, _, err := window.ParseQuery(sqlText, h.tbl.Schema)
	if err != nil {
		return &ComparisonResult{SQL: sqlText, Status: "error", Detail: "parse: " + err.Error()}
	}
	start := time.Now()
	got, _, err := window.Eval(h.tbl, q)
	engineMs := time.Since(start).Milliseconds()
	if err != nil {
		return &ComparisonResult{SQL: sqlText, Status: "error", Detail: "engine: " + err.Error()}
	}
	nInput := len(h.tbl.Schema.Columns)
	projected := make([][]string, len(got.Rows))
	for i, r := range got.Rows {
		line := make([]string, len(r)-nInput)
		for j := range line {
			line[j] = displayForCompare(r[nInput+j])
		}
		projected[i] = line
	}
	rw, err := RewriteQueryText(sqlText, h.tbl.Schema, h.names)
	if err != nil {
		return &ComparisonResult{SQL: sqlText, Status: "error", Detail: err.Error()}
	}
	// Compare as multisets: an output ORDER BY with ties may legitimately
	// re-order rows differently, and this test's input rows are unique so
	// the multiset comparison is exact for the window results.
	dbRows, err := QueryPostgres(ctx, h.db, rw)
	if err != nil {
		if isUnsupportedPG(err) {
			return &ComparisonResult{SQL: sqlText, Status: "skipped", Detail: err.Error()}
		}
		return &ComparisonResult{SQL: sqlText, Status: "error", Detail: err.Error()}
	}
	ok, detail := compareMultiset(projected, dbRows)
	if !ok {
		return &ComparisonResult{SQL: sqlText, Status: "mismatch", Detail: detail}
	}
	return &ComparisonResult{SQL: sqlText, Status: "match", EngineMs: engineMs}
}

// TestPostgresUniqueOrder compares the full feature matrix on data with unique
// order keys, so every query's result is deterministic.
func TestPostgresUniqueOrder(t *testing.T) {
	tbl := uniqueTable(300, 7)
	ctx, h := loadPG(t, tbl)
	if h == nil {
		return
	}
	queries := []string{
		"SELECT id, sum(v) OVER (ORDER BY v) FROM races",
		"SELECT id, sum(v) OVER (PARTITION BY g ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN 5 PRECEDING AND 2 PRECEDING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN 2 FOLLOWING AND 5 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC ROWS BETWEEN 3 PRECEDING AND 3 FOLLOWING) FROM races",
		"SELECT id, count(*) OVER (ORDER BY v) FROM races",
		"SELECT id, count(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, avg(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, avg(x) OVER (ORDER BY x ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, min(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, max(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM races",
		"SELECT id, rank() OVER (ORDER BY v) FROM races",
		"SELECT id, dense_rank() OVER (ORDER BY v) FROM races",
		"SELECT id, percent_rank() OVER (ORDER BY v) FROM races",
		"SELECT id, cume_dist() OVER (ORDER BY v) FROM races",
		"SELECT id, ntile(4) OVER (ORDER BY v) FROM races",
		"SELECT id, ntile(7) OVER (ORDER BY v DESC) FROM races",
		"SELECT id, row_number() OVER (PARTITION BY g ORDER BY v) FROM races",
		"SELECT id, rank() OVER (PARTITION BY g ORDER BY v) FROM races",
		"SELECT id, lag(v, 1, -1) OVER (ORDER BY id) FROM races",
		"SELECT id, lead(v, 2, v * 0 - 999) OVER (PARTITION BY g ORDER BY id) FROM races",
		"SELECT id, lag(v) OVER (ORDER BY v DESC) FROM races",
		"SELECT id, first_value(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, last_value(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, nth_value(v, 2) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, nth_value(v, 3) OVER (ORDER BY id) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE GROUP) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE TIES) FROM races",
		"SELECT id, min(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING EXCLUDE CURRENT ROW) FROM races",
		"SELECT id, max(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING EXCLUDE GROUP) FROM races",
		"SELECT id, min(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING EXCLUDE TIES) FROM races",
		"SELECT id, count(*) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE TIES) FROM races",
		"SELECT id, count(*) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE GROUP) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW) FROM races",
		"SELECT id, sum(x) OVER (ORDER BY x RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(x) OVER (ORDER BY x DESC NULLS LAST RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY ts RANGE BETWEEN INTERVAL '1 minute' PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, count(*) OVER (ORDER BY ts RANGE BETWEEN INTERVAL '30 seconds' PRECEDING AND INTERVAL '30 seconds' FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY ts DESC RANGE BETWEEN INTERVAL '2 minutes' PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY s ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY s DESC ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, sum(v) FILTER (WHERE v > 20) OVER (ORDER BY v) FROM races",
		"SELECT id, count(v) FILTER (WHERE v < 100) OVER (ORDER BY v ROWS BETWEEN 3 PRECEDING AND 3 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN 20 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING EXCLUDE CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v GROUPS BETWEEN 2 PRECEDING AND 2 FOLLOWING EXCLUDE TIES) FROM races",
	}
	failed := 0
	for _, q := range queries {
		res := runPGCompare(t, ctx, h, q)
		switch res.Status {
		case "match":
		case "skipped":
			t.Logf("SKIP %s: %s", q, res.Detail)
		default:
			t.Errorf("%s: %s | %s", res.Status, q, res.Detail)
			failed++
		}
	}
	if failed > 0 {
		t.Fatalf("%d/%d failed", failed, len(queries))
	}
}

// TestPostgresPeers compares queries whose results are invariant to the
// (undefined) order of rows within a peer group, on peer-heavy data with NULLs.
//
// ntile and row_number are excluded here: they depend on the order of rows
// inside a peer group, which SQL leaves implementation-defined.  Both engines
// use their own stable input order, so they can legitimately disagree with
// PostgreSQL on such queries.
func TestPostgresPeers(t *testing.T) {
	tbl := gen.GenerateSmall(250, 999)
	ctx, h := loadPG(t, tbl)
	if h == nil {
		return
	}
	queries := []string{
		"SELECT id, rank() OVER (ORDER BY v) FROM races",
		"SELECT id, dense_rank() OVER (ORDER BY v) FROM races",
		"SELECT id, percent_rank() OVER (ORDER BY v) FROM races",
		"SELECT id, cume_dist() OVER (ORDER BY v) FROM races",
		"SELECT id, rank() OVER (ORDER BY v DESC NULLS FIRST) FROM races",
		"SELECT id, rank() OVER (PARTITION BY g ORDER BY x) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS FIRST RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS FIRST RANGE BETWEEN 10 PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS LAST RANGE BETWEEN CURRENT ROW AND 10 FOLLOWING) FROM races",
		"SELECT id, count(*) OVER (ORDER BY v RANGE BETWEEN 10 PRECEDING AND 0 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v GROUPS BETWEEN 2 PRECEDING AND 0 FOLLOWING EXCLUDE CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v GROUPS BETWEEN 0 PRECEDING AND 2 FOLLOWING EXCLUDE GROUP) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v GROUPS BETWEEN 0 PRECEDING AND 0 FOLLOWING EXCLUDE TIES) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY x RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY x DESC NULLS LAST RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY s RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY ts RANGE BETWEEN INTERVAL '1 minute' PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY ts DESC RANGE BETWEEN INTERVAL '1 minute' PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY ts NULLS FIRST RANGE BETWEEN INTERVAL '1 minute' PRECEDING AND INTERVAL '1 minute' FOLLOWING) FROM races",
		"SELECT id, min(v) OVER (ORDER BY v ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) FROM races",
		"SELECT id, max(v) OVER (ORDER BY v GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE GROUP) FROM races",
		"SELECT id, count(*) OVER (ORDER BY v GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE TIES) FROM races",
	}
	failed := 0
	for _, q := range queries {
		res := runPGCompare(t, ctx, h, q)
		switch res.Status {
		case "match":
		case "skipped":
			t.Logf("SKIP %s: %s", q, res.Detail)
		default:
			t.Errorf("%s: %s | %s", res.Status, q, res.Detail)
			failed++
		}
	}
	if failed > 0 {
		t.Fatalf("%d/%d failed", failed, len(queries))
	}
}

// hasOutputOrder reports whether the query has a top-level (depth 0) output
// ORDER BY clause.
func hasOutputOrder(sqlText string) bool {
	lower := strings.ToLower(sqlText)
	depth := 0
	for i := 0; i < len(lower); i++ {
		switch lower[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && strings.HasPrefix(lower[i:], "order by") {
			return true
		}
	}
	return false
}
