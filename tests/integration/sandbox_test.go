// Integration tests against a real Docker daemon: the isolation claims of the sandbox, tried as an
// attacker would. Skipped when no daemon answers (IDP_SANDBOX_DOCKER_HOST, default the local socket);
// the images are python:3.12-slim and alpine:3.22 unless IDP_SANDBOX_PYTHON_IMAGE / _SHELL_IMAGE say otherwise.
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"idp-sandbox/internal/engine"
	"idp-sandbox/internal/sandbox"
)

const owner = "acme/alice"

// gvisor: the runs execute under runsc (IDP_SANDBOX_RUNTIME=runsc). nested: that daemon runs inside a container, where
// runsc cannot apply cgroup limits (it needs --ignore-cgroups), so the memory and process limits cannot hold and the
// wall-clock timeout is what contains a bomb (IDP_SANDBOX_NESTED=1 says so; the test then insists on containment, not on the limit).
var (
	gvisor = os.Getenv("IDP_SANDBOX_RUNTIME") == "runsc"
	nested = os.Getenv("IDP_SANDBOX_NESTED") == "1"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func setup(t *testing.T, tweak func(*sandbox.Config)) *sandbox.Manager {
	t.Helper()
	shell := env("IDP_SANDBOX_SHELL_IMAGE", "public.ecr.aws/docker/library/alpine:3.22")
	py := env("IDP_SANDBOX_PYTHON_IMAGE", "public.ecr.aws/docker/library/python:3.12-slim")
	eng, err := engine.New(env("IDP_SANDBOX_DOCKER_HOST", "unix:///var/run/docker.sock"), shell)
	if err != nil {
		t.Skip(err)
	}
	eng.Owner = "test-" + t.Name()
	eng.Runtime = os.Getenv("IDP_SANDBOX_RUNTIME") // runsc: every run and file operation under gVisor
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := eng.Ping(ctx); err != nil {
		t.Skip("no container engine:", err)
	}
	cfg := sandbox.DefaultConfig(py, shell)
	cfg.Limits.Timeout = 8 * time.Second
	cfg.Limits.WorkspaceBytes = 8 << 20
	cfg.Limits.OutputBytes = 4 << 10
	cfg.Limits.MemoryBytes = 128 << 20
	cfg.Namespace = "idp-sbx-test-"
	if tweak != nil {
		tweak(&cfg)
	}
	m, err := sandbox.New(ctx, cfg, eng, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Sweep(context.Background()) })
	return m
}

