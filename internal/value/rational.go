package value

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Rational arithmetic helpers.  They are used by the expression layer for
// numeric arithmetic and by aggregation for exact integer/numeric sums.

// ToRat converts an int or numeric value to a big.Rat.
func ToRat(v Value) (*big.Rat, error) {
	switch v.Kind {
	case KindInt:
		return new(big.Rat).SetInt64(v.I), nil
	case KindNumeric:
		r, ok := new(big.Rat).SetString(strings.TrimSpace(v.S))
		if !ok {
			return nil, fmt.Errorf("bad numeric %q", v.S)
		}
		return r, nil
	case KindFloat:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) {
			return nil, fmt.Errorf("cannot convert %v to numeric", v.F)
		}
		return new(big.Rat).SetFloat64(v.F), nil
	}
	return nil, fmt.Errorf("cannot convert %s to numeric", v.Kind)
}

// RatToString renders a rational as an exact decimal when the denominator is
// a product of 2s and 5s, and otherwise rounds to at most digits fractional
// digits.  Trailing zeros are removed (except that a plain integer keeps no
// decimal point).
func RatToString(r *big.Rat, digits int) string {
	if r.IsInt() {
		return r.Num().String()
	}
	// Determine whether the decimal expansion terminates.
	den := new(big.Int).Set(r.Denom())
	two := big.NewInt(2)
	five := big.NewInt(5)
	rem := new(big.Int)
	for {
		q, m := new(big.Int).QuoRem(den, two, rem)
		if m.Sign() != 0 {
			break
		}
		den = q
	}
	for {
		q, m := new(big.Int).QuoRem(den, five, rem)
		if m.Sign() != 0 {
			break
		}
		den = q
	}
	terminates := den.Cmp(big.NewInt(1)) == 0
	if terminates {
		// Use enough digits to represent the exact expansion.
		// denominator is 2^a*5^b; digits = max(a,b).
		scale := 0
		if r.Denom().Cmp(big.NewInt(1)) != 0 {
			n := new(big.Int).Set(r.Denom())
			a, b := 0, 0
			q := new(big.Int)
			for {
				q.QuoRem(n, two, rem)
				if rem.Sign() != 0 {
					break
				}
				n = new(big.Int).Set(q)
				a++
			}
			n = new(big.Int).Set(r.Denom())
			for {
				q.QuoRem(n, five, rem)
				if rem.Sign() != 0 {
					break
				}
				n = new(big.Int).Set(q)
				b++
			}
			if a > b {
				scale = a
			} else {
				scale = b
			}
		}
		s := r.FloatString(scale)
		return trimDecimal(s)
	}
	return trimDecimal(r.FloatString(digits))
}

func trimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

// RatToFixed renders with exactly digits fractional digits (rounding half
// away from zero), used to match PostgreSQL's numeric average output.
func RatToFixed(r *big.Rat, digits int) string {
	return r.FloatString(digits)
}
