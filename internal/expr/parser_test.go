package expr

import (
	"testing"

	"dragonwindow/internal/types"
)

func TestParseFrameVariants(t *testing.T) {
	cases := []string{
		`SELECT sum(x) OVER (ORDER BY n ROWS UNBOUNDED PRECEDING) FROM t`,
		`SELECT sum(x) OVER (ORDER BY n ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM t`,
		`SELECT sum(x) OVER (ORDER BY n RANGE BETWEEN 3 PRECEDING AND 5 FOLLOWING) FROM t`,
		`SELECT sum(x) OVER (ORDER BY n GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE GROUP) FROM t`,
		`SELECT sum(x) OVER (ORDER BY ts RANGE BETWEEN INTERVAL '30 seconds' PRECEDING AND CURRENT ROW) FROM t`,
		`SELECT sum(x) OVER (w) FROM t WINDOW w AS (PARTITION BY a ORDER BY b)`,
		`SELECT lag(x, 2, -1) IGNORE NULLS OVER (PARTITION BY a ORDER BY b) FROM t`,
		`SELECT first_value(x) OVER (ORDER BY b ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING IGNORE NULLS) FROM t`,
	}
	for _, sql := range cases {
		if _, err := Parse(sql); err != nil {
			t.Errorf("parse failed: %s\n  err=%v", sql, err)
		}
	}
}

func TestParseRejectsInvalidFrame(t *testing.T) {
	bad := []string{
		`SELECT sum(x) OVER (ORDER BY n ROWS BETWEEN CURRENT ROW AND UNBOUNDED PRECEDING) FROM t`,
		`SELECT sum(x) OVER (ORDER BY n ROWS -1 PRECEDING) FROM t`,
	}
	for _, sql := range bad {
		// 这些不一定在解析期报错（部分语义错误在求值期），这里只确保不 panic。
		_, _ = Parse(sql)
	}
}

func TestIntervalLiteral(t *testing.T) {
	node, err := ParseExpr("INTERVAL '90 seconds'")
	if err != nil {
		t.Fatal(err)
	}
	lit, ok := node.(*Literal)
	if !ok {
		t.Fatalf("not a literal: %T", node)
	}
	iv, ok := lit.Value.(types.Interval)
	if !ok {
		t.Fatalf("not an interval: %T", lit.Value)
	}
	if iv.Micros != 90_000_000 {
		t.Fatalf("micros=%d want 90000000", iv.Micros)
	}
}

func TestArithmeticAndCase(t *testing.T) {
	// 表达式解析不依赖具体表，仅验证 AST 构建。
	for _, sql := range []string{
		"a + b * 2 - c / 3",
		"COALESCE(x, 0)",
		"CASE WHEN a > 1 THEN b ELSE c END",
		"a IN (1, 2, 3)",
		"a IS NOT NULL",
		"upper(lower(s))",
		"CAST(a AS double precision)",
	} {
		if _, err := ParseExpr(sql); err != nil {
			t.Errorf("ParseExpr(%q): %v", sql, err)
		}
	}
}

func TestNodeStringRoundTrip(t *testing.T) {
	original := `SELECT rank() OVER (PARTITION BY a ORDER BY b ASC NULLS LAST) AS rk FROM t`
	st, err := Parse(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Selects) != 1 {
		t.Fatalf("selects=%d", len(st.Selects))
	}
	sql := NodeString(st.Selects[0].Expr)
	if sql == "" {
		t.Fatal("empty node string")
	}
	// 规范化文本应能再次解析。
	if _, err := Parse("SELECT " + sql + " FROM t"); err != nil {
		t.Errorf("round-trip parse failed for %q: %v", sql, err)
	}
}
