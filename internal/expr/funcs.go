package expr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// ParseInterval parses a PostgreSQL-style interval literal.  Supported forms:
//
//	'1 day', '2 hours 30 minutes', '90 seconds', '-1 day',
//	'1 year 2 mons', '01:30:00', '1 day 02:03:04.5'
func ParseInterval(s string) (value.Interval, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return value.Interval{}, fmt.Errorf("empty interval")
	}
	var iv value.Interval
	// time-of-day component may be given alone
	if t, ok := parseClock(s); ok {
		iv.Nanos = t
		return iv, nil
	}
	fields := strings.Fields(s)
	i := 0
	haveUnit := false
	for i < len(fields) {
		f := fields[i]
		if t, ok := parseClock(f); ok {
			iv.Nanos += t
			i++
			haveUnit = true
			continue
		}
		numText := f
		// "1day" form
		j := 0
		if j < len(numText) && (numText[j] == '-' || numText[j] == '+') {
			j++
		}
		for j < len(numText) && (numText[j] >= '0' && numText[j] <= '9' || numText[j] == '.') {
			j++
		}
		unit := strings.ToLower(strings.TrimSpace(numText[j:]))
		numText = numText[:j]
		if unit == "" {
			// unit is next field
			if i+1 >= len(fields) {
				return iv, fmt.Errorf("missing unit in interval %q", s)
			}
			unit = strings.ToLower(fields[i+1])
			i += 2
		} else {
			i++
		}
		unit = strings.TrimSuffix(unit, "s")
		// PostgreSQL accepts singular and plural abbreviations
		fval, err := strconv.ParseFloat(numText, 64)
		if err != nil {
			return iv, fmt.Errorf("bad interval number %q", numText)
		}
		switch unit {
		case "year", "yr", "y":
			iv.Months += int64(math.Round(fval * 12))
		case "mon", "month":
			iv.Months += int64(math.Round(fval))
		case "week", "wk", "w":
			iv.Days += int64(math.Round(fval * 7))
		case "day", "d":
			iv.Days += int64(math.Round(fval))
		case "hour", "hr", "h":
			iv.Nanos += int64(math.Round(fval * 3600e9))
		case "min", "minute", "m":
			iv.Nanos += int64(math.Round(fval * 60e9))
		case "sec", "second":
			iv.Nanos += int64(math.Round(fval * 1e9))
		case "millisecond", "milliseconds", "ms":
			iv.Nanos += int64(math.Round(fval * 1e6))
		case "microsecond", "microseconds", "us":
			iv.Nanos += int64(math.Round(fval * 1e3))
		default:
			return iv, fmt.Errorf("unknown interval unit %q", unit)
		}
		haveUnit = true
	}
	if !haveUnit {
		return iv, fmt.Errorf("cannot parse interval %q", s)
	}
	return iv, nil
}

// parseClock parses [-]HH:MM[:SS[.ffffff]] and returns nanoseconds.
func parseClock(s string) (int64, bool) {
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	hh, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, false
	}
	mm, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	var nanos int64
	if len(parts) == 3 {
		secText := parts[2]
		dot := strings.IndexByte(secText, '.')
		frac := ""
		if dot >= 0 {
			frac = secText[dot+1:]
			secText = secText[:dot]
		}
		ss, err := strconv.ParseInt(secText, 10, 64)
		if err != nil {
			return 0, false
		}
		nanos += ss * 1e9
		if frac != "" {
			if len(frac) > 9 {
				frac = frac[:9]
			}
			for len(frac) < 9 {
				frac += "0"
			}
			fn, err := strconv.ParseInt(frac, 10, 64)
			if err != nil {
				return 0, false
			}
			nanos += fn
		}
	}
	total := hh*3600e9 + mm*60e9 + nanos
	if neg {
		total = -total
	}
	return total, true
}

var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02",
	"15:04:05.999999999",
	"15:04:05",
}

func parseTemporalLiteral(typ, s string) (value.Value, error) {
	s = strings.TrimSpace(s)
	// timezone suffix for timestamptz
	if typ == "timestamptz" {
		for _, layout := range []string{"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05Z07:00", time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return value.TimestampTZ(t.UTC()), nil
			}
		}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			switch typ {
			case "date":
				return value.Date(t), nil
			case "time":
				return value.TimeOfDay(t), nil
			case "timestamptz":
				return value.TimestampTZ(t.UTC()), nil
			default:
				return value.Timestamp(t), nil
			}
		}
	}
	return value.Null, fmt.Errorf("cannot parse %s literal %q", typ, s)
}

