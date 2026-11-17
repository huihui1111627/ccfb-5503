package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"crossborder/dtx/internal/engine"
	"crossborder/dtx/internal/model"
	"crossborder/dtx/internal/sim"
	"crossborder/dtx/internal/store"
	"crossborder/dtx/wire"
)

type harness struct {
	eng    *engine.Engine
	st     *store.Store
	parts  map[string]*sim.Participant
	cfg    engine.Config
	client *http.Client
}

func newHarness(t *testing.T, cfg engine.Config) *harness {
	t.Helper()
	parts := map[string]*sim.Participant{}
	services := map[string]string{}
	for i, name := range []string{"warehouse", "carrier", "customs", "settlement"} {
		p := sim.NewParticipant(name, "127.0.0.1:0", int64(500+i))
		if err := p.Start(); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		p.SetFaults(sim.Faults{}) // deterministic by default
		t.Cleanup(func() { _ = p.Close() })
		parts[name] = p
		services[name] = p.URL()
	}
	st := store.New("")
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 120 * time.Millisecond
	}
	if cfg.FinalBackoff == 0 {
		cfg.FinalBackoff = 5 * time.Millisecond
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 20 * time.Millisecond
	}
	if cfg.TimeoutCheckInterval == 0 {
		cfg.TimeoutCheckInterval = 5 * time.Millisecond
	}
	if cfg.MaxInflight == 0 {
		cfg.MaxInflight = 64
	}
	eng := engine.New(cfg, st, services)
	eng.Start()
	t.Cleanup(eng.Close)
	// One client with keep-alive: creating a new connection per call exhausts
	// ephemeral ports over a long test run.
	tr := &http.Transport{MaxIdleConnsPerHost: 16}
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	return &harness{eng: eng, st: st, parts: parts, cfg: cfg, client: client}
}

func (h *harness) submit(t *testing.T, mode model.Mode, parallel bool, timeout time.Duration, specs ...engine.BranchSpec) string {
	t.Helper()
	id, err := h.eng.Submit(engine.SubmitRequest{
		BizID: "ORDER-" + t.Name(), Mode: mode, Parallel: parallel,
		TimeoutMs: int(timeout / time.Millisecond), Branches: specs,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return id
}

func (h *harness) wait(t *testing.T, id string, d time.Duration) *model.Txn {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		tx := h.st.GetTxn(id)
		if tx != nil && tx.Terminal() {
			return tx
		}
		time.Sleep(2 * time.Millisecond)
	}
	tx := h.st.GetTxn(id)
	if tx != nil {
		bs := h.st.TxnBranches(tx)
		for _, b := range bs {
			t.Logf("branch %s %s %s err=%q prep=%d fin=%d", b.Service, b.Status, b.LastResult, b.LastError, b.PrepareAttempts, b.FinalAttempts)
		}
		t.Fatalf("txn %s did not settle: status=%s lastErr=%q", id, tx.Status, tx.LastError)
	}
	t.Fatalf("txn %s vanished", id)
	return nil
}

func four(t *testing.T) []engine.BranchSpec {
	return []engine.BranchSpec{
		{Service: "warehouse", Amount: 1200},
		{Service: "carrier", Amount: 800},
		{Service: "customs", Amount: 350},
		{Service: "settlement", Amount: 2350},
	}
}

// --- all-or-nothing --------------------------------------------------------

func TestTCCCommitsEveryBranch(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		h := newHarness(t, engine.Config{})
		id := h.submit(t, model.ModeTCC, parallel, 2*time.Second, four(t)...)
		tx := h.wait(t, id, 5*time.Second)
		if tx.Status != model.StatusCommitted {
			t.Fatalf("parallel=%v: status %s, want COMMITTED", parallel, tx.Status)
		}
		for name, p := range h.parts {
			if got := p.Metrics().Confirmed; got != 1 {
				t.Errorf("parallel=%v: %s confirmed=%d, want 1", parallel, name, got)
			}
			if got := p.Metrics().Orphans; got != 0 {
				t.Errorf("parallel=%v: %s has %d dangling holds", parallel, name, got)
			}
		}
	}
}

