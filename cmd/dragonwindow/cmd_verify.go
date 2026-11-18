package main

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/expr"
	"dragonwindow/internal/querygen"
	"dragonwindow/internal/ref"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
	"dragonwindow/internal/window"
)

// verifyResult 汇总一次差分验证的结果。
type verifyResult struct {
	Queries       int
	RowsCompared  int64
	CellsCompared int64
	Mismatches    []mismatch
	EngineMs      int64
	RefMs         int64
	ReusedSorts   int
}

type mismatch struct {
	QueryIdx int
	Window   string
	Row      int
	Engine   any
	Ref      any
	SQL      string
}

func cmdVerify(args []string) error {
	f := parseFlags(args, "v")
	rows := f.intVal("rows", 20000)
	nq := f.intVal("queries", 200)
	seed := f.int64Val("seed", 20261119)
	verbose := f.flag("v")

	cfg := deriveConfig(rows, seed)
	main, _, _, _ := datagen.Generate(cfg)
	fmt.Printf("数据: %s, %d 行\n", main.SchemaText(), main.N)

	rng := rand.New(rand.NewSource(seed + 1))
	gen := querygen.New(rng)
	queries := gen.Generate(nq)

	res := runVerify(main, queries)
	printVerify(res, queries, verbose)
	if len(res.Mismatches) > 0 {
		return fmt.Errorf("发现 %d 处不一致", len(res.Mismatches))
	}
	fmt.Printf("\n✅ 全部 %d 条查询、%d 个窗口值逐行一致（引擎 %dms / 参考 %dms，共享排序复用 %d 次）\n",
		res.Queries, res.CellsCompared, res.EngineMs, res.RefMs, res.ReusedSorts)
	return nil
}

// runVerify 对每条查询分别跑引擎与参考实现并比对。
func runVerify(main *table.Table, queries []querygen.Query) verifyResult {
	res := verifyResult{Queries: len(queries)}
	eng := window.New()

	for qi, q := range queries {
		// 引擎侧。
		stmt, err := expr.Parse(q.SQL)
		if err != nil {
			res.Mismatches = append(res.Mismatches, mismatch{QueryIdx: qi, SQL: q.SQL,
				Engine: "parse-error: " + err.Error()})
			continue
		}
		t0 := time.Now()
		engOut, plan, err := eng.Execute(main, stmt)
		res.EngineMs += time.Since(t0).Milliseconds()
		if err != nil {
			res.Mismatches = append(res.Mismatches, mismatch{QueryIdx: qi, SQL: q.SQL,
				Engine: "engine-error: " + err.Error()})
			continue
		}
		res.ReusedSorts += plan.SortReused

		// 参考侧（独立 AST）。
		stmt2, perr := expr.Parse(q.SQL)
		if perr != nil {
			res.Mismatches = append(res.Mismatches, mismatch{QueryIdx: qi, SQL: q.SQL,
				Engine: "reparse-error: " + perr.Error()})
			continue
		}
		t1 := time.Now()
		ft, refRes, rerr := ref.Run(main, stmt2)
		res.RefMs += time.Since(t1).Milliseconds()
		if rerr != nil {
			res.Mismatches = append(res.Mismatches, mismatch{QueryIdx: qi, SQL: q.SQL,
				Engine: "ref-error: " + rerr.Error()})
			continue
		}

		// 找出该查询的窗口输出列（按 SELECT 顺序）。
		winLabels := windowOutputLabels(stmt)
		if len(winLabels) != len(refRes) {
			res.Mismatches = append(res.Mismatches, mismatch{QueryIdx: qi, SQL: q.SQL,
				Engine: fmt.Sprintf("window count engine=%d ref=%d", len(winLabels), len(refRes))})
			continue
		}
		for wi, label := range winLabels {
			col := engOut.ColIndex(label)
			if col < 0 {
				res.Mismatches = append(res.Mismatches, mismatch{QueryIdx: qi, Window: label, SQL: q.SQL,
					Engine: "missing output column"})
				continue
			}
			for r := 0; r < ft.N; r++ {
				got := engOut.Columns[col].Vals[r]
				want := refRes[wi][r]
				res.CellsCompared++
				if !valuesMatch(got, want) {
					mm := mismatch{QueryIdx: qi, Window: label, Row: r,
						Engine: got, Ref: want, SQL: q.SQL}
					res.Mismatches = append(res.Mismatches, mm)
					if len(res.Mismatches) >= 20 {
						return res
					}
				}
			}
		}
		res.RowsCompared += int64(ft.N)
	}
	return res
}

// windowOutputLabels 返回 SELECT 中窗口调用对应的输出列名。
func windowOutputLabels(stmt *expr.SelectStmt) []string {
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

// valuesMatch 比较引擎与参考值。数值允许极小浮点容差（仅用于跨大整数转浮点
// 的末位），其余要求规范化文本完全一致；NULL 严格相等。
func valuesMatch(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	af, aIsNum := toFloat64(a)
	bf, bIsNum := toFloat64(b)
	if aIsNum && bIsNum {
		if math.IsNaN(af) || math.IsNaN(bf) {
			return math.IsNaN(af) && math.IsNaN(bf)
		}
		// 精确优先：bit 相同直接通过。
		if types.FormatValue(a) == types.FormatValue(b) {
			return true
		}
		// 浮点相对容差 1e-12，覆盖不同聚合路径的舍入末位。
		den := math.Max(1, math.Abs(bf))
		return math.Abs(af-bf)/den < 1e-12
	}
	return types.FormatValue(a) == types.FormatValue(b)
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func printVerify(res verifyResult, queries []querygen.Query, verbose bool) {
	fmt.Printf("查询 %d 条, 比对窗口值 %d 个, 跨行 %d\n",
		res.Queries, res.CellsCompared, res.RowsCompared)
	if len(res.Mismatches) == 0 {
		return
	}
	fmt.Printf("\n❌ 发现 %d 处不一致（最多展示 20）:\n", len(res.Mismatches))
	for _, m := range res.Mismatches {
		fmt.Printf("  [查询#%d 窗口=%s 行=%d]\n", m.QueryIdx, m.Window, m.Row)
		fmt.Printf("    SQL: %s\n", m.SQL)
		fmt.Printf("    引擎=%v  参考=%v\n", m.Engine, m.Ref)
	}
}
