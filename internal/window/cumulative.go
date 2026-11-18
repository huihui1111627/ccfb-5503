package window

import (
	"dragonwindow/internal/expr"
)

// isCumulativeFrame 判断帧是否为「累计帧」：起点 UNBOUNDED PRECEDING、终点
// CURRENT ROW，且没有 EXCLUDE。这类帧随当前行单调扩张，可走线性前缀快速路径。
func isCumulativeFrame(f *expr.FrameSpec) bool {
	if f == nil || f.Exclude != "" {
		return false
	}
	if f.Start.Kind != "unbounded preceding" {
		return false
	}
	if f.End == nil {
		return true // 单边界简写等价于 ... TO CURRENT ROW
	}
	return f.End.Kind == "current row"
}

// runCumulative 用线性前缀计算累计帧。
//
// 关键正确性点：
//   - ROWS 模式 CURRENT ROW 就是当前行，帧 = [0, k+1)，逐点推进；
//   - RANGE/GROUPS 模式 CURRENT ROW 覆盖整个 peer 组，同组所有行帧终点相同，
//     按 peer 组边界推进并复用结果。
func runCumulative(bt *builtTree, spec aggBuildSpec, plen int, peerStart, peerEnd, perm []int, result []any, unit string) {
	switch bt.kind {
	case aggCount, aggSumInt, aggSumFloat, aggMin, aggMax, aggVar:
		if unit == "rows" {
			for k := 0; k < plen; k++ {
				result[perm[k]] = queryAggTree(bt, spec, [][2]int{{0, k + 1}})
			}
		} else {
			k := 0
			for k < plen {
				groupEnd := peerEnd[k]
				v := queryAggTree(bt, spec, [][2]int{{0, groupEnd}})
				for p := k; p < groupEnd; p++ {
					result[perm[p]] = v
				}
				k = groupEnd
			}
		}
	case aggStringAgg:
		if unit == "rows" {
			for k := 0; k < plen; k++ {
				if bt.strPrefixCount[k+1] == 0 {
					result[perm[k]] = nil
				} else {
					result[perm[k]] = bt.strPrefix[k+1]
				}
			}
		} else {
			k := 0
			for k < plen {
				groupEnd := peerEnd[k]
				var v any
				if bt.strPrefixCount[groupEnd] != 0 {
					v = bt.strPrefix[groupEnd]
				}
				for p := k; p < groupEnd; p++ {
					result[perm[p]] = v
				}
				k = groupEnd
			}
		}
	}
}
