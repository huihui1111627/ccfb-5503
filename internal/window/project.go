package window

import (
	"sort"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
)

// 输出表列布局：
//
//	若 SELECT 含 *：先放全部输入列，再放窗口虚拟列，最后放普通表达式列；
//	若无 *：窗口虚拟列在前，普通表达式列在后。
//
// 窗口虚拟列始终存在（SELECT 里裸写的窗口函数就是它的别名输出），普通
// 表达式列是 *之外的非窗口项。投影阶段把数据填进对应槽位。

// outputLayout 记录输出表的列位置约定。
//
// 可见输出列严格按 SELECT 顺序排列（* 展开为全部输入列）。窗口函数的预计算
// 结果作为「虚拟列」追加在所有可见列之后，仅用于表达式求值时回读，不直接暴露。
type outputLayout struct {
	inputCount int // 输入列数（仅 star 时物理存在）
	// visiblePhys[j] 是第 j 个 SELECT 项（去掉 star 展开后的非 star 项）的物理列。
	visiblePhys []int
	// winVirt[i] 是第 i 个窗口结果的虚拟列物理下标。
	winVirt []int
}

func buildOutputSchema(filtered *table.Table, stmt *expr.SelectStmt, winCols []*windowColumn) (*table.Table, *outputLayout) {
	lo := &outputLayout{inputCount: len(filtered.Columns)}
	out := &table.Table{Name: filtered.Name, N: filtered.N}
	star := hasSelectStar(stmt)

	// 1. 可见列：先放 * 展开的输入列。
	if star {
		for _, c := range filtered.Columns {
			out.Columns = append(out.Columns, &table.Column{Name: c.Name, Kind: c.Kind, Vals: make([]any, filtered.N)})
		}
	}
	// 2. 可见列：SELECT 里的非 star 项（窗口调用与普通表达式都建可见列）。
	for _, it := range stmt.Selects {
		if it.Star {
			continue
		}
		name := it.Alias
		if name == "" {
			name = expr.NodeString(it.Expr)
		}
		kind := types.TypeReal
		if wc, ok := it.Expr.(*expr.WindowCall); ok {
			kind = inferResultKind(findWinCol(winCols, wc))
		}
		lo.visiblePhys = append(lo.visiblePhys, len(out.Columns))
		out.AddColumn(name, kind, make([]any, filtered.N))
	}
	// 3. 虚拟窗口列：追加在末尾，每个窗口调用一个，供表达式回读。
	for _, wc := range winCols {
		lo.winVirt = append(lo.winVirt, len(out.Columns))
		out.AddColumn("_virt_"+wc.label, inferResultKind(wc), make([]any, filtered.N))
	}
	return out, lo
}

func findWinCol(winCols []*windowColumn, wc *expr.WindowCall) *windowColumn {
	for _, w := range winCols {
		if w.call == wc {
			return w
		}
	}
	return winCols[0]
}

func inferResultKind(wc *windowColumn) string {
	switch wc.call.Func {
	case "count", "row_number", "rank", "dense_rank", "ntile":
		return types.TypeInteger
	case "avg", "percent_rank", "cume_dist",
		"stddev_pop", "stddev_samp", "var_pop", "var_samp":
		return types.TypeReal
	}
	return types.TypeReal
}

func hasSelectStar(stmt *expr.SelectStmt) bool {
	for _, it := range stmt.Selects {
		if it.Star {
			return true
		}
	}
	return false
}

// dropVirtualColumns 移除投影/排序后不再需要的内部虚拟窗口列。
func dropVirtualColumns(out *table.Table, lo *outputLayout) {
	if len(lo.winVirt) == 0 {
		return
	}
	virt := map[int]bool{}
	for _, p := range lo.winVirt {
		virt[p] = true
	}
	kept := make([]*table.Column, 0, len(out.Columns)-len(lo.winVirt))
	for i, c := range out.Columns {
		if !virt[i] {
			kept = append(kept, c)
		}
	}
	out.Columns = kept
}

func indexOfCall(winCols []*windowColumn, wc *expr.WindowCall) int {
	for i, w := range winCols {
		if w.call == wc {
			return i
		}
	}
	return -1
}

// projectSelect 按 SELECT 顺序填充可见列，并把窗口预计算结果写入虚拟列。
func projectSelect(
	filtered *table.Table,
	out *table.Table,
	lo *outputLayout,
	stmt *expr.SelectStmt,
	winCols []*windowColumn,
	winCallCol map[*expr.WindowCall]int,
	colMap map[*expr.ColumnRef]int,
) error {
	n := filtered.N

	// 1. star 输入列直接拷贝。
	if hasSelectStar(stmt) {
		for c := 0; c < len(filtered.Columns); c++ {
			copy(out.Columns[c].Vals, filtered.Columns[c].Vals)
		}
	}
	// 2. 虚拟窗口列（供表达式回读）。
	for i, wc := range winCols {
		copy(out.Columns[lo.winVirt[i]].Vals, wc.result)
	}

	// 3. 可见列：与 stmt.Selects 的非 star 项一一对应。
	ctx := &expr.RowContext{
		ColMap: colMap,
		Vals:   func(col, row int) any { return filtered.Columns[col].Vals[row] },
	}
	visIdx := 0
	for _, it := range stmt.Selects {
		if it.Star {
			continue
		}
		phys := lo.visiblePhys[visIdx]
		visIdx++
		for r := 0; r < n; r++ {
			ctx.Row = r
			v, err := evalViaClone(it.Expr, ctx, out, winCallCol, r)
			if err != nil {
				return err
			}
			out.Columns[phys].Vals[r] = v
		}
	}
	out.N = n
	return nil
}

