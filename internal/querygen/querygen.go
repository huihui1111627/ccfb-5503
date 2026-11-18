// Package querygen 生成随机窗口查询，覆盖引擎支持的全部特性组合，并为每条
// 查询标注它能否在 SQLite / PostgreSQL 上运行（跨库对照用）。
package querygen

import (
	"fmt"
	"math/rand"
	"strings"
)

// Query 是一条生成的窗口查询。
type Query struct {
	SQL string
	// 窗口规格的结构化描述，供页面「窗口定义」区块直接渲染。
	Windows []WindowDesc
	// PG/SQLite 兼容性标记。
	PGOK   bool
	LiteOK bool
	// 特性标签，便于按特性统计测试覆盖。
	Features []string
}

// WindowDesc 描述一个窗口定义（人类可读 + 结构化）。
type WindowDesc struct {
	Func      string
	Partition []string
	OrderBy   []string
	FrameUnit string
	Frame     string
	Exclude   string
}

// Generator 随机查询生成器。
type Generator struct {
	rng *rand.Rand
	// 特性开关，跨库对照时可关闭不兼容特性。
	AllowExclude    bool
	AllowGroups     bool
	AllowDynamicOff bool
	AllowIgnoreNull bool
	AllowInterval   bool
}

// New 构造生成器。
func New(rng *rand.Rand) *Generator {
	return &Generator{
		rng:             rng,
		AllowExclude:    true,
		AllowGroups:     true,
		AllowDynamicOff: true,
		AllowIgnoreNull: true,
		AllowInterval:   true,
	}
}

// 可用于分区/排序/聚合的列（真实存在于 race_split）。
var intCols = []string{"team_id", "race_id", "split_no", "elapsed_sec", "lane", "penalty_sec"}
var floatCols = []string{"split_sec", "stroke_rate"}
var timeCols = []string{"split_time"}
var textCols = []string{"note"}
var nullableCols = map[string]bool{
	"stroke_rate": true, "penalty_sec": true, "note": true, "split_time": true,
	"elapsed_sec": true, "split_sec": true,
}

var rankFuncs = []string{"row_number", "rank", "dense_rank", "percent_rank", "cume_dist"}
var aggFuncs = []string{"count", "sum", "avg", "min", "max", "stddev_samp", "var_samp", "string_agg"}
var navFuncs = []string{"lag", "lead", "first_value", "last_value", "nth_value"}

// Generate 生成 n 条随机查询。
func (g *Generator) Generate(n int) []Query {
	out := make([]Query, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, g.one(i))
	}
	return out
}

