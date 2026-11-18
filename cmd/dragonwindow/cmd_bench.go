package main

import (
	"fmt"
	"math/rand"
	"time"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/expr"
	"dragonwindow/internal/querygen"
	"dragonwindow/internal/ref"
	"dragonwindow/internal/window"
)

func cmdBench(args []string) error {
	f := parseFlags(args)
	rows := f.intVal("rows", 100000)
	nq := f.intVal("queries", 50)
	seed := f.int64Val("seed", 20261119)

	cfg := deriveConfig(rows, seed)
	main, _, _, _ := datagen.Generate(cfg)
	fmt.Printf("性能基准: %d 行, %d 条窗口查询\n", main.N, nq)
	gen := querygen.New(rand.New(rand.NewSource(seed + 1)))
	queries := gen.Generate(nq)
	eng := window.New()

	var engTotal, refTotal time.Duration
	var engWindows time.Duration
	var sortReuse int
	parsed := make([]*expr.SelectStmt, 0, len(queries))
	for _, q := range queries {
		st, err := expr.Parse(q.SQL)
		if err != nil {
			continue
		}
		parsed = append(parsed, st)
	}

	// 引擎计时（含解析后的纯执行）。
	for _, st := range parsed {
		t0 := time.Now()
		_, plan, err := eng.Execute(main, st)
		engTotal += time.Since(t0)
		if err != nil {
			continue
		}
		sortReuse += plan.SortReused
		for _, w := range plan.Windows {
			engWindows += w.TreeBuild + w.FrameCalc
		}
	}

	// 暴力参考计时（仅在可承受的规模下运行；超大数据采样前若干条）。
	refBudget := f.intVal("refmax", 20)
	refN := len(parsed)
	if refN > refBudget {
		refN = refBudget
	}
	for i := 0; i < refN; i++ {
		t1 := time.Now()
		if _, _, err := ref.Run(main, parsed[i]); err != nil {
			return err
		}
		refTotal += time.Since(t1)
	}

	printBench(main.N, len(parsed), refN, engTotal, refTotal, engWindows, sortReuse)
	return nil
}

func printBench(n, nq, refN int, engTotal, refTotal, engWindows time.Duration, reuse int) {
	avgEng := engTotal / time.Duration(max1(nq))
	avgRef := refTotal / time.Duration(max1(refN))
	fmt.Println("\n---------------- 性能基准 ----------------")
	fmt.Printf("数据规模:        %d 行\n", n)
	fmt.Printf("引擎执行 %d 条查询总耗时: %v (平均 %v/条)\n", nq, engTotal, avgEng)
	fmt.Printf("引擎建树+帧计算耗时:      %v\n", engWindows)
	fmt.Printf("暴力参考 %d 条查询总耗时: %v (平均 %v/条)\n", refN, refTotal, avgRef)
	if avgRef > 0 {
		speedup := float64(avgRef) / float64(avgEng)
		fmt.Printf("单查询平均加速比:         %.1fx\n", speedup)
	}
	fmt.Printf("共享排序复用次数:         %d\n", reuse)
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}
