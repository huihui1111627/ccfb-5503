// Package engine coordinates TCC and Saga transactions.
//
// Invariants:
//   - A transaction has exactly one locked decision. Once it enters
//     COMMITTING or ROLLING_BACK it never changes direction, so a branch can
//     never see both a Confirm and a Cancel from the coordinator.
//   - Every final-phase call is idempotent and retried until the participant
//     gives a definitive answer. "Unknown" is never treated as done.
//   - A branch whose Try might have been applied is always rolled back, even
//     if the Try itself timed out. That, plus the participant-side tombstone,
//     is what prevents dangling reservations.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crossborder/dtx/internal/model"
	"crossborder/dtx/internal/store"
	"crossborder/dtx/internal/transport"
	"crossborder/dtx/wire"
)

// ErrClosed is returned when the engine is shutting down.
var ErrClosed = errors.New("engine closed")

// Config tunes the coordinator.
type Config struct {
	// CallTimeout bounds one participant call.
	CallTimeout time.Duration
	// PrepareBackoff is the initial retry delay for Try/Execute.
	PrepareBackoff time.Duration
	// FinalBackoff is the initial retry delay for Confirm/Cancel/Compensate.
	FinalBackoff time.Duration
	// MaxBackoff caps the retry delay.
	MaxBackoff time.Duration
	// StallDeadline is how long a final phase may make no progress before the
	// transaction is escalated to NEEDS_MANUAL. Zero disables escalation,
	// which is what a soak test wants.
	StallDeadline time.Duration
	// TimeoutCheckInterval is the global-deadline / recovery scan period.
	TimeoutCheckInterval time.Duration
	// MaxInflight limits concurrent participant calls across all txns.
	MaxInflight int
}

// DefaultConfig is production-ish.
func DefaultConfig() Config {
	return Config{
		CallTimeout:          250 * time.Millisecond,
		PrepareBackoff:       8 * time.Millisecond,
		FinalBackoff:         40 * time.Millisecond,
		MaxBackoff:           400 * time.Millisecond,
		StallDeadline:        3 * time.Second,
		TimeoutCheckInterval: 25 * time.Millisecond,
		MaxInflight:          512,
	}
}

// Engine is the coordinator.
type Engine struct {
	cfg      Config
	st       *store.Store
	client   *transport.Client
	services map[string]string // logical name -> base URL

	sem chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	running map[string]bool
	closed  bool

	// stats
	calls         atomic.Int64
	unknowns      atomic.Int64
	fenced        atomic.Int64
	compensations atomic.Int64
	manualOps     atomic.Int64
	started       time.Time
}

// ManualRequest is an operator arbitration.
type ManualRequest struct {
	TxnID    string
	Decision model.Direction
	Actor    string
	Reason   string
	// Force adopts the decision without waiting for the participant, for the
	// case where a participant is permanently unreachable. The transaction
	// is marked PARTIAL and branches whose outcome is unknown are recorded as
	// IRREVERSIBLE.
	Force bool
}

// New creates an engine. services maps logical participant names to base URLs.
func New(cfg Config, st *store.Store, services map[string]string) *Engine {
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 250 * time.Millisecond
	}
	if cfg.PrepareBackoff <= 0 {
		cfg.PrepareBackoff = 8 * time.Millisecond
	}
	if cfg.FinalBackoff <= 0 {
		cfg.FinalBackoff = 40 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 400 * time.Millisecond
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 512
	}
	if cfg.TimeoutCheckInterval <= 0 {
		cfg.TimeoutCheckInterval = 25 * time.Millisecond
	}
	e := &Engine{
		cfg: cfg, st: st, client: transport.New(),
		services: services, sem: make(chan struct{}, cfg.MaxInflight),
		running: map[string]bool{}, started: time.Now(),
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	return e
}

// Start launches the background sweeper.
func (e *Engine) Start() {
	e.wg.Add(1)
	go e.sweeper()
}

// Close stops background work and waits for in-flight transactions.
func (e *Engine) Close() {
	e.cancel()
	e.wg.Wait()
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
}

// EngineStats exposes engine counters.
type EngineStats struct {
	Calls         int64             `json:"calls"`
	Unknowns      int64             `json:"unknowns"`
	Fenced        int64             `json:"fenced"`
	Compensations int64             `json:"compensations"`
	ManualOps     int64             `json:"manual_ops"`
	Running       int               `json:"running"`
	UptimeSec     int64             `json:"uptime_sec"`
	ServiceURLs   map[string]string `json:"service_urls"`
}

// Stats snapshots counters.
func (e *Engine) Stats() EngineStats {
	e.mu.Lock()
	n := len(e.running)
	e.mu.Unlock()
	return EngineStats{
		Calls: e.calls.Load(), Unknowns: e.unknowns.Load(), Fenced: e.fenced.Load(),
		Compensations: e.compensations.Load(), ManualOps: e.manualOps.Load(),
		Running: n, UptimeSec: int64(time.Since(e.started).Seconds()),
		ServiceURLs: e.services,
	}
}

// Services returns the participant registry.
func (e *Engine) Services() map[string]string {
	out := make(map[string]string, len(e.services))
	for k, v := range e.services {
		out[k] = v
	}
	return out
}

// --- submission ------------------------------------------------------------

// BranchSpec describes one participant action.
type BranchSpec struct {
	Service string            `json:"service"`
	Amount  int64             `json:"amount"`
	Payload map[string]string `json:"payload,omitempty"`
}

// SubmitRequest is the SDK-facing transaction request.
type SubmitRequest struct {
	BizID     string       `json:"biz_id"`
	Mode      model.Mode   `json:"mode"`
	Branches  []BranchSpec `json:"branches"`
	TimeoutMs int          `json:"timeout_ms"`
	Parallel  bool         `json:"parallel"`
}