// evalViaClone 把表达式子树里的窗口调用替换为当前行的字面量，再标准求值。
func evalViaClone(
	n expr.Node,
	ctx *expr.RowContext,
	out *table.Table,
	winCallCol map[*expr.WindowCall]int,
	row int,
) (any, error) {
	clone := replaceWindows(n, out, winCallCol, row)
	return expr.Eval(clone, ctx)
}

func replaceWindows(
	n expr.Node,
	out *table.Table,
	winCallCol map[*expr.WindowCall]int,
	row int,
) expr.Node {
	switch x := n.(type) {
	case *expr.WindowCall:
		return &expr.Literal{Value: out.Columns[winCallCol[x]].Vals[row]}
	case *expr.UnaryOp:
		return &expr.UnaryOp{Op: x.Op, X: replaceWindows(x.X, out, winCallCol, row)}
	case *expr.BinaryOp:
		return &expr.BinaryOp{
			Op: x.Op,
			X:  replaceWindows(x.X, out, winCallCol, row),
			Y:  replaceWindows(x.Y, out, winCallCol, row),
		}
	case *expr.BetweenExpr:
		return &expr.BetweenExpr{
			X:      replaceWindows(x.X, out, winCallCol, row),
			Low:    replaceWindows(x.Low, out, winCallCol, row),
			High:   replaceWindows(x.High, out, winCallCol, row),
			Negate: x.Negate,
		}
	case *expr.InList:
		cp := &expr.InList{Negate: x.Negate, X: replaceWindows(x.X, out, winCallCol, row)}
		for _, e := range x.List {
			cp.List = append(cp.List, replaceWindows(e, out, winCallCol, row))
		}
		return cp
	case *expr.IsNullExpr:
		return &expr.IsNullExpr{X: replaceWindows(x.X, out, winCallCol, row), Negate: x.Negate}
	case *expr.CaseExpr:
		cp := &expr.CaseExpr{Else: replaceWindowsSafe(x.Else, out, winCallCol, row)}
		for _, w := range x.Whens {
			cp.Whens = append(cp.Whens, expr.When{
				Cond:   replaceWindows(w.Cond, out, winCallCol, row),
				Result: replaceWindows(w.Result, out, winCallCol, row),
			})
		}
		return cp
	case *expr.CastExpr:
		return &expr.CastExpr{X: replaceWindows(x.X, out, winCallCol, row), Type: x.Type}
	case *expr.FuncCall:
		cp := &expr.FuncCall{Name: x.Name, Star: x.Star, Distinct: x.Distinct}
		for _, a := range x.Args {
			cp.Args = append(cp.Args, replaceWindows(a, out, winCallCol, row))
		}
		return cp
	default:
		return n
	}
}

func replaceWindowsSafe(n expr.Node, out *table.Table, m map[*expr.WindowCall]int, row int) expr.Node {
	if n == nil {
		return nil
	}
	return replaceWindows(n, out, m, row)
}

// orderOutput 对结果表做最终排序（ORDER BY 可引用窗口结果列）。
func orderOutput(out *table.Table, stmt *expr.SelectStmt, colMap map[*expr.ColumnRef]int, winCallCol map[*expr.WindowCall]int) error {
	n := out.N
	type keyCol struct {
		vals []any
		item expr.OrderItem
	}
	var keys []keyCol
	for _, oi := range stmt.OrderBy {
		vals := make([]any, n)
		for r := 0; r < n; r++ {
			v, err := evalViaClone(oi.X, &expr.RowContext{
				ColMap: colMap,
				Row:    r,
				Vals:   func(col, row int) any { return out.Columns[col].Vals[row] },
			}, out, winCallCol, r)
			if err != nil {
				return err
			}
			vals[r] = v
		}
		keys = append(keys, keyCol{vals: vals, item: oi})
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		for _, k := range keys {
			sk := SortKey{Nulls: effectiveNulls(k.item), Desc: k.item.Desc}
			if c := compareOrderKey(k.vals[idx[a]], k.vals[idx[b]], sk); c != 0 {
				return c < 0
			}
		}
		return false
	})
	for _, c := range out.Columns {
		orig := append([]any(nil), c.Vals...)
		for newPos, oldPos := range idx {
			c.Vals[newPos] = orig[oldPos]
		}
	}
	return nil
}