func (g *Generator) one(idx int) Query {
	r := g.rng
	// 决定本条查询的「共享排序骨架」：分区键 + 排序键，多个窗口复用它，
	// 以验证「同一次排序只排一次」。
	partCol := choose(r, intCols[:3]) // team_id / race_id / split_no
	orderCol := g.pickOrderCol()
	desc := r.Intn(2) == 1
	nullsFirst := r.Intn(2) == 1
	orderSpec := orderCol + g.dirSuffix(desc, nullsFirst)

	// 一条查询里放 1~3 个窗口函数，其中至少两个共享同一排序骨架。
	k := 1 + r.Intn(3)
	var selects []string
	var descs []WindowDesc
	features := map[string]bool{}
	pgOK, liteOK := true, true

	// 第一个窗口固定为排名函数，结构简单稳定。
	seq := 0
	rankFn := choose(r, rankFuncs)
	if rankFn == "percent_rank" || rankFn == "cume_dist" {
		liteOK = liteOK && sqliteSupports(rankFn)
	}
	rankSQL := fmt.Sprintf("%s() OVER (PARTITION BY %s ORDER BY %s)", rankFn, partCol, orderSpec)
	rankAlias := fmt.Sprintf("w%d_%s", seq, rankFn)
	selects = append(selects, rankSQL+" AS "+rankAlias)
	descs = append(descs, WindowDesc{
		Func: rankFn, Partition: []string{partCol},
		OrderBy: []string{orderSpec},
	})
	features["rank:"+rankFn] = true

	for j := 1; j < k; j++ {
		seq++
		pick := r.Intn(3)
		switch pick {
		case 0:
			s, d, f, pg, lite := g.genAggregate(partCol, orderCol, orderSpec, desc, nullsFirst, features, seq)
			selects = append(selects, s)
			descs = append(descs, d)
			pgOK = pgOK && pg
			liteOK = liteOK && lite
			_ = f
		case 1:
			s, d, f, pg, lite := g.genFrameAggregate(partCol, orderCol, orderSpec, features, seq)
			selects = append(selects, s)
			descs = append(descs, d)
			pgOK = pgOK && pg
			liteOK = liteOK && lite
			_ = f
		default:
			s, d, f, pg, lite := g.genNavigation(partCol, orderSpec, features, seq)
			selects = append(selects, s)
			descs = append(descs, d)
			pgOK = pgOK && pg
			liteOK = liteOK && lite
			_ = f
		}
	}

	// 偶尔加一个基于窗口结果的标量表达式，验证投影里的窗口节点替换。
	if r.Intn(3) == 0 {
		selects = append(selects, "split_sec + 1 AS adjusted_split")
	}

	sql := fmt.Sprintf("SELECT race_id, team_id, split_no, %s FROM race_split", strings.Join(selects, ", "))

	featList := make([]string, 0, len(features))
	for f := range features {
		featList = append(featList, f)
	}
	return Query{
		SQL: sql, Windows: descs,
		PGOK: pgOK, LiteOK: liteOK, Features: featList,
	}
}

// ---- 各类窗口 -------------------------------------------------------------

func (g *Generator) genAggregate(partCol, orderCol, orderSpec string, desc, nullsFirst bool, feat map[string]bool, seq int) (string, WindowDesc, []string, bool, bool) {
	r := g.rng
	fn := choose(r, aggFuncs)
	col := g.pickAggCol(fn)
	arg := buildAggArg(fn, col)
	alias := fmt.Sprintf("w%d_%s", seq, fn)
	// string_agg 在 PostgreSQL 必须带分隔符参数；引擎也接受该形态。
	if fn == "string_agg" {
		arg = fmt.Sprintf("string_agg(%s, ',')", col)
	}
	// 默认帧（不显式写 frame），覆盖 PostgreSQL 默认帧语义。
	sql := fmt.Sprintf("%s OVER (PARTITION BY %s ORDER BY %s) AS %s",
		arg, partCol, orderSpec, alias)
	feat["agg:"+fn] = true
	feat["default-frame"] = true
	pg, lite := true, true
	if fn == "string_agg" {
		lite = false // SQLite 用 group_concat，函数名/语义不同
	}
	if fn == "stddev_samp" || fn == "var_samp" {
		lite = lite && sqliteSupports(fn)
	}
	d := WindowDesc{Func: fn, Partition: []string{partCol}, OrderBy: []string{orderSpec},
		Frame: "<default RANGE UNBOUNDED PRECEDING..CURRENT ROW>"}
	return sql, d, nil, pg, lite
}