// Validate checks the request against the registered participants.
func (e *Engine) Validate(r *SubmitRequest) error {
	if strings.TrimSpace(r.BizID) == "" {
		return errors.New("biz_id is required")
	}
	if r.Mode != model.ModeTCC && r.Mode != model.ModeSaga {
		return fmt.Errorf("mode must be %s or %s", model.ModeTCC, model.ModeSaga)
	}
	if len(r.Branches) == 0 {
		return errors.New("at least one branch is required")
	}
	for i, b := range r.Branches {
		if _, ok := e.services[b.Service]; !ok {
			return fmt.Errorf("branch %d: unknown service %q", i, b.Service)
		}
		if b.Amount < 0 {
			return fmt.Errorf("branch %d: amount must be >= 0", i)
		}
	}
	if r.TimeoutMs < 0 {
		return errors.New("timeout_ms must be >= 0")
	}
	return nil
}

// Submit registers a transaction and starts driving it. It returns once the
// transaction is durably recorded, not once it has finished.
func (e *Engine) Submit(r SubmitRequest) (string, error) {
	if err := e.Validate(&r); err != nil {
		return "", err
	}
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return "", ErrClosed
	}
	now := time.Now()
	timeout := time.Duration(r.TimeoutMs) * time.Millisecond
	if r.TimeoutMs <= 0 {
		timeout = 2 * time.Second
	}
	t := &model.Txn{
		ID:         model.NewID("txn"),
		BizID:      r.BizID,
		Mode:       r.Mode,
		Status:     model.StatusPreparing,
		TimeoutMs:  r.TimeoutMs,
		Parallel:   r.Parallel,
		CreatedAt:  now,
		StartedAt:  now,
		Deadline:   now.Add(timeout),
		ProgressAt: now,
		Audit:      []model.AuditEvent{{At: now, Actor: "coordinator", Action: "CREATED", To: string(model.StatusPreparing), Note: string(r.Mode)}},
	}
	branches := make([]*model.Branch, 0, len(r.Branches))
	for i, spec := range r.Branches {
		b := &model.Branch{
			ID:      model.NewID("br"),
			TxnID:   t.ID,
			Service: spec.Service,
			Seq:     i,
			Mode:    r.Mode,
			Amount:  spec.Amount,
			Payload: spec.Payload,
			Status:  model.BrPending,
		}
		branches = append(branches, b)
		t.Branches = append(t.Branches, b.ID)
	}
	e.st.Create(t, branches)
	e.st.AppendEvent(model.Event{At: now, TxnID: t.ID, BizID: t.BizID, Kind: "TXN_CREATED",
		Message: fmt.Sprintf("%s %s %d branches timeout=%s", t.Mode, t.BizID, len(branches), timeout)})

	e.goTxn(t.ID)
	return t.ID, nil
}

func (e *Engine) goTxn(id string) {
	e.mu.Lock()
	if e.running[id] || e.closed {
		e.mu.Unlock()
		return
	}
	e.running[id] = true
	e.mu.Unlock()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.running, id)
			e.mu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("txn %s panicked: %v", id, r)
				e.st.UpdateTxn(id, func(t *model.Txn) {
					if !t.Terminal() {
						if t.Decision == model.DirNone {
							t.Decision = model.DirRollback
						}
						t.StalledFrom = t.Status
						t.Status = model.StatusNeedsManual
						t.LastError = fmt.Sprintf("panic: %v", r)
						t.AddAudit(time.Now(), "coordinator", "PANIC", "", string(model.StatusNeedsManual), t.LastError)
					}
				})
			}
		}()
		e.drive(id)
	}()
}

// --- the main driver -------------------------------------------------------

func (e *Engine) drive(id string) {
	t := e.st.GetTxn(id)
	if t == nil || t.Terminal() {
		return
	}
	switch t.Status {
	case model.StatusPreparing:
		e.runPrepare(t)
	case model.StatusCommitting, model.StatusRollingBack:
		e.runFinal(t)
	case model.StatusNeedsManual:
		// Wait for an operator.
		return
	}
}

