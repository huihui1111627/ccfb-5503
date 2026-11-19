// Package gen generates synthetic dragon-boat race data and a large corpus of
// random window queries for validating the engine.
package gen

import (
	"fmt"
	"math/rand"
	"time"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// Layout describes the generated input table.
type Layout struct {
	Schema *table.Schema
	Rows   []table.Row
}

// Options control data generation.
type Options struct {
	Rows      int
	Seed      int64
	NullRate  float64 // probability of a NULL in nullable columns
	Partition int     // approx number of heats/partitions
}

// DefaultOptions produce a race-like dataset of the requested size.
func DefaultOptions(rows int, seed int64) Options {
	return Options{Rows: rows, Seed: seed, NullRate: 0.06, Partition: 64}
}

// Race schema:
//
//	heat    int            -- race/heat id (partition key candidate)
//	race    int            -- race within heat
//	crew    text           -- crew name
//	lane    int            -- lane number
//	split   int            -- race split index (km markers 1..6)
//	elapsed_ms  int        -- elapsed time at the split
//	split_ms    int        -- time for this split
//	stroke_rate float      -- strokes per minute, nullable
//	speed       float      -- boat speed m/s, nullable
//	ts          timestamp  -- wall-clock time of the split, nullable
//	ts_tz       timestamptz
//	day         date
//	odd         interval
//	rank_hint   int        -- deliberately arbitrary integer
func RaceSchema() *table.Schema {
	return table.NewSchema(
		table.Column{Name: "heat", Kind: value.KindInt},
		table.Column{Name: "race", Kind: value.KindInt},
		table.Column{Name: "crew", Kind: value.KindString},
		table.Column{Name: "lane", Kind: value.KindInt},
		table.Column{Name: "split", Kind: value.KindInt},
		table.Column{Name: "elapsed_ms", Kind: value.KindInt},
		table.Column{Name: "split_ms", Kind: value.KindInt},
		table.Column{Name: "stroke_rate", Kind: value.KindFloat},
		table.Column{Name: "speed", Kind: value.KindFloat},
		table.Column{Name: "ts", Kind: value.KindTimestamp},
		table.Column{Name: "ts_tz", Kind: value.KindTimestampTZ},
		table.Column{Name: "day", Kind: value.KindDate},
		table.Column{Name: "odd", Kind: value.KindInterval},
		table.Column{Name: "rank_hint", Kind: value.KindInt},
	)
}

// Generate builds the race table.
func Generate(opts Options) (*table.Table, error) {
	if opts.Rows <= 0 {
		return nil, fmt.Errorf("rows must be positive")
	}
	if opts.Partition <= 0 {
		opts.Partition = 1
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	schema := RaceSchema()
	t := table.New(schema)

	crews := []string{"赤龙", "青龙", "白龙", "黑龙", "金龙", "银龙", "飞龙", "云龙", "蛟龙", "游龙"}
	lanesPerRace := 6
	splits := 6
	// Number of heats; each heat has races with lanesPerRace boats and splits.
	racesPerHeat := 4
	rowsPerHeat := racesPerHeat * lanesPerRace * splits
	heats := (opts.Rows + rowsPerHeat - 1) / rowsPerHeat
	if heats < 1 {
		heats = 1
	}
	baseDay := time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC)
	now := time.Now

	_ = now
	count := 0
	for h := 1; h <= heats && count < opts.Rows; h++ {
		for r := 1; r <= racesPerHeat && count < opts.Rows; r++ {
			startTS := baseDay.Add(time.Duration(h*17+r*3) * time.Minute)
			// Per-lane base pace for the race.
			pace := make([]float64, lanesPerRace)
			for l := range pace {
				pace[l] = 4.2 + rng.Float64()*1.6 // m/s
			}
			for s := 1; s <= splits && count < opts.Rows; s++ {
				for l := 1; l <= lanesPerRace && count < opts.Rows; l++ {
					splitMs := int(40000 + rng.Intn(20000))
					elapsed := int(float64(s*500) / pace[l-1] * 1000)
					stroke := 55 + rng.Float64()*40
					speed := pace[l-1] + rng.NormFloat64()*0.05
					ts := startTS.Add(time.Duration(s*90)*time.Second + time.Duration(rng.Intn(20))*time.Second)
					row := table.Row{
						value.Int(int64(h)),
						value.Int(int64(r)),
						value.Str(crews[(h+r+l)%len(crews)]),
						value.Int(int64(l)),
						value.Int(int64(s)),
						value.Int(int64(elapsed)),
						value.Int(int64(splitMs)),
						value.Float(stroke),
						value.Float(speed),
						value.Timestamp(ts),
						value.TimestampTZ(ts),
						value.Date(startTS),
						value.IntervalV(value.Interval{Nanos: int64(rng.Intn(4)) * 90e9}),
						value.Int(int64(rng.Intn(1000))),
					}
					// Apply nulls to nullable columns.
					if rng.Float64() < opts.NullRate {
						row[7] = value.Null
					}
					if rng.Float64() < opts.NullRate {
						row[8] = value.Null
					}
					if rng.Float64() < opts.NullRate {
						row[9] = value.Null
					}
					if rng.Float64() < opts.NullRate {
						row[10] = value.Null
					}
					if rng.Float64() < opts.NullRate {
						row[11] = value.Null
					}
					if rng.Float64() < opts.NullRate {
						row[12] = value.Null
					}
					t.Rows = append(t.Rows, row)
					count++
				}
			}
		}
	}
	// Shuffle so partitions are interleaved in input order (exercises the sort).
	rng.Shuffle(len(t.Rows), func(i, j int) {
		t.Rows[i], t.Rows[j] = t.Rows[j], t.Rows[i]
	})
	return t, nil
}

// SmallSchema is a tiny table used by unit tests: integer and float columns
// with plenty of duplicates and NULLs so every frame edge case is reachable.
func SmallSchema() *table.Schema {
	return table.NewSchema(
		table.Column{Name: "id", Kind: value.KindInt},
		table.Column{Name: "g", Kind: value.KindInt},
		table.Column{Name: "v", Kind: value.KindInt},
		table.Column{Name: "x", Kind: value.KindFloat},
		table.Column{Name: "s", Kind: value.KindString},
		table.Column{Name: "ts", Kind: value.KindTimestamp},
	)
}

// GenerateSmall builds a small adversarial table.
func GenerateSmall(rows int, seed int64) *table.Table {
	rng := rand.New(rand.NewSource(seed))
	t := table.New(SmallSchema())
	for i := 0; i < rows; i++ {
		g := int64(rng.Intn(4) + 1)
		var v value.Value
		if rng.Float64() < 0.25 {
			v = value.Null
		} else {
			v = value.Int(int64(rng.Intn(8) * 10))
		}
		var x value.Value
		if rng.Float64() < 0.2 {
			x = value.Null
		} else {
			x = value.Float(float64(rng.Intn(10)) / 2)
		}
		var s value.Value
		if rng.Float64() < 0.2 {
			s = value.Null
		} else {
			s = value.Str(string(rune('a' + rng.Intn(5))))
		}
		var ts value.Value
		if rng.Float64() < 0.2 {
			ts = value.Null
		} else {
			ts = value.Timestamp(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.Intn(10)) * 15 * time.Second))
		}
		t.Rows = append(t.Rows, table.Row{
			value.Int(int64(i + 1)),
			value.Int(g),
			v,
			x,
			s,
			ts,
		})
	}
	return t
}

// UniqueTable builds a table whose numeric, text and timestamp columns are
// all distinct and NULL-free.  It is used when results must be deterministic
// for cross-database comparison: ROWS frames over peer groups have
// implementation-defined row order in SQL.
func UniqueTable(rows int, seed int64) *table.Table {
	rng := rand.New(rand.NewSource(seed))
	t := table.New(SmallSchema())
	perm := rng.Perm(rows)
	for i := 0; i < rows; i++ {
		v := perm[i] * 10
		t.Rows = append(t.Rows, table.Row{
			value.Int(int64(i + 1)),
			// Every non-id column is a distinct function of perm[i], so all
			// order keys are unique, no two full rows repeat, and results of
			// order-sensitive functions are deterministic across engines.
			value.Int(int64(perm[i]*4 + 1)),
			value.Int(int64(v)),
			value.Float(float64(v) / 4),
			value.Str(fmt.Sprintf("s%06d", perm[i])),
			value.Timestamp(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(perm[i]) * 15 * time.Second)),
		})
	}
	rng.Shuffle(len(t.Rows), func(i, j int) { t.Rows[i], t.Rows[j] = t.Rows[j], t.Rows[i] })
	return t
}
