// Package ref 是窗口语义的「逐行暴力扫描」参考实现。
//
// 与生产引擎刻意保持代码路径独立：
//   - 排序/表达式求值复用 window 包的基础能力（这两部分不涉及帧语义）；
//   - 帧边界完全按 SQL 定义在排序后的分区里逐行判断，不调用 frameResolver；
//   - 聚合对帧内成员逐行左折叠，不使用线段树；
//   - EXCLUDE 逐行判定成员资格。
//
// 只要该实现与引擎在几十万行随机查询上逐值一致，就同时验证了帧边界、
// 排除规则、线段树归并顺序与聚合状态的正确性。
package ref

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
	"dragonwindow/internal/window"
)

// Run 用暴力方式执行查询，返回与 window.Engine.Execute 同构的结果列集合。
// 为便于逐行比对，这里直接返回「过滤后表 + 每个窗口调用的结果列（按过滤后
// 原始行号对齐）」，不做最终投影；投影逻辑两侧共用、无需重复验证。
func Run(in *table.Table, stmt *expr.SelectStmt) (*table.Table, [][]any, error) {
	binder := expr.NewBinder(&schemaAdapter{t: in})
	calls, err := binder.BindSelect(stmt)
	if err != nil {
		return nil, nil, err
	}
	colMap := binder.ColumnMap()

	filter, err := window.EvalFilter(in, stmt.Where, colMap)
	if err != nil {
		return nil, nil, err
	}
	ft := filterRows(in, filter)

	// 在过滤后表上重新绑定（列下标不变）。
	results := make([][]any, len(calls))
	for i, wc := range calls {
		res, err := computeOne(ft, wc, colMap)
		if err != nil {
			return nil, nil, fmt.Errorf("ref window %d (%s): %w", i, wc.Func, err)
		}
		results[i] = res
	}
	return ft, results, nil
}

