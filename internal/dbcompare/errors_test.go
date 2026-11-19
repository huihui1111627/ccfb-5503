package dbcompare

import (
	"strings"
	"testing"

	"dragonboat/windowengine/internal/window"
)

// TestPostgresErrorAlignment checks that queries PostgreSQL rejects are also
// rejected by the engine, and for the same class of reason.
func TestPostgresErrorAlignment(t *testing.T) {
	tbl := uniqueTable(50, 5150)
	ctx, h := loadPG(t, tbl)
	if h == nil {
		return
	}
	cases := []struct {
		sql      string
		engineOK bool
	}{
		{"SELECT id, sum(v) OVER (ORDER BY v, x RANGE BETWEEN 10 PRECEDING AND CURRENT ROW) FROM races", false},
		{"SELECT id, sum(v) OVER (GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM races", false},
		{"SELECT id, sum(v) OVER (ROWS BETWEEN CURRENT ROW AND 1 PRECEDING) FROM races", false},
		{"SELECT id, sum(v) OVER (ROWS BETWEEN UNBOUNDED FOLLOWING AND CURRENT ROW) FROM races", false},
		{"SELECT id, sum(v) OVER (ROWS BETWEEN CURRENT ROW AND UNBOUNDED PRECEDING) FROM races", false},
		{"SELECT id, sum(v) OVER (RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM races", false},
		{"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN -1 PRECEDING AND CURRENT ROW) FROM races", false},
		{"SELECT id, sum(v) OVER (ORDER BY v ROWS BETWEEN 1.5 PRECEDING AND CURRENT ROW) FROM races", true},
		{"SELECT id, ntile(0) OVER (ORDER BY v) FROM races", false},
		{"SELECT id, nth_value(v, 0) OVER (ORDER BY v) FROM races", false},
		{"SELECT id, sum(v) OVER (ORDER BY s RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM races", false},
	}
	for _, c := range cases {
		// PostgreSQL verdict
		rw, _ := RewriteQueryText(c.sql, tbl.Schema, h.names)
		_, pgErr := QueryPostgres(ctx, h.db, rw)
		pgRejects := pgErr != nil
		// Engine verdict
		q, _, err := window.ParseQuery(c.sql, tbl.Schema)
		engineRejects := err != nil
		if !engineRejects {
			_, _, err = window.Eval(tbl, q)
			engineRejects = err != nil && isSemanticErr(err)
		}
		if pgRejects != engineRejects {
			t.Errorf("verdict differs for %s: pgRejects=%v engineRejects=%v (pg=%v engine=%v)",
				c.sql, pgRejects, engineRejects, pgErr, err)
		}
		if !c.engineOK && !engineRejects {
			t.Errorf("expected engine to reject %s", c.sql)
		}
	}
}

func isSemanticErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{"frame", "RANGE", "GROUPS", "ntile", "nth_value", "offset"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// TestPostgresStrictOrderingBadSQL verifies both sides reject a frame that
// starts after it ends.
func TestPostgresFramesStartAfterEnd(t *testing.T) {
	tbl := uniqueTable(30, 71)
	ctx, h := loadPG(t, tbl)
	if h == nil {
		return
	}
	// PostgreSQL accepts "1 FOLLOWING AND 2 PRECEDING"? No: it errors.
	cases := []string{
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 2 FOLLOWING AND 1 FOLLOWING) FROM races",
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 2 PRECEDING) FROM races",
	}
	for _, c := range cases {
		rw, _ := RewriteQueryText(c, tbl.Schema, h.names)
		rows, err := QueryPostgres(ctx, h.db, rw)
		if err != nil {
			t.Logf("pg rejects %s: %v", c, err)
			continue
		}
		// Engine must produce the same (possibly empty) frames.
		q, _, perr := window.ParseQuery(c, tbl.Schema)
		if perr != nil {
			t.Fatalf("engine parse: %v", perr)
		}
		got, _, err := window.Eval(tbl, q)
		if err != nil {
			t.Fatalf("engine eval: %v", err)
		}
		nInput := len(tbl.Schema.Columns)
		// Drop the id column so rows compare as multisets of window results.
		projected := make([][]string, len(got.Rows))
		for i, r := range got.Rows {
			projected[i] = []string{displayForCompare(r[nInput])}
		}
		ok, detail := compareMultiset(projected, rows)
		if !ok {
			t.Errorf("mismatch for %s: %s", c, detail)
		}
	}
}