// runPrepare performs the Try/Execute phase and locks the decision.
func (e *Engine) runPrepare(t *model.Txn) {
	branches := e.st.TxnBranches(t)
	if t.Mode == model.ModeTCC {
		e.tccPrepare(t, branches)
	} else {
		e.sagaPrepare(t, branches)
	}
	if e.ctx.Err() != nil {
		return
	}
	// Re-read: the deadline sweeper may have locked the decision meanwhile.
	cur := e.st.GetTxn(t.ID)
	if cur == nil || cur.Terminal() || cur.Status != model.StatusPreparing {
		if cur != nil && (cur.Status == model.StatusRollingBack || cur.Status == model.StatusCommitting) {
			e.runFinal(cur)
		}
		return
	}
	// Decide and check "everything prepared" under one lock, otherwise a Try
	// that lands between the check and the decision can be confirmed on a
	// transaction already heading to rollback.
	var decided model.Direction
	_ = e.st.UpdateTxnBranches(t.ID, func(tt *model.Txn, bs []*model.Branch) error {
		if tt.Terminal() || tt.Status != model.StatusPreparing || tt.Decision != model.DirNone {
			decided = tt.Decision
			if decided == model.DirNone {
				decided = model.DirRollback // someone else locked something odd
			}
			return nil
		}
		deadlinePassed := !tt.Deadline.IsZero() && time.Now().After(tt.Deadline)
		prepared := true
		reason := ""
		for _, b := range bs {
			switch b.Status {
			case model.BrTried, model.BrExecuted:
			default:
				prepared = false
				if reason == "" {
					reason = fmt.Sprintf("branch %s (%s) not prepared: %s", b.Service, b.ID, b.LastError)
				}
			}
		}
		from := tt.Status
		if deadlinePassed {
			// The clock is the arbiter: past the global deadline the
			// transaction rolls back even if every branch happened to make
			// it, because the caller has already been told it timed out.
			prepared = false
			reason = "global timeout during prepare"
			tt.TimedOut = true
		}
		if prepared {
			decided = model.DirCommit
			tt.Decision = model.DirCommit
			tt.Status = model.StatusCommitting
			tt.AddAudit(time.Now(), "coordinator", "DECISION_COMMIT", string(from), string(model.StatusCommitting), "all branches reserved")
		} else {
			decided = model.DirRollback
			tt.Decision = model.DirRollback
			tt.Status = model.StatusRollingBack
			if tt.TimedOut {
				reason = "global timeout during prepare"
			} else if reason == "" {
				reason = "prepare did not complete"
			}
			tt.LastError = reason
			tt.AddAudit(time.Now(), "coordinator", "DECISION_ROLLBACK", string(from), string(model.StatusRollingBack), reason)
		}
		tt.ProgressAt = time.Now()
		return nil
	})
	if decided == model.DirCommit && cur.Mode == model.ModeSaga {
		// Saga has no confirm phase: a fully executed transaction is done.
		e.finish(cur, model.StatusCommitted)
		return
	}
	e.runFinal(e.st.GetTxn(t.ID))
}

// tccPrepare runs Try on every branch, in parallel or in declared order.
func (e *Engine) tccPrepare(t *model.Txn, branches []*model.Branch) {
	if t.Parallel {
		var wg sync.WaitGroup
		for _, b := range branches {
			wg.Add(1)
			go func(b *model.Branch) {
				defer wg.Done()
				e.prepareOne(t, b)
			}(b)
		}
		wg.Wait()
		return
	}
	for _, b := range branches {
		if e.ctx.Err() != nil {
			return
		}
		if !e.prepareOne(t, b) {
			return
		}
	}
}

// sagaPrepare runs Execute strictly in declared order, stopping at the first
// branch that does not execute.
func (e *Engine) sagaPrepare(t *model.Txn, branches []*model.Branch) {
	for _, b := range branches {
		if e.ctx.Err() != nil {
			return
		}
		if !e.prepareOne(t, b) {
			return
		}
	}
}

// prepareOne calls Try/Execute with retries until a definitive answer or the
// transaction deadline. It returns false when the branch is not prepared,
// including when the outcome is still unknown at the deadline.
func (e *Engine) prepareOne(t *model.Txn, b *model.Branch) bool {
	path, op := wire.PathTCCTry, wire.OpTry
	if t.Mode == model.ModeSaga {
		path, op = wire.PathSagaExec, wire.OpExecute
	}
	for attempt := 1; ; attempt++ {
		if e.phaseGone(t) {
			return false
		}
		if !e.acquire() {
			return false
		}
		cur := e.st.GetBranch(b.ID)
		if cur != nil && cur.Status.Final() {
			e.release()
			return cur.Status == model.BrTried || cur.Status == model.BrExecuted
		}
		e.st.UpdateBranch(b.ID, func(br *model.Branch) {
			br.PrepareAttempts = attempt
			br.LastOp = op
			br.LastAttemptAt = time.Now()
			if t.Mode == model.ModeTCC {
				br.Status = model.BrTrying
			} else {
				br.Status = model.BrExecuting
			}
		})
		req := e.request(t, b, op, attempt)
		res := e.call(op, b.Service, path, req)
		e.release()
		e.recordAttempt(b, op, attempt, res, 0, false)

		if res.Verdict == transport.Applied {
			// The transaction may have decided to roll back while this call
			// was in flight, and the rollback may even have run already. Only
			// overwrite a branch that is still preparing; otherwise the
			// rollback's verdict is newer and wins. If the reservation did
			// land after the cancel, the participant's tombstone fences it.
			tried := false
			e.st.UpdateBranch(b.ID, func(br *model.Branch) {
				switch br.Status {
				case model.BrTrying, model.BrExecuting, model.BrUnknown, model.BrPending:
					if t.Mode == model.ModeTCC {
						br.Status = model.BrTried
					} else {
						br.Status = model.BrExecuted
					}
					br.LastResult = "OK"
					br.LastError = ""
					br.Unknown = false
					br.NextRetryAt = time.Time{}
					tried = true
				}
			})
			e.touchProgress(t)
			if tried {
				e.event(t, b.Service, strings.ToUpper(op)+"_OK", outcomeText(res))
			}
			return true
		}
		if res.Verdict == transport.Rejected {
			if res.Code == wire.CodeAlreadyCanceled {
				e.fenced.Add(1)
				e.st.UpdateBranch(b.ID, func(br *model.Branch) {
					br.Status = model.BrFailed
					br.LastResult = "FENCED"
					br.LastError = fmt.Sprintf("%s: %s", res.Code, res.Message)
					br.EmptyRollback = true
					br.FinishedAt = time.Now()
				})
				e.touchProgress(t)
				e.event(t, b.Service, "TRY_FENCED", res.Message)
				return false
			}
			e.st.UpdateBranch(b.ID, func(br *model.Branch) {
				br.Status = model.BrFailed
				br.LastResult = "REJECTED"
				br.LastError = fmt.Sprintf("%s: %s", op, res.Message)
				br.FinishedAt = time.Now()
			})
			e.touchProgress(t)
			e.event(t, b.Service, "TRY_REJECTED", res.Message)
			return false
		}
		// Unknown. Retry with backoff until the deadline.
		e.unknowns.Add(1)
		e.st.UpdateBranch(b.ID, func(br *model.Branch) {
			br.LastResult = "UNKNOWN"
			br.LastError = fmt.Sprintf("%s: %s", res.Code, res.Message)
			br.Unknown = true
		})
		backoff := e.backoff(attempt, e.cfg.PrepareBackoff)
		e.st.UpdateBranch(b.ID, func(br *model.Branch) { br.NextRetryAt = time.Now().Add(backoff) })
		if e.deadlineReached(t) || e.ctx.Err() != nil {
			e.st.UpdateBranch(b.ID, func(br *model.Branch) {
				if br.Status == model.BrTrying || br.Status == model.BrExecuting {
					br.Status = model.BrUnknown
				}
			})
			return false
		}
		if e.sleep(backoff) != nil {
			return false
		}
	}
}

