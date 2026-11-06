// idp-sandbox: an MCP server that runs agent-written code in isolated containers (ADR-0001).
//
// It is meant to sit behind the idp-mcp gateway as an upstream on a private network: the gateway
// authenticates, authorizes and audits every call, and this server has no credentials of its own.
// Configuration is by environment:
//
//	IDP_SANDBOX_LISTEN         address of /mcp (default 127.0.0.1:8570); a non-loopback address needs
//	                           IDP_SANDBOX_PRIVATE_NETWORK=true (the operator's statement that only the gateway reaches it)
//	IDP_SANDBOX_METRICS        address of /healthz /readyz /metrics (default 127.0.0.1:9570)
//	IDP_SANDBOX_DOCKER_HOST    default unix:///var/run/docker.sock
//	IDP_SANDBOX_PYTHON_IMAGE   default python:3.12-slim      IDP_SANDBOX_SHELL_IMAGE  default alpine:3.22
//	IDP_SANDBOX_RUNTIME        OCI runtime of the runs, for example runsc (gVisor) or kata; default the daemon's
//	IDP_SANDBOX_OWNER          label value that scopes the sweep (default "default")
//	IDP_SANDBOX_MEMORY_MB IDP_SANDBOX_CPUS IDP_SANDBOX_PIDS IDP_SANDBOX_TIMEOUT_S IDP_SANDBOX_OUTPUT_KB
//	IDP_SANDBOX_REQUIRE_CALLER  default true: a request must carry the caller the gateway forwards (register the
//	                           server in idp-mcp with identity=forward); false only for a standalone development run
//	IDP_SANDBOX_MAX_PER_CALLER  workspaces one caller may hold (default 8)
//	IDP_SANDBOX_WORKSPACE_MB IDP_SANDBOX_MAX_WORKSPACES IDP_SANDBOX_MAX_CONCURRENT IDP_SANDBOX_IDLE_MIN
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"idp-sandbox/internal/engine"
	"idp-sandbox/internal/mcp"
	"idp-sandbox/internal/sandbox"
	"idp-sandbox/internal/tools"
)

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "invalid %s=%q\n", k, v)
		os.Exit(2)
	}
	return n
}

type metrics struct {
	mu     sync.Mutex
	counts map[[2]string]int64
	nanos  map[string]int64
}

func (m *metrics) Observe(tool, outcome string, d time.Duration) {
	m.mu.Lock()
	m.counts[[2]string{tool, outcome}]++
	m.nanos[tool] += d.Nanoseconds()
	m.mu.Unlock()
}

