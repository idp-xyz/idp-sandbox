package kube

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"idp-sandbox/internal/sandbox"
)

const ws = "idp-sbx-0123456789abcdef0123456789abcdef"

func limits() sandbox.Limits {
	return sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 1, Pids: 64, Timeout: 20 * time.Second, OutputBytes: 1024,
		WorkspaceBytes: 64 << 20, TmpBytes: 16 << 20}
}

func spec() sandbox.RunSpec {
	return sandbox.RunSpec{Workspace: ws, Image: "python:test", Cmd: []string{"python", "-I", "-B", "/work/main.py"}, Limits: limits(), Timeout: 5 * time.Second}
}

func fixed(oc outcome) func(obj, []string) outcome { return func(obj, []string) outcome { return oc } }

func setup(t *testing.T, oc outcome) (*Kube, *fakeAPI) {
	t.Helper()
	f := newFake(t, fixed(oc))
	k := f.newKube(t, nil)
	if err := k.CreateWorkspace(context.Background(), ws, 64<<20); err != nil {
		t.Fatal(err)
	}
	return k, f
}

func assertClean(t *testing.T, f *fakeAPI) {
	t.Helper()
	for _, r := range []string{"jobs", "pods", "configmaps"} {
		if n := f.count(r); n != 0 {
			t.Errorf("%d %s left behind", n, r)
		}
	}
}

func TestWorkspaceIsAClaim(t *testing.T) {
	f := newFake(t, fixed(outcome{}))
	k := f.newKube(t, func(c *Config) { c.StorageClass = "fast" })
	if err := k.CreateWorkspace(context.Background(), ws, 64<<20); err != nil {
		t.Fatal(err)
	}
	pvc := f.created("persistentvolumeclaims")[0]
	if at(pvc, "metadata", "name") != ws || at(pvc, "metadata", "labels", LabelKey) != "me" {
		t.Errorf("metadata %v", pvc["metadata"])
	}
	if strs(at(pvc, "spec", "accessModes"))[0] != "ReadWriteOnce" || at(pvc, "spec", "resources", "requests", "storage") != "67108864" ||
		at(pvc, "spec", "storageClassName") != "fast" {
		t.Errorf("spec %v", pvc["spec"])
	}
	if err := k.CreateWorkspace(context.Background(), "Bad_Name", 1); !errors.Is(err, sandbox.ErrInvalid) {
		t.Errorf("invalid name: %v", err)
	}
}

func TestRunSplitsStdoutAndStderr(t *testing.T) {
	k, f := setup(t, outcome{stdout: "hello\nno newline", stderr: "warn\n"})
	res, err := k.Run(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hello\nno newline" || res.Stderr != "warn\n" || res.ExitCode != 0 || res.Truncated || res.TimedOut || res.OOMKilled || res.Duration <= 0 {
		t.Errorf("%+v", res)
	}
	// the user's command sits behind the wrapper, untouched
	cmd := cmdOf(f.created("jobs")[0])
	if len(cmd) != 8 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[3] != "sh" || strings.Join(cmd[4:], " ") != "python -I -B /work/main.py" {
		t.Errorf("command %q", cmd)
	}
	if !strings.Contains(cmd[2], `"$@" 2>/tmp/idp-stderr`) || strings.Contains(cmd[2], "<") || !strings.Contains(cmd[2], "head -c 1025 ") {
		t.Errorf("script %q", cmd[2])
	}
	// always cleaned up: Job with background propagation, no pods, no ConfigMaps
	assertClean(t, f)
	if len(f.deletes) != 1 || !strings.Contains(f.deletes[0], "propagationPolicy=Background") {
		t.Errorf("deletes %v", f.deletes)
	}
	// the log is read once with a bound of two output buffers plus the separator
	if len(f.logLimit) != 1 {
		t.Errorf("log reads %v", f.logLimit)
	}
}

