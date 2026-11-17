// Package wire defines the HTTP contract between the transaction coordinator
// and business participants.
//
// Every branch operation carries (txn_id, branch_id). Participants use that
// pair as an idempotency key and as the fence that prevents a late Try after a
// Cancel (or a late Execute after a Compensate) from taking effect.
package wire

import "fmt"

// Modes.
const (
	ModeTCC  = "TCC"
	ModeSaga = "SAGA"
)

// Branch operation paths.
const (
	PathTCCTry     = "/tcc/try"
	PathTCCConfirm = "/tcc/confirm"
	PathTCCCancel  = "/tcc/cancel"
	PathSagaExec   = "/saga/execute"
	PathSagaComp   = "/saga/compensate"
)

// Branch operation names as persisted on branch records.
const (
	OpTry        = "TRY"
	OpConfirm    = "CONFIRM"
	OpCancel     = "CANCEL"
	OpExecute    = "EXECUTE"
	OpCompensate = "COMPENSATE"

	// Aliases used by participants.
	OpTCCConfirm     = OpConfirm
	OpTCCCancel      = OpCancel
	OpSagaExecute    = OpExecute
	OpSagaCompensate = OpCompensate
)

// Request is the common envelope for all branch operations.
type Request struct {
	TxnID    string `json:"txn_id"`
	BranchID string `json:"branch_id"`
	// Service is the logical participant name (warehouse, carrier, customs,
	// settlement). Participants reject a request addressed to somebody else so
	// a misrouted retry cannot mutate the wrong ledger.
	Service string `json:"service"`
	// BizID is the external order id, used by the simulated ledgers for
	// reconciliation and by the SDK for tracing.
	BizID string `json:"biz_id"`
	// Seq is the position of this branch in the saga order (0 for TCC).
	Seq int `json:"seq"`
	// Amount is the business amount (RMB fen). All four simulated services
	// record their own view of it so reconciliation can assert equality.
	Amount int64 `json:"amount"`
	// Payload is opaque business data (SKU, lane, HS code, ...).
	Payload map[string]string `json:"payload,omitempty"`
	// Attempt is the coordinator's retry attempt, for observability only.
	Attempt int `json:"attempt,omitempty"`
}

// Response is returned by every participant operation.
type Response struct {
	// OK=true means the operation is definitively applied (or already was,
	// idempotently). OK=false with Retryable=false is a definitive failure.
	// OK=false with Retryable=true means "unknown, ask again".
	OK        bool   `json:"ok"`
	Retryable bool   `json:"retryable,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	// Outcome lets a participant report a definitive business rejection even
	// when the transport succeeded, e.g. "OUT_OF_STOCK".
	Outcome string `json:"outcome,omitempty"`
}

// Definite error codes understood by the coordinator.
const (
	CodeRejected        = "REJECTED"         // business rejection, final
	CodeAlreadyCanceled = "ALREADY_CANCELED" // late Try after Cancel: fenced
	CodeExpired         = "EXPIRED"          // reservation TTL elapsed
	CodeMisrouted       = "MISROUTED"
	CodeInternal        = "INTERNAL"
	CodeUnknown         = "UNKNOWN"
)

// ErrText renders a response as a human-readable string for logs/UI.
func (r Response) ErrText() string {
	if r.OK {
		return ""
	}
	return fmt.Sprintf("%s: %s", r.Code, r.Message)
}

// Metrics is what each simulated participant exposes for reconciliation.
type Metrics struct {
	Service string `json:"service"`
	// Reservations is the number of live resource holds (TCC Try or Saga
	// execute) currently on the books.
	Reservations int64 `json:"reservations"`
	// StaleReservations counts holds older than 30s that were never decided.
	StaleReservations int64 `json:"stale_reservations"`
	// Confirmed is the number of currently effective operations.
	Confirmed int64 `json:"confirmed"`
	// Canceled is the number of currently rolled-back operations.
	Canceled int64 `json:"canceled"`
	// Balance is the money/units held, in fen, for settlement-style services.
	Balance int64 `json:"balance"`
	// Orphans are resources held with no matching final decision, i.e. leaked
	// reservations. Must be zero after a quiescent reconciliation.
	Orphans int64 `json:"orphans"`
	// Fenced counts late prepare requests rejected after a rollback.
	Fenced int64 `json:"fenced"`
	// EmptyAcks counts rollback calls that found nothing to roll back.
	EmptyAcks int64 `json:"empty_acks"`
	// Restarts counts process restarts (simulated).
	Restarts int64 `json:"restarts"`
	// Records is the number of branch records the participant knows about.
	Records int64 `json:"records"`
	// Applied and Rejected are lifetime call counters.
	Applied  int64 `json:"applied"`
	Rejected int64 `json:"rejected"`
}

// Health is the participant liveness payload.
type Health struct {
	Service string `json:"service"`
	Healthy bool   `json:"healthy"`
}

// LedgerEntry is one applied business effect held by a participant.
type LedgerEntry struct {
	TxnID    string `json:"txn_id"`
	BranchID string `json:"branch_id"`
	Amount   int64  `json:"amount"`
	State    string `json:"state"`
}

// Ledger is a participant's live business effects, used for reconciliation.
type Ledger struct {
	Service string        `json:"service"`
	Entries []LedgerEntry `json:"entries"`
}
