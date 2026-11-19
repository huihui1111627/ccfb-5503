// Command wfverify runs the full differential validation suite:
//
//   - engine vs. brute-force reference over a large generated corpus
//   - engine vs. PostgreSQL and SQLite when those servers/bindings exist
//
// It exits non-zero if anything disagrees.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dragonboat/windowengine/internal/dbcompare"
	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/ref"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
	"dragonboat/windowengine/internal/window"
)

func main() {
	rows := flagInt("rows", 200_000, "race dataset rows")
	queries := flagInt("queries", 300, "random queries per profile")
	seed := flagInt64("seed", 2024, "random seed")
	skipPG := flagBool("skip-pg", false, "skip PostgreSQL comparison")
	skipSQLite := flagBool("skip-sqlite", false, "skip SQLite comparison")
	dbRows := flagInt("db-rows", 400, "rows loaded into PostgreSQL/SQLite for comparison")
	flagParse()

	start := time.Now()
	fmt.Printf("generating %d race rows...\n", *rows)
	tbl, err := gen.Generate(gen.DefaultOptions(*rows, *seed))
	if err != nil {
		fail("generate: %v", err)
	}
	fmt.Printf("dataset: %d rows, %d columns\n", len(tbl.Rows), tbl.Schema.Len())

	// The reference is intentionally naive: it sorts the partition and walks
	// every frame row by row.  On the largest datasets a full corpus is
	// dominated by sorting the same 400k rows once per query, so the exhaustive
	// cross-check runs on a sample of rows while the engine's timings are
	// reported on the full dataset.
	fmt.Println("\n== engine timing on the full dataset ==")
	reportTimings(tbl, 6)

	fmt.Println("\n== differential: engine vs. brute-force reference (bounded frames) ==")
	refTbl := sampleRows(tbl, refSampleRows(*rows), *seed+5)
	corpus, err := gen.GenCorpus(refTbl.Schema, *queries, *seed*31+7, "bounded")
	if err != nil {
		fail("corpus: %v", err)
	}
	engineStart := time.Now()
	type outcome struct {
		sql    string
		status string
		detail string
	}
	results := make([]outcome, len(corpus))
	// Limit concurrency: each worker holds an input copy and a result table,
	// and the reference builds per-partition data too.  More than a few
	// workers on a 400k-row dataset only adds GC pressure.
	workers := runtime.NumCPU()
	if len(tbl.Rows) > 100_000 && workers > 4 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var done atomic.Int64
	for i, c := range corpus {
		wg.Add(1)
		go func(i int, sqlText string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			o := outcome{sql: sqlText, status: "ok"}
			q, _, err := window.ParseQuery(sqlText, refTbl.Schema)
			if err != nil {
				o.status, o.detail = "PARSE FAIL", err.Error()
				results[i] = o
				return
			}
			got, _, err := window.Eval(refTbl, q)
			if err != nil {
				o.status, o.detail = "ENGINE FAIL", err.Error()
				results[i] = o
				return
			}
			want, err := ref.Eval(refTbl, q)
			if err != nil {
				o.status, o.detail = "REF FAIL", err.Error()
				results[i] = o
				return
			}
			if ok, detail := compareOutput(got, want, refTbl.Schema.Len()); !ok {
				o.status, o.detail = "MISMATCH", detail
			}
			results[i] = o
			if n := done.Add(1); n%50 == 0 {
				fmt.Printf("  %d/%d checked\n", n, len(corpus))
			}
		}(i, c.SQL)
	}
	wg.Wait()
	matched, failed := 0, 0
	for _, o := range results {
		if o.status == "ok" {
			matched++
			continue
		}
		failed++
		fmt.Printf("  %s %s: %s\n", o.status, o.sql, o.detail)
	}
	fmt.Printf("reference: %d matched, %d failed of %d queries (%.1fs)\n",
		matched, failed, len(corpus), time.Since(engineStart).Seconds())
	if failed > 0 {
		os.Exit(1)
	}

	fmt.Println("\n== differential: engine vs. reference (moderate data, all frame kinds) ==")
	{
		mid, err := gen.Generate(gen.DefaultOptions(1200, *seed+11))
		if err != nil {
			fail("generate: %v", err)
		}
		mc, _ := gen.GenCorpus(mid.Schema, 120, *seed*41+3, "race")
		m, f := 0, 0
		for _, c := range mc {
			q, _, err := window.ParseQuery(c.SQL, mid.Schema)
			if err != nil {
				f++
				continue
			}
			got, _, err := window.Eval(mid, q)
			if err != nil {
				f++
				continue
			}
			want, err := ref.Eval(mid, q)
			if err != nil {
				f++
				continue
			}
			ok, detail := compareOutput(got, want, mid.Schema.Len())
			if !ok {
				fmt.Printf("  MISMATCH %s: %s\n", c.SQL, detail)
				f++
				continue
			}
			m++
		}
		fmt.Printf("  %d matched, %d failed of %d queries\n", m, f, len(mc))
		if f > 0 {
			os.Exit(1)
		}
	}

	fmt.Println("\n== differential: adversarial (peers, NULLs, small rows) ==")
	for _, s := range []int64{1, 2, 3} {
		small := gen.GenerateSmall(150, *seed*7+s)
		corpus, _ := gen.GenCorpus(small.Schema, 200, *seed*17+s, "adversarial")
		m, f := 0, 0
		for _, c := range corpus {
			q, _, err := window.ParseQuery(c.SQL, small.Schema)
			if err != nil {
				f++
				continue
			}
			got, _, err := window.Eval(small, q)
			if err != nil {
				f++
				continue
			}
			want, err := ref.Eval(small, q)
			if err != nil {
				f++
				continue
			}
			ok, detail := compareOutput(got, want, small.Schema.Len())
			if !ok {
				fmt.Printf("  MISMATCH %s: %s\n", c.SQL, detail)
				f++
				continue
			}
			m++
		}
		fmt.Printf("  seed %d: %d matched, %d failed\n", s, m, f)
		if f > 0 {
			os.Exit(1)
		}
	}

	if !*skipPG {
		fmt.Println("\n== PostgreSQL 16 comparison ==")
		if err := comparePostgres(tbl, *dbRows, *seed); err != nil {
			fmt.Printf("  FAILED: %v\n", err)
			os.Exit(1)
		}
	}
	if !*skipSQLite {
		fmt.Println("\n== SQLite comparison ==")
		if err := compareSQLite(tbl, *dbRows, *seed); err != nil {
			fmt.Printf("  FAILED: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("\nALL CHECKS PASSED in %.1fs\n", time.Since(start).Seconds())
}

// comparePostgres runs the engine and PostgreSQL over a curated query set and
// a random corpus restricted to portable SQL.
func comparePostgres(big *table.Table, dbRows int, seed int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := dbcompare.OpenPostgres(ctx)
	if err != nil {
		fmt.Printf("  postgres unavailable, skipping: %v\n", err)
		return nil
	}
	defer db.Close()
	tbl := sampleRows(big, dbRows, seed)
	names, err := dbcompare.PreparePostgres(ctx, db, tbl, "races")
	if err != nil {
		return err
	}
	queries := dbcompare.RaceQueries()
	checked, matched, skipped := 0, 0, 0
	for _, q := range queries {
		if !dbcompare.PGCompatible(q) {
			skipped++
			continue
		}
		res := runCompare(ctx, db, tbl, names, q, "pg")
		switch res.Status {
		case "match":
			matched++
			checked++
		case "skipped":
			skipped++
			fmt.Printf("  postgres skip: %s\n    reason: %s\n", q, res.Detail)
		default:
			return fmt.Errorf("%s: %s: %s", res.Status, q, res.Detail)
		}
	}
	fmt.Printf("  postgres: %d matched, %d skipped (non-portable SQL) of %d curated queries\n", matched, skipped, len(queries))
	// Random queries are run on a unique-order table so that ROWS frames are
	// deterministic across implementations.
	uniq := gen.UniqueTable(maxInt(dbRows, 50), seed+1)
	names, err = dbcompare.PreparePostgres(ctx, db, uniq, "races")
	if err != nil {
		return err
	}
	rq := randomQueries(uniq, 60, seed+2)
	m2, s2 := 0, 0
	for _, q := range rq {
		if !dbcompare.PGCompatible(q) {
			s2++
			continue
		}
		res := runCompare(ctx, db, uniq, names, q, "pg")
		switch res.Status {
		case "match":
			m2++
		case "skipped":
			s2++
		default:
			return fmt.Errorf("%s: %s: %s", res.Status, q, res.Detail)
		}
	}
	fmt.Printf("  postgres random corpus: %d matched, %d skipped of %d queries\n", m2, s2, len(rq))
	return nil
}

func compareSQLite(big *table.Table, dbRows int, seed int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := dbcompare.OpenSQLite(ctx)
	if err != nil {
		fmt.Printf("  sqlite unavailable, skipping: %v\n", err)
		return nil
	}
	defer db.Close()
	tbl := sampleRows(big, dbRows, seed)
	names, err := dbcompare.PrepareSQLite(ctx, db, tbl, "races")
	if err != nil {
		return err
	}
	queries := dbcompare.RaceSQLiteQueries()
	matched, skipped := 0, 0
	for _, q := range queries {
		res := runCompare(ctx, db, tbl, names, q, "sqlite")
		switch res.Status {
		case "match":
			matched++
		case "skipped":
			skipped++
			fmt.Printf("  sqlite skip: %s: %s\n", q, res.Detail)
		default:
			return fmt.Errorf("%s: %s: %s", res.Status, q, res.Detail)
		}
	}
	fmt.Printf("  sqlite: %d matched, %d skipped of %d curated queries\n", matched, skipped, len(queries))
	return nil
}

func runCompare(ctx context.Context, db *sql.DB, tbl *table.Table, names []string, sqlText, which string) *dbcompare.ComparisonResult {
	q, _, err := window.ParseQuery(sqlText, tbl.Schema)
	if err != nil {
		return &dbcompare.ComparisonResult{SQL: sqlText, Status: "error", Detail: "parse: " + err.Error()}
	}
	got, _, err := window.Eval(tbl, q)
	if err != nil {
		return &dbcompare.ComparisonResult{SQL: sqlText, Status: "error", Detail: "engine: " + err.Error()}
	}
	nInput := tbl.Schema.Len()
	projected := make([][]string, len(got.Rows))
	for i, r := range got.Rows {
		line := make([]string, len(r)-nInput)
		for j := range line {
			line[j] = dbcompare.Display(r[nInput+j])
		}
		projected[i] = line
	}
	rw, err := dbcompare.Rewrite(sqlText, tbl.Schema, names)
	if err != nil {
		return &dbcompare.ComparisonResult{SQL: sqlText, Status: "error", Detail: err.Error()}
	}
	var rows [][]string
	switch which {
	case "pg":
		rows, err = dbcompare.QueryPostgres(ctx, db, rw)
	default:
		rows, err = dbcompare.QuerySQLite(ctx, db, rw)
	}
	if err != nil {
		if isUnsupported(err) {
			return &dbcompare.ComparisonResult{SQL: sqlText, Status: "skipped", Detail: err.Error()}
		}
		return &dbcompare.ComparisonResult{SQL: sqlText, Status: "error", Detail: err.Error()}
	}
	ok, detail := dbcompare.CompareMultiset(projected, rows)
	if !ok {
		return &dbcompare.ComparisonResult{SQL: sqlText, Status: "mismatch", Detail: detail}
	}
	return &dbcompare.ComparisonResult{SQL: sqlText, Status: "match"}
}

func flagInt(name string, def int, usage string) *int {
	p := new(int)
	*p = def
	flag.IntVar(p, name, def, usage)
	return p
}
func flagInt64(name string, def int64, usage string) *int64 {
	p := new(int64)
	*p = def
	flag.Int64Var(p, name, def, usage)
	return p
}
func flagBool(name string, def bool, usage string) *bool {
	p := new(bool)
	*p = def
	flag.BoolVar(p, name, def, usage)
	return p
}
func flagParse() { flag.Parse() }

func newRng(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

func isUnsupported(err error) bool {
	msg := err.Error()
	for _, s := range []string{
		"must not contain variables", "syntax error", "not implemented",
		"is not supported", "no such column", "near ", "unsupported",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// sampleRows deterministically picks n rows, preserving their input order.
func sampleRows(t *table.Table, n int, seed int64) *table.Table {
	if n >= len(t.Rows) {
		return t
	}
	rng := newRng(seed)
	idx := rng.Perm(len(t.Rows))[:n]
	sort.Ints(idx)
	out := &table.Table{Schema: t.Schema}
	for _, i := range idx {
		out.Rows = append(out.Rows, table.CloneRow(t.Rows[i]))
	}
	return out
}

func randomQueries(tbl *table.Table, n int, seed int64) []string {
	corpus, _ := gen.GenCorpus(tbl.Schema, n, seed*13+1, "race")
	out := make([]string, len(corpus))
	for i, c := range corpus {
		out[i] = c.SQL
	}
	return out
}

// compareOutput matches rows on their untouched input columns, because an
// output ORDER BY may re-order results and SQL leaves peer order undefined.
func compareOutput(got, want *table.Table, nInput int) (bool, string) {
	if len(got.Rows) != len(want.Rows) {
		return false, fmt.Sprintf("row counts differ: %d vs %d", len(got.Rows), len(want.Rows))
	}
	if got.Schema.Len() != want.Schema.Len() {
		return false, "column counts differ"
	}
	rowKey := func(r table.Row) string {
		var b strings.Builder
		for i := 0; i < nInput; i++ {
			if r[i].IsNull() {
				b.WriteString("\x00")
			} else {
				b.WriteString(r[i].Kind.String())
				b.WriteByte(0x1f)
				b.WriteString(r[i].String())
			}
			b.WriteByte(0x1e)
		}
		return b.String()
	}
	wantByKey := make(map[string]table.Row, len(want.Rows))
	dupes := false
	for _, r := range want.Rows {
		k := rowKey(r)
		if _, ok := wantByKey[k]; ok {
			dupes = true
		}
		wantByKey[k] = r
	}
	if dupes {
		// Identical input rows: compare the multiset of result tuples.
		type rec struct{ in, out string }
		outKey := func(r table.Row) string {
			var b strings.Builder
			for j := nInput; j < len(r); j++ {
				if r[j].IsNull() {
					b.WriteString("\x00")
				} else {
					b.WriteString(r[j].Kind.String() + ":" + r[j].String())
				}
				b.WriteByte(0x1f)
			}
			return b.String()
		}
		bag := map[rec]int{}
		for _, r := range want.Rows {
			bag[rec{rowKey(r), outKey(r)}]++
		}
		for i, r := range got.Rows {
			k := rec{rowKey(r), outKey(r)}
			if bag[k] == 0 {
				return false, fmt.Sprintf("row %d result tuple missing in reference", i)
			}
			bag[k]--
		}
		return true, ""
	}
	for i, r := range got.Rows {
		w, ok := wantByKey[rowKey(r)]
		if !ok {
			return false, fmt.Sprintf("engine row %d has no matching reference row", i)
		}
		for j := nInput; j < got.Schema.Len(); j++ {
			if !sameCell(r[j], w[j]) {
				return false, fmt.Sprintf("row %d, column %s: engine=%q reference=%q",
					i, got.Schema.Columns[j].Name, r[j].Display(), w[j].Display())
			}
		}
	}
	return true, ""
}

func sameCell(a, b value.Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() && b.IsNull()
	}
	if a.Kind.IsNumericKind() && b.Kind.IsNumericKind() {
		af, e1 := value.ToFloat(a)
		bf, e2 := value.ToFloat(b)
		if e1 == nil && e2 == nil {
			if af == bf {
				return true
			}
			diff := af - bf
			if diff < 0 {
				diff = -diff
			}
			scale := 1.0
			if m := absf(af); m > scale {
				scale = m
			}
			return diff <= 1e-9*scale
		}
	}
	c, err := value.Compare(a, b)
	if err != nil {
		return false
	}
	if a.Kind == value.KindString || b.Kind == value.KindString {
		return a.Kind == b.Kind && a.S == b.S
	}
	return c == 0
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func fail(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
	os.Exit(1)
}

// refSampleRows picks how many rows the exhaustive reference pass uses.
func refSampleRows(total int) int {
	if total <= 20000 {
		return total
	}
	return 20000
}

// reportTimings runs a few representative queries on the full dataset and
// prints per-query wall time and throughput.
func reportTimings(tbl *table.Table, n int) {
	cases := []string{
		"SELECT sum(elapsed_ms) OVER (PARTITION BY heat ORDER BY split, lane, elapsed_ms) AS s FROM races",
		"SELECT sum(elapsed_ms) OVER (ORDER BY elapsed_ms ROWS BETWEEN 100 PRECEDING AND 100 FOLLOWING) AS s FROM races",
		"SELECT avg(speed) OVER (ORDER BY speed ROWS BETWEEN 500 PRECEDING AND 500 FOLLOWING) AS a FROM races",
		"SELECT count(*) OVER (ORDER BY ts RANGE BETWEEN INTERVAL '2 minutes' PRECEDING AND INTERVAL '2 minutes' FOLLOWING) AS c FROM races",
		"SELECT sum(split_ms) OVER (ORDER BY lane GROUPS BETWEEN 2 PRECEDING AND 2 FOLLOWING) AS s FROM races",
		"SELECT rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS r, dense_rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS d FROM races",
	}
	for i, sqlText := range cases {
		if i >= n {
			break
		}
		q, _, err := window.ParseQuery(sqlText, tbl.Schema)
		if err != nil {
			fmt.Printf("  parse error: %v\n", err)
			continue
		}
		start := time.Now()
		if _, _, err := window.Eval(tbl, q); err != nil {
			fmt.Printf("  eval error: %v\n", err)
			continue
		}
		d := time.Since(start)
		fmt.Printf("  %7s  %6.2fM rows/s  %s\n", d.Round(time.Millisecond),
			float64(len(tbl.Rows))/d.Seconds()/1e6, shortQuery(sqlText))
	}
}

func shortQuery(s string) string {
	s = strings.TrimPrefix(s, "SELECT ")
	if i := strings.Index(s, " FROM races"); i > 0 {
		s = s[:i]
	}
	if len(s) > 84 {
		s = s[:84] + "..."
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
