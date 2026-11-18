package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"idp-sandbox/internal/egress"
	"idp-sandbox/internal/sandbox"
)

var egressKey = []byte("0123456789abcdef0123456789abcdef")

const proxyName = "idp-sandbox-egress.idp-sandbox.svc.cluster.local"

func withEgress(key []byte, addrs ...string) func(*Config) {
	return func(c *Config) {
		c.EgressProxy, c.EgressKey = proxyName, key
		c.Resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != proxyName {
				return nil, errors.New("asked for " + host)
			}
			if len(addrs) == 0 {
				return nil, errors.New("no such host")
			}
			var out []netip.Addr
			for _, a := range addrs {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		}
	}
}

// proxyEnv is the proxy URL the pod's HTTP clients are given; all four variables say the same.
func proxyEnv(t *testing.T, job obj) string {
	t.Helper()
	got := map[string]string{}
	for _, e := range at(job, "spec", "template", "spec").(obj)["containers"].([]any)[0].(obj)["env"].([]any) {
		if n := e.(obj)["name"].(string); strings.Contains(strings.ToLower(n), "proxy") {
			got[n] = e.(obj)["value"].(string)
		}
	}
	if len(got) == 0 {
		return ""
	}
	u := got["HTTP_PROXY"]
	if len(got) != 4 || got["HTTPS_PROXY"] != u || got["http_proxy"] != u || got["https_proxy"] != u {
		t.Fatalf("proxy variables %v", got)
	}
	return u
}

func networkLabels(job obj) []any {
	return []any{at(job, "metadata", "labels", NetworkLabel), at(job, "spec", "template", "metadata", "labels", NetworkLabel)}
}

// D22: asking for the network is one more label on the pod, the one the NetworkPolicies select, and a proxy URL that
// names the proxy by address (a run pod has no DNS) and carries the run's credential.
func TestAnEgressRunIsLabelledAndToldTheProxy(t *testing.T) {
	_, f := setup(t, outcome{})
	k := f.newKube(t, withEgress(egressKey, "10.96.44.7"))
	s := spec()
	s.Egress, s.Tenant = true, "acme"
	before := time.Now()
	if _, err := k.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	job := f.created("jobs")[0]
	if l := networkLabels(job); l[0] != NetworkEgress || l[1] != NetworkEgress {
		t.Errorf("network labels %v", l)
	}
	if at(job, "spec", "template", "metadata", "labels", RoleLabel) != "run" {
		t.Error("an egress run is not labelled as a run")
	}
	u, err := url.Parse(proxyEnv(t, job))
	if err != nil || u.Scheme != "http" || u.Host != "10.96.44.7:3128" || u.User.Username() != egress.User {
		t.Fatalf("proxy URL %v %v", u, err)
	}
	cred, _ := u.User.Password()
	// good wherever the proxy's pod happens to be: the address a pod of the proxy has is not the Service's
	c, err := egress.Verify(egressKey, cred, time.Now(), netip.MustParseAddr("192.168.82.7"))
	if err != nil || c.Tenant != "acme" || c.Proxy != "" || len(c.Workspace) != 8 || strings.Contains(ws, c.Workspace) {
		t.Fatalf("%+v %v", c, err)
	}
	// the run's 5 s, the 5 s the pod may take to start (Grace), and the slack
	if min, max := before.Add(70*time.Second).Unix(), time.Now().Add(70*time.Second).Unix(); c.Expires < min || c.Expires > max {
		t.Errorf("expires %d, want %d..%d", c.Expires, min, max)
	}
	assertClean(t, f)
}

func TestOnlyARunThatAskedGetsTheNetworkLabel(t *testing.T) {
	_, f := setup(t, outcome{})
	k := f.newKube(t, withEgress(egressKey, "10.96.44.7"))
	if _, err := k.Run(context.Background(), spec()); err != nil { // the proxy is configured; this run did not ask
		t.Fatal(err)
	}
	if err := k.WriteFile(context.Background(), ws, "a.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := k.NetworkProbe(context.Background(), "10.96.0.1", 443); err == nil {
		t.Fatal("the fake answered a probe")
	}
	jobs := f.created("jobs")
	if len(jobs) != 3 {
		t.Fatalf("%d jobs", len(jobs))
	}
	for _, job := range jobs {
		if l := networkLabels(job); l[0] != nil || l[1] != nil {
			t.Errorf("%v carries the network label: %v", at(job, "metadata", "name"), l)
		}
		if u := proxyEnv(t, job); u != "" {
			t.Errorf("%v was told a proxy: %s", at(job, "metadata", "name"), u)
		}
	}
	// a file operation never carries it, whatever it is handed
	asRun, asFile := obj{}, obj{} // as the API server would get them
	for role, into := range map[string]*obj{"run": &asRun, "file": &asFile} {
		b, err := json.Marshal(k.jobObject(jobSpec{role: role, workspace: ws, image: "alpine:test", egress: true, limits: limits(), jobName: "x"}))
		if err != nil || json.Unmarshal(b, into) != nil {
			t.Fatal(err)
		}
	}
	if l := networkLabels(asRun); l[0] != NetworkEgress || l[1] != NetworkEgress {
		t.Fatalf("the same job as a run: %v", l)
	}
	if l := networkLabels(asFile); l[0] != nil || l[1] != nil {
		t.Errorf("a file pod with the network label: %v", l)
	}
}

