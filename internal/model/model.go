// Package model holds the durable state of the coordinator: global
// transactions, branches and the audit trail.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Mode is the transaction pattern.
type Mode string

const (
	ModeTCC  Mode = "TCC"
	ModeSaga Mode = "SAGA"
)

// TxnStatus is the global transaction state.
type TxnStatus string

const (
	StatusPreparing   TxnStatus = "PREPARING"    // trying / executing branches
	StatusCommitting  TxnStatus = "COMMITTING"   // TCC confirm phase
	StatusRollingBack TxnStatus = "ROLLING_BACK" // cancel / compensate phase
	StatusCommitted   TxnStatus = "COMMITTED"
	StatusRolledBack  TxnStatus = "ROLLED_BACK"
	StatusNeedsManual TxnStatus = "NEEDS_MANUAL" // stalled, operator must decide
)

// Direction is the outcome the transaction is heading to.
type Direction string

const (
	DirNone     Direction = ""
	DirCommit   Direction = "COMMIT"
	DirRollback Direction = "ROLLBACK"
)

// BranchStatus is the per-branch state. TCC branches walk
// PENDING→TRYING→TRIED→CONFIRMING→CONFIRMED or →CANCELLING→CANCELLED.
// Saga branches walk PENDING→EXECUTING→EXECUTED→COMPENSATING→COMPENSATED.
type BranchStatus string

const (
	BrPending      BranchStatus = "PENDING"
	BrTrying       BranchStatus = "TRYING"
	BrTried        BranchStatus = "TRIED"
	BrConfirming   BranchStatus = "CONFIRMING"
	BrConfirmed    BranchStatus = "CONFIRMED"
	BrCancelling   BranchStatus = "CANCELLING"
	BrCancelled    BranchStatus = "CANCELLED"
	BrExecuting    BranchStatus = "EXECUTING"
	BrExecuted     BranchStatus = "EXECUTED"
	BrCompensating BranchStatus = "COMPENSATING"
	BrCompensated  BranchStatus = "COMPENSATED"
	BrFailed       BranchStatus = "FAILED"            // prepare definitively rejected
	BrUnknown      BranchStatus = "UNKNOWN"           // prepare outcome not known
	BrConfirmFail  BranchStatus = "CONFIRM_FAILED"    // final phase rejected; needs ops
	BrCancelFail   BranchStatus = "CANCEL_FAILED"     // final phase rejected; needs ops
	BrCompensFail  BranchStatus = "COMPENSATE_FAILED" // final phase rejected; needs ops
	BrSkipped      BranchStatus = "SKIPPED"           // never executed (saga)
	// BrIrreversible marks a branch whose effect is live but can no longer be
	// undone (a confirm that must not be reversed). Only an operator decision
	// can produce it.
	BrIrreversible BranchStatus = "IRREVERSIBLE"
)

// Final reports whether the branch needs no more work.
func (s BranchStatus) Final() bool {
	switch s {
	case BrConfirmed, BrCancelled, BrExecuted, BrCompensated, BrFailed, BrSkipped,
		BrConfirmFail, BrCancelFail, BrCompensFail, BrIrreversible:
		return true
	}
	return false
}

// Effective reports whether the branch's business effect is currently live.
func (s BranchStatus) Effective() bool {
	return s == BrConfirmed || s == BrExecuted || s == BrIrreversible
}

// Txn is the global transaction.
type Txn struct {
	ID     string    `json:"id"`
	BizID  string    `json:"biz_id"`
	Mode   Mode      `json:"mode"`
	Status TxnStatus `json:"status"`
	// Decision is locked once the coordinator enters a final phase.
	Decision Direction `json:"decision"`
	// StalledFrom remembers the phase a NEEDS_MANUAL transaction was in, so
	// the arbiter can tell whether committing is still possible.
	StalledFrom TxnStatus `json:"stalled_from,omitempty"`

	TimeoutMs int  `json:"timeout_ms"`
	Parallel  bool `json:"parallel"` // final phase + TCC prepare concurrency

	Branches []string `json:"branches"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	Deadline   time.Time `json:"deadline,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	TimedOut bool `json:"timed_out,omitempty"`
	// Partial is set when an operator decision left some branches effective
	// and others not. Normal TCC/Saga runs never set it: the invariant is
	// all-or-nothing.
	Partial   bool   `json:"partial,omitempty"`
	LastError string `json:"last_error,omitempty"`
	// ProgressAt is the last time a branch reached a new durable state (not
	// merely a retry). The stall detector keys off this so retrying forever
	// still escalates.
	ProgressAt time.Time `json:"progress_at,omitempty"`

	ManualDecisions []ManualDecision `json:"manual_decisions,omitempty"`
	Audit           []AuditEvent     `json:"audit,omitempty"`
}

