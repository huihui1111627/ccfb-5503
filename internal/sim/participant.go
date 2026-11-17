// Package sim implements the four cross-border services used to exercise the
// coordinator: warehouse, carrier, customs and settlement.
//
// Each participant is an idempotent resource ledger with a tombstone for every
// branch that has been rolled back. The tombstone is what makes an empty
// rollback safe: a Try that was delayed in the network and arrives after the
// Cancel is rejected instead of creating a dangling reservation.
package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"crossborder/dtx/wire"
)

// Business rejection outcomes, one per service.
var rejectOutcome = map[string]string{
	"warehouse":  "OUT_OF_STOCK",
	"carrier":    "NO_SLOT",
	"customs":    "QUOTA_EXHAUSTED",
	"settlement": "LIMIT_EXCEEDED",
}

// Faults controls the chaos injected by one participant.
type Faults struct {
	// LatencyMs is the mean base latency added to every request.
	LatencyMs int `json:"latency_ms"`
	// PreUnknownP returns a 5xx before applying. The client must retry.
	PreUnknownP float64 `json:"pre_unknown_p"`
	// PostHoldP applies the operation, then holds the response past the
	// client timeout. It applies to final-phase calls only (confirm/cancel/
	// compensate), which is the "carrier booked the slot but we never got the
	// acknowledgement" case.
	PostHoldP  float64 `json:"post_hold_p"`
	PostHoldMs int     `json:"post_hold_ms"`
	// DropP applies then drops the connection.
	DropP float64 `json:"drop_p"`
	// RejectP returns a definitive business rejection for Try/Execute.
	RejectP float64 `json:"reject_p"`
	// SlowHoldP sleeps before applying, long enough to cross the transaction
	// deadline. This is what manufactures "cancel overtakes try" races.
	SlowHoldP  float64 `json:"slow_hold_p"`
	SlowHoldMs int     `json:"slow_hold_ms"`
	// ReorderP sleeps before applying, but still within the client timeout.
	ReorderP  float64 `json:"reorder_p"`
	ReorderMs int     `json:"reorder_ms"`
}

// DefaultFaults is a moderate chaos profile.
func DefaultFaults(seed int64) Faults {
	r := rand.New(rand.NewPCG(uint64(seed), uint64(seed)*2654435761))
	return Faults{
		LatencyMs:   1 + r.IntN(4),
		PreUnknownP: 0.010,
		PostHoldP:   0.006,
		PostHoldMs:  420,
		DropP:       0.002,
		RejectP:     0.003,
		SlowHoldP:   0.0035,
		SlowHoldMs:  2200,
		ReorderP:    0.012,
		ReorderMs:   60 + r.IntN(90),
	}
}

type brState string

const (
	stTried       brState = "TRIED"
	stConfirmed   brState = "CONFIRMED"
	stCancelled   brState = "CANCELLED"
	stExecuted    brState = "EXECUTED"
	stCompensated brState = "COMPENSATED"
)

type rec struct {
	state     brState
	amount    int64
	mode      string
	seq       int
	createdAt time.Time
	updatedAt time.Time
}

// Participant is one simulated service.
type Participant struct {
	name string
	addr string

	mu        sync.Mutex
	recs      map[string]*rec
	faults    Faults
	downUntil time.Time
	gen       uint64
	rng       *rand.Rand
	started   time.Time

	// counters
	fenced      int64
	emptyAcks   int64
	applied     int64
	rejected    int64
	restarts    int64
	outageNanos int64

	srv *http.Server
	ln  net.Listener
}

