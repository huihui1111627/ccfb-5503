package table

import (
	"testing"
	"time"

	"dragonwindow/internal/types"
)

func TestJSONRoundTrip(t *testing.T) {
	src := New("t", []struct{ Name, Kind string }{
		{"i", types.TypeInteger},
		{"f", types.TypeReal},
		{"s", types.TypeText},
		{"b", types.TypeBoolean},
		{"ts", types.TypeTimestamp},
		{"iv", types.TypeInterval},
	})
	when := time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC)
	src.AppendRow([]any{int64(42), 3.14159, "hello", true, when, types.Interval{Micros: 1_500_000}})
	src.AppendRow([]any{nil, nil, nil, nil, nil, nil})

	b, err := src.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var dst Table
	if err := dst.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if dst.N != 2 || len(dst.Columns) != 6 {
		t.Fatalf("shape mismatch: n=%d cols=%d", dst.N, len(dst.Columns))
	}
	// 第一行逐列校验具体动态类型。
	if got := dst.Columns[0].Vals[0]; got != int64(42) {
		t.Errorf("int = %v (%T)", got, got)
	}
	if got := dst.Columns[1].Vals[0]; got != 3.14159 {
		t.Errorf("float = %v", got)
	}
	if got := dst.Columns[2].Vals[0]; got != "hello" {
		t.Errorf("text = %v", got)
	}
	if got := dst.Columns[3].Vals[0]; got != true {
		t.Errorf("bool = %v", got)
	}
	if got, ok := dst.Columns[4].Vals[0].(time.Time); !ok || !got.Equal(when) {
		t.Errorf("timestamp = %v", dst.Columns[4].Vals[0])
	}
	if got, ok := dst.Columns[5].Vals[0].(types.Interval); !ok || got.Micros != 1_500_000 {
		t.Errorf("interval = %v", dst.Columns[5].Vals[0])
	}
	// 第二行全部 NULL。
	for c := 0; c < 6; c++ {
		if dst.Columns[c].Vals[1] != nil {
			t.Errorf("col %d row1 should be NULL, got %v", c, dst.Columns[c].Vals[1])
		}
	}
}

func TestFloatRoundTripPreservesValue(t *testing.T) {
	src := New("t", []struct{ Name, Kind string }{{"f", types.TypeReal}})
	src.AppendRow([]any{91.66666666666667})
	b, _ := src.MarshalJSON()
	var dst Table
	dst.UnmarshalJSON(b)
	got := dst.Columns[0].Vals[0].(float64)
	if got != 91.66666666666667 {
		t.Errorf("float round-trip = %v", got)
	}
}