func TestMarkerIsPerRun(t *testing.T) {
	k, f := setup(t, outcome{})
	for i := 0; i < 2; i++ {
		if _, err := k.Run(context.Background(), spec()); err != nil {
			t.Fatal(err)
		}
	}
	re := regexp.MustCompile(`--IDP-STDERR-([0-9a-f]{16})--`)
	var seen []string
	for _, j := range f.created("jobs") {
		c := cmdOf(j)
		m := re.FindStringSubmatch(c[2])
		if m == nil {
			t.Fatalf("no marker in %q", c[2])
		}
		seen = append(seen, m[1])
	}
	if seen[0] == seen[1] {
		t.Error("marker reused")
	}
}

func TestNonZeroExit(t *testing.T) {
	k, f := setup(t, outcome{stdout: "partial", stderr: "Traceback\n", exit: 3})
	res, err := k.Run(context.Background(), spec())
	if err != nil || res.ExitCode != 3 || res.Stdout != "partial" || res.Stderr != "Traceback\n" || res.TimedOut {
		t.Fatalf("%+v %v", res, err)
	}
	assertClean(t, f)
}

func TestDeadlineExceededIsATimeout(t *testing.T) {
	k, f := setup(t, outcome{stdout: "so far", noMarker: true, exit: 137, deadline: true})
	res, err := k.Run(context.Background(), spec())
	if err != nil || !res.TimedOut || res.ExitCode != 137 || res.Stdout != "so far" || res.OOMKilled {
		t.Fatalf("%+v %v", res, err)
	}
	assertClean(t, f)
}

func TestRunPastItsTimeoutIsKilledAndReported(t *testing.T) {
	k, f := setup(t, outcome{stdout: "tick ", hang: true, noMarker: true})
	sp := spec()
	sp.Timeout = 400 * time.Millisecond
	res, err := k.Run(context.Background(), sp)
	if err != nil || !res.TimedOut || res.ExitCode != 137 || res.Stdout != "tick " {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Duration < 400*time.Millisecond || res.Duration > 4*time.Second {
		t.Errorf("duration %v", res.Duration)
	}
	assertClean(t, f)
}

func TestOOMKilled(t *testing.T) {
	k, f := setup(t, outcome{exit: 137, termReason: "OOMKilled", noMarker: true})
	res, err := k.Run(context.Background(), spec())
	if err != nil || !res.OOMKilled || res.ExitCode != 137 || res.TimedOut {
		t.Fatalf("%+v %v", res, err)
	}
	assertClean(t, f)
}

func TestOutputIsTruncated(t *testing.T) {
	lim := limits()
	lim.OutputBytes = 16
	cases := map[string]outcome{
		"stdout": {stdout: strings.Repeat("a", 100), stderr: "e"},
		"stderr": {stdout: "o", stderr: strings.Repeat("e", 100)},
		"both":   {stdout: strings.Repeat("a", 100), stderr: strings.Repeat("e", 100)},
	}
	for name, oc := range cases {
		t.Run(name, func(t *testing.T) {
			k, f := setup(t, oc)
			sp := spec()
			sp.Limits = lim
			res, err := k.Run(context.Background(), sp)
			if err != nil || !res.Truncated || len(res.Stdout) > 16 || len(res.Stderr) > 16 {
				t.Fatalf("%q %q %v %v", res.Stdout, res.Stderr, res.Truncated, err)
			}
			if name != "stderr" && res.Stdout != strings.Repeat("a", 16) {
				t.Errorf("stdout %q", res.Stdout)
			}
			if name == "stderr" && (res.Stdout != "o" || res.Stderr != strings.Repeat("e", 16)) {
				t.Errorf("%q %q", res.Stdout, res.Stderr)
			}
			assertClean(t, f)
		})
	}
	// output within the limit is not flagged, even exactly at it
	k, _ := setup(t, outcome{stdout: strings.Repeat("a", 16), stderr: strings.Repeat("e", 16)})
	sp := spec()
	sp.Limits = lim
	if res, err := k.Run(context.Background(), sp); err != nil || res.Truncated || len(res.Stdout) != 16 {
		t.Errorf("%+v %v", res, err)
	}
}

func TestMissingWorkspaceIsNotFound(t *testing.T) {
	f := newFake(t, fixed(outcome{}))
	k := f.newKube(t, nil)
	ctx := context.Background()
	sp := spec()
	if _, err := k.Run(ctx, sp); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("run: %v", err)
	}
	if err := k.WriteFile(ctx, ws, "a.txt", []byte("x")); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("write: %v", err)
	}
	if _, err := k.ReadFile(ctx, ws, "a.txt", 10); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("read: %v", err)
	}
	if _, err := k.ListFiles(ctx, ws, 10); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("list: %v", err)
	}
	if _, err := k.Run(ctx, sandbox.RunSpec{Workspace: "../x", Image: "i", Cmd: []string{"true"}, Limits: limits()}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("bad name: %v", err)
	}
	if f.called("POST") != 0 {
		t.Errorf("created objects for a missing workspace: %v", f.calls)
	}
}

