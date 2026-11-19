// Package table provides the in-memory table model used as the input and
// output of the window engine.
package table

import (
	"fmt"

	"dragonboat/windowengine/internal/value"
)

// Column describes one column of a table.
type Column struct {
	Name string
	Kind value.Kind
}

// Schema is an ordered list of columns with a name index.
type Schema struct {
	Columns []Column
	idx     map[string]int
}

// NewSchema builds a schema.  Column names are unique and case sensitive.
func NewSchema(cols ...Column) *Schema {
	s := &Schema{Columns: append([]Column(nil), cols...), idx: make(map[string]int, len(cols))}
	for i, c := range cols {
		if _, dup := s.idx[c.Name]; dup {
			panic("duplicate column " + c.Name)
		}
		s.idx[c.Name] = i
	}
	return s
}

// Index returns the ordinal of a column.
func (s *Schema) Index(name string) (int, bool) {
	i, ok := s.idx[name]
	return i, ok
}

// Kind returns the declared kind of a column.
func (s *Schema) Kind(name string) (value.Kind, bool) {
	i, ok := s.idx[name]
	if !ok {
		return value.KindNull, false
	}
	return s.Columns[i].Kind, true
}

// Len is the number of columns.
func (s *Schema) Len() int { return len(s.Columns) }

// Row is one record.  len(Row) == schema.Len().
type Row []value.Value

// Table is a schema plus rows.
type Table struct {
	Schema *Schema
	Rows   []Row
}

// New creates an empty table with the given schema.
func New(s *Schema) *Table { return &Table{Schema: s} }

// CloneRow returns a copy of r.
func CloneRow(r Row) Row { return append(Row(nil), r...) }

// Copy returns a deep copy of the table.
func (t *Table) Copy() *Table {
	out := &Table{Schema: t.Schema, Rows: make([]Row, len(t.Rows))}
	for i, r := range t.Rows {
		out.Rows[i] = CloneRow(r)
	}
	return out
}

// Project builds a new table by selecting columns by index.
func (t *Table) Project(indexes []int, names []string) *Table {
	cols := make([]Column, len(indexes))
	for i, idx := range indexes {
		name := t.Schema.Columns[idx].Name
		if names != nil && names[i] != "" {
			name = names[i]
		}
		cols[i] = Column{Name: name, Kind: t.Schema.Columns[idx].Kind}
	}
	out := New(NewSchema(cols...))
	out.Rows = make([]Row, len(t.Rows))
	for i, r := range t.Rows {
		nr := make(Row, len(indexes))
		for j, idx := range indexes {
			nr[j] = r[idx]
		}
		out.Rows[i] = nr
	}
	return out
}

// String renders a small preview, mainly for debugging.
func (t *Table) String() string {
	out := ""
	for _, c := range t.Schema.Columns {
		out += fmt.Sprintf("%s(%s)\t", c.Name, c.Kind)
	}
	out += "\n"
	for i, r := range t.Rows {
		if i >= 10 {
			out += fmt.Sprintf("... %d more rows\n", len(t.Rows)-10)
			break
		}
		for _, v := range r {
			out += v.Display() + "\t"
		}
		out += "\n"
	}
	return out
}
