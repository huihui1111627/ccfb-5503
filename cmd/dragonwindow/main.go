// Command dragonwindow 是窗口函数引擎的命令行入口。
//
// 子命令：
//
//	gen        生成赛事数据（可选落盘 JSON）
//	querygen   生成随机窗口查询
//	verify     引擎 vs 逐行暴力参考实现，逐值差分比对
//	crosscheck 与本机 SQLite / PostgreSQL 对照
//	bench      大规模性能基准（线段树 vs 暴力扫描耗时）
//	serve      启动查询页面
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:])
	case "querygen":
		err = cmdQueryGen(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "crosscheck":
		err = cmdCrossCheck(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "scale":
		err = cmdScale(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dragonwindow — Go 窗口函数计算引擎

用法:
  dragonwindow gen        [-rows N] [-seed S] [-out dir]
  dragonwindow querygen   [-n N] [-seed S] [-out file]
  dragonwindow verify     [-rows N] [-queries N] [-seed S] [-v]
  dragonwindow crosscheck [-rows N] [-queries N] [-db sqlite|postgres|both]
  dragonwindow bench      [-rows N] [-queries N]
  dragonwindow scale      [-rows N] [-queries N]   # 大规模只跑引擎，验证可扩展性
  dragonwindow serve      [-addr :8080] [-rows N]
`)
}
