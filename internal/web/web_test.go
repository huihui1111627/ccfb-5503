package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSmoke(t *testing.T) {
	s := NewServer()
	// index
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "窗口定义") {
		t.Fatalf("index: %d", rec.Code)
	}
	// meta
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/api/meta", nil))
	var meta map[string]any
	json.Unmarshal(rec.Body.Bytes(), &meta)
	if meta["rows"].(float64) < 1000 {
		t.Fatalf("meta rows %v", meta["rows"])
	}
	// data
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/api/data?limit=5", nil))
	var data struct {
		Rows  [][]string
		Total int
	}
	json.Unmarshal(rec.Body.Bytes(), &data)
	if len(data.Rows) != 5 {
		t.Fatalf("data rows %d", len(data.Rows))
	}
	// query
	body := `{"sql":"SELECT sum(elapsed_ms) OVER (PARTITION BY heat ORDER BY split) AS a, rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS r FROM races","limit":3,"checkAgainst":true}`
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/query", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("query: %d %s", rec.Code, rec.Body.String())
	}
	var qr map[string]any
	json.Unmarshal(rec.Body.Bytes(), &qr)
	chk := qr["check"].(map[string]any)
	if chk["ok"] != true {
		t.Fatalf("check failed: %v", chk)
	}
	plan := qr["plan"].(map[string]any)
	if len(plan["sorts"].([]any)) != 2 {
		t.Fatalf("sorts %v", plan["sorts"])
	}
	// generate
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"rows":1500,"seed":7}`)))
	if rec.Code != 200 {
		t.Fatalf("generate: %d", rec.Code)
	}
	// verify
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/api/verify?limit=10", nil))
	var vr map[string]any
	json.Unmarshal(rec.Body.Bytes(), &vr)
	if vr["failed"].(float64) != 0 {
		t.Fatalf("verify: %v", vr)
	}
	// error path
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/query", strings.NewReader(`{"sql":"SELECT bogus(v) OVER () FROM races"}`)))
	if rec.Code == 200 {
		t.Fatalf("expected error")
	}
}
