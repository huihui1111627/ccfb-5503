package window_test

import (
	"strings"
	"testing"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/expr"
	"dragonwindow/internal/ref"
	"dragonwindow/internal/window"
)

// TestAllFeaturesAgainstRef 把需求清单的特性集中到少量查询里，引擎与暴力参考
// 逐值比对，作为 8 项能力的端到端验收。
func TestAllFeaturesAgainstRef(t *testing.T) {
	cfg := datagen.DefaultConfig()
	cfg.Races, cfg.Teams, cfg.Splits, cfg.Seed = 4, 8, 10, 20261119
	main, _, _, _ := datagen.Generate(cfg)

	queries := []string{
		// 需求1：ROWS/RANGE/GROUPS 三单位 × 无界/当前行/偏移边界
		`SELECT team_id,
		  count(*) OVER (PARTITION BY team_id ORDER BY split_no ROWS BETWEEN UNBOUNDED PRECEDING AND 1 FOLLOWING) f_rows,
		  sum(split_sec) OVER (PARTITION BY team_id ORDER BY split_no RANGE BETWEEN 2 PRECEDING AND CURRENT ROW) f_range,
		  avg(split_sec) OVER (PARTITION BY team_id ORDER BY split_no GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) f_groups
		FROM race_split`,
		// 需求2：四种 EXCLUDE
		`SELECT team_id,
		  max(split_sec) OVER (PARTITION BY team_id ORDER BY split_no ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING EXCLUDE CURRENT ROW) ex_cur,
		  max(split_sec) OVER (PARTITION BY team_id ORDER BY split_no ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE GROUP) ex_grp,
		  max(split_sec) OVER (PARTITION BY team_id ORDER BY split_no ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE TIES) ex_ties,
		  max(split_sec) OVER (PARTITION BY team_id ORDER BY split_no ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE NO OTHERS) ex_no
		FROM race_split`,
		// 需求4：LAG/LEAD + IGNORE NULLS + 偏移 + 越界默认值
		`SELECT team_id,
		  lag(stroke_rate, 2, -1) IGNORE NULLS OVER (PARTITION BY team_id ORDER BY split_no) lg,
		  lead(stroke_rate, 1, -1) OVER (PARTITION BY team_id ORDER BY split_no) ld,
		  nth_value(split_sec, 2) OVER (PARTITION BY team_id ORDER BY split_no ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) nth
		FROM race_split`,
		// 需求6：动态偏移（每行各自计算的表达式）
		`SELECT team_id,
		  sum(split_sec) OVER (PARTITION BY team_id ORDER BY split_no ROWS BETWEEN (1 + split_no % 2) PRECEDING AND CURRENT ROW) dyn
		FROM race_split`,
		// 需求7：RANGE 时间间隔 + NULLS FIRST/LAST
		`SELECT team_id,
		  first_value(split_sec) OVER (ORDER BY split_time ASC NULLS FIRST RANGE BETWEEN INTERVAL '60 seconds' PRECEDING AND CURRENT ROW) iv_first,
		  last_value(split_sec) OVER (ORDER BY split_time DESC NULLS LAST RANGE BETWEEN CURRENT ROW AND INTERVAL '30 seconds' FOLLOWING) iv_last
		FROM race_split`,
	}

	eng := window.New()
	for qi, sql := range queries {
		stmt, err := expr.Parse(sql)
		if err != nil {
			t.Fatalf("query %d parse: %v", qi, err)
		}
		out, _, err := eng.Execute(main, stmt)
		if err != nil {
			t.Fatalf("query %d engine: %v", qi, err)
		}
		stmt2, _ := expr.Parse(sql)
		_, refRes, err := ref.Run(main, stmt2)
		if err != nil {
			t.Fatalf("query %d ref: %v", qi, err)
		}
		labels := windowOutputLabels(stmt)
		if len(labels) != len(refRes) {
			t.Fatalf("query %d window count %d vs ref %d", qi, len(labels), len(refRes))
		}
		for wi, label := range labels {
			col := out.ColIndex(label)
			if col < 0 {
				t.Fatalf("query %d missing column %s", qi, label)
			}
			for r := 0; r < main.N; r++ {
				if !valueEqual(out.Columns[col].Vals[r], refRes[wi][r]) {
					t.Errorf("query %d %s row %d: engine=%v ref=%v\nSQL=%s",
						qi, label, r, out.Columns[col].Vals[r], refRes[wi][r], strings.ReplaceAll(sql, "\n", " "))
				}
			}
		}
	}
}

func windowOutputLabels(stmt *expr.SelectStmt) []string {
	var out []string
	for _, it := range stmt.Selects {
		if it.Star {
			continue
		}
		if _, ok := it.Expr.(*expr.WindowCall); ok {
			name := it.Alias
			if name == "" {
				name = expr.NodeString(it.Expr)
			}
			out = append(out, name)
		}
	}
	return out
}
