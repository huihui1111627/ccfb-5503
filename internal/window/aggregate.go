package window

import (
	"fmt"
	"math/big"
	"strings"

	"dragonwindow/internal/types"
	"dragonwindow/internal/window/segtree"
)

// 本文件把每个聚合函数建模成线段树可用的幺半群。
//
// 关键正确性决策：浮点求和（SUM/AVG/方差）的中间状态用 big.Rat 保存。
// Rat 的加法是精确结合运算，因此无论线段树按什么顺序归并，最终分子分母
// 完全相同；最后在输出点做唯一一次 float64 转换，引擎与暴力参考实现可以
// 逐位相等。整数求和走 int64，时间/文本极值走可空标量。

// ---- COUNT ---------------------------------------------------------------

type countState struct{ N int64 }

type countMonoid struct{}

func (countMonoid) Identity() countState { return countState{} }
func (countMonoid) Combine(a, b countState) countState {
	return countState{N: a.N + b.N}
}

// ---- SUM / AVG：整数 ------------------------------------------------------

type sumIntState struct {
	N   int64 // 非空行数
	Sum int64
}

type sumIntMonoid struct{}

func (sumIntMonoid) Identity() sumIntState { return sumIntState{} }
func (sumIntMonoid) Combine(a, b sumIntState) sumIntState {
	return sumIntState{N: a.N + b.N, Sum: a.Sum + b.Sum}
}

// ---- SUM / AVG：浮点（Rat 精确累加）---------------------------------------

type sumFloatState struct {
	N   int64
	Sum *big.Rat // nil 视为 0
}

type sumFloatMonoid struct{}

func (sumFloatMonoid) Identity() sumFloatState { return sumFloatState{} }
func (sumFloatMonoid) Combine(a, b sumFloatState) sumFloatState {
	out := sumFloatState{N: a.N + b.N}
	switch {
	case a.Sum == nil:
		out.Sum = b.Sum
	case b.Sum == nil:
		out.Sum = a.Sum
	default:
		out.Sum = new(big.Rat).Add(a.Sum, b.Sum)
	}
	return out
}

// ---- 极值 -----------------------------------------------------------------

type minMaxState struct {
	Has bool
	V   any
	Max bool // false=MIN, true=MAX
}

type minMaxMonoid struct{ Max bool }

func (m minMaxMonoid) Identity() minMaxState { return minMaxState{Max: m.Max} }
func (m minMaxMonoid) Combine(a, b minMaxState) minMaxState {
	if !a.Has {
		return b
	}
	if !b.Has {
		return a
	}
	c, err := types.Compare(a.V, b.V)
	if err != nil {
		// 构造阶段类型已校验，这里不应发生。
		panic(fmt.Sprintf("minmax combine: %v", err))
	}
	win := a
	if (m.Max && c < 0) || (!m.Max && c > 0) {
		win = b
	}
	return minMaxState{Has: true, V: win.V, Max: m.Max}
}

// ---- 方差/标准差：Welford 统计量的可结合形式 -------------------------------
//
// 保存 (n, mean, M2)，合并公式取自 Chan/Golub/LeVeque (1979)：
//
//	delta = meanB - meanA
//	mean  = meanA + delta * nB / n
//	M2    = M2A + M2B + delta^2 * nA*nB/n
//
// mean 与 M2 用 big.Rat 保持精确；最终样本/总体方差只差一个除数。
type varState struct {
	N    int64
	Mean *big.Rat
	M2   *big.Rat
}

type varMonoid struct{}

func (varMonoid) Identity() varState { return varState{} }

