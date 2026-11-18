package window

import (
	"fmt"
	"strings"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/types"
)

// aggBuildSpec 是聚合建树的内部规格。
type aggBuildSpec struct {
	Func   string
	Kind   aggKind
	ArgIdx int    // 在 localVals 中的下标；count(*) 时为 -1
	Delim  string // string_agg 分隔符
}

func classifyAggregate(wc *expr.WindowCall, args []*Evaluated, filter []bool) aggBuildSpec {
	fn := wc.Func // 已小写
	spec := aggBuildSpec{Func: fn, ArgIdx: 0}
	if wc.Star {
		spec.Kind = aggCount
		spec.ArgIdx = -1
		return spec
	}
	switch fn {
	case "count":
		spec.Kind = aggCount
	case "sum", "avg":
		// 整数输入走 int64 状态，浮点输入走 Rat 状态；avg 在 int 状态上做除法。
		spec.Kind = aggSumInt
		if args[0].isFloat() {
			spec.Kind = aggSumFloat
		}
		if fn == "avg" {
			// avg 复用 sum 状态，最终按 N 做除法（输出时处理）。
		}
	case "min":
		spec.Kind = aggMin
	case "max":
		spec.Kind = aggMax
	case "stddev_pop", "stddev_samp", "var_pop", "var_samp":
		spec.Kind = aggVar
	case "string_agg":
		spec.Kind = aggStringAgg
		spec.Delim = ","
		if len(wc.Args) >= 2 {
			// 分隔符在编译期求值：要求常量文本。
			if d, ok := wc.Args[1].(*expr.Literal); ok {
				if s, ok2 := d.Value.(string); ok2 {
					spec.Delim = s
				}
			}
		}
	default:
		panic(fmt.Sprintf("aggregate %s not classified", fn))
	}
	return spec
}

// isFloat 判断求值列是否含浮点（有任一非空 float64 即按浮点聚合；
// 数据生成与查询生成保证同列类型一致，混合时提升为浮点）。
func (ev *Evaluated) isFloat() bool {
	for _, v := range ev.Vals {
		if _, ok := v.(float64); ok {
			return true
		}
	}
	return false
}

func buildAggTree(spec aggBuildSpec, localVals [][]any, filter []bool, plen int) (*builtTree, error) {
	bt := &builtTree{kind: spec.Kind, delim: spec.Delim}
	n := plen
	switch spec.Kind {
	case aggCount:
		// count(*) 只受 FILTER 影响；count(列) 还要排除参数为 NULL 的行。
		cntFilter := filter
		if spec.ArgIdx >= 0 {
			cntFilter = combineNullFilter(pickVals(spec, localVals), filter)
		}
		bt.cnt = buildCountTree(cntFilter, n)
	case aggSumInt:
		vals := pickVals(spec, localVals)
		if err := checkNumeric(vals, spec.Func); err != nil {
			return nil, err
		}
		bt.sumi = buildSumIntTree(vals, filter)
	case aggSumFloat:
		vals := pickVals(spec, localVals)
		if err := checkNumeric(vals, spec.Func); err != nil {
			return nil, err
		}
		bt.sumf = buildSumFloatTree(vals, filter)
	case aggMin, aggMax:
		vals := pickVals(spec, localVals)
		bt.mm = buildMinMaxTree(spec.Kind == aggMax, vals, filter)
	case aggVar:
		vals := pickVals(spec, localVals)
		if err := checkNumeric(vals, spec.Func); err != nil {
			return nil, err
		}
		bt.vr = buildVarTree(vals, filter)
	case aggStringAgg:
		vals := pickVals(spec, localVals)
		if err := checkText(vals, spec.Func); err != nil {
			return nil, err
		}
		bt.sa = buildStringAggTree(vals, filter)
		// 保存有序叶子，并预计算共享前缀缓冲：所有有效字符串依次追加进同一个
		// builder，记录每位置的累计字符串。累计帧（起点=分区起点）可直接取用，
		// 避免每行重拼整个帧导致的 O(n²)。
		bt.strVals = make([]string, len(vals))
		bt.strKeep = make([]bool, len(vals))
		var buf strings.Builder
		bt.strPrefix = make([]string, len(vals)+1)
		bt.strPrefixCount = make([]int, len(vals)+1)
		count := 0
		for i, v := range vals {
			if v != nil && (filter == nil || filter[i]) {
				s := v.(string)
				bt.strVals[i] = s
				bt.strKeep[i] = true
				if count > 0 {
					buf.WriteString(spec.Delim)
				}
				buf.WriteString(s)
				count++
			}
			// Builder.String() 共享底层字节，多次取前缀不产生量级拷贝。
			bt.strPrefix[i+1] = buf.String()
			bt.strPrefixCount[i+1] = count
		}
	}
	return bt, nil
}

func pickVals(spec aggBuildSpec, localVals [][]any) []any {
	if spec.ArgIdx < 0 {
		return nil
	}
	return localVals[spec.ArgIdx]
}

// combineNullFilter 把「参数非 NULL」与 FILTER 谓词合并成一个布尔掩码。
func combineNullFilter(argVals []any, filter []bool) []bool {
	out := make([]bool, len(argVals))
	for i, v := range argVals {
		if v == nil {
			continue
		}
		if filter == nil || filter[i] {
			out[i] = true
		}
	}
	return out
}

