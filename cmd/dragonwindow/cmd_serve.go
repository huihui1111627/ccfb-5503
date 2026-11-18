package main

import (
	"fmt"
	"net/http"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/web"
)

func cmdServe(args []string) error {
	f := parseFlags(args)
	addr := f.string("addr", ":8080")
	rows := f.intVal("rows", 20000)
	seed := f.int64Val("seed", 20261119)

	cfg := deriveConfig(rows, seed)
	main, _, _, _ := datagen.Generate(cfg)
	fmt.Printf("载入 %d 行赛事数据，启动查询页面: http://localhost%s\n", main.N, addr)

	srv := web.NewServer(main)
	return http.ListenAndServe(addr, srv.Handler())
}
