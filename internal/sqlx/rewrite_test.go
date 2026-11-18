package sqlx

import "testing"

func TestAddTieBreaker(t *testing.T) {
	cases := []struct{ in, wantContains string }{
		{
			"SELECT row_number() OVER (PARTITION BY a ORDER BY b ASC NULLS LAST) AS rn FROM t",
			"ORDER BY b ASC NULLS LAST, row_id ASC)",
		},
		{
			"SELECT sum(x) OVER (ORDER BY b ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t",
			"ORDER BY b, row_id ASC ROWS",
		},
		{
			"SELECT rank() OVER (PARTITION BY a) AS rk FROM t",
			"PARTITION BY a)", // 无 ORDER BY，不改动
		},
	}
	for _, c := range cases {
		got := AddTieBreaker(c.in)
		if !containsStr(got, c.wantContains) {
			t.Errorf("AddTieBreaker:\n in=%s\n got=%s\n want contains=%s", c.in, got, c.wantContains)
		}
	}
}

func TestAddTieBreakerIdempotent(t *testing.T) {
	in := "SELECT row_number() OVER (ORDER BY b) AS rn FROM t"
	once := AddTieBreaker(in)
	twice := AddTieBreaker(once)
	if once != twice {
		t.Errorf("not idempotent:\n once=%s\ntwice=%s", once, twice)
	}
}

func TestAddTieBreakerSkipsOffsetRange(t *testing.T) {
	cases := []string{
		"SELECT avg(x) OVER (ORDER BY t ASC RANGE BETWEEN 3 PRECEDING AND CURRENT ROW) FROM t",
		"SELECT count(x) OVER (ORDER BY t RANGE BETWEEN UNBOUNDED PRECEDING AND 5 FOLLOWING) FROM t",
		"SELECT avg(x) OVER (ORDER BY t RANGE 3 PRECEDING) FROM t",
	}
	for _, in := range cases {
		if got := AddTieBreaker(in); got != in {
			t.Errorf("offset RANGE must not get tie-breaker:\n in=%s\n got=%s", in, got)
		}
	}
	// 无偏移的默认 RANGE（UNBOUNDED..CURRENT ROW）应正常追加。
	in2 := "SELECT rank() OVER (ORDER BY b RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM t"
	if got2 := AddTieBreaker(in2); !containsStr(got2, "row_id ASC RANGE") {
		t.Errorf("default RANGE should get tie-breaker:\n got=%s", got2)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