func TestCleanupHappensWhenTheContextIsCancelled(t *testing.T) {
	k, f := setup(t, outcome{hang: true, noMarker: true})
	ctx, cancel := context.WithCancel(context.Background())
	sp := spec()
	sp.Stdin = []byte("input")
	sp.Timeout = time.Minute
	go func() {
		for f.count("pods") == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := k.Run(ctx, sp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if len(f.created("configmaps")) != 1 {
		t.Error("expected one configmap")
	}
	assertClean(t, f)
}

func TestCleanupHappensOnPullFailure(t *testing.T) {
	k, f := setup(t, outcome{pull: "ImagePullBackOff"})
	_, err := k.Run(context.Background(), spec())
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("%v", err)
	}
	assertClean(t, f)
}

func get(t *testing.T, o any, path ...string) any {
	t.Helper()
	v := at(o, path...)
	if v == nil {
		t.Fatalf("missing %v", path)
	}
	return v
}

// TestJobIsHardened asserts the security property: every isolation field of the Job, explicitly.
func TestJobIsHardened(t *testing.T) {
	_, f := setup(t, outcome{})
	k := f.newKube(t, func(c *Config) { c.RuntimeClass = "gvisor" })
	if _, err := k.Run(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	job := f.created("jobs")[0]
	pod := get(t, job, "spec", "template", "spec")
	tmpl := get(t, job, "spec", "template", "metadata", "labels")
	// labels: the NetworkPolicy selects idp.sandbox/role; the sweep selects idp.sandbox
	for _, l := range []any{get(t, job, "metadata", "labels"), tmpl} {
		m := l.(obj)
		if m[RoleLabel] != "run" || m[LabelKey] != "me" || m[WorkspaceLabel] != ws {
			t.Errorf("labels %v", m)
		}
	}
	spc := get(t, job, "spec")
	if spc.(obj)["backoffLimit"] != 0.0 || spc.(obj)["activeDeadlineSeconds"] != 10.0 || spc.(obj)["ttlSecondsAfterFinished"] != 120.0 {
		t.Errorf("job spec %v", spc)
	}
	p := pod.(obj)
	for key, want := range map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false, "enableServiceLinks": false,
		"hostNetwork": false, "hostPID": false, "hostIPC": false, "runtimeClassName": "gvisor", "shareProcessNamespace": false} {
		if p[key] != want {
			t.Errorf("pod.%s = %v, want %v", key, p[key], want)
		}
	}
	psc := get(t, p, "securityContext").(obj)
	if psc["runAsNonRoot"] != true || psc["runAsUser"] != 65534.0 || psc["runAsGroup"] != 65534.0 || psc["fsGroup"] != 65534.0 ||
		at(psc, "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("pod securityContext %v", psc)
	}
	cs := p["containers"].([]any)
	if len(cs) != 1 {
		t.Fatalf("%d containers", len(cs))
	}
	c := cs[0].(obj)
	if c["image"] != "python:test" || c["workingDir"] != "/work" || c["name"] != "run" {
		t.Errorf("container %v", c)
	}
	sc := get(t, c, "securityContext").(obj)
	if sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true || sc["privileged"] != false ||
		sc["runAsNonRoot"] != true || sc["runAsUser"] != 65534.0 || at(sc, "seccompProfile", "type") != "RuntimeDefault" ||
		len(strs(at(sc, "capabilities", "drop"))) != 1 || strs(at(sc, "capabilities", "drop"))[0] != "ALL" || at(sc, "capabilities", "add") != nil {
		t.Errorf("container securityContext %v", sc)
	}
	res := get(t, c, "resources").(obj)
	for _, kind := range []string{"requests", "limits"} {
		if at(res, kind, "memory") != "268435456" || at(res, kind, "cpu") != "1000m" {
			t.Errorf("resources.%s %v", kind, res[kind])
		}
	}
	env := map[string]string{}
	for _, e := range c["env"].([]any) {
		env[e.(obj)["name"].(string)] = e.(obj)["value"].(string)
	}
	if env["HOME"] != "/tmp" {
		t.Errorf("env %v", env)
	}
	mounts := map[string]string{}
	for _, m := range c["volumeMounts"].([]any) {
		mounts[m.(obj)["mountPath"].(string)] = m.(obj)["name"].(string)
	}
	if len(mounts) != 2 || mounts["/work"] != "work" || mounts["/tmp"] != "tmp" {
		t.Errorf("mounts %v", mounts)
	}
	vols := map[string]obj{}
	for _, v := range p["volumes"].([]any) {
		vols[v.(obj)["name"].(string)] = v.(obj)
	}
	if len(vols) != 2 || at(vols["work"], "persistentVolumeClaim", "claimName") != ws {
		t.Errorf("volumes %v", vols)
	}
	if at(vols["tmp"], "emptyDir", "sizeLimit") != "16777216" || at(vols["tmp"], "emptyDir", "medium") != "Memory" {
		t.Errorf("tmp volume %v", vols["tmp"])
	}
	for _, forbidden := range []string{"hostPath", "projected", "secret", "downwardAPI"} {
		for n, v := range vols {
			if v[forbidden] != nil {
				t.Errorf("volume %s is a %s", n, forbidden)
			}
		}
	}
	if _, ok := p["nodeName"]; ok {
		t.Error("a pinned node")
	}
}

func TestNoRuntimeClassByDefault(t *testing.T) {
	k, f := setup(t, outcome{})
	if _, err := k.Run(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	if _, ok := at(f.created("jobs")[0], "spec", "template", "spec").(obj)["runtimeClassName"]; ok {
		t.Error("runtimeClassName set without configuration")
	}
}

func TestWriteFileShape(t *testing.T) {
	k, f := setup(t, outcome{})
	data := []byte{0, 1, 0xff, 0xfe, 'x', '\n'} // not UTF-8
	if err := k.WriteFile(context.Background(), ws, "dir/a b.txt", data); err != nil {
		t.Fatal(err)
	}
	cm := f.created("configmaps")[0]
	if at(cm, "binaryData", "data") != base64.StdEncoding.EncodeToString(data) || at(cm, "metadata", "labels", LabelKey) != "me" {
		t.Errorf("configmap %v", cm)
	}
	job := f.created("jobs")[0]
	if at(job, "metadata", "labels", RoleLabel) != "file" {
		t.Errorf("role %v", at(job, "metadata", "labels"))
	}
	pod := get(t, job, "spec", "template", "spec").(obj)
	c := pod["containers"].([]any)[0].(obj)
	if c["image"] != "alpine:test" {
		t.Errorf("image %v", c["image"])
	}
	cmd := strs(c["command"])
	// sh -c <wrapper> sh sh -c <copy script> sh /work/<path>
	if got := strings.Join(cmd[4:], "|"); !strings.HasPrefix(got, "sh|-c|mkdir -p \"$(dirname \"$1\")\" && cat /in/data > \"$1\"|sh|/work/dir/a b.txt") {
		t.Errorf("command %q", got)
	}
	if !strings.Contains(cmd[2], "</in/data") {
		t.Errorf("wrapper %q", cmd[2])
	}
	var in obj
	for _, v := range pod["volumes"].([]any) {
		if v.(obj)["name"] == "in" {
			in = v.(obj)
		}
	}
	if at(in, "configMap", "name") != at(cm, "metadata", "name") {
		t.Errorf("in volume %v", in)
	}
	for _, m := range c["volumeMounts"].([]any) {
		if m.(obj)["name"] == "in" && (m.(obj)["mountPath"] != "/in" || m.(obj)["readOnly"] != true) {
			t.Errorf("in mount %v", m)
		}
	}
	// hardened like a run, and cleaned up
	if at(c, "securityContext", "readOnlyRootFilesystem") != true || at(pod, "automountServiceAccountToken") != false {
		t.Error("file job not hardened")
	}
	assertClean(t, f)
}

func TestWriteFileErrors(t *testing.T) {
	k, f := setup(t, outcome{exit: 1, stderr: "sh: can't create /work/a: No space left on device\n"})
	err := k.WriteFile(context.Background(), ws, "a", []byte("x"))
	if !errors.Is(err, sandbox.ErrInvalid) || !strings.Contains(err.Error(), "full") {
		t.Errorf("full: %v", err)
	}
	f.mu.Lock()
	f.behave = fixed(outcome{exit: 1, stderr: "is a directory"})
	f.mu.Unlock()
	if err := k.WriteFile(context.Background(), ws, "a", []byte("x")); !errors.Is(err, sandbox.ErrInvalid) {
		t.Errorf("other: %v", err)
	}
	before := f.called("POST")
	if err := k.WriteFile(context.Background(), ws, "big", make([]byte, MaxInputBytes+1)); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("too large: %v", err)
	}
	if f.called("POST") != before {
		t.Error("an oversized file reached the cluster")
	}
	// exactly the limit is accepted
	f.mu.Lock()
	f.behave = fixed(outcome{})
	f.mu.Unlock()
	if err := k.WriteFile(context.Background(), ws, "max", make([]byte, MaxInputBytes)); err != nil {
		t.Errorf("1 MiB: %v", err)
	}
	assertClean(t, f)
}

func TestReadFile(t *testing.T) {
	k, f := setup(t, outcome{stdout: "content"})
	b, err := k.ReadFile(context.Background(), ws, "a.txt", 100)
	if err != nil || string(b) != "content" {
		t.Fatalf("%q %v", b, err)
	}
	cmd := cmdOf(f.created("jobs")[0])
	script := cmd[6]
	for _, want := range []string{`[ -f "$1" ] && [ ! -L "$1" ] || exit 44`, `readlink -f "$1"`, `/work/*) ;; *) exit 45;; esac`, `cat "$1"`} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q: %s", want, script)
		}
	}
	if cmd[len(cmd)-1] != "/work/a.txt" {
		t.Errorf("path %q", cmd[len(cmd)-1])
	}
	for _, code := range []int{44, 45} { // not a regular file, a symlink, or one that leaves /work
		f.mu.Lock()
		f.behave = fixed(outcome{exit: code})
		f.mu.Unlock()
		if _, err := k.ReadFile(context.Background(), ws, "link", 100); !errors.Is(err, sandbox.ErrNoFile) {
			t.Errorf("exit %d: %v", code, err)
		}
	}
	f.mu.Lock()
	f.behave = fixed(outcome{stdout: strings.Repeat("x", 50)})
	f.mu.Unlock()
	if _, err := k.ReadFile(context.Background(), ws, "big", 10); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("too large: %v", err)
	}
	if b, err := k.ReadFile(context.Background(), ws, "exact", 50); err != nil || len(b) != 50 {
		t.Errorf("exact fit: %d %v", len(b), err)
	}
	f.mu.Lock()
	f.behave = fixed(outcome{exit: 2, stderr: "boom"})
	f.mu.Unlock()
	if _, err := k.ReadFile(context.Background(), ws, "x", 10); err == nil || errors.Is(err, sandbox.ErrNoFile) {
		t.Errorf("other failure: %v", err)
	}
	assertClean(t, f)
}

