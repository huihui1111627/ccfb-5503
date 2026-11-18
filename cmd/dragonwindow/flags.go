package main

import (
	"strconv"
)

// flags 是极简的位置无关命令行标志解析器（仅支持 -k v 与布尔开关）。
type flags struct {
	vals  map[string]string
	bools map[string]bool
}

func parseFlags(args []string, boolKeys ...string) *flags {
	f := &flags{vals: map[string]string{}, bools: map[string]bool{}}
	boolSet := map[string]bool{}
	for _, k := range boolKeys {
		boolSet[k] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			key := a[1:]
			if boolSet[key] {
				f.bools[key] = true
				continue
			}
			if i+1 < len(args) {
				f.vals[key] = args[i+1]
				i++
			}
		}
	}
	return f
}

func (f *flags) string(key, def string) string {
	if v, ok := f.vals[key]; ok {
		return v
	}
	return def
}

func (f *flags) intVal(key string, def int) int {
	if v, ok := f.vals[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (f *flags) int64Val(key string, def int64) int64 {
	if v, ok := f.vals[key]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func (f *flags) flag(key string) bool { return f.bools[key] }
