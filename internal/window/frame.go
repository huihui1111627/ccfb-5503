package window

import (
	"fmt"
	"time"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/types"
)

// Frame 描述一行在排序后数组里的帧范围（排除规则应用前）。
type Frame struct {
	Lo, Hi int // 半开区间 [Lo, Hi)，下标基于排序后的分区局部坐标
	Empty  bool
}

// frameResolver 持有一个窗口在一个分区上的全部帧计算上下文。
// 分区长度为 plen，所有下标都是分区局部坐标（0..plen-1）。
type frameResolver struct {
	plen       int
	unit       string // rows | range | groups
	orderKeys  []SortKey
	peerStart  []int
	peerEnd    []int
	offsetEval map[expr.Node]*Evaluated // 动态偏移表达式 -> 整列预求值（原始行坐标）
	perm       []int                    // 局部位置 -> 原始行号
	groupCache []int                    // GROUPS 模式：peer 组起点（懒计算一次）
	groupOf    []int                    // GROUPS 模式：每个位置所属组号（懒计算一次）
}

// resolveFrame 计算分区内位置 k（0-based）的帧。
// spec 已经过 resolve 阶段补全：无显式帧时由引擎填默认帧。
func (fr *frameResolver) resolveFrame(k int, spec *expr.FrameSpec) (Frame, error) {
	start, end := normalizeDefaults(spec, len(fr.orderKeys) > 0)

	// RANGE 模式下，排序键为 NULL 的行遇到「带偏移」边界时，NULL 不参与数值/
	// 间隔比较，该边界退化为只覆盖 NULL peer 组（PostgreSQL 实测语义）。
	// 注意：UNBOUNDED / CURRENT ROW 边界不走这里——默认帧仍从分区起点延伸到
	// NULL peer 组，因此 NULL 行也能看到前面的非空值。
	if fr.unit == "range" && len(fr.orderKeys) > 0 && frameHasOffsetBound(spec) {
		if cur := fr.orderKeys[0].Eval.Vals[fr.perm[k]]; cur == nil {
			return Frame{Lo: fr.peerStart[k], Hi: fr.peerEnd[k]}, nil
		}
	}

	lo, err := fr.bound(k, start, true)
	if err != nil {
		return Frame{}, err
	}
	hi, err := fr.bound(k, end, false)
	if err != nil {
		return Frame{}, err
	}
	if lo > hi {
		// 帧起点落在终点之后 => 空帧（PostgreSQL 行为）。
		return Frame{Lo: lo, Hi: hi, Empty: true}, nil
	}
	return Frame{Lo: lo, Hi: hi}, nil
}

// normalizeDefaults 把单边界简写补成 BETWEEN 形式，并实现 PostgreSQL 默认帧：
// 有 ORDER BY 时 RANGE UNBOUNDED PRECEDING TO CURRENT ROW（含 peer）；
// 无 ORDER BY 时整个分区为一 peer，两种默认帧等价于全分区。
func normalizeDefaults(spec *expr.FrameSpec, hasOrder bool) (expr.FrameBound, expr.FrameBound) {
	curRow := expr.FrameBound{Kind: "current row"}
	if spec == nil {
		return expr.FrameBound{Kind: "unbounded preceding"}, curRow
	}
	start := spec.Start
	end := curRow
	if spec.End != nil {
		end = *spec.End
	}
	_ = hasOrder
	return start, end
}

// bound 计算一个边界在分区局部坐标中的位置。
// isStart=true 时返回闭区间起点，isStart=false 时返回半开区间终点。
func (fr *frameResolver) bound(k int, b expr.FrameBound, isStart bool) (int, error) {
	switch b.Kind {
	case "unbounded preceding":
		return 0, nil
	case "unbounded following":
		return fr.plen, nil
	case "current row":
		return fr.currentRowBound(k, isStart), nil
	case "preceding", "following":
		off, err := fr.evalOffset(k, b)
		if err != nil {
			return 0, err
		}
		switch fr.unit {
		case "rows":
			return fr.rowsBound(k, b.Kind, off, isStart)
		case "groups":
			return fr.groupsBound(k, b.Kind, off, isStart)
		case "range":
			return fr.rangeBound(k, b.Kind, off, isStart)
		}
	}
	return 0, fmt.Errorf("unknown frame bound kind %q", b.Kind)
}

func (fr *frameResolver) currentRowBound(k int, isStart bool) int {
	switch fr.unit {
	case "rows":
		if isStart {
			return k
		}
		return k + 1
	case "range", "groups":
		// CURRENT ROW 在 RANGE/GROUPS 下覆盖整个 peer 组。
		if isStart {
			return fr.peerStart[k]
		}
		return fr.peerEnd[k]
	}
	return k
}