func castExpr(e Expr, typ string) (Expr, error) {
	return &cast{xtyp: typ, x: e}, nil
}

type cast struct {
	xtyp string
	x    Expr
}

func (c *cast) HasColumn() bool  { return c.x.HasColumn() }
func (c *cast) Kind() value.Kind { return kindForTypeName(c.xtyp) }

func kindForTypeName(typ string) value.Kind {
	switch strings.ToLower(typ) {
	case "int", "int4", "int8", "bigint", "smallint", "int2", "integer":
		return value.KindInt
	case "float", "float8", "double precision", "real", "float4":
		return value.KindFloat
	case "numeric", "decimal":
		return value.KindNumeric
	case "text", "varchar", "char":
		return value.KindString
	case "date":
		return value.KindDate
	case "timestamp":
		return value.KindTimestamp
	case "timestamptz":
		return value.KindTimestampTZ
	case "time":
		return value.KindTime
	case "interval":
		return value.KindInterval
	case "bool", "boolean":
		return value.KindBool
	}
	return value.KindNull
}
func (c *cast) String() string { return "(" + c.x.String() + ")::" + c.xtyp }

func (c *cast) Eval(row table.Row) (value.Value, error) {
	v, err := c.x.Eval(row)
	if err != nil || v.IsNull() {
		return v, err
	}
	typ := c.xtyp
	switch typ {
	case "int", "int4", "int8", "bigint", "smallint", "int2", "integer":
		n, err := value.AsInt64(v)
		if err != nil {
			return value.Null, err
		}
		return value.Int(n), nil
	case "float", "float8", "double precision", "real", "float4":
		f, err := value.ToFloat(v)
		if err != nil {
			return value.Null, err
		}
		return value.Float(f), nil
	case "numeric", "decimal":
		if v.Kind == value.KindNumeric {
			return v, nil
		}
		if v.Kind == value.KindInt {
			return value.Numeric(strconv.FormatInt(v.I, 10)), nil
		}
		if v.Kind == value.KindFloat {
			return value.Numeric(strconv.FormatFloat(v.F, 'f', -1, 64)), nil
		}
		if v.Kind == value.KindString {
			if _, err := strconv.ParseFloat(v.S, 64); err != nil {
				return value.Null, fmt.Errorf("invalid numeric %q", v.S)
			}
			return value.Numeric(v.S), nil
		}
	case "text", "varchar", "char":
		return value.Str(v.Display()), nil
	case "date":
		switch v.Kind {
		case value.KindDate:
			return v, nil
		case value.KindTimestamp, value.KindTimestampTZ:
			return value.Date(v.T), nil
		case value.KindString:
			return parseTemporalLiteral("date", v.S)
		}
	case "timestamp":
		switch v.Kind {
		case value.KindTimestamp:
			return v, nil
		case value.KindDate, value.KindTimestampTZ:
			return value.Timestamp(v.T), nil
		case value.KindString:
			return parseTemporalLiteral("timestamp", v.S)
		}
	case "timestamptz":
		switch v.Kind {
		case value.KindTimestampTZ:
			return v, nil
		case value.KindTimestamp, value.KindDate:
			return value.TimestampTZ(v.T), nil
		case value.KindString:
			return parseTemporalLiteral("timestamptz", v.S)
		}
	case "time":
		switch v.Kind {
		case value.KindTime:
			return v, nil
		case value.KindString:
			return parseTemporalLiteral("time", v.S)
		}
	case "interval":
		switch v.Kind {
		case value.KindInterval:
			return v, nil
		case value.KindString:
			iv, err := ParseInterval(v.S)
			if err != nil {
				return value.Null, err
			}
			return value.IntervalV(iv), nil
		}
	case "bool", "boolean":
		switch v.Kind {
		case value.KindBool:
			return v, nil
		case value.KindInt:
			return value.Bool(v.I != 0), nil
		case value.KindString:
			switch strings.ToLower(v.S) {
			case "t", "true", "1", "yes", "on":
				return value.Bool(true), nil
			case "f", "false", "0", "no", "off":
				return value.Bool(false), nil
			}
		}
	}
	return value.Null, fmt.Errorf("cannot cast %s to %s", v.Kind, typ)
}