func TestListFiles(t *testing.T) {
	k, f := setup(t, outcome{stdout: "12 ./a.txt\n3 ./dir/b c.txt\nbogus\n"})
	files, err := k.ListFiles(context.Background(), ws, 1000)
	if err != nil || len(files) != 2 || files[0] != (sandbox.FileInfo{Path: "a.txt", Size: 12}) || files[1] != (sandbox.FileInfo{Path: "dir/b c.txt", Size: 3}) {
		t.Fatalf("%v %v", files, err)
	}
	cmd := cmdOf(f.created("jobs")[0])
	if !strings.Contains(cmd[6], "find . -type f") || !strings.Contains(cmd[6], "head -n 1000") {
		t.Errorf("script %q", cmd[6])
	}
	f.mu.Lock()
	f.behave = fixed(outcome{exit: 1})
	f.mu.Unlock()
	if _, err := k.ListFiles(context.Background(), ws, 10); err == nil {
		t.Error("failure not reported")
	}
}

func TestDeleteWorkspaceAndSweep(t *testing.T) {
	f := newFake(t, fixed(outcome{}))
	k := f.newKube(t, nil)
	ctx := context.Background()
	mine := map[string]string{LabelKey: "me"}
	other := map[string]string{LabelKey: "someone-else"}
	with := func(l map[string]string, w string) map[string]string {
		m := map[string]string{WorkspaceLabel: w}
		for a, b := range l {
			m[a] = b
		}
		return m
	}
	for _, w := range []string{"w1", "w2"} {
		f.put("persistentvolumeclaims", w, mine)
		f.put("jobs", w+"-job", with(mine, w))
		f.put("configmaps", w+"-cm", with(mine, w))
		f.put("pods", w+"-pod", with(mine, w))
	}
	f.put("persistentvolumeclaims", "foreign", other)
	f.put("jobs", "foreign-job", with(other, "foreign"))

	if err := k.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]string{{"persistentvolumeclaims", "w1"}, {"jobs", "w1-job"}, {"configmaps", "w1-cm"}, {"pods", "w1-pod"}} {
		if f.has(r[0], r[1]) {
			t.Errorf("%v survived", r)
		}
	}
	if !f.has("persistentvolumeclaims", "w2") || !f.has("jobs", "w2-job") {
		t.Error("another workspace was touched")
	}
	if err := k.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Errorf("deleting what is gone: %v", err)
	}
	if err := k.DeleteWorkspace(ctx, "Not Valid"); err != nil {
		t.Errorf("invalid name: %v", err)
	}
	if err := k.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"persistentvolumeclaims", "jobs", "configmaps", "pods"} {
		if f.has(r, "w2") || f.has(r, "w2-job") || f.has(r, "w2-cm") || f.has(r, "w2-pod") {
			t.Errorf("%s of this owner survived the sweep", r)
		}
	}
	if !f.has("persistentvolumeclaims", "foreign") || !f.has("jobs", "foreign-job") {
		t.Error("the sweep removed another owner's objects")
	}
}

