package expr

import (
	"fmt"
	"strings"
)

// Node 是所有 AST 节点的公共接口。
type Node interface{ nodeMarker() }

// ---- 表达式节点 -----------------------------------------------------------

// Literal 字面量：nil 表示 NULL。
type Literal struct{ Value any }

// ColumnRef 列引用，Name 是最终解析出的列名（表限定名在解析阶段折叠）。
type ColumnRef struct {
	Table string // 可空
	Name  string
}

// UnaryOp 一元运算：-、+、NOT。
type UnaryOp struct {
	Op string
	X  Node
}

// BinaryOp 二元运算。Op 为小写后的运算符/关键字（and/or 也走这里）。
type BinaryOp struct {
	Op   string
	X, Y Node
}

// BetweenExpr a [NOT] BETWEEN low AND high。
type BetweenExpr struct {
	X         Node
	Low, High Node
	Negate    bool
}

// InList x [NOT] IN (v1, v2, ...)。
type InList struct {
	X      Node
	List   []Node
	Negate bool
}

// IsNullExpr x IS [NOT] NULL。
type IsNullExpr struct {
	X      Node
	Negate bool
}

// CaseExpr CASE [w] WHEN cond THEN r ... ELSE d END（搜索式 CASE）。
// 简单 CASE（CASE x WHEN v ...）在解析期改写为搜索式：x = v。
type CaseExpr struct {
	Whens []When
	Else  Node // 可空，缺省即 NULL
}

type When struct {
	Cond, Result Node
}

// CastExpr CAST(x AS type) 与 x::type。
type CastExpr struct {
	X    Node
	Type string
}

// FuncCall 标量函数调用（COALESCE/GREATEST/...）。
type FuncCall struct {
	Name string
	Args []Node
	// DISTINCT 仅在聚合函数窗口调用里出现，解析保留、语义层报错或处理。
	Distinct bool
	// 通配符 *，仅 count(*) 合法。
	Star bool
}

// WindowCall 窗口函数调用：func(args) [FILTER (WHERE ...)] OVER (...)。
type WindowCall struct {
	Func     string
	Args     []Node
	Star     bool
	Distinct bool
	Filter   Node // 可空
	Over     *OverSpec
	// IgnoreNulls 对应 IGNORE NULLS（导航函数）；缺省为 RESPECT NULLS。
	IgnoreNulls bool
	// FromLast 对应 FROM FIRST/LAST（nth_value 等），当前引擎只支持 FROM FIRST。
	FromLast bool
}

// OverSpec OVER 子句。引用具名窗口时 RefName 非空，其余字段在 resolve 阶段
// 从 WINDOW 定义拷贝过来（PostgreSQL 允许具名窗口再追加 frame 等，这里支持
// 最常见的「整段引用」与「具名基础上补 frame」两种形式）。
type OverSpec struct {
	RefName      string
	Partition    []Node
	OrderBy      []OrderItem
	Frame        *FrameSpec
	ResolvedFrom string // 调试用：来自哪个具名窗口
	// IgnoreNulls 在 OVER 子句内部用 IGNORE NULLS 指定（导航函数）。
	IgnoreNulls bool
}

// OrderItem ORDER BY 项。
type OrderItem struct {
	X     Node
	Desc  bool
	Nulls string // "first" | "last" | ""（未指定）
}

// FrameSpec 窗口帧。
type FrameSpec struct {
	Unit    string // rows | range | groups，小写
	Start   FrameBound
	End     *FrameBound // 可空表示仅起始边界（帧到当前行 peer 为止，由语义层补全）
	Exclude string      // "" | "current row" | "group" | "ties" | "no others"
}

// FrameBound 帧边界。
type FrameBound struct {
	Kind   string // unbounded preceding | preceding | current row | following | unbounded following
	Offset Node   // 仅 preceding/following 有；ROWS/GROUPS 为整型表达式，RANGE 为数值或 interval
}

// ---- 语句节点 -------------------------------------------------------------

// SelectStmt 是本引擎支持的完整查询形态：单表 SELECT + 窗口 + 可选过滤。
type SelectStmt struct {
	Selects []SelectItem
	From    string
	Where   Node
	Window  []NamedWindow
	OrderBy []OrderItem
	Limit   *int64
}

// SelectItem SELECT 列表项，Alias 可空。
type SelectItem struct {
	Expr  Node
	Alias string
	// Star 为 true 时表示 t.* 或 *，Expr 为 nil。
	Star      bool
	StarTable string
}

// NamedWindow WINDOW 子句里的具名窗口定义。
type NamedWindow struct {
	Name string
	Spec *OverSpec
}

// 节点标记方法
func (*Literal) nodeMarker()     {}
func (*ColumnRef) nodeMarker()   {}
func (*UnaryOp) nodeMarker()     {}
func (*BinaryOp) nodeMarker()    {}
func (*BetweenExpr) nodeMarker() {}
func (*InList) nodeMarker()      {}
func (*IsNullExpr) nodeMarker()  {}
func (*CaseExpr) nodeMarker()    {}
func (*CastExpr) nodeMarker()    {}
func (*FuncCall) nodeMarker()    {}
func (*WindowCall) nodeMarker()  {}
func (*SelectStmt) nodeMarker()  {}

// String 系列：把 AST 渲染回规范化 SQL 文本，用于页面展示与计划输出。