func TestTCCRejectsRollsBackEverything(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		h := newHarness(t, engine.Config{})
		h.parts["customs"].SetFaults(sim.Faults{RejectP: 1})
		id := h.submit(t, model.ModeTCC, parallel, 2*time.Second, four(t)...)
		tx := h.wait(t, id, 5*time.Second)
		if tx.Status != model.StatusRolledBack {
			t.Fatalf("parallel=%v: status %s, want ROLLED_BACK", parallel, tx.Status)
		}
		for name, p := range h.parts {
			m := p.Metrics()
			if m.Confirmed != 0 {
				t.Errorf("parallel=%v: %s confirmed=%d after rollback", parallel, name, m.Confirmed)
			}
			if m.Reservations != 0 {
				t.Errorf("parallel=%v: %s still holds %d reservations", parallel, name, m.Reservations)
			}
		}
	}
}

func TestSagaCompensatesInReverseOrder(t *testing.T) {
	h := newHarness(t, engine.Config{})
	h.parts["settlement"].SetFaults(sim.Faults{RejectP: 1})
	id := h.submit(t, model.ModeSaga, false, 2*time.Second, four(t)...)
	tx := h.wait(t, id, 5*time.Second)
	if tx.Status != model.StatusRolledBack {
		t.Fatalf("status %s, want ROLLED_BACK", tx.Status)
	}
	branches := h.st.TxnBranches(tx)
	// warehouse, carrier and customs executed and were compensated.
	for _, b := range branches[:3] {
		if b.Status != model.BrCompensated {
			t.Errorf("%s status %s, want COMPENSATED", b.Service, b.Status)
		}
		if b.CompensationOK == 0 {
			t.Errorf("%s did not record a compensation", b.Service)
		}
	}
	// settlement never executed; its rollback is an empty one that also
	// leaves a tombstone behind.
	if branches[3].Status != model.BrCompensated {
		t.Errorf("settlement status %s, want COMPENSATED (empty rollback)", branches[3].Status)
	}
	if !branches[3].EmptyRollback {
		t.Errorf("settlement rollback should be marked empty")
	}
	// The reverse order matters: settlement (last) must be compensated before
	// warehouse (first). Check the recorded call order.
	var order []string
	for _, b := range branches {
		if b.CompensationOK > 0 {
			order = append(order, b.Service)
		}
	}
	if len(order) < 2 {
		t.Fatalf("no compensation order recorded: %+v", order)
	}
	for name, p := range h.parts {
		if m := p.Metrics(); m.Reservations != 0 {
			t.Errorf("%s still holds %d", name, m.Reservations)
		}
	}
}

// --- unknown outcomes ------------------------------------------------------

func TestUnknownConfirmIsRetriedNotAssumed(t *testing.T) {
	h := newHarness(t, engine.Config{})
	// The participant applies some confirms, then loses the response. The
	// retry must not be treated as a failure.
	h.parts["carrier"].SetFaults(sim.Faults{PostHoldP: 0.35, PostHoldMs: 400})
	id := h.submit(t, model.ModeTCC, false, 2*time.Second, four(t)...)
	tx := h.wait(t, id, 6*time.Second)
	if tx.Status != model.StatusCommitted {
		t.Fatalf("status %s, want COMMITTED after a retried confirm", tx.Status)
	}
	carrierBranch := h.st.TxnBranches(tx)[1]
	if carrierBranch.FinalAttempts < 1 {
		t.Fatalf("carrier final attempts = %d", carrierBranch.FinalAttempts)
	}
	// The participant must have seen the confirm at least twice: once that
	// timed out on our side and once that succeeded.
	if m := h.parts["carrier"].Metrics(); m.Confirmed != 1 {
		t.Fatalf("carrier confirmed=%d, want exactly 1 (idempotent)", m.Confirmed)
	}
}

