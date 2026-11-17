// Package web serves the JSON API and the operator console.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"crossborder/dtx/internal/engine"
	"crossborder/dtx/internal/model"
	"crossborder/dtx/internal/store"
	"crossborder/dtx/internal/transport"
	"crossborder/dtx/wire"
)

//go:embed static/index.html
var assets embed.FS

// Server wires the API to the engine and the participant registry.
type Server struct {
	St  *store.Store
	Eng *engine.Engine
	// Services is the participant registry, used by the console to fetch
	// participant metrics.
	Services map[string]string

	once   sync.Once
	client *transport.Client
}

func (s *Server) httpClient() *transport.Client {
	s.once.Do(func() { s.client = transport.New() })
	return s.client
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/txns", s.handleTxns)
	mux.HandleFunc("/api/txns/", s.handleTxn)
	mux.HandleFunc("/api/branches", s.handleBranches)
	mux.HandleFunc("/api/compensations", s.handleComps)
	mux.HandleFunc("/api/suspensions", s.handleSuspensions)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/services", s.handleServices)
	mux.HandleFunc("/api/services/", s.handleServiceAdmin)
	mux.HandleFunc("/api/reconcile", s.handleReconcile)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/ledger/", s.handleParticipantLedger)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "time": time.Now()})
	})
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	buf, err := fs.ReadFile(assets, "static/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf)
}

// --- transactions ----------------------------------------------------------

type txnView struct {
	*model.Txn
	BranchViews []branchView `json:"branch_views"`
	// StuckOn names the branch the transaction is currently waiting for.
	StuckOn *branchView `json:"stuck_on,omitempty"`
	// CompensationRuns is the sum of successful rollback calls.
	CompensationRuns int `json:"compensation_runs"`
	// ReconcileURL makes the per-txn ledger cross-check one click away.
	Arbitrable bool `json:"arbitrable"`
}

type branchView struct {
	*model.Branch
	TxnStatus model.TxnStatus `json:"txn_status"`
}

func (s *Server) buildView(t *model.Txn) txnView {
	bs := s.St.TxnBranches(t)
	v := txnView{Txn: t, BranchViews: make([]branchView, 0, len(bs))}
	for _, b := range bs {
		v.BranchViews = append(v.BranchViews, branchView{Branch: b, TxnStatus: t.Status})
		v.CompensationRuns += b.CompensationOK
	}
	if !t.Terminal() {
		for i := range v.BranchViews {
			if !v.BranchViews[i].Status.Final() {
				v.StuckOn = &v.BranchViews[i]
				break
			}
		}
	}
	v.Arbitrable = s.Eng.ManualArbitrable(t)
	return v
}

func (s *Server) handleTxns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		if r.Method == http.MethodPost {
			s.handleSubmit(w, r)
			return
		}
		http.Error(w, "method not allowed", 405)
		return
	}
	q := r.URL.Query()
	f := store.Filter{
		Status:  q.Get("status"),
		Mode:    q.Get("mode"),
		Q:       q.Get("q"),
		Service: q.Get("service"),
		Stuck:   q.Get("stuck") == "1",
		Limit:   atoiDefault(q.Get("limit"), 50),
		Offset:  atoiDefault(q.Get("offset"), 0),
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	txns, total := s.St.List(f)
	items := make([]txnView, 0, len(txns))
	for _, t := range txns {
		items = append(items, s.buildView(t))
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total})
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req engine.SubmitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := s.Eng.Submit(req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"txn_id": id})
}

func (s *Server) handleTxn(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/txns/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeErr(w, 400, "txn id required")
		return
	}
	t := s.St.GetTxn(id)
	if t == nil {
		writeErr(w, 404, "transaction not found")
		return
	}
	if len(parts) > 1 {
		switch parts[1] {
		case "decide":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			s.handleDecide(w, r, id)
			return
		case "ledger":
			writeJSON(w, 200, s.ledgerFor(t))
			return
		default:
			writeErr(w, 404, "unknown subresource")
			return
		}
	}
	writeJSON(w, 200, s.buildView(t))
}

