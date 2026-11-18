// Package kube_test moves files through the Kubernetes backend on a real cluster (ADR-0001 D19): bytes of any kind, up
// to the transfer limit, in through ConfigMaps and back through pod logs, as the service account of deploy/kubernetes
// with its Role and the quota of the runs namespace. Run through tests/kind/transfer.sh.
package kube_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-sandbox/internal/kube"
	"idp-sandbox/internal/sandbox"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestBinaryFilesOnARealCluster(t *testing.T) {
	api := os.Getenv("IDP_SANDBOX_KUBE_API")
	if api == "" {
		t.Skip("IDP_SANDBOX_KUBE_API not set (bash tests/kind/transfer.sh)")
	}
	const limit = 25 << 20 // the service's default IDP_SANDBOX_TRANSFER_MB
	k, err := kube.New(kube.Config{APIServer: api, TokenFile: os.Getenv("IDP_SANDBOX_KUBE_TOKEN_FILE"), CAFile: os.Getenv("IDP_SANDBOX_KUBE_CA_FILE"),
		Namespace: env("IDP_SANDBOX_NAMESPACE", "idp-sandbox-runs"), Owner: env("IDP_SANDBOX_OWNER", "transfer-test"),
		HelperImage: env("IDP_SANDBOX_SHELL_IMAGE", "alpine:3.22"), RuntimeClass: os.Getenv("IDP_SANDBOX_RUNTIME_CLASS"),
		StorageClass: os.Getenv("IDP_SANDBOX_STORAGE_CLASS"), WorkspaceBytes: 64 << 20, MaxWriteBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := k.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var id [6]byte
	_, _ = rand.Read(id[:])
	ws := "idp-sbx-xfer-" + hex.EncodeToString(id[:])
	if err := k.CreateWorkspace(ctx, ws, 64<<20); err != nil {
		t.Fatal(err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if err := k.DeleteWorkspace(dctx, ws); err != nil {
			t.Errorf("deleting the workspace: %v", err)
		}
	}()
	random := func(n int) []byte {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		if n >= 6 {
			copy(b, []byte{0, 0xff, '\r', '\n', 0xc3, 0x28}) // NUL, a non-UTF-8 byte, CRLF, an invalid sequence
		}
		return b
	}
	// in and out, at the sizes where the path changes: one ConfigMap, two, several log pieces, the limit
	for _, c := range []struct {
		name string
		size int
	}{{"empty", 0}, {"six bytes", 6}, {"one ConfigMap", 1000 * 1000}, {"two ConfigMaps", 1000*1000 + 1}, {"over one log piece", 3<<20 + 1}, {"the limit", limit}} {
		data := random(c.size)
		p := fmt.Sprintf("in/%d.bin", c.size)
		start := time.Now()
		if err := k.WriteFile(ctx, ws, p, data); err != nil {
			t.Fatalf("%s: write: %v", c.name, err)
		}
		wrote := time.Since(start)
		start = time.Now()
		got, err := k.ReadBytes(ctx, ws, p, limit)
		if err != nil {
			t.Fatalf("%s: read: %v", c.name, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("%s: %d bytes came back different from the %d written", c.name, len(got), len(data))
		}
		t.Logf("%s (%d bytes): in %.1fs, out %.1fs", c.name, c.size, wrote.Seconds(), time.Since(start).Seconds())
	}
	if err := k.WriteFile(ctx, ws, "in/over.bin", make([]byte, limit+1)); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("a file over the limit: %v", err)
	}
	// a file a run made is read with the checksum the run itself reports
	lim := sandbox.Limits{MemoryBytes: 128 << 20, CPUs: 1, Pids: 32, Timeout: 60 * time.Second, OutputBytes: 64 << 10, WorkspaceBytes: 64 << 20, TmpBytes: 4 << 20}
	run := func(script string) sandbox.RunResult {
		t.Helper()
		res, err := k.Run(ctx, sandbox.RunSpec{Workspace: ws, Image: env("IDP_SANDBOX_SHELL_IMAGE", "alpine:3.22"), Cmd: []string{"sh", "-c", script}, Limits: lim, Timeout: 60 * time.Second})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("run %q: %+v %v", script, res, err)
		}
		return res
	}
	// under a hardened runtime the code sees that runtime's kernel, not the node's (gVisor reports 4.4.0)
	if rc := os.Getenv("IDP_SANDBOX_RUNTIME_CLASS"); rc == "gvisor" {
		if kernel := strings.TrimSpace(run("uname -r").Stdout); !strings.HasPrefix(kernel, "4.4.0") {
			t.Fatalf("RuntimeClass gvisor, but the run saw kernel %s", kernel)
		}
	} else if rc != "" {
		t.Logf("RuntimeClass %s: the run saw kernel %s", rc, strings.TrimSpace(run("uname -r").Stdout))
	}
	res := run(`mkdir -p out && dd if=/dev/urandom of=out/made.bin bs=1048576 count=5 2>/dev/null && sha256sum < out/made.bin | cut -d' ' -f1 && ln -s /etc/passwd leak && ln -s out/made.bin inside`)
	made, err := k.ReadBytes(ctx, ws, "out/made.bin", limit)
	if sum := sha256.Sum256(made); err != nil || len(made) != 5<<20 || hex.EncodeToString(sum[:]) != strings.TrimSpace(res.Stdout) {
		t.Fatalf("a file a run made: %d bytes, %v", len(made), err)
	}
	if _, err := k.ReadBytes(ctx, ws, "out/made.bin", 1<<20); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("a file over the reader's limit: %v", err)
	}
	// links are refused, whether they leave the workspace or not; so is what is not there
	for _, p := range []string{"leak", "inside", "missing.bin", "out"} {
		if _, err := k.ReadBytes(ctx, ws, p, limit); !errors.Is(err, sandbox.ErrNoFile) {
			t.Errorf("%s: %v", p, err)
		}
	}
	// two large files at once: the pieces of one do not use up the namespace's ConfigMap quota for the other
	var wg sync.WaitGroup
	errs := make([]error, 2)
	both := [][]byte{random(12 << 20), random(12 << 20)}
	for i := range both {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = k.WriteFile(ctx, ws, fmt.Sprintf("in/both-%d.bin", i), both[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("large file %d of two at once: %v", i, err)
		}
		if got, err := k.ReadBytes(ctx, ws, fmt.Sprintf("in/both-%d.bin", i), limit); err != nil || !bytes.Equal(got, both[i]) {
			t.Fatalf("large file %d of two at once came back different: %v", i, err)
		}
	}
}

// D20: what the service asks of the cluster before it serves. IDP_SANDBOX_PROBE_TARGET is an address every pod can
// reach unless a policy stops it (the kubernetes Service's cluster IP and port); IDP_SANDBOX_PROBE_EXPECT says what
// this cluster should answer: reached (no policy, or a CNI that does not enforce it) or blocked.
func TestNetworkProbeOnARealCluster(t *testing.T) {
	api, target, expect := os.Getenv("IDP_SANDBOX_KUBE_API"), os.Getenv("IDP_SANDBOX_PROBE_TARGET"), os.Getenv("IDP_SANDBOX_PROBE_EXPECT")
	if api == "" || target == "" || expect == "" {
		t.Skip("IDP_SANDBOX_KUBE_API, IDP_SANDBOX_PROBE_TARGET and IDP_SANDBOX_PROBE_EXPECT not set")
	}
	k, err := kube.New(kube.Config{APIServer: api, TokenFile: os.Getenv("IDP_SANDBOX_KUBE_TOKEN_FILE"), CAFile: os.Getenv("IDP_SANDBOX_KUBE_CA_FILE"),
		Namespace: env("IDP_SANDBOX_NAMESPACE", "idp-sandbox-runs"), Owner: env("IDP_SANDBOX_OWNER", "transfer-test"),
		HelperImage: env("IDP_SANDBOX_SHELL_IMAGE", "alpine:3.22"), RuntimeClass: os.Getenv("IDP_SANDBOX_RUNTIME_CLASS")})
	if err != nil {
		t.Fatal(err)
	}
	host, port, ok := strings.Cut(target, ":")
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil || !ok {
		t.Fatalf("IDP_SANDBOX_PROBE_TARGET %q is not host:port", target)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	reached, err := k.NetworkProbe(ctx, host, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a pod labelled like a run reached %s: %v (%.1fs)", target, reached, time.Since(start).Seconds())
	if reached != (expect == "reached") {
		t.Fatalf("expected %s", expect)
	}
}
