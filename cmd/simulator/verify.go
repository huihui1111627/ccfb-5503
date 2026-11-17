package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"crossborder/dtx/wire"
)

// exportTxn mirrors the coordinator's /api/export shape.
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

type anomaly struct {
	Kind   string `json:"kind"`
	TxnID  string `json:"txn_id,omitempty"`
	BizID  string `json:"biz_id,omitempty"`
	Detail string `json:"detail"`
}

// report is the outcome of a simulation run.
type report struct {
	Style             string           `json:"style"`
	Transactions      int              `json:"transactions"`
	Committed         int              `json:"committed"`
	RolledBack        int              `json:"rolled_back"`
	NeedsManual       int              `json:"needs_manual"`
	InFlight          int              `json:"in_flight"`
	OperatorCommitted int              `json:"operator_committed"`
	OperatorRolled    int              `json:"operator_rolled_back"`
	ManualArbitrated  int              `json:"manual_arbitrated"`
	EmptyRollbacks    int              `json:"empty_rollbacks"`
	Compensations     int              `json:"compensations"`
	Retries           int              `json:"retries"`
	Fenced            int64            `json:"fenced_late_try"`
	Orphans           map[string]int64 `json:"orphans"`
	Balances          map[string]int64 `json:"balances"`
	FourWayOrders     int              `json:"four_way_effective_orders"`
	Anomalies         []anomaly        `json:"anomalies"`
}

// OK reports whether the run proved the invariant with nothing left over.
func (r report) OK() bool {
	return len(r.Anomalies) == 0 && r.NeedsManual == 0 && r.InFlight == 0
}

type ledger struct {
	Service string             `json:"service"`
	Entries []wire.LedgerEntry `json:"entries"`
}

