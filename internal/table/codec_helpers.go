package table

import (
	"strconv"
)

func jsonNum(n int64) string {
	return strconv.FormatInt(n, 10)
}

func jsonFloat(f float64) string {
	// 最短往返表示，保证 float64 落盘后能原样还原。
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func parseInt(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}