// --- final phase -----------------------------------------------------------

func (e *Engine) runFinal(t *model.Txn) {
	if t.Status == model.StatusCommitting {
		e.runCommit(t)
	} else {
		e.runRollback(t)
	}
}

func (e *Engine) runCommit(t *model.Txn) {
	branches := e.st.TxnBranches(t)
	// Confirm is always driven forward in declaration order, or all at once.
	e.fanOut(t, branches, func(b *model.Branch) bool { return e.commitOne(t, b) })
	if e.ctx.Err() != nil {
		return
	}
	if e.allFinal(t, model.DirCommit) {
		e.finish(t, model.StatusCommitted)
		return
	}
	e.escalateIfStalled(t)
}

func (e *Engine) runRollback(t *model.Txn) {
	branches := e.st.TxnBranches(t)
	// Rollback runs in reverse declaration order (saga compensation order).
	rev := make([]*model.Branch, len(branches))
	for i, b := range branches {
		rev[len(branches)-1-i] = b
	}
	e.fanOut(t, rev, func(b *model.Branch) bool { return e.rollbackOne(t, b) })
	if e.ctx.Err() != nil {
		return
	}
	if e.allFinal(t, model.DirRollback) {
		e.finish(t, model.StatusRolledBack)
		return
	}
	e.escalateIfStalled(t)
}

// fanOut runs final-phase calls in parallel or sequentially.
func (e *Engine) fanOut(t *model.Txn, branches []*model.Branch, fn func(*model.Branch) bool) {
	if t.Parallel {
		var wg sync.WaitGroup
		for _, b := range branches {
			cur := e.st.GetBranch(b.ID)
			if cur == nil || e.settledForFinal(cur, t) {
				continue
			}
			wg.Add(1)
			go func(b *model.Branch) {
				defer wg.Done()
				fn(b)
			}(b)
		}
		wg.Wait()
		return
	}
	for _, b := range branches {
		if e.ctx.Err() != nil {
			return
		}
		cur := e.st.GetBranch(b.ID)
		if cur == nil || e.settledForFinal(cur, t) {
			continue
		}
		fn(b)
	}
}

// settledForFinal reports whether a branch needs no final-phase call.
//
// Finality is direction-dependent: a Saga branch that EXECUTED still needs a
// Compensate when the transaction rolls back, so EXECUTED is terminal for a
// commit but not for a rollback. A prepare that was definitively rejected
// still gets one rollback call: that call refreshes the participant's
// tombstone, so a Try still parked in the network cannot land after the
// transaction is over.
func (e *Engine) settledForFinal(b *model.Branch, t *model.Txn) bool {
	if t.Status == model.StatusCommitting {
		switch b.Status {
		case model.BrConfirmed, model.BrFailed, model.BrSkipped, model.BrIrreversible:
			return true
		}
		return false
	}
	switch b.Status {
	case model.BrCancelled, model.BrCompensated, model.BrSkipped, model.BrIrreversible:
		return true
	case model.BrFailed:
		return b.EmptyRollback
	}
	return false
}

func (e *Engine) commitOne(t *model.Txn, b *model.Branch) bool {
	cur := e.st.GetBranch(b.ID)
	if cur == nil {
		return false
	}
	switch cur.Status {
	case model.BrConfirmed:
		return true
	case model.BrFailed, model.BrSkipped:
		return true // nothing was reserved; nothing to confirm
	case model.BrConfirmFail:
		return false
	}
	return e.finalCall(t, b, wire.OpConfirm, wire.PathTCCConfirm, model.BrConfirmed, false)
}

func (e *Engine) rollbackOne(t *model.Txn, b *model.Branch) bool {
	cur := e.st.GetBranch(b.ID)
	if cur == nil {
		return false
	}
	switch cur.Status {
	case model.BrCancelled, model.BrCompensated:
		return true
	case model.BrCancelFail, model.BrCompensFail, model.BrIrreversible:
		return false
	case model.BrSkipped:
		return true
	}
	if t.Mode == model.ModeTCC {
		return e.finalCall(t, b, wire.OpCancel, wire.PathTCCCancel, model.BrCancelled, true)
	}
	return e.finalCall(t, b, wire.OpCompensate, wire.PathSagaComp, model.BrCompensated, true)
}