type decideReq struct {
	Decision string `json:"decision"`
	Actor    string `json:"actor"`
	Reason   string `json:"reason"`
	// Force skips waiting for an unreachable participant; the decision is
	// adopted and the transaction is marked partial.
	Force bool `json:"force"`
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request, id string) {
	var req decideReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	d := model.Direction(strings.ToUpper(strings.TrimSpace(req.Decision)))
	if req.Reason == "" {
		writeErr(w, 400, "reason is required for an auditable decision")
		return
	}
	if err := s.Eng.Manual(engine.ManualRequest{TxnID: id, Decision: d, Actor: req.Actor, Reason: req.Reason, Force: req.Force}); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// --- branches / compensations ---------------------------------------------

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	q := r.URL.Query()
	wantStatus := q.Get("status")
	service := q.Get("service")
	txnID := q.Get("txn_id")
	limit := atoiDefault(q.Get("limit"), 100)
	if limit > 500 {
		limit = 500
	}
	out := make([]branchView, 0, limit)
	for _, t := range s.St.All(0) {
		if txnID != "" && t.ID != txnID {
			continue
		}
		for _, b := range s.St.TxnBranches(t) {
			if service != "" && b.Service != service {
				continue
			}
			if wantStatus != "" && string(b.Status) != wantStatus {
				continue
			}
			out = append(out, branchView{Branch: b, TxnStatus: t.Status})
			if len(out) >= limit {
				break
			}
		}
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) handleComps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items := s.St.CompRecords(q.Get("txn_id"), q.Get("service"))
	writeJSON(w, 200, map[string]any{"items": items})
}

// --- suspension detection --------------------------------------------------

type orphanBranch struct {
	TxnID     string    `json:"txn_id"`
	BizID     string    `json:"biz_id"`
	BranchID  string    `json:"branch_id"`
	Service   string    `json:"service"`
	Status    string    `json:"status"`
	TxnStatus string    `json:"txn_status"`
	AgeSec    int64     `json:"age_sec"`
	Detail    string    `json:"detail"`
	At        time.Time `json:"at"`
}

type suspensionReport struct {
	GeneratedAt    time.Time      `json:"generated_at"`
	Items          []orphanBranch `json:"items"`
	EmptyRollbacks int            `json:"empty_rollbacks"`
	Fenced         int64          `json:"fenced"`
	Services       []wire.Metrics `json:"services"`
	Total          int            `json:"total"`
}

func (s *Server) handleSuspensions(w http.ResponseWriter, r *http.Request) {
	rep := suspensionReport{GeneratedAt: time.Now(), Items: []orphanBranch{}, Services: []wire.Metrics{}}
	now := time.Now()
	for _, t := range s.St.All(0) {
		bs := s.St.TxnBranches(t)
		for _, b := range bs {
			if b.EmptyRollback {
				rep.EmptyRollbacks++
			}
			reason := ""
			if t.Terminal() && !b.Status.Final() {
				reason = "全局事务已结束，但该分支仍有未决的预留/执行请求"
			} else if t.Status == model.StatusNeedsManual {
				reason = "全局事务卡死待人工裁决，分支结果未定"
			} else if t.Status == model.StatusRolledBack && (b.Status == model.BrCancelled || b.Status == model.BrCompensated) && b.Unknown {
				reason = "回滚结果曾未知，最终由幂等重试确认"
			}
			if reason == "" {
				continue
			}
			rep.Items = append(rep.Items, orphanBranch{
				TxnID: t.ID, BizID: t.BizID, BranchID: b.ID, Service: b.Service,
				Status: string(b.Status), TxnStatus: string(t.Status),
				AgeSec: int64(now.Sub(b.UpdatedAt).Seconds()), Detail: reason, At: b.UpdatedAt,
			})
		}
	}
	sort.Slice(rep.Items, func(i, j int) bool { return rep.Items[i].At.Before(rep.Items[j].At) })
	rep.Total = len(rep.Items)
	if rep.Total > 500 {
		rep.Items = rep.Items[:500]
	}
	for name, url := range s.Services {
		var m wire.Metrics
		if err := s.httpClient().FetchJSON(url+"/metrics", 300*time.Millisecond, &m); err == nil {
			rep.Services = append(rep.Services, m)
		} else {
			rep.Services = append(rep.Services, wire.Metrics{Service: name})
		}
	}
	sort.Slice(rep.Services, func(i, j int) bool { return rep.Services[i].Service < rep.Services[j].Service })
	for _, m := range rep.Services {
		rep.Fenced += m.Fenced
	}
	writeJSON(w, 200, rep)
}