func (g *Generator) genFrameAggregate(partCol, orderCol, orderSpec string, feat map[string]bool, seq int) (string, WindowDesc, []string, bool, bool) {
	r := g.rng
	fn := choose(r, []string{"count", "sum", "avg", "min", "max"})
	col := g.pickAggCol(fn)
	arg := buildAggArg(fn, col)

	unit := choose(r, []string{"rows", "range", "groups"})
	if unit == "groups" && !g.AllowGroups {
		unit = "rows"
	}
	// RANGE 偏移类型必须与排序列兼容：时间戳列只能用 INTERVAL。
	isTimeCol := false
	for _, c := range timeCols {
		if c == orderCol {
			isTimeCol = true
		}
	}
	if unit == "range" && isTimeCol {
		// 时间戳 RANGE 强制走 interval 路径（genFrame 内部据此生成）。
		feat["frame-unit:range-time"] = true
	}
	feat["frame-unit:"+unit] = true

	frame, dyn, interval := g.genFrame(unit, orderCol, feat)
	exclude := g.genExclude(feat)

	alias := fmt.Sprintf("w%d_f%s", seq, fn)
	sql := fmt.Sprintf("%s OVER (PARTITION BY %s ORDER BY %s %s%s) AS %s",
		arg, partCol, orderSpec, frame, exclude, alias)
	pg, lite := true, true
	if unit == "groups" {
		lite = sqliteSupports("groups")
	}
	if exclude != "" {
		lite = false // SQLite 不支持 EXCLUDE
		feat["exclude"] = true
	}
	if dyn {
		// PostgreSQL/SQLite 都要求 ROWS/GROUPS 帧偏移为常量，非常量偏移
		// （含列引用）会报 "must not contain variables"。这是引擎独有能力，
		// 仅在引擎 vs 暴力参考实现的差分测试中覆盖，跨库对照跳过。
		feat["dynamic-offset"] = true
		pg = false
		lite = false
	}
	if interval {
		feat["range-interval"] = true
		lite = false // SQLite 不支持 INTERVAL 形式的 RANGE 偏移
	}
	d := WindowDesc{
		Func: fn, Partition: []string{partCol}, OrderBy: []string{orderSpec},
		FrameUnit: unit, Frame: strings.TrimSpace(frame), Exclude: strings.TrimSpace(exclude),
	}
	return sql, d, nil, pg, lite
}

func (g *Generator) genNavigation(partCol, orderSpec string, feat map[string]bool, seq int) (string, WindowDesc, []string, bool, bool) {
	r := g.rng
	fn := choose(r, navFuncs)
	col := choose(r, floatCols)
	pg, lite := true, true
	var sql string
	switch fn {
	case "lag", "lead":
		step := 1 + r.Intn(3)
		ignore := g.AllowIgnoreNull && r.Intn(3) == 0
		ignoreClause := ""
		if ignore {
			ignoreClause = " IGNORE NULLS"
			feat["ignore-nulls"] = true
			// PostgreSQL 与 SQLite 都不支持 LAG/LEAD 的 IGNORE NULLS 语法，
			// 该能力仅在引擎 vs 暴力参考实现的差分测试中验证。
			pg = false
			lite = sqliteSupports("ignore-nulls")
		}
		// 带默认值，覆盖越界返回默认。
		sql = fmt.Sprintf("%s(%s, %d, -1)%s OVER (PARTITION BY %s ORDER BY %s) AS w%d_%s",
			fn, col, step, ignoreClause, partCol, orderSpec, seq, fn)
		feat["nav:"+fn] = true
	case "first_value", "last_value":
		ignore := g.AllowIgnoreNull && r.Intn(4) == 0
		// PostgreSQL 16 尚未实现 IGNORE NULLS 语法；SQLite 同样不支持。
		// 该能力仅在引擎 vs 暴力参考实现的差分测试中验证。
		ignoreClause := ""
		if ignore {
			ignoreClause = " IGNORE NULLS"
			feat["ignore-nulls"] = true
			pg = false
			lite = sqliteSupports("ignore-nulls")
		}
		sql = fmt.Sprintf("%s(%s) OVER (PARTITION BY %s ORDER BY %s "+
			"ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING%s) AS w%d_%s",
			fn, col, partCol, orderSpec, ignoreClause, seq, fn)
		feat["nav:"+fn] = true
	case "nth_value":
		n := 1 + r.Intn(3)
		sql = fmt.Sprintf("nth_value(%s, %d) OVER (PARTITION BY %s ORDER BY %s "+
			"ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS w%d_nth",
			col, n, partCol, orderSpec, seq)
		feat["nav:nth_value"] = true
	}
	d := WindowDesc{Func: fn, Partition: []string{partCol}, OrderBy: []string{orderSpec}}
	return sql, d, nil, pg, lite
}

// ---- 帧生成 ---------------------------------------------------------------

