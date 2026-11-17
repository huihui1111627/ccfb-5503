package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crossborder/dtx/internal/engine"
	"crossborder/dtx/internal/sdk"
	"crossborder/dtx/internal/sim"
	"crossborder/dtx/internal/store"
	"crossborder/dtx/internal/web"
)

var participants = []string{"warehouse", "carrier", "customs", "settlement"}

type options struct {
	n           int
	concurrency int
	timeoutMs   int
	parallelPct int
	tccPct      int
	seed        int64
	chaos       float64
	quiet       bool
	drill       bool
}

func main() {
	var (
		n           = flag.Int("n", 20000, "number of order transactions")
		conc        = flag.Int("c", 40, "concurrent transactions")
		timeoutMs   = flag.Int("timeout", 1500, "per-transaction deadline in ms")
		parallelPct = flag.Int("parallel", 50, "percent of transactions whose final phase runs in parallel")
		tccPct      = flag.Int("tcc", 60, "percent of transactions using TCC (rest Saga)")
		seed        = flag.Int64("seed", time.Now().UnixNano(), "random seed")
		addr        = flag.String("addr", "127.0.0.1:0", "in-process coordinator listen address")
		coordURL    = flag.String("coordinator", "", "external coordinator URL; empty runs everything in-process")
		chaos       = flag.Float64("chaos", 1.0, "fault multiplier (0 disables injected faults)")
		reportOut   = flag.String("report", "", "write the machine-readable report to this path")
		drill       = flag.Bool("drill", true, "run the manual-arbitration drill after the workload")
		quiet       = flag.Bool("quiet", false, "suppress per-run progress logs")
	)
	flag.Parse()
	if *n <= 0 {
		log.Fatal("-n must be positive")
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("seed=%d n=%d concurrency=%d chaos=%.2f", *seed, *n, *conc, *chaos)

	opts := options{n: *n, concurrency: *conc, timeoutMs: *timeoutMs, parallelPct: *parallelPct,
		tccPct: *tccPct, seed: *seed, chaos: *chaos, quiet: *quiet, drill: *drill}

	env, err := startEnv(*coordURL, *addr, *seed, opts)
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	defer env.stop()

	runWorkload(env, opts)
	waitSettled(env, 120*time.Second)

	manual := 0
	if opts.drill {
		manual = manualDrill(env, opts)
	}
	// Arbitrate anything the chaos left stalled, then let it settle again.
	manual += arbitrateStalled(env)

	waitSettled(env, 60*time.Second)
	report := verifyAll(env)
	report.ManualArbitrated = manual
	printReport(report, *reportOut)
	if !report.OK() {
		os.Exit(1)
	}
}

// --- environment -----------------------------------------------------------

type env struct {
	base      string
	client    *sdk.Client
	st        *store.Store
	eng       *engine.Engine
	parts     map[string]*sim.Participant
	inProcess bool
	stopFn    func()
	httpSrv   *http.Server
}

func (e *env) stop() {
	if e.stopFn != nil {
		e.stopFn()
	}
}

func startEnv(coordURL, addr string, seed int64, opts options) (*env, error) {
	e := &env{parts: map[string]*sim.Participant{}}
	if coordURL != "" {
		e.base = strings.TrimRight(coordURL, "/")
		e.client = sdk.NewClient(e.base)
		e.inProcess = false
		e.stopFn = func() {}
		if opts.chaos <= 0 {
			if err := scaleFaultsHTTP(e.base, participants, 0); err != nil {
				log.Printf("warning: could not disable participant faults: %v", err)
			}
		}
		log.Printf("external coordinator %s", e.base)
		return e, nil
	}

	services := map[string]string{}
	for i, name := range participants {
		p := sim.NewParticipant(name, addr, seed+int64(i)*31)
		if err := p.Start(); err != nil {
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
		e.parts[name] = p
		services[name] = p.URL()
	}
	if opts.chaos != 1.0 {
		for _, p := range e.parts {
			p.SetFaults(scaledFaults(p.Faults(), opts.chaos))
		}
	}
	e.st = store.New("")
	cfg := engine.DefaultConfig()
	cfg.StallDeadline = 3 * time.Second
	e.eng = engine.New(cfg, e.st, services)
	e.eng.Start()

	handler := (&web.Server{St: e.st, Eng: e.eng, Services: services}).Handler()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	e.httpSrv = &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = e.httpSrv.Serve(ln) }()
	e.base = "http://" + ln.Addr().String()
	e.client = sdk.NewClient(e.base)
	e.inProcess = true
	e.stopFn = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.httpSrv.Shutdown(ctx)
		e.eng.Close()
		for _, p := range e.parts {
			_ = p.Close()
		}
	}
	log.Printf("in-process coordinator at %s", e.base)
	return e, nil
}

