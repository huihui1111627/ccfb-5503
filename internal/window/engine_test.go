package window_test

import (
	"fmt"
	"strings"
	"testing"

	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/ref"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
	"dragonboat/windowengine/internal/window"
)

// runBoth parses, executes with the engine and the reference implementation,
// and compares every cell.
func runBoth(t *testing.T, tbl *table.Table, sql string) {
	t.Helper()
	q, _, err := window.ParseQuery(sql, tbl.Schema)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	got, _, err := window.Eval(tbl, q)
	if err != nil {
		t.Fatalf("engine %q: %v", sql, err)
	}
	want, err := ref.Eval(tbl, q)
	if err != nil {
		t.Fatalf("ref %q: %v", sql, err)
	}
	if err := compareTables(got, want, tbl.Schema.Len()); err != nil {
		t.Fatalf("%s\n%s", sql, err)
	}
}

// compareTables compares two results by input-row identity: the first nInput
// columns are the untouched input values.  This is required because an output
// ORDER BY may re-order rows and SQL leaves peer order undefined.
func compareTables(got, want *table.Table, nInput int) error {
	if len(got.Rows) != len(want.Rows) {
		return fmt.Errorf("row count: got %d want %d", len(got.Rows), len(want.Rows))
	}
	if got.Schema.Len() != want.Schema.Len() {
		return fmt.Errorf("column count: got %d want %d", got.Schema.Len(), want.Schema.Len())
	}
	// Detect the number of untouched input columns: identical schema value
	// kinds in both tables, but the trailing columns are results.  The engine
	// and reference implementations append results in the same positions, so
	// we can find nInput as the largest prefix whose values are row-identical
	// in both tables before sorting.  Simpler: use the fact that callers pass
	// tables built from the same input; the input columns are the columns up
	// to the first result column name.
	key := func(r table.Row) string {
		var b strings.Builder
		for i := 0; i < nInput; i++ {
			v := r[i]
			if v.IsNull() {
				b.WriteString("\x00")
			} else {
				b.WriteString(v.Kind.String())
				b.WriteByte(0x1f)
				b.WriteString(v.String())
			}
			b.WriteByte(0x1e)
		}
		return b.String()
	}
	wantByKey := make(map[string]table.Row, len(want.Rows))
	dup := false
	for _, r := range want.Rows {
		k := key(r)
		if _, ok := wantByKey[k]; ok {
			dup = true
		}
		wantByKey[k] = r
	}
	if dup {
		// Duplicate input rows: compare positionally (both sides keep input
		// order when there is no output ORDER BY; with one, equal input rows
		// also have equal results only for peer-insensitive functions, so fall
		// back to multisets of result tuples).
		return compareResultMultiset(got, want, nInput)
	}
	for i, r := range got.Rows {
		w, ok := wantByKey[key(r)]
		if !ok {
			return fmt.Errorf("engine row %d has no matching reference row", i)
		}
		for j := nInput; j < got.Schema.Len(); j++ {
			if !sameValue(r[j], w[j]) {
				return fmt.Errorf("row %d col %s: got %s (%s) want %s (%s)",
					i, got.Schema.Columns[j].Name, r[j].Display(), r[j].Kind, w[j].Display(), w[j].Kind)
			}
		}
	}
	return nil
}

func compareResultMultiset(got, want *table.Table, nInput int) error {
	type rec struct {
		in, out string
	}
	key := func(r table.Row, inN int) string {
		var b strings.Builder
		for i := 0; i < inN; i++ {
			if r[i].IsNull() {
				b.WriteString("\x00")
			} else {
				b.WriteString(r[i].Kind.String() + ":" + r[i].String())
			}
			b.WriteByte(0x1e)
		}
		return b.String()
	}
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
	wantSet := map[rec]int{}
	for _, r := range want.Rows {
		wantSet[rec{key(r, nInput), outKey(r)}]++
	}
	for i, r := range got.Rows {
		k := rec{key(r, nInput), outKey(r)}
		if wantSet[k] == 0 {
			return fmt.Errorf("row %d: result tuple not present in reference output", i)
		}
		wantSet[k]--
	}
	return nil
}

