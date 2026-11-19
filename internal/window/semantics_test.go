package window_test

import (
	"fmt"
	"testing"

	"dragonboat/windowengine/internal/ref"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
	"dragonboat/windowengine/internal/window"
)

// buildTable constructs a table with the given columns of values.
func buildTable(cols map[string][]value.Value, order []string) *table.Table {
	var schemaCols []table.Column
	for _, name := range order {
		kind := value.KindNull
		for _, v := range cols[name] {
			if !v.IsNull() {
				kind = v.Kind
				break
			}
		}
		schemaCols = append(schemaCols, table.Column{Name: name, Kind: kind})
	}
	schema := table.NewSchema(schemaCols...)
	t := table.New(schema)
	n := len(cols[order[0]])
	for i := 0; i < n; i++ {
		row := make(table.Row, len(order))
		for j, name := range order {
			row[j] = cols[name][i]
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// evalOne executes a query and returns the first result column as strings
// (rows in input order; the test queries have no output ORDER BY).
func evalOne(t *testing.T, tbl *table.Table, sql string) []string {
	t.Helper()
	q, _, err := window.ParseQuery(sql, tbl.Schema)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	out, _, err := window.Eval(tbl, q)
	if err != nil {
		t.Fatalf("eval %q: %v", sql, err)
	}
	nIn := tbl.Schema.Len()
	res := make([]string, len(out.Rows))
	for i, r := range out.Rows {
		res[i] = r[nIn].Display()
	}
	return res
}

// evalOneRef runs the same query through the reference implementation.
func evalOneRef(t *testing.T, tbl *table.Table, sql string) []string {
	t.Helper()
	q, _, err := window.ParseQuery(sql, tbl.Schema)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	out, err := ref.Eval(tbl, q)
	if err != nil {
		t.Fatalf("ref eval %q: %v", sql, err)
	}
	nIn := tbl.Schema.Len()
	res := make([]string, len(out.Rows))
	for i, r := range out.Rows {
		res[i] = r[nIn].Display()
	}
	return res
}

func ints(xs ...int64) []value.Value {
	out := make([]value.Value, len(xs))
	for i, x := range xs {
		out[i] = value.Int(x)
	}
	return out
}

func nullables(xs ...interface{}) []value.Value {
	out := make([]value.Value, len(xs))
	for i, x := range xs {
		if x == nil {
			out[i] = value.Null
		} else {
			out[i] = value.Int(int64(x.(int)))
		}
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRangeNullFrames pins the RANGE behaviour around NULL order values that
// was derived from PostgreSQL 16 probes.
func TestRangeNullFrames(t *testing.T) {
	// v = [10, NULL, 30, NULL, 50]; id = 1..5
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3, 4, 5),
		"v":  nullables(10, nil, 30, nil, 50),
	}, []string{"id", "v"})

	cases := []struct {
		sql  string
		want []string
	}{
		// Non-NULL current rows never include the NULL group with a finite
		// offset; UNBOUNDED and CURRENT ROW do include it.
		{"SELECT count(*) OVER (ORDER BY v NULLS LAST RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
			[]string{"1", "5", "2", "5", "3"}},
		{"SELECT count(*) OVER (ORDER BY v NULLS FIRST RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM races",
			[]string{"3", "2", "4", "2", "5"}},
		{"SELECT count(*) OVER (ORDER BY v NULLS LAST RANGE BETWEEN UNBOUNDED PRECEDING AND 20 FOLLOWING) FROM races",
			[]string{"2", "5", "3", "5", "3"}},
		{"SELECT count(*) OVER (ORDER BY v NULLS LAST RANGE BETWEEN 20 PRECEDING AND CURRENT ROW) FROM races",
			[]string{"1", "2", "2", "2", "2"}},
		{"SELECT count(*) OVER (ORDER BY v NULLS LAST RANGE BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM races",
			[]string{"5", "2", "4", "2", "3"}},
		{"SELECT count(*) OVER (ORDER BY v NULLS FIRST RANGE BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM races",
			[]string{"3", "5", "2", "5", "1"}},
	}
	for _, c := range cases {
		got := evalOne(t, tbl, c.sql)
		if !eqStrings(got, c.want) {
			t.Errorf("%s\n got %v\nwant %v", c.sql, got, c.want)
		}
		want := evalOneRef(t, tbl, c.sql)
		if !eqStrings(got, want) {
			t.Errorf("%s: engine %v != reference %v", c.sql, got, want)
		}
	}
}

// TestRangeCurrentRowPeers checks that RANGE CURRENT ROW is the peer group,
// including NULL peers and the unordered case.
func TestRangeCurrentRowPeers(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3, 4, 5, 6),
		"v":  nullables(10, 20, 20, 30, nil, nil),
	}, []string{"id", "v"})
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT count(*) OVER (ORDER BY v NULLS LAST RANGE BETWEEN CURRENT ROW AND CURRENT ROW) FROM races",
			[]string{"1", "1", "1", "1", "1", "1"}}, // not peers because ids? no: v peers
		{"SELECT sum(v) OVER (ORDER BY v NULLS LAST RANGE BETWEEN CURRENT ROW AND CURRENT ROW) FROM races",
			[]string{"10", "40", "40", "30", "", ""}},
		{"SELECT count(*) OVER (ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM races",
			[]string{"6", "6", "6", "6", "6", "6"}},
	}
	for _, c := range cases {
		got := evalOne(t, tbl, c.sql)
		refv := evalOneRef(t, tbl, c.sql)
		if !eqStrings(got, refv) {
			t.Errorf("%s: engine %v != reference %v", c.sql, got, refv)
		}
	}
}

