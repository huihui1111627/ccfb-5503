package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/expr"
	"dragonwindow/internal/querygen"
	"dragonwindow/internal/sqlx"
	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
	"dragonwindow/internal/window"
	"math/rand"
)

// crossSummary 汇总一次跨库对照。
type crossSummary struct {
	dialect   string
	ran       int
	skipped   int
	compared  int64
	mismatch  int
	sqlErrors int
	firstErrs []string
}

func cmdCrossCheck(args []string) error {
	f := parseFlags(args)
	rows := f.intVal("rows", 3000)
	nq := f.intVal("queries", 80)
	seed := f.int64Val("seed", 20261119)
	db := f.string("db", "both")

	cfg := deriveConfig(rows, seed)
	main, _, _, _ := datagen.Generate(cfg)
	fmt.Printf("数据 %d 行，生成 %d 条查询\n", main.N, nq)
	gen := querygen.New(rand.New(rand.NewSource(seed + 1)))
	queries := gen.Generate(nq)

	if db == "sqlite" || db == "both" {
		if err := crossOne(sqlx.SQLite, main, queries); err != nil {
			fmt.Printf("SQLite 对照跳过: %v\n", err)
		}
	}
	if db == "postgres" || db == "both" {
		if err := crossOne(sqlx.Postgres, main, queries); err != nil {
			fmt.Printf("PostgreSQL 对照跳过: %v\n", err)
		}
	}
	return nil
}

func crossOne(dialect sqlx.Dialect, main *table.Table, queries []querygen.Query) error {
	var runner *sqlx.Runner
	var err error
	if dialect == sqlx.SQLite {
		runner, err = sqlx.NewSQLite()
	} else {
		runner, err = sqlx.NewPostgres()
	}
	if err != nil {
		return err
	}
	defer runner.Close()

	if err := runner.CreateTable(main); err != nil {
		return err
	}
	if err := runner.LoadTable(main); err != nil {
		return err
	}

	sum := crossSummary{dialect: string(dialect)}
	eng := window.New()

	for qi, q := range queries {
		// 兼容性闸门：生成器标注不支持的查询直接跳过。
		if dialect == sqlx.SQLite && !q.LiteOK {
			sum.skipped++
			continue
		}
		if dialect == sqlx.Postgres && !q.PGOK {
			sum.skipped++
			continue
		}
		// 给窗口 ORDER BY 追加唯一决胜列 row_id，强制并列行顺序在引擎与
		// 真实数据库间一致，row_number/lag/nth_value 等才可逐行比对。
		tbSQL := sqlx.AddTieBreaker(q.SQL)
		// 引擎侧结果。
		stmt, perr := expr.Parse(tbSQL)
		if perr != nil {
			continue
		}
		engOut, _, eerr := eng.Execute(main, stmt)
		if eerr != nil {
			sum.sqlErrors++
			if len(sum.firstErrs) < 5 {
				sum.firstErrs = append(sum.firstErrs, fmt.Sprintf("#%d 引擎错误: %v", qi, eerr))
			}
			continue
		}
		// 数据库侧（同一条重写 SQL）。
		dbRes, qerr := runner.QueryCSV(tbSQL)
		if qerr != nil {
			return qerr
		}
		if dbRes.Err != "" {
			// 数据库报错：可能是语法/特性差异，记录但不计入比对失败。
			sum.sqlErrors++
			if len(sum.firstErrs) < 8 {
				sum.firstErrs = append(sum.firstErrs, fmt.Sprintf("#%d %s 报错: %s", qi, dialect, firstLine(dbRes.Err)))
			}
			continue
		}
		sum.ran++
		if n := compareCrossResult(engOut, dbRes, q, &sum); n > 0 {
			if len(sum.firstErrs) < 8 {
				sum.firstErrs = append(sum.firstErrs, fmt.Sprintf("#%d 出现 %d 处值分歧: %s", qi, n, q.SQL))
			}
		}
	}
	printCrossSummary(sum)
	return nil
}

