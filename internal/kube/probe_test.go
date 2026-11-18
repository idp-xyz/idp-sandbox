package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// D20: the probe is a pod the NetworkPolicy of the runs namespace selects exactly as it selects a run, with nothing
// mounted; what it prints decides.
func TestNetworkProbeIsAPodLikeARunAndSaysWhatItCouldReach(t *testing.T) {
	f := newFake(t, fixed(outcome{stdout: "BLOCKED\n"}))
	k := f.newKube(t, nil)
	reached, err := k.NetworkProbe(context.Background(), "10.96.0.1", 443)
	if err != nil || reached {
		t.Fatalf("a blocked probe: %v %v", reached, err)
	}
	job := f.created("jobs")[0]
	labels := at(job, "spec", "template", "metadata", "labels").(obj)
	if labels[RoleLabel] != "run" || labels[LabelKey] != "me" || labels[WorkspaceLabel] != nil {
		t.Errorf("the probe is labelled %v: the policy that cuts runs off would not select it as a run", labels)
	}
	pod := at(job, "spec", "template", "spec").(obj)
	for _, v := range pod["volumes"].([]any) {
		if v.(obj)["persistentVolumeClaim"] != nil {
			t.Error("the probe mounts a workspace")
		}
	}
	c := pod["containers"].([]any)[0].(obj)
	if c["workingDir"] != "/tmp" || at(c, "securityContext", "readOnlyRootFilesystem") != true || at(c, "securityContext", "runAsUser") != float64(sandboxUID) ||
		at(pod, "automountServiceAccountToken") != false {
		t.Errorf("the probe is not hardened like a run: %v", c)
	}
	cmd := cmdOf(job)
	if !strings.Contains(cmd[6], `nc -w 3 "$1" "$2"`) || cmd[len(cmd)-2] != "10.96.0.1" || cmd[len(cmd)-1] != "443" {
		t.Errorf("%v", cmd)
	}
	if f.called("GET /api/v1/namespaces/"+testNS+"/persistentvolumeclaims/") != 0 {
		t.Error("the probe looked for a workspace")
	}
	assertClean(t, f)

	for out, want := range map[string]struct {
		reached bool
		err     error
	}{"REACHED\n": {true, nil}, "NO-TOOL\n": {false, ErrNoProbeTool}} {
		f.mu.Lock()
		f.behave = fixed(outcome{stdout: out})
		f.mu.Unlock()
		reached, err := k.NetworkProbe(context.Background(), "10.96.0.1", 443)
		if reached != want.reached || !errors.Is(err, want.err) {
			t.Errorf("%q: %v %v", out, reached, err)
		}
	}
	f.mu.Lock()
	f.behave = fixed(outcome{stdout: "", exit: 1})
	f.mu.Unlock()
	if _, err := k.NetworkProbe(context.Background(), "10.96.0.1", 443); err == nil {
		t.Error("a probe without an answer is not an answer")
	}
	for _, bad := range []struct {
		host string
		port int
	}{{"", 443}, {"a b", 443}, {"$(reboot)", 443}, {"10.96.0.1", 0}, {"10.96.0.1", 70000}} {
		before := f.called("POST")
		if _, err := k.NetworkProbe(context.Background(), bad.host, bad.port); err == nil || f.called("POST") != before {
			t.Errorf("target %q:%d reached the cluster: %v", bad.host, bad.port, err)
		}
	}
	assertClean(t, f)
}

func TestAPIServerAddrIsUsableOnlyFromInsideACluster(t *testing.T) {
	f := newFake(t, fixed(outcome{}))
	for api, want := range map[string]struct {
		host string
		port int
		ok   bool
	}{
		"https://10.96.0.1:443":                {"10.96.0.1", 443, true},
		"https://10.96.0.1":                    {"10.96.0.1", 443, true},
		"https://kubernetes.default.svc:6443/": {"kubernetes.default.svc", 6443, true},
		"https://127.0.0.1:45221":              {"127.0.0.1", 45221, false}, // kubectl's view of a kind cluster: a pod's loopback is its own
		"http://localhost:8001":                {"localhost", 8001, false},
		"https://[::1]:6443":                   {"::1", 6443, false},
	} {
		k := f.newKube(t, func(c *Config) { c.APIServer = api })
		if host, port, ok := k.APIServerAddr(); host != want.host || port != want.port || ok != want.ok {
			t.Errorf("%s: %s %d %v", api, host, port, ok)
		}
	}
}
