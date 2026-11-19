// Package expr implements the expression language used by the window engine:
// column references, literals, unary/binary arithmetic, comparisons, boolean
// logic and a small set of scalar functions.  Expressions are compiled once
// against a schema and then evaluated per row, or in "constant" mode when the
// expression contains no column references (frame offsets, for example).
package expr

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// Expr is a compiled expression.
type Expr interface {
	// Eval evaluates the expression in the context of one row.  row may be
	// nil for constant expressions.
	Eval(row table.Row) (value.Value, error)
	// HasColumn reports whether the expression references any column.
	HasColumn() bool
	String() string
}

// Env is the compilation environment: a schema plus the set of column names
// visible from a table alias.
type Env struct {
	Schema *table.Schema
	// Aliases maps visible names to column indexes.  When nil, schema names
	// are used directly.
	Aliases map[string]int
}

// Resolve maps a column name to an index.
func (e *Env) Resolve(name string) (int, bool) {
	if e.Aliases != nil {
		i, ok := e.Aliases[name]
		return i, ok
	}
	if e.Schema == nil {
		return 0, false
	}
	return e.Schema.Index(name)
}

// KindOf returns the declared kind of a resolved column.
func (e *Env) KindOf(idx int) value.Kind { return e.Schema.Columns[idx].Kind }

// ---------- literals ----------

type literal struct{ v value.Value }

func (l *literal) Eval(table.Row) (value.Value, error) { return l.v, nil }
func (l *literal) HasColumn() bool                     { return false }
func (l *literal) Kind() value.Kind                    { return l.v.Kind }
func (l *literal) String() string                      { return l.v.String() }

// Lit wraps a constant value as an expression.
func Lit(v value.Value) Expr { return &literal{v: v} }

// ---------- column reference ----------

type columnRef struct {
	idx  int
	name string
	kind value.Kind
}

func (c *columnRef) Eval(row table.Row) (value.Value, error) {
	if row == nil || c.idx >= len(row) {
		return value.Null, fmt.Errorf("column %s out of range", c.name)
	}
	return row[c.idx], nil
}
func (c *columnRef) HasColumn() bool  { return true }
func (c *columnRef) Kind() value.Kind { return c.kind }
func (c *columnRef) String() string   { return c.name }

// Column resolves a column reference by name.
func Column(env *Env, name string) (Expr, error) {
	idx, ok := env.Resolve(name)
	if !ok {
		return nil, fmt.Errorf("unknown column %q", name)
	}
	return &columnRef{idx: idx, name: name, kind: env.KindOf(idx)}, nil
}

// ---------- arithmetic ----------

type unary struct {
	op string
	x  Expr
}

func (u *unary) Eval(row table.Row) (value.Value, error) {
	x, err := u.x.Eval(row)
	if err != nil {
		return value.Null, err
	}
	if x.IsNull() {
		return value.Null, nil
	}
	switch u.op {
	case "-":
		switch x.Kind {
		case value.KindInt:
			return value.Int(-x.I), nil
		case value.KindFloat:
			return value.Float(-x.F), nil
		case value.KindNumeric:
			r, err := value.ToRat(x)
			if err != nil {
				return value.Null, err
			}
			r.Neg(r)
			return value.Numeric(value.RatToString(r, 40)), nil
		case value.KindInterval:
			return value.IntervalV(value.Interval{Months: -x.D.Months, Days: -x.D.Days, Nanos: -x.D.Nanos}), nil
		}
		return value.Null, fmt.Errorf("cannot negate %s", x.Kind)
	case "+":
		return x, nil
	}
	return value.Null, fmt.Errorf("unknown unary operator %q", u.op)
}
func (u *unary) HasColumn() bool { return u.x.HasColumn() }
func (u *unary) Kind() value.Kind {
	k := KindOf(u.x)
	if u.op == "-" && (k == value.KindInterval || k == value.KindInt || k == value.KindFloat || k == value.KindNumeric) {
		return k
	}
	return k
}
func (u *unary) String() string { return u.op + u.x.String() }

type binary struct {
	op   string
	l, r Expr
}

func (b *binary) HasColumn() bool { return b.l.HasColumn() || b.r.HasColumn() }
func (b *binary) Kind() value.Kind {
	switch b.op {
	case "=", "<>", "<", "<=", ">", ">=", "AND", "OR":
		return value.KindBool
	}
	lk, rk := KindOf(b.l), KindOf(b.r)
	if b.op == "-" && lk.IsTemporalKind() && rk.IsTemporalKind() {
		return value.KindInterval
	}
	if lk == value.KindInterval || rk == value.KindInterval {
		if b.op == "*" || b.op == "/" {
			if lk == value.KindInterval {
				return value.KindInterval
			}
			return value.KindInterval
		}
		return value.KindInterval
	}
	if lk == value.KindFloat || rk == value.KindFloat {
		return value.KindFloat
	}
	if lk == value.KindNumeric || rk == value.KindNumeric {
		return value.KindNumeric
	}
	if lk == value.KindInt && rk == value.KindInt {
		return value.KindInt
	}
	if lk != value.KindNull {
		return lk
	}
	return rk
}

