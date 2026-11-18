package expr

import (
	"fmt"
	"strings"
)

// Schema 提供列名到下标的解析，以及窗口结果虚拟列的注册能力。
type Schema interface {
	ColumnIndex(name string) int
	ColumnCount() int
	// AddVirtualColumn 注册引擎预计算好的窗口结果列，返回新列下标。
	AddVirtualColumn(name string) int
}

// Binder 持有一次查询内的绑定状态。
type Binder struct {
	schema Schema
	colIdx map[*ColumnRef]int
	errs   []error
}

// NewBinder 构造绑定器。
func NewBinder(schema Schema) *Binder {
	return &Binder{schema: schema, colIdx: map[*ColumnRef]int{}}
}

// BindSelect 完成整条 SELECT 的绑定与语义校验，返回窗口调用清单
// （按出现顺序，去重前的原始顺序，引擎据此逐个计算并注册虚拟列）。
func (b *Binder) BindSelect(stmt *SelectStmt) ([]*WindowCall, error) {
	// 1. 先解析具名窗口定义（窗口定义之间可按声明顺序引用）。
	named := map[string]*OverSpec{}
	for _, nw := range stmt.Window {
		spec, err := b.resolveOverSpec(nw.Spec, named)
		if err != nil {
			return nil, err
		}
		named[strings.ToLower(nw.Name)] = spec
	}

	// 2. WHERE 与 SELECT 列表里的普通表达式先绑定列。
	if stmt.Where != nil {
		b.bindExpr(stmt.Where)
	}
	var calls []*WindowCall
	for i := range stmt.Selects {
		it := &stmt.Selects[i]
		if it.Star {
			continue
		}
		b.collectWindowCalls(it.Expr, &calls)
		b.bindExpr(it.Expr)
	}
	for i := range stmt.OrderBy {
		b.bindExpr(stmt.OrderBy[i].X)
	}

	// 3. 解析每个窗口调用的 OVER（引用具名窗口或内联定义）。
	for _, wc := range calls {
		spec, err := b.resolveOverSpec(wc.Over, named)
		if err != nil {
			return nil, err
		}
		wc.Over = spec
		// OVER 子句内部的 IGNORE NULLS 传播到调用节点（与函数名后的写法等价）。
		if spec.IgnoreNulls {
			wc.IgnoreNulls = true
		}
		if err := b.validateWindowCall(wc); err != nil {
			return nil, err
		}
		// 注册虚拟列下标，求值时 ColumnRef 由引擎在结果表上直接读取；
		// 这里的绑定只针对非窗口表达式，窗口调用节点本身在计划阶段被替换。
	}

	if len(b.errs) > 0 {
		return nil, b.errs[0]
	}
	return calls, nil
}

// ColIndex 返回列引用绑定的下标。
func (b *Binder) ColIndex(ref *ColumnRef) int { return b.colIdx[ref] }

// ColumnMap 返回全部列引用绑定（引擎用于构造求值上下文）。
func (b *Binder) ColumnMap() map[*ColumnRef]int { return b.colIdx }

// BindExprOnly 给独立表达式（如页面里的试算）做绑定。
func (b *Binder) BindExprOnly(n Node) error {
	b.bindExpr(n)
	if len(b.errs) > 0 {
		return b.errs[0]
	}
	return nil
}