func fetchAllTxns(base string) ([]exportTxn, error) {
	var out []exportTxn
	offset := 0
	for {
		var page struct {
			Items []exportTxn `json:"items"`
			Total int         `json:"total"`
		}
		if err := getJSON(context.Background(),
			fmt.Sprintf("%s/api/export?offset=%d&limit=2000", base, offset), &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if len(page.Items) == 0 || len(out) >= page.Total {
			break
		}
		offset += len(page.Items)
	}
	return out, nil
}

// verifyAll checks the all-or-nothing invariant and the four ledgers.
func verifyAll(e *env) report {
	r := report{Orphans: map[string]int64{}, Balances: map[string]int64{}, Anomalies: []anomaly{}}
	r.Style = map[bool]string{true: "in-process", false: "external"}[e.inProcess]

	txns, err := fetchAllTxns(e.base)
	if err != nil {
		r.Anomalies = append(r.Anomalies, anomaly{Kind: "EXPORT_FAILED", Detail: err.Error()})
		return r
	}

	// service -> txn id -> amount recorded as still effective
	held := map[string]map[string]int64{}
	for _, name := range participants {
		var l ledger
		if err := fetchLedger(e, name, &l); err != nil {
			r.Anomalies = append(r.Anomalies, anomaly{Kind: "LEDGER_UNREADABLE", Detail: name + ": " + err.Error()})
			continue
		}
		m := map[string]int64{}
		for _, entry := range l.Entries {
			if prev, dup := m[entry.TxnID]; dup && prev != entry.Amount {
				r.Anomalies = append(r.Anomalies, anomaly{Kind: "SERVICE_INTERNAL_INCONSISTENT", TxnID: entry.TxnID,
					Detail: fmt.Sprintf("%s holds both %d and %d for one txn", name, prev, entry.Amount)})
			}
			m[entry.TxnID] = entry.Amount
		}
		held[name] = m
		r.Balances[name] = sum(m)
	}

	coordConfirmed := map[string]int64{} // service -> amount according to coordinator
	r.Transactions = len(txns)
	for _, t := range txns {
		switch t.Status {
		case "COMMITTED":
			r.Committed++
			if t.Partial {
				r.OperatorCommitted++
			}
		case "ROLLED_BACK":
			r.RolledBack++
			if t.Partial {
				r.OperatorRolled++
			}
		case "NEEDS_MANUAL":
			r.NeedsManual++
		default:
			r.InFlight++
		}

		hasEffect := map[string]bool{}
		for _, b := range t.Branches {
			if b.EmptyRollback {
				r.EmptyRollbacks++
			}
			r.Compensations += b.CompensationOK
			if b.PrepareAttempts > 1 {
				r.Retries += b.PrepareAttempts - 1
			}
			if b.FinalAttempts > 1 {
				r.Retries += b.FinalAttempts - 1
			}
			if b.Status == "CONFIRMED" || b.Status == "EXECUTED" || b.Status == "IRREVERSIBLE" {
				hasEffect[b.Service] = true
				coordConfirmed[b.Service] += b.Amount
			}
		}

		if t.Status == "ROLLED_BACK" && !t.Partial {
			for _, name := range participants {
				if amt, ok := held[name][t.ID]; ok {
					r.Anomalies = append(r.Anomalies, anomaly{Kind: "ROLLED_BACK_STILL_EFFECTIVE",
						TxnID: t.ID, BizID: t.BizID,
						Detail: fmt.Sprintf("%s still holds %d after rollback", name, amt)})
				}
			}
		}
		if t.Status == "COMMITTED" && !t.Partial {
			for _, name := range participants {
				amt, ok := held[name][t.ID]
				if !ok {
					r.Anomalies = append(r.Anomalies, anomaly{Kind: "COMMITTED_MISSING_EFFECT",
						TxnID: t.ID, BizID: t.BizID,
						Detail: fmt.Sprintf("%s has no effect for a committed transaction", name)})
					continue
				}
				want := int64(-1)
				for _, b := range t.Branches {
					if b.Service == name {
						want = b.Amount
					}
				}
				if amt != want {
					r.Anomalies = append(r.Anomalies, anomaly{Kind: "AMOUNT_MISMATCH",
						TxnID: t.ID, BizID: t.BizID,
						Detail: fmt.Sprintf("%s recorded %d, order says %d", name, amt, want)})
				}
			}
		}
	}

	// Participant-side books: no live hold may survive a settled transaction.
	for _, name := range participants {
		var m wire.Metrics
		if e.parts[name] != nil {
			m = e.parts[name].Metrics()
		} else {
			var svc struct {
				Metrics []wire.Metrics `json:"metrics"`
			}
			_ = getJSON(context.Background(), e.base+"/api/services", &svc)
			for _, x := range svc.Metrics {
				if x.Service == name {
					m = x
				}
			}
		}
		r.Orphans[name] = m.Orphans
		r.Fenced += m.Fenced
		if m.Orphans > 0 && r.InFlight == 0 && r.NeedsManual == 0 {
			r.Anomalies = append(r.Anomalies, anomaly{Kind: "DANGLING_RESERVATION",
				Detail: fmt.Sprintf("%s reports %d live holds with no transaction in flight", name, m.Orphans)})
		}
	}

	// Coordinator's view of each service must equal that service's ledger.
	for _, name := range participants {
		if held[name] == nil {
			continue
		}
		if coordConfirmed[name] != r.Balances[name] {
			r.Anomalies = append(r.Anomalies, anomaly{Kind: "BALANCE_DISAGREES",
				Detail: fmt.Sprintf("%s ledger sums to %d, coordinator says %d", name, r.Balances[name], coordConfirmed[name])})
		}
	}

	// The four systems must agree on WHICH orders are live, not on how much
	// each billed: storage, freight, duty and settlement are different amounts
	// for the same order. Reconciled means the effective order set is
	// identical across all four and each recorded amount matches the order.
	liveSets := map[string]map[string]bool{}
	for _, name := range participants {
		set := map[string]bool{}
		for _, t := range txns {
			if _, ok := held[name][t.ID]; ok {
				set[t.ID] = true
			}
		}
		liveSets[name] = set
	}
	var base map[string]bool
	baseName := ""
	for _, name := range participants {
		set := liveSets[name]
		if base == nil {
			base, baseName = set, name
			continue
		}
		for id := range set {
			if !base[id] {
				r.Anomalies = append(r.Anomalies, anomaly{Kind: "FOUR_WAY_SET_MISMATCH", TxnID: id,
					Detail: fmt.Sprintf("%s holds this order but %s does not", name, baseName)})
			}
		}
		for id := range base {
			if !set[id] {
				r.Anomalies = append(r.Anomalies, anomaly{Kind: "FOUR_WAY_SET_MISMATCH", TxnID: id,
					Detail: fmt.Sprintf("%s holds this order but %s does not", baseName, name)})
			}
		}
	}
	r.FourWayOrders = len(base)
	if base != nil && len(base) != r.Committed {
		r.Anomalies = append(r.Anomalies, anomaly{Kind: "FOUR_WAY_COUNT_MISMATCH",
			Detail: fmt.Sprintf("four services hold %d live orders, coordinator reports %d committed", len(base), r.Committed)})
	}
	return r
}

func fetchLedger(e *env, name string, out *ledger) error {
	if p := e.parts[name]; p != nil {
		l := p.Export()
		out.Service = l.Service
		out.Entries = l.Entries
		return nil
	}
	return getJSON(context.Background(), e.base+"/ledger/"+name, out)
}

func sum(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

func printReport(r report, path string) {
	sort.Slice(r.Anomalies, func(i, j int) bool { return r.Anomalies[i].TxnID < r.Anomalies[j].TxnID })
	log.Printf("========== 核对结果 ==========")
	log.Printf("运行方式=%s 事务=%d 已提交=%d 已回滚=%d 待裁决=%d 进行中=%d",
		r.Style, r.Transactions, r.Committed, r.RolledBack, r.NeedsManual, r.InFlight)
	log.Printf("人工裁决=%d（提交 %d / 回滚 %d）", r.ManualArbitrated, r.OperatorCommitted, r.OperatorRolled)
	log.Printf("空回滚=%d 补偿执行=%d 重试=%d 迟到预留被拒=%d",
		r.EmptyRollbacks, r.Compensations, r.Retries, r.Fenced)
	names := make([]string, 0, len(r.Balances))
	for n := range r.Balances {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		log.Printf("账本 %-11s 余额=%d 悬挂预留=%d", n, r.Balances[n], r.Orphans[n])
	}
	log.Printf("四方生效订单集合一致: %d 笔（协调器已提交 %d 笔）", r.FourWayOrders, r.Committed)
	ok := len(r.Anomalies) == 0 && r.NeedsManual == 0 && r.InFlight == 0
	if ok {
		log.Printf("结论: PASS — 每笔下单要么四方全部生效，要么四方全部撤销；四方生效订单集合与协调器一致，账已对平")
	} else {
		log.Printf("结论: FAIL — %d 处异常", len(r.Anomalies))
		shown := r.Anomalies
		if len(shown) > 40 {
			shown = shown[:40]
		}
		for _, a := range shown {
			log.Printf("  [%s] txn=%s biz=%s %s", a.Kind, a.TxnID, a.BizID, a.Detail)
		}
	}
	if path != "" {
		buf, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			log.Printf("写报告失败: %v", err)
		} else {
			log.Printf("报告已写入 %s", path)
		}
	}
	_ = strings.TrimSpace
}