func (m *metrics) write(w http.ResponseWriter, workspaces int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# TYPE idp_sandbox_calls_total counter")
	for k, v := range m.counts {
		fmt.Fprintf(w, "idp_sandbox_calls_total{tool=%q,outcome=%q} %d\n", k[0], k[1], v)
	}
	fmt.Fprintln(w, "# TYPE idp_sandbox_call_seconds_total counter")
	for k, v := range m.nanos {
		fmt.Fprintf(w, "idp_sandbox_call_seconds_total{tool=%q} %g\n", k, float64(v)/1e9)
	}
	fmt.Fprintf(w, "# TYPE idp_sandbox_workspaces gauge\nidp_sandbox_workspaces %d\n", workspaces)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	listen := env("IDP_SANDBOX_LISTEN", "127.0.0.1:8570")
	if host, _, err := net.SplitHostPort(listen); err != nil {
		fmt.Fprintln(os.Stderr, "invalid IDP_SANDBOX_LISTEN:", err)
		os.Exit(2)
	} else if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && os.Getenv("IDP_SANDBOX_PRIVATE_NETWORK") != "true" {
		fmt.Fprintln(os.Stderr, "refusing to listen on", listen, "without IDP_SANDBOX_PRIVATE_NETWORK=true: this server has no authentication, only the gateway may reach it")
		os.Exit(2)
	}
	cfg := sandbox.DefaultConfig(env("IDP_SANDBOX_PYTHON_IMAGE", "python:3.12-slim"), env("IDP_SANDBOX_SHELL_IMAGE", "alpine:3.22"))
	cfg.Limits.MemoryBytes = envInt("IDP_SANDBOX_MEMORY_MB", cfg.Limits.MemoryBytes>>20) << 20
	cfg.Limits.CPUs = float64(envInt("IDP_SANDBOX_CPUS", int64(cfg.Limits.CPUs)))
	cfg.Limits.Pids = envInt("IDP_SANDBOX_PIDS", cfg.Limits.Pids)
	cfg.Limits.Timeout = time.Duration(envInt("IDP_SANDBOX_TIMEOUT_S", int64(cfg.Limits.Timeout.Seconds()))) * time.Second
	cfg.Limits.OutputBytes = int(envInt("IDP_SANDBOX_OUTPUT_KB", int64(cfg.Limits.OutputBytes>>10))) << 10
	cfg.Limits.WorkspaceBytes = envInt("IDP_SANDBOX_WORKSPACE_MB", cfg.Limits.WorkspaceBytes>>20) << 20
	cfg.MaxWorkspaces = int(envInt("IDP_SANDBOX_MAX_WORKSPACES", int64(cfg.MaxWorkspaces)))
	cfg.MaxConcurrent = int(envInt("IDP_SANDBOX_MAX_CONCURRENT", int64(cfg.MaxConcurrent)))
	cfg.MaxPerOwner = int(envInt("IDP_SANDBOX_MAX_PER_CALLER", int64(cfg.MaxPerOwner)))
	cfg.RequireOwner = os.Getenv("IDP_SANDBOX_REQUIRE_CALLER") != "false"
	cfg.IdleTTL = time.Duration(envInt("IDP_SANDBOX_IDLE_MIN", int64(cfg.IdleTTL.Minutes()))) * time.Minute

	eng, err := engine.New(env("IDP_SANDBOX_DOCKER_HOST", "unix:///var/run/docker.sock"), env("IDP_SANDBOX_SHELL_IMAGE", "alpine:3.22"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	eng.Runtime = os.Getenv("IDP_SANDBOX_RUNTIME")
	eng.Owner = env("IDP_SANDBOX_OWNER", "default")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := eng.Ping(pingCtx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "the container engine does not answer:", err)
		os.Exit(1)
	}
	cancel()
	mgr, err := sandbox.New(ctx, cfg, eng, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	met := &metrics{counts: map[[2]string]int64{}, nanos: map[string]int64{}}
	srv := mcp.New("idp-sandbox", version,
		"Runs code in an isolated sandbox: create a workspace, write files, run Python or shell, read the results. There is no network and nothing persists beyond the workspace.",
		tools.New(mgr, log, met))

	mux := http.NewServeMux()
	mux.Handle("/mcp", srv)
	main := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: cfg.Limits.Timeout + 60*time.Second}
	ops := http.NewServeMux()
	ops.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	ops.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := mgr.Ready(c); err != nil {
			http.Error(w, "container engine unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	ops.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { met.write(w, mgr.Count()) })
	opsSrv := &http.Server{Addr: env("IDP_SANDBOX_METRICS", "127.0.0.1:9570"), Handler: ops, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := mgr.Reap(ctx); n > 0 {
					log.Info("reaped idle workspaces", "n", n)
				}
			}
		}
	}()
	errc := make(chan error, 2)
	go func() { errc <- main.ListenAndServe() }()
	go func() { errc <- opsSrv.ListenAndServe() }()
	log.Info("idp-sandbox listening", "mcp", listen, "ops", opsSrv.Addr, "version", version, "runtime", env("IDP_SANDBOX_RUNTIME", "default"),
		"memoryMB", cfg.Limits.MemoryBytes>>20, "timeout", cfg.Limits.Timeout.String())
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = main.Shutdown(shutdown)
	_ = opsSrv.Shutdown(shutdown)
	// workspaces live in memory: remove them with the process
	_ = eng.Sweep(shutdown)
}
