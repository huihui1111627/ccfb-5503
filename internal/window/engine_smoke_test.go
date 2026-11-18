package window_test

import (
	"testing"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/ref"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
	"dragonwindow/internal/window"
)

func sampleTable() *table.Table {
	t := table.New("race_split", []struct{ Name, Kind string }{
		{"team_id", types.TypeInteger},
		{"race_id", types.TypeInteger},
		{"split_no", types.TypeInteger},
		{"split_sec", types.TypeInteger},
		{"stroke_rate", types.TypeReal},
		{"note", types.TypeText},
	})
	rows := [][]any{
		{int64(1), int64(10), int64(1), int64(92), 72.5, "a"},
		{int64(1), int64(10), int64(2), int64(88), 74.0, "b"},
		{int64(1), int64(10), int64(3), int64(95), nil, "c"},
		{int64(2), int64(10), int64(1), int64(90), 73.2, "d"},
		{int64(2), int64(10), int64(2), int64(87), 75.1, "e"},
		{int64(2), int64(10), int64(3), int64(99), 71.8, nil},
	}
	for _, r := range rows {
		t.AppendRow(r)
	}
	return t
}

// assertAgainstRef 跑引擎与参考实现，并对每个窗口调用逐行比对（按过滤后原始行号）。
func assertAgainstRef(t *testing.T, sql string) {
	t.Helper()
	in := sampleTable()

	stmt, err := expr.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	engOut, _, err := window.New().Execute(in, stmt)
	if err != nil {
		t.Fatalf("engine execute: %v", err)
	}

	stmt2, _ := expr.Parse(sql)
	ft, refRes, err := ref.Run(in, stmt2)
	if err != nil {
		t.Fatalf("ref run: %v", err)
	}

	// 收集引擎侧每个窗口调用的结果（通过重新投影到原始行序）。
	// 引擎输出在无最终 ORDER BY 时行序 = 过滤后行序，直接按列取窗口列。
	winCalls := collectWindowCalls(t, sql)
	if len(winCalls) != len(refRes) {
		t.Fatalf("window call count mismatch: engine %d ref %d", len(winCalls), len(refRes))
	}
	for wi, label := range winCalls {
		col := engOut.ColIndex(label)
		if col < 0 {
			t.Fatalf("engine output missing window column %q; have %v", label, engOut.Columns)
		}
		for r := 0; r < ft.N; r++ {
			got := engOut.Columns[col].Vals[r]
			want := refRes[wi][r]
			if !valueEqual(got, want) {
				t.Errorf("%s row %d: engine=%v (%T) ref=%v (%T)", label, r, got, got, want, want)
			}
		}
	}
}

// collectWindowCalls 返回 SQL 中窗口调用对应的输出列名（与引擎命名/别名对齐）。
func collectWindowCalls(t *testing.T, sql string) []string {
	t.Helper()
	stmt, err := expr.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, it := range stmt.Selects {
		if it.Star {
			continue
		}
		if _, ok := it.Expr.(*expr.WindowCall); ok {
			name := it.Alias
			if name == "" {
				name = expr.NodeString(it.Expr)
			}
			labels = append(labels, name)
		}
	}
	return labels
}

func valueEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return types.FormatValue(a) == types.FormatValue(b)
}

func TestSmokeRanking(t *testing.T) {
	assertAgainstRef(t, `SELECT team_id, split_no,
		row_number() OVER (PARTITION BY team_id ORDER BY split_sec) AS rn,
		rank() OVER (PARTITION BY team_id ORDER BY split_sec) AS rk,
		dense_rank() OVER (PARTITION BY team_id ORDER BY split_sec) AS dr
		FROM race_split`)
}

func TestSmokeRunningSum(t *testing.T) {
	assertAgainstRef(t, `SELECT team_id, split_sec,
		sum(split_sec) OVER (PARTITION BY team_id ORDER BY split_no
			ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS run_sum
		FROM race_split`)
}

func TestSmokeLagLead(t *testing.T) {
	assertAgainstRef(t, `SELECT team_id, split_no, split_sec,
		lag(split_sec, 1, 0) OVER (PARTITION BY team_id ORDER BY split_no) AS prev_sec,
		lead(split_sec, 1) OVER (PARTITION BY team_id ORDER BY split_no) AS next_sec
		FROM race_split`)
}

func TestSmokeAggregateFrames(t *testing.T) {
	assertAgainstRef(t, `SELECT team_id, split_no, split_sec,
		avg(split_sec) OVER (PARTITION BY team_id ORDER BY split_no
			ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) AS mov_avg,
		max(split_sec) OVER (PARTITION BY team_id) AS pmax,
		count(*) OVER (PARTITION BY team_id) AS pcnt
		FROM race_split`)
}
