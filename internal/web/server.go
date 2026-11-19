// Package web serves the window-engine workbench: a single page with the
// input data, window definitions, execution plan and results, backed by a
// small JSON API.
package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dragonboat/windowengine/internal/gen"
	"dragonboat/windowengine/internal/ref"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
	"dragonboat/windowengine/internal/window"
)

// Server holds the current dataset and generated queries.
type Server struct {
	mu      sync.RWMutex
	dataset *dataset
	mux     *http.ServeMux
}

type dataset struct {
	name    string
	tbl     *table.Table
	queries []gen.QueryCase
	rows    int
	seed    int64
}

// NewServer builds the HTTP handler.
func NewServer() *Server {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/meta", s.handleMeta)
	s.mux.HandleFunc("/api/data", s.handleData)
	s.mux.HandleFunc("/api/query", s.handleQuery)
	s.mux.HandleFunc("/api/generate", s.handleGenerate)
	s.mux.HandleFunc("/api/queries", s.handleQueries)
	s.mux.HandleFunc("/api/verify", s.handleVerify)
	// Preload the default dataset.
	if err := s.loadDataset(3000, 20240601, 64); err != nil {
		panic(err)
	}
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

func (s *Server) loadDataset(rows int, seed int64, partitions int) error {
	opts := gen.DefaultOptions(rows, seed)
	opts.Partition = partitions
	tbl, err := gen.Generate(opts)
	if err != nil {
		return err
	}
	queries, err := gen.GenCorpus(tbl.Schema, 120, seed+7, "race")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dataset = &dataset{name: "races", tbl: tbl, queries: queries, rows: rows, seed: seed}
	return nil
}

func (s *Server) snapshot() *dataset {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dataset
}

type columnMeta struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	ds := s.snapshot()
	cols := make([]columnMeta, len(ds.tbl.Schema.Columns))
	for i, c := range ds.tbl.Schema.Columns {
		cols[i] = columnMeta{Name: c.Name, Kind: c.Kind.String()}
	}
	writeJSON(w, map[string]any{
		"table":      ds.name,
		"columns":    cols,
		"rows":       len(ds.tbl.Rows),
		"seed":       ds.seed,
		"queries":    len(ds.queries),
		"defaultSQL": "SELECT sum(elapsed_ms) OVER (PARTITION BY heat ORDER BY split ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS running_ms, lag(elapsed_ms, 1, 0) OVER (PARTITION BY heat ORDER BY split) AS prev_ms, rank() OVER (PARTITION BY heat ORDER BY elapsed_ms) AS rank_in_heat FROM races",
	})
}

func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	ds := s.snapshot()
	limit := 50
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 1000 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	cols := make([]columnMeta, len(ds.tbl.Schema.Columns))
	for i, c := range ds.tbl.Schema.Columns {
		cols[i] = columnMeta{Name: c.Name, Kind: c.Kind.String()}
	}
	end := offset + limit
	if end > len(ds.tbl.Rows) {
		end = len(ds.tbl.Rows)
	}
	if offset > len(ds.tbl.Rows) {
		offset = len(ds.tbl.Rows)
	}
	rows := make([][]string, 0, end-offset)
	for _, r := range ds.tbl.Rows[offset:end] {
		line := make([]string, len(r))
		for i, v := range r {
			line[i] = v.Display()
		}
		rows = append(rows, line)
	}
	writeJSON(w, map[string]any{
		"columns": cols,
		"rows":    rows,
		"total":   len(ds.tbl.Rows),
		"offset":  offset,
		"limit":   limit,
	})
}

func (s *Server) handleQueries(w http.ResponseWriter, r *http.Request) {
	ds := s.snapshot()
	out := make([]map[string]any, len(ds.queries))
	for i, q := range ds.queries {
		out[i] = map[string]any{"sql": q.SQL, "features": q.Features}
	}
	writeJSON(w, map[string]any{"queries": out})
}

type queryRequest struct {
	SQL          string `json:"sql"`
	Limit        int    `json:"limit"`
	CheckAgainst bool   `json:"checkAgainst"`
}