// finalCall retries one final operation until the participant answers
// definitively. An unknown answer is never accepted as success.
func (e *Engine) finalCall(t *model.Txn, b *model.Branch, op, path string, success model.BranchStatus, isRollback bool) bool {
	// Persist the intent before the call so a crash mid-call leaves a record
	// the coordinator knows to retry.
	e.st.UpdateBranch(b.ID, func(br *model.Branch) {
		if isRollback {
			if t.Mode == model.ModeTCC {
				br.Status = model.BrCancelling
			} else {
				br.Status = model.BrCompensating
			}
		} else {
			br.Status = model.BrConfirming
		}
		br.LastOp = op
	})
	failStatus := model.BrConfirmFail
	if isRollback {
		failStatus = model.BrCancelFail
		if t.Mode == model.ModeSaga {
			failStatus = model.BrCompensFail
		}
	}
	want := t.Status
	for attempt := 1; ; attempt++ {
		if !e.phaseActive(t, want) {
			return false
		}
		if !e.acquire() {
			return false
		}
		e.st.UpdateBranch(b.ID, func(br *model.Branch) {
			br.FinalAttempts = attempt
			br.LastAttemptAt = time.Now()
			if isRollback {
				br.CompensationAttempts = attempt
			}
		})
		req := e.request(t, b, op, attempt)
		res := e.call(op, b.Service, path, req)
		e.release()
		backoff := e.backoff(attempt, e.cfg.FinalBackoff)
		e.recordAttempt(b, op, attempt, res, backoff, res.Empty)

		if res.Verdict == transport.Applied {
			e.st.UpdateBranch(b.ID, func(br *model.Branch) {
				br.Status = success
				br.LastResult = "OK"
				br.LastError = ""
				br.Unknown = false
				br.NextRetryAt = time.Time{}
				br.FinishedAt = time.Now()
				if res.Empty {
					br.EmptyRollback = true
				}
				if isRollback {
					br.CompensationOK++
				}
			})
			if isRollback {
				e.compensations.Add(1)
			}
			e.touchProgress(t)
			e.event(t, b.Service, strings.ToUpper(op)+"_OK", outcomeText(res))
			return true
		}
		if res.Verdict == transport.Rejected {
			// A conflict means the participant already made the opposite
			// irreversible decision, which the coordinator never asks for.
			// Record the truth rather than forcing a status the ledger does
			// not agree with.
			if res.Outcome == "ALREADY_CANCELLED" && !isRollback {
				e.st.UpdateBranch(b.ID, func(br *model.Branch) {
					br.Status = model.BrCancelled
					br.LastResult = "CONFLICT"
					br.LastError = "participant had already cancelled this branch"
					br.FinishedAt = time.Now()
				})
				e.st.UpdateTxn(t.ID, func(tt *model.Txn) { tt.Partial = true })
				e.touchProgress(t)
				e.event(t, b.Service, "CONFIRM_CONFLICT", res.Message)
				return true
			}
			if res.Outcome == "ALREADY_CONFIRMED" && isRollback {
				e.st.UpdateBranch(b.ID, func(br *model.Branch) {
					br.Status = model.BrIrreversible
					br.LastResult = "CONFLICT"
					br.LastError = "participant had already confirmed this branch; rollback cannot undo it"
					br.FinishedAt = time.Now()
				})
				e.st.UpdateTxn(t.ID, func(tt *model.Txn) { tt.Partial = true })
				e.touchProgress(t)
				e.event(t, b.Service, "ROLLBACK_CONFLICT", res.Message)
				return false
			}
			e.st.UpdateBranch(b.ID, func(br *model.Branch) {
				br.Status = failStatus
				br.LastResult = "REJECTED"
				br.LastError = fmt.Sprintf("%s: %s", res.Code, res.Message)
				br.FinishedAt = time.Now()
			})
			e.touchProgress(t)
			e.event(t, b.Service, strings.ToUpper(op)+"_REJECTED", res.Message)
			return false
		}
		// Unknown: record it and retry until we know. This is the case that
		// turns "car slot booked but money not taken" into a retry instead of
		// a silent success.
		e.unknowns.Add(1)
		e.st.UpdateBranch(b.ID, func(br *model.Branch) {
			br.LastResult = "UNKNOWN"
			br.LastError = fmt.Sprintf("%s: %s", res.Code, res.Message)
			br.Unknown = true
			br.NextRetryAt = time.Now().Add(backoff)
		})
		if e.ctx.Err() != nil {
			return false
		}
		if e.sleep(backoff) != nil {
			return false
		}
	}
}

// --- orchestration helpers -------------------------------------------------

func (e *Engine) allFinal(t *model.Txn, dir model.Direction) bool {
	for _, b := range e.st.TxnBranches(t) {
		if dir == model.DirCommit {
			switch b.Status {
			case model.BrConfirmed, model.BrFailed, model.BrSkipped, model.BrIrreversible:
			default:
				return false
			}
		} else {
			switch b.Status {
			case model.BrCancelled, model.BrCompensated, model.BrSkipped, model.BrIrreversible, model.BrFailed:
			default:
				return false
			}
		}
	}
	return true
}

func (e *Engine) finish(t *model.Txn, status model.TxnStatus) {
	now := time.Now()
	e.st.UpdateTxn(t.ID, func(tt *model.Txn) {
		if tt.Terminal() {
			return
		}
		from := tt.Status
		tt.Status = status
		tt.FinishedAt = now
		tt.LastError = ""
		tt.AddAudit(now, "coordinator", "FINISH_"+string(status), string(from), string(status), "")
	})
	e.st.AppendEvent(model.Event{At: now, TxnID: t.ID, BizID: t.BizID, Kind: "TXN_" + string(status),
		Message: "all branches settled"})
}