func ws(t *testing.T, m *sandbox.Manager) string {
	t.Helper()
	id, _, err := m.Create(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func run(t *testing.T, m *sandbox.Manager, id, lang, code string) sandbox.RunResult {
	t.Helper()
	res, err := m.Run(context.Background(), owner, id, lang, code, 0, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestRunsCodeAndKeepsFilesBetweenRuns(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	r := run(t, m, id, "python", "open('note.txt','w').write('kept'); print('hello', 6*7)")
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "hello 42" {
		t.Fatalf("%+v", r)
	}
	r = run(t, m, id, "shell", "cat note.txt; echo; echo oops >&2; exit 3")
	if r.ExitCode != 3 || !strings.Contains(r.Stdout, "kept") || !strings.Contains(r.Stderr, "oops") {
		t.Fatalf("%+v", r)
	}
	b, err := m.ReadFile(context.Background(), owner, id, "note.txt")
	if err != nil || string(b) != "kept" {
		t.Fatalf("read: %q %v", b, err)
	}
	files, err := m.ListFiles(context.Background(), owner, id)
	if err != nil || len(files) != 1 || files[0].Path != "note.txt" {
		t.Fatalf("list: %v %v", files, err)
	}
	if err := m.WriteFile(context.Background(), owner, id, "sub/dir/in.txt", []byte("from the agent")); err != nil {
		t.Fatal(err)
	}
	r = run(t, m, id, "python", "print(open('sub/dir/in.txt').read())")
	if !strings.Contains(r.Stdout, "from the agent") {
		t.Fatalf("%+v", r)
	}
}

func TestRunsUnprivilegedWithoutCapabilitiesOrWritableRoot(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	r := run(t, m, id, "shell", `id -u; grep -E '^(CapEff|CapBnd|NoNewPrivs|Seccomp):' /proc/self/status; (echo x > /etc/pwned) 2>&1; ls /var/run/docker.sock 2>&1; env | grep -ci -E 'secret|token|password|key' `)
	if !strings.HasPrefix(r.Stdout, "65534\n") {
		t.Fatalf("not the unprivileged user: %q", r.Stdout)
	}
	wants := []string{"CapEff:\t0000000000000000", "NoNewPrivs:\t1", "Seccomp:\t2", "Read-only file system", "No such file"}
	if gvisor { // gVisor's kernel reports no NoNewPrivs/seccomp of its own; the write fails with EACCES for uid 65534
		wants = []string{"CapEff:\t0000000000000000", "Permission denied", "No such file"}
	}
	for _, want := range wants {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("missing %q in\n%s", want, r.Stdout)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(r.Stdout), "0") {
		t.Errorf("the environment carries something secret-looking:\n%s", r.Stdout)
	}
}

func TestNoNetwork(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	r := run(t, m, id, "python", `
import socket
for target in [("1.1.1.1", 53), ("172.17.0.1", 2375), ("127.0.0.1", 8570)]:
    s = socket.socket(); s.settimeout(2)
    try:
        s.connect(target); print("CONNECTED", target)
    except OSError as e:
        print("blocked", target, e.errno)
try:
    print("RESOLVED", socket.gethostbyname("example.com"))
except OSError:
    print("no dns")
# the kernel's fallback tunnel devices (tunl0, sit0, ...) appear in every namespace once their module is loaded, and are down
print([n for _, n in socket.if_nameindex() if n not in ('tunl0', 'sit0', 'ip6tnl0', 'gre0', 'gretap0', 'erspan0', 'ip_vti0', 'ip6_vti0', 'ip6gre0')])
`)
	if strings.Contains(r.Stdout, "CONNECTED") || strings.Contains(r.Stdout, "RESOLVED") {
		t.Fatalf("the sandbox reached the network:\n%s", r.Stdout)
	}
	// loopback only; a gVisor sandbox without a network has no interface at all, which is stricter
	if !strings.Contains(r.Stdout, "['lo']") && !(gvisor && strings.Contains(r.Stdout, "[]")) {
		t.Fatalf("interfaces other than loopback:\n%s", r.Stdout)
	}
}

func TestForkBombIsContained(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	r := run(t, m, id, "python", `
import os, sys
n = 0
try:
    while n < 2000:
        if os.fork() == 0:
            import time; time.sleep(30); os._exit(0)
        n += 1
except OSError as e:
    print("stopped at", n, e.errno)
    sys.stdout.flush()
    os._exit(0)
print("NOT STOPPED", n)
`)
	if nested && gvisor {
		if !r.TimedOut {
			t.Fatalf("not contained: %+v", r)
		}
		t.Log("nested gVisor without cgroups: the process limit cannot hold; the timeout contained the bomb")
	} else if gvisor {
		// gVisor counts the sandbox's own host threads against the process limit, not the guest's processes: a
		// guest fork bomb exhausts it and the whole sandbox is killed (and reported as out of memory) within a second
		if r.ExitCode != 137 || r.TimedOut || r.Duration > 5*time.Second {
			t.Fatalf("the sandbox was not killed by the process limit: %+v", r)
		}
	} else if strings.Contains(r.Stdout, "NOT STOPPED") || !strings.Contains(r.Stdout, "stopped at") {
		t.Fatalf("%+v", r)
	}
	// and the host is intact: the next run works
	if r := run(t, m, id, "shell", "echo alive"); !strings.Contains(r.Stdout, "alive") {
		t.Fatalf("%+v", r)
	}
}

func TestMemoryBombIsKilled(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	r := run(t, m, id, "python", "x = []\nwhile True: x.append(bytearray(8 << 20))")
	if nested && gvisor {
		if !r.TimedOut && !r.OOMKilled {
			t.Fatalf("not contained: %+v", r)
		}
		t.Log("nested gVisor: the memory limit cannot hold; the timeout contained the bomb")
	} else if !r.OOMKilled || r.ExitCode == 0 {
		t.Fatalf("%+v", r)
	}
}

func TestInfiniteLoopTimesOut(t *testing.T) {
	m := setup(t, func(c *sandbox.Config) { c.Limits.Timeout = 3 * time.Second })
	id := ws(t, m)
	start := time.Now()
	r := run(t, m, id, "python", "while True: pass")
	if !r.TimedOut || time.Since(start) > 15*time.Second {
		t.Fatalf("%+v after %s", r, time.Since(start))
	}
	// a caller can only shorten the timeout
	res, err := m.Run(context.Background(), owner, id, "python", "import time; time.sleep(60)", time.Hour, false)
	if err != nil || !res.TimedOut {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestOutputFloodIsCappedAndStopped(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	start := time.Now()
	r := run(t, m, id, "python", "import sys\nwhile True: sys.stdout.write('A' * 65536)")
	if !r.Truncated || len(r.Stdout) > 4<<10 || time.Since(start) > 15*time.Second {
		t.Fatalf("truncated=%v len=%d after %s", r.Truncated, len(r.Stdout), time.Since(start))
	}
}

func TestDiskFillStopsAtTheQuota(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	r := run(t, m, id, "python", `
import errno
chunk = b"x" * (1 << 20)
total = 0
try:
    with open("fill.bin", "wb") as f:
        while True:
            f.write(chunk); f.flush(); total += 1
except OSError as e:
    print("full after", total, "MiB", errno.errorcode.get(e.errno))
`)
	if !strings.Contains(r.Stdout, "full after") || !strings.Contains(r.Stdout, "ENOSPC") && !strings.Contains(r.Stdout, "EFBIG") {
		t.Fatalf("%+v", r)
	}
}

func TestWorkspacesAreIsolatedFromEachOther(t *testing.T) {
	m := setup(t, nil)
	a, b := ws(t, m), ws(t, m)
	run(t, m, a, "shell", "echo secret-of-a > a.txt")
	r := run(t, m, b, "shell", "ls -A; cat /work/a.txt 2>&1; cat ../work/../a.txt 2>&1")
	if strings.Contains(r.Stdout, "secret-of-a") {
		t.Fatalf("workspace b saw workspace a:\n%s", r.Stdout)
	}
	if _, err := m.ReadFile(context.Background(), owner, b, "a.txt"); err == nil {
		t.Fatal("read across workspaces")
	}
	if _, err := m.ReadFile(context.Background(), owner, "00000000000000000000000000000000", "a.txt"); err == nil {
		t.Fatal("a guessed id worked")
	}
}

func TestSymlinkInWorkspaceIsNotFollowed(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	run(t, m, id, "shell", "ln -s /etc/passwd link; ln -s ../../../../etc/shadow link2; mkdir d; ln -s /work d/up")
	for _, p := range []string{"link", "link2", "d/up"} {
		if b, err := m.ReadFile(context.Background(), owner, id, p); err == nil {
			t.Errorf("read %s through a symlink: %q", p, b)
		}
	}
}

func TestPathValidation(t *testing.T) {
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", ".idp/main.py", ".", "a\\b", "a\x00b", strings.Repeat("a/", 12) + "f"} {
		if _, err := sandbox.CleanPath(p); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	for p, want := range map[string]string{"a.txt": "a.txt", "d/./e.txt": "d/e.txt", "d//e": "d/e"} {
		if got, err := sandbox.CleanPath(p); err != nil || got != want {
			t.Errorf("%q: %q %v", p, got, err)
		}
	}
}

func TestOneRunAtATimeAndCapacity(t *testing.T) {
	m := setup(t, func(c *sandbox.Config) { c.MaxWorkspaces = 2 })
	id := ws(t, m)
	done := make(chan struct{})
	go func() { _, _ = m.Run(context.Background(), owner, id, "shell", "sleep 3", 0, false); close(done) }()
	time.Sleep(1500 * time.Millisecond)
	if _, err := m.Run(context.Background(), owner, id, "shell", "echo x", 0, false); err != sandbox.ErrBusy {
		t.Errorf("a second run in a busy workspace: %v", err)
	}
	<-done
	ws(t, m)
	if _, _, err := m.Create(context.Background(), owner); err != sandbox.ErrCapacity {
		t.Errorf("a third workspace: %v", err)
	}
}

func TestDeleteAndReap(t *testing.T) {
	m := setup(t, func(c *sandbox.Config) { c.IdleTTL = time.Nanosecond })
	id := ws(t, m)
	run(t, m, id, "shell", "echo x > f")
	if err := m.Delete(context.Background(), owner, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadFile(context.Background(), owner, id, "f"); err != sandbox.ErrNotFound {
		t.Errorf("after delete: %v", err)
	}
	ws(t, m)
	time.Sleep(10 * time.Millisecond)
	if n := m.Reap(context.Background()); n != 1 || m.Count() != 0 {
		t.Errorf("reaped %d, %d left", n, m.Count())
	}
}

func TestInvalidRequestsAreRefused(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	if _, err := m.Run(context.Background(), owner, id, "ruby", "puts 1", 0, false); err == nil {
		t.Error("an unlisted language ran")
	}
	if _, err := m.Run(context.Background(), owner, id, "python", "", 0, false); err == nil {
		t.Error("empty code ran")
	}
	if _, err := m.Run(context.Background(), owner, id, "python", strings.Repeat("#", 300<<10), 0, false); err == nil {
		t.Error("oversized code ran")
	}
	if err := m.WriteFile(context.Background(), owner, id, "big", make([]byte, 2<<20)); err != sandbox.ErrFileTooLarge {
		t.Errorf("oversized file: %v", err)
	}
}

func TestAWorkspaceBelongsToItsCaller(t *testing.T) {
	m := setup(t, func(c *sandbox.Config) { c.MaxPerOwner = 2 })
	id := ws(t, m)
	run(t, m, id, "shell", "echo mine > f")
	bob := "acme/bob"
	ctx := context.Background()
	if _, err := m.Run(ctx, bob, id, "shell", "cat f", 0, false); err != sandbox.ErrNotFound {
		t.Errorf("another caller ran in the workspace: %v", err)
	}
	if _, err := m.ReadFile(ctx, bob, id, "f"); err != sandbox.ErrNotFound {
		t.Errorf("another caller read from it: %v", err)
	}
	if err := m.WriteFile(ctx, bob, id, "g", []byte("x")); err != sandbox.ErrNotFound {
		t.Errorf("another caller wrote to it: %v", err)
	}
	if _, err := m.ListFiles(ctx, bob, id); err != sandbox.ErrNotFound {
		t.Errorf("another caller listed it: %v", err)
	}
	if err := m.Delete(ctx, bob, id); err != sandbox.ErrNotFound {
		t.Errorf("another caller deleted it: %v", err)
	}
	// the same subject in another tenant is another owner
	if _, err := m.ReadFile(ctx, "globex/alice", id, "f"); err != sandbox.ErrNotFound {
		t.Errorf("the same subject of another tenant read it: %v", err)
	}
	// no identity is refused, and quotas are per caller
	if _, _, err := m.Create(ctx, ""); err != sandbox.ErrNoCaller {
		t.Errorf("anonymous create: %v", err)
	}
	ws(t, m)
	if _, _, err := m.Create(ctx, owner); err != sandbox.ErrOwnerQuota {
		t.Errorf("a third workspace of one caller: %v", err)
	}
	if _, _, err := m.Create(ctx, bob); err != nil {
		t.Errorf("bob is held back by alice's quota: %v", err)
	}
	if b, err := m.ReadFile(ctx, owner, id, "f"); err != nil || string(b) != "mine\n" {
		t.Errorf("the owner lost access: %q %v", b, err)
	}
}

func TestRunsUnderTheExpectedKernel(t *testing.T) {
	m := setup(t, nil)
	id := ws(t, m)
	r := run(t, m, id, "shell", "cat /proc/version")
	if gvisor != strings.Contains(r.Stdout, "Linux version 4.4.0") {
		t.Fatalf("gvisor=%v but the code sees %q", gvisor, r.Stdout)
	}
	if gvisor {
		if r2 := run(t, m, id, "shell", "dmesg | head -1"); !strings.Contains(r2.Stdout, "Starting gVisor") {
			t.Fatalf("no gVisor in dmesg: %q", r2.Stdout)
		}
	}
}
