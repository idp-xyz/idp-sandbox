package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-sandbox/internal/engine"
	"idp-sandbox/internal/sandbox"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// egressSetup starts, on a user-defined network, an origin web server ("origin", allow-listed), another one
// ("other", not allow-listed) and the egress proxy, and returns a manager whose runs may ask for egress.
func egressSetup(t *testing.T) *sandbox.Manager {
	t.Helper()
	py := env("IDP_SANDBOX_PYTHON_IMAGE", "public.ecr.aws/docker/library/python:3.12-slim")
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no docker")
	}
	build := exec.Command("bash", "../../deploy/build-egress.sh", "idp-sandbox-egress:test")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the proxy: %v\n%s", err, out)
	}
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	net := "idp-sbx-test-" + suffix
	docker(t, "network", "create", net)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", net).Run() })
	web := func(name, body string) {
		docker(t, "run", "-d", "--rm", "--name", name+"-"+suffix, "--network", net, "--network-alias", name, "--label", "idp.sandbox=test-egress",
			py, "python", "-c", "import http.server,socketserver\nclass H(http.server.BaseHTTPRequestHandler):\n  def do_GET(s):\n    s.send_response(200); s.end_headers(); s.wfile.write(b'"+body+"')\nsocketserver.TCPServer(('0.0.0.0',8000),H).serve_forever()")
	}
	web("origin", "hello from the allowed origin")
	web("other", "you should not see this")
	proxy := "proxy-" + suffix
	docker(t, "run", "-d", "--rm", "--name", proxy, "--network", net, "--label", "idp.sandbox=test-egress",
		"-e", "IDP_EGRESS_HOSTS=origin", "-e", "IDP_EGRESS_PORTS=8000", "-e", "IDP_EGRESS_ALLOW_PRIVATE=true", "idp-sandbox-egress:test")
	t.Cleanup(func() {
		for _, n := range []string{"origin-" + suffix, "other-" + suffix, proxy} {
			_ = exec.Command("docker", "rm", "-f", n).Run()
		}
	})
	time.Sleep(1500 * time.Millisecond)

	shell := env("IDP_SANDBOX_SHELL_IMAGE", "public.ecr.aws/docker/library/alpine:3.22")
	eng, err := engine.New(env("IDP_SANDBOX_DOCKER_HOST", "unix:///var/run/docker.sock"), shell)
	if err != nil {
		t.Skip(err)
	}
	eng.Owner = "test-" + suffix
	eng.Runtime = os.Getenv("IDP_SANDBOX_RUNTIME")
	eng.EgressProxy, eng.EgressPort = proxy, 3128
	cfg := sandbox.DefaultConfig(py, shell)
	cfg.Limits.Timeout = 15 * time.Second
	cfg.Limits.WorkspaceBytes = 8 << 20
	cfg.Namespace = "idp-sbx-egress-"
	cfg.EgressEnabled = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, err := sandbox.New(ctx, cfg, eng, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Sweep(context.Background()) })
	// the proxy must also be reachable from the per-run networks: it is attached to each, and removed after
	return m
}

func runEgress(t *testing.T, m *sandbox.Manager, id, code string) sandbox.RunResult {
	t.Helper()
	r, err := m.Run(context.Background(), owner, id, "python", code, 0, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return r
}

func TestEgressReachesOnlyWhatTheOperatorAllowed(t *testing.T) {
	m := egressSetup(t)
	id := ws(t, m)
	r := runEgress(t, m, id, `
import urllib.request, urllib.error, os
print("proxy env", os.environ.get("HTTP_PROXY", "")[:7])
def get(url):
    try:
        return urllib.request.urlopen(url, timeout=8).read().decode()
    except urllib.error.HTTPError as e:
        return "HTTP %d" % e.code
    except Exception as e:
        return "ERR %s" % type(e).__name__
print("allowed:", get("http://origin:8000/"))
print("other:", get("http://other:8000/"))
print("outside:", get("http://example.com:8000/"))
`)
	for _, want := range []string{"proxy env http://", "allowed: hello from the allowed origin", "other: HTTP 403", "outside: HTTP 403"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("missing %q in\n%s\n%s", want, r.Stdout, r.Stderr)
		}
	}
	if strings.Contains(r.Stdout, "you should not see this") {
		t.Fatal("reached a host that is not on the allow-list")
	}
}