func scaledFaults(f sim.Faults, mult float64) sim.Faults {
	if mult <= 0 {
		f.PreUnknownP, f.PostHoldP, f.DropP, f.RejectP, f.SlowHoldP, f.ReorderP = 0, 0, 0, 0, 0, 0
		return f
	}
	f.PreUnknownP *= mult
	f.PostHoldP *= mult
	f.DropP *= mult
	f.RejectP *= mult
	f.SlowHoldP *= mult
	f.ReorderP *= mult
	return f
}

// --- workload --------------------------------------------------------------

func newOrder(rng *rand.Rand, i int, opts options) (bizID, mode string, parallel bool, amounts map[string]int64) {
	bizID = fmt.Sprintf("CB-%06d", i)
	parallel = rng.IntN(100) < opts.parallelPct
	if rng.IntN(100) < opts.tccPct {
		mode = "TCC"
	} else {
		mode = "SAGA"
	}
	amounts = map[string]int64{}
	for _, s := range participants {
		amounts[s] = int64(100 + rng.IntN(5000))
	}
	return
}

func buildOrder(bizID, mode string, parallel bool, amounts map[string]int64, timeoutMs int) *sdk.Builder {
	b := sdk.TCC(bizID)
	if mode == "SAGA" {
		b = sdk.Saga(bizID)
	}
	b.WithTimeout(time.Duration(timeoutMs) * time.Millisecond).WithParallel(parallel)
	for _, s := range participants {
		b.Branch(s, amounts[s], map[string]string{"hs_code": "8517.62", "lane": "SZX-LAX"})
	}
	return b
}

func runWorkload(e *env, opts options) {
	var submitted, failed atomic.Int64
	start := time.Now()
	jobs := make(chan int, opts.concurrency*2)
	var wg sync.WaitGroup
	total := opts.n
	for w := 0; w < opts.concurrency; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(opts.seed)+uint64(worker)*131, uint64(worker)*7919+3))
			for i := range jobs {
				bizID, mode, parallel, amounts := newOrder(rng, i, opts)
				if _, err := e.client.Begin(buildOrder(bizID, mode, parallel, amounts, opts.timeoutMs)); err != nil {
					failed.Add(1)
					if failed.Load() <= 10 {
						log.Printf("submit %s failed: %v", bizID, err)
					}
					continue
				}
				submitted.Add(1)
			}
		}(w + 1)
	}
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	log.Printf("submitted %d/%d transactions (%d submit errors) in %s (%.0f/s)",
		submitted.Load(), total, failed.Load(), elapsed.Round(time.Millisecond),
		float64(submitted.Load())/elapsed.Seconds())
}

// --- waiting & arbitration -------------------------------------------------