// NewParticipant creates a participant listening on addr (e.g. 127.0.0.1:0).
func NewParticipant(name, addr string, seed int64) *Participant {
	r := rand.New(rand.NewPCG(uint64(seed)*7919, uint64(seed)*104729+17))
	p := &Participant{
		name:    name,
		addr:    addr,
		recs:    map[string]*rec{},
		faults:  DefaultFaults(seed),
		rng:     r,
		started: time.Now(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(wire.PathTCCTry, p.handler(wire.OpTry))
	mux.HandleFunc(wire.PathTCCConfirm, p.handler(wire.OpConfirm))
	mux.HandleFunc(wire.PathTCCCancel, p.handler(wire.OpCancel))
	mux.HandleFunc(wire.PathSagaExec, p.handler(wire.OpExecute))
	mux.HandleFunc(wire.PathSagaComp, p.handler(wire.OpCompensate))
	mux.HandleFunc("/metrics", p.handleMetrics)
	mux.HandleFunc("/health", p.handleHealth)
	mux.HandleFunc("/ledger/export", p.handleExport)
	mux.HandleFunc("/admin/faults", p.handleFaults)
	mux.HandleFunc("/admin/restart", p.handleRestart)
	mux.HandleFunc("/admin/reset", p.handleReset)
	mux.HandleFunc("/admin/down", p.handleDown)
	p.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return p
}

// Start binds the listener and serves in the background.
func (p *Participant) Start() error {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	p.ln = ln
	p.addr = ln.Addr().String()
	go func() {
		_ = p.srv.Serve(ln)
	}()
	go p.outageLoop()
	return nil
}

// URL returns the base URL.
func (p *Participant) URL() string { return "http://" + p.addr }

// Addr returns the listen address.
func (p *Participant) Addr() string { return p.addr }

// Close stops the server.
func (p *Participant) Close() error {
	if p.srv != nil {
		return p.srv.Close()
	}
	return nil
}

// outageLoop periodically takes the process down for a short window. Durable
// state survives, in-flight calls fail, just like a rolling restart.
func (p *Participant) outageLoop() {
	for {
		time.Sleep(time.Duration(6+rand.IntN(9)) * time.Second)
		p.mu.Lock()
		if time.Now().Before(p.downUntil) {
			p.mu.Unlock()
			continue
		}
		d := time.Duration(120+rand.IntN(160)) * time.Millisecond
		p.downUntil = time.Now().Add(d)
		p.gen++
		p.restarts++
		p.outageNanos += int64(d)
		p.mu.Unlock()
	}
}

// Chaos decides per-request behaviour. It is evaluated under p.mu.
func (p *Participant) chaos(op string, prepare bool) (latency, reorder, slow, postHold time.Duration, preUnknown, reject, drop bool, gen uint64) {
	f := p.faults
	gen = p.gen
	latency = time.Duration(f.LatencyMs) * time.Millisecond
	if f.ReorderP > 0 && p.rng.Float64() < f.ReorderP {
		reorder = time.Duration(f.ReorderMs) * time.Millisecond
	}
	if prepare && f.SlowHoldP > 0 && p.rng.Float64() < f.SlowHoldP {
		slow = time.Duration(f.SlowHoldMs) * time.Millisecond
	}
	if f.PreUnknownP > 0 && p.rng.Float64() < f.PreUnknownP {
		preUnknown = true
	}
	if prepare && f.RejectP > 0 && p.rng.Float64() < f.RejectP {
		reject = true
	}
	if !prepare && f.PostHoldP > 0 && p.rng.Float64() < f.PostHoldP {
		postHold = time.Duration(f.PostHoldMs) * time.Millisecond
	}
	if f.DropP > 0 && p.rng.Float64() < f.DropP {
		drop = true
	}
	return
}

// handler builds the HTTP handler for one operation.
func (p *Participant) handler(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		now := time.Now()
		p.mu.Lock()
		down := now.Before(p.downUntil)
		p.mu.Unlock()
		if down {
			w.Header().Set("Connection", "close")
			writeResp(w, http.StatusServiceUnavailable, wire.Response{
				OK: false, Retryable: true, Code: "DOWN", Message: "participant restarting",
			})
			return
		}

		var req wire.Request
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Service != p.name {
			writeResp(w, http.StatusOK, wire.Response{OK: false, Code: wire.CodeMisrouted,
				Message: fmt.Sprintf("%s received request for %q", p.name, req.Service)})
			return
		}

		prepare := op == wire.OpTry || op == wire.OpExecute
		p.mu.Lock()
		latency, reorder, slow, postHold, preUnknown, reject, drop, cgen := p.chaos(op, prepare)
		p.mu.Unlock()

		if latency > 0 {
			time.Sleep(latency)
		}
		if reorder > 0 || slow > 0 {
			time.Sleep(reorder + slow)
			// The process may have restarted while this request was parked in
			// the network. The in-flight request is lost.
			p.mu.Lock()
			restarted := p.gen != cgen
			p.mu.Unlock()
			if restarted {
				writeResp(w, http.StatusServiceUnavailable, wire.Response{OK: false, Retryable: true, Code: "RESTARTED"})
				return
			}
		}
		if preUnknown {
			writeResp(w, http.StatusInternalServerError, wire.Response{OK: false, Retryable: true, Code: "DB_BUSY", Message: "injected transient failure"})
			return
		}
		if reject {
			p.mu.Lock()
			p.rejected++
			p.mu.Unlock()
			writeResp(w, http.StatusOK, wire.Response{
				OK: false, Code: wire.CodeRejected, Outcome: rejectOutcome[p.name],
				Message: "business rule rejected the request",
			})
			return
		}

		resp := p.apply(op, &req)
		if resp.OK {
			p.mu.Lock()
			p.applied++
			p.mu.Unlock()
		}

		if postHold > 0 {
			time.Sleep(postHold)
		}
		if drop {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		writeResp(w, http.StatusOK, resp)
	}
}

// apply executes the state machine for one operation under the participant
// lock. It is the participant-side fence that makes the whole thing safe.
func (p *Participant) apply(op string, req *wire.Request) wire.Response {
	key := req.TxnID + "/" + req.BranchID
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.recs[key]
	if !ok {
		r = &rec{mode: "", amount: req.Amount, seq: req.Seq, createdAt: time.Now()}
		p.recs[key] = r
	}
	now := time.Now()
	r.updatedAt = now

	switch op {
	case wire.OpTry:
		if r.state == stCancelled {
			p.fenced++
			return wire.Response{OK: false, Code: wire.CodeAlreadyCanceled,
				Outcome: "FENCED", Message: "branch was rolled back before this try arrived"}
		}
		if r.state == stTried || r.state == stConfirmed {
			return wire.Response{OK: true, Outcome: "IDEMPOTENT"}
		}
		r.state, r.mode, r.amount, r.seq = stTried, wire.ModeTCC, req.Amount, req.Seq
		return wire.Response{OK: true, Outcome: "RESERVED"}

	case wire.OpTCCConfirm:
		switch r.state {
		case stTried:
			r.state = stConfirmed
			return wire.Response{OK: true, Outcome: "CONFIRMED"}
		case stConfirmed:
			return wire.Response{OK: true, Outcome: "IDEMPOTENT"}
		case stCancelled:
			return wire.Response{OK: false, Code: "CONFLICT", Outcome: "ALREADY_CANCELLED",
				Message: "confirm arrived after cancel"}
		default:
			return wire.Response{OK: false, Code: "NOT_TRIED", Outcome: "CONFLICT",
				Message: "confirm without a reservation"}
		}

	case wire.OpTCCCancel:
		switch r.state {
		case stTried:
			r.state = stCancelled
			return wire.Response{OK: true, Outcome: "RELEASED"}
		case stCancelled:
			return wire.Response{OK: true, Outcome: "IDEMPOTENT"}
		case stConfirmed:
			return wire.Response{OK: false, Code: "CONFLICT", Outcome: "ALREADY_CONFIRMED",
				Message: "cancel arrived after confirm"}
		default:
			// Empty rollback: record a tombstone so a late Try is fenced.
			r.state = stCancelled
			p.emptyAcks++
			if req.Amount != 0 {
				r.amount = req.Amount
			}
			return wire.Response{OK: true, Code: wire.CodeAlreadyCanceled, Outcome: "EMPTY"}
		}

	case wire.OpSagaExecute:
		if r.state == stCompensated {
			p.fenced++
			return wire.Response{OK: false, Code: wire.CodeAlreadyCanceled,
				Outcome: "FENCED", Message: "branch was compensated before this execute arrived"}
		}
		if r.state == stExecuted {
			return wire.Response{OK: true, Outcome: "IDEMPOTENT"}
		}
		r.state, r.mode, r.amount, r.seq = stExecuted, wire.ModeSaga, req.Amount, req.Seq
		return wire.Response{OK: true, Outcome: "EXECUTED"}

	case wire.OpSagaCompensate:
		switch r.state {
		case stExecuted:
			r.state = stCompensated
			return wire.Response{OK: true, Outcome: "REVERSED"}
		case stCompensated:
			return wire.Response{OK: true, Outcome: "IDEMPOTENT"}
		default:
			r.state = stCompensated
			p.emptyAcks++
			return wire.Response{OK: true, Code: wire.CodeAlreadyCanceled, Outcome: "EMPTY"}
		}
	}
	return wire.Response{OK: false, Code: wire.CodeInternal, Message: "unknown op " + op}
}

func writeResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Participant) handleHealth(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	healthy := time.Now().After(p.downUntil)
	p.mu.Unlock()
	writeResp(w, http.StatusOK, wire.Health{Service: p.name, Healthy: healthy})
}

func (p *Participant) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	writeResp(w, http.StatusOK, p.Metrics())
}

