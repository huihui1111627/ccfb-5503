package segtree

import "testing"

type sumMonoid struct{}

func (sumMonoid) Identity() int        { return 0 }
func (sumMonoid) Combine(a, b int) int { return a + b }

func TestBuildAndQuery(t *testing.T) {
	leaves := []int{1, 2, 3, 4, 5, 6, 7}
	tr := Build(sumMonoid{}, leaves)
	cases := []struct {
		l, r, want int
	}{
		{0, 7, 28},
		{0, 1, 1},
		{2, 5, 12}, // 3+4+5
		{3, 4, 4},
		{0, 0, 0},
		{5, 7, 13},
		{-3, 100, 28}, // 越界夹紧
	}
	for _, c := range cases {
		if got := tr.Query(c.l, c.r); got != c.want {
			t.Errorf("Query(%d,%d)=%d want %d", c.l, c.r, got, c.want)
		}
	}
}

func TestQueryOrderMatchesLeftFold(t *testing.T) {
	// 字符串拼接不满足交换律，用来验证 Query 的节点归并顺序 = 从左到右。
	m := concatMonoid{}
	leaves := []concat{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}}
	tr := Build(m, leaves)
	got := tr.Query(1, 5).s
	if got != "bcde" {
		t.Fatalf("ordered query = %q want bcde", got)
	}
	// 多段区间（模拟 EXCLUDE 拆段后的手动合并）由调用方保证顺序，
	// 这里验证单点查询。
	if tr.Query(0, 1).s != "a" {
		t.Fatalf("point query = %q want a", tr.Query(0, 1).s)
	}
}

type concat struct{ s string }

type concatMonoid struct{}

func (concatMonoid) Identity() concat { return concat{} }
func (concatMonoid) Combine(a, b concat) concat {
	return concat{s: a.s + b.s}
}

func TestUpdate(t *testing.T) {
	leaves := []int{1, 2, 3}
	tr := Build(sumMonoid{}, leaves)
	tr.Update(1, 10)
	if got := tr.Total(); got != 14 {
		t.Fatalf("after update total=%d want 14", got)
	}
}
