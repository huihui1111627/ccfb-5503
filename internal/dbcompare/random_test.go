package dbcompare

import (
	"testing"

	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/window"
)

// TestPostgresRandomCorpus takes random generated queries, keeps those that
// PostgreSQL can execute verbatim (constants only, no extension features),
// and compares them on unique-order data.
func TestPostgresRandomCorpus(t *testing.T) {
	tbl := uniqueTable(400, 2024)
	ctx, h := loadPG(t, tbl)
	if h == nil {
		return
	}
	corpus, err := gen.GenCorpus(tbl.Schema, 2500, 31415, "adversarial")
	if err != nil {
		t.Fatal(err)
	}
	checked, matched, skipped, shown := 0, 0, 0, 0
	var failures []string
	statusCounts := map[string]int{}
	skipShown := 0
	for _, c := range corpus {
		if !PGCompatible(c.SQL) {
			skipped++
			continue
		}
		q, _, err := window.ParseQuery(c.SQL, tbl.Schema)
		if err != nil {
			t.Logf("PARSE FAIL %s: %v", c.SQL, err)
			continue
		}
		checked++
		res := runPGCompare(t, ctx, h, c.SQL)
		statusCounts[res.Status]++
		switch res.Status {
		case "match":
			matched++
		case "skipped":
			skipped++
			if skipShown < 5 {
				skipShown++
				t.Logf("SKIP %s | %s", c.SQL, res.Detail)
			}
		default:
			shown++
			if shown <= 8 {
				t.Errorf("%s: %s | %s", res.Status, c.SQL, res.Detail)
			} else {
				failures = append(failures, res.Status+": "+c.SQL+" | "+res.Detail)
			}
		}
		_ = q
	}
	t.Logf("random corpus: %d compared, %d matched, %d skipped as PG-incompatible; statuses: %v", checked, matched, skipped, statusCounts)
	if matched != checked {
		t.Fatalf("%d/%d random queries disagreed with PostgreSQL; first failures: %v",
			checked-matched, checked, failures[:minInt(len(failures), 3)])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
