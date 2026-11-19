package dbcompare

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/window"
)

func loadSQLite(t *testing.T, tbl *table.Table) (context.Context, *pgHandle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	t.Cleanup(cancel)
	db, err := OpenSQLite(ctx)
	if err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	names, _ := safeNames(tbl.Schema)
	if err := LoadSQLite(ctx, db, "races", tbl); err != nil {
		t.Fatalf("load: %v", err)
	}
	return ctx, &pgHandle{db: db, names: names, tbl: tbl}
}

func runSQLiteCompare(t *testing.T, ctx context.Context, h *pgHandle, sqlText string) *ComparisonResult {
	t.Helper()
	q, _, err := window.ParseQuery(sqlText, h.tbl.Schema)
	if err != nil {
		return &ComparisonResult{SQL: sqlText, Status: "error", Detail: "parse: " + err.Error()}
	}
	got, _, err := window.Eval(h.tbl, q)
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
	dbRows, err := QuerySQLite(ctx, h.db, rw)
	if err != nil {
		return &ComparisonResult{SQL: sqlText, Status: "skipped", Detail: err.Error()}
	}
	ok, detail := compareRows(projected, dbRows)
	if !ok {
		return &ComparisonResult{SQL: sqlText, Status: "mismatch", Detail: detail}
	}
	return &ComparisonResult{SQL: sqlText, Status: "match"}
}

// TestSQLiteUniqueOrder compares the SQLite-supported subset on unique order
// keys.  SQLite lacks GROUPS and EXCLUDE, so those are not included.
func TestSQLiteUniqueOrder(t *testing.T) {
	tbl := uniqueTable(200, 31)
	ctx, h := loadSQLite(t, tbl)
	if h == nil {
		return
	}
	queries := []string{
		"SELECT id, sum(v) OVER (ORDER BY v) FROM races",
		"SELECT id, sum(v) OVER (PARTITION BY g ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC ROWS BETWEEN 3 PRECEDING AND 3 FOLLOWING) FROM races",
		"SELECT id, count(*) OVER (ORDER BY v) FROM races",
		"SELECT id, count(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, avg(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, min(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, max(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, rank() OVER (ORDER BY v) FROM races",
		"SELECT id, dense_rank() OVER (ORDER BY v) FROM races",
		"SELECT id, percent_rank() OVER (ORDER BY v) FROM races",
		"SELECT id, cume_dist() OVER (ORDER BY v) FROM races",
		"SELECT id, ntile(4) OVER (ORDER BY v) FROM races",
		"SELECT id, row_number() OVER (PARTITION BY g ORDER BY v) FROM races",
		"SELECT id, lag(v, 1, -1) OVER (ORDER BY id) FROM races",
		"SELECT id, lead(v, 2, v * 0 - 999) OVER (PARTITION BY g ORDER BY id) FROM races",
		"SELECT id, first_value(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, last_value(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, nth_value(v, 2) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT id, sum(x) OVER (ORDER BY x RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY s ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(v) FILTER (WHERE v > 20) OVER (ORDER BY v) FROM races",
	}
	failed := 0
	matches := 0
	for _, q := range queries {
		res := runSQLiteCompare(t, ctx, h, q)
		switch res.Status {
		case "match":
			matches++
		case "skipped":
			t.Logf("SKIP %s: %s", q, res.Detail)
		default:
			t.Errorf("%s: %s | %s", res.Status, q, res.Detail)
			failed++
		}
	}
	t.Logf("sqlite: %d matches, %d failures of %d", matches, failed, len(queries))
	if failed > 0 {
		t.Fatalf("%d/%d failed", failed, len(queries))
	}
}

// TestSQLitePeers compares peer-invariant queries including NULL placement.
func TestSQLitePeers(t *testing.T) {
	tbl := gen.GenerateSmall(200, 777)
	ctx, h := loadSQLite(t, tbl)
	if h == nil {
		return
	}
	queries := []string{
		"SELECT id, rank() OVER (ORDER BY v NULLS LAST) FROM races",
		"SELECT id, dense_rank() OVER (ORDER BY v NULLS LAST) FROM races",
		"SELECT id, percent_rank() OVER (ORDER BY v NULLS LAST) FROM races",
		"SELECT id, cume_dist() OVER (ORDER BY v NULLS LAST) FROM races",
		"SELECT id, rank() OVER (ORDER BY v DESC NULLS FIRST) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS LAST) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC NULLS FIRST) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS FIRST RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS LAST RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v DESC NULLS FIRST RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS FIRST RANGE BETWEEN 10 PRECEDING AND CURRENT ROW) FROM races",
		"SELECT id, sum(v) OVER (ORDER BY v NULLS LAST RANGE BETWEEN CURRENT ROW AND 10 FOLLOWING) FROM races",
		"SELECT id, sum(x) OVER (ORDER BY x NULLS LAST RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT id, sum(x) OVER (ORDER BY x DESC NULLS FIRST RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
	}
	failed := 0
	for _, q := range queries {
		res := runSQLiteCompare(t, ctx, h, q)
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
	_ = sql.ErrNoRows
}
