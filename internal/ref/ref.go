// Package ref implements a deliberately naive reference window engine: for
// every row it materialises the partition, finds the frame by scanning, and
// aggregates by iterating the frame.  It exists only to cross-check the real
// engine in tests.  It shares the expression/value/table layers (so the two
// agree on expression semantics) but not the window algorithm.
package ref

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"dragonboat/windowengine/internal/expr"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
	"dragonboat/windowengine/internal/window"
)

// Eval runs the query with brute-force frame construction.
func Eval(t *table.Table, q *window.Query) (*table.Table, error) {
	// Build the output schema and allocate each row once.
	cols := make([]table.Column, 0, t.Schema.Len()+len(q.Items))
	cols = append(cols, t.Schema.Columns...)
	for _, it := range q.Items {
		cols = append(cols, table.Column{Name: it.OutName, Kind: inferKind(it)})
	}
	schema := table.NewSchema(cols...)
	out := &table.Table{Schema: schema, Rows: make([]table.Row, len(t.Rows))}
	for i, r := range t.Rows {
		nr := make(table.Row, len(cols))
		copy(nr, r)
		out.Rows[i] = nr
	}

	for ii, it := range q.Items {
		col := len(t.Schema.Columns) + ii
		if it.Passthrough != nil {
			for ri := range t.Rows {
				v, err := it.Passthrough.Eval(t.Rows[ri])
				if err != nil {
					return nil, err
				}
				out.Rows[ri][col] = v
			}
			continue
		}
		if err := evalItemBF(t, out, q, it, col); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func inferKind(it *window.WindowItem) value.Kind {
	switch it.Kind {
	case window.FuncRanking:
		return value.KindInt
	case window.FuncOffset, window.FuncValue:
		if len(it.Call.Args) > 0 {
			return expr.KindOf(it.Call.Args[0])
		}
	case window.FuncAggregate:
		switch strings.ToLower(it.Call.Name) {
		case "count":
			return value.KindInt
		case "avg":
			return value.KindNumeric
		default:
			if len(it.Call.Args) > 0 {
				return expr.KindOf(it.Call.Args[0])
			}
		}
	}
	return value.KindNull
}

func evalItemBF(t *table.Table, out *table.Table, q *window.Query, it *window.WindowItem, col int) error {
	spec := it.Spec
	n := len(t.Rows)

	// Partition row lists, in input order.
	parts := map[string][]int{}
	var order []string
	for i, r := range t.Rows {
		key, err := partitionKey(spec, r)
		if err != nil {
			return err
		}
		if _, ok := parts[key]; !ok {
			order = append(order, key)
		}
		parts[key] = append(parts[key], i)
	}

	for _, key := range order {
		members := parts[key]
		if err := evalPartitionBF(t, out, it, spec, members, col); err != nil {
			return err
		}
	}
	_ = n
	return nil
}

// partitionKey builds a string key from partition expressions; NULLs get a
// distinct marker so they group together.
func partitionKey(spec *window.Spec, r table.Row) (string, error) {
	var b strings.Builder
	for _, e := range spec.Partition {
		v, err := e.Eval(r)
		if err != nil {
			return "", err
		}
		if v.IsNull() {
			b.WriteString("\x00N")
		} else {
			b.WriteString(v.Kind.String())
			b.WriteString("\x00")
			b.WriteString(v.String())
		}
		b.WriteString("\x01")
	}
	return b.String(), nil
}

func evalPartitionBF(t *table.Table, out *table.Table, it *window.WindowItem, spec *window.Spec, members []int, col int) error {
	m := len(members)
	frame := effFrame(it)

	// Order the partition members by the ORDER BY keys (stable, input order as
	// tiebreak).  PostgreSQL's row order within a peer group is not defined;
	// our engines agree on input order.
	ordered := append([]int(nil), members...)
	keyVals := make([][]value.Value, len(ordered))
	for i, id := range ordered {
		kv, err := evalKeys(spec.Order, t.Rows[id])
		if err != nil {
			return err
		}
		keyVals[i] = kv
	}
	idx := make([]int, len(ordered))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return cmpKeys(keyVals[idx[a]], keyVals[idx[b]], spec.Order) < 0
	})
	sortedIDs := make([]int, m)
	sortedKeys := make([][]value.Value, m)
	for i, id := range idx {
		sortedIDs[i] = ordered[id]
		sortedKeys[i] = keyVals[id]
	}
	ordered = sortedIDs

	switch it.Kind {
	case window.FuncRanking:
		return rankingBF(out, it, ordered, sortedKeys, spec, col)
	case window.FuncOffset:
		return offsetBF(t, out, it, ordered, spec, col)
	case window.FuncAggregate, window.FuncValue:
		return frameBF(t, out, it, spec, frame, ordered, sortedKeys, col)
	}
	return fmt.Errorf("unknown item kind")
}