// TestRowsPerRowOffset is the per-row frame offset extension: the offset is an
// expression evaluated on the current row.
func TestRowsPerRowOffset(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3, 4, 5, 6),
		"v":  ints(1, 2, 3, 4, 5, 6),
	}, []string{"id", "v"})
	// Start offset = v % 3 (0,2,0,1,2,0), end = CURRENT ROW.
	sql := "SELECT sum(id) OVER (ORDER BY id ROWS BETWEEN v % 3 PRECEDING AND CURRENT ROW) FROM races"
	got := evalOne(t, tbl, sql)
	want := evalOneRef(t, tbl, sql)
	if !eqStrings(got, want) {
		t.Fatalf("engine %v != reference %v", got, want)
	}
	// Manual expectation: rows 1..6, frames [1,1],[1,2],[3,3],[3,4],[3,5],[6,6]
	manual := []string{"1", "3", "3", "7", "12", "6"}
	if !eqStrings(got, manual) {
		t.Fatalf("got %v want %v", got, manual)
	}
}

// TestOffsetIgnoreNulls pins the IGNORE NULLS extension for lag/lead.
func TestOffsetIgnoreNulls(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3, 4, 5, 6, 7),
		"v":  nullables(10, nil, 30, nil, nil, 60, 70),
	}, []string{"id", "v"})
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT lag(v, 1) IGNORE NULLS OVER (ORDER BY id) FROM races",
			[]string{"", "10", "10", "30", "30", "30", "60"}},
		{"SELECT lead(v, 1) IGNORE NULLS OVER (ORDER BY id) FROM races",
			[]string{"30", "30", "60", "60", "60", "70", ""}},
		{"SELECT lag(v, 2, -1) IGNORE NULLS OVER (ORDER BY id) FROM races",
			[]string{"-1", "-1", "-1", "10", "10", "10", "30"}},
		// Non-null values are 10,30,60,70.  Row 5 sees 70 as the second
		// non-null ahead (skipping rows 6 and 7's single non-null each...
		// rows 4,5 have none ahead but row 6=60, row 7=70, so row 5 -> 70).
		{"SELECT lead(v, 2, -1) IGNORE NULLS OVER (ORDER BY id) FROM races",
			[]string{"60", "60", "70", "70", "70", "-1", "-1"}},
		// Without IGNORE NULLS the positional row is returned as-is.
		{"SELECT lag(v, 1) OVER (ORDER BY id) FROM races",
			[]string{"", "10", "", "30", "", "", "60"}},
	}
	for _, c := range cases {
		got := evalOne(t, tbl, c.sql)
		if !eqStrings(got, c.want) {
			t.Errorf("%s\n got %v\nwant %v", c.sql, got, c.want)
		}
	}
}

// TestSharedSort checks that windows with the same ordering share one sort and
// produce identical results to separate evaluation.
func TestSharedSort(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3, 4, 5, 6, 7, 8),
		"g":  ints(1, 1, 2, 2, 1, 1, 2, 2),
		"v":  ints(5, 3, 8, 1, 9, 2, 7, 4),
	}, []string{"id", "g", "v"})
	sql := "SELECT sum(v) OVER (PARTITION BY g ORDER BY v) AS a, rank() OVER (PARTITION BY g ORDER BY v) AS b, lag(v) OVER (PARTITION BY g ORDER BY v) AS c FROM races"
	q, _, err := window.ParseQuery(sql, tbl.Schema)
	if err != nil {
		t.Fatal(err)
	}
	out, plan, err := window.Eval(tbl, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sorts) != 1 {
		t.Fatalf("expected 1 shared sort, got %d", len(plan.Sorts))
	}
	if len(plan.Sorts[0].Items) != 3 {
		t.Fatalf("expected 3 items on the shared sort, got %v", plan.Sorts[0].Items)
	}
	// Compare against running the three windows separately.
	nIn := tbl.Schema.Len()
	sep := []string{
		"SELECT sum(v) OVER (PARTITION BY g ORDER BY v) AS a FROM races",
		"SELECT rank() OVER (PARTITION BY g ORDER BY v) AS b FROM races",
		"SELECT lag(v) OVER (PARTITION BY g ORDER BY v) AS c FROM races",
	}
	for j, s := range sep {
		want := evalOne(t, tbl, s)
		for i := range out.Rows {
			if out.Rows[i][nIn+j].Display() != want[i] {
				t.Fatalf("column %d row %d: shared=%s separate=%s",
					j, i, out.Rows[i][nIn+j].Display(), want[i])
			}
		}
	}
}