func (varMonoid) Combine(a, b varState) varState {
	if a.N == 0 {
		return b
	}
	if b.N == 0 {
		return a
	}
	n := a.N + b.N
	delta := new(big.Rat).Sub(b.Mean, a.Mean)
	nA := big.NewRat(a.N, 1)
	nB := big.NewRat(b.N, 1)
	nTot := big.NewRat(n, 1)

	mean := new(big.Rat).Mul(delta, new(big.Rat).Quo(nB, nTot))
	mean.Add(mean, a.Mean)

	m2 := new(big.Rat).Add(a.M2, b.M2)
	corr := new(big.Rat).Mul(delta, delta)
	corr.Mul(corr, new(big.Rat).Quo(new(big.Rat).Mul(nA, nB), nTot))
	m2.Add(m2, corr)
	return varState{N: n, Mean: mean, M2: m2}
}

// ---- STRING_AGG：有序字符串拼接 -------------------------------------------
//
// 字符串拼接不适合线段树的可结合状态：用 []string 做 Combine 会在 O(n log n)
// 个节点上反复搬运切片，退化为近似 O(n²) 的内存拷贝。这里改用「叶子字符串数组
// + 前缀分隔偏移」：建树只保存分区局部的有序字符串与一个计数树，帧查询时对
// 实际命中的区间做一次 strings.Builder 拼接，复杂度 O(帧长)，无中间切片。
type stringAggState struct {
	// 仅用于满足线段树接口；真正的字符串放在 builtTree.strVals。
	Parts []string
}

type stringAggMonoid struct{}

func (stringAggMonoid) Identity() stringAggState { return stringAggState{} }
func (stringAggMonoid) Combine(a, b stringAggState) stringAggState {
	if len(a.Parts) == 0 {
		return b
	}
	if len(b.Parts) == 0 {
		return a
	}
	merged := make([]string, 0, len(a.Parts)+len(b.Parts))
	merged = append(merged, a.Parts...)
	merged = append(merged, b.Parts...)
	return stringAggState{Parts: merged}
}

// ---- 聚合规格：把函数名 + 输入列映射到具体的树与结果提取 --------------------

// aggKind 是聚合的内部分类。
type aggKind int

const (
	aggCount aggKind = iota
	aggSumInt
	aggSumFloat
	aggMin
	aggMax
	aggVar
	aggStringAgg
)

// AggSpec 描述一个待计算的聚合窗口列。
type AggSpec struct {
	Func    string // 小写
	Kind    aggKind
	ArgExpr Evaluated // 已预求值的参数列（nil 表示 count(*)）
	Filter  []bool    // 长度=过滤后的表行数；nil 表示无 FILTER
	// STRING_AGG 的分隔符（默认逗号，引擎子集固定不支持帧内 ORDER BY）。
	Delim string
}

// builtTrees 持有一个聚合在一个分区上的线段树及取值逻辑。
// 不同状态类型用 any 承载，查询热路径在具体函数内做类型断言。
type builtTree struct {
	kind  aggKind
	delim string
	cnt   *segtree.Tree[countState]
	sumi  *segtree.Tree[sumIntState]
	sumf  *segtree.Tree[sumFloatState]
	mm    *segtree.Tree[minMaxState]
	vr    *segtree.Tree[varState]
	sa    *segtree.Tree[stringAggState]
	// strVals 是分区局部、按排序顺序排列的字符串叶子（string_agg 专用）。
	strVals []string
	// strKeep 标记每个局部位置是否计入 string_agg（非 NULL 且过 FILTER）。
	strKeep []bool
	// strPrefix[i] 是有效叶子在 [0,i) 范围内用 delim 连接的累计字符串；
	// strPrefixCount[i] 是该范围内的有效字符串数。累计帧可 O(1) 取用。
	strPrefix      []string
	strPrefixCount []int
}

func (bt *builtTree) query(l, r int) any {
	switch bt.kind {
	case aggCount:
		return bt.cnt.Query(l, r).N
	case aggSumInt:
		st := bt.sumi.Query(l, r)
		if st.N == 0 {
			return nil
		}
		return st.Sum
	case aggSumFloat:
		st := bt.sumf.Query(l, r)
		if st.N == 0 {
			return nil
		}
		f, _ := st.Sum.Float64()
		return f
	case aggMin, aggMax:
		st := bt.mm.Query(l, r)
		if !st.Has {
			return nil
		}
		return st.V
	case aggVar:
		st := bt.vr.Query(l, r)
		return st // 由 finishVariance 按函数名提取
	case aggStringAgg:
		st := bt.sa.Query(l, r)
		if len(st.Parts) == 0 {
			return nil
		}
		return strings.Join(st.Parts, bt.delim)
	}
	return nil
}