func (g *Generator) genFrame(unit, orderCol string, feat map[string]bool) (frame string, dynamic, interval bool) {
	r := g.rng
	// 边界形态：无界、当前行、偏移量，随机组合成合法的 BETWEEN。
	startKind := choose(r, []string{"unbounded", "current", "offset"})
	endKind := choose(r, []string{"current", "offset", "unbounded"})

	startBound := g.bound(unit, orderCol, "preceding", startKind, &dynamic, &interval)
	endBound := g.bound(unit, orderCol, "following", endKind, &dynamic, &interval)
	frame = fmt.Sprintf("%s BETWEEN %s AND %s", strings.ToUpper(unit), startBound, endBound)
	return frame, dynamic, interval
}

func (g *Generator) bound(unit, orderCol, dir, kind string, dynamic *bool, interval *bool) string {
	r := g.rng
	switch kind {
	case "unbounded":
		if dir == "preceding" {
			return "UNBOUNDED PRECEDING"
		}
		return "UNBOUNDED FOLLOWING"
	case "current":
		return "CURRENT ROW"
	}
	// 偏移量：常数或动态表达式。时间戳排序列的 RANGE 只能用 INTERVAL。
	if unit == "range" && orderCol == "split_time" {
		if !g.AllowInterval {
			return "CURRENT ROW"
		}
		*interval = true
		secs := []int{15, 30, 60, 90}[r.Intn(4)]
		return fmt.Sprintf("INTERVAL '%d seconds' %s", secs, strings.ToUpper(dir))
	}
	if g.AllowDynamicOff && r.Intn(3) == 0 {
		*dynamic = true
		// 动态偏移：用一个恒正的整型表达式，每行各自计算。
		return "1 + (split_no % 2) " + strings.ToUpper(dir)
	}
	// 常量偏移。
	switch unit {
	case "rows", "groups":
		return fmt.Sprintf("%d %s", 1+r.Intn(4), strings.ToUpper(dir))
	case "range":
		return fmt.Sprintf("%d %s", 1+r.Intn(5), strings.ToUpper(dir))
	}
	return "CURRENT ROW"
}

func (g *Generator) genExclude(feat map[string]bool) string {
	if !g.AllowExclude || g.rng.Intn(4) != 0 {
		return ""
	}
	ex := choose(g.rng, []string{"CURRENT ROW", "GROUP", "TIES", "NO OTHERS"})
	return " EXCLUDE " + ex
}

// ---- 工具 -----------------------------------------------------------------

func (g *Generator) pickOrderCol() string {
	pool := append(append([]string{}, intCols...), floatCols...)
	pool = append(pool, timeCols...)
	return choose(g.rng, pool)
}

func (g *Generator) pickAggCol(fn string) string {
	if fn == "string_agg" {
		return choose(g.rng, textCols)
	}
	if fn == "count" {
		// count 偶尔用 *，否则用任意列。
		if g.rng.Intn(4) == 0 {
			return "*"
		}
	}
	pool := append(append([]string{}, intCols...), floatCols...)
	return choose(g.rng, pool)
}

func buildAggArg(fn, col string) string {
	if col == "*" {
		return "count(*)"
	}
	return fmt.Sprintf("%s(%s)", fn, col)
}

func (g *Generator) dirSuffix(desc, nullsFirst bool) string {
	var b strings.Builder
	if desc {
		b.WriteString(" DESC")
	} else {
		b.WriteString(" ASC")
	}
	if nullsFirst {
		b.WriteString(" NULLS FIRST")
	} else {
		b.WriteString(" NULLS LAST")
	}
	return b.String()
}

func choose(r *rand.Rand, s []string) string {
	return s[r.Intn(len(s))]
}

// sqliteSupports 标注某特性在 SQLite 3.31+ 的支持情况。
func sqliteSupports(feature string) bool {
	switch feature {
	case "groups", "exclude", "ignore-nulls":
		return false // SQLite 不支持 GROUPS 帧、EXCLUDE、IGNORE NULLS
	case "percent_rank", "cume_dist":
		return true // SQLite 3.25+ 支持
	case "stddev_samp", "var_samp":
		return false // SQLite 无内置 stddev/var 聚合
	}
	return true
}