// TestFrameValidation checks error cases against PostgreSQL's rules.
func TestFrameValidation(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2),
		"v":  ints(10, 20),
		"s":  {value.Str("a"), value.Str("b")},
	}, []string{"id", "v", "s"})
	bad := []string{
		"SELECT sum(v) OVER (ROWS BETWEEN CURRENT ROW AND 1 PRECEDING) FROM races",
		"SELECT sum(v) OVER (ROWS BETWEEN UNBOUNDED FOLLOWING AND CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (ROWS BETWEEN CURRENT ROW AND UNBOUNDED PRECEDING) FROM races",
		"SELECT sum(v) OVER (GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (ORDER BY v, id RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM races",
		"SELECT sum(v) OVER (ORDER BY s RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM races",
	}
	for _, sql := range bad {
		if _, _, err := window.ParseQuery(sql, tbl.Schema); err == nil {
			// Some errors are detected at execution time instead.
			q, _, _ := window.ParseQuery(sql, tbl.Schema)
			if _, _, err := window.Eval(tbl, q); err == nil {
				t.Errorf("expected error for %s", sql)
			}
		}
	}
}

// TestExclusionSemantics pins EXCLUDE CURRENT ROW / GROUP / TIES.
func TestExclusionSemantics(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3, 4, 5, 6),
		"v":  ints(10, 20, 20, 30, 40, 50),
	}, []string{"id", "v"})
	cases := []struct {
		sql  string
		want []string
	}{
		// Expectations verified against PostgreSQL 16.
		{"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE CURRENT ROW) FROM races",
			[]string{"20", "30", "50", "60", "80", "40"}},
		{"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE GROUP) FROM races",
			[]string{"20", "10", "30", "60", "80", "40"}},
		{"SELECT sum(v) OVER (ORDER BY v ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE TIES) FROM races",
			[]string{"30", "30", "50", "90", "120", "90"}},
		// MIN/MAX with EXCLUDE must recompute the extreme.
		{"SELECT min(v) OVER (ORDER BY v ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING EXCLUDE GROUP) FROM races",
			[]string{"20", "10", "10", "20", "20", "30"}},
	}
	for _, c := range cases {
		got := evalOne(t, tbl, c.sql)
		if !eqStrings(got, c.want) {
			t.Errorf("%s\n got %v\nwant %v", c.sql, got, c.want)
		}
	}
}

// TestNumericAndFloatAvg checks PostgreSQL output types for AVG.
func TestAvgTypes(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3),
		"v":  ints(10, 20, 33),
		"x":  {value.Float(1.5), value.Float(2.5), value.Float(3.5)},
	}, []string{"id", "v", "x"})
	got := evalOne(t, tbl, "SELECT avg(v) OVER (ORDER BY id) FROM races")
	want := []string{"10", "15", "21"}
	if !eqStrings(got, want) {
		t.Errorf("avg(int): got %v want %v", got, want)
	}
	gotF := evalOne(t, tbl, "SELECT avg(x) OVER (ORDER BY id) FROM races")
	if gotF[2] != fmt.Sprintf("%.6g", 2.5) && gotF[2] != "2.5" {
		t.Errorf("avg(float): got %v", gotF)
	}
}

// TestNtileDistribution checks bucket sizes (first buckets get the extra row).
func TestNtileDistribution(t *testing.T) {
	tbl := buildTable(map[string][]value.Value{
		"id": ints(1, 2, 3, 4, 5, 6, 7),
		"v":  ints(1, 2, 3, 4, 5, 6, 7),
	}, []string{"id", "v"})
	got := evalOne(t, tbl, "SELECT ntile(3) OVER (ORDER BY v) FROM races")
	want := []string{"1", "1", "1", "2", "2", "3", "3"}
	if !eqStrings(got, want) {
		t.Errorf("ntile: got %v want %v", got, want)
	}
	got5 := evalOne(t, tbl, "SELECT ntile(5) OVER (ORDER BY v) FROM races")
	want5 := []string{"1", "1", "2", "2", "3", "4", "5"}
	if !eqStrings(got5, want5) {
		t.Errorf("ntile(5): got %v want %v", got5, want5)
	}
}
