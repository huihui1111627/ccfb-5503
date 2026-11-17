// Package store is the coordinator's in-memory state with optional durable
// snapshots. All mutation goes through the store so the operator page and the
// engines see a consistent view.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"crossborder/dtx/internal/model"
)

const (
	maxEvents      = 4000
	maxCompRecords = 8000
)

// Filter selects transactions for the list page.
type Filter struct {
	Status  string
	Mode    string
	Q       string // matches txn id or biz id
	Service string
	Stuck   bool
	Limit   int
	Offset  int
}

// Stats is the dashboard aggregate.
type Stats struct {
	Total               int            `json:"total"`
	ByStatus            map[string]int `json:"by_status"`
	ByMode              map[string]int `json:"by_mode"`
	Committed           int            `json:"committed"`
	RolledBack          int            `json:"rolled_back"`
	NeedsManual         int            `json:"needs_manual"`
	InFlight            int            `json:"in_flight"`
	EmptyRollbacks      int            `json:"empty_rollbacks"`
	CompensationRuns    int            `json:"compensation_runs"`
	Retries             int            `json:"retries"`
	Stuck               int            `json:"stuck"`
	TimedOut            int            `json:"timed_out"`
	ConfirmedBranches   int            `json:"confirmed_branches"`
	CompensatedBranches int            `json:"compensated_branches"`
	// Reconcile is filled in by the simulator, not the store.
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	Throughput   float64 `json:"throughput_per_sec"`
}

// Store holds all coordinator state.
type Store struct {
	mu       sync.RWMutex
	txns     map[string]*model.Txn
	branches map[string]*model.Branch
	events   []model.Event
	comps    []model.CompRecord
	order    []string // txn ids in creation order

	startedAt time.Time
	path      string
	dirty     bool
}

// New creates an empty store. path may be empty to disable snapshots.
func New(path string) *Store {
	return &Store{
		txns:      map[string]*model.Txn{},
		branches:  map[string]*model.Branch{},
		startedAt: time.Now(),
		path:      path,
	}
}

// --- transaction mutations -------------------------------------------------

// Create inserts a new txn and its branches atomically.
func (s *Store) Create(t *model.Txn, branches []*model.Branch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if _, dup := s.txns[t.ID]; dup {
		return
	}
	s.txns[t.ID] = t
	s.order = append(s.order, t.ID)
	for _, b := range branches {
		b.CreatedAt, b.UpdatedAt = now, now
		s.branches[b.ID] = b
	}
	s.dirty = true
}

// GetTxn returns a deep copy.
func (s *Store) GetTxn(id string) *model.Txn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.txns[id]; ok {
		return t.Clone()
	}
	return nil
}

// GetBranch returns a deep copy.
func (s *Store) GetBranch(id string) *model.Branch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.branches[id]; ok {
		return b.Clone()
	}
	return nil
}

// UpdateTxn applies fn under the store lock.
func (s *Store) UpdateTxn(id string, fn func(*model.Txn)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.txns[id]; ok {
		fn(t)
		t.UpdatedAt = time.Now()
		s.dirty = true
	}
}

// UpdateBranch applies fn under the store lock.
func (s *Store) UpdateBranch(id string, fn func(*model.Branch)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.branches[id]; ok {
		fn(b)
		b.UpdatedAt = time.Now()
		s.dirty = true
	}
}

// UpdateTxnBranches applies fn with the txn and all of its branches under a
// single lock, so a decision that depends on branch state is atomic. fn may
// return an error to abort the mutation.
func (s *Store) UpdateTxnBranches(id string, fn func(*model.Txn, []*model.Branch) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txns[id]
	if !ok {
		return os.ErrNotExist
	}
	bs := make([]*model.Branch, 0, len(t.Branches))
	for _, bid := range t.Branches {
		if b, ok := s.branches[bid]; ok {
			bs = append(bs, b)
		}
	}
	if err := fn(t, bs); err != nil {
		return err
	}
	t.UpdatedAt = time.Now()
	s.dirty = true
	return nil
}