func checkNumeric(vals []any, fn string) error {
	for _, v := range vals {
		if v == nil {
			continue
		}
		if !types.IsNumeric(v) {
			return fmt.Errorf("function %s requires numeric argument, got %s", fn, types.TypeName(v))
		}
	}
	return nil
}

func checkText(vals []any, fn string) error {
	for _, v := range vals {
		if v == nil {
			continue
		}
		if _, ok := v.(string); !ok {
			return fmt.Errorf("function %s requires text argument, got %s", fn, types.TypeName(v))
		}
	}
	return nil
}

// queryAggTree 在一组（EXCLUDE 后）区间上查询聚合，多区间按从左到右合并，
// 与逐行扫描帧的折叠顺序一致。
func queryAggTree(bt *builtTree, spec aggBuildSpec, ranges [][2]int) any {
	if len(ranges) == 0 {
		return emptyAggResult(spec)
	}
	switch spec.Kind {
	case aggCount:
		var n int64
		for _, rg := range ranges {
			n += bt.cnt.Query(rg[0], rg[1]).N
		}
		return n
	case aggSumInt:
		acc := sumIntMonoid{}.Identity()
		for _, rg := range ranges {
			acc = sumIntMonoid{}.Combine(acc, bt.sumi.Query(rg[0], rg[1]))
		}
		if acc.N == 0 {
			return nil
		}
		if spec.Func == "avg" {
			return float64(acc.Sum) / float64(acc.N)
		}
		return acc.Sum
	case aggSumFloat:
		acc := sumFloatMonoid{}.Identity()
		for _, rg := range ranges {
			acc = sumFloatMonoid{}.Combine(acc, bt.sumf.Query(rg[0], rg[1]))
		}
		if acc.N == 0 {
			return nil
		}
		f, _ := acc.Sum.Float64()
		if spec.Func == "avg" {
			return f / float64(acc.N)
		}
		return f
	case aggMin, aggMax:
		acc := bt.mm.Query(ranges[0][0], ranges[0][1])
		m := minMaxMonoid{Max: spec.Kind == aggMax}
		for _, rg := range ranges[1:] {
			acc = m.Combine(acc, bt.mm.Query(rg[0], rg[1]))
		}
		if !acc.Has {
			return nil
		}
		return acc.V
	case aggVar:
		acc := varMonoid{}.Identity()
		for _, rg := range ranges {
			acc = varMonoid{}.Combine(acc, bt.vr.Query(rg[0], rg[1]))
		}
		return finishVariance(spec.Func, acc)
	case aggStringAgg:
		// 累计帧（单段且从分区起点 0 开始）：直接取预计算前缀，O(1)。
		// 这是默认帧/UNBOUNDED PRECEDING 的常见形态，也是 O(n²) 热点所在。
		if len(ranges) == 1 && ranges[0][0] == 0 {
			end := ranges[0][1]
			if bt.strPrefixCount[end] == 0 {
				return nil
			}
			return bt.strPrefix[end]
		}
		// 滑动帧或 EXCLUDE 拆出的多段：区间内有效叶子做一次有序拼接。
		var b strings.Builder
		first := true
		for _, rg := range ranges {
			for p := rg[0]; p < rg[1]; p++ {
				if !bt.strKeep[p] {
					continue
				}
				if !first {
					b.WriteString(spec.Delim)
				}
				b.WriteString(bt.strVals[p])
				first = false
			}
		}
		if first {
			return nil // 帧内无有效字符串
		}
		return b.String()
	}
	return nil
}

// emptyAggResult 空帧（或全被 EXCLUDE）时的结果：计数为 0，其余为 NULL。
func emptyAggResult(spec aggBuildSpec) any {
	if spec.Kind == aggCount {
		return int64(0)
	}
	return nil
}

// ---- 导航函数参数 ----------------------------------------------------------

func buildNavSpec(wc *expr.WindowCall, args []*Evaluated) (NavSpec, error) {
	nav := NavSpec{Func: wc.Func}
	switch wc.Func {
	case "lag", "lead":
		if len(args) < 1 {
			return nav, fmt.Errorf("%s requires at least 1 argument", wc.Func)
		}
		nav.Arg = args[0]
		if len(args) >= 2 {
			nav.Offset = args[1]
		}
		if len(args) >= 3 {
			nav.Default = args[2]
		}
		nav.IgnoreNull = wc.IgnoreNulls
	case "first_value", "last_value":
		if len(args) != 1 {
			return nav, fmt.Errorf("%s requires 1 argument", wc.Func)
		}
		nav.Arg = args[0]
		nav.IgnoreNull = wc.IgnoreNulls
	case "nth_value":
		if len(args) != 2 {
			return nav, fmt.Errorf("nth_value requires 2 arguments")
		}
		nav.Arg = args[0]
		nav.Nth = args[1]
		nav.IgnoreNull = wc.IgnoreNulls
	default:
		return nav, fmt.Errorf("unknown navigation function %s", wc.Func)
	}
	return nav, nil
}
