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
//	IDP_SANDBOX_OFFICE_IMAGE   the office run profile's image (ADR-0001 D14, deploy/office); unset = no language office
//	IDP_SANDBOX_RUNTIME        OCI runtime of the runs, for example runsc (gVisor) or kata; default the daemon's
//	IDP_SANDBOX_OWNER          label value that scopes the sweep (default "default")
//	IDP_SANDBOX_MEMORY_MB IDP_SANDBOX_CPUS IDP_SANDBOX_PIDS IDP_SANDBOX_TIMEOUT_S IDP_SANDBOX_OUTPUT_KB
//	IDP_SANDBOX_EGRESS_PROXY    enables network=egress for runs (default off). Docker backend: the name of the running
//	                           idp-sandbox-egress container. Kubernetes backend: the DNS name of the proxy's Service (or
//	                           its cluster IP); the NetworkPolicies of deploy/kubernetes-egress decide who reaches it (D22)
//	IDP_SANDBOX_EGRESS_PORT     its port (default 3128)
//	IDP_SANDBOX_EGRESS_KEY_FILE the key shared with the proxy (its IDP_EGRESS_KEY_FILE): a run's proxy URL then carries a
//	                           credential that says the caller's tenant, and the proxy applies that tenant's allow-list
//	                           (ADR-0001 D21). The tenant is the forwarded caller's, so verify it (IDP_SANDBOX_CALLER_KEYS_FILE)
//	IDP_SANDBOX_REQUIRE_CALLER  default true: a request must carry the caller the gateway forwards (register the
//	                           server in idp-mcp with identity=forward); false only for a standalone development run
//	IDP_SANDBOX_CALLER_KEYS_FILE  PEM public key(s) of the gateway (P-256): every tools/call must then carry the gateway's
//	                           assertion over the caller (ADR-0001 D18; the gateway's IDP_GATEWAY_CALLER_KEY_FILE), signed for
//	IDP_SANDBOX_CALLER_AUDIENCE   this server's endpoint as registered in idp-mcp, for example http://sandbox:8570/mcp.
//	                           Unset: the forwarded caller is taken as it arrives, and only the private network vouches for it
//	IDP_SANDBOX_MAX_PER_CALLER  workspaces one caller may hold (default 8)
//	IDP_SANDBOX_WORKSPACE_MB IDP_SANDBOX_MAX_WORKSPACES IDP_SANDBOX_MAX_CONCURRENT IDP_SANDBOX_IDLE_MIN
//	IDP_SANDBOX_NETWORK_CHECK   Kubernetes backend: enforce (default) refuses to start when a pod labelled like a run can
//	                           reach the API server (the NetworkPolicy is missing or the CNI does not enforce it, D20);
//	                           warn logs it and starts; off does not look
//	IDP_SANDBOX_FILES_URL       the Workspace's internal file endpoint (e.g. http://workspace:8560/internal/v1/files); set,
//	                           it offers sandbox_import_file and sandbox_export_file (ADR-0001 D15; on Kubernetes the bytes
//	                           travel in pieces through ConfigMaps and pod logs, D19)
//	IDP_SANDBOX_TRANSFER_MB     largest file moved in or out (default 25; at most the workspace's size)
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

	"idp-sandbox/internal/egress"
	"idp-sandbox/internal/engine"
	"idp-sandbox/internal/files"
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
	if img := os.Getenv("IDP_SANDBOX_OFFICE_IMAGE"); img != "" {
		cfg.Languages["office"] = sandbox.OfficeLanguage(img)
	}
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
	cfg.MaxTransferBytes = envInt("IDP_SANDBOX_TRANSFER_MB", cfg.MaxTransferBytes>>20) << 20
	if cfg.MaxTransferBytes > cfg.Limits.WorkspaceBytes {
		fmt.Fprintln(os.Stderr, "IDP_SANDBOX_TRANSFER_MB is larger than the workspace (IDP_SANDBOX_WORKSPACE_MB)")
		os.Exit(2)
	}

	// Egress (D13, D21, D22): the proxy runs may be let out through, and the key shared with it. Off without a proxy.
	egressProxy, egressPort := os.Getenv("IDP_SANDBOX_EGRESS_PROXY"), int(envInt("IDP_SANDBOX_EGRESS_PORT", 3128))
	var egressKey []byte
	if f := os.Getenv("IDP_SANDBOX_EGRESS_KEY_FILE"); f != "" {
		if egressProxy == "" {
			fmt.Fprintln(os.Stderr, "IDP_SANDBOX_EGRESS_KEY_FILE is set without IDP_SANDBOX_EGRESS_PROXY")
			os.Exit(2)
		}
		key, err := egress.LoadKey(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "IDP_SANDBOX_EGRESS_KEY_FILE:", err)
			os.Exit(2)
		}
		egressKey = key
	}
	cfg.EgressEnabled = egressProxy != ""

	var be sandbox.Backend
	switch backend := env("IDP_SANDBOX_BACKEND", "docker"); backend {
	case "docker":
		eng, err := engine.New(env("IDP_SANDBOX_DOCKER_HOST", "unix:///var/run/docker.sock"), env("IDP_SANDBOX_SHELL_IMAGE", "alpine:3.22"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		eng.Runtime = os.Getenv("IDP_SANDBOX_RUNTIME")
		eng.Owner = env("IDP_SANDBOX_OWNER", "default")
		eng.EgressProxy, eng.EgressPort, eng.EgressKey = egressProxy, egressPort, egressKey
		be = eng
	case "kubernetes":
		k, err := newKubeBackend(cfg, egressProxy, egressPort, egressKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		be = k
	default:
		fmt.Fprintf(os.Stderr, "unknown IDP_SANDBOX_BACKEND %q (docker or kubernetes)\n", backend)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := be.Ping(pingCtx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "the isolation backend does not answer:", err)
		os.Exit(1)
	}
	cancel()
	// D20: on Kubernetes "no network" is a NetworkPolicy, which holds only where the CNI enforces it. Find out before
	// serving: a pod labelled like a run must not reach the API server.
	if p, ok := be.(interface {
		APIServerAddr() (string, int, bool)
		NetworkProbe(context.Context, string, int) (bool, error)
		EgressProbe(context.Context, string, int) (bool, error, error)
	}); ok {
		switch mode := env("IDP_SANDBOX_NETWORK_CHECK", "enforce"); mode {
		case "off":
			log.Warn("the network probe is off: nothing checks that run pods are cut off from the network")
		case "enforce", "warn":
			host, port, usable := p.APIServerAddr()
			if !usable {
				log.Warn("the network probe was skipped: the API server address is not one a pod would use", "address", host)
				break
			}
			probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			reached, err := p.NetworkProbe(probeCtx, host, port)
			cancel()
			switch {
			case err != nil && mode == "enforce":
				fmt.Fprintln(os.Stderr, "the network probe could not run (IDP_SANDBOX_NETWORK_CHECK=warn or off to start anyway):", err)
				os.Exit(1)
			case err != nil:
				log.Warn("the network probe could not run", "err", err)
			case reached && mode == "enforce":
				fmt.Fprintf(os.Stderr, "a pod labelled like a run reached %s:%d: the NetworkPolicy of the runs namespace is missing or the cluster's CNI does not enforce it, so runs would have the network. Refusing to start (IDP_SANDBOX_NETWORK_CHECK=warn or off to start anyway)\n", host, port)
				os.Exit(1)
			case reached:
				log.Error("RUNS ARE NOT CUT OFF FROM THE NETWORK: a pod labelled like a run reached the API server", "address", host, "port", port)
			default:
				log.Info("run pods are cut off from the network (the probe could not reach the API server)", "address", host, "port", port)
			}
			if !cfg.EgressEnabled {
				break
			}
			// D22: a run that may use the egress proxy is let out by a NetworkPolicy of its own. It must lead to the
			// proxy and nowhere else.
			probeCtx, cancel = context.WithTimeout(ctx, 90*time.Second)
			reached, proxyErr, err := p.EgressProbe(probeCtx, host, port)
			cancel()
			switch {
			case err != nil && mode == "enforce":
				fmt.Fprintln(os.Stderr, "the network probe of runs with network=egress could not run (IDP_SANDBOX_NETWORK_CHECK=warn or off to start anyway):", err)
				os.Exit(1)
			case err != nil:
				log.Warn("the network probe of runs with network=egress could not run", "err", err)
			case reached && mode == "enforce":
				fmt.Fprintf(os.Stderr, "a pod labelled like a run with network=egress reached %s:%d: the NetworkPolicy that lets those runs out lets them reach more than the egress proxy. Refusing to start (IDP_SANDBOX_NETWORK_CHECK=warn or off to start anyway)\n", host, port)
				os.Exit(1)
			case reached:
				log.Error("RUNS WITH network=egress REACH MORE THAN THE EGRESS PROXY: a pod labelled like one reached the API server", "address", host, "port", port)
			case proxyErr != nil:
				// not a reason to stay down: nothing is exposed, and the proxy may simply start after this service
				log.Warn("a pod labelled like a run with network=egress did not reach the egress proxy: such runs will fail until the proxy is up and a NetworkPolicy lets them in", "proxy", egressProxy, "err", proxyErr)
			default:
				log.Info("run pods with network=egress reach the egress proxy and not the API server", "proxy", egressProxy, "address", host, "port", port)
			}
		default:
			fmt.Fprintf(os.Stderr, "IDP_SANDBOX_NETWORK_CHECK must be enforce, warn or off, not %q\n", mode)
			os.Exit(2)
		}
	}
	mgr, err := sandbox.New(ctx, cfg, be, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	met := &metrics{counts: map[[2]string]int64{}, nanos: map[string]int64{}}
	var fc *files.Client
	if u := os.Getenv("IDP_SANDBOX_FILES_URL"); u != "" {
		if !mgr.TransfersSupported() {
			fmt.Fprintln(os.Stderr, "IDP_SANDBOX_FILES_URL is set but this backend does not move files byte for byte, or IDP_SANDBOX_TRANSFER_MB is 0")
			os.Exit(2)
		}
		fc = files.NewClient(u, cfg.MaxTransferBytes)
		log.Info("file transfers enabled", "endpoint", u, "maxMB", cfg.MaxTransferBytes>>20)
	}
	srv := mcp.New("idp-sandbox", version,
		"Runs code in an isolated sandbox: create a workspace, write files, run Python or shell, read the results. There is no network and nothing persists beyond the workspace.",
		tools.NewWithFiles(mgr, log, met, fc))

	srv.Log = log
	if f := os.Getenv("IDP_SANDBOX_CALLER_KEYS_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err == nil {
			srv.Callers, err = mcp.NewCallerVerifier(b, os.Getenv("IDP_SANDBOX_CALLER_AUDIENCE"))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "IDP_SANDBOX_CALLER_KEYS_FILE / IDP_SANDBOX_CALLER_AUDIENCE:", err)
			os.Exit(2)
		}
		log.Info("callers must be signed by the gateway", "audience", os.Getenv("IDP_SANDBOX_CALLER_AUDIENCE"), "keys", srv.Callers.KeyIDs())
	} else if os.Getenv("IDP_SANDBOX_CALLER_AUDIENCE") != "" {
		fmt.Fprintln(os.Stderr, "IDP_SANDBOX_CALLER_AUDIENCE is set without IDP_SANDBOX_CALLER_KEYS_FILE")
		os.Exit(2)
	} else if cfg.RequireOwner {
		log.Warn("the caller the gateway forwards is not verified: anything that reaches this port can name any caller (set IDP_SANDBOX_CALLER_KEYS_FILE and IDP_SANDBOX_CALLER_AUDIENCE)")
	}

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
	_ = be.Sweep(shutdown)
}
