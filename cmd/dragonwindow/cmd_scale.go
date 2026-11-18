package main

import (
	"fmt"
	"math/rand"
	"time"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/expr"
	"dragonwindow/internal/querygen"
	"dragonwindow/internal/window"
)

// cmdScale 在大规模（默认几十万行）上只跑引擎，验证其可扩展性与稳定性，
// 不运行 O(n²) 的暴力参考。正确性由中小规模的 verify/crosscheck 覆盖。
func cmdScale(args []string) error {
	f := parseFlags(args)
	rows := f.intVal("rows", 300000)
	nq := f.intVal("queries", 300)
	seed := f.int64Val("seed", 20261119)

	cfg := deriveConfig(rows, seed)
	main, _, _, _ := datagen.Generate(cfg)
	fmt.Printf("大规模引擎验证: %d 行, %d 条查询\n", main.N, nq)
	gen := querygen.New(rand.New(rand.NewSource(seed + 1)))
	queries := gen.Generate(nq)
	eng := window.New()

	var ok, failed int
	var total time.Duration
	var maxMS int64
	for i, q := range queries {
		st, err := expr.Parse(q.SQL)
		if err != nil {
			failed++
			fmt.Printf("  [解析失败 #%d] %v\n", i, err)
			continue
		}
		t0 := time.Now()
		out, plan, err := eng.Execute(main, st)
		elapsed := time.Since(t0)
		total += elapsed
		if ms := elapsed.Milliseconds(); ms > maxMS {
			maxMS = ms
		}
		if err != nil {
			failed++
			fmt.Printf("  [执行失败 #%d] %v\n      %s\n", i, err, q.SQL)
			continue
		}
		if out.N != main.N {
			failed++
			fmt.Printf("  [行数异常 #%d] got=%d want=%d\n", i, out.N, main.N)
			continue
		}
		_ = plan
		ok++
	}
	fmt.Println("\n---------------- 大规模验证结果 ----------------")
	fmt.Printf("成功 %d 条, 失败 %d 条, 数据 %d 行\n", ok, failed, main.N)
	fmt.Printf("总耗时 %v, 平均 %v/条, 最慢 %dms/条\n",
		total, total/time.Duration(max1(ok)), maxMS)
	if failed == 0 {
		fmt.Printf("✅ 引擎在 %d 行上稳定跑通全部 %d 条窗口查询\n", main.N, ok)
	}
	return nil
}