func sameValue(a, b value.Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() && b.IsNull()
	}
	if a.Kind.IsNumericKind() && b.Kind.IsNumericKind() {
		af, err1 := value.ToFloat(a)
		bf, err2 := value.ToFloat(b)
		if err1 != nil || err2 != nil {
			return false
		}
		if af == bf {
			return true
		}
		// tolerate float formatting differences
		return fmt.Sprintf("%.12g", af) == fmt.Sprintf("%.12g", bf)
	}
	if a.Kind != b.Kind {
		return false
	}
	c, err := value.Compare(a, b)
	return err == nil && c == 0 && a.String() == b.String() || err == nil && c == 0 && a.Kind != value.KindString
}

func TestBasicFrames(t *testing.T) {
	tbl := gen.GenerateSmall(60, 7)
	queries := []string{
		"SELECT sum(v) OVER (ORDER BY id) FROM races",
		"SELECT sum(v) OVER (PARTITION BY g ORDER BY v) FROM races",
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM races",
		"SELECT count(*) OVER (ORDER BY v) FROM races",
		"SELECT count(v) OVER (ORDER BY v) FROM races",
		"SELECT avg(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT min(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT max(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT sum(v) OVER (ORDER BY v GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT sum(v) OVER (ORDER BY v RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT sum(v) OVER (ORDER BY v DESC RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING) FROM races",
		"SELECT rank() OVER (ORDER BY v) FROM races",
		"SELECT dense_rank() OVER (ORDER BY v) FROM races",
		"SELECT row_number() OVER (PARTITION BY g ORDER BY v) FROM races",
		"SELECT percent_rank() OVER (ORDER BY v) FROM races",
		"SELECT cume_dist() OVER (ORDER BY v) FROM races",
		"SELECT ntile(4) OVER (ORDER BY v) FROM races",
		"SELECT lag(v, 1, -1) OVER (ORDER BY id) FROM races",
		"SELECT lead(v, 2) OVER (PARTITION BY g ORDER BY id) FROM races",
		"SELECT first_value(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT last_value(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM races",
		"SELECT nth_value(v, 2) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 2 FOLLOWING) FROM races",
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE GROUP) FROM races",
		"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE TIES) FROM races",
		"SELECT min(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (ORDER BY v RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (ORDER BY v RANGE BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM races",
		"SELECT count(*) OVER (ORDER BY ts RANGE BETWEEN INTERVAL '1 minute' PRECEDING AND CURRENT ROW) FROM races",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) { runBoth(t, tbl, q) })
	}
}

func TestRandomSmall(t *testing.T) {
	tbl := gen.GenerateSmall(80, 11)
	corpus, err := gen.GenCorpus(tbl.Schema, 300, 5, "adversarial")
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, c := range corpus {
		q, _, err := window.ParseQuery(c.SQL, tbl.Schema)
		if err != nil {
			t.Logf("PARSE FAIL %s: %v", c.SQL, err)
			failures++
			continue
		}
		got, _, err := window.Eval(tbl, q)
		if err != nil {
			t.Logf("ENGINE FAIL %s: %v", c.SQL, err)
			failures++
			continue
		}
		want, err := ref.Eval(tbl, q)
		if err != nil {
			t.Logf("REF FAIL %s: %v", c.SQL, err)
			failures++
			continue
		}
		if err := compareTables(got, want, tbl.Schema.Len()); err != nil {
			t.Logf("MISMATCH %s: %v", c.SQL, err)
			failures++
		}
		if failures > 10 {
			t.Fatalf("too many failures")
		}
	}
	if failures > 0 {
		t.Fatalf("%d/%d failures", failures, len(corpus))
	}
}
