package window

import (
	"math/big"

	"dragonboat/windowengine/internal/value"
)

// aggTree is a segment tree over the rows of one partition (in window order)
// that answers frame aggregates in O(log n) instead of rescanning the frame.
//
// It tracks eligible rows (FILTER passes), non-null eligible values and the
// sum/min/max over those values.  For EXCLUDE clauses the result is adjusted
// by subtracting/adding the few relevant positions, each O(log n).
type aggTree struct {
	n    int
	kind value.Kind
	cnt  []int64
	nn   []int64
	sumI []int64
	sumF []float64
	sumR []*big.Rat
	minV []value.Value
	maxV []value.Value
	has  []bool
	size int
}

func newAggTree(n int, kind value.Kind) *aggTree {
	size := 1
	for size < n {
		size <<= 1
	}
	cap := size * 2
	t := &aggTree{n: n, kind: kind, size: size}
	t.cnt = make([]int64, cap)
	t.nn = make([]int64, cap)
	t.has = make([]bool, cap)
	t.minV = make([]value.Value, cap)
	t.maxV = make([]value.Value, cap)
	switch kind {
	case value.KindInt, value.KindNull:
		t.sumI = make([]int64, cap)
	case value.KindFloat:
		t.sumF = make([]float64, cap)
	case value.KindNumeric:
		t.sumR = make([]*big.Rat, cap)
		for i := range t.sumR {
			t.sumR[i] = new(big.Rat)
		}
	}
	return t
}

func (t *aggTree) build(eligible []bool, vals []value.Value) {
	for i := 0; i < t.size; i++ {
		idx := t.size + i
		if i >= t.n || (eligible != nil && !eligible[i]) {
			continue
		}
		t.cnt[idx] = 1
		if vals == nil {
			continue
		}
		v := vals[i]
		if v.IsNull() {
			continue
		}
		t.nn[idx] = 1
		t.has[idx] = true
		t.minV[idx], t.maxV[idx] = v, v
		switch t.kind {
		case value.KindInt, value.KindNull:
			if v.Kind == value.KindInt {
				t.sumI[idx] += v.I
			}
		case value.KindFloat:
			if f, err := value.ToFloat(v); err == nil {
				t.sumF[idx] += f
			}
		case value.KindNumeric:
			if r, err := value.ToRat(v); err == nil {
				t.sumR[idx].Add(t.sumR[idx], r)
			}
		}
	}
	for i := t.size - 1; i >= 1; i-- {
		t.pull(i)
	}
}

func (t *aggTree) pull(i int) {
	l, r := i*2, i*2+1
	t.cnt[i] = t.cnt[l] + t.cnt[r]
	t.nn[i] = t.nn[l] + t.nn[r]
	t.has[i] = t.has[l] || t.has[r]
	if t.sumI != nil {
		t.sumI[i] = t.sumI[l] + t.sumI[r]
	}
	if t.sumF != nil {
		t.sumF[i] = t.sumF[l] + t.sumF[r]
	}
	if t.sumR != nil {
		t.sumR[i].Add(t.sumR[l], t.sumR[r])
	}
	switch {
	case !t.has[l]:
		t.minV[i], t.maxV[i] = t.minV[r], t.maxV[r]
	case !t.has[r]:
		t.minV[i], t.maxV[i] = t.minV[l], t.maxV[l]
	default:
		minv, maxv := t.minV[l], t.maxV[l]
		if c, err := value.Compare(t.minV[r], minv); err == nil && c < 0 {
			minv = t.minV[r]
		}
		if c, err := value.Compare(t.maxV[r], maxv); err == nil && c > 0 {
			maxv = t.maxV[r]
		}
		t.minV[i], t.maxV[i] = minv, maxv
	}
}

// aggResult accumulates a set of disjoint intervals.
type aggResult struct {
	cnt  int64
	nn   int64
	sumI int64
	sumF float64
	sumR *big.Rat
	min  value.Value
	max  value.Value
	has  bool
}

func (t *aggTree) newResult() *aggResult {
	r := &aggResult{min: value.Null, max: value.Null}
	if t.sumR != nil {
		r.sumR = new(big.Rat)
	}
	return r
}

// result returns the aggregate over [l, r] (inclusive).  It allocates a fresh
// accumulator; hot loops should use reset+addRange instead.
func (t *aggTree) result(l, r int) *aggResult {
	res := t.newResult()
	t.addRange(res, l, r)
	return res
}