func effFrame(it *window.WindowItem) *window.Frame {
	if it.Spec.Frame != nil {
		return it.Spec.Frame
	}
	if len(it.Spec.Order) > 0 {
		return &window.Frame{Unit: window.Range, Start: window.Bound{Mode: window.BoundUnboundedPreceding}, End: window.Bound{Mode: window.BoundCurrentRow}}
	}
	return &window.Frame{Unit: window.Rows, Start: window.Bound{Mode: window.BoundUnboundedPreceding}, End: window.Bound{Mode: window.BoundUnboundedFollowing}}
}

func evalKeys(keys []window.SortKey, r table.Row) ([]value.Value, error) {
	out := make([]value.Value, len(keys))
	for i, k := range keys {
		v, err := k.Expr.Eval(r)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func cmpKeys(a, b []value.Value, keys []window.SortKey) int {
	for i := range keys {
		an, bn := a[i].IsNull(), b[i].IsNull()
		if an || bn {
			if an && bn {
				continue
			}
			nf := keys[i].NullsFirst
			if an {
				if nf {
					return -1
				}
				return 1
			}
			if nf {
				return 1
			}
			return -1
		}
		c, err := value.Compare(a[i], b[i])
		if err != nil {
			continue
		}
		if c != 0 {
			if keys[i].Desc {
				return -c
			}
			return c
		}
	}
	return 0
}

// ---- ranking ----

func rankingBF(out *table.Table, it *window.WindowItem, ordered []int, keys [][]value.Value, spec *window.Spec, col int) error {
	m := len(ordered)
	hasOrder := len(spec.Order) > 0
	// peer group of each position by brute force comparison
	groupOfPos := make([]int, m)
	groupStart := make([]int, m)
	groupEnd := make([]int, m)
	if hasOrder {
		gid := 0
		for i := 0; i < m; {
			j := i
			for j+1 < m && cmpKeys(keys[j+1], keys[i], spec.Order) == 0 {
				j++
			}
			for k := i; k <= j; k++ {
				groupOfPos[k] = gid
				groupStart[k] = i
				groupEnd[k] = j
			}
			gid++
			i = j + 1
		}
	} else {
		for k := 0; k < m; k++ {
			groupStart[k], groupEnd[k] = 0, m-1
		}
	}
	name := strings.ToLower(it.Call.Name)
	switch name {
	case "row_number":
		for i, id := range ordered {
			out.Rows[id][col] = value.Int(int64(i + 1))
		}
	case "rank":
		for i, id := range ordered {
			out.Rows[id][col] = value.Int(int64(groupStart[i] + 1))
		}
	case "dense_rank":
		for i, id := range ordered {
			out.Rows[id][col] = value.Int(int64(groupOfPos[i] + 1))
		}
	case "percent_rank":
		for i, id := range ordered {
			if m <= 1 {
				out.Rows[id][col] = value.Float(0)
			} else {
				out.Rows[id][col] = value.Float(float64(groupStart[i]) / float64(m-1))
			}
		}
	case "cume_dist":
		for i, id := range ordered {
			out.Rows[id][col] = value.Float(float64(groupEnd[i]+1) / float64(m))
		}
	case "ntile":
		v, err := it.Call.Args[0].Eval(nil)
		if err != nil {
			return err
		}
		if v.IsNull() {
			return fmt.Errorf("argument of ntile must not be null")
		}
		nb, err := value.AsInt64(v)
		if err != nil || nb <= 0 {
			return fmt.Errorf("argument of ntile must be greater than zero")
		}
		base := int64(m) / nb
		extra := int64(m) % nb
		i := 0
		for bucket := int64(1); bucket <= nb && i < m; bucket++ {
			size := base
			if bucket <= extra {
				size++
			}
			for k := int64(0); k < size && i < m; k++ {
				out.Rows[ordered[i]][col] = value.Int(bucket)
				i++
			}
		}
	default:
		return fmt.Errorf("unknown ranking function %s", name)
	}
	return nil
}

// ---- lag/lead ----

func offsetBF(t *table.Table, out *table.Table, it *window.WindowItem, ordered []int, spec *window.Spec, col int) error {
	m := len(ordered)
	name := it.Call.Name
	args := it.Call.Args
	vals := make([]value.Value, m)
	for i, id := range ordered {
		v, err := args[0].Eval(t.Rows[id])
		if err != nil {
			return err
		}
		vals[i] = v
	}
	// Positions of non-null values, in order.  With IGNORE NULLS the n-th
	// non-null neighbour is a binary search over this index (the semantics are
	// still "skip nulls toward the requested direction").
	var nonNull []int
	if it.Call.IgnoreNulls {
		for p, v := range vals {
			if !v.IsNull() {
				nonNull = append(nonNull, p)
			}
		}
	}
	for i, id := range ordered {
		off := int64(1)
		if len(args) >= 2 {
			ov, err := args[1].Eval(t.Rows[id])
			if err != nil {
				return err
			}
			if ov.IsNull() {
				out.Rows[id][col] = value.Null
				continue
			}
			off, err = value.AsInt64(ov)
			if err != nil {
				return err
			}
		}
		if name == "lead" {
			off = -off
		}
		def := value.Null
		if len(args) >= 3 {
			dv, err := args[2].Eval(t.Rows[id])
			if err != nil {
				return err
			}
			def = dv
		}
		// brute force: enumerate direction
		dir := -1
		if off < 0 {
			dir, off = 1, -off
		}
		target := -1
		if off == 0 {
			target = i
		} else if it.Call.IgnoreNulls {
			// Count non-null values strictly before i, then step off
			// positions through the index.
			pos := sort.SearchInts(nonNull, i)
			var idx int
			if dir < 0 {
				idx = pos - int(off)
			} else {
				// Skip i itself when it is non-null.
				start := pos
				if start < len(nonNull) && nonNull[start] == i {
					start++
				}
				idx = start + int(off) - 1
			}
			if idx >= 0 && idx < len(nonNull) {
				target = nonNull[idx]
			}
		} else {
			p := i + dir*int(off)
			if p >= 0 && p < m {
				target = p
			}
		}
		if target < 0 {
			out.Rows[id][col] = def
		} else {
			out.Rows[id][col] = vals[target]
		}
	}
	return nil
}

// ---- frames (aggregates and value functions) ----

func frameBF(t *table.Table, out *table.Table, it *window.WindowItem, spec *window.Spec, frame *window.Frame, ordered []int, keys [][]value.Value, col int) error {
	m := len(ordered)
	rows := make([]table.Row, m)
	for i, id := range ordered {
		rows[i] = t.Rows[id]
	}
	wrapper := &bfWindow{
		rows:  rows,
		keys:  keys,
		spec:  spec,
		frame: frame,
		m:     m,
	}
	// Evaluate the per-row arguments and FILTER once per partition.  This
	// does not change semantics: the frame is still found and scanned row by
	// row; only repeated expression evaluation is factored out.
	if it.Kind == window.FuncAggregate && len(it.Call.Args) > 0 {
		vals := make([]value.Value, m)
		for i, r := range rows {
			v, err := it.Call.Args[0].Eval(r)
			if err != nil {
				return err
			}
			vals[i] = v
		}
		wrapper.aggVals = vals
		wrapper.aggValCol = -1
	}
	if it.Kind == window.FuncAggregate && it.Call.Filter != nil {
		elig := make([]bool, m)
		for i, r := range rows {
			v, err := it.Call.Filter.Eval(r)
			if err != nil {
				return err
			}
			elig[i] = !v.IsNull() && v.Kind == value.KindBool && v.B
		}
		wrapper.eligible = elig
	}
	for i, id := range ordered {
		s, e, err := wrapper.bounds(i)
		if err != nil {
			return err
		}
		if s < 0 {
			s = 0
		}
		if e >= m {
			e = m - 1
		}
		v, err := wrapper.aggregate(it, i, s, e)
		if err != nil {
			return err
		}
		out.Rows[id][col] = v
	}
	return nil
}

type bfWindow struct {
	rows  []table.Row
	keys  [][]value.Value
	spec  *window.Spec
	frame *window.Frame
	m     int
	// Precomputed aggregate argument and FILTER results (nil when absent).
	aggVals   []value.Value
	aggValCol int
	eligible  []bool
	// peer group cache
	groupOf []int
	gS      []int
	gE      []int
	built   bool
}

func (w *bfWindow) buildGroups() {
	if w.built {
		return
	}
	w.built = true
	w.groupOf = make([]int, w.m)
	if w.m == 0 {
		return
	}
	if len(w.spec.Order) == 0 {
		// Without ORDER BY every row is a peer of every other row.
		w.gS = append(w.gS, 0)
		w.gE = append(w.gE, w.m-1)
		for k := 0; k < w.m; k++ {
			w.groupOf[k] = 0
		}
		return
	}
	for i := 0; i < w.m; {
		j := i
		for j+1 < w.m && cmpKeys(w.keys[j+1], w.keys[i], w.spec.Order) == 0 {
			j++
		}
		w.gS = append(w.gS, i)
		w.gE = append(w.gE, j)
		for k := i; k <= j; k++ {
			w.groupOf[k] = len(w.gS) - 1
		}
		i = j + 1
	}
}

// bounds computes the frame by scanning every row, exactly as the SQL
// standard describes: a row is in the frame if it satisfies the start and end
// predicates.
func (w *bfWindow) bounds(i int) (int, int, error) {
	w.buildGroups()
	f := w.frame
	switch f.Unit {
	case window.Rows:
		s, err := w.rowsEdge(f.Start, i, true)
		if err != nil {
			return 0, -1, err
		}
		e, err := w.rowsEdge(f.End, i, false)
		if err != nil {
			return 0, -1, err
		}
		return s, e, nil
	case window.Groups:
		s, err := w.groupsEdge(f.Start, i, true)
		if err != nil {
			return 0, -1, err
		}
		e, err := w.groupsEdge(f.End, i, false)
		if err != nil {
			return 0, -1, err
		}
		return s, e, nil
	case window.Range:
		s, err := w.rangeEdge(f.Start, i, true)
		if err != nil {
			return 0, -1, err
		}
		e, err := w.rangeEdge(f.End, i, false)
		if err != nil {
			return 0, -1, err
		}
		return s, e, nil
	}
	return 0, -1, fmt.Errorf("bad unit")
}

func (w *bfWindow) offsetVal(b window.Bound, i int) (value.Value, bool, error) {
	if b.Offset == nil {
		return value.Null, false, nil
	}
	v, err := b.Offset.Eval(w.rows[i])
	if err != nil {
		return value.Null, false, err
	}
	return v, true, nil
}

func (w *bfWindow) rowsEdge(b window.Bound, i int, isStart bool) (int, error) {
	switch b.Mode {
	case window.BoundUnboundedPreceding:
		return 0, nil
	case window.BoundUnboundedFollowing:
		return w.m - 1, nil
	case window.BoundCurrentRow:
		return i, nil
	}
	v, _, err := w.offsetVal(b, i)
	if err != nil {
		return 0, err
	}
	if v.IsNull() {
		return 0, fmt.Errorf("frame %s offset must not be null", edgeName(isStart))
	}
	if !v.Kind.IsNumericKind() {
		return 0, fmt.Errorf("frame %s offset must be a number", edgeName(isStart))
	}
	f, err := value.ToFloat(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("frame %s offset must be finite", edgeName(isStart))
	}
	off := int64(math.Round(f))
	if off < 0 {
		return 0, fmt.Errorf("frame %s offset must not be negative", edgeName(isStart))
	}
	if b.Mode == window.BoundPreceding {
		return i - int(off), nil
	}
	return i + int(off), nil
}

func edgeName(isStart bool) string {
	if isStart {
		return "starting"
	}
	return "ending"
}

func (w *bfWindow) groupsEdge(b window.Bound, i int, isStart bool) (int, error) {
	switch b.Mode {
	case window.BoundUnboundedPreceding:
		return 0, nil
	case window.BoundUnboundedFollowing:
		return w.m - 1, nil
	case window.BoundCurrentRow:
		if isStart {
			return w.gS[w.groupOf[i]], nil
		}
		return w.gE[w.groupOf[i]], nil
	}
	v, _, err := w.offsetVal(b, i)
	if err != nil {
		return 0, err
	}
	if v.IsNull() {
		return 0, fmt.Errorf("frame %s offset must not be null", edgeName(isStart))
	}
	f, err := value.ToFloat(v)
	if err != nil {
		return 0, err
	}
	off := int64(math.Round(f))
	if off < 0 {
		return 0, fmt.Errorf("frame %s offset must not be negative", edgeName(isStart))
	}
	g := w.groupOf[i]
	target := g + int(off)
	if b.Mode == window.BoundPreceding {
		target = g - int(off)
	}
	if target < 0 {
		if isStart {
			return 0, nil
		}
		return -1, nil
	}
	if target >= len(w.gS) {
		if isStart {
			return w.m, nil
		}
		return w.m - 1, nil
	}
	if isStart {
		return w.gS[target], nil
	}
	return w.gE[target], nil
}

// rangeEdge is the brute-force RANGE edge: it tests each row in the partition
// against the bound predicate, with NULL order values treated per PostgreSQL.
func (w *bfWindow) rangeEdge(b window.Bound, i int, isStart bool) (int, error) {
	m := w.m
	switch b.Mode {
	case window.BoundUnboundedPreceding:
		return 0, nil
	case window.BoundUnboundedFollowing:
		return m - 1, nil
	}
	if len(w.spec.Order) == 0 {
		// Without ORDER BY, RANGE CURRENT ROW means the whole partition (all
		// rows are peers) and offsets are rejected at validation.
		return 0, nil
	}
	order := w.spec.Order[0]
	cur := w.keys[i][0]
	if b.Mode == window.BoundCurrentRow {
		// Peers: equal order values (NULL == NULL).
		if isStart {
			for p := i; p >= 0; p-- {
				if cmpKeys(w.keys[p], w.keys[i], w.spec.Order) != 0 {
					return p + 1, nil
				}
			}
			return 0, nil
		}
		for p := i; p < m; p++ {
			if cmpKeys(w.keys[p], w.keys[i], w.spec.Order) != 0 {
				return p - 1, nil
			}
		}
		return m - 1, nil
	}
	v, _, err := w.offsetVal(b, i)
	if err != nil {
		return 0, err
	}
	if v.IsNull() {
		return 0, fmt.Errorf("frame %s offset must not be null", edgeName(isStart))
	}
	ok := value.OffsetCompatible(order.Kind, v.Kind)
	if order.Kind == value.KindNull {
		ok = value.OffsetCompatible(cur.Kind, v.Kind)
	}
	if !ok {
		return 0, fmt.Errorf("RANGE with offset PRECEDING/FOLLOWING is not supported for column type %s and offset type %s", order.Kind, v.Kind)
	}
	if !nonNegativeOffset(v) {
		return 0, fmt.Errorf("invalid preceding or following size in window function")
	}
	if cur.IsNull() {
		// NULL current row: finite offsets keep the edge within the NULL group.
		if isStart {
			return w.peerFirst(i), nil
		}
		return w.peerLast(i), nil
	}
	limit, err := rangeLimitBF(cur, v, b.Mode, order.Desc)
	if err != nil {
		return 0, err
	}
	// Determine, for each row, whether it lies before/after the limit in sort
	// order, and derive the edge by scanning.
	if isStart {
		// first position whose value is not before limit
		for p := 0; p < m; p++ {
			val := w.keys[p][0]
			if val.IsNull() {
				// NULLs are never in a finite-offset frame edge for a non-null
				// current row; if they sort before the limit they are skipped.
				if nullIsBefore(val, limit, order) {
					continue
				}
				// NULLs sorting after the limit: edge is here.
				return p, nil
			}
			c, err := value.Compare(val, limit)
			if err != nil {
				return 0, err
			}
			before := c < 0
			if order.Desc {
				before = c > 0
			}
			if !before {
				return p, nil
			}
		}
		return m, nil
	}
	// last position not beyond limit
	for p := m - 1; p >= 0; p-- {
		val := w.keys[p][0]
		if val.IsNull() {
			if nullIsBefore(val, limit, order) {
				return p, nil
			}
			continue
		}
		c, err := value.Compare(val, limit)
		if err != nil {
			return 0, err
		}
		beyond := c > 0
		if order.Desc {
			beyond = c < 0
		}
		if !beyond {
			return p, nil
		}
	}
	return -1, nil
}

func nullIsBefore(nullVal, limit value.Value, order window.SortKey) bool {
	// NULL sorts before everything when NULLS FIRST.
	return order.NullsFirst
}

func (w *bfWindow) peerFirst(i int) int {
	p := i
	for p > 0 && cmpKeys(w.keys[p-1], w.keys[i], w.spec.Order) == 0 {
		p--
	}
	return p
}
func (w *bfWindow) peerLast(i int) int {
	p := i
	for p+1 < w.m && cmpKeys(w.keys[p+1], w.keys[i], w.spec.Order) == 0 {
		p++
	}
	return p
}

func nonNegativeOffset(v value.Value) bool {
	switch v.Kind {
	case value.KindInterval:
		return v.D.Months >= 0 && v.D.Days >= 0 && v.D.Nanos >= 0
	case value.KindInt:
		return v.I >= 0
	case value.KindFloat:
		return v.F >= 0
	case value.KindNumeric:
		f, err := value.ToFloat(v)
		return err == nil && f >= 0
	}
	return false
}

func rangeLimitBF(cur, off value.Value, mode window.BoundMode, desc bool) (value.Value, error) {
	if mode == window.BoundPreceding {
		if desc {
			return addValues(cur, off)
		}
		return subValues(cur, off)
	}
	if desc {
		return subValues(cur, off)
	}
	return addValues(cur, off)
}

func addValues(a, b value.Value) (value.Value, error) { return value.AddOffset(a, b) }
func subValues(a, b value.Value) (value.Value, error) {
	neg, err := negValue(b)
	if err != nil {
		return value.Null, err
	}
	return value.AddOffset(a, neg)
}

func negValue(v value.Value) (value.Value, error) {
	switch v.Kind {
	case value.KindInt:
		return value.Int(-v.I), nil
	case value.KindFloat:
		return value.Float(-v.F), nil
	case value.KindNumeric:
		r, err := value.ToRat(v)
		if err != nil {
			return value.Null, err
		}
		r.Neg(r)
		return value.Numeric(value.RatToFixed(r, 40)), nil
	case value.KindInterval:
		return value.IntervalV(value.Interval{Months: -v.D.Months, Days: -v.D.Days, Nanos: -v.D.Nanos}), nil
	}
	return value.Null, fmt.Errorf("cannot negate %s", v.Kind)
}

// aggregate evaluates an aggregate or value function over the brute-force
// frame [s, e], after applying exclusions.
func (w *bfWindow) aggregate(it *window.WindowItem, i, s, e int) (value.Value, error) {
	if it.Kind == window.FuncValue {
		// Value functions are answered by scanning the frame for the k-th
		// selectable position; no position list is materialised.
		return w.valueFuncFrame(it, i, s, e)
	}
	positions := []int{}
	for p := s; p <= e; p++ {
		positions = append(positions, p)
	}
	// exclusion
	f := w.frame
	switch f.Exclusion {
	case window.ExcludeCurrentRow:
		positions = removePos(positions, i)
	case window.ExcludeGroup:
		gs, ge := w.gS[w.groupOf[i]], w.gE[w.groupOf[i]]
		filtered := positions[:0]
		for _, p := range positions {
			if p < gs || p > ge {
				filtered = append(filtered, p)
			}
		}
		positions = filtered
	case window.ExcludeTies:
		gs, ge := w.gS[w.groupOf[i]], w.gE[w.groupOf[i]]
		filtered := positions[:0]
		for _, p := range positions {
			if p >= gs && p <= ge && p != i {
				continue
			}
			filtered = append(filtered, p)
		}
		positions = filtered
	}

	return w.aggregateFunc(it, positions)
}

func removePos(ps []int, p int) []int {
	out := ps[:0]
	for _, q := range ps {
		if q != p {
			out = append(out, q)
		}
	}
	return out
}

func (w *bfWindow) aggregateFunc(it *window.WindowItem, positions []int) (value.Value, error) {
	name := strings.ToLower(it.Call.Name)
	// The frame is scanned one row at a time, as the SQL standard describes.
	// Values are accumulated in place rather than collected into a slice, so
	// that the reference stays usable on large inputs; the semantics are the
	// same as gathering the frame and reducing it.
	var (
		vals      []value.Value
		types     []value.Kind
		countStar int64
		cnt       int64
		nn        int64
		sumI      int64
		sumF      float64
		sumR      = new(big.Rat)
		minV      value.Value
		maxV      value.Value
		has       bool
		kind      value.Kind
	)
	for _, p := range positions {
		if w.eligible != nil && !w.eligible[p] {
			continue
		}
		if name == "count" && (it.Call.Star || len(it.Call.Args) == 0) {
			countStar++
			continue
		}
		var v value.Value
		if w.aggVals != nil {
			v = w.aggVals[p]
		} else {
			var err error
			v, err = it.Call.Args[0].Eval(w.rows[p])
			if err != nil {
				return value.Null, err
			}
		}
		cnt++
		if v.IsNull() {
			continue
		}
		nn++
		if kind == value.KindNull || kind == value.KindInt && v.Kind != value.KindInt {
			// Mixed kinds: prefer the wider/more general kind.
			if kindPriority(v.Kind) > kindPriority(kind) {
				kind = v.Kind
			}
		}
		switch v.Kind {
		case value.KindInt:
			sumI += v.I
		case value.KindFloat:
			f, _ := value.ToFloat(v)
			sumF += f
		default:
			if r, err := value.ToRat(v); err == nil {
				sumR.Add(sumR, r)
			}
		}
		if !has {
			minV, maxV, has = v, v, true
		} else {
			if c, err := value.Compare(v, minV); err == nil && c < 0 {
				minV = v
			}
			if c, err := value.Compare(v, maxV); err == nil && c > 0 {
				maxV = v
			}
		}
	}
	switch name {
	case "count":
		if it.Call.Star || len(it.Call.Args) == 0 {
			return value.Int(countStar), nil
		}
		return value.Int(nn), nil
	case "sum":
		if nn == 0 {
			return value.Null, nil
		}
		switch kind {
		case value.KindFloat:
			return value.Float(sumF), nil
		case value.KindNumeric:
			return value.Numeric(value.RatToString(sumR, 40)), nil
		default:
			return value.Int(sumI), nil
		}
	case "avg":
		if nn == 0 {
			return value.Null, nil
		}
		if kind == value.KindFloat {
			return value.Float(sumF / float64(nn)), nil
		}
		r := new(big.Rat)
		if kind == value.KindNumeric {
			r.Set(sumR)
		} else {
			r.SetInt64(sumI)
		}
		r.Quo(r, new(big.Rat).SetInt64(nn))
		return value.Numeric(value.RatToString(r, 40)), nil
	case "min":
		return minV, nil
	case "max":
		return maxV, nil
	}
	_ = vals
	_ = types
	_ = cnt
	switch name {
	case "count":
		if it.Call.Star || len(it.Call.Args) == 0 {
			return value.Int(int64(len(vals))), nil
		}
		n := int64(0)
		for _, v := range vals {
			if !v.IsNull() {
				n++
			}
		}
		return value.Int(n), nil
	case "sum":
		kind := typesKind(types)
		return sumVals(vals, kind)
	case "avg":
		kind := typesKind(types)
		return avgVals(vals, kind)
	case "min":
		return minVals(vals)
	case "max":
		return maxVals(vals)
	}
	return value.Null, fmt.Errorf("unknown aggregate %s", name)
}

// kindPriority orders kinds by generality for mixed-kind aggregates.
func kindPriority(k value.Kind) int {
	switch k {
	case value.KindNull:
		return 0
	case value.KindInt:
		return 1
	case value.KindNumeric:
		return 2
	case value.KindFloat:
		return 3
	}
	return 4
}

func typesKind(types []value.Kind) value.Kind {
	for _, k := range types {
		if k != value.KindNull {
			return k
		}
	}
	return value.KindNull
}

func sumVals(vals []value.Value, kind value.Kind) (value.Value, error) {
	switch kind {
	case value.KindNull:
		return value.Null, nil
	case value.KindFloat:
		sum := 0.0
		n := int64(0)
		for _, v := range vals {
			if v.IsNull() {
				continue
			}
			f, err := value.ToFloat(v)
			if err != nil {
				return value.Null, err
			}
			sum += f
			n++
		}
		if n == 0 {
			return value.Null, nil
		}
		return value.Float(sum), nil
	case value.KindNumeric:
		sum := newBigRat()
		n := int64(0)
		for _, v := range vals {
			if v.IsNull() {
				continue
			}
			r, err := value.ToRat(v)
			if err != nil {
				return value.Null, err
			}
			sum.Add(sum, r)
			n++
		}
		if n == 0 {
			return value.Null, nil
		}
		return value.Numeric(value.RatToString(sum, 40)), nil
	default:
		var sum int64
		n := int64(0)
		for _, v := range vals {
			if v.IsNull() {
				continue
			}
			iv, err := value.AsInt64(v)
			if err != nil {
				return value.Null, err
			}
			sum += iv
			n++
		}
		if n == 0 {
			return value.Null, nil
		}
		return value.Int(sum), nil
	}
}

func avgVals(vals []value.Value, kind value.Kind) (value.Value, error) {
	switch kind {
	case value.KindNull:
		return value.Null, nil
	case value.KindFloat:
		sum := 0.0
		n := int64(0)
		for _, v := range vals {
			if v.IsNull() {
				continue
			}
			f, err := value.ToFloat(v)
			if err != nil {
				return value.Null, err
			}
			sum += f
			n++
		}
		if n == 0 {
			return value.Null, nil
		}
		return value.Float(sum / float64(n)), nil
	default:
		sum := newBigRat()
		n := int64(0)
		for _, v := range vals {
			if v.IsNull() {
				continue
			}
			r, err := value.ToRat(v)
			if err != nil {
				return value.Null, err
			}
			sum.Add(sum, r)
			n++
		}
		if n == 0 {
			return value.Null, nil
		}
		sum.Quo(sum, newBigRatInt(n))
		return value.Numeric(value.RatToString(sum, 40)), nil
	}
}

func minVals(vals []value.Value) (value.Value, error) {
	best := value.Null
	for _, v := range vals {
		if v.IsNull() {
			continue
		}
		if best.IsNull() {
			best = v
			continue
		}
		c, err := value.Compare(v, best)
		if err != nil {
			return value.Null, err
		}
		if c < 0 {
			best = v
		}
	}
	return best, nil
}

func maxVals(vals []value.Value) (value.Value, error) {
	best := value.Null
	for _, v := range vals {
		if v.IsNull() {
			continue
		}
		if best.IsNull() {
			best = v
			continue
		}
		c, err := value.Compare(v, best)
		if err != nil {
			return value.Null, err
		}
		if c > 0 {
			best = v
		}
	}
	return best, nil
}

// valueFunc implements first/last/nth over the (excluded) frame positions.
// valueFuncFrame scans the frame [s,e] and selects first/last/nth position,
// honouring EXCLUDE and IGNORE NULLS, exactly as walking the frame row by row.
func (w *bfWindow) valueFuncFrame(it *window.WindowItem, cur, s, e int) (value.Value, error) {
	name := strings.ToLower(it.Call.Name)
	ign := it.Call.IgnoreNulls
	// nth_value's n is per-row.
	want := 1
	if name == "last_value" {
		want = -1
	} else if name == "nth_value" {
		nv, err := it.Call.Args[1].Eval(w.rows[cur])
		if err != nil {
			return value.Null, err
		}
		if nv.IsNull() {
			return value.Null, nil
		}
		n, err := value.AsInt64(nv)
		if err != nil {
			return value.Null, err
		}
		if n <= 0 {
			return value.Null, fmt.Errorf("argument of nth_value must be greater than zero")
		}
		want = int(n)
	}
	gs, ge := -1, -2
	if w.frame.Exclusion == window.ExcludeGroup || w.frame.Exclusion == window.ExcludeTies {
		gs, ge = w.gS[w.groupOf[cur]], w.gE[w.groupOf[cur]]
	}
	if name == "last_value" {
		// Walk backwards.
		for p := e; p >= s; p-- {
			if !w.positionSelectable(it, cur, p, ign, gs, ge) {
				continue
			}
			if want == -1 {
				return it.Call.Args[0].Eval(w.rows[p])
			}
			want--
		}
		return value.Null, nil
	}
	seen := 0
	for p := s; p <= e; p++ {
		if !w.positionSelectable(it, cur, p, ign, gs, ge) {
			continue
		}
		seen++
		if seen == want {
			return it.Call.Args[0].Eval(w.rows[p])
		}
	}
	return value.Null, nil
}

// positionSelectable reports whether frame position p contributes a selectable
// value for the current row after EXCLUDE and IGNORE NULLS.
func (w *bfWindow) positionSelectable(it *window.WindowItem, cur, p int, ignoreNull bool, gs, ge int) bool {
	switch w.frame.Exclusion {
	case window.ExcludeCurrentRow:
		if p == cur {
			return false
		}
	case window.ExcludeGroup:
		if p >= gs && p <= ge {
			return false
		}
	case window.ExcludeTies:
		if p >= gs && p <= ge && p != cur {
			return false
		}
	}
	if ignoreNull {
		// The value must be re-evaluated to tell; this is the same cost the
		// naive scan pays.
		v, err := it.Call.Args[0].Eval(w.rows[p])
		if err != nil || v.IsNull() {
			return false
		}
	}
	return true
}

func (w *bfWindow) valueFunc(it *window.WindowItem, cur int, positions []int) (value.Value, error) {
	name := strings.ToLower(it.Call.Name)
	ign := it.Call.IgnoreNulls
	// selectable positions
	if ign {
		filtered := positions[:0]
		for _, p := range positions {
			v, err := it.Call.Args[0].Eval(w.rows[p])
			if err != nil {
				return value.Null, err
			}
			if !v.IsNull() {
				filtered = append(filtered, p)
			}
		}
		positions = filtered
	}
	pick := func(idx int) (value.Value, error) {
		if idx < 0 || idx >= len(positions) {
			return value.Null, nil
		}
		return it.Call.Args[0].Eval(w.rows[positions[idx]])
	}
	// positions already had the exclusion applied by aggregate().
	switch name {
	case "first_value":
		return pick(0)
	case "last_value":
		return pick(len(positions) - 1)
	case "nth_value":
		// n is per-row
		nv, err := it.Call.Args[1].Eval(w.rows[cur])
		if err != nil {
			return value.Null, err
		}
		if nv.IsNull() {
			return value.Null, nil
		}
		n, err := value.AsInt64(nv)
		if err != nil {
			return value.Null, err
		}
		if n <= 0 {
			return value.Null, fmt.Errorf("argument of nth_value must be greater than zero")
		}
		return pick(int(n) - 1)
	}
	return value.Null, fmt.Errorf("unknown value function %s", name)
}

func newBigRat() *big.Rat           { return new(big.Rat) }
func newBigRatInt(n int64) *big.Rat { return new(big.Rat).SetInt64(n) }
