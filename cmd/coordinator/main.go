// Command coordinator runs the transaction coordinator, the operator console
// and optionally the four simulated participants in one process.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"crossborder/dtx/internal/engine"
	"crossborder/dtx/internal/sim"
	"crossborder/dtx/internal/store"
	"crossborder/dtx/internal/web"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8080", "coordinator listen address")
		dataFile    = flag.String("data", "", "snapshot path for durable state (empty = memory only)")
		withSim     = flag.Bool("with-sim", true, "run the four simulated participants in-process")
		simBase     = flag.String("sim-base", "127.0.0.1", "interface for simulated participants to bind")
		callTimeout = flag.Duration("call-timeout", 250*time.Millisecond, "per-call participant timeout")
		deadline    = flag.Duration("stall", 3*time.Second, "final-phase no-progress time before NEEDS_MANUAL")
		servicesArg = flag.String("services", "", "comma-separated name=url participant registry; overrides -with-sim")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	services := map[string]string{}

	participants := map[string]*sim.Participant{}
	if *servicesArg != "" {
		for _, pair := range strings.Split(*servicesArg, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok {
				log.Fatalf("bad -services entry %q, want name=url", pair)
			}
			services[k] = v
		}
	} else if *withSim {
		names := []string{"warehouse", "carrier", "customs", "settlement"}
		for i, name := range names {
			p := sim.NewParticipant(name, fmt.Sprintf("%s:0", *simBase), int64(100+i))
			if err := p.Start(); err != nil {
				log.Fatalf("start %s: %v", name, err)
			}
			participants[name] = p
			services[name] = p.URL()
			log.Printf("participant %-10s %s", name, p.URL())
		}
	} else {
		log.Fatal("no participants: pass -services or keep -with-sim")
	}

	st := store.New(*dataFile)
	if *dataFile != "" {
		if err := st.Load(); err != nil {
			log.Printf("WARNING: snapshot %s unreadable (%v); starting empty", *dataFile, err)
		} else {
			log.Printf("loaded snapshot from %s", *dataFile)
		}
	}
	stopSave := make(chan struct{})
	st.StartAutoSave(2*time.Second, stopSave)

	cfg := engine.DefaultConfig()
	cfg.CallTimeout = *callTimeout
	cfg.StallDeadline = *deadline
	eng := engine.New(cfg, st, services)
	eng.Start()

	srv := &web.Server{St: st, Eng: eng, Services: services}
	handler := srv.Handler()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
	}
	go func() {
		log.Printf("coordinator listening on http://%s/  (console at /)", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	// Print a compact status line every 5s so a terminal user sees progress.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			s := st.Stats()
			names := make([]string, 0, len(participants))
			for n := range participants {
				names = append(names, n)
			}
			sort.Strings(names)
			line := fmt.Sprintf("txns=%d committed=%d rolled_back=%d in_flight=%d manual=%d",
				s.Total, s.Committed, s.RolledBack, s.InFlight, s.NeedsManual)
			for _, n := range names {
				m := participants[n].Metrics()
				line += fmt.Sprintf(" | %s hold=%d fenced=%d", n, m.Orphans, m.Fenced)
			}
			log.Print(line)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Print("shutting down; in-flight transactions will resume on next start if -data is set")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	eng.Close()
	close(stopSave)
	_ = st.Close()
	for _, p := range participants {
		_ = p.Close()
	}
}