func computeOne(t *table.Table, wc *expr.WindowCall, colMap map[*expr.ColumnRef]int) ([]any, error) {
	spec := wc.Over
	// 预求值分区/排序。
	partEvals := make([]*window.Evaluated, len(spec.Partition))
	for i, p := range spec.Partition {
		ev, err := window.EvalColumn(t, p, colMap)
		if err != nil {
			return nil, err
		}
		partEvals[i] = ev
	}
	orderEvals := make([]*window.Evaluated, len(spec.OrderBy))
	for i, o := range spec.OrderBy {
		ev, err := window.EvalColumn(t, o.X, colMap)
		if err != nil {
			return nil, err
		}
		orderEvals[i] = ev
	}
	sp := window.SortPartition(t.N, partEvals, orderEvals, spec.OrderBy, spec.Partition)

	argEvals := make([]*window.Evaluated, len(wc.Args))
	for i, a := range wc.Args {
		ev, err := window.EvalColumn(t, a, colMap)
		if err != nil {
			return nil, err
		}
		argEvals[i] = ev
	}
	filterBools, err := window.EvalFilter(t, wc.Filter, colMap)
	if err != nil {
		return nil, err
	}
	frameOff, err := preEvalOffsets(t, spec, colMap)
	if err != nil {
		return nil, err
	}

	result := make([]any, t.N)
	frameSpec := effectiveFrame(spec)

	for pi := 0; pi < len(sp.PartStart)-1; pi++ {
		ps, pe := sp.PartStart[pi], sp.PartStart[pi+1]
		plen := pe - ps
		perm := sp.Perm[ps:pe]
		// PeerStart/PeerEnd 已在 SortPartition 阶段按分区局部坐标存储。
		pStart := sp.PeerStart[ps:pe]
		pEnd := sp.PeerEnd[ps:pe]

		if expr.IsAggregate(wc.Func) || isFrameNav(wc.Func) {
			for k := 0; k < plen; k++ {
				members, err := bruteFrameMembers(plen, k, perm, sp, pStart, pEnd, frameSpec, frameOff, t, colMap)
				if err != nil {
					return nil, err
				}
				if expr.IsAggregate(wc.Func) {
					result[perm[k]] = bruteAggregate(wc, argEvals, filterBools, perm, members)
				} else {
					v, err := bruteNavFrame(wc, argEvals, perm, members, k)
					if err != nil {
						return nil, err
					}
					result[perm[k]] = v
				}
			}
		} else if isRank(wc.Func) {
			if err := bruteRank(wc, argEvals, sp.Keys, perm, plen, result); err != nil {
				return nil, err
			}
		} else {
			if err := bruteLagLead(wc, argEvals, sp.Keys, perm, plen, result); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func isRank(fn string) bool {
	switch fn {
	case "row_number", "rank", "dense_rank", "percent_rank", "cume_dist", "ntile":
		return true
	}
	return false
}

func isFrameNav(fn string) bool {
	switch fn {
	case "first_value", "last_value", "nth_value":
		return true
	}
	return false
}

// ---- 独立帧成员计算 -------------------------------------------------------

// bruteFrameMembers 返回当前行帧内、应用 EXCLUDE 后的成员局部下标列表（升序）。
func bruteFrameMembers(
	plen, k int,
	perm []int,
	sp *window.SortedPerm,
	peerStart, peerEnd []int,
	frame *expr.FrameSpec,
	off map[expr.Node]*window.Evaluated,
	t *table.Table,
	colMap map[*expr.ColumnRef]int,
) ([]int, error) {
	// RANGE 模式下排序键为 NULL、且帧含「带偏移」边界时：NULL 不参与偏移比较，
	// 帧退化为只覆盖 NULL peer 组。默认帧（UNBOUNDED..CURRENT ROW）不走此路径，
	// NULL 行仍能看到分区起点到 NULL 段的非空值（与 PostgreSQL 实测一致）。
	if frame.Unit == "range" && len(sp.Keys) > 0 && refFrameHasOffset(frame) {
		if cur := sp.Keys[0].Eval.Vals[perm[k]]; cur == nil {
			var members []int
			for p := peerStart[k]; p < peerEnd[k]; p++ {
				if !excluded(frame.Exclude, p, k, peerStart, peerEnd) {
					members = append(members, p)
				}
			}
			return members, nil
		}
	}
	startLo, _, err := bruteBound(plen, k, perm, sp, peerStart, peerEnd, frame, true, off, t, colMap)
	if err != nil {
		return nil, err
	}
	_, endHi, err := bruteBound(plen, k, perm, sp, peerStart, peerEnd, frame, false, off, t, colMap)
	if err != nil {
		return nil, err
	}
	// 起点取闭区间下界，终点取半开上界；起点超过终点即空帧。
	if startLo > endHi {
		return nil, nil
	}
	var members []int
	for p := startLo; p < endHi; p++ {
		if !excluded(frame.Exclude, p, k, peerStart, peerEnd) {
			members = append(members, p)
		}
	}
	return members, nil
}

// refFrameHasOffset 判断帧是否含带表达式偏移的边界。
func refFrameHasOffset(f *expr.FrameSpec) bool {
	if f.Start.Kind == "preceding" || f.Start.Kind == "following" {
		return true
	}
	if f.End != nil && (f.End.Kind == "preceding" || f.End.Kind == "following") {
		return true
	}
	return false
}

// excluded 按 EXCLUDE 规则判定局部位置 p 是否被排除。
func excluded(rule string, p, k int, peerStart, peerEnd []int) bool {
	switch rule {
	case "", "no others":
		return false
	case "current row":
		return p == k
	case "group":
		return p >= peerStart[k] && p < peerEnd[k]
	case "ties":
		return p >= peerStart[k] && p < peerEnd[k] && p != k
	}
	return false
}

// bruteBound 独立计算一个边界。返回半开区间坐标（起点也返回其闭区间位置，
// 调用方对起点直接用作包含下标、终点用作排他下标）。
func bruteBound(
	plen, k int,
	perm []int,
	sp *window.SortedPerm,
	peerStart, peerEnd []int,
	frame *expr.FrameSpec,
	isStart bool,
	off map[expr.Node]*window.Evaluated,
	t *table.Table,
	colMap map[*expr.ColumnRef]int,
) (int, int, error) {
	var b expr.FrameBound
	start, end := frame.Start, frame.End
	if isStart {
		b = start
	} else {
		if end == nil {
			b = expr.FrameBound{Kind: "current row"}
		} else {
			b = *end
		}
	}
	switch b.Kind {
	case "unbounded preceding":
		return 0, plen, nil
	case "unbounded following":
		return plen, plen, nil
	case "current row":
		if frame.Unit == "rows" {
			if isStart {
				return k, k + 1, nil
			}
			return k, k + 1, nil
		}
		return peerStart[k], peerEnd[k], nil
	}

	// 带偏移的边界：先取当前行的偏移值。
	rawRow := perm[k]
	ev := off[b.Offset]
	if ev == nil {
		return 0, 0, fmt.Errorf("internal: offset not evaluated")
	}
	offVal := ev.Vals[rawRow]
	if offVal == nil {
		return 0, 0, fmt.Errorf("frame offset must not evaluate to NULL")
	}

	switch frame.Unit {
	case "rows":
		n, err := asInt(offVal)
		if err != nil {
			return 0, 0, err
		}
		var t0 int
		if b.Kind == "preceding" {
			t0 = k - int(n)
		} else {
			t0 = k + int(n)
		}
		if isStart {
			if t0 < 0 {
				t0 = 0
			}
			if t0 > plen {
				t0 = plen
			}
			return t0, t0 + 1, nil
		}
		if t0 >= plen {
			return plen, plen, nil
		}
		if t0 < 0 {
			t0 = 0
		}
		return t0, t0 + 1, nil
	case "groups":
		n, err := asInt(offVal)
		if err != nil {
			return 0, 0, err
		}
		curGroup := 0
		var starts []int
		for p := 0; p < plen; p++ {
			if peerStart[p] == p {
				starts = append(starts, p)
			}
			if p <= k {
				curGroup = len(starts) - 1
			}
		}
		var g int
		if b.Kind == "preceding" {
			g = curGroup - int(n)
		} else {
			g = curGroup + int(n)
		}
		if g < 0 {
			g = 0
		}
		if g >= len(starts) {
			return plen, plen, nil
		}
		gs := starts[g]
		ge := peerEnd[gs]
		if isStart {
			return gs, ge, nil
		}
		return gs, ge, nil
	case "range":
		return bruteRange(plen, k, perm, sp, peerStart, peerEnd, b, offVal, isStart)
	}
	return 0, 0, fmt.Errorf("unknown bound %s", b.Kind)
}

// bruteRange 逐行扫描整个分区判定 RANGE 成员资格，完全不依赖二分。
func bruteRange(
	plen, k int,
	perm []int,
	sp *window.SortedPerm,
	peerStart, peerEnd []int,
	b expr.FrameBound,
	offVal any,
	isStart bool,
) (int, int, error) {
	if len(sp.Keys) == 0 {
		return 0, 0, fmt.Errorf("RANGE with offset requires ORDER BY")
	}
	key := sp.Keys[0]
	curVal := key.Eval.Vals[perm[k]]
	if curVal == nil {
		// NULL 行：帧只含其 peer 组（起止都指向 peer 边界）。
		return peerStart[k], peerEnd[k], nil
	}
	// 计算自然值空间目标。
	target, err := rangeTarget(curVal, offVal, b.Kind, key.Desc)
	if err != nil {
		return 0, 0, err
	}
	// 逐行扫描：收集满足有向比较条件的位置，取最小/最大。
	lo, hi := -1, -1
	for p := 0; p < plen; p++ {
		v := key.Eval.Vals[perm[p]]
		if v == nil {
			continue
		}
		c, err := types.Compare(v, target)
		if err != nil {
			return 0, 0, err
		}
		if key.Desc {
			c = -c
		}
		ok := false
		if isStart {
			ok = c >= 0 // 起点：第一个 >= target
		} else {
			ok = c <= 0 // 终点：最后一个 <= target
		}
		if ok {
			if lo < 0 {
				lo = p
			}
			hi = p + 1
		}
	}
	if lo < 0 {
		// 无任何行满足条件：返回空区间信号 [0,0)，调用方结合另一端判断为空帧。
		return 0, 0, nil
	}
	return lo, hi, nil
}

// rangeTarget 计算自然值空间的目标值。
func rangeTarget(cur, off any, dir string, desc bool) (any, error) {
	if iv, ok := off.(types.Interval); ok {
		ct, ok := cur.(time.Time)
		if !ok {
			return nil, fmt.Errorf("INTERVAL offset requires timestamp ORDER BY")
		}
		delta := iv.Micros
		// 与引擎一致：preceding 与 DESC 同真或同假时取减号。
		if (dir == "preceding") == !desc {
			delta = -delta
		}
		return ct.Add(time.Duration(delta) * time.Microsecond), nil
	}
	cf, err := types.ToFloat(cur)
	if err != nil {
		return nil, fmt.Errorf("numeric RANGE requires numeric ORDER BY: %w", err)
	}
	of, err := types.ToFloat(off)
	if err != nil {
		return nil, err
	}
	if (dir == "preceding") == !desc {
		of = -of
	}
	return cf + of, nil
}

// ---- 暴力聚合（逐行左折叠）------------------------------------------------

func bruteAggregate(wc *expr.WindowCall, args []*window.Evaluated, filter []bool, perm, members []int) any {
	fn := wc.Func
	if fn == "count" {
		var n int64
		for _, p := range members {
			raw := perm[p]
			if wc.Star {
				n++
				continue
			}
			if args[0].Vals[raw] != nil && (filter == nil || filter[raw]) {
				n++
			}
		}
		return n
	}
	// 收集有效参数值（非 NULL 且过 FILTER），保持帧内顺序。
	var vals []any
	for _, p := range members {
		raw := perm[p]
		v := args[0].Vals[raw]
		if v == nil || (filter != nil && !filter[raw]) {
			continue
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return emptyFor(fn)
	}
	switch fn {
	case "sum":
		return foldSum(vals)
	case "avg":
		s := foldSum(vals)
		switch x := s.(type) {
		case int64:
			return float64(x) / float64(len(vals))
		case float64:
			return x / float64(len(vals))
		}
	case "min":
		best := vals[0]
		for _, v := range vals[1:] {
			if c, _ := types.Compare(v, best); c < 0 {
				best = v
			}
		}
		return best
	case "max":
		best := vals[0]
		for _, v := range vals[1:] {
			if c, _ := types.Compare(v, best); c > 0 {
				best = v
			}
		}
		return best
	case "stddev_pop", "stddev_samp", "var_pop", "var_samp":
		return foldVariance(fn, vals)
	case "string_agg":
		delim := ","
		if len(wc.Args) >= 2 {
			if lit, ok := wc.Args[1].(*expr.Literal); ok {
				if s, ok2 := lit.Value.(string); ok2 {
					delim = s
				}
			}
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v.(string)
		}
		return strings.Join(parts, delim)
	}
	return nil
}

// foldSum 与引擎的状态类型严格对应：纯整数返回 int64，含浮点用 big.Rat 精确
// 求和后转 float64，保证两条路径位级一致。
func foldSum(vals []any) any {
	allInt := true
	for _, v := range vals {
		if _, ok := v.(int64); !ok {
			allInt = false
			break
		}
	}
	if allInt {
		var s int64
		for _, v := range vals {
			s += v.(int64)
		}
		return s
	}
	acc := new(big.Rat)
	for _, v := range vals {
		f, _ := types.ToFloat(v)
		acc.Add(acc, new(big.Rat).SetFloat64(f))
	}
	out, _ := acc.Float64()
	return out
}

func foldVariance(fn string, vals []any) any {
	n := int64(len(vals))
	mean := new(big.Rat)
	for _, v := range vals {
		f, _ := types.ToFloat(v)
		mean.Add(mean, new(big.Rat).SetFloat64(f))
	}
	mean.Quo(mean, big.NewRat(n, 1))
	m2 := new(big.Rat)
	for _, v := range vals {
		f, _ := types.ToFloat(v)
		d := new(big.Rat).Sub(new(big.Rat).SetFloat64(f), mean)
		m2.Add(m2, new(big.Rat).Mul(d, d))
	}
	switch fn {
	case "var_pop":
		r := new(big.Rat).Quo(m2, big.NewRat(n, 1))
		out, _ := r.Float64()
		return out
	case "var_samp":
		if n < 2 {
			return nil
		}
		r := new(big.Rat).Quo(m2, big.NewRat(n-1, 1))
		out, _ := r.Float64()
		return out
	case "stddev_pop":
		r := new(big.Rat).Quo(m2, big.NewRat(n, 1))
		out, _ := r.Float64()
		return math.Sqrt(out)
	case "stddev_samp":
		if n < 2 {
			return nil
		}
		r := new(big.Rat).Quo(m2, big.NewRat(n-1, 1))
		out, _ := r.Float64()
		return math.Sqrt(out)
	}
	return nil
}

func emptyFor(fn string) any {
	if fn == "count" {
		return int64(0)
	}
	return nil
}
