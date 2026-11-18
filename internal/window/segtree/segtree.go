// Package segtree 实现基于幺半群（monoid）的泛型线段树。
//
// 设计目标：窗口帧在相邻行之间滑动时，任意 [l,r) 区间的聚合都能在 O(log n)
// 内得到，而不是对帧内每行重新扫描。聚合只需提供「单位元 Identity 与结合
// 运算 Combine」，所有窗口聚合（COUNT/SUM/AVG/MIN/MAX/方差/字符串拼接）
// 都被建模成幺半群。
//
// 区间查询按从左到右的顺序归并节点，保证浮点求和的结合顺序与暴力实现
// 「逐行左折叠」完全一致，从而两套实现可以逐位（bit-identical）比对。
package segtree

// Monoid 是线段树承载的代数结构。
type Monoid[T any] interface {
	// Identity 返回单位元 m，满足 Combine(m, x) == Combine(x, m) == x。
	Identity() T
	// Combine 必须满足结合律；交换律不要求（字符串拼接依赖顺序）。
	Combine(a, b T) T
}

// Tree 是一棵迭代式（数组存储）线段树。
type Tree[T any] struct {
	n    int // 叶子数量（输入长度）
	size int // 填满到 2 的幂后的叶子容量
	t    []T
	m    Monoid[T]
}

// Build 用 leaves 构造线段树，复杂度 O(n)。
func Build[T any](m Monoid[T], leaves []T) *Tree[T] {
	n := len(leaves)
	size := 1
	for size < n {
		size <<= 1
	}
	tr := &Tree[T]{n: n, size: size, m: m}
	tr.t = make([]T, size*2)
	id := m.Identity()
	for i := range tr.t {
		tr.t[i] = id
	}
	copy(tr.t[size:size+n], leaves)
	for i := size - 1; i >= 1; i-- {
		tr.t[i] = m.Combine(tr.t[i<<1], tr.t[i<<1|1])
	}
	return tr
}

// Len 返回叶子数量。
func (tr *Tree[T]) Len() int { return tr.n }

// Update 覆盖某个叶子的值并向上更新，复杂度 O(log n)。
// 当前窗口引擎里输入列不可变，预留给以后的流式场景。
func (tr *Tree[T]) Update(pos int, v T) {
	p := tr.size + pos
	tr.t[p] = v
	for p > 1 {
		p >>= 1
		tr.t[p] = tr.m.Combine(tr.t[p<<1], tr.t[p<<1|1])
	}
}

// Query 返回半开区间 [l,r) 的聚合，l/r 越界会被夹紧。空区间返回单位元。
// 节点按左段、右段顺序归并，保持与逐行扫描相同的结合顺序。
func (tr *Tree[T]) Query(l, r int) T {
	if l < 0 {
		l = 0
	}
	if r > tr.n {
		r = tr.n
	}
	if l >= r {
		return tr.m.Identity()
	}
	l += tr.size
	r += tr.size
	var leftAcc, rightAcc T = tr.m.Identity(), tr.m.Identity()
	for l < r {
		if l&1 == 1 {
			leftAcc = tr.m.Combine(leftAcc, tr.t[l])
			l++
		}
		if r&1 == 1 {
			r--
			rightAcc = tr.m.Combine(tr.t[r], rightAcc)
		}
		l >>= 1
		r >>= 1
	}
	return tr.m.Combine(leftAcc, rightAcc)
}

// Total 返回整棵树的聚合。
func (tr *Tree[T]) Total() T { return tr.t[1] }

// Point 取叶子原值。
func (tr *Tree[T]) Point(pos int) T { return tr.t[tr.size+pos] }
