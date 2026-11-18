package sqlx

import (
	"regexp"
	"strings"
)

// 匹配窗口定义里的 ORDER BY ... 片段，截止到 ROWS/RANGE/GROUPS/EXCLUDE/右括号。
// 捕获组：1=前缀(OVER...ORDER BY)，2=排序主体，3=帧关键字/右括号。
var windowOrderRE = regexp.MustCompile(`(?i)(OVER\s*\([^)]*?ORDER\s+BY\s+)([^)]*?)(\s*(?:ROWS|RANGE|GROUPS|EXCLUDE|\)))`)

// 匹配整条「带值偏移 RANGE」的 OVER (...) 子句。
// 值偏移可以出现在起点或终点：RANGE <n|INTERVAL> ...，或 RANGE BETWEEN ...
// AND <n|INTERVAL> ...。UNBOUNDED / CURRENT ROW 不计入，因此默认帧不匹配。
var offsetRangeOverRE = regexp.MustCompile(
	`(?is)OVER\s*\([^)]*?RANGE\s+` +
		`(?:BETWEEN\s+[^)]*?(?:\d|INTERVAL)\s+(?:PRECEDING|FOLLOWING)` + // BETWEEN 任一端带值
		`|(?:\d|INTERVAL)\s+(?:PRECEDING|FOLLOWING))` + // 单边界值偏移
		`[^)]*?\)`)

// AddTieBreaker 重写 SQL，给每个含 ORDER BY 的窗口定义追加唯一决胜列 row_id，
// 使排序键完全唯一，row_number/lag/nth_value 等对并列顺序敏感的函数因此在引擎
// 与真实数据库之间得到可逐行比对的确定性结果。
//
// 例外：带值偏移的 RANGE 帧在 PostgreSQL 与本引擎里都只允许一个 ORDER BY 列，
// 追加决胜列会使其非法，故跳过；而 RANGE 按值匹配，聚合结果本就不依赖并列行序。
//
// 仅作用于 OVER (...) 内部的 ORDER BY，不触碰顶层 ORDER BY / WHERE。
func AddTieBreaker(sql string) string {
	skipSpans := offsetRangeOverRE.FindAllStringIndex(sql, -1)
	// 用匹配在原文的字节位置判断是否落在「带值偏移 RANGE」跳过区间内。
	return replaceWithIndex(sql, windowOrderRE, skipSpans)
}

// replaceWithIndex 在每个 windowOrderRE 匹配处决定是否追加决胜列。
func replaceWithIndex(sql string, re *regexp.Regexp, skipSpans [][]int) string {
	var out strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(sql, -1) {
		start, end := loc[0], loc[1]
		out.WriteString(sql[last:start])
		m := sql[start:end]
		sub := re.FindStringSubmatch(m)
		prefix, body, suffix := sub[1], sub[2], sub[3]
		switch {
		case strings.Contains(strings.ToLower(body), "row_id"):
			out.WriteString(m) // 已含决胜列
		case inAnySpan(start, skipSpans):
			out.WriteString(m) // 带值偏移的 RANGE，跳过
		default:
			out.WriteString(prefix + body + ", row_id ASC" + suffix)
		}
		last = end
	}
	out.WriteString(sql[last:])
	return out.String()
}

func inAnySpan(pos int, spans [][]int) bool {
	for _, s := range spans {
		if pos >= s[0] && pos < s[1] {
			return true
		}
	}
	return false
}
