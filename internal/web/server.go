// Package web 提供窗口引擎的查询页面与 HTTP API。
//
// 页面分五个区块，对应用户需求：数据表、窗口定义、执行计划、结果与耗时。
// 静态资源通过 go:embed 打包，单个二进制即可启动。
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"dragonwindow/internal/datagen"
	"dragonwindow/internal/expr"
	"dragonwindow/internal/table"
	"dragonwindow/internal/window"
)

//go:embed static/*
var staticFS embed.FS

// Server 持有当前会话的数据表与引擎。
type Server struct {
	data   *table.Table
	engine *window.Engine
	mux    *http.ServeMux
}

// NewServer 用给定数据表构造服务。
func NewServer(t *table.Table) *Server {
	s := &Server{data: t, engine: window.New()}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

// Handler 返回路由处理器。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/api/table", s.handleTable)
	s.mux.HandleFunc("/api/query", s.handleQuery)
	s.mux.HandleFunc("/api/regenerate", s.handleRegenerate)
	s.mux.Handle("/", s.staticHandler())
}

// staticHandler 提供嵌入的前端文件，找不到时回退到 index.html（单页应用）。
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		if _, err := fs.Stat(sub, path[1:]); err != nil {
			// 未知路径回退首页。
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// tableResponse 是数据表区块的载荷。
type tableResponse struct {
	Name      string    `json:"name"`
	Columns   []colMeta `json:"columns"`
	Rows      [][]any   `json:"rows"`
	N         int       `json:"n"`
	Truncated bool      `json:"truncated"`
}

type colMeta struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (s *Server) handleTable(w http.ResponseWriter, r *http.Request) {
	resp := s.tablePayload(100)
	writeJSON(w, resp)
}

// tablePayload 构造表数据，超过 limit 行时截断并标记。
func (s *Server) tablePayload(limit int) tableResponse {
	t := s.data
	resp := tableResponse{Name: t.Name, N: t.N}
	for _, c := range t.Columns {
		resp.Columns = append(resp.Columns, colMeta{Name: c.Name, Kind: c.Kind})
	}
	show := t.N
	if show > limit {
		show = limit
		resp.Truncated = true
	}
	resp.Rows = make([][]any, show)
	for i := 0; i < show; i++ {
		row := make([]any, len(t.Columns))
		for j, c := range t.Columns {
			row[j] = renderCell(c.Vals[i])
		}
		resp.Rows[i] = row
	}
	return resp
}

// queryRequest 是查询请求体。
type queryRequest struct {
	SQL string `json:"sql"`
}

// queryResponse 是查询结果与计划的合并载荷。
type queryResponse struct {
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	Columns   []colMeta   `json:"columns,omitempty"`
	Rows      [][]any     `json:"rows,omitempty"`
	N         int         `json:"n,omitempty"`
	Plan      planPayload `json:"plan"`
	ElapsedMS int64       `json:"elapsed_ms"`
}

type planPayload struct {
	Stages     []stagePayload `json:"stages"`
	Windows    []winPayload   `json:"windows"`
	TotalMS    int64          `json:"total_ms"`
	SortReused int            `json:"sort_reused"`
}

type stagePayload struct {
	Name       string `json:"name"`
	Detail     string `json:"detail"`
	RowsIn     int    `json:"rows_in"`
	RowsOut    int    `json:"rows_out"`
	DurationMS int64  `json:"duration_ms"`
}

type winPayload struct {
	Label       string   `json:"label"`
	Func        string   `json:"func"`
	Partition   []string `json:"partition"`
	OrderBy     []string `json:"order_by"`
	Frame       string   `json:"frame"`
	Exclude     string   `json:"exclude"`
	TreeBuildMS int64    `json:"tree_build_ms"`
	FrameCalcMS int64    `json:"frame_calc_ms"`
	SortCached  bool     `json:"sort_cached"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, queryResponse{OK: false, Error: "invalid request body: " + err.Error()})
		return
	}
	stmt, err := expr.Parse(req.SQL)
	if err != nil {
		writeJSON(w, queryResponse{OK: false, Error: "解析失败: " + err.Error()})
		return
	}
	t0 := time.Now()
	out, plan, err := s.engine.Execute(s.data, stmt)
	elapsed := time.Since(t0)
	if err != nil {
		writeJSON(w, queryResponse{OK: false, Error: "执行失败: " + err.Error(), ElapsedMS: elapsed.Milliseconds()})
		return
	}
	resp := queryResponse{
		OK: true, N: out.N, ElapsedMS: elapsed.Milliseconds(),
		Plan: buildPlanPayload(plan),
	}
	for _, c := range out.Columns {
		resp.Columns = append(resp.Columns, colMeta{Name: c.Name, Kind: c.Kind})
	}
	show := out.N
	limit := 500
	if show > limit {
		show = limit
	}
	resp.Rows = make([][]any, show)
	for i := 0; i < show; i++ {
		row := make([]any, len(out.Columns))
		for j, c := range out.Columns {
			row[j] = renderCell(c.Vals[i])
		}
		resp.Rows[i] = row
	}
	writeJSON(w, resp)
}

func buildPlanPayload(plan *window.Plan) planPayload {
	p := planPayload{SortReused: plan.SortReused, TotalMS: plan.Total.Milliseconds()}
	for _, st := range plan.Stages {
		p.Stages = append(p.Stages, stagePayload{
			Name: st.Name, Detail: st.Detail, RowsIn: st.RowsIn,
			RowsOut: st.RowsOut, DurationMS: st.Duration.Milliseconds(),
		})
	}
	for _, wp := range plan.Windows {
		p.Windows = append(p.Windows, winPayload{
			Label: wp.Label, Func: wp.Func, Partition: wp.Partition,
			OrderBy: wp.OrderBy, Frame: wp.Frame, Exclude: wp.Exclude,
			TreeBuildMS: wp.TreeBuild.Milliseconds(), FrameCalcMS: wp.FrameCalc.Milliseconds(),
			SortCached: wp.SortCached,
		})
	}
	return p
}

// handleRegenerate 用指定规模/种子重新生成数据。
func (s *Server) handleRegenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Races  int   `json:"races"`
		Teams  int   `json:"teams"`
		Splits int   `json:"splits"`
		Seed   int64 `json:"seed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfg := datagen.Config{
		Races: orDefault(req.Races, 20), Teams: orDefault(req.Teams, 40),
		Splits: orDefault(req.Splits, 12), NullRate: 0.08, TieRate: 0.18,
		Seed: orDefault64(req.Seed, 20261119),
	}
	main, _, _, _ := datagen.Generate(cfg)
	s.data = main
	writeJSON(w, map[string]any{"ok": true, "rows": main.N})
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orDefault64(v, def int64) int64 {
	if v <= 0 {
		return def
	}
	return v
}

// renderCell 把动态值转成 JSON 友好的展示形态。
func renderCell(v any) any {
	if v == nil {
		return nil
	}
	// time.Time / Interval 等通过 types.FormatValue 统一成文本，前端直接渲染。
	switch x := v.(type) {
	case time.Time:
		return x.UTC().Format("2006-01-02 15:04:05.000000")
	default:
		_ = x
		return v
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, fmt.Sprintf("encode: %v", err), http.StatusInternalServerError)
	}
}
