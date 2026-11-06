// Package deploy checks deploy/kubernetes for internal consistency, with no cluster involved (the
// reference host has none): every manifest is named and namespaced, the Role grants exactly what the
// backend calls, the runs namespace denies all network traffic and is bounded by a quota, and the
// service is hardened. It cannot show that a CNI enforces the policies; that needs a real cluster.
package deploy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"idp-sandbox/internal/kube"
)

const (
	dir      = "../../deploy/kubernetes"
	svcNS    = "idp-sandbox"
	runsNS   = "idp-sandbox-runs"
	gatewayN = "idp-mcp"
)

type doc = map[string]any

func load(t *testing.T, name string) []doc {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	var out []doc
	for {
		var d doc
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

func get(d any, path ...string) any {
	for _, p := range path {
		m, ok := d.(doc)
		if !ok {
			return nil
		}
		d = m[p]
	}
	return d
}

func str(d any, path ...string) string { s, _ := get(d, path...).(string); return s }
func list(d any, path ...string) []any { l, _ := get(d, path...).([]any); return l }
func name(d doc) string                { return str(d, "kind") + "/" + str(d, "metadata", "name") }
func ns(d doc) string                  { return str(d, "metadata", "namespace") }

func strs(l []any) []string {
	var out []string
	for _, v := range l {
		out = append(out, v.(string))
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

type manifests struct {
	kust   doc
	docs   []doc
	byKind map[string][]doc
}

func loadAll(t *testing.T) *manifests {
	t.Helper()
	m := &manifests{kust: load(t, "kustomization.yaml")[0], byKind: map[string][]doc{}}
	for _, r := range list(m.kust, "resources") {
		for _, d := range load(t, r.(string)) {
			m.docs = append(m.docs, d)
			m.byKind[str(d, "kind")] = append(m.byKind[str(d, "kind")], d)
		}
	}
	if len(m.docs) < 14 {
		t.Fatalf("only %d documents", len(m.docs))
	}
	return m
}

func (m *manifests) find(kind, namespace, n string) doc {
	for _, d := range m.byKind[kind] {
		if str(d, "metadata", "name") == n && (namespace == "" || ns(d) == namespace) {
			return d
		}
	}
	return nil
}

func TestKustomizationUsesLabelsNotCommonLabels(t *testing.T) {
	m := loadAll(t)
	if m.kust["commonLabels"] != nil {
		t.Error("commonLabels rewrites NetworkPolicy peer selectors; use labels with includeTemplates")
	}
	if m.kust["namespace"] != nil {
		t.Error("a kustomization namespace would move the runs namespace's objects into the service's")
	}
	ls := list(m.kust, "labels")
	if len(ls) != 1 || get(ls[0], "includeTemplates") != true {
		t.Errorf("labels: %v", ls)
	}
	// every file in the directory except the kustomization is listed
	listed := map[string]bool{"kustomization.yaml": true}
	for _, r := range list(m.kust, "resources") {
		listed[r.(string)] = true
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") && !listed[e.Name()] {
			t.Errorf("%s is not in the kustomization", e.Name())
		}
	}
	if len(list(m.kust, "images")) != 1 || str(list(m.kust, "images")[0], "name") != "idp-sandbox" {
		t.Error("image override missing")
	}
}

func TestEveryManifestIsNamedAndNamespaced(t *testing.T) {
	m := loadAll(t)
	for _, d := range m.docs {
		if str(d, "apiVersion") == "" || str(d, "kind") == "" || str(d, "metadata", "name") == "" {
			t.Errorf("incomplete object: %v", d)
		}
		if str(d, "kind") == "Namespace" {
			if ns(d) != "" {
				t.Errorf("%s: a namespace in a namespace", name(d))
			}
			continue
		}
		if n := ns(d); n != svcNS && n != runsNS {
			t.Errorf("%s: namespace %q", name(d), n)
		}
		if strings.HasPrefix(str(d, "kind"), "Cluster") {
			t.Errorf("%s: nothing cluster-wide belongs here", name(d))
		}
	}
	got := map[string]bool{}
	for _, n := range m.byKind["Namespace"] {
		got[str(n, "metadata", "name")] = true
		if str(n, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" {
			t.Errorf("%s does not enforce the restricted pod security standard", name(n))
		}
	}
	if !got[svcNS] || !got[runsNS] || len(got) != 2 {
		t.Errorf("namespaces %v", got)
	}
	if str(m.find("Namespace", "", svcNS), "metadata", "labels", "idp-mcp/mcp-upstream") != "true" {
		t.Error("the service namespace is not labelled as an MCP upstream for the gateway")
	}
}

func TestServiceIsHardenedAndWired(t *testing.T) {
	m := loadAll(t)
	if len(m.byKind["Deployment"]) != 1 {
		t.Fatalf("%d deployments", len(m.byKind["Deployment"]))
	}
	for _, k := range []string{"Job", "CronJob", "DaemonSet", "StatefulSet", "Pod", "Secret"} {
		if len(m.byKind[k]) != 0 {
			t.Errorf("unexpected %s", k)
		}
	}
	d := m.byKind["Deployment"][0]
	if ns(d) != svcNS {
		t.Errorf("deployed to %s", ns(d))
	}
	// one replica, recreated: the workspace table is in memory and the start-up sweep deletes this owner's objects
	if get(d, "spec", "replicas") != 1 || str(d, "spec", "strategy", "type") != "Recreate" {
		t.Error("the sandbox must be a single replica with Recreate")
	}
	spec, _ := get(d, "spec", "template", "spec").(doc)
	// the one pod that holds a Kubernetes credential, and runs as the account the Role is bound to
	if spec["automountServiceAccountToken"] != true || spec["serviceAccountName"] != "idp-sandbox" || spec["enableServiceLinks"] != false {
		t.Error("service account wiring")
	}
	if m.find("ServiceAccount", svcNS, "idp-sandbox") == nil {
		t.Error("service account missing")
	}
	if get(spec, "securityContext", "runAsNonRoot") != true || str(spec, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		t.Error("pod securityContext")
	}
	if spec["hostNetwork"] != nil || spec["hostPID"] != nil || spec["hostIPC"] != nil || spec["volumes"] != nil {
		t.Error("host namespaces or volumes on the service")
	}
	cs := list(spec, "containers")
	if len(cs) != 1 {
		t.Fatalf("%d containers", len(cs))
	}
	c := cs[0].(doc)
	if str(c, "image") != "idp-sandbox" {
		t.Errorf("image %q is not overridden through kustomization images", str(c, "image"))
	}
	sc := get(c, "securityContext")
	if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true || !eq(strs(list(sc, "capabilities", "drop")), []string{"ALL"}) {
		t.Errorf("container securityContext %v", sc)
	}
	if get(c, "resources", "limits", "memory") == nil || get(c, "resources", "limits", "cpu") == nil || get(c, "resources", "requests") == nil {
		t.Error("resources")
	}
	ports := map[string]int{}
	for _, p := range list(c, "ports") {
		ports[str(p, "name")], _ = get(p, "containerPort").(int)
	}
	if ports["mcp"] != 8570 || ports["ops"] != 9570 {
		t.Errorf("ports %v", ports)
	}
	for probe, path := range map[string]string{"readinessProbe": "/readyz", "livenessProbe": "/healthz"} {
		if str(c, probe, "httpGet", "path") != path || str(c, probe, "httpGet", "port") != "ops" {
			t.Errorf("%s: %v", probe, c[probe])
		}
	}
	env := map[string]string{}
	for _, e := range list(c, "env") {
		env[str(e, "name")], _ = get(e, "value").(string)
	}
	for k, v := range map[string]string{"IDP_SANDBOX_BACKEND": "kubernetes", "IDP_SANDBOX_NAMESPACE": runsNS,
		"IDP_SANDBOX_LISTEN": "0.0.0.0:8570", "IDP_SANDBOX_PRIVATE_NETWORK": "true", "IDP_SANDBOX_METRICS": "0.0.0.0:9570"} {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// the Service selects the pods and exposes 8570
	svc := m.find("Service", svcNS, "idp-sandbox")
	labels, _ := get(d, "spec", "template", "metadata", "labels").(doc)
	sel, _ := get(svc, "spec", "selector").(doc)
	if len(sel) == 0 {
		t.Fatal("service without selector")
	}
	for k, v := range sel {
		if labels[k] != v {
			t.Errorf("service selector %s=%v matches no pod", k, v)
		}
	}
	if get(list(svc, "spec", "ports")[0], "port") != 8570 {
		t.Error("service port")
	}
}

// what the backend calls (internal/kube): the Role may not grant more than this
var allowed = map[string]map[string]bool{
	"batch/jobs":              {"get": true, "list": true, "create": true, "delete": true},
	"/configmaps":             {"get": true, "list": true, "create": true, "delete": true},
	"/persistentvolumeclaims": {"get": true, "list": true, "create": true, "delete": true},
	"/pods":                   {"get": true, "list": true, "delete": true},
	"/pods/log":               {"get": true},
}

func TestRoleGrantsExactlyWhatTheBackendCalls(t *testing.T) {
	m := loadAll(t)
	if len(m.byKind["Role"]) != 1 || len(m.byKind["RoleBinding"]) != 1 || len(m.byKind["ClusterRole"])+len(m.byKind["ClusterRoleBinding"]) != 0 {
		t.Fatal("expected one Role and one RoleBinding, nothing cluster-wide")
	}
	role := m.byKind["Role"][0]
	if ns(role) != runsNS {
		t.Errorf("the Role is in %s: it must be in the runs namespace only", ns(role))
	}
	granted := map[string]map[string]bool{}
	for _, r := range list(role, "rules") {
		if r.(doc)["resourceNames"] != nil || r.(doc)["nonResourceURLs"] != nil {
			t.Errorf("unexpected rule shape %v", r)
		}
		for _, g := range list(r, "apiGroups") {
			for _, res := range list(r, "resources") {
				key := g.(string) + "/" + res.(string)
				if granted[key] == nil {
					granted[key] = map[string]bool{}
				}
				for _, v := range list(r, "verbs") {
					granted[key][v.(string)] = true
				}
			}
		}
	}
	for key, verbs := range granted {
		max, ok := allowed[key]
		if !ok {
			t.Errorf("resource %q is not used by the backend", key)
			continue
		}
		for v := range verbs {
			if !max[v] {
				t.Errorf("%s: verb %q is not used by the backend", key, v)
			}
		}
	}
	for key, verbs := range allowed { // and nothing the backend needs is missing
		for v := range verbs {
			if !granted[key][v] {
				t.Errorf("%s: verb %q is needed and not granted", key, v)
			}
		}
	}
	rb := m.byKind["RoleBinding"][0]
	if ns(rb) != runsNS || str(rb, "roleRef", "kind") != "Role" || str(rb, "roleRef", "name") != str(role, "metadata", "name") {
		t.Errorf("binding %v", rb)
	}
	subs := list(rb, "subjects")
	if len(subs) != 1 || str(subs[0], "kind") != "ServiceAccount" || str(subs[0], "name") != "idp-sandbox" || str(subs[0], "namespace") != svcNS {
		t.Errorf("subjects %v", subs)
	}
}

func TestRunsNamespaceHasNoNetwork(t *testing.T) {
	m := loadAll(t)
	var denyAll, runs bool
	for _, p := range m.byKind["NetworkPolicy"] {
		if ns(p) != runsNS {
			continue
		}
		// nothing in the runs namespace may allow anything: no policy there carries a rule
		if len(list(p, "spec", "ingress")) != 0 || len(list(p, "spec", "egress")) != 0 {
			t.Errorf("%s allows traffic in the runs namespace", name(p))
		}
		types := strs(list(p, "spec", "policyTypes"))
		sel, _ := get(p, "spec", "podSelector").(doc)
		if len(sel) == 0 && eq(types, []string{"Egress", "Ingress"}) {
			denyAll = true
		}
		if exprs := list(sel, "matchExpressions"); len(exprs) == 1 {
			e := exprs[0]
			// the label the backend puts on every run and file pod
			if str(e, "key") == kube.RoleLabel && str(e, "operator") == "In" && eq(strs(list(e, "values")), []string{"file", "run"}) && eq(types, []string{"Egress", "Ingress"}) {
				runs = true
			}
		}
	}
	if !denyAll {
		t.Error("no default-deny (ingress and egress) in the runs namespace")
	}
	if !runs {
		t.Errorf("no explicit deny-all for pods with %s in {run,file}", kube.RoleLabel)
	}
}

func TestServiceNetworkPolicy(t *testing.T) {
	m := loadAll(t)
	var denyAll, dns, sandbox bool
	for _, p := range m.byKind["NetworkPolicy"] {
		if ns(p) != svcNS {
			continue
		}
		sel, _ := get(p, "spec", "podSelector").(doc)
		types := strs(list(p, "spec", "policyTypes"))
		switch {
		case len(sel) == 0 && eq(types, []string{"Egress", "Ingress"}):
			denyAll = len(list(p, "spec", "ingress"))+len(list(p, "spec", "egress")) == 0
		case len(sel) == 0 && eq(types, []string{"Egress"}):
			dns = true
			for _, r := range list(p, "spec", "egress") {
				to := list(r, "to")
				if len(to) != 1 || str(to[0], "podSelector", "matchLabels", "k8s-app") != "kube-dns" {
					t.Errorf("dns policy reaches %v", to)
				}
				for _, port := range list(r, "ports") {
					if get(port, "port") != 53 {
						t.Errorf("dns policy port %v", port)
					}
				}
			}
		case str(sel, "matchLabels", "app.kubernetes.io/component") == "sandbox":
			sandbox = true
			ing := list(p, "spec", "ingress")
			if len(ing) != 1 {
				t.Fatalf("%d ingress rules", len(ing))
			}
			from := list(ing[0], "from")
			if len(from) != 1 { // one peer with BOTH selectors: the gateway pods of the gateway namespace, not either
				t.Fatalf("%d ingress peers", len(from))
			}
			if str(from[0], "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != gatewayN ||
				str(from[0], "podSelector", "matchLabels", "app.kubernetes.io/component") != "gateway" || get(from[0], "ipBlock") != nil {
				t.Errorf("ingress peer %v", from[0])
			}
			ports := list(ing[0], "ports")
			if len(ports) != 1 || get(ports[0], "port") != 8570 {
				t.Errorf("ingress ports %v", ports)
			}
			// egress: the API server only
			eg := list(p, "spec", "egress")
			if len(eg) != 1 {
				t.Fatalf("%d egress rules", len(eg))
			}
			to := list(eg[0], "to")
			if len(to) != 1 || get(to[0], "ipBlock") == nil || get(to[0], "namespaceSelector") != nil {
				t.Errorf("egress peers %v", to)
			}
			for _, port := range list(eg[0], "ports") {
				if pv := get(port, "port"); pv != 6443 && pv != 443 {
					t.Errorf("egress port %v", pv)
				}
			}
		default:
			t.Errorf("unexpected policy %s", name(p))
		}
	}
	if !denyAll || !dns || !sandbox {
		t.Errorf("default-deny %v, dns %v, sandbox %v", denyAll, dns, sandbox)
	}
}

func TestRunsNamespaceIsBounded(t *testing.T) {
	m := loadAll(t)
	q := m.find("ResourceQuota", runsNS, "idp-sandbox-runs")
	if q == nil {
		t.Fatal("no ResourceQuota in the runs namespace")
	}
	for _, k := range []string{"pods", "count/jobs.batch", "configmaps", "persistentvolumeclaims", "requests.storage",
		"requests.cpu", "requests.memory", "limits.cpu", "limits.memory"} {
		if get(q, "spec", "hard", k) == nil {
			t.Errorf("quota lacks %s", k)
		}
	}
	lr := m.find("LimitRange", runsNS, "idp-sandbox-runs")
	if lr == nil {
		t.Fatal("no LimitRange in the runs namespace")
	}
	types := map[string]doc{}
	for _, l := range list(lr, "spec", "limits") {
		types[str(l, "type")] = l.(doc)
	}
	if get(types["Container"], "max", "cpu") == nil || get(types["Container"], "max", "memory") == nil || get(types["PersistentVolumeClaim"], "max", "storage") == nil {
		t.Errorf("limit range %v", types)
	}
	// the runs namespace holds no workload of its own: its pods are the service's, created at run time
	for _, d := range m.docs {
		if ns(d) == runsNS {
			switch str(d, "kind") {
			case "Deployment", "Job", "CronJob", "Pod", "DaemonSet", "StatefulSet", "ServiceAccount", "Service":
				t.Errorf("%s in the runs namespace", name(d))
			}
		}
	}
}