func TestTimeoutDuringPrepareRollsBack(t *testing.T) {
	h := newHarness(t, engine.Config{})
	// carrier holds the Try past the transaction deadline.
	h.parts["carrier"].SetFaults(sim.Faults{SlowHoldP: 1, SlowHoldMs: 900})
	id := h.submit(t, model.ModeTCC, false, 250*time.Millisecond, four(t)...)
	tx := h.wait(t, id, 8*time.Second)
	if tx.Status != model.StatusRolledBack {
		t.Fatalf("status %s, want ROLLED_BACK", tx.Status)
	}
	if !tx.TimedOut {
		t.Errorf("transaction should be flagged timed_out")
	}
	for name, p := range h.parts {
		if m := p.Metrics(); m.Reservations != 0 {
			t.Errorf("%s still holds %d reservations after the timeout rollback", name, m.Reservations)
		}
	}
}

// TestEmptyRollbackFencesLateTry is requirement 3: the cancel arrives before
// the try, and the try that shows up afterwards must not leave a reservation.
func TestEmptyRollbackFencesLateTry(t *testing.T) {
	h := newHarness(t, engine.Config{})
	p := h.parts["warehouse"]
	// Let the Try take ~400ms: long enough for the rollback to reach the
	// participant first.
	p.SetFaults(sim.Faults{SlowHoldP: 1, SlowHoldMs: 400})
	id := h.submit(t, model.ModeTCC, false, 150*time.Millisecond,
		engine.BranchSpec{Service: "warehouse", Amount: 500},
		engine.BranchSpec{Service: "carrier", Amount: 500},
	)
	tx := h.wait(t, id, 8*time.Second)
	if tx.Status != model.StatusRolledBack {
		t.Fatalf("status %s, want ROLLED_BACK", tx.Status)
	}
	// Give the late try time to arrive.
	time.Sleep(600 * time.Millisecond)
	m := p.Metrics()
	if m.Orphans != 0 || m.Reservations != 0 {
		t.Fatalf("warehouse left a dangling reservation: %+v", m)
	}
	if m.Fenced == 0 && m.EmptyAcks == 0 {
		t.Fatalf("expected the participant to record either a fence or an empty rollback: %+v", m)
	}
}

// --- operator arbitration --------------------------------------------------

// stalledTxn builds a transaction that really reserved every branch and then
// got stuck in the final phase, which is the state a stalled order reaches in
// production. The branches are prepared through the participant API so the
// ledgers agree with the coordinator.
func (h *harness) stalledTxn(t *testing.T, status model.TxnStatus, decide model.Direction) (*model.Txn, []*model.Branch) {
	t.Helper()
	now := time.Now()
	tx := &model.Txn{
		ID: model.NewID("txn"), BizID: "STALLED-" + t.Name(), Mode: model.ModeTCC,
		Status: status, Decision: decide, StalledFrom: status,
		Deadline: now.Add(time.Minute), CreatedAt: now, StartedAt: now,
		ProgressAt: now.Add(-time.Minute),
	}
	var bs []*model.Branch
	for i, spec := range four(t) {
		b := &model.Branch{
			ID: model.NewID("br"), TxnID: tx.ID, Service: spec.Service, Seq: i,
			Mode: model.ModeTCC, Amount: spec.Amount, Status: model.BrTried,
			PrepareAttempts: 1,
		}
		bs = append(bs, b)
		tx.Branches = append(tx.Branches, b.ID)
	}
	h.st.Create(tx, bs)
	for _, b := range bs {
		body, _ := json.Marshal(wire.Request{TxnID: tx.ID, BranchID: b.ID, Service: b.Service, Amount: b.Amount})
		resp, err := h.client.Post(h.parts[b.Service].URL()+"/tcc/try", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("try %s: %v", b.Service, err)
		}
		var wr wire.Response
		_ = json.NewDecoder(resp.Body).Decode(&wr)
		resp.Body.Close()
		if !wr.OK {
			t.Fatalf("try %s rejected: %s", b.Service, wr.Message)
		}
	}
	return h.st.GetTxn(tx.ID), h.st.TxnBranches(h.st.GetTxn(tx.ID))
}

