package window_test

import (
	"testing"

	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/ref"
	"dragonboat/windowengine/internal/window"
)

// TestDifferentialManySeeds runs a sizeable random corpus against the
// brute-force reference.  It is the primary correctness gate.
func TestDifferentialManySeeds(t *testing.T) {
	seeds := []int64{1, 2, 3, 4, 5}
	total, failed := 0, 0
	for _, seed := range seeds {
		tbl := gen.GenerateSmall(120, seed*97+3)
		corpus, err := gen.GenCorpus(tbl.Schema, 250, seed*13+7, "adversarial")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range corpus {
			total++
			q, _, err := window.ParseQuery(c.SQL, tbl.Schema)
			if err != nil {
				t.Logf("[seed %d] PARSE FAIL %s: %v", seed, c.SQL, err)
				failed++
				continue
			}
			got, _, err := window.Eval(tbl, q)
			if err != nil {
				t.Logf("[seed %d] ENGINE FAIL %s: %v", seed, c.SQL, err)
				failed++
				continue
			}
			want, err := ref.Eval(tbl, q)
			if err != nil {
				t.Logf("[seed %d] REF FAIL %s: %v", seed, c.SQL, err)
				failed++
				continue
			}
			if err := compareTables(got, want, tbl.Schema.Len()); err != nil {
				t.Logf("[seed %d] MISMATCH %s: %v", seed, c.SQL, err)
				failed++
			}
		}
	}
	t.Logf("%d queries, %d failures", total, failed)
	if failed > 0 {
		t.Fatalf("%d/%d failures", failed, total)
	}
}

// TestDifferentialRace exercises the full race dataset (with temporal and
// float order keys) against the reference.
func TestDifferentialRace(t *testing.T) {
	tbl, err := gen.Generate(gen.DefaultOptions(1500, 424242))
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := gen.GenCorpus(tbl.Schema, 150, 99, "race")
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, c := range corpus {
		q, _, err := window.ParseQuery(c.SQL, tbl.Schema)
		if err != nil {
			t.Logf("PARSE FAIL %s: %v", c.SQL, err)
			failed++
			continue
		}
		got, _, err := window.Eval(tbl, q)
		if err != nil {
			t.Logf("ENGINE FAIL %s: %v", c.SQL, err)
			failed++
			continue
		}
		want, err := ref.Eval(tbl, q)
		if err != nil {
			t.Logf("REF FAIL %s: %v", c.SQL, err)
			failed++
			continue
		}
		if err := compareTables(got, want, tbl.Schema.Len()); err != nil {
			t.Logf("MISMATCH %s: %v", c.SQL, err)
			failed++
		}
	}
	if failed > 0 {
		t.Fatalf("%d/%d failures", failed, len(corpus))
	}
}