// escalateIfStalled moves a transaction to NEEDS_MANUAL when the final phase
// has made no durable progress for StallDeadline. Retrying does not count as
// progress, so an unreachable participant still escalates.
func (e *Engine) escalateIfStalled(t *model.Txn) {
	if e.cfg.StallDeadline <= 0 {
		return
	}
	cur := e.st.GetTxn(t.ID)
	if cur == nil || cur.Terminal() || cur.Status == model.StatusNeedsManual {
		return
	}
	if time.Since(cur.ProgressAt) < e.cfg.StallDeadline {
		return
	}
	if !e.isRunning(t.ID) {
		return
	}
	// Escalation is a last resort: if the last retries did land, the
	// transaction is settled and must not be handed to an operator.
	if e.allFinal(cur, cur.Decision) {
		if cur.Decision == model.DirCommit {
			e.finish(cur, model.StatusCommitted)
		} else {
			e.finish(cur, model.StatusRolledBack)
		}
		return
	}
	now := time.Now()
	from := cur.Status
	changed := false
	e.st.UpdateTxn(t.ID, func(tt *model.Txn) {
		if tt.Terminal() || tt.Status == model.StatusNeedsManual {
			return
		}
		tt.StalledFrom = tt.Status
		tt.Status = model.StatusNeedsManual
		tt.LastError = "final phase made no progress; operator decision required"
		tt.AddAudit(now, "coordinator", "STALLED", string(from), string(model.StatusNeedsManual), tt.LastError)
		changed = true
	})
	if changed {
		e.st.AppendEvent(model.Event{TxnID: t.ID, BizID: t.BizID, Kind: "TXN_NEEDS_MANUAL",
			Message: "final phase stalled; waiting for operator"})
	}
}

func (e *Engine) request(t *model.Txn, b *model.Branch, op string, attempt int) *wire.Request {
	return &wire.Request{
		TxnID: t.ID, BranchID: b.ID, Service: b.Service, BizID: t.BizID,
		Seq: b.Seq, Amount: b.Amount, Payload: b.Payload, Attempt: attempt,
	}
}

func (e *Engine) call(op, service, path string, req *wire.Request) transport.Result {
	url, ok := e.services[service]
	if !ok {
		return transport.Result{Verdict: transport.Rejected, Code: wire.CodeMisrouted, Message: "no URL for " + service}
	}
	e.calls.Add(1)
	return e.client.Call(url, path, req, e.cfg.CallTimeout)
}

func (e *Engine) recordAttempt(b *model.Branch, op string, n int, res transport.Result, backoff time.Duration, empty bool) {
	now := time.Now()
	e.st.UpdateBranch(b.ID, func(br *model.Branch) {
		br.AddHistory(model.Attempt{
			Op: op, N: n, StartedAt: now.Add(-res.Dur), DurMs: res.Dur.Milliseconds(),
			Result: res.Verdict.String(), Code: res.Code, Message: res.Message,
			BackoffMs: backoff.Milliseconds(), Empty: empty,
		})
	})
	if op == wire.OpConfirm || op == wire.OpCancel || op == wire.OpCompensate {
		e.st.AppendComp(model.CompRecord{
			TxnID: b.TxnID, BranchID: b.ID, Service: b.Service, Op: op,
			Attempt: n, DurMs: res.Dur.Milliseconds(), Result: res.Verdict.String(),
			Code: res.Code, Message: res.Message, BackoffMs: backoff.Milliseconds(), Empty: empty,
		})
	}
}

func (e *Engine) event(t *model.Txn, service, kind, msg string) {
	e.st.AppendEvent(model.Event{TxnID: t.ID, BizID: t.BizID, Service: service, Kind: kind, Message: msg})
}

func (e *Engine) touchProgress(t *model.Txn) {
	e.st.UpdateTxn(t.ID, func(tt *model.Txn) { tt.ProgressAt = time.Now() })
}

func outcomeText(r transport.Result) string {
	if r.Outcome != "" {
		return r.Outcome
	}
	return r.Verdict.String()
}

func (e *Engine) backoff(attempt int, base time.Duration) time.Duration {
	if base <= 0 {
		base = 10 * time.Millisecond
	}
	exp := attempt - 1
	if exp > 8 {
		exp = 8
	}
	d := base << exp
	if d > e.cfg.MaxBackoff {
		d = e.cfg.MaxBackoff
	}
	half := int64(d / 2)
	if half <= 0 {
		return d
	}
	return time.Duration(half + rand.Int64N(half+1))
}

// deadlineReached reports whether prepare must stop. A locked decision always
// stops it.
func (e *Engine) deadlineReached(t *model.Txn) bool {
	cur := e.st.GetTxn(t.ID)
	if cur == nil {
		return true
	}
	if cur.Decision != model.DirNone || cur.Status != model.StatusPreparing {
		return true
	}
	return !cur.Deadline.IsZero() && time.Now().After(cur.Deadline)
}

func (e *Engine) acquire() bool {
	select {
	case e.sem <- struct{}{}:
		return true
	default:
	}
	select {
	case e.sem <- struct{}{}:
		return true
	case <-e.ctx.Done():
		return false
	}
}

func (e *Engine) release() { <-e.sem }

func (e *Engine) sleep(d time.Duration) error {
	if d <= 0 {
		return e.ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-e.ctx.Done():
		return ErrClosed
	}
}

// --- sweeper ---------------------------------------------------------------

// sweeper enforces global deadlines, resumes transactions left mid-flight
// (including after a coordinator restart), and escalates stalls.
func (e *Engine) sweeper() {
	defer e.wg.Done()
	t := time.NewTicker(e.cfg.TimeoutCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.sweepOnce()
		}
	}
}

