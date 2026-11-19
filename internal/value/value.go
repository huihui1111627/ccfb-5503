// Package value defines the dynamic value model shared by the table, expression
// and window layers.
//
// The engine is deliberately small: values are either NULL or one of a handful
// of runtime kinds.  Kinds mirror the PostgreSQL type families that matter for
// window semantics (integers, floats, numerics, text, timestamps, dates,
// times, intervals and booleans).  Ordering, comparison and the "+ - * /"
// arithmetic used by frame offsets are implemented once, here, so that the
// window engine and the SQL front-end cannot disagree about them.
package value

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Kind enumerates the runtime type of a value.
type Kind uint8

const (
	KindNull Kind = iota
	KindInt
	KindFloat
	KindNumeric
	KindString
	KindBool
	KindTimestamp
	KindTimestampTZ
	KindDate
	KindTime
	KindInterval
)

// Value is a single cell.  Only the field matching Kind is meaningful.
type Value struct {
	Kind Kind
	I    int64
	F    float64
	S    string
	B    bool
	T    time.Time
	D    Interval
}

// Interval is a PostgreSQL-style interval: months are kept separately from
// the day/time part because month arithmetic is calendar based.
type Interval struct {
	Months int64
	Days   int64
	Nanos  int64
}

var Null = Value{Kind: KindNull}

func Int(i int64) Value           { return Value{Kind: KindInt, I: i} }
func Float(f float64) Value       { return Value{Kind: KindFloat, F: f} }
func Str(s string) Value          { return Value{Kind: KindString, S: s} }
func Bool(b bool) Value           { return Value{Kind: KindBool, B: b} }
func IntervalV(iv Interval) Value { return Value{Kind: KindInterval, D: iv} }

func Timestamp(t time.Time) Value   { return Value{Kind: KindTimestamp, T: t} }
func TimestampTZ(t time.Time) Value { return Value{Kind: KindTimestampTZ, T: t} }

// Date truncates to midnight so that two dates compare by calendar day.
func Date(t time.Time) Value {
	y, m, d := t.Date()
	return Value{Kind: KindDate, T: time.Date(y, m, d, 0, 0, 0, 0, t.Location())}
}

// TimeOfDay keeps only the clock time, with a fixed reference date.
func TimeOfDay(t time.Time) Value {
	return Value{Kind: KindTime, T: time.Date(2000, 1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)}
}

// Numeric builds a numeric value.  Numerics are stored as their canonical
// decimal string and compared/converted through strconv on demand; the
// engine only needs exact comparisons and conversions, not arithmetic.
func Numeric(s string) Value { return Value{Kind: KindNumeric, S: s} }

func (v Value) IsNull() bool { return v.Kind == KindNull }

func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "NULL"
	case KindInt:
		return strconv.FormatInt(v.I, 10)
	case KindFloat:
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	case KindNumeric, KindString:
		return v.S
	case KindBool:
		if v.B {
			return "true"
		}
		return "false"
	case KindTimestamp:
		return v.T.UTC().Format("2006-01-02 15:04:05.999999")
	case KindTimestampTZ:
		return v.T.UTC().Format("2006-01-02 15:04:05.999999+00")
	case KindDate:
		return v.T.Format("2006-01-02")
	case KindTime:
		return v.T.Format("15:04:05.999999")
	case KindInterval:
		return v.D.String()
	}
	return "?"
}

// Display renders a value for end users (web tables / JSON).
func (v Value) Display() string {
	if v.IsNull() {
		return ""
	}
	return v.String()
}

func (iv Interval) String() string {
	var b strings.Builder
	if iv.Months != 0 {
		fmt.Fprintf(&b, "%d mons ", iv.Months)
	}
	d := iv.Days
	n := iv.Nanos
	neg := d < 0 || (d == 0 && n < 0)
	if neg {
		b.WriteByte('-')
		d = -d
		n = -n
	}
	fmt.Fprintf(&b, "%d days %02d:%02d:%02d", d, n/3600e9, (n/60e9)%60, (n/1e9)%60)
	if rem := n % 1e9; rem != 0 {
		fmt.Fprintf(&b, ".%09d", rem)
	}
	return strings.TrimSpace(b.String())
}

// KindName is the user-facing type name.
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindNumeric:
		return "numeric"
	case KindString:
		return "text"
	case KindBool:
		return "bool"
	case KindTimestamp:
		return "timestamp"
	case KindTimestampTZ:
		return "timestamptz"
	case KindDate:
		return "date"
	case KindTime:
		return "time"
	case KindInterval:
		return "interval"
	}
	return "unknown"
}

// IsNumericKind reports whether the kind supports numeric arithmetic and
// range offsets (integer types, floats, numerics).
func (k Kind) IsNumericKind() bool {
	return k == KindInt || k == KindFloat || k == KindNumeric
}