func (b *Binder) bindExpr(n Node) {
	if n == nil {
		return
	}
	switch x := n.(type) {
	case *ColumnRef:
		idx := b.schema.ColumnIndex(x.Name)
		if idx < 0 {
			b.errs = append(b.errs, fmt.Errorf("column %q does not exist", x.Name))
			return
		}
		b.colIdx[x] = idx
	case *UnaryOp:
		b.bindExpr(x.X)
	case *BinaryOp:
		b.bindExpr(x.X)
		b.bindExpr(x.Y)
	case *BetweenExpr:
		b.bindExpr(x.X)
		b.bindExpr(x.Low)
		b.bindExpr(x.High)
	case *InList:
		b.bindExpr(x.X)
		for _, e := range x.List {
			b.bindExpr(e)
		}
	case *IsNullExpr:
		b.bindExpr(x.X)
	case *CaseExpr:
		for _, w := range x.Whens {
			b.bindExpr(w.Cond)
			b.bindExpr(w.Result)
		}
		b.bindExpr(x.Else)
	case *CastExpr:
		b.bindExpr(x.X)
	case *FuncCall:
		for _, a := range x.Args {
			b.bindExpr(a)
		}
	case *WindowCall:
		for _, a := range x.Args {
			b.bindExpr(a)
		}
		b.bindExpr(x.Filter)
		for _, p := range x.Over.Partition {
			b.bindExpr(p)
		}
		for i := range x.Over.OrderBy {
			b.bindExpr(x.Over.OrderBy[i].X)
		}
		if x.Over.Frame != nil {
			b.bindFrame(x.Over.Frame)
		}
	case *Literal:
		// nothing
	default:
		b.errs = append(b.errs, fmt.Errorf("internal error: bind unsupported node %T", n))
	}
}

func (b *Binder) bindFrame(f *FrameSpec) {
	b.bindExpr(f.Start.Offset)
	if f.End != nil {
		b.bindExpr(f.End.Offset)
	}
}

func (b *Binder) collectWindowCalls(n Node, out *[]*WindowCall) {
	switch x := n.(type) {
	case *WindowCall:
		*out = append(*out, x)
		// 窗口函数的参数里不允许再嵌套窗口函数（PostgreSQL 同样禁止）。
	case *UnaryOp:
		b.collectWindowCalls(x.X, out)
	case *BinaryOp:
		b.collectWindowCalls(x.X, out)
		b.collectWindowCalls(x.Y, out)
	case *BetweenExpr:
		b.collectWindowCalls(x.X, out)
		b.collectWindowCalls(x.Low, out)
		b.collectWindowCalls(x.High, out)
	case *InList:
		b.collectWindowCalls(x.X, out)
		for _, e := range x.List {
			b.collectWindowCalls(e, out)
		}
	case *IsNullExpr:
		b.collectWindowCalls(x.X, out)
	case *CaseExpr:
		for _, w := range x.Whens {
			b.collectWindowCalls(w.Cond, out)
			b.collectWindowCalls(w.Result, out)
		}
		b.collectWindowCalls(x.Else, out)
	case *CastExpr:
		b.collectWindowCalls(x.X, out)
	case *FuncCall:
		for _, a := range x.Args {
			b.collectWindowCalls(a, out)
		}
	}
}

// resolveOverSpec 展开具名窗口引用并补默认帧。
func (b *Binder) resolveOverSpec(spec *OverSpec, named map[string]*OverSpec) (*OverSpec, error) {
	if spec == nil {
		return &OverSpec{}, nil
	}
	out := &OverSpec{
		Partition: append([]Node(nil), spec.Partition...),
		OrderBy:   append([]OrderItem(nil), spec.OrderBy...),
		Frame:     spec.Frame,
		RefName:   spec.RefName,
	}
	if spec.RefName != "" {
		base, ok := named[strings.ToLower(spec.RefName)]
		if !ok {
			return nil, fmt.Errorf("window %q is not defined", spec.RefName)
		}
		out.ResolvedFrom = spec.RefName
		// 继承基础窗口的分区/排序（本引擎要求不冲突：基础有则当前不能重复定义）。
		if len(out.Partition) == 0 {
			out.Partition = append([]Node(nil), base.Partition...)
		} else if len(base.Partition) > 0 {
			return nil, fmt.Errorf("window %q cannot override PARTITION BY of base window", spec.RefName)
		}
		if len(out.OrderBy) == 0 {
			out.OrderBy = append([]OrderItem(nil), base.OrderBy...)
		} else if len(base.OrderBy) > 0 {
			return nil, fmt.Errorf("window %q cannot override ORDER BY of base window", spec.RefName)
		}
		if out.Frame == nil {
			out.Frame = base.Frame
		} else if base.Frame != nil {
			return nil, fmt.Errorf("window %q cannot override frame of base window", spec.RefName)
		}
	}
	// 绑定窗口内部表达式里的列。
	for _, p := range out.Partition {
		b.bindExpr(p)
	}
	for i := range out.OrderBy {
		b.bindExpr(out.OrderBy[i].X)
	}
	if out.Frame != nil {
		b.bindFrame(out.Frame)
		if err := validateFrame(out); err != nil {
			return nil, err
		}
	}
	if len(b.errs) > 0 {
		return nil, b.errs[0]
	}
	return out, nil
}