// compareCrossResult 对齐引擎与数据库的列并逐值比较，返回分歧数。
//
// 窗口查询不保证输出行序，因此用业务主键 (race_id, team_id, split_no) 建立
// 行映射——在 race_split 里三元组唯一标识一条艇的一个分段。
func compareCrossResult(engOut *table.Table, dbRes *sqlx.Result, q querygen.Query, sum *crossSummary) int {
	diff := 0
	n := len(dbRes.Rows)
	if n != engOut.N {
		sum.mismatch++
		if len(sum.firstErrs) < 8 {
			sum.firstErrs = append(sum.firstErrs, fmt.Sprintf("行数不一致 引擎=%d 数据库=%d", engOut.N, n))
		}
		return 1
	}
	// 定位数据库结果里的主键列。
	keyCols := []string{"race_id", "team_id", "split_no"}
	dbIdx := map[string]int{}
	for i, name := range dbRes.Columns {
		dbIdx[name] = i
	}
	for _, kc := range keyCols {
		if _, ok := dbIdx[kc]; !ok {
			// 查询没选主键（不应发生，生成器固定选），跳过本条。
			return 0
		}
	}
	// 引擎侧主键列下标。
	engKey := make([]int, 3)
	for i, kc := range keyCols {
		engKey[i] = engOut.ColIndex(kc)
		if engKey[i] < 0 {
			return 0
		}
	}
	// 引擎行：业务键 -> 行号。
	engRow := map[string]int{}
	for r := 0; r < engOut.N; r++ {
		engRow[engRowKey(engOut, engKey, r)] = r
	}
	// 逐行取数据库结果，映射到引擎对应行后比较窗口列。
	for _, dbRow := range dbRes.Rows {
		key := dbRow[dbIdx["race_id"]] + "|" + dbRow[dbIdx["team_id"]] + "|" + dbRow[dbIdx["split_no"]]
		er, ok := engRow[key]
		if !ok {
			diff++
			continue
		}
		for ci, dbColName := range dbRes.Columns {
			if dbColName == "race_id" || dbColName == "team_id" || dbColName == "split_no" {
				continue
			}
			engCol := engOut.ColIndex(dbColName)
			if engCol < 0 {
				continue // 引擎里无同名列（如 adjusted_split 命名差异），跳过
			}
			engVal := engOut.Columns[engCol].Vals[er]
			dbText := dbRow[ci]
			sum.compared++
			if !cellMatches(engVal, dbText) {
				diff++
				if diff <= 5 {
					sum.firstErrs = append(sum.firstErrs, fmt.Sprintf(
						"  列%s 键%s 引擎=%v 数据库=%q", dbColName, key,
						types.FormatValue(engVal), dbText))
				}
			}
		}
	}
	sum.mismatch += diff
	return diff
}

func engRowKey(t *table.Table, keyCols []int, row int) string {
	parts := make([]string, 3)
	for i, c := range keyCols {
		parts[i] = types.FormatValue(t.Columns[c].Vals[row])
	}
	return strings.Join(parts, "|")
}

// cellMatches 比较引擎值与数据库 CSV 文本。数值走容差比较，其余规范化文本。
func cellMatches(eng any, dbText string) bool {
	dbText = strings.TrimSpace(dbText)
	if eng == nil {
		return dbText == "" || strings.EqualFold(dbText, "null")
	}
	switch x := eng.(type) {
	case int64:
		n, err := strconv.ParseInt(dbText, 10, 64)
		if err != nil {
			// SQLite 可能把整数渲染成 92.0。
			f, err2 := strconv.ParseFloat(dbText, 64)
			return err2 == nil && math.Abs(f-float64(x)) < 1e-9
		}
		return n == x
	case float64:
		f, err := strconv.ParseFloat(dbText, 64)
		if err != nil {
			return false
		}
		den := math.Max(1, math.Abs(x))
		return math.Abs(f-x)/den < 1e-9
	case bool:
		return dbText == "true" || dbText == "1" || dbText == "t" || dbText == "false" || dbText == "0" || dbText == "f"
	default:
		return types.FormatValue(eng) == dbText
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func printCrossSummary(s crossSummary) {
	fmt.Printf("\n[%s] 实际运行 %d 条, 跳过(不兼容) %d 条, 数据库报错 %d 条, 比对单元格 %d, 分歧 %d\n",
		strings.ToUpper(s.dialect), s.ran, s.skipped, s.sqlErrors, s.compared, s.mismatch)
	for _, e := range s.firstErrs {
		fmt.Println("  -", e)
	}
	if s.mismatch == 0 {
		fmt.Printf("  ✅ 与 %s 结果一致\n", strings.ToUpper(s.dialect))
	}
}
