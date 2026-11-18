package window

import "fmt"

// effectiveRanges 应用 EXCLUDE 后，把帧拆成至多三个有序子区间（排序坐标）。
// 线段树分别查询再按顺序合并，聚合结果与逐行暴力扫描一致。
func effectiveRanges(f Frame, k int, peerStart, peerEnd []int, exclude string) [][2]int {
	if f.Empty {
		return nil
	}
	lo, hi := f.Lo, f.Hi
	if lo >= hi {
		return nil
	}
	if exclude == "" || exclude == "no others" {
		if exclude == "no others" {
			// EXCLUDE NO OTHERS：显式声明不排除任何行，与缺省等价。
			return [][2]int{{lo, hi}}
		}
		return [][2]int{{lo, hi}}
	}

	// 计算需要排除的连续段（EXCLUDE 的各种情况都只产生一个连续洞）。
	var exLo, exHi int
	switch exclude {
	case "current row":
		exLo, exHi = k, k+1
	case "group":
		exLo, exHi = peerStart[k], peerEnd[k]
	case "ties":
		// EXCLUDE TIES：排除 peer 组内「除当前行外」的并列行，保留当前行。
		// peer 组范围 [peerStart[k], peerEnd[k])，从中挖掉当前行 k，
		// 帧内 peer 组之外的行不受影响。拆成最多三段：
		//   [lo, peerStart)        帧内、peer 组之前
		//   [k, k+1)               当前行（保留）
		//   [peerEnd, hi)          帧内、peer 组之后
		gLo, gHi := peerStart[k], peerEnd[k]
		var out [][2]int
		if lo < gLo {
			out = append(out, [2]int{lo, gLo})
		}
		// 当前行（仅当它落在帧内；正常帧定义下恒成立）。
		if k >= lo && k+1 <= hi {
			out = append(out, [2]int{k, k + 1})
		}
		if gHi < hi {
			out = append(out, [2]int{gHi, hi})
		}
		return out
	default:
		return [][2]int{{lo, hi}}
	}

	// current row / group：从 [lo,hi) 里挖掉 [exLo,exHi)。
	var out [][2]int
	if lo < exLo {
		out = append(out, [2]int{lo, exLo})
	}
	if exHi < hi {
		out = append(out, [2]int{exHi, hi})
	}
	return out
}

// ---- 排名类函数（与帧无关，按分区/排序一次性计算）--------------------------

// rankResults 在一个分区的排序坐标上计算全部排名函数，返回按函数名索引的值。
func rankResults(plen int, orderKeys []SortKey, perm []int) map[string][]any {
	out := map[string][]any{
		"row_number":   make([]any, plen),
		"rank":         make([]any, plen),
		"dense_rank":   make([]any, plen),
		"percent_rank": make([]any, plen),
		"cume_dist":    make([]any, plen),
	}
	rank := int64(1)
	dense := int64(1)
	peerStart := 0
	for k := 0; k < plen; k++ {
		if k > 0 && !peerEqual(orderKeys, perm[k-1], perm[k]) {
			rank = int64(k + 1)
			dense++
			peerStart = k
		}
		out["row_number"][k] = int64(k + 1)
		out["rank"][k] = rank
		out["dense_rank"][k] = dense
		_ = peerStart
	}
	// percent_rank = (rank-1)/(n-1)；n=1 时为 0。cume_dist = peerEnd/n。
	for k := 0; k < plen; k++ {
		rk := out["rank"][k].(int64)
		if plen <= 1 {
			out["percent_rank"][k] = float64(0)
		} else {
			out["percent_rank"][k] = float64(rk-1) / float64(plen-1)
		}
	}
	// cume_dist 需要 peer 组终点，由调用方传入的 peerEnd 计算更直接，
	// 这里通过 rank 反推同 rank 组的终点。
	groupEnd := 0
	for k := 0; k < plen; {
		j := k + 1
		for j < plen && out["rank"][j].(int64) == out["rank"][k].(int64) {
			j++
		}
		groupEnd = j
		cd := float64(groupEnd) / float64(plen)
		for p := k; p < j; p++ {
			out["cume_dist"][p] = cd
		}
		k = j
	}
	return out
}

// ntileResults 计算 NTILE(bucket)：与 PostgreSQL 一致，余数个桶多分 1 行，
// 大桶在前。
func ntileResults(plen int, buckets int64) ([]any, error) {
	if buckets <= 0 {
		return nil, fmt.Errorf("ntile argument must be positive")
	}
	out := make([]any, plen)
	base := plen / int(buckets)
	rem := plen % int(buckets)
	pos := 0
	for b := int64(0); b < buckets; b++ {
		size := base
		if int(b) < rem {
			size++
		}
		for x := 0; x < size; x++ {
			if pos < plen {
				out[pos] = b + 1
				pos++
			}
		}
	}
	return out, nil
}
