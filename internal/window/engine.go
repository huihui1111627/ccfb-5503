// Package window 是窗口函数计算引擎：接收内存表与解析好的 SELECT 语句，
// 输出带窗口计算结果的表。
//
// 执行流水线：
//
//  1. WHERE 过滤（保留原始行号，结果表按过滤后的顺序输出）
//  2. 所有分区/排序/帧偏移/函数参数表达式整列预求值
//  3. 按排序指纹缓存 SortedPerm，同规格的多个窗口只排序一次
//  4. 每个窗口按分区建树（线段树），逐行 O(log n) 求帧聚合；排名/导航函数
//     走各自的线性算法
//  5. SELECT 列表逐行求值（窗口调用节点替换为预计算列），输出结果表
package window

import (
	"fmt"
	"strings"
	"time"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
)

// PlanStage 是执行计划里的一个步骤，供页面展示。
type PlanStage struct {
	Name     string
	Detail   string
	RowsIn   int
	RowsOut  int
	Duration time.Duration
}

// Plan 记录一次查询的完整执行计划与耗时。
type Plan struct {
	Stages     []PlanStage
	Total      time.Duration
	SortReused int // 因共享排序而省下的排序次数
	Windows    []WindowPlan
}

// WindowPlan 是单个窗口函数的计划描述。
type WindowPlan struct {
	Label      string
	Func       string
	Partition  []string
	OrderBy    []string
	Frame      string
	Exclude    string
	TreeBuild  time.Duration
	FrameCalc  time.Duration
	SortCached bool
	Duration   time.Duration
}

func (p *Plan) add(s PlanStage) { p.Stages = append(p.Stages, s) }

// Engine 是无状态的引擎入口（每次 Execute 独立）。
type Engine struct{}

// New 构造引擎。
func New() *Engine { return &Engine{} }

// windowColumn 记录一个窗口调用与其预计算结果列（结果表中的物理列下标）。
type windowColumn struct {
	call   *expr.WindowCall
	result []any
	kind   string // aggregate | rank | nav
	label  string
}

// Execute 执行查询，返回结果表与计划。
func (e *Engine) Execute(in *table.Table, stmt *expr.SelectStmt) (*table.Table, *Plan, error) {
	t0 := time.Now()
	plan := &Plan{}

	// 1. 绑定（列解析、具名窗口展开、静态校验）。
	binder := expr.NewBinder(&schemaAdapter{t: in})
	calls, err := binder.BindSelect(stmt)
	if err != nil {
		return nil, nil, err
	}
	colMap := binderColumnMap(binder)

	// 2. WHERE 过滤。无 WHERE 时直接复用原表，避免无谓的全表逐行复制。
	filter, err := EvalFilter(in, stmt.Where, colMap)
	if err != nil {
		return nil, nil, err
	}
	var filtered *table.Table
	if stmt.Where == nil {
		filtered = in
	} else {
		filtered = filterRows(in, filter)
	}
	plan.add(PlanStage{Name: "WHERE 过滤", RowsIn: in.N, RowsOut: filtered.N,
		Detail: whereDetail(stmt.Where)})

	// 过滤后的表需要重新建立列绑定（列下标不变，行数变化）。
	filteredBinder := expr.NewBinder(&schemaAdapter{t: filtered})
	if err := filteredBinder.BindExprOnly(stmt.Where); stmt.Where != nil && err != nil {
		return nil, nil, err
	}
	fColMap := map[*expr.ColumnRef]int{}
	for ref := range colMap {
		fColMap[ref] = colMap[ref]
	}

	// 3. 逐个计算窗口函数。
	sortCache := map[string]*SortedPerm{}
	var winCols []*windowColumn
	for i, wc := range calls {
		wc0 := time.Now()
		label := windowLabel(wc, i)
		res, wp, err := e.computeWindow(filtered, wc, fColMap, sortCache, plan, label)
		if err != nil {
			return nil, nil, err
		}
		wp.Duration = time.Since(wc0)
		plan.Windows = append(plan.Windows, wp)
		kind := "aggregate"
		if expr.IsNonAggWindowFunc(wc.Func) {
			if isRankFunc(wc.Func) {
				kind = "rank"
			} else {
				kind = "nav"
			}
		}
		winCols = append(winCols, &windowColumn{call: wc, result: res, kind: kind, label: label})
	}

	// 4. 组装输出表：先确定输出列。
	out, layout := buildOutputSchema(filtered, stmt, winCols)

	// 5. 逐行求 SELECT 表达式。窗口节点替换为虚拟结果列读取。
	winCallCol := map[*expr.WindowCall]int{}
	for i, wc := range winCols {
		winCallCol[wc.call] = layout.winVirt[i]
	}
	evalStart := time.Now()
	if err := projectSelect(filtered, out, layout, stmt, winCols, winCallCol, fColMap); err != nil {
		return nil, nil, err
	}
	plan.add(PlanStage{Name: "SELECT 投影", RowsIn: filtered.N, RowsOut: out.N, Duration: time.Since(evalStart)})

	// 6. 输出排序与 LIMIT。
	if len(stmt.OrderBy) > 0 {
		s := time.Now()
		if err := orderOutput(out, stmt, fColMap, winCallCol); err != nil {
			return nil, nil, err
		}
		plan.add(PlanStage{Name: "输出排序 ORDER BY", RowsIn: out.N, RowsOut: out.N, Duration: time.Since(s)})
	}
	if stmt.Limit != nil && int(*stmt.Limit) < out.N {
		out = limitTable(out, int(*stmt.Limit))
		plan.add(PlanStage{Name: "LIMIT", RowsOut: out.N})
	}

	// 7. 移除内部虚拟窗口列，只保留 SELECT 声明的可见列。
	dropVirtualColumns(out, layout)

	plan.Total = time.Since(t0)
	return out, plan, nil
}

// ---- 辅助 ---------------------------------------------------------------

func whereDetail(n expr.Node) string {
	if n == nil {
		return "无过滤，全表 "
	}
	return expr.NodeString(n)
}

func windowLabel(wc *expr.WindowCall, i int) string {
	return fmt.Sprintf("w%d_%s", i+1, wc.Func)
}

func isRankFunc(fn string) bool {
	switch strings.ToLower(fn) {
	case "row_number", "rank", "dense_rank", "percent_rank", "cume_dist", "ntile":
		return true
	}
	return false
}

// schemaAdapter 让 table.Table 满足 expr.Schema。
type schemaAdapter struct{ t *table.Table }

func (s *schemaAdapter) ColumnIndex(name string) int { return s.t.ColIndex(name) }
func (s *schemaAdapter) ColumnCount() int            { return len(s.t.Columns) }
func (s *schemaAdapter) AddVirtualColumn(name string) int {
	idx := len(s.t.Columns)
	s.t.AddColumn(name, types.TypeReal, make([]any, s.t.N))
	return idx
}

func binderColumnMap(b *expr.Binder) map[*expr.ColumnRef]int {
	// Binder 内部 map 不直接暴露；通过重新遍历收集。引擎只需要绑定关系，
	// 这里让 Binder 提供访问器（见 binder_expose.go）。
	return b.ColumnMap()
}

func filterRows(in *table.Table, keep []bool) *table.Table {
	out := in.CloneEmpty(in.Name)
	for r := 0; r < in.N; r++ {
		if keep != nil && !keep[r] {
			continue
		}
		out.AppendRow(in.RowValues(r))
	}
	return out
}

func limitTable(t *table.Table, n int) *table.Table {
	out := t.CloneEmpty(t.Name)
	for r := 0; r < n; r++ {
		out.AppendRow(t.RowValues(r))
	}
	return out
}