type planJSON struct {
	Sorts []window.PlanSort `json:"sorts"`
	Items []window.PlanItem `json:"items"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req queryRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	sqlText := strings.TrimSpace(req.SQL)
	if sqlText == "" {
		writeJSONError(w, http.StatusBadRequest, "empty query")
		return
	}
	ds := s.snapshot()
	q, tableName, err := window.ParseQuery(sqlText, ds.tbl.Schema)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse error: "+err.Error())
		return
	}
	if !strings.EqualFold(tableName, ds.name) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown table %q (use %s)", tableName, ds.name))
		return
	}
	start := time.Now()
	out, plan, err := window.Eval(ds.tbl, q)
	elapsed := time.Since(start)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "execution error: "+err.Error())
		return
	}
	limit := req.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows := resultRows(out, ds.tbl.Schema.Len(), limit)
	resp := map[string]any{
		"columns":   resultColumns(out, ds.tbl.Schema.Len()),
		"rows":      rows,
		"totalRows": len(out.Rows),
		"elapsedMs": float64(elapsed.Microseconds()) / 1000.0,
		"plan":      planJSON{Sorts: plan.Sorts, Items: plan.Items},
		"warnings":  plan.Warnings,
		"rowCount":  len(ds.tbl.Rows),
	}
	if req.CheckAgainst {
		refStart := time.Now()
		want, refErr := ref.Eval(ds.tbl, q)
		refElapsed := time.Since(refStart)
		if refErr != nil {
			resp["check"] = map[string]any{"ok": false, "error": refErr.Error()}
		} else {
			ok, detail := compareOutput(out, want, ds.tbl.Schema.Len())
			resp["check"] = map[string]any{
				"ok":          ok,
				"detail":      detail,
				"referenceMs": float64(refElapsed.Microseconds()) / 1000.0,
			}
		}
	}
	writeJSON(w, resp)
}

func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rows int   `json:"rows"`
		Seed int64 `json:"seed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Rows <= 0 {
		req.Rows = 3000
	}
	if req.Rows > 400_000 {
		writeJSONError(w, http.StatusBadRequest, "rows limit is 400000")
		return
	}
	if req.Seed == 0 {
		req.Seed = time.Now().UnixNano() % 1_000_000
	}
	if err := s.loadDataset(req.Rows, req.Seed, 64); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "rows": req.Rows, "seed": req.Seed})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	ds := s.snapshot()
	limit := 40
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	matched, failed := 0, 0
	var failures []map[string]string
	for i, c := range ds.queries {
		if i >= limit {
			break
		}
		q, _, err := window.ParseQuery(c.SQL, ds.tbl.Schema)
		if err != nil {
			failed++
			failures = append(failures, map[string]string{"sql": c.SQL, "error": "parse: " + err.Error()})
			continue
		}
		got, _, err := window.Eval(ds.tbl, q)
		if err != nil {
			failed++
			failures = append(failures, map[string]string{"sql": c.SQL, "error": "engine: " + err.Error()})
			continue
		}
		want, err := ref.Eval(ds.tbl, q)
		if err != nil {
			failed++
			failures = append(failures, map[string]string{"sql": c.SQL, "error": "reference: " + err.Error()})
			continue
		}
		ok, detail := compareOutput(got, want, ds.tbl.Schema.Len())
		if !ok {
			failed++
			failures = append(failures, map[string]string{"sql": c.SQL, "error": detail})
			continue
		}
		matched++
	}
	writeJSON(w, map[string]any{"checked": matched + failed, "matched": matched, "failed": failed, "failures": failures})
}

func resultColumns(t *table.Table, nInput int) []columnMeta {
	cols := make([]columnMeta, 0, t.Schema.Len()-nInput)
	for i := nInput; i < t.Schema.Len(); i++ {
		c := t.Schema.Columns[i]
		cols = append(cols, columnMeta{Name: c.Name, Kind: c.Kind.String()})
	}
	return cols
}

func resultRows(t *table.Table, nInput, limit int) [][]string {
	n := len(t.Rows)
	if limit > 0 && n > limit {
		n = limit
	}
	rows := make([][]string, n)
	for i := 0; i < n; i++ {
		r := t.Rows[i]
		line := make([]string, t.Schema.Len()-nInput)
		for j := range line {
			line[j] = r[nInput+j].Display()
		}
		rows[i] = line
	}
	return rows
}

// compareOutput compares window result columns.  The engine output may be
// re-ordered by an output ORDER BY, so rows are matched on their input-row
// identity: the first nInput columns are the original row values, and we key
// on the whole original tuple (the generated data has no duplicate full rows;
// if it did, both sides would still need the same multiset handling).
func compareOutput(got, want *table.Table, nInput int) (bool, string) {
	if len(got.Rows) != len(want.Rows) {
		return false, fmt.Sprintf("row counts differ: %d vs %d", len(got.Rows), len(want.Rows))
	}
	keyOf := func(r table.Row) string {
		var b strings.Builder
		for i := 0; i < nInput; i++ {
			if r[i].IsNull() {
				b.WriteString("\x00")
			} else {
				b.WriteString(r[i].Kind.String())
				b.WriteByte(':')
				b.WriteString(r[i].String())
			}
			b.WriteByte('\x01')
		}
		return b.String()
	}
	byKey := make(map[string]table.Row, len(want.Rows))
	dupes := 0
	for _, r := range want.Rows {
		k := keyOf(r)
		if _, ok := byKey[k]; ok {
			dupes++
		}
		byKey[k] = r
	}
	if dupes > 0 {
		// Fall back to positional comparison for identical input rows (their
		// window values are identical too, so this is safe when no output
		// ordering was applied).
		for i := range got.Rows {
			for j := nInput; j < got.Schema.Len(); j++ {
				if !sameCell(got.Rows[i][j], want.Rows[i][j]) {
					return false, fmt.Sprintf("row %d, column %s: engine=%q reference=%q",
						i, got.Schema.Columns[j].Name, got.Rows[i][j].Display(), want.Rows[i][j].Display())
				}
			}
		}
		return true, ""
	}
	for i, r := range got.Rows {
		k := keyOf(r)
		w, ok := byKey[k]
		if !ok {
			return false, fmt.Sprintf("engine row %d has no matching reference row", i)
		}
		for j := nInput; j < got.Schema.Len(); j++ {
			if !sameCell(r[j], w[j]) {
				return false, fmt.Sprintf("row %d, column %s: engine=%q reference=%q",
					i, got.Schema.Columns[j].Name, r[j].Display(), w[j].Display())
			}
		}
	}
	return true, ""
}

func sameCell(a, b value.Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() && b.IsNull()
	}
	if a.Kind.IsNumericKind() && b.Kind.IsNumericKind() {
		af, e1 := value.ToFloat(a)
		bf, e2 := value.ToFloat(b)
		if e1 == nil && e2 == nil {
			if af == bf {
				return true
			}
			// Relative tolerance for float accumulation order differences.
			diff := af - bf
			if diff < 0 {
				diff = -diff
			}
			scale := 1.0
			if m := absf(af); m > scale {
				scale = m
			}
			return diff <= 1e-9*scale
		}
	}
	c, err := value.Compare(a, b)
	if err != nil {
		return false
	}
	if a.Kind == value.KindString && b.Kind == value.KindString {
		return a.S == b.S
	}
	return c == 0
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