// KindOf returns the statically known kind of an expression, or KindNull when
// it depends on runtime data.
func KindOf(e Expr) value.Kind {
	if k, ok := e.(interface{ Kind() value.Kind }); ok {
		return k.Kind()
	}
	return value.KindNull
}
func (b *binary) String() string { return "(" + b.l.String() + " " + b.op + " " + b.r.String() + ")" }

func (b *binary) Eval(row table.Row) (value.Value, error) {
	l, err := b.l.Eval(row)
	if err != nil {
		return value.Null, err
	}
	r, err := b.r.Eval(row)
	if err != nil {
		return value.Null, err
	}
	return EvalBinary(b.op, l, r)
}

// EvalBinary applies a binary operator to two values with SQL NULL semantics.
func EvalBinary(op string, l, r value.Value) (value.Value, error) {
	switch op {
	case "AND", "OR":
		return evalBoolLogic(op, l, r)
	}
	if l.IsNull() || r.IsNull() {
		return value.Null, nil
	}
	switch op {
	case "=", "<>", "<", "<=", ">", ">=":
		c, err := value.Compare(l, r)
		if err != nil {
			return value.Null, err
		}
		switch op {
		case "=":
			return value.Bool(c == 0), nil
		case "<>":
			return value.Bool(c != 0), nil
		case "<":
			return value.Bool(c < 0), nil
		case "<=":
			return value.Bool(c <= 0), nil
		case ">":
			return value.Bool(c > 0), nil
		case ">=":
			return value.Bool(c >= 0), nil
		}
	case "+", "-", "*", "/", "%":
		return evalArith(op, l, r)
	}
	return value.Null, fmt.Errorf("unknown operator %q", op)
}

func evalBoolLogic(op string, l, r value.Value) (value.Value, error) {
	lb, err := truth3(l)
	if err != nil {
		return value.Null, err
	}
	rb, err := truth3(r)
	if err != nil {
		return value.Null, err
	}
	if op == "AND" {
		if lb == 0 || rb == 0 {
			return value.Bool(false), nil
		}
		if lb < 0 || rb < 0 {
			return value.Null, nil
		}
		return value.Bool(true), nil
	}
	// OR
	if lb > 0 || rb > 0 {
		return value.Bool(true), nil
	}
	if lb < 0 || rb < 0 {
		return value.Null, nil
	}
	return value.Bool(false), nil
}

// truth3 maps a value to 1/0/-1 (unknown).
func truth3(v value.Value) (int, error) {
	if v.IsNull() {
		return -1, nil
	}
	if v.Kind != value.KindBool {
		return 0, fmt.Errorf("expected boolean, got %s", v.Kind)
	}
	if v.B {
		return 1, nil
	}
	return 0, nil
}

func evalArith(op string, l, r value.Value) (value.Value, error) {
	// Interval arithmetic.
	if l.Kind == value.KindInterval || r.Kind == value.KindInterval {
		return evalIntervalArith(op, l, r)
	}
	// Temporal arithmetic: timestamp ± interval already handled above.
	if l.Kind.IsTemporalKind() && r.Kind.IsTemporalKind() {
		if op != "-" {
			return value.Null, fmt.Errorf("unsupported temporal operator %q", op)
		}
		return temporalDiff(l, r)
	}
	if !l.Kind.IsNumericKind() || !r.Kind.IsNumericKind() {
		return value.Null, fmt.Errorf("operator %q not defined for %s and %s", op, l.Kind, r.Kind)
	}
	// Integer arithmetic stays exact, with overflow falling back to numeric.
	if l.Kind == value.KindInt && r.Kind == value.KindInt {
		switch op {
		case "+":
			s := l.I + r.I
			if (r.I > 0 && s < l.I) || (r.I < 0 && s > l.I) {
				return ratArith(op, l, r)
			}
			return value.Int(s), nil
		case "-":
			s := l.I - r.I
			if (r.I < 0 && s < l.I) || (r.I > 0 && s > l.I) {
				return ratArith(op, l, r)
			}
			return value.Int(s), nil
		case "*":
			if l.I != 0 && (l.I*r.I)/l.I != r.I {
				return ratArith(op, l, r)
			}
			return value.Int(l.I * r.I), nil
		case "/":
			if r.I == 0 {
				return value.Null, fmt.Errorf("division by zero")
			}
			if l.I%r.I == 0 {
				return value.Int(l.I / r.I), nil
			}
			return ratArith(op, l, r)
		case "%":
			if r.I == 0 {
				return value.Null, fmt.Errorf("division by zero")
			}
			return value.Int(l.I % r.I), nil
		}
	}
	if l.Kind == value.KindFloat || r.Kind == value.KindFloat {
		lf, err := value.ToFloat(l)
		if err != nil {
			return value.Null, err
		}
		rf, err := value.ToFloat(r)
		if err != nil {
			return value.Null, err
		}
		switch op {
		case "+":
			return value.Float(lf + rf), nil
		case "-":
			return value.Float(lf - rf), nil
		case "*":
			return value.Float(lf * rf), nil
		case "/":
			if rf == 0 {
				return value.Null, fmt.Errorf("division by zero")
			}
			return value.Float(lf / rf), nil
		case "%":
			return value.Float(math.Mod(lf, rf)), nil
		}
	}
	return ratArith(op, l, r)
}