func TestEgressRunsCannotBypassTheProxy(t *testing.T) {
	m := egressSetup(t)
	id := ws(t, m)
	r := runEgress(t, m, id, `
import socket, os
def tcp(host, port):
    s = socket.socket(); s.settimeout(3)
    try:
        s.connect((host, port)); return "CONNECTED"
    except OSError as e:
        return "blocked"
ip = lambda h: socket.gethostbyname(h) if True else None
try:
    origin_ip = socket.gethostbyname("origin")
except OSError:
    origin_ip = "none"
print("origin by name:", origin_ip)           # the origin is not on this network: no DNS entry
print("direct origin ip:", tcp("172.17.0.2", 8000), tcp("172.18.0.2", 8000))
print("internet:", tcp("1.1.1.1", 443), tcp("8.8.8.8", 53))
print("docker socket path:", os.path.exists("/var/run/docker.sock"))
nets = [l.split(":")[0].strip() for l in open("/proc/net/dev").readlines()[2:]]
print("interfaces:", sorted(nets))
`)
	if strings.Contains(r.Stdout, "CONNECTED") || strings.Contains(r.Stdout, "docker socket path: True") {
		t.Fatalf("a run bypassed the proxy:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "origin by name: none") {
		t.Errorf("the origin resolved from the run's own network:\n%s", r.Stdout)
	}
}

func TestEgressRunsDoNotSeeEachOther(t *testing.T) {
	m := egressSetup(t)
	a, b := ws(t, m), ws(t, m)
	var wg sync.WaitGroup
	var ra, rb sandbox.RunResult
	wg.Add(2)
	go func() {
		defer wg.Done()
		ra = runEgress(t, m, a, `
import socket, time
s = socket.gethostbyname(socket.gethostname())
srv = socket.socket(); srv.bind(("0.0.0.0", 9000)); srv.listen(5)
open("ip.txt", "w").write(s)
print("A listens on", s)
time.sleep(8)
`)
	}()
	time.Sleep(3 * time.Second)
	go func() {
		defer wg.Done()
		rb = runEgress(t, m, b, `
import socket, re
mine = socket.gethostbyname(socket.gethostname())
subnet = ".".join(mine.split(".")[:3])
found = []
for i in range(1, 12):
    s = socket.socket(); s.settimeout(0.3)
    try:
        s.connect((subnet + "." + str(i), 9000)); found.append(i)
    except OSError:
        pass
print("B is", mine, "and reached", found)
`)
	}()
	wg.Wait()
	if !strings.Contains(ra.Stdout, "A listens on") || !strings.Contains(rb.Stdout, "reached []") {
		t.Fatalf("A: %q %q\nB: %q %q", ra.Stdout, ra.Stderr, rb.Stdout, rb.Stderr)
	}
	// and no per-run network is left behind
	out := docker(t, "network", "ls", "--filter", "name=idp-sbx-net-", "-q")
	if out != "" {
		t.Errorf("per-run networks were left behind: %s", out)
	}
}

func TestEgressIsRefusedWhenNotConfigured(t *testing.T) {
	m := setup(t, nil) // no proxy configured
	id := ws(t, m)
	if _, err := m.Run(context.Background(), owner, id, "python", "print(1)", 0, true); err != sandbox.ErrNoEgress {
		t.Fatalf("%v", err)
	}
	r := run(t, m, id, "python", "import os; print(os.environ.get('HTTP_PROXY'))")
	if strings.TrimSpace(r.Stdout) != "None" {
		t.Fatalf("a run without egress has a proxy: %q", r.Stdout)
	}
	_ = os.Getenv
}