// AppendEvent records a global timeline entry.
func (s *Store) AppendEvent(e model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.events = append(s.events, e)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
}

// AppendComp records a final-phase attempt.
func (s *Store) AppendComp(c model.CompRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.At.IsZero() {
		c.At = time.Now()
	}
	s.comps = append(s.comps, c)
	if len(s.comps) > maxCompRecords {
		s.comps = s.comps[len(s.comps)-maxCompRecords:]
	}
}

// --- reads -----------------------------------------------------------------

// TxnBranches returns the branches of a txn in seq order.
func (s *Store) TxnBranches(t *model.Txn) []*model.Branch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Branch, 0, len(t.Branches))
	for _, id := range t.Branches {
		if b, ok := s.branches[id]; ok {
			out = append(out, b.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// BranchOfTxn is a convenience lookup.
func (s *Store) BranchOfTxn(txnID, branchID string) *model.Branch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.branches[branchID]; ok && b.TxnID == txnID {
		return b.Clone()
	}
	return nil
}

// List returns matching txns plus the total match count before pagination.
func (s *Store) List(f Filter) ([]*model.Txn, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Txn
	q := strings.ToLower(strings.TrimSpace(f.Q))
	for i := len(s.order) - 1; i >= 0; i-- {
		t := s.txns[s.order[i]]
		if t == nil {
			continue
		}
		if f.Status != "" && string(t.Status) != f.Status {
			continue
		}
		if f.Mode != "" && string(t.Mode) != f.Mode {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(t.ID), q) && !strings.Contains(strings.ToLower(t.BizID), q) {
			continue
		}
		if f.Service != "" && !s.branchServiceMatchesLocked(t, f.Service) {
			continue
		}
		if f.Stuck && t.Terminal() {
			continue
		}
		out = append(out, t.Clone())
	}
	total := len(out)
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, total
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, total
}

func (s *Store) branchServiceMatchesLocked(t *model.Txn, service string) bool {
	for _, id := range t.Branches {
		if b := s.branches[id]; b != nil && b.Service == service {
			return true
		}
	}
	return false
}

// Events returns the most recent events oldest-first, newest last, capped.
func (s *Store) Events(limit int) []model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.events) {
		limit = len(s.events)
	}
	out := make([]model.Event, limit)
	copy(out, s.events[len(s.events)-limit:])
	return out
}

// EventsAfter returns events strictly after seq (index-based cursor).
func (s *Store) EventsAfter(cursor int64, limit int) ([]model.Event, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cursor < 0 || cursor > int64(len(s.events)) {
		cursor = 0 // ring was trimmed; restart
	}
	start := int(cursor)
	if len(s.events)-start > limit {
		// give the client the most recent window instead of a huge backlog
		start = len(s.events) - limit
	}
	out := make([]model.Event, len(s.events)-start)
	copy(out, s.events[start:])
	return out, int64(len(s.events))
}

// CompRecords returns final-phase records, newest first.
func (s *Store) CompRecords(txnID, service string) []model.CompRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.CompRecord, 0, 128)
	for i := len(s.comps) - 1; i >= 0; i-- {
		c := s.comps[i]
		if txnID != "" && c.TxnID != txnID {
			continue
		}
		if service != "" && c.Service != service {
			continue
		}
		out = append(out, c)
		if len(out) >= 500 {
			break
		}
	}
	return out
}

