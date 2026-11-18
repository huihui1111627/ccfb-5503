package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/querygen"
	"dragonwindow/internal/table"
)

// deriveConfig 按目标行数反推数据规模（保持 races*teams*splits 接近 rows）。
func deriveConfig(rows int, seed int64) datagen.Config {
	c := datagen.DefaultConfig()
	c.Seed = seed
	if rows <= 0 {
		return c
	}
	// 固定 splits=12，teams 取 40，调整 races。
	c.Splits = 12
	c.Teams = 40
	perRace := c.Teams * c.Splits // 480
	c.Races = (rows + perRace - 1) / perRace
	if c.Races < 1 {
		c.Races = 1
	}
	return c
}

func cmdGen(args []string) error {
	f := parseFlags(args)
	rows := f.intVal("rows", 300000)
	seed := f.int64Val("seed", 20261119)
	outDir := f.string("out", "")

	cfg := deriveConfig(rows, seed)
	main, teams, races, _ := datagen.Generate(cfg)
	fmt.Printf("生成 race_split: %d 行 (races=%d teams=%d splits=%d)\n",
		main.N, cfg.Races, cfg.Teams, cfg.Splits)
	fmt.Printf("维度表 dim_team=%d 行, dim_race=%d 行\n", teams.N, races.N)

	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		for _, tb := range []*table.Table{main, teams, races} {
			if err := writeJSON(filepath.Join(outDir, tb.Name+".json"), tb); err != nil {
				return err
			}
		}
		fmt.Printf("已写入 %s/{race_split,dim_team,dim_race}.json\n", outDir)
	}
	return nil
}

func cmdQueryGen(args []string) error {
	f := parseFlags(args)
	n := f.intVal("n", 300)
	seed := f.int64Val("seed", 777)
	outFile := f.string("out", "")

	g := querygen.New(rand.New(rand.NewSource(seed)))
	queries := g.Generate(n)
	fmt.Printf("生成 %d 条随机窗口查询\n", len(queries))

	// 特性覆盖统计。
	feats := map[string]int{}
	pgN, liteN := 0, 0
	for _, q := range queries {
		for _, ft := range q.Features {
			feats[ft]++
		}
		if q.PGOK {
			pgN++
		}
		if q.LiteOK {
			liteN++
		}
	}
	fmt.Printf("PostgreSQL 可运行: %d, SQLite 可运行: %d\n", pgN, liteN)
	fmt.Println("特性覆盖:")
	for ft, c := range feats {
		fmt.Printf("  %-22s %d\n", ft, c)
	}

	if outFile != "" {
		b, err := json.MarshalIndent(queries, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(outFile, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("已写入 %s\n", outFile)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
