// Package datagen 生成龙舟赛事的模拟数据。
//
// 主表 race_split 记录每条艇在每个分段点的计时（长表），覆盖窗口引擎需要
//
//	exercising 的全部数据形态：多类型列、可空列、并列值、时间戳、文本。
package datagen

import (
	"math"
	"math/rand"
	"strconv"
	"time"

	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
)

// Config 控制数据规模与分布。
type Config struct {
	Races    int // 比赛场次
	Teams    int // 每支参赛队
	Splits   int // 每场分段数（起点+途中划+冲线）
	NullRate float64
	TieRate  float64 // 制造并列（分段秒数撞值）的概率
	Seed     int64
}

// DefaultConfig 给出几十万行规模的默认参数。
func DefaultConfig() Config {
	return Config{
		Races: 60, Teams: 40, Splits: 12,
		NullRate: 0.08, TieRate: 0.18, Seed: 20261119,
	}
}

// RowCount 返回配置对应的总行数。
func (c Config) RowCount() int { return c.Races * c.Teams * c.Splits }

// Generate 生成主表 race_split 与两张小维度表。
func Generate(cfg Config) (main *table.Table, teams, races *table.Table, rng *rand.Rand) {
	rng = rand.New(rand.NewSource(cfg.Seed))

	main = table.New("race_split", []struct{ Name, Kind string }{
		{"row_id", types.TypeInteger},
		{"race_id", types.TypeInteger},
		{"team_id", types.TypeInteger},
		{"split_no", types.TypeInteger},
		{"elapsed_sec", types.TypeInteger},  // 累计用时（秒）
		{"split_sec", types.TypeReal},       // 本分段用时（秒，保留一位小数）
		{"stroke_rate", types.TypeReal},     // 桨频（次/分）
		{"split_time", types.TypeTimestamp}, // 到达分段点的时刻
		{"lane", types.TypeInteger},         // 航道
		{"dnf", types.TypeBoolean},          // 是否未完赛
		{"penalty_sec", types.TypeInteger},  // 罚时（可空）
		{"note", types.TypeText},            // 文本备注（可空）
	})

	baseTime := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	rowID := int64(0)

	notes := []string{"steady", "sprint", "turn", "recovery", "draft", "push"}

	for r := 0; r < cfg.Races; r++ {
		raceID := int64(1000 + r)
		raceStart := baseTime.Add(time.Duration(r*37) * time.Minute)
		for tm := 0; tm < cfg.Teams; tm++ {
			teamID := int64(200 + tm)
			lane := int64((tm % 6) + 1)
			dnfAt := -1
			if rng.Float64() < 0.03 {
				dnfAt = 1 + rng.Intn(cfg.Splits) // 在某一分段后弃赛
			}
			elapsed := int64(0)
			clock := raceStart
			// 每队一个基础配速，让成绩有层次但保留随机性。
			basePace := 85.0 + rng.Float64()*15
			for s := 0; s < cfg.Splits; s++ {
				rowID++
				splitNo := int64(s + 1)
				dnf := dnfAt >= 0 && s >= dnfAt

				// 分段用时：基础配速 + 波动，末段冲刺略快。
				pace := basePace + rng.NormFloat64()*3.0
				if s == cfg.Splits-1 {
					pace -= 2.5
				}
				splitSec := round1(pace)

				// 制造并列：随机把分段秒数对齐到最近的整数档。
				if rng.Float64() < cfg.TieRate {
					splitSec = float64(int64(pace + 0.5))
				}

				// 桨频：滑动平均桨频的原始信号，随分段起伏，带噪声。
				stroke := 68.0 + 12.0*wave(s, cfg.Splits) + rng.NormFloat64()*1.5
				stroke = round1(stroke)

				elapsed += int64(pace + 0.5)
				clock = clock.Add(time.Duration(pace * float64(time.Second)))

				// 可空列。
				var strokeVal any = stroke
				if rng.Float64() < cfg.NullRate {
					strokeVal = nil
				}
				var penalty any
				if rng.Float64() < 0.12 {
					penalty = int64(2 * (1 + rng.Intn(5)))
				}
				var noteVal any
				if rng.Float64() < 0.55 {
					noteVal = notes[rng.Intn(len(notes))]
				}
				splitTime := clock
				if dnf && s > dnfAt {
					// 弃赛后的计时点置空，制造时间戳空值。
					main.AppendRow([]any{
						rowID, raceID, teamID, splitNo,
						nil, nil, strokeVal, nil, lane, dnf, penalty, noteVal,
					})
					continue
				}
				main.AppendRow([]any{
					rowID, raceID, teamID, splitNo,
					elapsed, splitSec, strokeVal, splitTime, lane, dnf, penalty, noteVal,
				})
			}
		}
	}

	// 维度表：teams。
	teams = table.New("dim_team", []struct{ Name, Kind string }{
		{"team_id", types.TypeInteger},
		{"team_name", types.TypeText},
		{"club", types.TypeText},
	})
	clubs := []string{"Dragon", "Pearl", "Lotus", "Tide", "Phoenix", "Harbor"}
	for tm := 0; tm < cfg.Teams; tm++ {
		teams.AppendRow([]any{
			int64(200 + tm),
			"Team-" + pad(200+tm),
			clubs[tm%len(clubs)],
		})
	}

	// 维度表：races。
	races = table.New("dim_race", []struct{ Name, Kind string }{
		{"race_id", types.TypeInteger},
		{"race_name", types.TypeText},
		{"distance_m", types.TypeInteger},
		{"race_time", types.TypeTimestamp},
	})
	for r := 0; r < cfg.Races; r++ {
		races.AppendRow([]any{
			int64(1000 + r),
			"Dragon-Bowl-2026-R" + pad(r),
			int64(500 + 200*(r%3)),
			baseTime.Add(time.Duration(r*37) * time.Minute),
		})
	}
	return main, teams, races, rng
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

func wave(i, n int) float64 {
	// 平滑的正弦起伏，模拟比赛中桨频的节奏变化。
	if n <= 1 {
		return 0
	}
	x := float64(i) / float64(n-1)
	return 0.5 + 0.5*sinpi(2*x)
}

// sinpi 显式包装 math.Sin，语义清晰。
func sinpi(x float64) float64 { return math.Sin(x) }

func pad(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