// --- events / stats / services --------------------------------------------

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	limit := atoiDefault(q.Get("limit"), 200)
	if cursor == 0 {
		ev := s.St.Events(limit)
		_, next := s.St.EventsAfter(-1, 0)
		writeJSON(w, 200, map[string]any{"items": ev, "cursor": next})
		return
	}
	ev, next := s.St.EventsAfter(cursor, limit)
	writeJSON(w, 200, map[string]any{"items": ev, "cursor": next})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.St.Stats()
	writeJSON(w, 200, map[string]any{"store": st, "engine": s.Eng.Stats()})
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	out := make([]wire.Metrics, 0, len(s.Services))
	health := make([]wire.Health, 0, len(s.Services))
	names := make([]string, 0, len(s.Services))
	for n := range s.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		var m wire.Metrics
		if err := s.httpClient().FetchJSON(s.Services[name]+"/metrics", 300*time.Millisecond, &m); err == nil {
			out = append(out, m)
		} else {
			out = append(out, wire.Metrics{Service: name})
		}
		var h wire.Health
		if err := s.httpClient().FetchJSON(s.Services[name]+"/health", 300*time.Millisecond, &h); err == nil {
			health = append(health, h)
		}
	}
	writeJSON(w, 200, map[string]any{"metrics": out, "health": health})
}

// handleServiceAdmin proxies fault/restart controls for the console.
func (s *Server) handleServiceAdmin(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/services/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeErr(w, 404, "unknown admin path")
		return
	}
	base, ok := s.Services[parts[0]]
	if !ok {
		writeErr(w, 404, "unknown service")
		return
	}
	var path string
	switch parts[1] {
	case "restart":
		path = "/admin/restart"
	case "down":
		path = "/admin/down"
	case "faults":
		path = "/admin/faults"
	default:
		writeErr(w, 404, "unknown admin action")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+path, io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// --- reconciliation view ---------------------------------------------------

type ledgerCheck struct {
	Service string             `json:"service"`
	Error   string             `json:"error,omitempty"`
	Holds   []wire.LedgerEntry `json:"holds"`
}

func (s *Server) ledgerFor(t *model.Txn) map[string]any {
	out := map[string]any{"txn_id": t.ID, "status": t.Status}
	checks := make([]ledgerCheck, 0, len(s.Services))
	for _, b := range s.St.TxnBranches(t) {
		_ = b
	}
	for name, url := range s.Services {
		var l wire.Ledger
		lc := ledgerCheck{Service: name}
		if err := s.httpClient().FetchJSON(url+"/ledger/export", 500*time.Millisecond, &l); err != nil {
			lc.Error = err.Error()
		} else {
			for _, e := range l.Entries {
				if e.TxnID == t.ID {
					lc.Holds = append(lc.Holds, e)
				}
			}
		}
		checks = append(checks, lc)
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Service < checks[j].Service })
	out["services"] = checks
	return out
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	// Best-effort live view: compare each service's balance against the
	// coordinator's own confirmed branches.
	confirmed := map[string]int64{}
	rollback := map[string]int64{}
	for _, t := range s.St.All(0) {
		for _, b := range s.St.TxnBranches(t) {
			switch b.Status {
			case model.BrConfirmed, model.BrExecuted:
				confirmed[b.Service] += b.Amount
			case model.BrCancelled, model.BrCompensated:
				rollback[b.Service] += b.Amount
			}
		}
	}
	out := map[string]any{}
	for name, url := range s.Services {
		var m wire.Metrics
		if err := s.httpClient().FetchJSON(url+"/metrics", 500*time.Millisecond, &m); err != nil {
			out[name] = map[string]any{"error": err.Error()}
			continue
		}
		out[name] = map[string]any{
			"participant":                  m,
			"coordinator_confirmed_amount": confirmed[name],
			"balance_match":                m.Balance == confirmed[name],
			"orphans":                      m.Orphans,
		}
	}
	writeJSON(w, 200, out)
}

