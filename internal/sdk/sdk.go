// Package sdk is what a business service imports to start and inspect a
// distributed transaction.
//
// Typical use:
//
//	c := sdk.NewClient("http://coordinator:8080")
//	tx, err := c.Begin(sdk.TCC("ORDER-123").
//	    Branch("warehouse", 1200, nil).
//	    Branch("carrier", 800, nil).
//	    Branch("customs", 350, nil).
//	    Branch("settlement", 2350, nil).
//	    WithTimeout(2 * time.Second).
//	    WithParallel(true))
//	// tx.ID identifies the transaction for later lookups.
//	res, err := tx.Wait(ctx)
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the coordinator.
type Client struct {
	base string
	http *http.Client
}

// NewClient creates a client.
func NewClient(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// Mode mirrors the coordinator's transaction modes.
type Mode string

const (
	ModeTCC  Mode = "TCC"
	ModeSaga Mode = "SAGA"
)

// BranchSpec is one participant action.
type BranchSpec struct {
	Service string            `json:"service"`
	Amount  int64             `json:"amount"`
	Payload map[string]string `json:"payload,omitempty"`
}

// Builder accumulates a transaction.
type Builder struct {
	mode      Mode
	bizID     string
	branches  []BranchSpec
	timeoutMs int
	parallel  bool
}

// TCC starts a TCC builder: reserve everything, then confirm.
func TCC(bizID string) *Builder {
	return &Builder{mode: ModeTCC, bizID: bizID, timeoutMs: 2000}
}

// Saga starts a Saga builder: execute in order, compensate in reverse.
func Saga(bizID string) *Builder {
	return &Builder{mode: ModeSaga, bizID: bizID, timeoutMs: 2000}
}

// Branch declares one participant action.
func (b *Builder) Branch(service string, amount int64, payload map[string]string) *Builder {
	b.branches = append(b.branches, BranchSpec{Service: service, Amount: amount, Payload: payload})
	return b
}

// WithTimeout sets the global transaction deadline.
func (b *Builder) WithTimeout(d time.Duration) *Builder {
	b.timeoutMs = int(d / time.Millisecond)
	return b
}

// WithParallel orders the final phase (and, for TCC, the Try phase) to run
// concurrently instead of in declared order.
func (b *Builder) WithParallel(p bool) *Builder {
	b.parallel = p
	return b
}

// --- types -----------------------------------------------------------------

// Branch is a branch's state as returned by the coordinator.
type Branch struct {
	ID                   string            `json:"id"`
	TxnID                string            `json:"txn_id"`
	Service              string            `json:"service"`
	Seq                  int               `json:"seq"`
	Status               string            `json:"status"`
	Amount               int64             `json:"amount"`
	PrepareAttempts      int               `json:"prepare_attempts"`
	FinalAttempts        int               `json:"final_attempts"`
	CompensationAttempts int               `json:"compensation_attempts"`
	CompensationOK       int               `json:"compensation_ok"`
	EmptyRollback        bool              `json:"empty_rollback,omitempty"`
	LastError            string            `json:"last_error,omitempty"`
	LastResult           string            `json:"last_result,omitempty"`
	Payload              map[string]string `json:"payload,omitempty"`
}

// Txn is a global transaction as returned by the coordinator.
type Txn struct {
	ID          string            `json:"id"`
	BizID       string            `json:"biz_id"`
	Mode        Mode              `json:"mode"`
	Status      string            `json:"status"`
	Decision    string            `json:"decision"`
	StalledFrom string            `json:"stalled_from,omitempty"`
	TimedOut    bool              `json:"timed_out,omitempty"`
	Parallel    bool              `json:"parallel"`
	TimeoutMs   int               `json:"timeout_ms"`
	Branches    []string          `json:"branches"`
	CreatedAt   time.Time         `json:"created_at"`
	Deadline    time.Time         `json:"deadline"`
	FinishedAt  time.Time         `json:"finished_at,omitempty"`
	LastError   string            `json:"last_error,omitempty"`
	BranchViews []Branch          `json:"branch_views,omitempty"`
	Audit       []json.RawMessage `json:"audit,omitempty"`
}

// Terminal reports whether the transaction is done.
func (t *Txn) Terminal() bool {
	return t.Status == "COMMITTED" || t.Status == "ROLLED_BACK"
}

// Succeeded reports whether every branch took effect.
func (t *Txn) Succeeded() bool { return t.Status == "COMMITTED" }

// Tx is a handle returned by Begin.
type Tx struct {
	TxnID string
	c     *Client
}

// ID returns the transaction id.
func (t *Tx) ID() string { return t.TxnID }

// Get fetches the current state.
func (t *Tx) Get(ctx context.Context) (*Txn, error) { return t.c.Get(ctx, t.TxnID) }

// Manual arbitrates a stalled transaction.
func (t *Tx) Manual(ctx context.Context, commit bool, actor, reason string) error {
	d := "ROLLBACK"
	if commit {
		d = "COMMIT"
	}
	return t.c.Manual(ctx, t.TxnID, d, actor, reason)
}

// Force arbitrates a transaction whose participant will never answer.
func (t *Tx) Force(ctx context.Context, commit bool, actor, reason string) error {
	d := "ROLLBACK"
	if commit {
		d = "COMMIT"
	}
	return t.c.ManualForce(ctx, t.TxnID, d, actor, reason)
}

// Wait polls until the transaction is terminal or ctx ends.
func (t *Tx) Wait(ctx context.Context, every time.Duration) (*Txn, error) {
	if every <= 0 {
		every = 25 * time.Millisecond
	}
	for {
		tx, err := t.Get(ctx)
		if err != nil {
			return nil, err
		}
		if tx.Terminal() {
			return tx, nil
		}
		select {
		case <-ctx.Done():
			return tx, ctx.Err()
		case <-time.After(every):
		}
	}
}

// --- client API ------------------------------------------------------------

type submitResp struct {
	TxnID string `json:"txn_id"`
	Error string `json:"error,omitempty"`
}

// Begin submits the transaction and returns a handle. It does not wait for
// the outcome.
func (c *Client) Begin(b *Builder) (*Tx, error) {
	body, err := json.Marshal(map[string]any{
		"biz_id":     b.bizID,
		"mode":       b.mode,
		"branches":   b.branches,
		"timeout_ms": b.timeoutMs,
		"parallel":   b.parallel,
	})
	if err != nil {
		return nil, err
	}
	var out submitResp
	if err := c.post("/api/txns", body, &out); err != nil {
		return nil, err
	}
	if out.TxnID == "" {
		return nil, fmt.Errorf("coordinator returned no txn id: %s", out.Error)
	}
	return &Tx{TxnID: out.TxnID, c: c}, nil
}

// Run is Begin + Wait.
func (c *Client) Run(ctx context.Context, b *Builder) (*Txn, error) {
	tx, err := c.Begin(b)
	if err != nil {
		return nil, err
	}
	return tx.Wait(ctx, 25*time.Millisecond)
}

// Get fetches a transaction with its branches.
func (c *Client) Get(ctx context.Context, id string) (*Txn, error) {
	var out Txn
	if err := c.get(ctx, "/api/txns/"+id, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Manual arbitrates a stalled transaction.
func (c *Client) Manual(ctx context.Context, id, decision, actor, reason string) error {
	return c.decide(ctx, id, decision, actor, reason, false)
}

// ManualForce adopts a decision even though a participant is unreachable.
func (c *Client) ManualForce(ctx context.Context, id, decision, actor, reason string) error {
	return c.decide(ctx, id, decision, actor, reason, true)
}

func (c *Client) decide(ctx context.Context, id, decision, actor, reason string, force bool) error {
	body, _ := json.Marshal(map[string]any{"decision": decision, "actor": actor, "reason": reason, "force": force})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/txns/"+id+"/decide", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("decision rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func (c *Client) post(path string, body []byte, out any) error {
	resp, err := c.http.Post(c.base+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("coordinator error (%d): %s", resp.StatusCode, truncate(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("coordinator error (%d): %s", resp.StatusCode, truncate(raw))
	}
	return json.Unmarshal(raw, out)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300]
	}
	if s == "" {
		return "empty response"
	}
	return s
}

// ErrNotFound is returned when a transaction does not exist.
var ErrNotFound = errors.New("transaction not found")
