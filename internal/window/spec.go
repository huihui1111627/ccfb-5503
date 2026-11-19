// Package window implements the window-function execution engine: partitioning,
// ordering, frame construction (ROWS / RANGE / GROUPS, with every bound and
// exclusion combination), and the aggregate / ranking / offset / value
// functions themselves.
//
// The public entry point is Eval, which takes a table plus a query and returns
// a table with the window results appended.
package window

import (
	"fmt"
	"strings"

	"dragonboat/windowengine/internal/expr"
	"dragonboat/windowengine/internal/value"
)

// FrameUnit is the unit a frame bound is expressed in.
type FrameUnit int

const (
	Rows FrameUnit = iota
	Range
	Groups
)

func (u FrameUnit) String() string {
	switch u {
	case Rows:
		return "ROWS"
	case Range:
		return "RANGE"
	case Groups:
		return "GROUPS"
	}
	return "?"
}

// BoundMode is the kind of a frame bound.
type BoundMode int

const (
	BoundUnboundedPreceding BoundMode = iota
	BoundPreceding
	BoundCurrentRow
	BoundFollowing
	BoundUnboundedFollowing
)

func (m BoundMode) String() string {
	switch m {
	case BoundUnboundedPreceding:
		return "UNBOUNDED PRECEDING"
	case BoundPreceding:
		return "PRECEDING"
	case BoundCurrentRow:
		return "CURRENT ROW"
	case BoundFollowing:
		return "FOLLOWING"
	case BoundUnboundedFollowing:
		return "UNBOUNDED FOLLOWING"
	}
	return "?"
}

// Bound is one frame edge.
// Offset is non-nil for BoundPreceding / BoundFollowing.  When the offset
// expression references columns it is evaluated per current row (an extension
// over PostgreSQL, which only accepts constants here); constant offsets are
// evaluated once up front.
type Bound struct {
	Mode   BoundMode
	Offset expr.Expr
}

// Exclusion is the EXCLUDE clause.
type Exclusion int

const (
	ExcludeNoOthers Exclusion = iota
	ExcludeCurrentRow
	ExcludeGroup
	ExcludeTies
)

func (e Exclusion) String() string {
	switch e {
	case ExcludeCurrentRow:
		return "EXCLUDE CURRENT ROW"
	case ExcludeGroup:
		return "EXCLUDE GROUP"
	case ExcludeTies:
		return "EXCLUDE TIES"
	}
	return "EXCLUDE NO OTHERS"
}

// Frame is a window frame specification.
// A nil Frame means the SQL default: RANGE UNBOUNDED PRECEDING .. CURRENT ROW
// when the window has ORDER BY, and the whole partition otherwise.
type Frame struct {
	Unit      FrameUnit
	Start     Bound
	End       Bound
	Exclusion Exclusion
}

// SortKey is one ORDER BY item.
type SortKey struct {
	Expr       expr.Expr
	Desc       bool
	NullsFirst bool
	// Text is retained for display / plan output.
	Text string
	// Kind is the value kind produced by Expr, resolved at parse time.
	Kind value.Kind
	// Ordinal, when > 0, means this key is an output-column reference
	// ("ORDER BY 1") rather than an expression.
	Ordinal int
	// OrdinalName is the resolved column name for an ordinal key.
	OrdinalName string
}

// Spec is one named window definition.
type Spec struct {
	Name       string
	Partition  []expr.Expr
	Order      []SortKey
	Frame      *Frame
	PartitionT []string
	OrderT     []string
}

// FuncKind classifies window functions by their evaluation strategy.
type FuncKind int

const (
	// FuncAggregate functions consume the frame contents.
	FuncAggregate FuncKind = iota
	// FuncRanking functions depend only on the row's rank/peer information.
	FuncRanking
	// FuncOffset functions (lag/lead) depend on a positional offset.
	FuncOffset
	// FuncValue functions (first_value/last_value/nth_value) consume the frame
	// but answer a positional query, so they use an order-statistics structure.
	FuncValue
)

// FuncCall is a call to a window function.
type FuncCall struct {
	Name string
	// Args are the raw argument expressions (for aggregates, lag/lead,
	// nth_value's n, ntile's buckets).
	Args []expr.Expr
	// ArgTexts retain the original argument text for display / plan output.
	ArgTexts []string
	// Star is true for count(*).
	Star bool
	// Filter is the optional FILTER (WHERE ...) clause.
	Filter expr.Expr
	// IgnoreNulls is the extension for lag/lead/nth_value/first_value...:
	// skip NULL values when locating the n-th row.
	IgnoreNulls bool
}

// WindowItem is one SELECT-list item containing a window function.
type WindowItem struct {
	// OutName is the output column name.
	OutName string
	// SourceText is the original expression text, for display.
	SourceText string
	Call       FuncCall
	Kind       FuncKind
	// Spec is the resolved window definition.
	Spec *Spec
	// RefName names a window defined in the WINDOW clause, if used.
	RefName string
	// Passthrough is set for non-window select items; it is evaluated per row
	// and appended unchanged.
	Passthrough expr.Expr
}

// Query is a fully parsed window query.
type Query struct {
	Items []*WindowItem
	// Windows are definitions from the WINDOW clause.
	Windows []*Spec
	// OutputOrder optionally sorts the final result.
	OutputOrder []SortKey
}

// ---- errors ----

// SemError is a user-facing semantic error (bad frame, bad types, ...).
type SemError struct{ Msg string }

func (e *SemError) Error() string { return e.Msg }

func semErrf(format string, args ...any) error {
	return &SemError{Msg: fmt.Sprintf(format, args...)}
}

// FuncKindFor returns the evaluation strategy for a window function name.
func FuncKindFor(name string) (FuncKind, error) {
	switch strings.ToLower(name) {
	case "count", "sum", "avg", "min", "max":
		return FuncAggregate, nil
	case "rank", "dense_rank", "percent_rank", "cume_dist", "ntile", "row_number":
		return FuncRanking, nil
	case "lag", "lead":
		return FuncOffset, nil
	case "first_value", "last_value", "nth_value":
		return FuncValue, nil
	}
	return 0, semErrf("unknown window function %q", name)
}

// TypeName renders a kind for plan output.
func TypeName(k value.Kind) string { return k.String() }
