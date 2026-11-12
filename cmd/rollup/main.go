// Command rollup runs the educational rollup.
//
//	rollup devnet   --config rollup.json --l1 http://127.0.0.1:8545 --listen 127.0.0.1:9545
//	rollup verifier --config rollup.json --l1 http://127.0.0.1:8545 --listen 127.0.0.1:9546
//
// devnet runs the sequencer, the batcher and the derivation in one process (the batcher key comes
// from the BATCHER_KEY environment variable). verifier runs only the derivation: it rebuilds the
// canonical chain from L1 and serves it read-only.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/sergiiogg7/go-mini-rollup/internal/batcher"
	"github.com/sergiiogg7/go-mini-rollup/internal/config"
	"github.com/sergiiogg7/go-mini-rollup/internal/derive"
	"github.com/sergiiogg7/go-mini-rollup/internal/exec"
	"github.com/sergiiogg7/go-mini-rollup/internal/l1"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/rpcapi"
	"github.com/sergiiogg7/go-mini-rollup/internal/sequencer"
)

const version = "go-mini-rollup/0.1.0"

//go:embed status.html
var statusPage []byte

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rollup devnet|verifier|version [flags]")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "devnet":
		run(log, true, os.Args[2:])
	case "verifier":
		run(log, false, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

func run(log *slog.Logger, sequence bool, args []string) {
	fs := flag.NewFlagSet("rollup", flag.ExitOnError)
	cfgPath := fs.String("config", "rollup.json", "rollup configuration (the L2 genesis)")
	l1URL := fs.String("l1", "http://127.0.0.1:8545", "L1 JSON-RPC endpoint")
	listen := fs.String("listen", "127.0.0.1:9545", "address for the L2 JSON-RPC, /status and /metrics")
	_ = fs.Parse(args)

	fatal := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config", err)
	}
	cl, err := l1.Dial(*l1URL, cfg.Inbox, cfg.Portal)
	if err != nil {
		fatal("dial L1", err)
	}
	defer cl.Close()

	reg := metrics.NewRegistry()
	metrics.RegisterAll(reg)
	p0 := cfg.Forks[0].Params
	ex := exec.New(p0.ChainID, p0.GasLimit, cfg.FeeVault)
	der, err := derive.New(cfg, ex, cl, reg)
	if err != nil {
		fatal("create derivator", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var chain rpcapi.Chain
	if sequence {
		seq, err := sequencer.New(cfg, ex, cl, der, reg)
		if err != nil {
			fatal("create sequencer", err)
		}
		keyHex := strings.TrimPrefix(os.Getenv("BATCHER_KEY"), "0x")
		key, err := crypto.HexToECDSA(keyHex)
		if err != nil {
			fatal("BATCHER_KEY must be a hex private key", err)
		}
		if crypto.PubkeyToAddress(key.PublicKey) != cfg.Batcher {
			fatal("batcher key", errors.New("BATCHER_KEY does not match the batcher address in the config"))
		}
		bat := batcher.New(cfg, seq, cl, cl, key, reg)
		chain = &rpcapi.SeqChain{Seq: seq, Der: der, L1: cl, Bat: bat, Reg: reg}
		go every(ctx, 300*time.Millisecond, func() {
			if _, err := seq.Tick(); err != nil {
				reg.Inc(metrics.LoopErrors, "loop", "sequencer")
				log.Warn("sequencer tick", "err", err)
			}
		})
		go every(ctx, time.Second, func() {
			if err := bat.Tick(); err != nil {
				reg.Inc(metrics.LoopErrors, "loop", "batcher")
				log.Warn("batcher tick", "err", err)
			}
		})
	} else {
		chain = &rpcapi.VerifierChain{Der: der, L1: cl}
	}
	go every(ctx, 200*time.Millisecond, func() {
		for {
			ok, err := der.Step()
			if errors.Is(err, derive.ErrDeepReorg) {
				fatal("derivation", err)
			}
			if err != nil {
				reg.Inc(metrics.LoopErrors, "loop", "derivation")
				log.Warn("derivation step", "err", err)
				return
			}
			if !ok {
				return
			}
		}
	})

	rpcHandler, err := rpcapi.New(chain, ex, p0.ChainID, version)
	if err != nil {
		fatal("rpc", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		switch {
		case r.Method == http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(statusPage)
		default:
			rpcHandler.ServeHTTP(w, r)
		}
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	role := "verifier"
	if sequence {
		role = "sequencer+batcher+derivation"
	}
	log.Info("rollup started", "role", role, "chainId", p0.ChainID, "rpc", "http://"+*listen, "status", "http://"+*listen+"/", "l1", *l1URL, "l1Start", cfg.L1Start)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("serve", err)
	}
	log.Info("stopped")
}