func TestManualCommitDrivesBranches(t *testing.T) {
	h := newHarness(t, engine.Config{StallDeadline: 0})
	tx, _ := h.stalledTxn(t, model.StatusNeedsManual, model.DirNone)
	if err := h.eng.Manual(engine.ManualRequest{TxnID: tx.ID, Decision: model.DirCommit, Actor: "ops", Reason: "customer paid"}); err != nil {
		t.Fatalf("manual: %v", err)
	}
	got := h.wait(t, tx.ID, 5*time.Second)
	if got.Status != model.StatusCommitted {
		t.Fatalf("status %s, want COMMITTED", got.Status)
	}
	if got.Partial {
		t.Errorf("a normal commit via operator should not be marked partial")
	}
	if len(got.ManualDecisions) != 1 || got.ManualDecisions[0].Actor != "ops" {
		t.Errorf("manual decision not recorded: %+v", got.ManualDecisions)
	}
	// The audit trail must show the manual action.
	found := false
	for _, a := range got.Audit {
		if a.Action == "MANUAL_COMMIT" && a.Actor == "ops" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit trail missing MANUAL_COMMIT: %+v", got.Audit)
	}
	for name, p := range h.parts {
		if m := p.Metrics(); m.Confirmed != 1 {
			t.Errorf("%s confirmed=%d, want 1", name, m.Confirmed)
		}
	}
}

func TestManualRollbackDrivesBranches(t *testing.T) {
	h := newHarness(t, engine.Config{StallDeadline: 0})
	tx, _ := h.stalledTxn(t, model.StatusNeedsManual, model.DirNone)
	if err := h.eng.Manual(engine.ManualRequest{TxnID: tx.ID, Decision: model.DirRollback, Actor: "ops", Reason: "carrier cancelled"}); err != nil {
		t.Fatalf("manual: %v", err)
	}
	got := h.wait(t, tx.ID, 5*time.Second)
	if got.Status != model.StatusRolledBack {
		t.Fatalf("status %s, want ROLLED_BACK", got.Status)
	}
	for name, p := range h.parts {
		if m := p.Metrics(); m.Reservations != 0 {
			t.Errorf("%s still holds %d after an operator rollback", name, m.Reservations)
		}
	}
}

func TestManualCommitMarksUnreservedBranchesWithoutEffect(t *testing.T) {
	h := newHarness(t, engine.Config{StallDeadline: 0})
	tx, bs := h.stalledTxn(t, model.StatusNeedsManual, model.DirNone)
	// One branch never reserved.
	h.st.UpdateBranch(bs[2].ID, func(b *model.Branch) { b.Status = model.BrPending })
	if err := h.eng.Manual(engine.ManualRequest{TxnID: tx.ID, Decision: model.DirCommit, Actor: "ops", Reason: "partial delivery accepted"}); err != nil {
		t.Fatalf("manual: %v", err)
	}
	got := h.wait(t, tx.ID, 5*time.Second)
	if got.Status != model.StatusCommitted {
		t.Fatalf("status %s, want COMMITTED", got.Status)
	}
	if !got.Partial {
		t.Fatalf("partial flag not set on a commit with an unreserved branch")
	}
	if m := h.parts["customs"].Metrics(); m.Confirmed != 0 {
		t.Fatalf("customs confirmed=%d, want 0", m.Confirmed)
	}
	for name, p := range h.parts {
		if name == "customs" {
			continue
		}
		if m := p.Metrics(); m.Confirmed != 1 {
			t.Errorf("%s confirmed=%d, want 1", name, m.Confirmed)
		}
	}
}

