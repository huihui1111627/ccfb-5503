package window

import (
	"dragonboat/windowengine/internal/value"
)

func kindOfValues(vals []value.Value) value.Kind {
	for _, v := range vals {
		if !v.IsNull() {
			return v.Kind
		}
	}
	return value.KindNull
}

func negate(v value.Value) (value.Value, error) {
	switch v.Kind {
	case value.KindInt:
		return value.Int(-v.I), nil
	case value.KindFloat:
		return value.Float(-v.F), nil
	case value.KindNumeric:
		r, err := value.ToRat(v)
		if err != nil {
			return value.Null, err
		}
		r.Neg(r)
		return value.Numeric(value.RatToString(r, 40)), nil
	case value.KindInterval:
		return value.IntervalV(value.Interval{Months: -v.D.Months, Days: -v.D.Days, Nanos: -v.D.Nanos}), nil
	}
	return value.Null, errBadNegate(v.Kind)
}

func errBadNegate(k value.Kind) error { return semErrf("cannot negate %s", k) }