// Metrics snapshots the participant's books.
func (p *Participant) Metrics() wire.Metrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := wire.Metrics{Service: p.name}
	now := time.Now()
	for _, r := range p.recs {
		switch r.state {
		case stTried:
			m.Reservations++
			if now.Sub(r.updatedAt) > 30*time.Second {
				m.StaleReservations++
			}
		case stConfirmed, stExecuted:
			m.Confirmed++
			m.Balance += r.amount
		case stCancelled, stCompensated:
			m.Canceled++
		}
	}
	m.Orphans = m.Reservations
	m.Fenced = p.fenced
	m.EmptyAcks = p.emptyAcks
	m.Restarts = p.restarts
	m.Records = int64(len(p.recs))
	m.Applied = p.applied
	m.Rejected = p.rejected
	return m
}

// Ledger is the full participant ledger for reconciliation.
type Ledger struct {
	Service string             `json:"service"`
	Entries []wire.LedgerEntry `json:"entries"`
}

// Export returns all live business effects: entries whose amount is still on
// the books. A committed order shows up once per service; a rolled-back one
// shows up nowhere.
func (p *Participant) Export() Ledger {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := Ledger{Service: p.name, Entries: make([]wire.LedgerEntry, 0, len(p.recs)/2)}
	for key, r := range p.recs {
		if r.state != stConfirmed && r.state != stExecuted {
			continue
		}
		txn, branch := splitKey(key)
		l.Entries = append(l.Entries, wire.LedgerEntry{TxnID: txn, BranchID: branch, Amount: r.amount, State: string(r.state)})
	}
	sort.Slice(l.Entries, func(i, j int) bool { return l.Entries[i].TxnID < l.Entries[j].TxnID })
	return l
}