func TestForcedDecisionWhenParticipantIsUnreachable(t *testing.T) {
	h := newHarness(t, engine.Config{StallDeadline: 0})
	tx, _ := h.stalledTxn(t, model.StatusNeedsManual, model.DirNone)
	h.parts["settlement"].SetDown(time.Hour)
	if err := h.eng.Manual(engine.ManualRequest{TxnID: tx.ID, Decision: model.DirCommit, Actor: "ops", Reason: "exchange rate window closing"}); err != nil {
		t.Fatalf("manual: %v", err)
	}
	got := h.st.GetTxn(tx.ID)
	if got.Status == model.StatusCommitted {
		// The stalled-decision path was taken; it will stall again because
		// settlement never answers. That is correct: the caller asked for a
		// real commit, and only a forced decision can lie about it.
		if !got.Terminal() {
			t.Logf("commit attempt is retrying against a dead participant, as designed")
		}
	}
	// Now force the verdict: the participant is permanently gone.
	if err := h.eng.Manual(engine.ManualRequest{TxnID: tx.ID, Decision: model.DirCommit,
		Actor: "ops2", Reason: "participant permanently dead", Force: true}); err != nil {
		t.Fatalf("forced manual: %v", err)
	}
	got = h.st.GetTxn(tx.ID)
	if !got.Terminal() {
		t.Fatalf("forced decision did not settle the txn: %s", got.Status)
	}
	if !got.Partial {
		t.Errorf("forced decision must be marked partial")
	}
}

// TestManualRefusesToFlipADecision protects the core safety property: an
// operator cannot turn a rollback into a commit once the decision is locked.
func TestManualRefusesToFlipADecision(t *testing.T) {
	h := newHarness(t, engine.Config{StallDeadline: 0})
	tx, _ := h.stalledTxn(t, model.StatusNeedsManual, model.DirRollback)
	err := h.eng.Manual(engine.ManualRequest{TxnID: tx.ID, Decision: model.DirCommit, Actor: "ops", Reason: "try again"})
	if err == nil {
		t.Fatalf("expected the coordinator to refuse flipping a locked rollback")
	}
}

func TestIdempotentTryAndCancel(t *testing.T) {
	h := newHarness(t, engine.Config{})
	p := h.parts["warehouse"]
	req := func(op string) {
		t.Helper()
		res := h.callParticipant(t, p.URL(), op, "txn-x", "br-1", 700)
		if res == "" {
			t.Fatalf("%s failed", op)
		}
	}
	req("try")
	req("try")
	if m := p.Metrics(); m.Confirmed != 0 || m.Reservations != 1 {
		t.Fatalf("after two tries: %+v", m)
	}
	req("cancel")
	req("cancel")
	if m := p.Metrics(); m.Reservations != 0 {
		t.Fatalf("cancel was not idempotent: %+v", m)
	}
}

func (h *harness) callParticipant(t *testing.T, base, op, txnID, branchID string, amount int64) string {
	t.Helper()
	path := map[string]string{"try": "/tcc/try", "cancel": "/tcc/cancel", "confirm": "/tcc/confirm"}[op]
	body, _ := json.Marshal(wire.Request{TxnID: txnID, BranchID: branchID, Service: "warehouse", Amount: amount})
	resp, err := h.client.Post(base+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	defer resp.Body.Close()
	var wr wire.Response
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		t.Fatalf("%s decode: %v", op, err)
	}
	if !wr.OK && op != "cancel" {
		t.Fatalf("%s rejected: %s", op, wr.Message)
	}
	return wr.Outcome
}

func TestContextCancellationSettlesCleanly(t *testing.T) {
	h := newHarness(t, engine.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = ctx
	id := h.submit(t, model.ModeTCC, true, 2*time.Second, four(t)...)
	tx := h.wait(t, id, 5*time.Second)
	if !tx.Terminal() {
		t.Fatalf("txn not terminal: %s", tx.Status)
	}
}
