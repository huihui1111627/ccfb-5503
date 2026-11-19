package window

import (
	"strings"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// evalOffsetFunc implements lag and lead.
//
//	lag(value [, offset [, default]])  IGNORE NULLS
//
// offset defaults to 1 and may be any non-null integer (negative offsets flip
// the direction, as in PostgreSQL).  default is evaluated on the current row
// and used when the target position falls outside the partition.  With
// IGNORE NULLS the n-th non-null value in the given direction is returned
// (an extension; PostgreSQL 16 does not support IGNORE NULLS).
func evalOffsetFunc(it *WindowItem, pc *partitionContext, out *table.Table, col int) error {
	name := it.Call.Name
	args := it.Call.Args
	m := pc.m
	vals, err := evalArgColumn(args[0], pc)
	if err != nil {
		return err
	}
	offsetExpr := Expr(literalOne)
	if len(args) >= 2 {
		offsetExpr = args[1]
	}
	var defaultExpr Expr
	if len(args) >= 3 {
		defaultExpr = args[2]
	}
	var defaults []value.Value
	if defaultExpr != nil {
		defaults = make([]value.Value, m)
		for i := 0; i < m; i++ {
			v, err := defaultExpr.Eval(pc.rows[pc.pt.order[pc.base+i]])
			if err != nil {
				return err
			}
			defaults[i] = v
		}
	}
	offsets := make([]value.Value, m)
	for i := 0; i < m; i++ {
		v, err := offsetExpr.Eval(pc.rows[pc.pt.order[pc.base+i]])
		if err != nil {
			return err
		}
		offsets[i] = v
	}

	// For IGNORE NULLS we walk the value slice, skipping NULLs.  A prefix
	// index would also work, but offsets may be per-row; the walk is bounded
	// by the partition and only used with the extension.
	for i := 0; i < m; i++ {
		def := value.Null
		if defaults != nil {
			def = defaults[i]
		}
		ov := offsets[i]
		if ov.IsNull() {
			out.Rows[pc.pt.order[pc.base+i]][col] = value.Null
			continue
		}
		off, err := value.AsInt64(ov)
		if err != nil {
			return semErrf("offset argument of %s must be an integer", name)
		}
		if name == "lead" {
			off = -off
		}
		dir := -1
		if off < 0 {
			dir = 1
			off = -off
		}
		if off == 0 {
			out.Rows[pc.pt.order[pc.base+i]][col] = vals[i]
			continue
		}
		if off > int64(m) {
			out.Rows[pc.pt.order[pc.base+i]][col] = def
			continue
		}
		var got value.Value
		found := false
		if it.Call.IgnoreNulls {
			idx := -1
			if dir < 0 {
				idx = nthNonNullBackward(vals, i-1, int(off))
			} else {
				idx = nthNonNullForward(vals, i+1, int(off))
			}
			if idx >= 0 {
				got, found = vals[idx], true
			}
		} else {
			target := i + dir*int(off)
			if target >= 0 && target < m {
				got, found = vals[target], true
			}
		}
		if found {
			out.Rows[pc.pt.order[pc.base+i]][col] = got
		} else {
			out.Rows[pc.pt.order[pc.base+i]][col] = def
		}
	}
	return nil
}

// Expr is the minimal expression interface used here; it avoids importing the
// expr package just for the type name.
type Expr interface {
	Eval(table.Row) (value.Value, error)
	HasColumn() bool
	String() string
}

var literalOne = oneExpr{}

type oneExpr struct{}

func (oneExpr) Eval(table.Row) (value.Value, error) { return value.Int(1), nil }
func (oneExpr) HasColumn() bool                     { return false }
func (oneExpr) String() string                      { return "1" }

func nthNonNullBackward(vals []value.Value, from, k int) int {
	for i := from; i >= 0; i-- {
		if !vals[i].IsNull() {
			k--
			if k == 0 {
				return i
			}
		}
	}
	return -1
}

func nthNonNullForward(vals []value.Value, from, k int) int {
	for i := from; i < len(vals); i++ {
		if !vals[i].IsNull() {
			k--
			if k == 0 {
				return i
			}
		}
	}
	return -1
}

// evalValueItem implements first_value, last_value and nth_value over the
// row's frame.
//
// Positioning is relative to the frame contents: first_value is the first
// row of the frame, last_value the last, nth_value the n-th (counting NULLs
// unless IGNORE NULLS is given).  A value tree answers each query in O(log n).
func evalValueItem(it *WindowItem, pc *partitionContext, fd *frameData, out *table.Table, col int) error {
	name := strings.ToLower(it.Call.Name)
	vals, err := evalArgColumn(it.Call.Args[0], pc)
	if err != nil {
		return err
	}
	ignoreNull := it.Call.IgnoreNulls
	var tree *valueTree
	if ignoreNull || fd.excl != ExcludeNoOthers || name == "nth_value" {
		tree = newValueTree(vals, ignoreNull)
	}
	var nExpr Expr
	if name == "nth_value" {
		nExpr = it.Call.Args[1]
	}
	for i := 0; i < pc.m; i++ {
		s, e := fd.frames[i][0], fd.frames[i][1]
		if s > e {
			out.Rows[pc.pt.order[pc.base+i]][col] = value.Null
			continue
		}
		var res value.Value
		switch name {
		case "first_value":
			if fd.excl == ExcludeNoOthers && !ignoreNull {
				res = vals[s]
				break
			}
			res = tree.firstIn(fd, i, s, e)
		case "last_value":
			if fd.excl == ExcludeNoOthers && !ignoreNull {
				res = vals[e]
				break
			}
			res = tree.lastIn(fd, i, s, e)
		case "nth_value":
			nv, err := nExpr.Eval(pc.rows[pc.pt.order[pc.base+i]])
			if err != nil {
				return err
			}
			if nv.IsNull() {
				res = value.Null
				break
			}
			n, err := value.AsInt64(nv)
			if err != nil {
				return semErrf("argument of nth_value must be an integer")
			}
			if n <= 0 {
				return semErrf("argument of nth_value must be greater than zero")
			}
			res = tree.nthExcluding(fd, i, s, e, int(n))
		}
		out.Rows[pc.pt.order[pc.base+i]][col] = res
	}
	return nil
}

// valueTree is an order-statistics tree over a partition's argument values.
// Each node stores how many selectable positions it contains: non-null
// positions when ignoreNulls is set, every position otherwise.
type valueTree struct {
	vals       []value.Value
	ignoreNull bool
	n          int
	size       int
	cnt        []int64
}

func newValueTree(vals []value.Value, ignoreNull bool) *valueTree {
	size := 1
	for size < len(vals) {
		size <<= 1
	}
	if size == 0 {
		size = 1
	}
	t := &valueTree{vals: vals, ignoreNull: ignoreNull, n: len(vals), size: size, cnt: make([]int64, size*2)}
	for i, v := range vals {
		if ignoreNull && v.IsNull() {
			continue
		}
		t.cnt[size+i] = 1
	}
	for i := size - 1; i >= 1; i-- {
		t.cnt[i] = t.cnt[i*2] + t.cnt[i*2+1]
	}
	return t
}

// last returns the last row of the frame (or last non-null with ignoreNulls).
func (t *valueTree) last(l, r int) value.Value {
	if l < 0 {
		l = 0
	}
	if r >= t.n {
		r = t.n - 1
	}
	if l > r {
		return value.Null
	}
	if !t.ignoreNull {
		return t.vals[r]
	}
	c := t.count(l, r)
	if c == 0 {
		return value.Null
	}
	return t.nth(l, r, c)
}

// count returns the number of selectable positions in [l, r].
func (t *valueTree) count(l, r int) int {
	return int(t.rangeCount(l, r))
}

func (t *valueTree) rangeCount(l, r int) int64 {
	if l < 0 {
		l = 0
	}
	if r >= t.n {
		r = t.n - 1
	}
	if l > r {
		return 0
	}
	var rec func(node, nl, nr int) int64
	rec = func(node, nl, nr int) int64 {
		if r < nl || nr < l {
			return 0
		}
		if l <= nl && nr <= r {
			return t.cnt[node]
		}
		mid := (nl + nr) / 2
		return rec(node*2, nl, mid) + rec(node*2+1, mid+1, nr)
	}
	return rec(1, 0, t.size-1)
}

// nth returns the value at the k-th selectable position of [l, r], or NULL.
func (t *valueTree) nth(l, r, k int) value.Value {
	if l < 0 {
		l = 0
	}
	if r >= t.n {
		r = t.n - 1
	}
	if l > r || k <= 0 {
		return value.Null
	}
	before := t.rangeCount(0, l-1)
	// Must not select past r.
	if t.rangeCount(0, r)-before < int64(k) {
		return value.Null
	}
	target := before + int64(k)
	pos := t.selectGlobal(target)
	if pos < 0 || pos > r {
		return value.Null
	}
	return t.vals[pos]
}

// selectGlobal returns the position of the target-th (1-based) selectable
// element in the whole tree, or -1.
func (t *valueTree) selectGlobal(target int64) int {
	if target <= 0 || target > t.cnt[1] {
		return -1
	}
	node := 1
	lo, hi := 0, t.size-1
	for node < t.size {
		mid := (lo + hi) / 2
		left := node * 2
		if t.cnt[left] >= target {
			node = left
			hi = mid
		} else {
			target -= t.cnt[left]
			node = left + 1
			lo = mid + 1
		}
	}
	if lo >= t.n {
		return -1
	}
	return lo
}

// frameIntervals returns the frame of row i as disjoint position intervals
// after applying the EXCLUDE clause.  EXCLUDE removes at most the current row
// and one contiguous peer group, so this is a small interval list.
func frameIntervals(fd *frameData, i, s, e int) [][2]int {
	if s > e {
		return nil
	}
	switch fd.excl {
	case ExcludeCurrentRow:
		if i < s || i > e {
			return [][2]int{{s, e}}
		}
		out := make([][2]int, 0, 2)
		if s <= i-1 {
			out = append(out, [2]int{s, i - 1})
		}
		if i+1 <= e {
			out = append(out, [2]int{i + 1, e})
		}
		return out
	case ExcludeGroup:
		gs, ge := fd.gStarts[fd.groupOf[i]], fd.gEnds[fd.groupOf[i]]
		lo, hi := maxInt(gs, s), minInt(ge, e)
		if lo > hi {
			return [][2]int{{s, e}}
		}
		out := make([][2]int, 0, 2)
		if s <= lo-1 {
			out = append(out, [2]int{s, lo - 1})
		}
		if hi+1 <= e {
			out = append(out, [2]int{hi + 1, e})
		}
		return out
	case ExcludeTies:
		gs, ge := fd.gStarts[fd.groupOf[i]], fd.gEnds[fd.groupOf[i]]
		lo, hi := maxInt(gs, s), minInt(ge, e)
		if lo > hi {
			return [][2]int{{s, e}}
		}
		out := make([][2]int, 0, 3)
		if s <= lo-1 {
			out = append(out, [2]int{s, lo - 1})
		}
		if i >= lo && i <= hi {
			out = append(out, [2]int{i, i})
		}
		if hi+1 <= e {
			out = append(out, [2]int{hi + 1, e})
		}
		return out
	}
	return [][2]int{{s, e}}
}

// countIntervals counts selectable positions in the interval list.
func (t *valueTree) countIntervals(ivs [][2]int) int {
	n := 0
	for _, iv := range ivs {
		n += t.count(iv[0], iv[1])
	}
	return n
}

// kthIntervals returns the k-th selectable value across the interval list.
func (t *valueTree) kthIntervals(ivs [][2]int, k int) value.Value {
	if k <= 0 {
		return value.Null
	}
	for _, iv := range ivs {
		c := t.count(iv[0], iv[1])
		if k > c {
			k -= c
			continue
		}
		return t.nth(iv[0], iv[1], k)
	}
	return value.Null
}

// firstIn returns the first selectable value of row i's frame.
func (t *valueTree) firstIn(fd *frameData, i, s, e int) value.Value {
	return t.kthIntervals(frameIntervals(fd, i, s, e), 1)
}

// lastIn returns the last selectable value of row i's frame.
func (t *valueTree) lastIn(fd *frameData, i, s, e int) value.Value {
	ivs := frameIntervals(fd, i, s, e)
	c := t.countIntervals(ivs)
	if c == 0 {
		return value.Null
	}
	return t.kthIntervals(ivs, c)
}

func (t *valueTree) nthExcluding(fd *frameData, i, s, e, k int) value.Value {
	return t.kthIntervals(frameIntervals(fd, i, s, e), k)
}

// selectablePositions retained as a no-op placeholder for API clarity.
func selectablePositions(fd *frameData, ignoreNull bool, vals []value.Value) []int { return nil }