// ManualDecision is one operator arbitration, kept forever for audit.
type ManualDecision struct {
	At         time.Time `json:"at"`
	Actor      string    `json:"actor"`
	Decision   Direction `json:"decision"`
	Reason     string    `json:"reason"`
	PrevStatus TxnStatus `json:"prev_status"`
}

// AuditEvent records an automatic or manual state transition.
type AuditEvent struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"` // "coordinator" or operator
	Action string    `json:"action"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// Attempt is one network call to a participant, kept in a capped history.
type Attempt struct {
	Op        string    `json:"op"`
	N         int       `json:"n"`
	StartedAt time.Time `json:"started_at"`
	DurMs     int64     `json:"dur_ms"`
	Result    string    `json:"result"` // OK / UNKNOWN / REJECTED / ERROR
	Code      string    `json:"code,omitempty"`
	Message   string    `json:"message,omitempty"`
	BackoffMs int64     `json:"backoff_ms,omitempty"`
	Empty     bool      `json:"empty,omitempty"`
}

// Branch is one participant action inside a transaction.
type Branch struct {
	ID      string            `json:"id"`
	TxnID   string            `json:"txn_id"`
	Service string            `json:"service"`
	Seq     int               `json:"seq"`
	Mode    Mode              `json:"mode"`
	Amount  int64             `json:"amount"`
	Payload map[string]string `json:"payload,omitempty"`

	Status BranchStatus `json:"status"`
	// PrepareAttempts counts Try/Execute calls; FinalAttempts counts
	// Confirm/Cancel/Compensate calls, including retries.
	PrepareAttempts int `json:"prepare_attempts"`
	FinalAttempts   int `json:"final_attempts"`
	// CompensationAttempts counts rollback-direction calls; CompensationOK
	// counts how many of those were necessary (non-empty) successes.
	CompensationAttempts int  `json:"compensation_attempts"`
	CompensationOK       int  `json:"compensation_ok"`
	EmptyRollback        bool `json:"empty_rollback,omitempty"`
	Unknown              bool `json:"unknown,omitempty"`

	LastOp        string    `json:"last_op,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastResult    string    `json:"last_result,omitempty"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	NextRetryAt   time.Time `json:"next_retry_at,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	History []Attempt `json:"history,omitempty"`
}

// CompRecord is a single final-phase (confirm/cancel/compensate) call, for the
// compensation audit page.
type CompRecord struct {
	At        time.Time `json:"at"`
	TxnID     string    `json:"txn_id"`
	BizID     string    `json:"biz_id"`
	BranchID  string    `json:"branch_id"`
	Service   string    `json:"service"`
	Op        string    `json:"op"` // CONFIRM / CANCEL / COMPENSATE
	Attempt   int       `json:"attempt"`
	DurMs     int64     `json:"dur_ms"`
	Result    string    `json:"result"`
	Code      string    `json:"code,omitempty"`
	Message   string    `json:"message,omitempty"`
	BackoffMs int64     `json:"backoff_ms,omitempty"`
	Empty     bool      `json:"empty,omitempty"`
}

// Event is a global timeline entry (capped ring).
type Event struct {
	At      time.Time `json:"at"`
	TxnID   string    `json:"txn_id"`
	BizID   string    `json:"biz_id,omitempty"`
	Service string    `json:"service,omitempty"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// NewID returns a random, sortable-ish identifier.
func NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// Terminal reports whether the transaction will change no more.
func (t *Txn) Terminal() bool {
	return t.Status == StatusCommitted || t.Status == StatusRolledBack
}

// Clone returns a deep copy safe to hand to readers.
func (t *Txn) Clone() *Txn {
	c := *t
	c.Branches = append([]string(nil), t.Branches...)
	c.ManualDecisions = append([]ManualDecision(nil), t.ManualDecisions...)
	c.Audit = append([]AuditEvent(nil), t.Audit...)
	return &c
}

// Clone returns a deep copy safe to hand to readers.
func (b *Branch) Clone() *Branch {
	c := *b
	if b.Payload != nil {
		c.Payload = make(map[string]string, len(b.Payload))
		for k, v := range b.Payload {
			c.Payload[k] = v
		}
	}
	c.History = append([]Attempt(nil), b.History...)
	return &c
}

// AddAudit appends an audit event, keeping the last 100.
func (t *Txn) AddAudit(at time.Time, actor, action, from, to, note string) {
	t.Audit = append(t.Audit, AuditEvent{At: at, Actor: actor, Action: action, From: from, To: to, Note: note})
	if len(t.Audit) > 100 {
		t.Audit = t.Audit[len(t.Audit)-100:]
	}
}

// AddHistory appends a capped attempt history entry.
func (b *Branch) AddHistory(a Attempt) {
	b.History = append(b.History, a)
	if len(b.History) > 20 {
		b.History = b.History[len(b.History)-20:]
	}
}
