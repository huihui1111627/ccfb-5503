package window

import (
	"math"
	"sort"
	"time"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
)

// sqrtMath 集中 math.Sqrt 入口，引擎与参考实现共用，保证方差结果逐位一致。
func sqrtMath(f float64) float64 { return math.Sqrt(f) }

// Evaluated 是一个表达式在整张（已过滤）表上预求值的结果列。
type Evaluated struct {
	Vals []any
}

// EvalColumn 在输入表上求一个表达式的值，返回物理列形式。
// 所有分区/排序/帧偏移表达式都先整列求值，后续引擎只做数组访问。
func EvalColumn(t *table.Table, n expr.Node, colMap map[*expr.ColumnRef]int) (*Evaluated, error) {
	out := &Evaluated{Vals: make([]any, t.N)}
	ctx := &expr.RowContext{
		ColMap: colMap,
		Vals:   func(col, row int) any { return t.Columns[col].Vals[row] },
	}
	for r := 0; r < t.N; r++ {
		ctx.Row = r
		v, err := expr.Eval(n, ctx)
		if err != nil {
			return nil, err
		}
		out.Vals[r] = v
	}
	return out, nil
}

// EvalFilter 求布尔过滤表达式，NULL 与 false 都视作不通过（SQL 语义）。
func EvalFilter(t *table.Table, n expr.Node, colMap map[*expr.ColumnRef]int) ([]bool, error) {
	if n == nil {
		return nil, nil
	}
	out := make([]bool, t.N)
	ctx := &expr.RowContext{
		ColMap: colMap,
		Vals:   func(col, row int) any { return t.Columns[col].Vals[row] },
	}
	for r := 0; r < t.N; r++ {
		ctx.Row = r
		v, err := expr.Eval(n, ctx)
		if err != nil {
			return nil, err
		}
		out[r], _ = v.(bool)
	}
	return out, nil
}

// SortKey 是一列排序键。
type SortKey struct {
	Eval  *Evaluated
	Desc  bool
	Nulls string // first | last
}

// SortedPerm 持有一个窗口规格对应的排序排列与分区/并列信息。
type SortedPerm struct {
	Perm      []int  // 排序后第 k 个位置对应的原行号
	PartStart []int  // 每个分区在排序数组中的起始下标，长度=分区数+1（末尾=n）
	PeerStart []int  // 排序后每个位置所在 peer 组的起始下标
	PeerEnd   []int  // 排序后每个位置所在 peer 组的结束下标（排他）
	PartOf    []int  // 排序后每个位置所属分区序号
	SortSig   string // 共享排序的指纹（分区键+排序键的规范化文本）
	Orders    []expr.OrderItem
	PartExprs []expr.Node
	Keys      []SortKey // 含求值列，供排名/帧计算复用
	PartEvals []*Evaluated
}

// sortSignature 计算「同一次排序」的指纹：分区表达式 + 排序项完全相同
// （含 ASC/DESC、NULLS FIRST/LAST）的窗口共用一次排序。
func sortSignature(part []expr.Node, orders []expr.OrderItem) string {
	h := ""
	for _, p := range part {
		h += "P:" + expr.NodeString(p) + ";"
	}
	for _, o := range orders {
		d := "A"
		if o.Desc {
			d = "D"
		}
		h += "O:" + expr.NodeString(o.X) + ":" + d + ":" + o.Nulls + ";"
	}
	return h
}

