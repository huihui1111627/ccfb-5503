package window

import (
	"fmt"
	"math/big"
	"strings"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// evalGroup evaluates every item that shares one sort/partitioning.
func evalGroup(src, out *table.Table, q *Query, g *windowGroup, plan *Plan) error {
	spec := g.spec
	keys := compileKeys(spec.Order, src.Schema)
	outIdx := make([]int, len(g.items))
	for i, it := range g.items {
		idx, ok := out.Schema.Index(it.OutName)
		if !ok {
			return fmt.Errorf("internal: output column %q missing", it.OutName)
		}
		outIdx[i] = idx
	}

	pt, err := prepare(src, spec, keys)
	if err != nil {
		return err
	}
	for pi := range pt.partStarts {
		base, end := pt.partStarts[pi], pt.partEnds[pi]
		if err := evalPartition(src, out, g, pt, base, end, outIdx); err != nil {
			return err
		}
	}
	return nil
}

func evalPartition(src, out *table.Table, g *windowGroup, pt *preparedTable, base, end int, outIdx []int) error {
	spec := g.spec
	pc, err := newPartitionContext(pt, spec, base, end, src.Rows)
	if err != nil {
		return err
	}
	frameCache := map[string]*frameData{}
	getFrames := func(f *Frame) (*frameData, error) {
		key := frameIdentity(f)
		if fd, ok := frameCache[key]; ok {
			return fd, nil
		}
		fd, err := computeFrames(pc, f, src.Rows)
		if err != nil {
			return nil, err
		}
		frameCache[key] = fd
		return fd, nil
	}

	for ii, it := range g.items {
		col := outIdx[ii]
		switch it.Kind {
		case FuncRanking:
			if err := evalRanking(it, pc, out, col); err != nil {
				return err
			}
		case FuncOffset:
			if err := evalOffsetFunc(it, pc, out, col); err != nil {
				return err
			}
		case FuncAggregate:
			fd, err := getFrames(effectiveFrame(it))
			if err != nil {
				return err
			}
			if err := evalAggregateItem(it, pc, fd, out, col); err != nil {
				return err
			}
		case FuncValue:
			fd, err := getFrames(effectiveFrame(it))
			if err != nil {
				return err
			}
			if err := evalValueItem(it, pc, fd, out, col); err != nil {
				return err
			}
		}
	}
	return nil
}

// evalArgColumn evaluates an argument over the partition in window order.
func evalArgColumn(e interface {
	Eval(table.Row) (value.Value, error)
}, pc *partitionContext) ([]value.Value, error) {
	vals := make([]value.Value, pc.m)
	for i := 0; i < pc.m; i++ {
		v, err := e.Eval(pc.rows[pc.pt.order[pc.base+i]])
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func evalEligible(filter interface {
	Eval(table.Row) (value.Value, error)
}, pc *partitionContext) ([]bool, error) {
	elig := make([]bool, pc.m)
	for i := 0; i < pc.m; i++ {
		v, err := filter.Eval(pc.rows[pc.pt.order[pc.base+i]])
		if err != nil {
			return nil, err
		}
		elig[i] = !v.IsNull() && v.Kind == value.KindBool && v.B
	}
	return elig, nil
}

// evalAggregateItem computes one aggregate over every row's frame using the
// segment tree.
func evalAggregateItem(it *WindowItem, pc *partitionContext, fd *frameData, out *table.Table, col int) error {
	name := strings.ToLower(it.Call.Name)
	isCountStar := name == "count" && (it.Call.Star || len(it.Call.Args) == 0)
	var vals []value.Value
	var err error
	if !isCountStar {
		vals, err = evalArgColumn(it.Call.Args[0], pc)
		if err != nil {
			return err
		}
	}
	var eligible []bool
	if it.Call.Filter != nil {
		eligible, err = evalEligible(it.Call.Filter, pc)
		if err != nil {
			return err
		}
	}
	kind := value.KindNull
	if !isCountStar {
		kind = kindOfValues(vals)
	}
	tree := newAggTree(pc.m, kind)
	tree.build(eligible, vals)

	res := tree.newResult()
	for i := 0; i < pc.m; i++ {
		s, e := fd.frames[i][0], fd.frames[i][1]
		tree.reset(res)
		tree.addRange(res, s, e)
		excluded := applyExclusion(tree, res, fd, i, vals, s, e)
		if excluded && (name == "min" || name == "max") {
			// EXCLUDE may have removed the extreme value; recompute min/max
			// over the resulting frame.  Only MIN/MAX need this rescan.
			res.recomputeExtremes(fd, i, s, e, vals, eligible)
		}
		v, err := aggregateValue(name, res, kind, vals == nil)
		if err != nil {
			return err
		}
		out.Rows[pc.pt.order[pc.base+i]][col] = v
	}
	return nil
}

func applyExclusion(tree *aggTree, res *aggResult, fd *frameData, i int, vals []value.Value, s, e int) bool {
	n := 0
	switch fd.excl {
	case ExcludeCurrentRow:
		if i >= s && i <= e {
			tree.subtract(res, i, i)
			n++
		}
	case ExcludeGroup:
		lo := maxInt(fd.gStarts[fd.groupOf[i]], s)
		hi := minInt(fd.gEnds[fd.groupOf[i]], e)
		if lo <= hi {
			tree.subtract(res, lo, hi)
			n++
		}
	case ExcludeTies:
		lo := maxInt(fd.gStarts[fd.groupOf[i]], s)
		hi := minInt(fd.gEnds[fd.groupOf[i]], e)
		if lo <= hi {
			tree.subtract(res, lo, hi)
			if i >= s && i <= e {
				tree.addRange(res, i, i)
			}
			n++
		}
	}
	return n > 0
}

func aggregateValue(name string, res *aggResult, kind value.Kind, countStar bool) (value.Value, error) {
	switch name {
	case "count":
		if countStar {
			return value.Int(res.cnt), nil
		}
		return value.Int(res.nn), nil
	case "sum":
		if res.nn == 0 {
			return value.Null, nil
		}
		switch kind {
		case value.KindNumeric:
			return value.Numeric(value.RatToString(res.sumR, 40)), nil
		case value.KindFloat:
			return value.Float(res.sumF), nil
		default:
			return value.Int(res.sumI), nil
		}
	case "avg":
		if res.nn == 0 {
			return value.Null, nil
		}
		switch kind {
		case value.KindFloat:
			// PostgreSQL: avg(double precision) returns double precision.
			return value.Float(res.sumF / float64(res.nn)), nil
		}
		r := new(big.Rat)
		if kind == value.KindNumeric {
			r.Set(res.sumR)
		} else {
			r.SetInt64(res.sumI)
		}
		r.Quo(r, new(big.Rat).SetInt64(res.nn))
		return value.Numeric(value.RatToString(r, 40)), nil
	case "min":
		return res.min, nil
	case "max":
		return res.max, nil
	}
	return value.Null, semErrf("unknown aggregate %q", name)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
