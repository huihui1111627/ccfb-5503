package window

import (
	"time"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
)

// runAggregate 在每个分区上为聚合建树，并逐行做 O(log n) 帧查询。
func (e *Engine) runAggregate(
	t *table.Table,
	sp *SortedPerm,
	frameSpec *expr.FrameSpec,
	offsetEval map[expr.Node]*Evaluated,
	wc *expr.WindowCall,
	args []*Evaluated,
	filterBools []bool,
	result []any,
	wp *WindowPlan,
) error {
	buildTotal := time.Duration(0)
	frameTotal := time.Duration(0)

	spec := classifyAggregate(wc, args, filterBools)

	for pi := 0; pi < len(sp.PartStart)-1; pi++ {
		ps, pe := sp.PartStart[pi], sp.PartStart[pi+1]
		plen := pe - ps
		perm := sp.Perm[ps:pe]

		// 分区局部坐标下的 peer 数组与各输入列。
		pPeerStart := sp.PeerStart[ps:pe]
		pPeerEnd := sp.PeerEnd[ps:pe]
		localVals := make([][]any, len(args))
		for i, ev := range args {
			localVals[i] = gatherLocal(ev.Vals, perm)
		}
		var localFilter []bool
		if filterBools != nil {
			localFilter = make([]bool, plen)
			for k, raw := range perm {
				localFilter[k] = filterBools[raw]
			}
		}

		tb0 := time.Now()
		bt, err := buildAggTree(spec, localVals, localFilter, plen)
		if err != nil {
			return err
		}
		buildTotal += time.Since(tb0)

		fr := &frameResolver{
			plen:       plen,
			unit:       frameSpec.Unit,
			orderKeys:  sp.Keys,
			peerStart:  pPeerStart,
			peerEnd:    pPeerEnd,
			offsetEval: offsetEval,
			perm:       perm,
		}

		// 累计帧快速路径：UNBOUNDED PRECEDING 到 CURRENT ROW、无 EXCLUDE。
		// 这是累计排名/累计用时最常见的形态，用一次线性前缀累加替代每行
		// O(log n) 的树查询，结果与树路径走完全相同的幺半群，值严格一致。
		if isCumulativeFrame(frameSpec) {
			fc0 := time.Now()
			runCumulative(bt, spec, plen, pPeerStart, pPeerEnd, perm, result, frameSpec.Unit)
			frameTotal += time.Since(fc0)
			continue
		}

		fc0 := time.Now()
		for k := 0; k < plen; k++ {
			f, err := fr.resolveFrame(k, frameSpec)
			if err != nil {
				return err
			}
			ranges := effectiveRanges(f, k, pPeerStart, pPeerEnd, frameSpec.Exclude)
			v := queryAggTree(bt, spec, ranges)
			result[perm[k]] = v
		}
		frameTotal += time.Since(fc0)
	}
	wp.TreeBuild = buildTotal
	wp.FrameCalc = frameTotal
	return nil
}

// gatherLocal 按排列把整表列收拢成分区局部列。
func gatherLocal(vals []any, perm []int) []any {
	out := make([]any, len(perm))
	for k, raw := range perm {
		out[k] = vals[raw]
	}
	return out
}

// runNavigation 计算 LAG/LEAD/FIRST_VALUE/LAST_VALUE/NTH_VALUE。
func (e *Engine) runNavigation(
	t *table.Table,
	sp *SortedPerm,
	frameSpec *expr.FrameSpec,
	offsetEval map[expr.Node]*Evaluated,
	wc *expr.WindowCall,
	args []*Evaluated,
	result []any,
	wp *WindowPlan,
) error {
	nav, err := buildNavSpec(wc, args)
	if err != nil {
		return err
	}
	fc0 := time.Now()
	for pi := 0; pi < len(sp.PartStart)-1; pi++ {
		ps, pe := sp.PartStart[pi], sp.PartStart[pi+1]
		plen := pe - ps
		perm := sp.Perm[ps:pe]
		partSP := subSortedPerm(sp, ps, pe)
		fr := &frameResolver{
			plen:       plen,
			unit:       frameSpec.Unit,
			orderKeys:  sp.Keys,
			peerStart:  partSP.PeerStart,
			peerEnd:    partSP.PeerEnd,
			offsetEval: offsetEval,
			perm:       perm,
		}
		res, err := evalNavigation(plen, partSP, fr, frameSpec, nav)
		if err != nil {
			return err
		}
		for k := 0; k < plen; k++ {
			result[perm[k]] = res[k]
		}
	}
	wp.FrameCalc = time.Since(fc0)
	return nil
}

// subSortedPerm 生成分区局部视图。PeerStart/PeerEnd 已在 SortPartition 阶段
// 按分区局部坐标存储，这里直接切片，不再平移。
func subSortedPerm(sp *SortedPerm, ps, pe int) *SortedPerm {
	out := &SortedPerm{
		Perm:      sp.Perm[ps:pe],
		PeerStart: sp.PeerStart[ps:pe],
		PeerEnd:   sp.PeerEnd[ps:pe],
		Orders:    sp.Orders,
		Keys:      sp.Keys,
	}
	return out
}
