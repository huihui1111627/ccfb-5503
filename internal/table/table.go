// Package table 定义输入/输出表的内存表示。
//
// 采用列式存储：每一列一个 []any，行在列内对齐。
// 窗口计算期间会大量按列读取排序键，列式布局对缓存更友好。
package table

import (
	"fmt"
	"time"

	"dragonwindow/internal/types"
)

// Column 是一列的元数据与数据。
type Column struct {
	Name string
	// 声明类型，仅用于展示与跨库对照时建表；实际值仍以动态类型存放。
	Kind string
	Vals []any
}

// Table 是一张内存表。
type Table struct {
	Name    string
	Columns []*Column
	N       int
}

// New 按列定义建空表。
func New(name string, cols []struct{ Name, Kind string }) *Table {
	t := &Table{Name: name}
	for _, c := range cols {
		t.Columns = append(t.Columns, &Column{Name: c.Name, Kind: c.Kind})
	}
	return t
}

// ColIndex 返回列名下标，找不到时返回 -1。
func (t *Table) ColIndex(name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// MustCol 找不到列直接 panic（用于解析后已校验的内部路径）。
func (t *Table) MustCol(name string) int {
	i := t.ColIndex(name)
	if i < 0 {
		panic(fmt.Sprintf("internal error: column %q not found", name))
	}
	return i
}

// Get 取一行的一列值。
func (t *Table) Get(row int, col int) any { return t.Columns[col].Vals[row] }

// AppendRow 追加一行，按列顺序给值。
func (t *Table) AppendRow(vals []any) {
	if len(vals) != len(t.Columns) {
		panic(fmt.Sprintf("AppendRow: got %d values, want %d columns", len(vals), len(t.Columns)))
	}
	for i, v := range vals {
		t.Columns[i].Vals = append(t.Columns[i].Vals, v)
	}
	t.N++
}

// AddColumn 追加一整列（结果列）。vals 为 nil 时分配全 NULL 列。
func (t *Table) AddColumn(name, kind string, vals []any) *Column {
	if vals == nil {
		vals = make([]any, t.N)
	}
	if len(vals) != t.N {
		panic("AddColumn: column length mismatch")
	}
	c := &Column{Name: name, Kind: kind, Vals: vals}
	t.Columns = append(t.Columns, c)
	return c
}

// CloneEmpty 返回同结构空表。
func (t *Table) CloneEmpty(name string) *Table {
	nt := &Table{Name: name}
	for _, c := range t.Columns {
		nt.Columns = append(nt.Columns, &Column{Name: c.Name, Kind: c.Kind})
	}
	return nt
}

// RowValues 取一整行。
func (t *Table) RowValues(row int) []any {
	out := make([]any, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Vals[row]
	}
	return out
}

// SchemaText 输出人类可读的 schema。
func (t *Table) SchemaText() string {
	s := t.Name + "("
	for i, c := range t.Columns {
		if i > 0 {
			s += ", "
		}
		s += c.Name + " " + c.Kind
	}
	return s + ")"
}

// 便捷构造器，供数据生成器使用 ------------------------------------------------

func IntCol(name string, vals []int64) *Column {
	vs := make([]any, len(vals))
	for i, v := range vals {
		vs[i] = v
	}
	return &Column{Name: name, Kind: types.TypeInteger, Vals: vs}
}

func NullableIntCol(name string, vals []any) *Column {
	return &Column{Name: name, Kind: types.TypeInteger, Vals: vals}
}

func FloatCol(name string, vals []float64) *Column {
	vs := make([]any, len(vals))
	for i, v := range vals {
		vs[i] = v
	}
	return &Column{Name: name, Kind: types.TypeReal, Vals: vs}
}

func TextCol(name string, vals []string) *Column {
	vs := make([]any, len(vals))
	for i, v := range vals {
		vs[i] = v
	}
	return &Column{Name: name, Kind: types.TypeText, Vals: vs}
}

func BoolCol(name string, vals []bool) *Column {
	vs := make([]any, len(vals))
	for i, v := range vals {
		vs[i] = v
	}
	return &Column{Name: name, Kind: types.TypeBoolean, Vals: vs}
}

func TimeCol(name string, vals []time.Time) *Column {
	vs := make([]any, len(vals))
	for i, v := range vals {
		vs[i] = v.UTC()
	}
	return &Column{Name: name, Kind: types.TypeTimestamp, Vals: vs}
}

// FromCols 由列组装表并推断行数。
func FromCols(name string, cols ...*Column) *Table {
	t := &Table{Name: name, Columns: cols}
	if len(cols) > 0 {
		t.N = len(cols[0].Vals)
		for _, c := range cols[1:] {
			if len(c.Vals) != t.N {
				panic("FromCols: mismatched column lengths")
			}
		}
	}
	return t
}