func (e *Engine) sweepOnce() {
	now := time.Now()
	for _, tn := range e.st.Pending() {
		if tn.Status == model.StatusNeedsManual {
			continue
		}
		if e.isRunning(tn.ID) {
			if tn.Status != model.StatusPreparing && e.cfg.StallDeadline > 0 {
				// A final phase that is technically "running" but has been
				// retrying against a dead participant still needs escalation.
				if now.Sub(tn.ProgressAt) > e.cfg.StallDeadline {
					e.escalateIfStalled(tn)
				}
			}
			continue
		}
		// Recovery path: the driver vanished (restart, crash, or the final
		// phase gave up). Resume from the persisted state.
		e.goTxn(tn.ID)
		if tn.Status == model.StatusPreparing && !tn.Deadline.IsZero() && now.After(tn.Deadline) {
			e.timeoutTxn(tn)
		}
	}
}

// phaseActive reports whether a final-phase loop should keep retrying: the
// transaction is still in the phase it started in and somebody else (an
// operator, or the stall detector) has not taken it over.
func (e *Engine) phaseActive(t *model.Txn, want model.TxnStatus) bool {
	cur := e.st.GetTxn(t.ID)
	if cur == nil || cur.Terminal() {
		return false
	}
	return cur.Status == want
}

// phaseGone reports whether the prepare loop should stop.
func (e *Engine) phaseGone(t *model.Txn) bool {
	cur := e.st.GetTxn(t.ID)
	if cur == nil || cur.Terminal() {
		return true
	}
	return cur.Status != model.StatusPreparing || cur.Decision != model.DirNone
}

func (e *Engine) isRunning(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id]
}

// timeoutTxn forces a rollback when the global deadline elapses during
// prepare. A transaction already committing is left alone: the decision is
// locked and rollback would corrupt it.
func (e *Engine) timeoutTxn(tn *model.Txn) {
	if e.isRunning(tn.ID) {
		return
	}
	locked := false
	e.st.UpdateTxn(tn.ID, func(t *model.Txn) {
		if t.Terminal() || t.Status != model.StatusPreparing || t.Decision != model.DirNone {
			return
		}
		t.TimedOut = true
		t.Status = model.StatusRollingBack
		t.Decision = model.DirRollback
		t.ProgressAt = time.Now()
		t.LastError = "global timeout during prepare"
		t.AddAudit(time.Now(), "coordinator", "DECISION_ROLLBACK", string(model.StatusPreparing), string(model.StatusRollingBack), t.LastError)
		locked = true
	})
	if locked {
		e.st.AppendEvent(model.Event{TxnID: tn.ID, BizID: tn.BizID, Kind: "TXN_TIMEOUT",
			Message: "global timeout; rolling back"})
		e.goTxn(tn.ID)
	}
}

// --- operator arbitration --------------------------------------------------

// Manual arbitrates a transaction. Two situations are allowed:
//
//  1. The transaction is stalled (NEEDS_MANUAL) with no participant call in
//     flight. The operator's decision is applied and the normal final phase
//     drives every branch to a definitive outcome.
//  2. The participant is unreachable and an in-flight call may still land.
//     Retrying is pointless (it would just queue behind a dead call), so the
//     operator's decision is ADOPTED: branches are settled from the
//     coordinator's last known state, unknown branches are marked
//     IRREVERSIBLE, and the transaction becomes PARTIAL. This is the only way
//     a real deployment escapes a truly dead participant, and the console
//     shows it in red.
func (e *Engine) Manual(req ManualRequest) error {
	if req.Decision != model.DirCommit && req.Decision != model.DirRollback {
		return errors.New("decision must be COMMIT or ROLLBACK")
	}
	if req.Actor == "" {
		req.Actor = "operator"
	}
	if e.st.GetTxn(req.TxnID) == nil {
		return errors.New("transaction not found")
	}
	cur := e.st.GetTxn(req.TxnID)
	if cur.Terminal() {
		return fmt.Errorf("transaction already finished as %s", cur.Status)
	}
	if cur.Decision != model.DirNone && cur.Decision != req.Decision {
		return fmt.Errorf("transaction already decided %s; refusing to flip it", cur.Decision)
	}
	if req.Force {
		if err := e.forceDecision(req); err != nil {
			return err
		}
	} else if cur.Status == model.StatusNeedsManual {
		if err := e.applyStalledDecision(req); err != nil {
			return err
		}
	} else {
		if e.cfg.StallDeadline <= 0 || time.Since(cur.ProgressAt) < e.cfg.StallDeadline || !e.isRunning(cur.ID) {
			return fmt.Errorf("transaction is still making progress; arbitration only applies to stalled transactions")
		}
		if err := e.forceDecision(req); err != nil {
			return err
		}
	}
	e.manualOps.Add(1)
	e.st.AppendEvent(model.Event{TxnID: req.TxnID, Kind: "TXN_MANUAL_" + string(req.Decision),
		Message: fmt.Sprintf("%s decided %s: %s", req.Actor, req.Decision, req.Reason)})
	e.goTxn(req.TxnID)
	return nil
}

