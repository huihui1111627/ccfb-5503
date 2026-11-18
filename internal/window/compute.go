package window

import (
	"fmt"
	"time"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
)

// computeWindow 计算一个窗口函数调用，返回按「过滤后表原始行号」对齐的结果列。
func (e *Engine) computeWindow(
	t *table.Table,
	wc *expr.WindowCall,
	colMap map[*expr.ColumnRef]int,
	sortCache map[string]*SortedPerm,
	plan *Plan,
	label string,
) ([]any, WindowPlan, error) {
	spec := wc.Over
	wp := WindowPlan{
		Label:   label,
		Func:    wc.Func,
		Frame:   expr.FrameString(effectiveFrame(spec)),
		Exclude: effectiveFrame(spec).Exclude,
	}

	// 1. 预求值分区键、排序键。
	partEvals := make([]*Evaluated, len(spec.Partition))
	for i, p := range spec.Partition {
		ev, err := EvalColumn(t, p, colMap)
		if err != nil {
			return nil, wp, err
		}
		partEvals[i] = ev
		wp.Partition = append(wp.Partition, expr.NodeString(p))
	}
	orderEvals := make([]*Evaluated, len(spec.OrderBy))
	for i, o := range spec.OrderBy {
		ev, err := EvalColumn(t, o.X, colMap)
		if err != nil {
			return nil, wp, err
		}
		orderEvals[i] = ev
		wp.OrderBy = append(wp.OrderBy, o.String())
	}

	// 2. 排序（带共享缓存）。
	sig := sortSignature(spec.Partition, spec.OrderBy)
	sp, cached := sortCache[sig]
	if !cached {
		s := time.Now()
		sp = SortPartition(t.N, partEvals, orderEvals, spec.OrderBy, spec.Partition)
		plan.add(PlanStage{
			Name:     "排序 " + label,
			RowsIn:   t.N,
			RowsOut:  t.N,
			Duration: time.Since(s),
			Detail:   "分区键 " + fmt.Sprint(wp.Partition) + " 排序键 " + fmt.Sprint(wp.OrderBy),
		})
		sortCache[sig] = sp
	} else {
		plan.SortReused++
		wp.SortCached = true
	}

	// 3. 预求值帧偏移表达式（动态偏移：每行各自计算）。
	frameSpec := effectiveFrame(spec)
	offsetEval, err := preEvalOffsets(t, frameSpec, colMap)
	if err != nil {
		return nil, wp, err
	}

	// 4. 预求值函数参数与 FILTER。
	argEvals := make([]*Evaluated, len(wc.Args))
	for i, a := range wc.Args {
		ev, err := EvalColumn(t, a, colMap)
		if err != nil {
			return nil, wp, err
		}
		argEvals[i] = ev
	}
	filterBools, err := EvalFilter(t, wc.Filter, colMap)
	if err != nil {
		return nil, wp, err
	}

	result := make([]any, t.N)

	// 5. 按函数族分发。
	if isRankFunc(wc.Func) {
		err = e.runRank(t, sp, wc, argEvals, result, wp)
	} else if expr.IsAggregate(wc.Func) {
		err = e.runAggregate(t, sp, frameSpec, offsetEval, wc, argEvals, filterBools, result, &wp)
	} else {
		err = e.runNavigation(t, sp, frameSpec, offsetEval, wc, argEvals, result, &wp)
	}
	if err != nil {
		return nil, wp, err
	}
	return result, wp, nil
}

// effectiveFrame 返回生效的帧规格（nil 时补 PostgreSQL 默认帧）。
func effectiveFrame(spec *expr.OverSpec) *expr.FrameSpec {
	if spec.Frame != nil {
		return spec.Frame
	}
	return &expr.FrameSpec{
		Unit:  "range",
		Start: expr.FrameBound{Kind: "unbounded preceding"},
		End:   &expr.FrameBound{Kind: "current row"},
	}
}

// preEvalOffsets 把帧里出现的全部偏移表达式整列预求值，按节点指针缓存。
func preEvalOffsets(t *table.Table, f *expr.FrameSpec, colMap map[*expr.ColumnRef]int) (map[expr.Node]*Evaluated, error) {
	out := map[expr.Node]*Evaluated{}
	collect := func(b *expr.FrameBound) error {
		if b == nil || b.Offset == nil {
			return nil
		}
		if _, ok := out[b.Offset]; ok {
			return nil
		}
		ev, err := EvalColumn(t, b.Offset, colMap)
		if err != nil {
			return err
		}
		out[b.Offset] = ev
		return nil
	}
	if err := collect(&f.Start); err != nil {
		return nil, err
	}
	if f.End != nil {
		if err := collect(f.End); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- 排名函数 -------------------------------------------------------------

func (e *Engine) runRank(t *table.Table, sp *SortedPerm, wc *expr.WindowCall, args []*Evaluated, result []any, wp WindowPlan) error {
	ntileArg := map[int]int64{} // 分区起点 -> 桶数（NTILE 要求常量参数）
	for pi := 0; pi < len(sp.PartStart)-1; pi++ {
		ps, pe := sp.PartStart[pi], sp.PartStart[pi+1]
		plen := pe - ps
		perm := sp.Perm[ps:pe]
		keys := sp.Keys
		if wc.Func == "ntile" {
			if len(wc.Args) != 1 {
				return fmt.Errorf("ntile takes 1 argument")
			}
			v := args[0].Vals[perm[0]]
			n, err := toInt64(v)
			if err != nil {
				return fmt.Errorf("ntile argument: %v", err)
			}
			ntileArg[ps] = n
			res, err := ntileResults(plen, n)
			if err != nil {
				return err
			}
			for k := 0; k < plen; k++ {
				result[perm[k]] = res[k]
			}
			continue
		}
		rm := rankResults(plen, keys, perm)
		col := rm[wc.Func]
		if col == nil {
			return fmt.Errorf("rank function %s not available", wc.Func)
		}
		for k := 0; k < plen; k++ {
			result[perm[k]] = col[k]
		}
	}
	return nil
}
