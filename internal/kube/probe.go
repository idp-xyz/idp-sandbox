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

// NetworkProbe reports whether a pod labelled like a run could open a TCP connection to host:port. true means the
// runs of this namespace are not cut off from the network.
func (k *Kube) NetworkProbe(ctx context.Context, host string, port int) (reached bool, err error) {
	if net.ParseIP(host) == nil && !dnsName.MatchString(host) || port < 1 || port > 65535 {
		return false, fmt.Errorf("%w: invalid probe target", sandbox.ErrInvalid)
	}
	const script = `command -v nc >/dev/null 2>&1 || { echo NO-TOOL; exit 0; }
if nc -w 3 "$1" "$2" </dev/null >/dev/null 2>&1; then echo REACHED; else echo BLOCKED; fi`
	res, err := k.execute(ctx, jobSpec{role: "run", noWorkspace: true, image: k.cfg.HelperImage,
		userCmd: []string{"sh", "-c", script, "sh", host, strconv.Itoa(port)},
		limits:  sandbox.Limits{MemoryBytes: 32 << 20, CPUs: 0.2, Pids: 16, OutputBytes: 1024, TmpBytes: 1 << 20}}, 20*time.Second)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(res.Stdout) {
	case "REACHED":
		return true, nil
	case "BLOCKED":
		return false, nil
	case "NO-TOOL":
		return false, ErrNoProbeTool
	}
	return false, fmt.Errorf("the network probe ended without an answer (exit %d, timed out %v)", res.ExitCode, res.TimedOut)
}