// applyStalledDecision records the operator verdict and re-arms the branches
// for the normal final phase.
func (e *Engine) applyStalledDecision(req ManualRequest) error {
	now := time.Now()
	var notEffective []string
	err := e.st.UpdateTxnBranches(req.TxnID, func(t *model.Txn, bs []*model.Branch) error {
		if t.Terminal() {
			return fmt.Errorf("transaction already finished as %s", t.Status)
		}
		if t.Status != model.StatusNeedsManual {
			return fmt.Errorf("transaction left NEEDS_MANUAL as %s; retry the decision", t.Status)
		}
		from := t.Status
		if req.Decision == model.DirCommit {
			for _, b := range bs {
				switch b.Status {
				case model.BrTried, model.BrExecuted, model.BrUnknown:
					b.Status = model.BrConfirming
				case model.BrPending:
					notEffective = append(notEffective, b.Service+"/"+b.ID)
					b.Status = model.BrFailed
					b.LastError = "operator committed without this branch reserving"
					b.FinishedAt = now
				case model.BrFailed:
					notEffective = append(notEffective, b.Service+"/"+b.ID)
				case model.BrTrying, model.BrExecuting:
					return fmt.Errorf("branch %s (%s) is %s; cannot commit until its prepare settles",
						b.Service, b.ID, b.Status)
				default:
					notEffective = append(notEffective, b.Service+"/"+b.ID)
				}
			}
		} else {
			for _, b := range bs {
				switch b.Status {
				case model.BrPending:
					b.Status = model.BrCancelled
					b.LastResult = "EMPTY"
					b.EmptyRollback = true
					b.FinishedAt = now
				case model.BrConfirmed:
					// Confirmed is irreversible at the participant, and the
					// operator asked for rollback; settle it as such instead
					// of pretending it was cancelled.
					b.Status = model.BrIrreversible
					b.LastError = "already confirmed; rollback cannot undo it"
					b.FinishedAt = now
				case model.BrFailed, model.BrSkipped:
					// nothing to roll back
				default:
					if t.Mode == model.ModeTCC {
						b.Status = model.BrCancelling
					} else {
						b.Status = model.BrCompensating
					}
				}
			}
		}
		t.ManualDecisions = append(t.ManualDecisions, model.ManualDecision{
			At: now, Actor: req.Actor, Decision: req.Decision, Reason: req.Reason, PrevStatus: from,
		})
		t.Decision = req.Decision
		if req.Decision == model.DirCommit {
			t.Status = model.StatusCommitting
			t.Partial = len(notEffective) > 0
		} else {
			t.Status = model.StatusRollingBack
		}
		t.ProgressAt = now
		t.LastError = ""
		note := req.Reason
		if len(notEffective) > 0 {
			note += " (无实际效果: " + strings.Join(notEffective, ", ") + ")"
		}
		t.AddAudit(now, req.Actor, "MANUAL_"+string(req.Decision), string(from), string(t.Status), note)
		return nil
	})
	if err != nil {
		return err
	}
	if len(notEffective) > 0 {
		e.st.AppendEvent(model.Event{TxnID: req.TxnID, Kind: "TXN_PARTIAL_COMMIT",
			Message: "operator committed with branches that never reserved: " + strings.Join(notEffective, ", ")})
	}
	return nil
}

// forceDecision adopts an operator verdict while a participant call is still
// in flight. It never calls the unreachable participant again.
func (e *Engine) forceDecision(req ManualRequest) error {
	now := time.Now()
	var partialReasons []string
	err := e.st.UpdateTxnBranches(req.TxnID, func(t *model.Txn, bs []*model.Branch) error {
		if t.Terminal() {
			return fmt.Errorf("transaction already finished as %s", t.Status)
		}
		if t.Decision != model.DirNone && t.Decision != req.Decision {
			return fmt.Errorf("transaction already decided %s; refusing to flip it", t.Decision)
		}
		from := t.Status
		for _, b := range bs {
			if req.Decision == model.DirCommit {
				switch b.Status {
				case model.BrConfirmed, model.BrExecuted:
					// already effective
				case model.BrTried, model.BrUnknown, model.BrTrying, model.BrExecuting,
					model.BrConfirming, model.BrCancelling, model.BrCompensating, model.BrPending:
					b.Status = model.BrIrreversible
					b.LastError = "operator forced COMMIT while participant was unreachable; effect assumed live"
					partialReasons = append(partialReasons, b.Service+"/"+b.ID)
					b.FinishedAt = now
				default:
					b.Status = model.BrFailed
					b.LastError = "operator forced COMMIT; branch never took effect"
					partialReasons = append(partialReasons, b.Service+"/"+b.ID)
					b.FinishedAt = now
				}
			} else {
				switch b.Status {
				case model.BrCancelled, model.BrCompensated, model.BrFailed, model.BrSkipped:
					// nothing live
				case model.BrConfirmed, model.BrExecuted, model.BrTried,
					model.BrUnknown, model.BrTrying, model.BrExecuting,
					model.BrCancelling, model.BrCompensating, model.BrConfirming, model.BrPending:
					b.Status = model.BrIrreversible
					b.LastError = "operator forced ROLLBACK while participant was unreachable; effect may still be live"
					partialReasons = append(partialReasons, b.Service+"/"+b.ID)
					b.FinishedAt = now
				}
			}
		}
		t.ManualDecisions = append(t.ManualDecisions, model.ManualDecision{
			At: now, Actor: req.Actor, Decision: req.Decision, Reason: req.Reason, PrevStatus: from,
		})
		t.Decision = req.Decision
		t.Partial = true
		if req.Decision == model.DirCommit {
			t.Status = model.StatusCommitted
		} else {
			t.Status = model.StatusRolledBack
		}
		t.FinishedAt = now
		t.ProgressAt = now
		t.LastError = ""
		t.AddAudit(now, req.Actor, "MANUAL_FORCED_"+string(req.Decision), string(from), string(t.Status),
			req.Reason+" (无法确认的分支: "+strings.Join(partialReasons, ", ")+")")
		return nil
	})
	if err != nil {
		return err
	}
	e.st.AppendEvent(model.Event{TxnID: req.TxnID, Kind: "TXN_FORCED_" + string(req.Decision),
		Message: "operator forced the outcome while a participant was unreachable: " + strings.Join(partialReasons, ", ")})
	return nil
}

// ManualArbitrable reports whether a txn is a candidate for the operator.
func (e *Engine) ManualArbitrable(t *model.Txn) bool {
	if t == nil || t.Terminal() {
		return false
	}
	return t.Status == model.StatusNeedsManual || time.Since(t.ProgressAt) > 10*time.Second
}
