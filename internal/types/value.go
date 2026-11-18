// Package types 定义引擎使用的标量值类型与比较规则。
//
// 值用 interface{} 承载，具体动态类型约定：
//
//	nil        -> NULL
//	int64      -> INTEGER
//	float64    -> REAL
//	string     -> TEXT
//	bool       -> BOOLEAN
//	time.Time  -> TIMESTAMP（内部统一 UTC）
//	Interval   -> INTERVAL（微秒精度）
package types

import (
	"fmt"
	"math"
	"time"
)

// Interval 表示时间间隔，微秒精度。RANGE 帧按时间偏移时使用。
type Interval struct {
	Micros int64
}

// 类型名，与 PostgreSQL 类型名风格对齐。
const (
	TypeNull      = "NULL"
	TypeInteger   = "integer"
	TypeReal      = "double precision"
	TypeText      = "text"
	TypeBoolean   = "boolean"
	TypeTimestamp = "timestamp with time zone"
	TypeInterval  = "interval"
)

// TypeName 返回值的类型名。
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return TypeNull
	case int64:
		return TypeInteger
	case float64:
		return TypeReal
	case string:
		return TypeText
	case bool:
		return TypeBoolean
	case time.Time:
		return TypeTimestamp
	case Interval:
		return TypeInterval
	default:
		panic(fmt.Sprintf("internal error: unknown value type %T", v))
	}
}

// IsNull 判断值是否为 SQL NULL。
func IsNull(v any) bool { return v == nil }

// IsNumeric 判断是否为数值类型。
func IsNumeric(v any) bool {
	switch v.(type) {
	case int64, float64:
		return true
	default:
		return false
	}
}

// Compare 比较两个非空值，返回 -1/0/1。
// int 与 float 之间按数值比较；timestamp 之间按时刻比较；
// 其余仅同类型可比较，与 PostgreSQL 行为对齐（不可比较时报错）。
func Compare(a, b any) (int, error) {
	if a == nil || b == nil {
		panic("Compare: NULL must be handled by caller")
	}
	switch x := a.(type) {
	case int64:
		switch y := b.(type) {
		case int64:
			return cmpInt64(x, y), nil
		case float64:
			return cmpFloat(float64(x), y), nil
		}
	case float64:
		switch y := b.(type) {
		case float64:
			return cmpFloat(x, y), nil
		case int64:
			return cmpFloat(x, float64(y)), nil
		}
	case string:
		if y, ok := b.(string); ok {
			switch {
			case x < y:
				return -1, nil
			case x > y:
				return 1, nil
			default:
				return 0, nil
			}
		}
	case bool:
		if y, ok := b.(bool); ok {
			if x == y {
				return 0, nil
			}
			if !x {
				return -1, nil // false < true，与 PostgreSQL 一致
			}
			return 1, nil
		}
	case time.Time:
		if y, ok := b.(time.Time); ok {
			switch {
			case x.Before(y):
				return -1, nil
			case x.After(y):
				return 1, nil
			default:
				return 0, nil
			}
		}
	case Interval:
		if y, ok := b.(Interval); ok {
			return cmpInt64(x.Micros, y.Micros), nil
		}
	}
	return 0, fmt.Errorf("operator does not exist: %s = %s", TypeName(a), TypeName(b))
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		// NaN 落到 default；本引擎不主动产生 NaN，保持确定性即可。
		return 0
	}
}

// Equal 判断两个值是否 SQL 相等（NULL 与任何值都不相等，由调用方先行处理）。
func Equal(a, b any) (bool, error) {
	c, err := Compare(a, b)
	return c == 0, err
}

// ToFloat 把数值转成 float64。
func ToFloat(v any) (float64, error) {
	switch x := v.(type) {
	case int64:
		return float64(x), nil
	case float64:
		return x, nil
	default:
		return 0, fmt.Errorf("cannot cast %s to double precision", TypeName(v))
	}
}

// ToInt 把数值转成 int64，浮点按截断（向零）处理，与 PostgreSQL 强转一致。
func ToInt(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	default:
		return 0, fmt.Errorf("cannot cast %s to integer", TypeName(v))
	}
}

// ToBool 转布尔。
func ToBool(v any) (bool, error) {
	if x, ok := v.(bool); ok {
		return x, nil
	}
	return false, fmt.Errorf("cannot cast %s to boolean", TypeName(v))
}

// ToText 转文本。
func ToText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case time.Time:
		return x.UTC().Format("2006-01-02 15:04:05.999999+00")
	case Interval:
		return FormatInterval(x)
	default:
		return FormatValue(v)
	}
}

// ToTime 转时间戳。
func ToTime(v any) (time.Time, error) {
	if x, ok := v.(time.Time); ok {
		return x, nil
	}
	return time.Time{}, fmt.Errorf("cannot cast %s to timestamp", TypeName(v))
}

// FormatValue 以确定性的文本形式渲染值（NULL 渲染为 NULL）。
func FormatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return formatFloat(x)
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case time.Time:
		return x.UTC().Format("2006-01-02 15:04:05.999999+00")
	case Interval:
		return FormatInterval(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func formatFloat(x float64) string {
	if math.IsInf(x, 1) {
		return "Infinity"
	}
	if math.IsInf(x, -1) {
		return "-Infinity"
	}
	// 'g' 精度 -1 给出最短往返表示，保证确定性。
	return fmt.Sprintf("%g", x)
}

// FormatInterval 渲染间隔为 PostgreSQL interval 字面量风格文本。
func FormatInterval(i Interval) string {
	neg := i.Micros < 0
	u := i.Micros
	if neg {
		u = -u
	}
	sec := u / 1_000_000
	frac := u % 1_000_000
	d := sec / 86400
	sec %= 86400
	h := sec / 3600
	sec %= 3600
	m := sec / 60
	s := sec % 60
	out := ""
	if d != 0 {
		out += fmt.Sprintf("%d day%s ", d, plural(d))
	}
	out += fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if frac != 0 {
		out += fmt.Sprintf(".%06d", frac)
	}
	if neg {
		return "-" + out
	}
	return out
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}