func TestPing(t *testing.T) {
	f := newFake(t, fixed(outcome{}))
	k := f.newKube(t, nil)
	if err := k.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.ready = 503
	f.mu.Unlock()
	if err := k.Ping(context.Background()); err == nil {
		t.Error("an unready API server passed")
	}
	f.mu.Lock()
	f.ready = 200
	f.token = "other"
	f.mu.Unlock()
	if err := k.Ping(context.Background()); err == nil {
		t.Error("a rejected token passed")
	}
}

func TestNamesAreValidDNSLabels(t *testing.T) {
	k, f := setup(t, outcome{})
	_ = k.WriteFile(context.Background(), ws, "x", []byte("y"))
	_, _ = k.Run(context.Background(), spec())
	jobs := f.created("jobs")
	if len(jobs) != 2 {
		t.Fatal(len(jobs))
	}
	for _, j := range jobs {
		n := at(j, "metadata", "name").(string)
		if !dnsLabel.MatchString(n) || len(n) > 57 {
			t.Errorf("job name %q", n)
		}
	}
	if at(jobs[0], "metadata", "name") == at(jobs[1], "metadata", "name") {
		t.Error("names collide")
	}
}

func TestTLSWithExplicitCA(t *testing.T) {
	f := newFake(t, fixed(outcome{}))
	f.srv.Close()
	f.srv = newTLS(t, f)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	writeCA(t, ca, f)
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := New(Config{APIServer: f.srv.URL, TokenFile: tok, CAFile: ca, Namespace: testNS, HelperImage: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	// without the CA the connection is refused
	k, err = New(Config{APIServer: f.srv.URL, Token: "tok", Namespace: testNS, HelperImage: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Ping(context.Background()); err == nil {
		t.Error("an untrusted server certificate was accepted")
	}
}

func TestInClusterConfiguration(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := New(Config{HelperImage: "x"}); err == nil {
		t.Error("no cluster, no address, yet no error")
	}
	if _, err := New(Config{APIServer: "http://127.0.0.1:1", HelperImage: "x", Namespace: "BAD"}); err == nil {
		t.Error("bad namespace accepted")
	}
	if _, err := New(Config{APIServer: "http://127.0.0.1:1", HelperImage: "x", Owner: "has space"}); err == nil {
		t.Error("bad owner accepted")
	}
	if _, err := New(Config{APIServer: "http://127.0.0.1:1"}); err == nil {
		t.Error("no helper image accepted")
	}
}

// TestWrapperWithARealShell runs the wrapper script with the local sh: exit status, the stderr split and
// its cap are properties of the shell, not of the fake.
func TestWrapperWithARealShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	errPath := filepath.Join(t.TempDir(), "stderr")
	run := func(n int, script string) (string, string, bool, int) {
		marker := randHex(8)
		cmd := wrap(marker, n, false, errPath, []string{"sh", "-c", script})
		out, err := exec.Command(cmd[0], cmd[1:]...).Output()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		so, se, tr := splitLog(string(out), marker, n)
		return so, se, tr, code
	}
	so, se, tr, code := run(100, `printf 'out'; echo err >&2; exit 7`)
	if so != "out" || se != "err\n" || tr || code != 7 {
		t.Errorf("%q %q %v %d", so, se, tr, code)
	}
	so, se, tr, code = run(8, `printf 'o\n\n'; yes e | head -c 1000 >&2`)
	if so != "o\n\n" || len(se) != 8 || !tr || code != 0 {
		t.Errorf("%q %q %v %d", so, se, tr, code)
	}
	so, se, _, code = run(100, `exec /nonexistent/command`)
	if so != "" || code != 127 || se == "" {
		t.Errorf("%q %q %d", so, se, code)
	}
	// code that prints something that looks like the marker line cannot forge the split: it does not know the marker
	so, se, _, _ = run(200, `printf '\n--IDP-STDERR-0000000000000000--\nforged'`)
	if !strings.Contains(so, "forged") || se != "" {
		t.Errorf("%q %q", so, se)
	}
}

func strs(o any) []string {
	var out []string
	for _, v := range o.([]any) {
		out = append(out, v.(string))
	}
	return out
}

func cmdOf(job obj) []string {
	return strs(at(job, "spec", "template", "spec", "containers").([]any)[0].(obj)["command"])
}
