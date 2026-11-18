package ref

import (
	"fmt"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
	"dragonwindow/internal/window"
)

// bruteRank 暴力计算排名类函数（逐行扫描 peer）。
func bruteRank(wc *expr.WindowCall, args []*window.Evaluated, keys []window.SortKey, perm []int, plen int, result []any) error {
	fn := wc.Func
	if fn == "ntile" {
		v := args[0].Vals[perm[0]]
		n, err := asInt(v)
		if err != nil {
			return fmt.Errorf("ntile: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("ntile argument must be positive")
		}
		base := plen / int(n)
		rem := plen % int(n)
		pos := 0
		for b := int64(0); b < n; b++ {
			size := base
			if int(b) < rem {
				size++
			}
			for x := 0; x < size && pos < plen; x++ {
				result[perm[pos]] = b + 1
				pos++
			}
		}
		return nil
	}
	for k := 0; k < plen; k++ {
		raw := perm[k]
		switch fn {
		case "row_number":
			result[raw] = int64(k + 1)
		case "rank":
			var r int64 = 1
			for p := 0; p < k; p++ {
				if !peerAt(keys, perm, p, k) {
					r++
				}
			}
			result[raw] = r
		case "dense_rank":
			seen := map[int]struct{}{}
			var r int64 = 1
			for p := 0; p < k; p++ {
				if !peerAt(keys, perm, p, k) {
					// 仅在遇到新的不同值组时 +1：用 rank 组起点计数。
					if p == 0 || !peerAt(keys, perm, p-1, p) {
						seen[p] = struct{}{}
					}
				}
			}
			r = int64(len(seen)) + 1
			result[raw] = r
		case "percent_rank":
			var rank int64 = 1
			for p := 0; p < k; p++ {
				if !peerAt(keys, perm, p, k) {
					rank++
				}
			}
			if plen <= 1 {
				result[raw] = float64(0)
			} else {
				result[raw] = float64(rank-1) / float64(plen-1)
			}
		case "cume_dist":
			// peer 组终点 / n。
			end := k
			for end < plen && peerAt(keys, perm, k, end) {
				end++
			}
			result[raw] = float64(end) / float64(plen)
		}
	}
	return nil
}

// peerAt 判断分区局部位置 a、b 是否为 peer（排序键全等，NULL 相等）。
func peerAt(keys []window.SortKey, perm []int, a, b int) bool {
	for _, k := range keys {
		va := k.Eval.Vals[perm[a]]
		vb := k.Eval.Vals[perm[b]]
		if va == nil || vb == nil {
			if va != vb {
				return false
			}
			continue
		}
		if c, _ := types.Compare(va, vb); c != 0 {
			return false
		}
	}
	return true
}

// bruteLagLead 暴力计算 LAG/LEAD（含 IGNORE NULLS、偏移、默认值）。
func bruteLagLead(wc *expr.WindowCall, args []*window.Evaluated, keys []window.SortKey, perm []int, plen int, result []any) error {
	backward := wc.Func == "lag"
	arg := args[0]
	for k := 0; k < plen; k++ {
		step := int64(1)
		if len(args) >= 2 {
			v := args[1].Vals[perm[k]]
			if v == nil {
				return fmt.Errorf("LAG/LEAD offset must not be NULL")
			}
			n, err := asInt(v)
			if err != nil {
				return err
			}
			if n < 0 {
				return fmt.Errorf("LAG/LEAD offset must not be negative")
			}
			step = n
		}
		pos := k
		var counted int64
		dir := 1
		if backward {
			dir = -1
		}
		found := false
		for counted < step {
			pos += dir
			if pos < 0 || pos >= plen {
				break
			}
			if wc.IgnoreNulls && arg.Vals[perm[pos]] == nil {
				continue
			}
			counted++
			if counted == step {
				result[perm[k]] = arg.Vals[perm[pos]]
				found = true
				break
			}
		}
		if !found {
			if len(args) >= 3 {
				result[perm[k]] = args[2].Vals[perm[k]]
			} else {
				result[perm[k]] = nil
			}
		}
	}
	return nil
}

// bruteNavFrame 在已算出的帧成员里计算 first/last/nth value。
func bruteNavFrame(wc *expr.WindowCall, args []*window.Evaluated, perm, members []int, k int) (any, error) {
	arg := args[0]
	get := func(p int) any { return arg.Vals[perm[p]] }
	switch wc.Func {
	case "first_value":
		for _, p := range members {
			v := get(p)
			if wc.IgnoreNulls && v == nil {
				continue
			}
			return v, nil
		}
		return nil, nil
	case "last_value":
		for i := len(members) - 1; i >= 0; i-- {
			v := get(members[i])
			if wc.IgnoreNulls && v == nil {
				continue
			}
			return v, nil
		}
		return nil, nil
	case "nth_value":
		nAny := args[1].Vals[perm[k]]
		if nAny == nil {
			return nil, nil
		}
		n, err := asInt(nAny)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("nth_value offset must be positive")
		}
		var seen int64
		for _, p := range members {
			v := get(p)
			if wc.IgnoreNulls && v == nil {
				continue
			}
			seen++
			if seen == n {
				return v, nil
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown nav function %s", wc.Func)
}

func asInt(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		return 0, fmt.Errorf("offset must be integer, got double precision")
	default:
		return 0, fmt.Errorf("offset must be integer, got %T", v)
	}
}

// ---- 共享小工具 -----------------------------------------------------------

func filterRows(in *table.Table, keep []bool) *table.Table {
	out := in.CloneEmpty(in.Name)
	for r := 0; r < in.N; r++ {
		if keep != nil && !keep[r] {
			continue
		}
		out.AppendRow(in.RowValues(r))
	}
	return out
}

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

func preEvalOffsets(t *table.Table, spec *expr.OverSpec, colMap map[*expr.ColumnRef]int) (map[expr.Node]*window.Evaluated, error) {
	f := effectiveFrame(spec)
	out := map[expr.Node]*window.Evaluated{}
	collect := func(b *expr.FrameBound) error {
		if b == nil || b.Offset == nil || out[b.Offset] != nil {
			return nil
		}
		ev, err := window.EvalColumn(t, b.Offset, colMap)
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

type schemaAdapter struct{ t *table.Table }

func (s *schemaAdapter) ColumnIndex(name string) int { return s.t.ColIndex(name) }
func (s *schemaAdapter) ColumnCount() int            { return len(s.t.Columns) }
func (s *schemaAdapter) AddVirtualColumn(name string) int {
	idx := len(s.t.Columns)
	s.t.AddColumn(name, types.TypeReal, make([]any, s.t.N))
	return idx
}