// handleParticipantLedger proxies a participant's full ledger, so a verifier
// can reconcile without knowing the participant addresses.
func (s *Server) handleParticipantLedger(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ledger/")
	base, ok := s.Services[name]
	if !ok {
		writeErr(w, 404, "unknown service")
		return
	}
	var l wire.Ledger
	if err := s.httpClient().FetchJSON(base+"/ledger/export", time.Second, &l); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, l)
}

// --- bulk export for verifiers --------------------------------------------

// exportTxn is a compact transaction record; verification does not need the
// per-call history, and including it would multiply the payload.
type exportTxn struct {
	ID         string         `json:"id"`
	BizID      string         `json:"biz_id"`
	Mode       string         `json:"mode"`
	Status     string         `json:"status"`
	Decision   string         `json:"decision"`
	Partial    bool           `json:"partial"`
	TimedOut   bool           `json:"timed_out"`
	Manual     int            `json:"manual"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at,omitempty"`
	Branches   []exportBranch `json:"branches"`
}

type exportBranch struct {
	ID              string `json:"id"`
	Service         string `json:"service"`
	Seq             int    `json:"seq"`
	Status          string `json:"status"`
	Amount          int64  `json:"amount"`
	PrepareAttempts int    `json:"prepare_attempts"`
	FinalAttempts   int    `json:"final_attempts"`
	CompensationOK  int    `json:"compensation_ok"`
	EmptyRollback   bool   `json:"empty_rollback"`
	LastError       string `json:"last_error,omitempty"`
}

// handleExport pages through transactions newest-first. Pass ?offset= and
// ?limit= (max 2000) and repeat until fewer than limit rows come back.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 1000)
	if limit > 2000 {
		limit = 2000
	}
	offset := atoiDefault(q.Get("offset"), 0)
	all := s.St.All(0)
	if offset >= len(all) {
		writeJSON(w, 200, map[string]any{"items": []exportTxn{}, "total": len(all)})
		return
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	items := make([]exportTxn, 0, end-offset)
	for _, t := range all[offset:end] {
		et := exportTxn{
			ID: t.ID, BizID: t.BizID, Mode: string(t.Mode), Status: string(t.Status),
			Decision: string(t.Decision), Partial: t.Partial, TimedOut: t.TimedOut,
			Manual: len(t.ManualDecisions), StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
		}
		for _, b := range s.St.TxnBranches(t) {
			et.Branches = append(et.Branches, exportBranch{
				ID: b.ID, Service: b.Service, Seq: b.Seq, Status: string(b.Status),
				Amount: b.Amount, PrepareAttempts: b.PrepareAttempts, FinalAttempts: b.FinalAttempts,
				CompensationOK: b.CompensationOK, EmptyRollback: b.EmptyRollback, LastError: b.LastError,
			})
		}
		items = append(items, et)
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": len(all)})
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func atoiDefault(s string, d int) int {
	if s == "" {
		return d
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}

// String renders a short description of a txn for logs.
func String(t *model.Txn) string {
	return fmt.Sprintf("%s(%s/%s)", t.ID, t.Mode, t.Status)
}