// reset clears an accumulator so it can be reused for the next frame without
// allocating.
func (t *aggTree) reset(res *aggResult) {
	res.cnt, res.nn, res.sumI, res.sumF = 0, 0, 0, 0
	res.min, res.max, res.has = value.Null, value.Null, false
	if t.sumR != nil {
		if res.sumR == nil {
			res.sumR = new(big.Rat)
		} else {
			res.sumR.SetInt64(0)
		}
	}
}

// addRange adds [l, r] to res.  The query is iterative to avoid per-call
// recursion overhead; the interval decomposition visits O(log n) nodes.
func (t *aggTree) addRange(res *aggResult, l, r int) {
	if l < 0 {
		l = 0
	}
	if r >= t.n {
		r = t.n - 1
	}
	if l > r {
		return
	}
	for l, r = l+t.size, r+t.size; l <= r; l, r = l>>1, r>>1 {
		if l&1 == 1 {
			t.mergeNode(res, l)
			l++
		}
		if r&1 == 0 {
			t.mergeNode(res, r)
			r--
		}
	}
}

// subtract removes [l, r] from res.
func (t *aggTree) subtract(res *aggResult, l, r int) {
	if l < 0 {
		l = 0
	}
	if r >= t.n {
		r = t.n - 1
	}
	if l > r {
		return
	}
	for l, r = l+t.size, r+t.size; l <= r; l, r = l>>1, r>>1 {
		if l&1 == 1 {
			t.removeNode(res, l)
			l++
		}
		if r&1 == 0 {
			t.removeNode(res, r)
			r--
		}
	}
}

func (t *aggTree) mergeNode(res *aggResult, node int) {
	res.cnt += t.cnt[node]
	res.nn += t.nn[node]
	if t.sumI != nil {
		res.sumI += t.sumI[node]
	}
	if t.sumF != nil {
		res.sumF += t.sumF[node]
	}
	if t.sumR != nil && t.has[node] {
		res.sumR.Add(res.sumR, t.sumR[node])
	}
	if t.has[node] {
		if !res.has {
			res.min, res.max = t.minV[node], t.maxV[node]
			res.has = true
		} else {
			if c, err := value.Compare(t.minV[node], res.min); err == nil && c < 0 {
				res.min = t.minV[node]
			}
			if c, err := value.Compare(t.maxV[node], res.max); err == nil && c > 0 {
				res.max = t.maxV[node]
			}
		}
	}
}

func (t *aggTree) removeNode(res *aggResult, node int) {
	res.cnt -= t.cnt[node]
	res.nn -= t.nn[node]
	if t.sumI != nil {
		res.sumI -= t.sumI[node]
	}
	if t.sumF != nil {
		res.sumF -= t.sumF[node]
	}
	if t.sumR != nil && t.has[node] {
		res.sumR.Sub(res.sumR, t.sumR[node])
	}
	// MIN/MAX cannot be subtracted.  The caller detects any EXCLUDE and
	// recomputes extremes by scanning the frame when they are requested.
}

// recomputeExtremes re-derives MIN/MAX by scanning frame [s,e] minus the
// exclusion.  It is only used when an EXCLUDE clause is present and the
// aggregate is MIN or MAX, where subtraction is not possible.
func (res *aggResult) recomputeExtremes(fd *frameData, i, s, e int, vals []value.Value, eligible []bool) {
	res.min, res.max, res.has = value.Null, value.Null, false
	consider := func(pos int) {
		if pos < s || pos > e || pos >= len(vals) || vals[pos].IsNull() {
			return
		}
		if eligible != nil && !eligible[pos] {
			return
		}
		if !res.has {
			res.min, res.max, res.has = vals[pos], vals[pos], true
			return
		}
		if c, err := value.Compare(vals[pos], res.min); err == nil && c < 0 {
			res.min = vals[pos]
		}
		if c, err := value.Compare(vals[pos], res.max); err == nil && c > 0 {
			res.max = vals[pos]
		}
	}
	switch fd.excl {
	case ExcludeCurrentRow:
		for p := s; p <= e; p++ {
			if p != i {
				consider(p)
			}
		}
	case ExcludeGroup:
		gs, ge := fd.gStarts[fd.groupOf[i]], fd.gEnds[fd.groupOf[i]]
		for p := s; p <= e; p++ {
			if p < gs || p > ge {
				consider(p)
			}
		}
	case ExcludeTies:
		gs, ge := fd.gStarts[fd.groupOf[i]], fd.gEnds[fd.groupOf[i]]
		for p := s; p <= e; p++ {
			if p >= gs && p <= ge && p != i {
				continue
			}
			consider(p)
		}
	}
}