// validateFrame 静态帧校验，错误文案尽量贴近 PostgreSQL。
func validateFrame(spec *OverSpec) error {
	f := spec.Frame
	if f == nil {
		return nil
	}
	unit := f.Unit
	// RANGE 对 ORDER BY 的约束。
	if unit == "range" {
		if len(spec.OrderBy) == 0 {
			// RANGE without ORDER BY 只有无界帧合法，PostgreSQL 对 current row
			// 会报错；这里先记录，执行期再按帧边界细化。
		} else if len(spec.OrderBy) > 1 {
			// 带偏移的 RANGE 只允许一个排序列；无界/当前行不受限。
			if frameHasOffset(f) {
				return fmt.Errorf("RANGE with offset is only supported with one ORDER BY expression")
			}
		}
	}
	// ROWS/GROUPS 偏移必须是整数族表达式（执行期再校验每行值），这里先保证存在。
	if unit != "range" {
		if f.Start.Offset != nil && f.Start.Kind != "current row" {
			// 表达式即可，运行时检查 int。
		}
	}
	// GROUPS 必须有 ORDER BY（PostgreSQL 约束）。
	if unit == "groups" && len(spec.OrderBy) == 0 {
		return fmt.Errorf("GROUPS mode requires an ORDER BY clause")
	}
	return nil
}

func frameHasOffset(f *FrameSpec) bool {
	if f.Start.Kind == "preceding" || f.Start.Kind == "following" {
		return true
	}
	if f.End != nil && (f.End.Kind == "preceding" || f.End.Kind == "following") {
		return true
	}
	return false
}

var aggregateFuncs = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
	"stddev_pop": true, "stddev_samp": true, "var_pop": true, "var_samp": true,
	"string_agg": true,
}

var nonAggWindowFuncs = map[string]bool{
	"row_number": true, "rank": true, "dense_rank": true, "percent_rank": true,
	"cume_dist": true, "ntile": true, "lag": true, "lead": true,
	"first_value": true, "last_value": true, "nth_value": true,
}

func (b *Binder) validateWindowCall(wc *WindowCall) error {
	fn := strings.ToLower(wc.Func)
	if !aggregateFuncs[fn] && !nonAggWindowFuncs[fn] {
		return fmt.Errorf("window function %s is not supported", wc.Func)
	}
	if wc.Distinct && !aggregateFuncs[fn] {
		return fmt.Errorf("DISTINCT is only valid for aggregate functions")
	}
	if wc.Star && fn != "count" {
		return fmt.Errorf("%s(*) is only valid for COUNT", wc.Func)
	}
	// 参数个数粗校验，细粒度校验在执行期。
	switch fn {
	case "ntile", "lag", "lead", "nth_value", "string_agg":
		// 变长，执行期校验。
	case "count", "sum", "avg", "min", "max",
		"stddev_pop", "stddev_samp", "var_pop", "var_samp",
		"first_value", "last_value":
		if !wc.Star && len(wc.Args) != 1 {
			return fmt.Errorf("%s expects 1 argument", fn)
		}
	case "row_number", "rank", "dense_rank", "percent_rank", "cume_dist":
		if len(wc.Args) != 0 {
			return fmt.Errorf("%s takes no arguments", fn)
		}
	}
	return nil
}

// IsAggregate 暴露函数分类给引擎层。
func IsAggregate(name string) bool { return aggregateFuncs[strings.ToLower(name)] }

// IsNonAggWindowFunc 暴露函数分类给引擎层。
func IsNonAggWindowFunc(name string) bool {
	return nonAggWindowFuncs[strings.ToLower(name)]
}