// IsTemporalKind reports whether the kind is a date/time type.
func (k Kind) IsTemporalKind() bool {
	return k == KindTimestamp || k == KindTimestampTZ || k == KindDate || k == KindTime
}

// Compare orders two non-null values, returning -1, 0 or 1.
//
// NULLs never reach Compare: sort and partition code handles them first so
// that NULLS FIRST/LAST can be applied uniformly.
func Compare(a, b Value) (int, error) {
	switch {
	case a.Kind == KindNull || b.Kind == KindNull:
		return 0, fmt.Errorf("Compare called with NULL")
	case a.Kind.IsNumericKind() && b.Kind.IsNumericKind():
		return compareNumeric(a, b)
	case a.Kind == KindString && b.Kind == KindString:
		return strings.Compare(a.S, b.S), nil
	case a.Kind == KindBool && b.Kind == KindBool:
		return compareBool(a.B, b.B), nil
	case a.Kind.IsTemporalKind() && b.Kind.IsTemporalKind():
		// PostgreSQL compares date/timestamp/timestamptz by absolute time and
		// time-of-day within its own domain.
		if a.Kind == KindTime && b.Kind != KindTime || b.Kind == KindTime && a.Kind != KindTime {
			return 0, fmt.Errorf("cannot compare %s with %s", a.Kind, b.Kind)
		}
		return compareTime(a.T, b.T), nil
	case a.Kind == KindInterval && b.Kind == KindInterval:
		// Intervals compare by their total duration assuming 30-day months,
		// matching PostgreSQL's interval_cmp.
		an := a.D.Months*30*86400e9 + a.D.Days*86400e9 + a.D.Nanos
		bn := b.D.Months*30*86400e9 + b.D.Days*86400e9 + b.D.Nanos
		return cmpInt64(an, bn), nil
	}
	return 0, fmt.Errorf("cannot compare %s with %s", a.Kind, b.Kind)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

func compareTime(a, b time.Time) int {
	if a.Before(b) {
		return -1
	}
	if a.After(b) {
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareNumeric(a, b Value) (int, error) {
	// Float comparisons treat NaN as greater than everything else, as
	// PostgreSQL does.  Anything compared with a float is converted to float,
	// which is exact enough for frame arithmetic and ordering of the values
	// the generator produces.
	if a.Kind == KindFloat || b.Kind == KindFloat {
		af, err := ToFloat(a)
		if err != nil {
			return 0, err
		}
		bf, err := ToFloat(b)
		if err != nil {
			return 0, err
		}
		if math.IsNaN(af) || math.IsNaN(bf) {
			switch {
			case math.IsNaN(af) && math.IsNaN(bf):
				return 0, nil
			case math.IsNaN(af):
				return 1, nil
			}
			return -1, nil
		}
		return cmpFloat(af, bf), nil
	}
	if a.Kind == KindInt && b.Kind == KindInt {
		return cmpInt64(a.I, b.I), nil
	}
	// Mixed integer/numeric: compare as decimal strings via big-free parsing.
	af, err := numericToFloat(a)
	if err != nil {
		return 0, err
	}
	bf, err := numericToFloat(b)
	if err != nil {
		return 0, err
	}
	return cmpFloat(af, bf), nil
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func numericToFloat(v Value) (float64, error) {
	if v.Kind == KindInt {
		return float64(v.I), nil
	}
	f, err := strconv.ParseFloat(v.S, 64)
	if err != nil {
		return 0, fmt.Errorf("bad numeric %q", v.S)
	}
	return f, nil
}

// ToFloat converts any numeric kind to float64.
func ToFloat(v Value) (float64, error) {
	if v.Kind == KindFloat {
		return v.F, nil
	}
	return numericToFloat(v)
}

// AsInt64 converts an integer-ish value (int, integral float, integral
// numeric) to int64.
func AsInt64(v Value) (int64, error) {
	switch v.Kind {
	case KindInt:
		return v.I, nil
	case KindFloat:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) {
			return 0, fmt.Errorf("cannot convert %v to integer", v.F)
		}
		return int64(math.RoundToEven(v.F)), nil
	case KindNumeric:
		f, err := strconv.ParseFloat(v.S, 64)
		if err != nil {
			return 0, fmt.Errorf("bad numeric %q", v.S)
		}
		return int64(math.RoundToEven(f)), nil
	}
	return 0, fmt.Errorf("cannot convert %s to integer", v.Kind)
}

// AddOffset adds an offset value to a base value.  It implements the
// arithmetic PostgreSQL uses for RANGE frames:
//
//	numeric + numeric  = numeric (int stays int when possible)
//	date + interval    = timestamp
//	timestamp + interval = timestamp
//	time + interval    = time (wrapping within a day)
//	interval + interval = interval
//	interval - interval = interval
func AddOffset(base, off Value) (Value, error) {
	switch {
	case off.IsNull():
		return Null, nil
	case base.IsNull():
		return Null, nil
	case base.Kind.IsNumericKind() && off.Kind.IsNumericKind():
		if base.Kind == KindInt && off.Kind == KindInt {
			sum := base.I + off.I
			if (off.I > 0 && sum < base.I) || (off.I < 0 && sum > base.I) {
				return Float(float64(base.I) + float64(off.I)), nil
			}
			return Int(sum), nil
		}
		bf, err := ToFloat(base)
		if err != nil {
			return Null, err
		}
		of, err := ToFloat(off)
		if err != nil {
			return Null, err
		}
		return Float(bf + of), nil
	case off.Kind == KindInterval:
		iv := IntervalV(off.D)
		switch base.Kind {
		case KindTimestamp:
			return Timestamp(addInterval(base.T, off.D)), nil
		case KindTimestampTZ:
			return TimestampTZ(addInterval(base.T, off.D)), nil
		case KindDate:
			return Timestamp(addInterval(base.T, off.D)), nil
		case KindTime:
			t := addInterval(time.Date(2000, 1, 1, base.T.Hour(), base.T.Minute(), base.T.Second(), base.T.Nanosecond(), time.UTC), off.D)
			return TimeOfDay(time.Date(2000, 1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)), nil
		case KindInterval:
			return IntervalV(Interval{
				Months: base.D.Months + iv.D.Months,
				Days:   base.D.Days + iv.D.Days,
				Nanos:  base.D.Nanos + iv.D.Nanos,
			}), nil
		}
	}
	return Null, fmt.Errorf("cannot add %s offset to %s", off.Kind, base.Kind)
}

func addInterval(t time.Time, iv Interval) time.Time {
	if iv.Months != 0 {
		t = t.AddDate(0, int(iv.Months), 0)
	}
	t = t.AddDate(0, 0, int(iv.Days))
	return t.Add(time.Duration(iv.Nanos))
}

// OffsetCompatible reports whether a RANGE offset of kind offKind may be used
// with an ORDER BY column of kind orderKind.  This mirrors PostgreSQL's
// in_range support matrix for the types the engine knows about.
func OffsetCompatible(orderKind, offKind Kind) bool {
	switch {
	case orderKind.IsNumericKind() && offKind.IsNumericKind():
		return true
	case orderKind.IsTemporalKind() && offKind == KindInterval:
		return true
	case orderKind == KindInterval && offKind == KindInterval:
		return true
	}
	return false
}

// Key is a compact, branch-free comparison key for sorting.  Sorting code
// precomputes one Key per (row, sort key) so the comparator does not have to
// dispatch on kinds millions of times.
//
// KindKey* mirrors the Value kinds; only numeric and temporal kinds are
// flattened into the numeric field.  Strings and booleans keep their payload.
type Key struct {
	kind Kind
	num  float64
	ts   int64
	s    string
	b    bool
}

// MakeKey builds a comparison key for a non-null value.  NULL values are
// handled by the caller's NULLS FIRST/LAST logic.
func MakeKey(v Value) Key {
	switch v.Kind {
	case KindInt:
		return Key{kind: KindInt, num: float64(v.I), ts: v.I}
	case KindFloat:
		return Key{kind: KindFloat, num: v.F, ts: fkey(v.F)}
	case KindNumeric:
		f, err := ToFloat(v)
		if err != nil {
			f = 0
		}
		return Key{kind: KindNumeric, num: f, ts: fkey(f)}
	case KindTimestamp, KindTimestampTZ, KindDate, KindTime:
		return Key{kind: v.Kind, num: float64(v.T.UnixNano()), ts: v.T.UnixNano()}
	case KindString:
		return Key{kind: KindString, s: v.S}
	case KindBool:
		return Key{kind: KindBool, b: v.B}
	case KindInterval:
		n := v.D.Months*30*86400e9 + v.D.Days*86400e9 + v.D.Nanos
		return Key{kind: KindInterval, num: float64(n), ts: n}
	}
	return Key{kind: KindNull}
}

// fkey maps a float to an order-preserving integer (so -0.0 and 0.0 compare
// equal and NaN sorts last).
func fkey(f float64) int64 {
	if f != f {
		return 1<<63 - 1
	}
	if f == 0 {
		return 0
	}
	return int64(f*1e9) + 1<<62
}

// CompareKeys compares two keys of compatible kinds.
func CompareKeys(a, b Key) int {
	// Numeric and interval kinds share the numeric field; temporal kinds use
	// the nanosecond field.  Mixing families only happens for date/timestamp,
	// which share ts.
	if a.kind == KindString || b.kind == KindString {
		return cmpString(a.s, b.s)
	}
	if a.kind == KindBool || b.kind == KindBool {
		return compareBool(a.b, b.b)
	}
	if isTemporalKey(a.kind) && isTemporalKey(b.kind) {
		return cmpInt64(a.ts, b.ts)
	}
	return cmpFloat(a.num, b.num)
}

func isTemporalKey(k Kind) bool { return k.IsTemporalKind() }

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