func (o *OrderItem) String() string {
	var b strings.Builder
	b.WriteString(nodeSQL(o.X))
	if o.Desc {
		b.WriteString(" DESC")
	} else {
		b.WriteString(" ASC")
	}
	if o.Nulls != "" {
		b.WriteString(" NULLS " + strings.ToUpper(o.Nulls))
	}
	return b.String()
}

func (f *FrameSpec) String() string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(f.Unit))
	b.WriteString(" BETWEEN ")
	b.WriteString(boundSQL(&f.Start))
	if f.End != nil {
		b.WriteString(" AND ")
		b.WriteString(boundSQL(f.End))
	} else {
		// 单边界简写
	}
	if f.Exclude != "" {
		b.WriteString(" EXCLUDE " + excludeSQL(f.Exclude))
	}
	return b.String()
}

func boundSQL(b *FrameBound) string {
	switch b.Kind {
	case "unbounded preceding":
		return "UNBOUNDED PRECEDING"
	case "preceding":
		return nodeSQL(b.Offset) + " PRECEDING"
	case "current row":
		return "CURRENT ROW"
	case "following":
		return nodeSQL(b.Offset) + " FOLLOWING"
	case "unbounded following":
		return "UNBOUNDED FOLLOWING"
	}
	return "?"
}

func excludeSQL(e string) string {
	switch e {
	case "current row":
		return "CURRENT ROW"
	case "group":
		return "GROUP"
	case "ties":
		return "TIES"
	case "no others":
		return "NO OTHERS"
	}
	return strings.ToUpper(e)
}

func nodeSQL(n Node) string {
	switch x := n.(type) {
	case *Literal:
		return litSQL(x.Value)
	case *ColumnRef:
		if x.Table != "" {
			return x.Table + "." + x.Name
		}
		return x.Name
	case *UnaryOp:
		return x.Op + "(" + nodeSQL(x.X) + ")"
	case *BinaryOp:
		return "(" + nodeSQL(x.X) + " " + x.Op + " " + nodeSQL(x.Y) + ")"
	case *BetweenExpr:
		s := nodeSQL(x.X)
		if x.Negate {
			s += " NOT"
		}
		return s + " BETWEEN " + nodeSQL(x.Low) + " AND " + nodeSQL(x.High)
	case *InList:
		parts := make([]string, len(x.List))
		for i, e := range x.List {
			parts[i] = nodeSQL(e)
		}
		s := nodeSQL(x.X) + " IN (" + strings.Join(parts, ", ") + ")"
		if x.Negate {
			return "NOT " + s
		}
		return s
	case *IsNullExpr:
		if x.Negate {
			return nodeSQL(x.X) + " IS NOT NULL"
		}
		return nodeSQL(x.X) + " IS NULL"
	case *CaseExpr:
		var b strings.Builder
		b.WriteString("CASE ")
		for _, w := range x.Whens {
			b.WriteString("WHEN " + nodeSQL(w.Cond) + " THEN " + nodeSQL(w.Result) + " ")
		}
		if x.Else != nil {
			b.WriteString("ELSE " + nodeSQL(x.Else) + " ")
		}
		b.WriteString("END")
		return b.String()
	case *CastExpr:
		return "CAST(" + nodeSQL(x.X) + " AS " + x.Type + ")"
	case *FuncCall:
		args := make([]string, len(x.Args))
		for i, a := range x.Args {
			args[i] = nodeSQL(a)
		}
		return x.Name + "(" + strings.Join(args, ", ") + ")"
	case *WindowCall:
		return windowCallSQL(x)
	}
	return "?"
}

func windowCallSQL(w *WindowCall) string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(w.Func))
	b.WriteString("(")
	if w.Star {
		b.WriteString("*")
	} else {
		args := make([]string, len(w.Args))
		for i, a := range w.Args {
			args[i] = nodeSQL(a)
		}
		b.WriteString(strings.Join(args, ", "))
	}
	b.WriteString(")")
	if w.Filter != nil {
		b.WriteString(" FILTER (WHERE " + nodeSQL(w.Filter) + ")")
	}
	b.WriteString(" OVER (")
	var parts []string
	if len(w.Over.Partition) > 0 {
		ps := make([]string, len(w.Over.Partition))
		for i, p := range w.Over.Partition {
			ps[i] = nodeSQL(p)
		}
		parts = append(parts, "PARTITION BY "+strings.Join(ps, ", "))
	}
	if len(w.Over.OrderBy) > 0 {
		os := make([]string, len(w.Over.OrderBy))
		for i, o := range w.Over.OrderBy {
			os[i] = o.String()
		}
		parts = append(parts, "ORDER BY "+strings.Join(os, ", "))
	}
	if w.Over.Frame != nil {
		parts = append(parts, w.Over.Frame.String())
	}
	b.WriteString(strings.Join(parts, " "))
	b.WriteString(")")
	return b.String()
}

func litSQL(v any) string {
	if v == nil {
		return "NULL"
	}
	switch x := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

// NodeString 返回表达式 AST 的规范化 SQL 文本（用于排序指纹与计划展示）。
func NodeString(n Node) string {
	if n == nil {
		return ""
	}
	return nodeSQL(n)
}

// FrameString 返回帧规格的规范化 SQL 文本。
func FrameString(f *FrameSpec) string {
	if f == nil {
		return ""
	}
	return f.String()
}