// finishVariance 把 varState 转成四个方差类函数之一的结果。
func finishVariance(fn string, st varState) any {
	if st.N == 0 {
		return nil
	}
	switch fn {
	case "avg":
		f, _ := st.Mean.Float64()
		return f
	case "var_samp", "stddev_samp":
		if st.N < 2 {
			return nil
		}
		v := new(big.Rat).Quo(st.M2, big.NewRat(st.N-1, 1))
		f, _ := v.Float64()
		if fn == "stddev_samp" {
			return sqrtFloat(f)
		}
		return f
	case "var_pop", "stddev_pop":
		v := new(big.Rat).Quo(st.M2, big.NewRat(st.N, 1))
		f, _ := v.Float64()
		if fn == "stddev_pop" {
			return sqrtFloat(f)
		}
		return f
	}
	return nil
}

func sqrtFloat(f float64) float64 {
	// math.Sqrt 是确定性的内置运算，引擎与参考实现都在输出点调用同一份代码。
	return sqrtMath(f)
}

// ---- 建树：把一列值转成叶子状态 -------------------------------------------

func buildCountTree(filter []bool, n int) *segtree.Tree[countState] {
	leaves := make([]countState, n)
	for i := 0; i < n; i++ {
		if filter == nil || filter[i] {
			leaves[i] = countState{N: 1}
		}
	}
	return segtree.Build(countMonoid{}, leaves)
}

func buildSumIntTree(vals []any, filter []bool) *segtree.Tree[sumIntState] {
	leaves := make([]sumIntState, len(vals))
	for i, v := range vals {
		if v == nil || (filter != nil && !filter[i]) {
			continue
		}
		leaves[i] = sumIntState{N: 1, Sum: v.(int64)}
	}
	return segtree.Build(sumIntMonoid{}, leaves)
}

func buildSumFloatTree(vals []any, filter []bool) *segtree.Tree[sumFloatState] {
	leaves := make([]sumFloatState, len(vals))
	for i, v := range vals {
		if v == nil || (filter != nil && !filter[i]) {
			continue
		}
		f, _ := types.ToFloat(v)
		leaves[i] = sumFloatState{N: 1, Sum: new(big.Rat).SetFloat64(f)}
	}
	return segtree.Build(sumFloatMonoid{}, leaves)
}

func buildMinMaxTree(max bool, vals []any, filter []bool) *segtree.Tree[minMaxState] {
	m := minMaxMonoid{Max: max}
	leaves := make([]minMaxState, len(vals))
	for i, v := range vals {
		if v == nil || (filter != nil && !filter[i]) {
			leaves[i] = m.Identity()
			continue
		}
		leaves[i] = minMaxState{Has: true, V: v, Max: max}
	}
	return segtree.Build(m, leaves)
}

func buildVarTree(vals []any, filter []bool) *segtree.Tree[varState] {
	leaves := make([]varState, len(vals))
	for i, v := range vals {
		if v == nil || (filter != nil && !filter[i]) {
			continue
		}
		f, _ := types.ToFloat(v)
		leaves[i] = varState{N: 1, Mean: big.NewRat(0, 1).SetFloat64(f), M2: big.NewRat(0, 1)}
	}
	return segtree.Build(varMonoid{}, leaves)
}

func buildStringAggTree(vals []any, filter []bool) *segtree.Tree[stringAggState] {
	leaves := make([]stringAggState, len(vals))
	for i, v := range vals {
		if v == nil || (filter != nil && !filter[i]) {
			continue
		}
		leaves[i] = stringAggState{Parts: []string{v.(string)}}
	}
	return segtree.Build(stringAggMonoid{}, leaves)
}