type statsResp struct {
	Store  store.Stats        `json:"store"`
	Engine engine.EngineStats `json:"engine"`
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (e *env) stats() (statsResp, error) {
	var s statsResp
	err := getJSON(context.Background(), e.base+"/api/stats", &s)
	return s, err
}

func waitSettled(e *env, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		s, err := e.stats()
		if err == nil && s.Store.InFlight == 0 && s.Store.NeedsManual == 0 {
			log.Printf("all transactions settled")
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	s, _ := e.stats()
	log.Printf("settle wait timed out: in_flight=%d needs_manual=%d", s.Store.InFlight, s.Store.NeedsManual)
}

// stalledTxns lists transactions waiting for an operator.
func (e *env) stalledTxns() []exportTxn {
	var page struct {
		Items []exportTxn `json:"items"`
	}
	_ = getJSON(context.Background(), e.base+"/api/export?limit=2000&offset=0", &page)
	out := make([]exportTxn, 0, 8)
	for _, t := range page.Items {
		if t.Status == "NEEDS_MANUAL" {
			out = append(out, t)
		}
	}
	return out
}

// arbitrateStalled plays the on-call operator: every stalled transaction gets
// an explicit verdict, and the coordinator then drives the branches.
func arbitrateStalled(e *env) int {
	n := 0
	for _, t := range e.stalledTxns() {
		var parts []string
		for _, b := range t.Branches {
			parts = append(parts, fmt.Sprintf("%s/%s/prep=%d/fin=%d/%s", b.Service, b.Status, b.PrepareAttempts, b.FinalAttempts, b.LastError))
		}
		log.Printf("stalled %s decision=%s mode=%s branches: %s", t.ID, t.Decision, t.Mode, strings.Join(parts, " | "))
		// Prefer the direction the coordinator already chose; fall back to
		// rollback, which is the safe outcome.
		dec := t.Decision
		if dec == "" {
			dec = "ROLLBACK"
		}
		if err := e.client.Manual(context.Background(), t.ID, dec, "ops-oncall",
			"fault cleared; adopting the coordinator's direction"); err != nil {
			log.Printf("arbitrate %s: %v", t.ID, err)
			continue
		}
		n++
	}
	if n > 0 {
		log.Printf("operator arbitrated %d stalled transactions", n)
	}
	return n
}

// manualDrill stalls a few transactions for real: one participant is taken
// down while calls are in flight, the transactions escalate to NEEDS_MANUAL
// (the coordinator cannot decide what the dead participant did), and an
// operator then settles them through the same API the console uses. Some are
// arbitrated as COMMIT, some as ROLLBACK, and both must leave the books
// consistent.
func manualDrill(e *env, opts options) int {
	victim := e.parts["settlement"]
	if victim == nil {
		return 0 // external mode: the simulator does not own the participants
	}
	log.Printf("manual drill: taking settlement down mid-flight")
	victim.SetDown(30 * time.Second) // long enough that the operator decides first
	ids := make([]string, 0, 6)
	rng := rand.New(rand.NewPCG(uint64(opts.seed), 0x5eed))
	for i := 0; i < 6; i++ {
		bizID, mode, parallel, amounts := newOrder(rng, 900000+i, opts)
		tx, err := e.client.Begin(buildOrder(bizID, mode, parallel, amounts, 600))
		if err != nil {
			log.Printf("drill submit failed: %v", err)
			continue
		}
		ids = append(ids, tx.ID())
	}
	deadline := time.Now().Add(12 * time.Second)
	escalated := 0
	for time.Now().Before(deadline) {
		seen := map[string]bool{}
		for _, t := range e.stalledTxns() {
			seen[t.ID] = true
		}
		escalated = 0
		for _, id := range ids {
			if seen[id] {
				escalated++
			}
		}
		if escalated == len(ids) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("manual drill: %d/%d transactions escalated to NEEDS_MANUAL while settlement was down",
		escalated, len(ids))
	if escalated == 0 {
		return 0
	}
	decided := 0
	for i, id := range ids {
		t, gerr := e.client.Get(context.Background(), id)
		if gerr != nil {
			log.Printf("manual drill: read %s: %v", id, gerr)
			continue
		}
		// The operator is not asked to reverse a safety rollback: adopt the
		// direction the coordinator already locked, and for the ones still
		// undecided alternate between COMMIT and ROLLBACK.
		dec := string(t.Decision)
		if dec == "" {
			dec = "ROLLBACK"
			if i%2 == 0 {
				dec = "COMMIT"
			}
		}
		err := e.client.Manual(context.Background(), id, dec, "ops-drill",
			"drill: settlement unreachable; on-call settles the customer order")
		if err != nil {
			// A transaction that finished on its own is fine.
			if t, gerr := e.client.Get(context.Background(), id); gerr == nil && t.Terminal() {
				log.Printf("manual drill: %s already finished as %s", id, t.Status)
				continue
			}
			log.Printf("manual drill: decide %s: %v", id, err)
			continue
		}
		decided++
	}
	log.Printf("manual drill: operator arbitrated %d/%d stalled orders", decided, len(ids))
	// The participant comes back; the operator's forced decisions stay.
	victim.SetDown(0)
	return decided
}

// --- fault control for external mode --------------------------------------

func scaleFaultsHTTP(base string, names []string, mult float64) error {
	for _, n := range names {
		var f sim.Faults
		if err := getJSON(context.Background(), base+"/api/services/"+n+"/faults", &f); err != nil {
			return err
		}
		body, _ := json.Marshal(scaledFaults(f, mult))
		req, _ := http.NewRequest(http.MethodPost, base+"/api/services/"+n+"/faults", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
	}
	return nil
}
