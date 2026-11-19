package window

import (
	"fmt"
	"math"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// frameData holds the computed frame [start,end] (partition-local, inclusive)
// for every row of one partition, under one frame specification.
type frameData struct {
	frames [][2]int
	// peer-group data, needed by the EXCLUDE clause
	groupOf []int
	gStarts []int
	gEnds   []int
	excl    Exclusion
}

// partitionContext carries per-partition data shared by all frame evaluations.
type partitionContext struct {
	pt     *preparedTable
	spec   *Spec
	base   int
	end    int
	rows   []table.Row
	m      int
	hasOrd bool

	peerF []int
	peerL []int
	gOf   []int
	gS    []int
	gE    []int

	// RANGE order-column data (exactly one ORDER BY key when offsets are used).
	ordKind    value.Kind
	ordVals    []value.Value // non-NULL order values in sort order
	nullLo     int           // partition-local first NULL position (m if none)
	nullHi     int           // partition-local last NULL position (-1 if none)
	nullsAtEnd bool
	orderDesc  bool
}

func newPartitionContext(pt *preparedTable, spec *Spec, base, end int, rows []table.Row) (*partitionContext, error) {
	m := end - base
	pc := &partitionContext{
		pt:     pt,
		spec:   spec,
		base:   base,
		end:    end,
		rows:   rows,
		m:      m,
		hasOrd: len(spec.Order) > 0,
		nullLo: m,
		nullHi: -1,
	}
	pc.peerF, pc.peerL = pt.precomputePeers(base, end, pc.hasOrd)
	pc.gOf = make([]int, m)
	for i := 0; i < m; {
		j := pc.peerL[i]
		pc.gS = append(pc.gS, i)
		pc.gE = append(pc.gE, j)
		for k := i; k <= j; k++ {
			pc.gOf[k] = len(pc.gS) - 1
		}
		i = j + 1
	}
	if len(spec.Order) > 0 {
		pc.orderDesc = spec.Order[0].Desc
	}
	if len(spec.Order) == 1 {
		pc.ordKind = spec.Order[0].Kind
		nn := make([]value.Value, 0, m)
		for i := 0; i < m; i++ {
			v := pt.keyAt(base + i)[0]
			if v.IsNull() {
				if i < pc.nullLo {
					pc.nullLo = i
				}
				pc.nullHi = i
				continue
			}
			nn = append(nn, v)
		}
		pc.ordVals = nn
		pc.nullsAtEnd = !spec.Order[0].NullsFirst
		if k := kindOfValues(nn); k != value.KindNull {
			pc.ordKind = k
		}
		if pc.ordKind == value.KindNull {
			pc.ordKind = spec.Order[0].Kind
		}
	}
	return pc, nil
}

// computeFrames evaluates the frame bounds for every row of the partition.
func computeFrames(pc *partitionContext, frame *Frame, rows []table.Row) (*frameData, error) {
	fd := &frameData{
		frames:  make([][2]int, pc.m),
		groupOf: pc.gOf,
		gStarts: pc.gS,
		gEnds:   pc.gE,
		excl:    frame.Exclusion,
	}
	startConst, haveStart, err := constOffset(frame.Start)
	if err != nil {
		return nil, err
	}
	endConst, haveEnd, err := constOffset(frame.End)
	if err != nil {
		return nil, err
	}
	for i := 0; i < pc.m; i++ {
		s, err := pc.bound(frame, frame.Start, i, true, startConst, haveStart, rows)
		if err != nil {
			return nil, err
		}
		e, err := pc.bound(frame, frame.End, i, false, endConst, haveEnd, rows)
		if err != nil {
			return nil, err
		}
		if s < 0 {
			s = 0
		}
		if e >= pc.m {
			e = pc.m - 1
		}
		fd.frames[i] = [2]int{s, e}
	}
	return fd, nil
}

func constOffset(b Bound) (value.Value, bool, error) {
	if b.Offset == nil {
		return value.Null, false, nil
	}
	if b.Offset.HasColumn() {
		return value.Null, false, nil
	}
	v, err := b.Offset.Eval(nil)
	if err != nil {
		return value.Null, false, err
	}
	return v, true, nil
}

func (pc *partitionContext) evalBoundOffset(b Bound, i int, isStart bool, cv value.Value, haveConst bool, rows []table.Row) (value.Value, error) {
	if haveConst {
		return cv, nil
	}
	if b.Offset == nil {
		return value.Null, fmt.Errorf("missing frame offset")
	}
	return b.Offset.Eval(rows[pc.pt.order[pc.base+i]])
}

func (pc *partitionContext) bound(frame *Frame, b Bound, i int, isStart bool, cv value.Value, haveConst bool, rows []table.Row) (int, error) {
	switch frame.Unit {
	case Rows:
		return pc.rowsBound(b, i, isStart, cv, haveConst, rows)
	case Groups:
		return pc.groupsBound(b, i, isStart, cv, haveConst, rows)
	case Range:
		return pc.rangeBound(b, i, isStart, cv, haveConst, rows)
	}
	return 0, fmt.Errorf("unknown frame unit")
}

func whichName(isStart bool) string {
	if isStart {
		return "starting"
	}
	return "ending"
}

// offsetMagnitude validates and converts a frame offset.  PostgreSQL requires
// offsets to be non-null and non-negative; fractional values round to the
// nearest integer, halves away from zero.
func offsetMagnitude(v value.Value, which string) (int64, error) {
	if v.IsNull() {
		return 0, semErrf("frame %s offset must not be null", which)
	}
	if !v.Kind.IsNumericKind() {
		return 0, semErrf("frame %s offset must be a number, got %s", which, v.Kind)
	}
	f, err := value.ToFloat(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, semErrf("frame %s offset must be finite", which)
	}
	r := math.Round(f)
	if r < 0 {
		return 0, semErrf("frame %s offset must not be negative", which)
	}
	if r > 9.0e15 {
		return math.MaxInt64, nil
	}
	return int64(r), nil
}

func (pc *partitionContext) rowsBound(b Bound, i int, isStart bool, cv value.Value, haveConst bool, rows []table.Row) (int, error) {
	switch b.Mode {
	case BoundUnboundedPreceding:
		return 0, nil
	case BoundUnboundedFollowing:
		return pc.m - 1, nil
	case BoundCurrentRow:
		return i, nil
	case BoundPreceding, BoundFollowing:
		v, err := pc.evalBoundOffset(b, i, isStart, cv, haveConst, rows)
		if err != nil {
			return 0, err
		}
		mag, err := offsetMagnitude(v, whichName(isStart))
		if err != nil {
			return 0, err
		}
		if b.Mode == BoundPreceding {
			if mag > int64(pc.m) {
				mag = int64(pc.m) + 1
			}
			return i - int(mag), nil
		}
		if mag > int64(pc.m) {
			mag = int64(pc.m) + 1
		}
		return i + int(mag), nil
	}
	return 0, fmt.Errorf("bad bound")
}

func (pc *partitionContext) groupsBound(b Bound, i int, isStart bool, cv value.Value, haveConst bool, rows []table.Row) (int, error) {
	switch b.Mode {
	case BoundUnboundedPreceding:
		return 0, nil
	case BoundUnboundedFollowing:
		return pc.m - 1, nil
	case BoundCurrentRow:
		if isStart {
			return pc.peerF[i], nil
		}
		return pc.peerL[i], nil
	case BoundPreceding, BoundFollowing:
		v, err := pc.evalBoundOffset(b, i, isStart, cv, haveConst, rows)
		if err != nil {
			return 0, err
		}
		mag, err := offsetMagnitude(v, whichName(isStart))
		if err != nil {
			return 0, err
		}
		g := pc.gOf[i]
		target := g + int(mag)
		if b.Mode == BoundPreceding {
			target = g - int(mag)
		}
		if target < 0 {
			if isStart {
				return 0, nil
			}
			return -1, nil
		}
		if target >= len(pc.gS) {
			if isStart {
				return pc.m, nil
			}
			return pc.m - 1, nil
		}
		if isStart {
			return pc.gS[target], nil
		}
		return pc.gE[target], nil
	}
	return 0, fmt.Errorf("bad bound")
}

// ---------- RANGE ----------

// rangeBound computes a RANGE frame edge following PostgreSQL semantics.
//
// NULL order values all sort together at one end.  A finite offset bound with
// a non-NULL current row never includes the NULL group: NULLs behave as -inf
// (NULLS FIRST) or +inf (NULLS LAST) sort keys.  UNBOUNDED bounds and CURRENT
// ROW operate on the whole partition including NULLs.  When the current row
// itself is NULL, a finite offset bound resolves to the NULL group's edge.
func (pc *partitionContext) rangeBound(b Bound, i int, isStart bool, cv value.Value, haveConst bool, rows []table.Row) (int, error) {
	switch b.Mode {
	case BoundUnboundedPreceding:
		return 0, nil
	case BoundUnboundedFollowing:
		return pc.m - 1, nil
	case BoundCurrentRow:
		// CURRENT ROW in RANGE mode is the peer group, including NULL peers
		// and including the unordered case where all rows are peers.
		if isStart {
			return pc.peerF[i], nil
		}
		return pc.peerL[i], nil
	}

	v, err := pc.evalBoundOffset(b, i, isStart, cv, haveConst, rows)
	if err != nil {
		return 0, err
	}
	if v.IsNull() {
		return 0, semErrf("frame %s offset must not be null", whichName(isStart))
	}
	if !value.OffsetCompatible(pc.ordKind, v.Kind) {
		return 0, semErrf("RANGE with offset PRECEDING/FOLLOWING is not supported for column type %s and offset type %s",
			pc.ordKind, v.Kind)
	}
	if !offsetNonNegative(v) {
		return 0, semErrf("invalid preceding or following size in window function")
	}

	cur := pc.curValue(i)
	if cur.IsNull() {
		// A NULL current row can only be meaningfully bounded by the NULL
		// group itself; finite offsets keep both edges on that group, while
		// UNBOUNDED FOLLOWING may extend past it.
		if isStart {
			if b.Mode == BoundPreceding {
				return pc.nullLo, nil
			}
			return pc.nullLo, nil
		}
		if b.Mode == BoundFollowing {
			return pc.nullHi, nil
		}
		return pc.nullHi, nil
	}

	limit, err := rangeLimit(cur, v, b.Mode, pc.orderDesc)
	if err != nil {
		return 0, err
	}
	nn := len(pc.ordVals)
	nullCount := pc.m - nn
	offset := 0
	if !pc.nullsAtEnd {
		offset = nullCount
	}
	desc := pc.orderDesc
	if isStart {
		pos := pc.lowerBound(limit, desc)
		return offset + pos, nil
	}
	pos := pc.upperBound(limit, desc)
	return offset + pos - 1, nil
}

func offsetNonNegative(v value.Value) bool {
	switch v.Kind {
	case value.KindInterval:
		return v.D.Months >= 0 && v.D.Days >= 0 && v.D.Nanos >= 0
	case value.KindInt:
		return v.I >= 0
	case value.KindFloat:
		return !(v.F < 0) && !math.IsNaN(v.F)
	case value.KindNumeric:
		f, err := value.ToFloat(v)
		return err == nil && f >= 0
	}
	return false
}

func (pc *partitionContext) curValue(i int) value.Value {
	return pc.pt.keyAt(pc.base + i)[0]
}

// rangeLimit computes the value limit for a bound: current value moved toward
// the frame edge by the offset.
//
// PRECEDING moves backwards along the sort order, FOLLOWING forwards.
// Ascending order: PRECEDING = cur - off.  Descending: PRECEDING = cur + off.
func rangeLimit(cur, off value.Value, mode BoundMode, desc bool) (value.Value, error) {
	if mode == BoundPreceding {
		if desc {
			return value.AddOffset(cur, off)
		}
		neg, err := negate(off)
		if err != nil {
			return value.Null, err
		}
		return value.AddOffset(cur, neg)
	}
	if desc {
		neg, err := negate(off)
		if err != nil {
			return value.Null, err
		}
		return value.AddOffset(cur, neg)
	}
	return value.AddOffset(cur, off)
}

// lowerBound returns the first index in ordVals not before limit in sort order
// (ASC: value >= limit; DESC: value <= limit).
func (pc *partitionContext) lowerBound(limit value.Value, desc bool) int {
	return searchValues(pc.ordVals, limit, desc, true)
}

// upperBound returns the first index in ordVals beyond limit in sort order
// (ASC: value > limit; DESC: value < limit).
func (pc *partitionContext) upperBound(limit value.Value, desc bool) int {
	return searchValues(pc.ordVals, limit, desc, false)
}

func searchValues(vals []value.Value, limit value.Value, desc, lower bool) int {
	lo, hi := 0, len(vals)
	for lo < hi {
		mid := (lo + hi) / 2
		c, err := value.Compare(vals[mid], limit)
		if err != nil {
			c = 0
		}
		before := c < 0
		if desc {
			before = c > 0
		}
		beyond := c > 0
		if desc {
			beyond = c < 0
		}
		var goLeft bool
		if lower {
			goLeft = !before // first position not before limit
		} else {
			goLeft = beyond // first position beyond limit
		}
		if goLeft {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// framesForItem caches per-partition frame computations across items.
type frameCacheEntry struct {
	key  string
	data *frameData
}

func frameKey(f *Frame) string {
	return f.Unit.String() + "|" + boundKey(f.Start) + "|" + boundKey(f.End) + "|" + f.Exclusion.String()
}

// frameIdentity describes a frame specification including the exact offset
// expressions, so per-row offsets are never shared between different frames.
func frameIdentity(f *Frame) string {
	return frameKey(f)
}

func boundKey(b Bound) string {
	if b.Offset != nil {
		return b.Mode.String() + ":" + b.Offset.String()
	}
	return b.Mode.String()
}
