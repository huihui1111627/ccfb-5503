package expr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dragonwindow/internal/types"
)

// ParseIntervalLiteral 解析 PostgreSQL 风格 interval 文本。
// 支持两种形态：
//  1. 单位后缀：'30 seconds'、'2 hours'、'500 milliseconds'（可复数）
//  2. 复合：'1 day 02:03:04.5'、'01:02:03'
//
// 结果统一为微秒。
func ParseIntervalLiteral(s string) (types.Interval, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return types.Interval{}, fmt.Errorf("invalid interval literal: empty")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = strings.TrimSpace(s[1:])
	}

	// 形态 1：数字 + 单位
	if m := intervalUnitRE.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[0]) == s {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return types.Interval{}, fmt.Errorf("invalid interval number: %s", m[1])
		}
		unit := strings.TrimSuffix(strings.ToLower(m[2]), "s") // 去复数
		mul, ok := intervalUnitMicros[unit]
		if !ok {
			return types.Interval{}, fmt.Errorf("unknown interval unit: %s", m[2])
		}
		micros := int64(n * float64(mul))
		if neg {
			micros = -micros
		}
		return types.Interval{Micros: micros}, nil
	}

	// 形态 2：复合文本，逐段吃掉 "N day(s)" 和 HH:MM:SS[.ffffff]
	rest := s
	var micros int64
	dayRE := regexp.MustCompile(`^(\d+)\s+days?\s*`)
	if m := dayRE.FindStringSubmatch(rest); m != nil {
		d, _ := strconv.ParseInt(m[1], 10, 64)
		micros += d * 86400 * 1_000_000
		rest = strings.TrimPrefix(rest, m[0])
	}
	rest = strings.TrimSpace(rest)
	if rest != "" {
		hms, err := parseHMS(rest)
		if err != nil {
			return types.Interval{}, fmt.Errorf("invalid interval literal %q: %v", s, err)
		}
		micros += hms
	}
	if neg {
		micros = -micros
	}
	return types.Interval{Micros: micros}, nil
}

var intervalUnitRE = regexp.MustCompile(`^([0-9]*\.?[0-9]+(?:[eE][+-]?[0-9]+)?)\s*([a-zA-Z]+)$`)

var intervalUnitMicros = map[string]int64{
	"microsecond": 1,
	"millisecond": 1_000,
	"second":      1_000_000,
	"minute":      60 * 1_000_000,
	"hour":        3600 * 1_000_000,
	"day":         86400 * 1_000_000,
	"week":        7 * 86400 * 1_000_000,
}

var hmsRE = regexp.MustCompile(`^(\d+):(\d{1,2}):(\d{1,2})(?:\.(\d{1,6}))?$`)

func parseHMS(s string) (int64, error) {
	m := hmsRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("expected HH:MM:SS[.ffffff]")
	}
	h, _ := strconv.ParseInt(m[1], 10, 64)
	mi, _ := strconv.ParseInt(m[2], 10, 64)
	sec, _ := strconv.ParseInt(m[3], 10, 64)
	var frac int64
	if m[4] != "" {
		f := m[4]
		for len(f) < 6 {
			f += "0"
		}
		frac, _ = strconv.ParseInt(f, 10, 64)
	}
	return h*3600_000_000 + mi*60_000_000 + sec*1_000_000 + frac, nil
}

// 支持的时间戳字面量格式（UTC 解释；带时区偏移的按偏移换算）。
var timestampLayouts = []string{
	"2006-01-02 15:04:05.999999-07:00",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999-07:00",
	"2006-01-02T15:04:05.999999Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// ParseTimestampLiteral 解析时间戳字面量。
func ParseTimestampLiteral(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp literal: %q", s)
}
