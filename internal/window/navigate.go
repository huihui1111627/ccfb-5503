package window

import (
	"fmt"

	"dragonwindow/internal/expr"
)

// NavSpec 描述一个导航类窗口函数调用。
type NavSpec struct {
	Func       string // lag | lead | first_value | last_value | nth_value
	Arg        *Evaluated
	Offset     *Evaluated // lag/lead 的偏移表达式（每行各自求值）；nil 用默认 1
	Default    *Evaluated // 越界默认值；nil 即 SQL NULL
	IgnoreNull bool
	Nth        *Evaluated // nth_value 的 N 表达式
}

// evalNavigation 在一个分区上计算导航函数，返回长度 plen 的结果（分区局部坐标）。
// 帧与 EXCLUDE 对 first_value/last_value/nth_value 生效；lag/lead 只看分区和排序。
func evalNavigation(plen int, sp *SortedPerm, fr *frameResolver, frameSpec *expr.FrameSpec, nav NavSpec) ([]any, error) {
	out := make([]any, plen)
	perm := sp.Perm
	argAt := func(localPos int) any {
		return nav.Arg.Vals[perm[localPos]]
	}

	switch nav.Func {
	case "lag", "lead":
		backward := nav.Func == "lag"
		for k := 0; k < plen; k++ {
			step := int64(1)
			if nav.Offset != nil {
				v := nav.Offset.Vals[perm[k]]
				if v == nil {
					return nil, fmt.Errorf("LAG/LEAD offset must not be NULL")
				}
				n, err := toInt64(v)
				if err != nil {
					return nil, err
				}
				if n < 0 {
					return nil, fmt.Errorf("LAG/LEAD offset must not be negative")
				}
				step = n
			}
			target := navFind(plen, k, step, backward, nav.IgnoreNull, argAt)
			if target < 0 {
				// 越界：返回默认值表达式在当前行上的值（未给即 NULL）。
				if nav.Default != nil {
					out[k] = nav.Default.Vals[perm[k]]
				} else {
					out[k] = nil
				}
				continue
			}
			out[k] = argAt(target)
		}
		return out, nil

	case "first_value", "last_value", "nth_value":
		for k := 0; k < plen; k++ {
			f, err := fr.resolveFrame(k, frameSpec)
			if err != nil {
				return nil, err
			}
			ranges := effectiveRanges(f, k, sp.PeerStart, sp.PeerEnd, frameSpec.Exclude)
			out[k] = navFramePick(nav, ranges, argAt, perm[k])
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown navigation function %s", nav.Func)
}

// navFind 从位置 k 出发，按方向跳过 IGNORE NULLS 后找第 step 个「计数行」。
// 找不到返回 -1。RESPECT NULLS 模式下空值也计入步数。
func navFind(plen, k int, step int64, backward, ignoreNull bool, argAt func(int) any) int {
	if step == 0 {
		return k
	}
	dir := 1
	if backward {
		dir = -1
	}
	pos := k
	var counted int64
	for counted < step {
		pos += dir
		if pos < 0 || pos >= plen {
			return -1
		}
		if ignoreNull && argAt(pos) == nil {
			continue
		}
		counted++
	}
	return pos
}

// navFramePick 在帧（已应用 EXCLUDE 的区间集合）里取 first/last/nth 值。
// nth_value 的 N 从 1 开始，只统计帧内「非排除」行；IGNORE NULLS 时跳过空值。
// curRawRow 是当前排序位置对应的原始行号，用于对 N 表达式逐行求值。
func navFramePick(nav NavSpec, ranges [][2]int, argAt func(int) any, curRawRow int) any {
	switch nav.Func {
	case "first_value":
		for _, rg := range ranges {
			for p := rg[0]; p < rg[1]; p++ {
				v := argAt(p)
				if nav.IgnoreNull && v == nil {
					continue
				}
				return v
			}
		}
		return nil
	case "last_value":
		for i := len(ranges) - 1; i >= 0; i-- {
			rg := ranges[i]
			for p := rg[1] - 1; p >= rg[0]; p-- {
				v := argAt(p)
				if nav.IgnoreNull && v == nil {
					continue
				}
				return v
			}
		}
		return nil
	case "nth_value":
		// N 在当前行求值（允许是表达式，每行各自计算）。
		nAny := nav.Nth.Vals[curRawRow]
		if nAny == nil {
			return nil
		}
		n, err := toInt64(nAny)
		if err != nil {
			panic(fmt.Sprintf("nth_value: %v", err))
		}
		if n <= 0 {
			panic("nth_value offset must be positive")
		}
		var seen int64
		for _, rg := range ranges {
			for p := rg[0]; p < rg[1]; p++ {
				v := argAt(p)
				if nav.IgnoreNull && v == nil {
					continue
				}
				seen++
				if seen == n {
					return v
				}
			}
		}
		return nil
	}
	return nil
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		// PostgreSQL 严格要求 integer；浮点拒绝，避免静默截断造成与库不一致。
		return 0, fmt.Errorf("offset must be integer, got double precision")
	default:
		return 0, fmt.Errorf("offset must be integer, got %T", v)
	}
}
