package window

import (
	"testing"
	"time"

	"dragonwindow/internal/expr"
	"dragonwindow/internal/types"
)

// resolverHarness 构造一个可控的 frameResolver，排序值与排列直接给定。
type resolverHarness struct {
	fr *frameResolver
}

func newHarness(plen int, orderVals []any, perm []int, desc bool, nulls string) *resolverHarness {
	keyEval := &Evaluated{Vals: orderVals}
	keys := []SortKey{{Eval: keyEval, Desc: desc, Nulls: nulls}}
	sp := SortPartition(plen, nil, []*Evaluated{keyEval},
		[]expr.OrderItem{{X: &expr.ColumnRef{Name: "k"}, Desc: desc, Nulls: nulls}}, nil)
	_ = keys
	fr := &frameResolver{
		plen: plen, unit: "rows", orderKeys: sp.Keys,
		peerStart: sp.PeerStart, peerEnd: sp.PeerEnd, perm: sp.Perm,
		offsetEval: map[expr.Node]*Evaluated{},
	}
	return &resolverHarness{fr: fr}
}

func offNode(v any) (*expr.Literal, *Evaluated) {
	n := &expr.Literal{Value: v}
	return n, &Evaluated{Vals: repeatVal(v, 16)}
}

func repeatVal(v any, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestRowsFrameBounds 验证 ROWS 单位的常量/动态偏移边界。
func TestRowsFrameBounds(t *testing.T) {
	vals := []any{int64(10), int64(20), int64(30), int64(40), int64(50)}
	h := newHarness(5, vals, nil, false, "last")
	h.fr.unit = "rows"
	node, ev := offNode(int64(2))
	h.fr.offsetEval[node] = ev
	spec := &expr.FrameSpec{Unit: "rows",
		Start: expr.FrameBound{Kind: "preceding", Offset: node},
		End:   &expr.FrameBound{Kind: "current row"},
	}
	// k=0: 2 preceding 越界钉到 0 => [0,1)
	f0, err := h.fr.resolveFrame(0, spec)
	if err != nil || f0.Lo != 0 || f0.Hi != 1 {
		t.Fatalf("k0 frame=%+v err=%v want [0,1)", f0, err)
	}
	// k=3: [1,4)
	f3, _ := h.fr.resolveFrame(3, spec)
	if f3.Lo != 1 || f3.Hi != 4 {
		t.Fatalf("k3 frame=%+v want [1,4)", f3)
	}
}

// TestDynamicOffsetPerRow 验证偏移表达式每行各自求值。
func TestDynamicOffsetPerRow(t *testing.T) {
	vals := []any{int64(10), int64(20), int64(30), int64(40), int64(50)}
	h := newHarness(5, vals, nil, false, "last")
	h.fr.unit = "rows"
	node := &expr.Literal{Value: int64(1)}
	// 每行偏移不同：1,1,3,1,1
	h.fr.offsetEval[node] = &Evaluated{Vals: []any{
		int64(1), int64(1), int64(3), int64(1), int64(1),
	}}
	spec := &expr.FrameSpec{Unit: "rows",
		Start: expr.FrameBound{Kind: "preceding", Offset: node},
		End:   &expr.FrameBound{Kind: "current row"},
	}
	f2, _ := h.fr.resolveFrame(2, spec) // offset=3 => lo = 2-3 钉到 0
	if f2.Lo != 0 || f2.Hi != 3 {
		t.Fatalf("dynamic k2 frame=%+v want [0,3)", f2)
	}
	f3, _ := h.fr.resolveFrame(3, spec) // offset=1 => lo=2
	if f3.Lo != 2 || f3.Hi != 4 {
		t.Fatalf("dynamic k3 frame=%+v want [2,4)", f3)
	}
}

// TestGroupsFrame 验证 GROUPS 单位按 peer 组定位。
func TestGroupsFrame(t *testing.T) {
	// 排序值制造两个 peer 组：[10,10] 与 [20,20,20]
	vals := []any{int64(10), int64(10), int64(20), int64(20), int64(20)}
	h := newHarness(5, vals, nil, false, "last")
	h.fr.unit = "groups"
	node, ev := offNode(int64(1))
	h.fr.offsetEval[node] = ev
	spec := &expr.FrameSpec{Unit: "groups",
		Start: expr.FrameBound{Kind: "preceding", Offset: node},
		End:   &expr.FrameBound{Kind: "current row"},
	}
	// 第二组（k=2..4）：1 group preceding => 起点回到第一组起点 0，终点当前组末尾 5。
	f2, _ := h.fr.resolveFrame(2, spec)
	if f2.Lo != 0 || f2.Hi != 5 {
		t.Fatalf("groups k2 frame=%+v want [0,5)", f2)
	}
	// 第一组（k=0）：preceding 越界钉到组 0。
	f0, _ := h.fr.resolveFrame(0, spec)
	if f0.Lo != 0 || f0.Hi != 2 {
		t.Fatalf("groups k0 frame=%+v want [0,2)", f0)
	}
}

// TestRangeIntervalFrame 验证 RANGE 时间间隔偏移。
func TestRangeIntervalFrame(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	vals := []any{
		base, base.Add(30 * time.Second), base.Add(60 * time.Second),
		base.Add(90 * time.Second),
	}
	h := newHarness(4, vals, nil, false, "last")
	h.fr.unit = "range"
	ivNode := &expr.Literal{Value: types.Interval{Micros: 30_000_000}}
	h.fr.offsetEval[ivNode] = &Evaluated{Vals: repeatVal(types.Interval{Micros: 30_000_000}, 4)}
	spec := &expr.FrameSpec{Unit: "range",
		Start: expr.FrameBound{Kind: "preceding", Offset: ivNode},
		End:   &expr.FrameBound{Kind: "current row"},
	}
	// k=3 (t=90s)：30s preceding => target 60s，起点是 t=60s 的位置 2，终点当前 peer 4。
	f3, err := h.fr.resolveFrame(3, spec)
	if err != nil {
		t.Fatal(err)
	}
	if f3.Lo != 2 || f3.Hi != 4 {
		t.Fatalf("interval k3 frame=%+v want [2,4)", f3)
	}
}

// TestExcludeVariants 验证四种 EXCLUDE 的区间拆分。
func TestExcludeVariants(t *testing.T) {
	// 单个 peer 组覆盖 [0,4)，当前行 k=1。
	peerStart := []int{0, 0, 0, 0, 0}
	peerEnd := []int{4, 4, 4, 4, 4}
	f := Frame{Lo: 0, Hi: 4}

	cases := []struct {
		rule string
		want [][2]int
	}{
		{"", [][2]int{{0, 4}}},
		{"no others", [][2]int{{0, 4}}},
		{"current row", [][2]int{{0, 1}, {2, 4}}},
		{"group", nil},             // 整组被排除
		{"ties", [][2]int{{1, 2}}}, // 只留当前行
	}
	for _, c := range cases {
		got := effectiveRanges(f, 1, peerStart, peerEnd, c.rule)
		if !rangesEqual(got, c.want) {
			t.Errorf("EXCLUDE %q: got=%v want=%v", c.rule, got, c.want)
		}
	}
}

func rangesEqual(a, b [][2]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
