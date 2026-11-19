package window

import (
	"strings"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// evalRanking implements the ranking family: row_number, rank, dense_rank,
// percent_rank, cume_dist and ntile.
//
// All of them depend only on the row's position within its peer group.
// Values are computed once per peer group, so each is O(1) per row.
func evalRanking(it *WindowItem, pc *partitionContext, out *table.Table, col int) error {
	name := strings.ToLower(it.Call.Name)
	m := pc.m
	switch name {
	case "row_number":
		for i := 0; i < m; i++ {
			out.Rows[pc.pt.order[pc.base+i]][col] = value.Int(int64(i + 1))
		}
		return nil
	case "rank", "percent_rank", "cume_dist", "dense_rank":
		numGroups := len(pc.gS)
		for gi := 0; gi < numGroups; gi++ {
			s, e := pc.gS[gi], pc.gE[gi]
			rank := int64(s + 1)
			var v value.Value
			switch name {
			case "rank":
				v = value.Int(rank)
			case "dense_rank":
				v = value.Int(int64(gi + 1))
			case "percent_rank":
				if m <= 1 {
					v = value.Float(0)
				} else {
					v = value.Float(float64(s) / float64(m-1))
				}
			case "cume_dist":
				v = value.Float(float64(e+1) / float64(m))
			}
			for i := s; i <= e; i++ {
				out.Rows[pc.pt.order[pc.base+i]][col] = v
			}
		}
		return nil
	case "ntile":
		return evalNtile(it, pc, out, col)
	}
	return semErrf("unknown ranking function %q", it.Call.Name)
}

func evalNtile(it *WindowItem, pc *partitionContext, out *table.Table, col int) error {
	if len(it.Call.Args) != 1 {
		return semErrf("ntile takes exactly one argument")
	}
	m := pc.m
	// ntile's argument is constant per partition; PostgreSQL rejects NULL,
	// zero, negative and per-row-varying arguments (it requires a constant).
	v, err := it.Call.Args[0].Eval(pc.rows[pc.pt.order[pc.base]])
	if err != nil {
		return err
	}
	if v.IsNull() {
		return semErrf("argument of ntile must not be null")
	}
	n, err := value.AsInt64(v)
	if err != nil {
		return err
	}
	if n <= 0 {
		return semErrf("argument of ntile must be greater than zero")
	}
	// PostgreSQL divides the partition into n buckets, the first
	// (m mod n) buckets having (m/n)+1 rows.
	base := int64(m) / n
	extra := int64(m) % n
	i := 0
	for bucket := int64(1); bucket <= n && i < m; bucket++ {
		size := base
		if bucket <= extra {
			size++
		}
		for k := int64(0); k < size && i < m; k++ {
			out.Rows[pc.pt.order[pc.base+i]][col] = value.Int(bucket)
			i++
		}
	}
	return nil
}
