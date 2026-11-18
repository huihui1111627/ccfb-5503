package window_test

import (
	"strings"
	"testing"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/expr"
	"dragonwindow/internal/window"
)

// TestSharedSortReuse 验证需求5：分区+排序规格相同的多个窗口只排序一次。
func TestSharedSortReuse(t *testing.T) {
	cfg := datagen.DefaultConfig()
	cfg.Races, cfg.Teams, cfg.Splits, cfg.Seed = 3, 6, 8, 20261119
	main, _, _, _ := datagen.Generate(cfg)

	// 三个窗口共享完全相同的 PARTITION BY team_id ORDER BY split_no 骨架。
	sql := `SELECT team_id,
		rank() OVER (PARTITION BY team_id ORDER BY split_no) rk,
		sum(split_sec) OVER (PARTITION BY team_id ORDER BY split_no) sm,
		avg(stroke_rate) OVER (PARTITION BY team_id ORDER BY split_no) av
	FROM race_split`
	stmt, err := expr.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	_, plan, err := window.New().Execute(main, stmt)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SortReused < 2 {
		t.Fatalf("expected at least 2 sort reuses for 3 shared-spec windows, got %d", plan.SortReused)
	}
	// 计划里只应出现一次排序阶段。
	sortStages := 0
	for _, s := range plan.Stages {
		if strings.HasPrefix(s.Name, "排序") {
			sortStages++
		}
	}
	if sortStages != 1 {
		t.Fatalf("expected exactly 1 sort stage, got %d", sortStages)
	}
}
