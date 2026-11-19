// Command wfbench benchmarks the engine on the race dataset and prints a
// timing table, optionally including the brute-force reference for scale.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/ref"
	"dragonboat/windowengine/internal/window"
)

type benchCase struct {
	name string
	sql  string
}

func main() {
	rows := flag.Int("rows", 300_000, "race dataset rows")
	seed := flag.Int64("seed", 2024, "random seed")
	withRef := flag.Bool("ref", false, "also time the brute-force reference (slow)")
	flag.Parse()

	tbl, err := gen.Generate(gen.DefaultOptions(*rows, *seed))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("dataset: %d rows, %d columns\n\n", len(tbl.Rows), tbl.Schema.Len())

	cases := []benchCase{
		{"running total per heat", "SELECT sum(elapsed_ms) OVER (PARTITION BY heat ORDER BY split) AS running FROM races"},
		{"sliding sum 100 rows", "SELECT sum(elapsed_ms) OVER (ORDER BY elapsed_ms ROWS BETWEEN 100 PRECEDING AND 100 FOLLOWING) AS s FROM races"},
		{"sliding avg 1000 rows", "SELECT avg(speed) OVER (ORDER BY speed ROWS BETWEEN 500 PRECEDING AND 500 FOLLOWING) AS a FROM races"},
		{"range 2 minutes", "SELECT sum(elapsed_ms) OVER (ORDER BY ts RANGE BETWEEN INTERVAL '2 minutes' PRECEDING AND INTERVAL '2 minutes' FOLLOWING) AS s FROM races"},
		{"range value window", "SELECT count(*) OVER (ORDER BY stroke_rate RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING) AS c FROM races"},
		{"groups 2 around", "SELECT sum(split_ms) OVER (ORDER BY lane GROUPS BETWEEN 2 PRECEDING AND 2 FOLLOWING) AS s FROM races"},
		{"rank + dense_rank + ntile", "SELECT rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS r, dense_rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS d, ntile(4) OVER (PARTITION BY heat ORDER BY elapsed_ms) AS n FROM races"},
		{"lag/lead per heat", "SELECT lag(elapsed_ms, 1, 0) OVER (PARTITION BY heat ORDER BY split) AS p, lead(elapsed_ms, 1, 0) OVER (PARTITION BY heat ORDER BY split) AS n FROM races"},
		{"gap to previous", "SELECT lag(elapsed_ms, 1) OVER (PARTITION BY heat ORDER BY split, lane, elapsed_ms) AS prev_ms FROM races"},
		{"three windows one sort", "SELECT rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS r, dense_rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS d, cume_dist() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS c FROM races"},
		{"three sorts", "SELECT rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS a, rank() OVER (PARTITION BY heat ORDER BY split_ms) AS b, rank() OVER (PARTITION BY crew ORDER BY speed) AS c FROM races"},
		{"exclude ties sliding", "SELECT sum(elapsed_ms) OVER (ORDER BY split_ms ROWS BETWEEN 50 PRECEDING AND 50 FOLLOWING EXCLUDE TIES) AS s FROM races"},
		{"first/last/nth", "SELECT first_value(elapsed_ms) OVER (ORDER BY split_ms ROWS BETWEEN 20 PRECEDING AND CURRENT ROW) AS f, last_value(elapsed_ms) OVER (ORDER BY split_ms ROWS BETWEEN 20 PRECEDING AND CURRENT ROW) AS l, nth_value(elapsed_ms, 5) OVER (ORDER BY split_ms ROWS BETWEEN 20 PRECEDING AND CURRENT ROW) AS n FROM races"},
	}

	fmt.Printf("%-28s %10s %12s %14s\n", "query", "rows", "engine", "M rows/s")
	fmt.Printf("%-28s %10s %12s %14s\n", "-----", "----", "------", "---------")
	for _, c := range cases {
		q, _, err := window.ParseQuery(c.sql, tbl.Schema)
		if err != nil {
			fmt.Printf("%-28s parse error: %v\n", c.name, err)
			continue
		}
		// warm-up
		if _, _, err := window.Eval(tbl, q); err != nil {
			fmt.Printf("%-28s eval error: %v\n", c.name, err)
			continue
		}
		const reps = 3
		times := make([]time.Duration, reps)
		for i := 0; i < reps; i++ {
			start := time.Now()
			if _, _, err := window.Eval(tbl, q); err != nil {
				fmt.Printf("%-28s eval error: %v\n", c.name, err)
				times[i] = -1
				break
			}
			times[i] = time.Since(start)
		}
		sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
		med := times[len(times)/2]
		rate := float64(len(tbl.Rows)) / med.Seconds() / 1e6
		fmt.Printf("%-28s %10d %12s %13.2f\n", c.name, len(tbl.Rows), med.Round(time.Millisecond), rate)
	}

	if *withRef {
		fmt.Println("\nreference comparison (smaller dataset):")
		small, _ := gen.Generate(gen.DefaultOptions(5000, *seed))
		for _, c := range cases[:6] {
			q, _, err := window.ParseQuery(c.sql, small.Schema)
			if err != nil {
				continue
			}
			t0 := time.Now()
			window.Eval(small, q)
			eng := time.Since(t0)
			t1 := time.Now()
			ref.Eval(small, q)
			refT := time.Since(t1)
			fmt.Printf("  %-26s engine %8s   reference %8s   speedup %6.1fx\n",
				c.name, eng.Round(time.Millisecond), refT.Round(time.Millisecond),
				float64(refT)/float64(eng))
		}
	}
}
