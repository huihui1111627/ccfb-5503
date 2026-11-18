package table

import (
	"encoding/json"
	"time"

	"dragonwindow/internal/types"
)

// jsonTable 是表的可序列化形态：值统一转成带类型标签的记录，避免 interface
// 反序列化后丢失具体类型（尤其 time.Time 与 int64/float64 的区分）。
type jsonTable struct {
	Name    string    `json:"name"`
	Columns []jsonCol `json:"columns"`
	N       int       `json:"n"`
}

type jsonCol struct {
	Name string    `json:"name"`
	Kind string    `json:"kind"`
	Cell []jsonVal `json:"cell"`
}

// jsonVal 用 type 标签区分标量的具体动态类型。
type jsonVal struct {
	Type string `json:"t"`
	// 数值/文本/布尔都能放进 Data；时间戳与间隔用专门字段。
	Data   string     `json:"d,omitempty"`
	Time   *time.Time `json:"ts,omitempty"`
	Micros *int64     `json:"us,omitempty"`
}

// MarshalJSON 自定义表序列化。
func (t *Table) MarshalJSON() ([]byte, error) {
	jt := jsonTable{Name: t.Name, N: t.N}
	for _, c := range t.Columns {
		jc := jsonCol{Name: c.Name, Kind: c.Kind}
		for _, v := range c.Vals {
			jc.Cell = append(jc.Cell, encodeVal(v))
		}
		jt.Columns = append(jt.Columns, jc)
	}
	return json.Marshal(jt)
}

// UnmarshalJSON 自定义表反序列化。
func (t *Table) UnmarshalJSON(b []byte) error {
	var jt jsonTable
	if err := json.Unmarshal(b, &jt); err != nil {
		return err
	}
	t.Name = jt.Name
	t.N = jt.N
	t.Columns = nil
	for _, jc := range jt.Columns {
		c := &Column{Name: jc.Name, Kind: jc.Kind, Vals: make([]any, len(jc.Cell))}
		for i, jv := range jc.Cell {
			c.Vals[i] = decodeVal(jv)
		}
		t.Columns = append(t.Columns, c)
	}
	return nil
}

func encodeVal(v any) jsonVal {
	if v == nil {
		return jsonVal{Type: types.TypeNull}
	}
	switch x := v.(type) {
	case int64:
		return jsonVal{Type: types.TypeInteger, Data: jsonNum(x)}
	case float64:
		return jsonVal{Type: types.TypeReal, Data: jsonFloat(x)}
	case string:
		return jsonVal{Type: types.TypeText, Data: x}
	case bool:
		if x {
			return jsonVal{Type: types.TypeBoolean, Data: "t"}
		}
		return jsonVal{Type: types.TypeBoolean, Data: "f"}
	case time.Time:
		tt := x.UTC()
		return jsonVal{Type: types.TypeTimestamp, Time: &tt}
	case types.Interval:
		m := x.Micros
		return jsonVal{Type: types.TypeInterval, Micros: &m}
	}
	return jsonVal{Type: types.TypeNull}
}

func decodeVal(jv jsonVal) any {
	switch jv.Type {
	case types.TypeNull:
		return nil
	case types.TypeInteger:
		n, _ := parseInt(jv.Data)
		return n
	case types.TypeReal:
		f, _ := parseFloat(jv.Data)
		return f
	case types.TypeText:
		return jv.Data
	case types.TypeBoolean:
		return jv.Data == "t"
	case types.TypeTimestamp:
		if jv.Time != nil {
			return jv.Time.UTC()
		}
		return nil
	case types.TypeInterval:
		if jv.Micros != nil {
			return types.Interval{Micros: *jv.Micros}
		}
		return nil
	}
	return nil
}