// Stats computes aggregate counters.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{ByStatus: map[string]int{}, ByMode: map[string]int{}}
	now := time.Now()
	var latencySum float64
	var latencyN int
	var minStart, maxEnd time.Time
	for _, t := range s.txns {
		st.Total++
		st.ByStatus[string(t.Status)]++
		st.ByMode[string(t.Mode)]++
		if t.TimedOut {
			st.TimedOut++
		}
		switch t.Status {
		case model.StatusCommitted:
			st.Committed++
		case model.StatusRolledBack:
			st.RolledBack++
		case model.StatusNeedsManual:
			st.NeedsManual++
		default:
			st.InFlight++
			if now.Sub(t.UpdatedAt) > 30*time.Second {
				st.Stuck++
			}
		}
		if t.FinishedAt.After(t.StartedAt) {
			latencySum += t.FinishedAt.Sub(t.StartedAt).Seconds() * 1000
			latencyN++
		}
		if !t.StartedAt.IsZero() && (minStart.IsZero() || t.StartedAt.Before(minStart)) {
			minStart = t.StartedAt
		}
		if t.FinishedAt.After(maxEnd) {
			maxEnd = t.FinishedAt
		}
		for _, bid := range t.Branches {
			b := s.branches[bid]
			if b == nil {
				continue
			}
			if b.EmptyRollback {
				st.EmptyRollbacks++
			}
			switch b.Status {
			case model.BrConfirmed, model.BrExecuted:
				st.ConfirmedBranches++
			case model.BrCompensated, model.BrCancelled:
				st.CompensatedBranches++
			}
			if b.CompensationOK > 0 {
				st.CompensationRuns += b.CompensationOK
			}
			if b.PrepareAttempts > 1 {
				st.Retries += b.PrepareAttempts - 1
			}
			if b.FinalAttempts > 1 {
				st.Retries += b.FinalAttempts - 1
			}
		}
	}
	if latencyN > 0 {
		st.AvgLatencyMs = latencySum / float64(latencyN)
	}
	span := maxEnd.Sub(minStart).Seconds()
	if span > 0.001 {
		st.Throughput = float64(st.Committed+st.RolledBack) / span
	}
	return st
}

// Pending returns non-terminal txns for recovery / timeout scanning.
func (s *Store) Pending() []*model.Txn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Txn, 0, 64)
	for _, t := range s.txns {
		if !t.Terminal() {
			out = append(out, t.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// All returns every txn, newest first, capped at limit (for stats/export).
func (s *Store) All(limit int) []*model.Txn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.order) {
		limit = len(s.order)
	}
	out := make([]*model.Txn, 0, limit)
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.txns[s.order[i]].Clone())
	}
	return out
}

// --- durable snapshots -----------------------------------------------------

type snapshot struct {
	SavedAt  time.Time          `json:"saved_at"`
	Txns     []*model.Txn       `json:"txns"`
	Branches []*model.Branch    `json:"branches"`
	Comps    []model.CompRecord `json:"comps"`
}

// SnapshotPath returns the configured path.
func (s *Store) SnapshotPath() string { return s.path }

// Save writes the state atomically if a path is configured and something
// changed. Terminal transactions are kept (the audit trail matters) but the
// caller is expected to rotate the file.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snap := snapshot{SavedAt: time.Now()}
	snap.Txns = make([]*model.Txn, 0, len(s.order))
	for _, id := range s.order {
		if t := s.txns[id]; t != nil {
			snap.Txns = append(snap.Txns, t)
		}
	}
	snap.Branches = make([]*model.Branch, 0, len(s.branches))
	for _, b := range s.branches {
		snap.Branches = append(snap.Branches, b)
	}
	snap.Comps = append(snap.Comps, s.comps...)
	s.dirty = false
	s.mu.Unlock()

	buf, err := json.Marshal(&snap)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Load restores a snapshot, if present. Errors are returned so the caller can
// decide to start empty (and shout).
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	buf, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(buf, &snap); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range snap.Txns {
		if _, ok := s.txns[t.ID]; ok {
			continue
		}
		s.txns[t.ID] = t
		s.order = append(s.order, t.ID)
	}
	for _, b := range snap.Branches {
		s.branches[b.ID] = b
	}
	s.comps = append(s.comps, snap.Comps...)
	if len(s.comps) > maxCompRecords {
		s.comps = s.comps[len(s.comps)-maxCompRecords:]
	}
	return nil
}

// Close persists one final time.
func (s *Store) Close() error { return s.Save() }

// StartAutoSave writes a snapshot every interval until stop is closed.
func (s *Store) StartAutoSave(interval time.Duration, stop <-chan struct{}) {
	if s.path == "" {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = s.Save()
			case <-stop:
				_ = s.Save()
				return
			}
		}
	}()
}
