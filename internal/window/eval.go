package window

import (
	"fmt"
	"strings"

	"dragonboat/windowengine/internal/expr"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// MaxRows bounds the number of rows the engine accepts in one query.
const MaxRows = 5_000_000

// Plan describes how the engine will execute a query: which windows share a
// sort, which items share an aggregate tree, and the function/strategy used
// for each output column.
type Plan struct {
	Sorts    []PlanSort
	Items    []PlanItem
	Warnings []string
}

type PlanSort struct {
	ID        string   `json:"id"`
	Partition []string `json:"partition"`
	Order     []string `json:"order"`
	Items     []string `json:"items"`
}

type PlanItem struct {
	OutName   string `json:"outName"`
	Func      string `json:"func"`
	Strategy  string `json:"strategy"`
	Window    string `json:"window"`
	Frame     string `json:"frame"`
	Exclusion string `json:"exclusion"`
	SortID    string `json:"sortId"`
}

// Eval executes the query against t and returns a new table containing the
// original columns followed by one column per window item.
func Eval(t *table.Table, q *Query) (*table.Table, *Plan, error) {
	if len(t.Rows) > MaxRows {
		return nil, nil, fmt.Errorf("input has %d rows, limit is %d", len(t.Rows), MaxRows)
	}
	// Assign every item a sort group: items with identical PARTITION BY and
	// ORDER BY share one sort.  We also merge orderings that are prefixes of
	// one another (see below).
	groups, plan, err := planGroups(q)
	if err != nil {
		return nil, nil, err
	}

	// Build the output schema, then allocate rows once with all result
	// columns present and copy each input row a single time.
	cols := make([]table.Column, 0, t.Schema.Len()+len(q.Items))
	cols = append(cols, t.Schema.Columns...)
	// Window items are numbered by their position in q.Items.
	for _, it := range q.Items {
		name := uniqueNameOf(cols, it.OutName)
		it.OutName = name
		cols = append(cols, table.Column{Name: name, Kind: resultKind(it)})
	}
	outSchema := table.NewSchema(cols...)
	out := &table.Table{Schema: outSchema, Rows: make([]table.Row, len(t.Rows))}
	for i, r := range t.Rows {
		nr := make(table.Row, len(cols))
		copy(nr, r)
		out.Rows[i] = nr
	}

	// Evaluate passthrough items (plain expressions in the select list).
	for ii, it := range q.Items {
		if it.Passthrough == nil {
			continue
		}
		col := t.Schema.Len() + ii
		for ri := range out.Rows {
			v, err := it.Passthrough.Eval(t.Rows[ri])
			if err != nil {
				return nil, nil, err
			}
			out.Rows[ri][col] = v
		}
	}
	// Evaluate each group of items.
	for _, g := range groups {
		if err := evalGroup(t, out, q, g, plan); err != nil {
			return nil, nil, err
		}
	}
	// Output ordering.
	if len(q.OutputOrder) > 0 {
		for i := range q.OutputOrder {
			k := &q.OutputOrder[i]
			if k.OrdinalName != "" {
				idx, ok := out.Schema.Index(k.OrdinalName)
				if !ok {
					return nil, nil, fmt.Errorf("ORDER BY position refers to missing column %q", k.OrdinalName)
				}
				k.Expr = outputColumnRef{idx: idx, name: k.OrdinalName}
				k.Kind = out.Schema.Columns[idx].Kind
				k.Ordinal = 0
			}
		}
		keys := compileKeys(q.OutputOrder, out.Schema)
		idx, err := sortIndexes(out, keys)
		if err != nil {
			return nil, nil, err
		}
		sorted := make([]table.Row, len(out.Rows))
		for i, id := range idx {
			sorted[i] = out.Rows[id]
		}
		out.Rows = sorted
	}
	return out, plan, nil
}

// resultKind infers the output column kind for a window item.
func resultKind(it *WindowItem) value.Kind {
	switch it.Kind {
	case FuncRanking:
		return value.KindInt
	case FuncOffset:
		if len(it.Call.Args) > 0 {
			if k := exprKind(it.Call.Args[0]); k != value.KindNull {
				return k
			}
		}
		return value.KindNull
	case FuncValue:
		if len(it.Call.Args) > 0 {
			return exprKind(it.Call.Args[0])
		}
		return value.KindNull
	case FuncAggregate:
		switch strings.ToLower(it.Call.Name) {
		case "count":
			return value.KindInt
		case "avg":
			return value.KindNumeric
		case "sum", "min", "max":
			if len(it.Call.Args) > 0 {
				return exprKind(it.Call.Args[0])
			}
		}
	}
	return value.KindNull
}

// exprKind returns the statically known kind of an expression, or KindNull
// when it cannot be determined without evaluating.
func exprKind(e expr.Expr) value.Kind {
	if k, ok := e.(interface{ Kind() value.Kind }); ok {
		return k.Kind()
	}
	return value.KindNull
}

// uniqueNameOf picks a column name not already present in cols.
func uniqueNameOf(cols []table.Column, base string) string {
	if base == "" {
		base = "expr"
	}
	taken := func(name string) bool {
		for _, c := range cols {
			if c.Name == name {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s_%d", base, i)
		if !taken(name) {
			return name
		}
	}
}

func uniqueName(s *table.Schema, base string) string {
	if base == "" {
		base = "expr"
	}
	if _, ok := s.Index(base); !ok {
		return base
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s_%d", base, i)
		if _, ok := s.Index(name); !ok {
			return name
		}
	}
}

// windowGroup is a set of items sharing one prepared sort.
type windowGroup struct {
	spec  *Spec
	sig   string
	items []*WindowItem
	id    string
}

func planGroups(q *Query) ([]*windowGroup, *Plan, error) {
	plan := &Plan{}
	seen := map[string]*windowGroup{}
	var groups []*windowGroup
	for _, it := range q.Items {
		if it.Passthrough != nil {
			continue
		}
		sig := partitionSignature(it.Spec) + "\x01" + orderSignature(it.Spec.Order)
		g, ok := seen[sig]
		if !ok {
			g = &windowGroup{spec: it.Spec, sig: sig}
			g.id = fmt.Sprintf("S%d", len(groups)+1)
			seen[sig] = g
			groups = append(groups, g)
			ps := PlanSort{ID: g.id, Partition: it.Spec.PartitionT, Order: it.Spec.OrderT}
			plan.Sorts = append(plan.Sorts, ps)
		}
		g.items = append(g.items, it)
	}
	// Fill plan sort members.
	for gi, g := range groups {
		for _, it := range g.items {
			plan.Sorts[gi].Items = append(plan.Sorts[gi].Items, it.OutName)
		}
	}
	// Plan items.
	for _, it := range q.Items {
		if it.Passthrough != nil {
			continue
		}
		sig := partitionSignature(it.Spec) + "\x01" + orderSignature(it.Spec.Order)
		g := seen[sig]
		frame := effectiveFrame(it)
		strategy := strategyFor(it)
		pi := PlanItem{
			OutName:   it.OutName,
			Func:      strings.ToLower(it.Call.Name),
			Strategy:  strategy,
			Window:    frameText(it.Spec),
			Frame:     frameDesc(frame),
			Exclusion: frame.Exclusion.String(),
			SortID:    g.id,
		}
		plan.Items = append(plan.Items, pi)
	}
	return groups, plan, nil
}

func strategyFor(it *WindowItem) string {
	switch it.Kind {
	case FuncAggregate:
		return "segment-tree frame aggregate"
	case FuncRanking:
		switch strings.ToLower(it.Call.Name) {
		case "ntile":
			return "peer-group rank, O(1) per row"
		default:
			return "peer-group scan, O(1) per row"
		}
	case FuncOffset:
		return "positional offset into ordered partition"
	case FuncValue:
		if it.Call.IgnoreNulls {
			return "prefix-count + order-statistic select (IGNORE NULLS)"
		}
		return "order-statistic segment tree over frame"
	}
	return "unknown"
}

func frameText(sp *Spec) string {
	txt := "OVER("
	if len(sp.PartitionT) > 0 {
		txt += "PARTITION BY " + strings.Join(sp.PartitionT, ", ") + " "
	}
	if len(sp.OrderT) > 0 {
		txt += "ORDER BY " + strings.Join(sp.OrderT, ", ") + " "
	}
	txt += ")"
	return strings.TrimSpace(txt)
}

// effectiveFrame returns the frame the engine will use, applying SQL defaults.
func effectiveFrame(it *WindowItem) *Frame {
	if it.Spec.Frame != nil {
		f := *it.Spec.Frame
		return &f
	}
	if it.Kind == FuncRanking || it.Kind == FuncOffset {
		// Ranking and offset functions ignore the frame.  Return the whole
		// partition as a descriptive frame.
		return &Frame{Unit: Rows, Start: Bound{Mode: BoundUnboundedPreceding}, End: Bound{Mode: BoundUnboundedFollowing}}
	}
	if it.Spec.Frame != nil {
		f := *it.Spec.Frame
		return &f
	}
	if len(it.Spec.Order) > 0 {
		return &Frame{Unit: Range, Start: Bound{Mode: BoundUnboundedPreceding}, End: Bound{Mode: BoundCurrentRow}}
	}
	return &Frame{Unit: Rows, Start: Bound{Mode: BoundUnboundedPreceding}, End: Bound{Mode: BoundUnboundedFollowing}}
}

func frameDesc(f *Frame) string {
	return f.Unit.String() + " BETWEEN " + boundDesc(f.Start) + " AND " + boundDesc(f.End)
}

func boundDesc(b Bound) string {
	switch b.Mode {
	case BoundPreceding, BoundFollowing:
		txt := "?"
		if b.Offset != nil {
			txt = b.Offset.String()
		}
		if b.Mode == BoundPreceding {
			return txt + " PRECEDING"
		}
		return txt + " FOLLOWING"
	}
	return b.Mode.String()
}

func compileKeys(keys []SortKey, schema *table.Schema) []sortKeyPlan {
	out := make([]sortKeyPlan, len(keys))
	for i, k := range keys {
		out[i] = sortKeyPlan{key: k}
	}
	return out
}

// outputColumnRef references an output column by index, used to implement
// positional ORDER BY.
type outputColumnRef struct {
	idx  int
	name string
}

func (o outputColumnRef) Eval(row table.Row) (value.Value, error) {
	if o.idx >= len(row) {
		return value.Null, fmt.Errorf("output column %s out of range", o.name)
	}
	return row[o.idx], nil
}
func (o outputColumnRef) HasColumn() bool { return true }
func (o outputColumnRef) String() string  { return o.name }