// evalOffset 取当前行的偏移表达式值并做类型/取值校验。
func (fr *frameResolver) evalOffset(k int, b expr.FrameBound) (any, error) {
	rawRow := fr.perm[k]
	ev := fr.offsetEval[b.Offset]
	if ev == nil {
		// 常量表达式也在引擎层统一预求值，理论不会走到这里。
		return nil, fmt.Errorf("internal error: frame offset not evaluated")
	}
	v := ev.Vals[rawRow]
	if v == nil {
		return nil, fmt.Errorf("frame offset must not evaluate to NULL")
	}
	if iv, ok := v.(int64); ok && iv < 0 {
		return nil, fmt.Errorf("frame offset must not be negative")
	}
	if fv, ok := v.(float64); ok && fv < 0 {
		return nil, fmt.Errorf("frame offset must not be negative")
	}
	if iv, ok := v.(types.Interval); ok && iv.Micros < 0 {
		return nil, fmt.Errorf("frame offset must not be negative")
	}
	return v, nil
}

// ---- ROWS：按物理行号偏移 --------------------------------------------------

func (fr *frameResolver) rowsBound(k int, dir string, off any, isStart bool) (int, error) {
	n, err := asIntOffset(off)
	if err != nil {
		return 0, err
	}
	var t int
	if dir == "preceding" {
		t = k - int(n)
	} else {
		t = k + int(n)
	}
	if isStart {
		// 闭区间起点：越界钉在分区边界。
		if t < 0 {
			t = 0
		}
		if t > fr.plen {
			t = fr.plen
		}
		return t, nil
	}
	// 半开区间终点：指向目标行的下一个位置；越界钉在分区两端。
	if t >= fr.plen {
		return fr.plen, nil
	}
	if t < 0 {
		t = 0
	}
	return t + 1, nil
}

func asIntOffset(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		// PostgreSQL ROWS/GROUPS 只接受 integer；浮点直接报错。
		return 0, fmt.Errorf("ROWS/GROUPS frame offset must be integer, got double precision")
	}
	return 0, fmt.Errorf("ROWS/GROUPS frame offset must be integer, got %s", types.TypeName(v))
}

// ---- GROUPS：按 peer 组号偏移 ----------------------------------------------

func (fr *frameResolver) groupsBound(k int, dir string, off any, isStart bool) (int, error) {
	n, err := asIntOffset(off)
	if err != nil {
		return 0, err
	}
	// 组起点与组号在整个分区上只计算一次（原实现每行重算导致 O(n²)）。
	groupStarts := fr.ensureGroups()
	curGroup := fr.groupOf[k]
	var g int
	if dir == "preceding" {
		g = curGroup - int(n)
	} else {
		g = curGroup + int(n)
	}
	if g < 0 {
		g = 0
	}
	if g >= len(groupStarts) {
		return fr.plen, nil
	}
	if isStart {
		return groupStarts[g], nil
	}
	// 终点：该组的 peerEnd。
	return fr.peerEnd[groupStarts[g]], nil
}

// ensureGroups 懒计算并缓存分区内的 peer 组起点与每位置的组号。O(plen) 一次。
func (fr *frameResolver) ensureGroups() []int {
	if fr.groupCache != nil {
		return fr.groupCache
	}
	starts := make([]int, 0)
	groupOf := make([]int, fr.plen)
	gi := -1
	for p := 0; p < fr.plen; p++ {
		if fr.peerStart[p] == p {
			starts = append(starts, p)
			gi++
		}
		groupOf[p] = gi
	}
	fr.groupCache = starts
	fr.groupOf = groupOf
	return starts
}

// groupStarts（保留兼容名，内部走缓存）。
func (fr *frameResolver) groupStarts() []int {
	return fr.ensureGroups()
}

// ---- RANGE：按排序键值偏移 -------------------------------------------------

func (fr *frameResolver) rangeBound(k int, dir string, off any, isStart bool) (int, error) {
	if len(fr.orderKeys) == 0 {
		return 0, fmt.Errorf("RANGE with offset requires an ORDER BY clause")
	}
	key := fr.orderKeys[0]
	curVal := key.Eval.Vals[fr.perm[k]]
	// 排序键为 NULL：PostgreSQL 中 NULL 行的偏移帧只覆盖其 peer 组。
	if curVal == nil {
		if isStart {
			return fr.peerStart[k], nil
		}
		return fr.peerEnd[k], nil
	}

	switch x := off.(type) {
	case types.Interval:
		return fr.rangeTime(k, dir, x.Micros, isStart, key)
	case int64, float64:
		return fr.rangeNumeric(k, dir, off, isStart, key)
	default:
		return 0, fmt.Errorf("RANGE offset type %s is incompatible with ORDER BY column", types.TypeName(off))
	}
}

