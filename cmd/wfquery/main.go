// Command wfquery runs one window query against generated race data and
// prints the result, the plan and timings.  Optionally it cross-checks against
// the brute-force reference and PostgreSQL.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"dragonboat/windowengine/internal/dbcompare"
	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/ref"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/window"
)

func main() {
	rows := flag.Int("rows", 20000, "race dataset rows")
	seed := flag.Int64("seed", 2024, "random seed")
	sqlText := flag.String("sql", "", "query (defaults to a running-total example)")
	checkRef := flag.Bool("ref", false, "cross-check with the brute-force reference")
	checkPG := flag.Bool("pg", false, "cross-check with PostgreSQL")
	limit := flag.Int("limit", 20, "result rows to print")
	flag.Parse()

	if *sqlText == "" {
		*sqlText = "SELECT sum(elapsed_ms) OVER (PARTITION BY heat ORDER BY split) AS running_ms, " +
			"lag(elapsed_ms, 1, 0) OVER (PARTITION BY heat ORDER BY split, lane, elapsed_ms) AS prev_ms, " +
			"rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS rank_in_heat FROM races"
	}
	tbl, err := gen.Generate(gen.DefaultOptions(*rows, *seed))
	if err != nil {
		fail(err)
	}
	q, _, err := window.ParseQuery(*sqlText, tbl.Schema)
	if err != nil {
		fail(err)
	}
	start := time.Now()
	out, plan, err := window.Eval(tbl, q)
	elapsed := time.Since(start)
	if err != nil {
		fail(err)
	}

	fmt.Printf("input: %d rows, %d columns\n", len(tbl.Rows), tbl.Schema.Len())
	fmt.Printf("engine: %s for %d rows (%.2fM rows/s)\n\n", elapsed.Round(time.Microsecond),
		len(out.Rows), float64(len(out.Rows))/elapsed.Seconds()/1e6)

	fmt.Println("plan:")
	for _, s := range plan.Sorts {
		fmt.Printf("  %s  partition=%v order=%v items=%v\n", s.ID, s.Partition, s.Order, s.Items)
	}
	for _, it := range plan.Items {
		fmt.Printf("  %-14s %-8s %-34s %s %s\n", it.OutName, it.Func, it.Strategy, it.Frame, it.Exclusion)
	}

	nIn := tbl.Schema.Len()
	fmt.Println("\nresults:")
	cols := out.Schema.Columns[nIn:]
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.Name
	}
	fmt.Println("  " + strings.Join(header, "\t"))
	for i, r := range out.Rows {
		if i >= *limit {
			fmt.Printf("  ... %d more rows\n", len(out.Rows)-i)
			break
		}
		cells := make([]string, len(cols))
		for j := range cols {
			cells[j] = r[nIn+j].Display()
		}
		fmt.Println("  " + strings.Join(cells, "\t"))
	}

	if *checkRef {
		t0 := time.Now()
		want, err := ref.Eval(tbl, q)
		refTime := time.Since(t0)
		if err != nil {
			fail(fmt.Errorf("reference: %w", err))
		}
		ok, detail := compareOutput(out, want, nIn)
		if ok {
			fmt.Printf("\nreference: MATCH (%s, engine %.1fx faster)\n", refTime.Round(time.Millisecond),
				float64(refTime)/float64(elapsed))
		} else {
			fmt.Printf("\nreference: MISMATCH %s\n", detail)
			os.Exit(1)
		}
	}

	if *checkPG {
		checkPostgres(tbl, *sqlText, out, nIn)
	}
}

func checkPostgres(tbl *table.Table, sqlText string, out *table.Table, nIn int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := dbcompare.OpenPostgres(ctx)
	if err != nil {
		fmt.Printf("\npostgres: unavailable (%v)\n", err)
		return
	}
	defer db.Close()
	if len(tbl.Rows) > 5000 {
		fmt.Println("\npostgres: skipped (use -rows <= 5000 for database comparison)")
		return
	}
	names, err := dbcompare.PreparePostgres(ctx, db, tbl, "races")
	if err != nil {
		fmt.Printf("\npostgres: load failed: %v\n", err)
		return
	}
	rw, _ := dbcompare.Rewrite(sqlText, tbl.Schema, names)
	rows, err := dbcompare.QueryPostgres(ctx, db, rw)
	if err != nil {
		fmt.Printf("\npostgres: query failed: %v\n", err)
		return
	}
	mine := make([][]string, len(out.Rows))
	for i, r := range out.Rows {
		line := make([]string, out.Schema.Len()-nIn)
		for j := range line {
			line[j] = dbcompare.Display(r[nIn+j])
		}
		mine[i] = line
	}
	if ok, detail := dbcompare.CompareMultiset(mine, rows); ok {
		fmt.Println("\npostgres: MATCH")
	} else {
		fmt.Printf("\npostgres: MISMATCH %s\n", detail)
		os.Exit(1)
	}
}

func compareOutput(got, want *table.Table, nInput int) (bool, string) {
	if len(got.Rows) != len(want.Rows) || got.Schema.Len() != want.Schema.Len() {
		return false, "shape differs"
	}
	for i := range got.Rows {
		for j := nInput; j < got.Schema.Len(); j++ {
			if !dbcompare.CellEqual(got.Rows[i][j], want.Rows[i][j]) {
				return false, fmt.Sprintf("row %d column %s: engine=%q reference=%q", i,
					got.Schema.Columns[j].Name, got.Rows[i][j].Display(), want.Rows[i][j].Display())
			}
		}
	}
	return true, ""
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
