package kube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"idp-sandbox/internal/sandbox"
)

// "No network" on this backend is a NetworkPolicy, and a NetworkPolicy is only as good as the cluster's CNI: where the
// CNI does not enforce policies, or the policy was not applied, every run can reach the cluster and the internet, and
// nothing says so (ADR-0001 D20). The probe finds out: a pod labelled exactly like a run tries to open a TCP connection
// to an address every pod can otherwise reach, the API server.

// ErrNoProbeTool: the helper image has no nc, so the probe could not try.
var ErrNoProbeTool = errors.New("the helper image has no nc: the network probe could not run")

// APIServerAddr is the address this backend talks to the API server on; ok is false when that is a loopback address,
// which means something else from inside a pod (an out-of-cluster development run).
func (k *Kube) APIServerAddr() (host string, port int, ok bool) {
	u, err := url.Parse(k.c.base)
	if err != nil {
		return "", 0, false
	}
	host = u.Hostname()
	port, _ = strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
		if u.Scheme == "http" {
			port = 80
		}
	}
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return host, port, false
	}
	return host, port, host != ""
}

// probeScript answers, for each host and port it is given, REACHED or BLOCKED on a line of its own.
const probeScript = `command -v nc >/dev/null 2>&1 || { echo NO-TOOL; exit 0; }
while [ $# -ge 2 ]; do
if nc -w 3 "$1" "$2" </dev/null >/dev/null 2>&1; then echo REACHED; else echo BLOCKED; fi
shift 2; done`

type probeTarget struct {
	host string
	port int
}

// probe runs a pod labelled like a run (with egress: like a run that may use the egress proxy) that tries to open a TCP
// connection to each target, and reports which it reached.
func (k *Kube) probe(ctx context.Context, egress bool, targets ...probeTarget) ([]bool, error) {
	cmd := []string{"sh", "-c", probeScript, "sh"}
	for _, t := range targets {
		if net.ParseIP(t.host) == nil && !dnsName.MatchString(t.host) || t.port < 1 || t.port > 65535 {
			return nil, fmt.Errorf("%w: invalid probe target", sandbox.ErrInvalid)
		}
		cmd = append(cmd, t.host, strconv.Itoa(t.port))
	}
	res, err := k.execute(ctx, jobSpec{role: "run", noWorkspace: true, egress: egress, image: k.cfg.HelperImage, userCmd: cmd,
		limits: sandbox.Limits{MemoryBytes: 32 << 20, CPUs: 0.2, Pids: 16, OutputBytes: 1024, TmpBytes: 1 << 20}}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(res.Stdout)
	if len(lines) == 1 && lines[0] == "NO-TOOL" {
		return nil, ErrNoProbeTool
	}
	if len(lines) != len(targets) {
		return nil, fmt.Errorf("the network probe ended without an answer (exit %d, timed out %v)", res.ExitCode, res.TimedOut)
	}
	reached := make([]bool, len(lines))
	for i, l := range lines {
		switch l {
		case "REACHED":
			reached[i] = true
		case "BLOCKED":
		default:
			return nil, fmt.Errorf("the network probe ended without an answer (exit %d, timed out %v)", res.ExitCode, res.TimedOut)
		}
	}
	return reached, nil
}

// NetworkProbe reports whether a pod labelled like a run could open a TCP connection to host:port. true means the
// runs of this namespace are not cut off from the network.
func (k *Kube) NetworkProbe(ctx context.Context, host string, port int) (reached bool, err error) {
	r, err := k.probe(ctx, false, probeTarget{host, port})
	if err != nil {
		return false, err
	}
	return r[0], nil
}

// EgressProbe is the same question for a run that may use the egress proxy (D22): a pod labelled like one must reach the
// proxy and nothing else. reached: it opened a connection to host:port, so the policy that lets such runs out lets
// them out too far. proxy: nil when it reached the proxy, else why not (the proxy is not up, its Service cannot be
// found, or no policy lets these pods in): runs that ask for the network would fail, and nothing else would.
func (k *Kube) EgressProbe(ctx context.Context, host string, port int) (reached bool, proxy error, err error) {
	addr, perr := k.proxyAddr(ctx)
	targets := []probeTarget{{host, port}}
	if perr == nil {
		targets = append(targets, probeTarget{addr.String(), k.cfg.EgressPort})
	}
	r, err := k.probe(ctx, true, targets...)
	if err != nil {
		return false, nil, err
	}
	if perr == nil && !r[1] {
		perr = fmt.Errorf("no connection to %s", net.JoinHostPort(addr.String(), strconv.Itoa(k.cfg.EgressPort)))
	}
	return r[0], perr, nil
}