func (p *Participant) handleExport(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.Export())
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

func (p *Participant) handleFaults(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p.mu.Lock()
		f := p.faults
		p.mu.Unlock()
		writeResp(w, http.StatusOK, f)
		return
	}
	var f Faults
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.faults = f
	p.mu.Unlock()
	writeResp(w, http.StatusOK, f)
}

func (p *Participant) handleRestart(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	p.gen++
	p.restarts++
	p.downUntil = time.Now().Add(200 * time.Millisecond)
	p.mu.Unlock()
	writeResp(w, http.StatusOK, wire.Response{OK: true, Outcome: "RESTARTING"})
}

// handleDown takes the participant offline for a while. It is the clean way to
// reproduce an unreachable participant from the console or a script.
func (p *Participant) handleDown(w http.ResponseWriter, r *http.Request) {
	ms := 5000
	if v := r.URL.Query().Get("ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ms = n
		}
	}
	p.SetDown(time.Duration(ms) * time.Millisecond)
	writeResp(w, http.StatusOK, wire.Response{OK: true, Outcome: fmt.Sprintf("DOWN_FOR_%dMS", ms)})
}

func (p *Participant) handleReset(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	p.recs = map[string]*rec{}
	p.fenced, p.emptyAcks, p.applied, p.rejected = 0, 0, 0, 0
	p.downUntil = time.Time{}
	p.mu.Unlock()
	writeResp(w, http.StatusOK, wire.Response{OK: true, Outcome: "RESET"})
}

// SetDown takes the participant down for d. Used by the simulator to create a
// fault storm that exercises the manual-arbitration path.
func (p *Participant) SetDown(d time.Duration) {
	p.mu.Lock()
	p.downUntil = time.Now().Add(d)
	p.gen++
	p.restarts++
	p.mu.Unlock()
}

// Healthy reports whether the participant is accepting traffic.
func (p *Participant) Healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().After(p.downUntil)
}

// SetFaults replaces the chaos profile.
func (p *Participant) SetFaults(f Faults) {
	p.mu.Lock()
	p.faults = f
	p.mu.Unlock()
}

// Faults returns the current chaos profile.
func (p *Participant) Faults() Faults {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.faults
}