func TestEgressWithoutAProxyIsRefusedBeforeTheCluster(t *testing.T) {
	_, f := setup(t, outcome{})
	s := spec()
	s.Egress = true
	for name, c := range map[string]struct {
		mod  func(*Config)
		want error
	}{
		"no proxy configured":             {nil, sandbox.ErrNoEgress},
		"its Service cannot be looked up": {withEgress(egressKey), nil},
	} {
		k := f.newKube(t, c.mod)
		before := f.called("POST")
		_, err := k.Run(context.Background(), s)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || f.called("POST") != before {
			t.Errorf("%s: %v, %d objects created", name, err, f.called("POST")-before)
		}
	}
}

func TestTheProxyURLWithoutAKeyAndByAddress(t *testing.T) {
	_, f := setup(t, outcome{})
	s := spec()
	s.Egress, s.Tenant = true, "acme"
	for name, c := range map[string]struct {
		mod  func(*Config)
		want string
	}{
		"no key: no credential":        {withEgress(nil, "10.96.44.7"), "http://10.96.44.7:3128"},
		"a dual-stack Service: IPv4":   {withEgress(nil, "fd00:10:96::7", "10.96.44.7"), "http://10.96.44.7:3128"},
		"an IPv6-only Service":         {withEgress(nil, "fd00:10:96::7"), "http://[fd00:10:96::7]:3128"},
		"an address, and another port": {func(c *Config) { c.EgressProxy, c.EgressPort = "10.96.44.9", 8080 }, "http://10.96.44.9:8080"},
	} {
		k := f.newKube(t, func(cfg *Config) {
			c.mod(cfg)
			if cfg.Resolve == nil {
				cfg.Resolve = func(context.Context, string) ([]netip.Addr, error) {
					return nil, errors.New("an address is not looked up")
				}
			}
		})
		if _, err := k.Run(context.Background(), s); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		jobs := f.created("jobs")
		if got := proxyEnv(t, jobs[len(jobs)-1]); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

func TestEgressConfigurationThatCannotWorkIsRefused(t *testing.T) {
	f := newFake(t, fixed(outcome{}))
	for name, mod := range map[string]func(*Config){
		"a key without a proxy":     func(c *Config) { c.EgressKey = egressKey },
		"a URL instead of a name":   func(c *Config) { c.EgressProxy = "http://proxy:3128" },
		"a name with a shell in it": func(c *Config) { c.EgressProxy = "$(reboot)" },
		"a port out of range":       func(c *Config) { c.EgressProxy, c.EgressPort = "proxy", 70000 },
	} {
		cfg := Config{APIServer: f.srv.URL, Token: f.token, Namespace: testNS, HelperImage: "alpine:test"}
		mod(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// D22: the start-up probe of egress runs is a pod with exactly their labels, asked whether it reaches the API server
// (it must not) and the proxy (it should).
func TestEgressProbeIsAPodLikeAnEgressRun(t *testing.T) {
	answer := "BLOCKED\nREACHED\n"
	f := newFake(t, func(obj, []string) outcome { return outcome{stdout: answer} })
	k := f.newKube(t, withEgress(egressKey, "10.96.44.7"))
	reached, proxy, err := k.EgressProbe(context.Background(), "10.96.0.1", 443)
	if err != nil || reached || proxy != nil {
		t.Fatalf("%v %v %v", reached, proxy, err)
	}
	job := f.created("jobs")[0]
	labels := at(job, "spec", "template", "metadata", "labels").(obj)
	if labels[RoleLabel] != "run" || labels[NetworkLabel] != NetworkEgress || labels[LabelKey] != "me" || labels[WorkspaceLabel] != nil {
		t.Errorf("the probe is labelled %v: the policies would not select it as an egress run", labels)
	}
	if proxyEnv(t, job) != "" {
		t.Error("the probe was given a credential it has no use for")
	}
	cmd := cmdOf(job)
	if got := strings.Join(cmd[len(cmd)-4:], " "); got != "10.96.0.1 443 10.96.44.7 3128" {
		t.Errorf("targets %q", got)
	}
	assertClean(t, f)

	for out, want := range map[string]struct{ reached, proxy bool }{
		"REACHED\nREACHED\n": {true, true}, "BLOCKED\nBLOCKED\n": {false, false}, "REACHED\nBLOCKED\n": {true, false}} {
		f.mu.Lock()
		answer = out
		f.mu.Unlock()
		reached, proxy, err := k.EgressProbe(context.Background(), "10.96.0.1", 443)
		if err != nil || reached != want.reached || (proxy == nil) != want.proxy {
			t.Errorf("%q: %v %v %v", out, reached, proxy, err)
		}
	}
	for _, out := range []string{"NO-TOOL\n", "REACHED\n", "", "REACHED\nMAYBE\n"} {
		f.mu.Lock()
		answer = out
		f.mu.Unlock()
		if _, _, err := k.EgressProbe(context.Background(), "10.96.0.1", 443); err == nil {
			t.Errorf("%q is not an answer about two targets", out)
		}
	}
	// the proxy's Service cannot be found: the question about the API server is still asked
	f.mu.Lock()
	answer = "BLOCKED\n"
	f.mu.Unlock()
	k = f.newKube(t, withEgress(egressKey))
	reached, proxy, err = k.EgressProbe(context.Background(), "10.96.0.1", 443)
	if err != nil || reached || proxy == nil {
		t.Errorf("%v %v %v", reached, proxy, err)
	}
}