func ratArith(op string, l, r value.Value) (value.Value, error) {
	lr, err := value.ToRat(l)
	if err != nil {
		return value.Null, err
	}
	rr, err := value.ToRat(r)
	if err != nil {
		return value.Null, err
	}
	out := new(big.Rat)
	switch op {
	case "+":
		out.Add(lr, rr)
	case "-":
		out.Sub(lr, rr)
	case "*":
		out.Mul(lr, rr)
	case "/":
		if rr.Sign() == 0 {
			return value.Null, fmt.Errorf("division by zero")
		}
		out.Quo(lr, rr)
	case "%":
		return value.Null, fmt.Errorf("modulo of numerics not supported")
	default:
		return value.Null, fmt.Errorf("unknown arithmetic operator %q", op)
	}
	return value.Numeric(value.RatToString(out, 40)), nil
}

func evalIntervalArith(op string, l, r value.Value) (value.Value, error) {
	if l.Kind == value.KindInterval && r.Kind == value.KindInterval {
		switch op {
		case "+":
			return value.IntervalV(value.Interval{Months: l.D.Months + r.D.Months, Days: l.D.Days + r.D.Days, Nanos: l.D.Nanos + r.D.Nanos}), nil
		case "-":
			return value.IntervalV(value.Interval{Months: l.D.Months - r.D.Months, Days: l.D.Days - r.D.Days, Nanos: l.D.Nanos - r.D.Nanos}), nil
		}
		return value.Null, fmt.Errorf("operator %q not defined for intervals", op)
	}
	// interval * numeric scales the interval; interval / numeric divides it.
	var iv value.Interval
	var num value.Value
	scale, flip := false, false
	switch {
	case l.Kind == value.KindInterval && r.Kind.IsNumericKind():
		iv, num, scale = l.D, r, true
	case r.Kind == value.KindInterval && l.Kind.IsNumericKind():
		iv, num, scale = r.D, l, true
		flip = true
	}
	if scale {
		f, err := value.ToFloat(num)
		if err != nil {
			return value.Null, err
		}
		if flip && op != "*" {
			return value.Null, fmt.Errorf("cannot divide numeric by interval")
		}
		switch op {
		case "*":
			return value.IntervalV(value.Interval{Months: int64(math.Round(float64(iv.Months) * f)), Days: int64(math.Round(float64(iv.Days) * f)), Nanos: int64(math.Round(float64(iv.Nanos) * f))}), nil
		case "/":
			if f == 0 {
				return value.Null, fmt.Errorf("division by zero")
			}
			return value.IntervalV(value.Interval{Months: int64(math.Round(float64(iv.Months) / f)), Days: int64(math.Round(float64(iv.Days) / f)), Nanos: int64(math.Round(float64(iv.Nanos) / f))}), nil
		}
	}
	return value.Null, fmt.Errorf("operator %q not defined for %s and %s", op, l.Kind, r.Kind)
}

func temporalDiff(l, r value.Value) (value.Value, error) {
	lt, err := toTime(l)
	if err != nil {
		return value.Null, err
	}
	rt, err := toTime(r)
	if err != nil {
		return value.Null, err
	}
	d := lt.Sub(rt)
	return value.IntervalV(value.Interval{Nanos: int64(d)}), nil
}

func toTime(v value.Value) (time.Time, error) {
	switch v.Kind {
	case value.KindTimestamp, value.KindTimestampTZ, value.KindDate, value.KindTime:
		return v.T, nil
	}
	return time.Time{}, fmt.Errorf("expected temporal value, got %s", v.Kind)
}

// CompileBinary builds a binary expression node.
func CompileBinary(op string, l, r Expr) Expr { return &binary{op: op, l: l, r: r} }

// CompileUnary builds a unary expression node.
func CompileUnary(op string, x Expr) Expr { return &unary{op: op, x: x} }

// ParseLiteral parses a literal token from the query language.
func ParseLiteral(tok string) (value.Value, error) {
	tok = strings.TrimSpace(tok)
	if len(tok) >= 2 && (tok[0] == '\'' && tok[len(tok)-1] == '\'') {
		return value.Str(strings.ReplaceAll(tok[1:len(tok)-1], "''", "'")), nil
	}
	switch strings.ToLower(tok) {
	case "null":
		return value.Null, nil
	case "true":
		return value.Bool(true), nil
	case "false":
		return value.Bool(false), nil
	}
	if i, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return value.Int(i), nil
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		return value.Float(f), nil
	}
	return value.Null, fmt.Errorf("cannot parse literal %q", tok)
}

// NumericLiteral creates a decimal numeric literal, used by the SQL parser.
func NumericLiteral(text string) Expr {
	return Lit(value.Numeric(text))
}