// SortPartition 对整张过滤后的表做稳定排序并切分区、计算 peer 组。
// 多个窗口共用同一份 SortSignature 时只调用一次（引擎层有缓存）。
func SortPartition(nRows int, partEvals, orderEvals []*Evaluated, orders []expr.OrderItem, partExprs []expr.Node) *SortedPerm {
	perm := make([]int, nRows)
	for i := range perm {
		perm[i] = i
	}

	keys := make([]SortKey, len(orders))
	for i, o := range orders {
		keys[i] = SortKey{
			Eval:  orderEvals[i],
			Desc:  o.Desc,
			Nulls: effectiveNulls(o),
		}
	}

	sort.SliceStable(perm, func(i, j int) bool {
		a, b := perm[i], perm[j]
		// 先按分区键（始终升序，NULLS LAST 风格的分区相等性不需要方向）。
		for _, pe := range partEvals {
			if c := compareAscNulls(pe.Vals[a], pe.Vals[b]); c != 0 {
				return c < 0
			}
		}
		// 再按每个排序键。
		for _, k := range keys {
			if c := compareOrderKey(k.Eval.Vals[a], k.Eval.Vals[b], k); c != 0 {
				return c < 0
			}
		}
		// 完全相等时稳定排序保持原行序。
		return false
	})

	sp := &SortedPerm{
		Perm:      perm,
		PeerStart: make([]int, nRows),
		PeerEnd:   make([]int, nRows),
		PartOf:    make([]int, nRows),
		SortSig:   sortSignature(partExprs, orders),
		Orders:    orders,
		PartExprs: partExprs,
		Keys:      keys,
		PartEvals: partEvals,
	}

	// 分区边界 + peer 组，一次线性扫描完成。
	// PeerStart/PeerEnd 存「分区局部坐标」：每个分区从 0 重新计数，frameResolver
	// 与参考实现都在分区局部坐标里索引它们。PartStart 仍是全表排序坐标。
	partStart := []int{0}
	partID := 0
	peerBegin := 0 // 全表坐标：当前 peer 组起点
	for k := 0; k < nRows; k++ {
		if k > 0 {
			diffPart := partitionChanged(partEvals, perm[k-1], perm[k])
			if diffPart {
				// 收尾上一个 peer 组（局部坐标 = 全表坐标减去分区起点）。
				ps := partStart[partID]
				for p := peerBegin; p < k; p++ {
					sp.PeerEnd[p] = k - ps
					sp.PeerStart[p] = peerBegin - ps
				}
				partStart = append(partStart, k)
				partID++
				peerBegin = k
			} else if !peerEqual(keys, perm[k-1], perm[k]) {
				// 同分区、排序键不再相等 -> 新 peer 组。
				ps := partStart[partID]
				for p := peerBegin; p < k; p++ {
					sp.PeerEnd[p] = k - ps
					sp.PeerStart[p] = peerBegin - ps
				}
				peerBegin = k
			}
		}
		sp.PartOf[k] = partID
		// 当前行的 peer 起点（局部坐标），分区起点在下方回填。
		sp.PeerStart[k] = peerBegin - partStart[partID]
	}
	// 收尾最后一个 peer 组与分区。
	ps := partStart[partID]
	for p := peerBegin; p < nRows; p++ {
		sp.PeerEnd[p] = nRows - ps
		sp.PeerStart[p] = peerBegin - ps
	}
	partStart = append(partStart, nRows)
	sp.PartStart = partStart
	return sp
}

// effectiveNulls 返回排序项实际生效的 NULL 位置：
// PostgreSQL 默认 ASC -> NULLS LAST，DESC -> NULLS FIRST。
func effectiveNulls(o expr.OrderItem) string {
	if o.Nulls != "" {
		return o.Nulls
	}
	if o.Desc {
		return "first"
	}
	return "last"
}

// compareOrderKey 按单个排序键比较两个原始行号对应的值。
func compareOrderKey(a, b any, k SortKey) int {
	nullCmp := 0
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		nullCmp = -1
	} else if b == nil {
		nullCmp = 1
	}
	if nullCmp != 0 {
		if k.Nulls == "first" {
			return nullCmp // NULL 排前面：a 为 NULL 时 a<b
		}
		return -nullCmp
	}
	c, err := types.Compare(a, b)
	if err != nil {
		// 不可比较的类型在数据生成侧已规避；出现时按文本兜底，保持确定性。
		sa, sb := types.ToText(a), types.ToText(b)
		if sa < sb {
			c = -1
		} else if sa > sb {
			c = 1
		}
	}
	if k.Desc {
		return -c
	}
	return c
}

// compareAscNulls 分区键比较：NULL 彼此相等，NULL 小于非 NULL（仅用于分区排序，
// 分区只要求「相等的聚到一起」，方向不影响分区结果）。
func compareAscNulls(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	c, err := types.Compare(a, b)
	if err != nil {
		sa, sb := types.ToText(a), types.ToText(b)
		if sa < sb {
			return -1
		} else if sa > sb {
			return 1
		}
		return 0
	}
	return c
}

// peerEqual 判断两个原始行在给定排序键下是否为 peer（排序键全等，含 NULL 相等）。
func peerEqual(keys []SortKey, rowA, rowB int) bool {
	for _, k := range keys {
		a, b := k.Eval.Vals[rowA], k.Eval.Vals[rowB]
		if a == nil || b == nil {
			if a != b {
				return false
			}
			continue
		}
		if c, _ := types.Compare(a, b); c != 0 {
			return false
		}
	}
	return true
}

// partitionChanged NULL 感知地判断两个原始行的分区键是否不同。
func partitionChanged(partEvals []*Evaluated, rowA, rowB int) bool {
	for _, pe := range partEvals {
		a, b := pe.Vals[rowA], pe.Vals[rowB]
		if a == nil || b == nil {
			if a != b {
				return true
			}
			continue
		}
		if c, _ := types.Compare(a, b); c != 0 {
			return true
		}
	}
	return false
}

// timeToMicros 时间戳转微秒，供 RANGE 间隔偏移使用。
func timeToMicros(t time.Time) int64 { return t.UTC().UnixMicro() }