// compileFunc compiles scalar functions.
func compileFunc(name string, args []Expr) (Expr, error) {
	upper := strings.ToUpper(name)
	switch upper {
	case "COALESCE":
		if len(args) == 0 {
			return nil, fmt.Errorf("COALESCE requires arguments")
		}
		return &coalesce{args: args}, nil
	case "ABS":
		if len(args) != 1 {
			return nil, fmt.Errorf("ABS takes one argument")
		}
		return &scalarFunc{name: upper, args: args}, nil
	case "GREATEST", "LEAST":
		if len(args) < 1 {
			return nil, fmt.Errorf("%s requires arguments", upper)
		}
		return &scalarFunc{name: upper, args: args}, nil
	case "ROUND":
		if len(args) < 1 || len(args) > 2 {
			return nil, fmt.Errorf("ROUND takes one or two arguments")
		}
		return &scalarFunc{name: upper, args: args}, nil
	case "NULLIF":
		if len(args) != 2 {
			return nil, fmt.Errorf("NULLIF takes two arguments")
		}
		return &scalarFunc{name: upper, args: args}, nil
	}
	return nil, fmt.Errorf("unknown function %q", name)
}

type coalesce struct{ args []Expr }

func (c *coalesce) Kind() value.Kind {
	for _, a := range c.args {
		if k := KindOf(a); k != value.KindNull {
			return k
		}
	}
	return value.KindNull
}
func (c *coalesce) HasColumn() bool {
	for _, a := range c.args {
		if a.HasColumn() {
			return true
		}
	}
	return false
}
func (c *coalesce) String() string { return "coalesce(...)" }
func (c *coalesce) Eval(row table.Row) (value.Value, error) {
	for _, a := range c.args {
		v, err := a.Eval(row)
		if err != nil {
			return value.Null, err
		}
		if !v.IsNull() {
			return v, nil
		}
	}
	return value.Null, nil
}

type scalarFunc struct {
	name string
	args []Expr
}

func (s *scalarFunc) Kind() value.Kind {
	switch s.name {
	case "ABS", "ROUND":
		if len(s.args) > 0 {
			return KindOf(s.args[0])
		}
	case "GREATEST", "LEAST", "NULLIF":
		if len(s.args) > 0 {
			return KindOf(s.args[0])
		}
	}
	return value.KindNull
}
func (s *scalarFunc) HasColumn() bool {
	for _, a := range s.args {
		if a.HasColumn() {
			return true
		}
	}
	return false
}
func (s *scalarFunc) String() string { return strings.ToLower(s.name) + "(...)" }

func (s *scalarFunc) Eval(row table.Row) (value.Value, error) {
	vals := make([]value.Value, len(s.args))
	for i, a := range s.args {
		v, err := a.Eval(row)
		if err != nil {
			return value.Null, err
		}
		vals[i] = v
	}
	switch s.name {
	case "ABS":
		v := vals[0]
		if v.IsNull() {
			return value.Null, nil
		}
		switch v.Kind {
		case value.KindInt:
			if v.I < 0 {
				return value.Int(-v.I), nil
			}
			return v, nil
		case value.KindFloat:
			return value.Float(math.Abs(v.F)), nil
		case value.KindNumeric:
			r, err := value.ToRat(v)
			if err != nil {
				return value.Null, err
			}
			r.Abs(r)
			return value.Numeric(value.RatToString(r, 40)), nil
		}
		return value.Null, fmt.Errorf("ABS not defined for %s", v.Kind)
	case "GREATEST", "LEAST":
		var best value.Value
		for _, v := range vals {
			if v.IsNull() {
				// PostgreSQL ignores NULLs for GREATEST/LEAST
				continue
			}
			if best.IsNull() {
				best = v
				continue
			}
			c, err := value.Compare(v, best)
			if err != nil {
				return value.Null, err
			}
			if s.name == "GREATEST" && c > 0 || s.name == "LEAST" && c < 0 {
				best = v
			}
		}
		return best, nil
	case "ROUND":
		v := vals[0]
		if v.IsNull() {
			return value.Null, nil
		}
		digits := 0
		if len(vals) == 2 {
			d, err := value.AsInt64(vals[1])
			if err != nil {
				return value.Null, err
			}
			digits = int(d)
		}
		r, err := value.ToRat(v)
		if err != nil {
			return value.Null, err
		}
		return value.Numeric(value.RatToFixed(r, digits)), nil
	case "NULLIF":
		a, b := vals[0], vals[1]
		if a.IsNull() || b.IsNull() {
			return a, nil
		}
		c, err := value.Compare(a, b)
		if err != nil {
			return value.Null, err
		}
		if c == 0 {
			return value.Null, nil
		}
		return a, nil
	}
	return value.Null, fmt.Errorf("unknown function %s", s.name)
}