func (fr *frameResolver) rangeTime(k int, dir string, micros int64, isStart bool, key SortKey) (int, error) {
	cur := key.Eval.Vals[fr.perm[k]]
	ct, ok := cur.(time.Time)
	if !ok {
		return 0, fmt.Errorf("RANGE with INTERVAL offset requires a timestamp ORDER BY column")
	}
	// 自然值空间目标：preceding 在 ASC 下为更早（更小）时刻，DESC 下反过来。
	delta := micros
	if (dir == "preceding") == !key.Desc {
		delta = -micros
	}
	target := ct.Add(microsToDur(delta))
	return fr.binarySearchValue(k, target, isStart, key, dir)
}

func microsToDur(micros int64) time.Duration {
	return time.Duration(micros) * time.Microsecond
}

func (fr *frameResolver) rangeNumeric(k int, dir string, off any, isStart bool, key SortKey) (int, error) {
	cur := key.Eval.Vals[fr.perm[k]]
	cf, err := types.ToFloat(cur)
	if err != nil {
		return 0, fmt.Errorf("RANGE numeric offset requires numeric ORDER BY column, got %s", types.TypeName(cur))
	}
	of, err := types.ToFloat(off)
	if err != nil {
		return 0, err
	}
	// 自然值空间目标：
	//   ASC  preceding -> cur-off ; following -> cur+off
	//   DESC preceding -> cur+off ; following -> cur-off
	// 即「preceding 与 DESC 同时出现或同时不出现」时取减号。
	if (dir == "preceding") == !key.Desc {
		of = -of
	}
	target := cf + of
	return fr.binarySearchValue(k, target, isStart, key, dir)
}

// binarySearchValue 在已排序的分区里定位「值满足帧边界条件」的位置。
//
// target 始终是自然值空间里的目标值（方向翻转已在调用处完成），比较器内部
// 按 ASC/DESC 翻转，因此在「有向序列」上看到的总是单调非降序列。
//
//	起始边界（PRECEDING/FOLLOWING 的起点）：有向序列中第一个 >= target 的位置
//	终点边界（半开）：有向序列中第一个 >  target 的位置
//
// NULL 全部集中在序列一端，永不与数值/时间目标匹配，搜索时整体跳过。
func (fr *frameResolver) binarySearchValue(k int, target any, isStart bool, key SortKey, dir string) (int, error) {
	lo, hi := fr.nonNullSpan(key)
	if lo >= hi {
		// 分区里全是 NULL：退化为当前 peer。
		if isStart {
			return fr.peerStart[k], nil
		}
		return fr.peerEnd[k], nil
	}

	// cmpAt: 排序位置 p 的值与 target 的有向比较（已计入 ASC/DESC）。
	cmpAt := func(p int) int {
		v := key.Eval.Vals[fr.perm[p]]
		c, err := types.Compare(v, target)
		if err != nil {
			panic(fmt.Sprintf("range compare: %v", err))
		}
		if key.Desc {
			c = -c
		}
		return c
	}

	if isStart {
		// 第一个 cmpAt >= 0。
		return clamp(lowerBound(lo, hi, func(p int) bool { return cmpAt(p) >= 0 }), 0, fr.plen), nil
	}
	// 第一个 cmpAt > 0（半开终点）。
	return clamp(lowerBound(lo, hi, func(p int) bool { return cmpAt(p) > 0 }), 0, fr.plen), nil
}

// nonNullSpan 返回非 NULL 值在排序序列里的连续区间 [lo,hi)。
// NULLS FIRST 时非空段在右，NULLS LAST 时在左；全空时 lo==hi。
func (fr *frameResolver) nonNullSpan(key SortKey) (int, int) {
	nullsFirst := effectiveNulls(expr.OrderItem{Desc: key.Desc, Nulls: key.Nulls}) == "first"
	if nullsFirst {
		lo := 0
		for lo < fr.plen && key.Eval.Vals[fr.perm[lo]] == nil {
			lo++
		}
		return lo, fr.plen
	}
	hi := fr.plen
	for hi > 0 && key.Eval.Vals[fr.perm[hi-1]] == nil {
		hi--
	}
	return 0, hi
}

// lowerBound 二分：返回 [lo,hi) 内第一个 pred(p)=true 的位置；全真返回 hi。
func lowerBound(lo, hi int, pred func(int) bool) int {
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if pred(mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// frameHasOffsetBound 判断帧规格里是否含带偏移量（PRECEDING/FOLLOWING 带表达式）
// 的边界。仅这种边界在 NULL 排序键下需要 NULL peer 退化处理。
func frameHasOffsetBound(spec *expr.FrameSpec) bool {
	if spec == nil {
		return false
	}
	if spec.Start.Kind == "preceding" || spec.Start.Kind == "following" {
		return true
	}
	if spec.End != nil && (spec.End.Kind == "preceding" || spec.End.Kind == "following") {
		return true
	}
	return false
}
