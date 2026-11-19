package deploy

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"idp-sandbox/internal/egress"
	"idp-sandbox/internal/kube"
)

// deploy/kubernetes-egress (ADR-0001 D22): deploy/kubernetes plus the egress proxy. These tests keep the overlay's
// own files consistent with each other and with what the backend puts on a pod; that a CNI enforces the two policies
// is shown on a real cluster (tests/kind/egress.sh).
const egressDir = "../../deploy/kubernetes-egress"

func loadEgress(t *testing.T, name string) []doc {
	t.Helper()
	rel, err := filepath.Rel(dir, filepath.Join(egressDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return load(t, rel)
}

func only(t *testing.T, docs []doc, kind, namespace string) doc {
	t.Helper()
	var found []doc
	for _, d := range docs {
		if str(d, "kind") == kind && ns(d) == namespace {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s in %s, want one", len(found), kind, namespace)
	}
	return found[0]
}

func envOf(container any) map[string]string {
	out := map[string]string{}
	for _, e := range list(container, "env") {
		out[str(e, "name")] = str(e, "value")
	}
	return out
}

// the labels the backend puts on a run that may use the proxy, and on nothing else
var egressRun = map[string]string{kube.RoleLabel: "run", kube.NetworkLabel: kube.NetworkEgress}

func isEgressRun(sel any) bool {
	m, _ := get(sel, "matchLabels").(doc)
	if len(m) != len(egressRun) || get(sel, "matchExpressions") != nil {
		return false
	}
	for k, v := range egressRun {
		if m[k] != v {
			return false
		}
	}
	return true
}

func TestEgressOverlayIsTheBasePlusTheProxy(t *testing.T) {
	k := loadEgress(t, "kustomization.yaml")[0]
	if k["commonLabels"] != nil || k["namespace"] != nil {
		t.Error("commonLabels rewrites NetworkPolicy peer selectors, and a namespace would merge the two namespaces")
	}
	used := map[string]bool{"kustomization.yaml": true}
	var base bool
	for _, r := range list(k, "resources") {
		base = base || r == "../kubernetes"
		used[r.(string)] = true
	}
	if !base {
		t.Error("the overlay does not build on deploy/kubernetes")
	}
	for _, p := range list(k, "patches") {
		used[str(p, "path")] = true
	}
	for _, g := range list(k, "configMapGenerator") {
		for _, f := range list(g, "files") {
			used[f.(string)] = true
		}
	}
	entries, _ := os.ReadDir(egressDir)
	for _, e := range entries {
		if !used[e.Name()] {
			t.Errorf("%s is not used by the kustomization", e.Name())
		}
		if strings.Contains(strings.ToLower(e.Name()), "key") || strings.Contains(strings.ToLower(e.Name()), "secret") {
			t.Errorf("%s: the shared key does not belong in the repository", e.Name())
		}
	}
	ls := list(k, "labels")
	if len(ls) != 1 || get(ls[0], "includeTemplates") != true || get(ls[0], "includeSelectors") != nil {
		t.Errorf("labels: %v", ls)
	}
	if imgs := list(k, "images"); len(imgs) != 1 || str(imgs[0], "name") != "idp-sandbox-egress" || !strings.HasPrefix(str(imgs[0], "digest"), "sha256:") {
		t.Errorf("images: %v", imgs)
	}
	for _, f := range []string{"egress.yaml", "networkpolicy.yaml"} {
		for _, d := range loadEgress(t, f) {
			if n := ns(d); n != svcNS && n != runsNS {
				t.Errorf("%s: namespace %q", name(d), n)
			}
			if k := str(d, "kind"); k == "Secret" || k == "Namespace" || strings.HasPrefix(k, "Cluster") || k == "Role" || k == "RoleBinding" {
				t.Errorf("%s does not belong in the overlay", name(d))
			}
		}
	}
}

// An egress run may open connections to the proxy's pods on the proxy's port, and that is all the overlay adds to the
// runs namespace: no ingress, no DNS, no address block.
func TestEgressRunsReachTheProxyAndNothingElse(t *testing.T) {
	docs := loadEgress(t, "networkpolicy.yaml")
	p := only(t, docs, "NetworkPolicy", runsNS)
	if !isEgressRun(get(p, "spec", "podSelector")) {
		t.Fatalf("%s selects %v, not exactly the pods the backend labels as egress runs", name(p), get(p, "spec", "podSelector"))
	}
	if !eq(strs(list(p, "spec", "policyTypes")), []string{"Egress"}) || get(p, "spec", "ingress") != nil {
		t.Errorf("%s is about more than egress", name(p))
	}
	rules := list(p, "spec", "egress")
	if len(rules) != 1 {
		t.Fatalf("%d egress rules", len(rules))
	}
	to := list(rules[0], "to")
	if len(to) != 1 { // one peer with both selectors: the proxy's pods in the service's namespace, not either
		t.Fatalf("%d peers", len(to))
	}
	nsSel, _ := get(to[0], "namespaceSelector", "matchLabels").(doc)
	podSel, _ := get(to[0], "podSelector", "matchLabels").(doc)
	if len(nsSel) != 1 || nsSel["kubernetes.io/metadata.name"] != svcNS || len(podSel) != 1 || podSel["app.kubernetes.io/component"] != "egress" ||
		get(to[0], "ipBlock") != nil {
		t.Errorf("peer %v", to[0])
	}
	ports := list(rules[0], "ports")
	if len(ports) != 1 || get(ports[0], "port") != 3128 || str(ports[0], "protocol") != "TCP" {
		t.Errorf("ports %v", ports)
	}
	// the base still cuts every run off, and the overlay's policy does not select a run that did not ask
	var base bool
	for _, b := range loadAll(t).byKind["NetworkPolicy"] {
		base = base || (ns(b) == runsNS && str(b, "metadata", "name") == "run-pods-no-network")
	}
	if !base {
		t.Error("the base policy of the runs namespace is gone")
	}
}

func TestEgressProxyTakesOnlyEgressRunsAndReachesOnlyPublicAddresses(t *testing.T) {
	docs := loadEgress(t, "networkpolicy.yaml")
	p := only(t, docs, "NetworkPolicy", svcNS)
	sel, _ := get(p, "spec", "podSelector", "matchLabels").(doc)
	if len(sel) != 1 || sel["app.kubernetes.io/component"] != "egress" {
		t.Fatalf("%s selects %v", name(p), sel)
	}
	if !eq(strs(list(p, "spec", "policyTypes")), []string{"Egress", "Ingress"}) {
		t.Errorf("policy types %v", get(p, "spec", "policyTypes"))
	}
	ing := list(p, "spec", "ingress")
	if len(ing) != 1 || len(list(ing[0], "from")) != 1 {
		t.Fatalf("ingress %v", ing)
	}
	from := list(ing[0], "from")[0]
	nsSel, _ := get(from, "namespaceSelector", "matchLabels").(doc)
	if len(nsSel) != 1 || nsSel["kubernetes.io/metadata.name"] != runsNS || !isEgressRun(get(from, "podSelector")) || get(from, "ipBlock") != nil {
		t.Errorf("ingress peer %v: every pod it admits could use the proxy", from)
	}
	if ports := list(ing[0], "ports"); len(ports) != 1 || get(ports[0], "port") != 3128 {
		t.Errorf("ingress ports %v", ports)
	}
	eg := list(p, "spec", "egress")
	if len(eg) != 1 || len(list(eg[0], "to")) != 1 {
		t.Fatalf("egress %v", eg)
	}
	to := list(eg[0], "to")[0]
	if get(to, "namespaceSelector") != nil || get(to, "podSelector") != nil || str(to, "ipBlock", "cidr") != "0.0.0.0/0" {
		t.Fatalf("egress peer %v", to)
	}
	var except []netip.Prefix
	for _, c := range list(to, "ipBlock", "except") {
		except = append(except, netip.MustParsePrefix(c.(string)))
	}
	excluded := func(addr netip.Addr) bool {
		for _, e := range except {
			if e.Contains(addr) {
				return true
			}
		}
		return false
	}
	// whatever the proxy itself refuses to connect to, the cluster refuses too, and nothing else (D22): every /24 of the
	// IPv4 space is compared (the smallest special-purpose block is a /24, 192.0.0.9 and 192.0.0.10 are refused with
	// their /24), so a block added to the proxy's table and not to the excepts is found, and so is an except that
	// cuts off public addresses
	for n := uint64(0); n < 1<<32; n += 256 {
		addr := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), 0})
		if refused := egress.Blocked(addr); refused != excluded(addr) {
			t.Fatalf("%s: the proxy refuses it: %v; the policy excludes it: %v", addr, refused, excluded(addr))
		}
	}
	for _, a := range []string{"0.0.0.0", "10.255.255.254", "100.127.255.254", "127.0.0.1", "169.254.169.254", "172.31.255.254", "192.0.0.9", "192.0.0.10",
		"192.168.255.254", "198.19.255.254", "239.255.255.250", "240.0.0.1", "255.255.255.255"} {
		if addr := netip.MustParseAddr(a); !excluded(addr) || !egress.Blocked(addr) {
			t.Errorf("%s: excluded %v, refused by the proxy %v", a, excluded(addr), egress.Blocked(addr))
		}
	}
	for _, a := range []string{"1.1.1.1", "93.184.216.34", "151.101.0.223", "192.31.196.1"} {
		if excluded(netip.MustParseAddr(a)) {
			t.Errorf("the public address %s is excluded", a)
		}
	}
	if len(list(eg[0], "ports")) == 0 {
		t.Error("the proxy may reach any port")
	}
	for _, port := range list(eg[0], "ports") {
		if pv := get(port, "port"); pv != 443 && pv != 80 {
			t.Errorf("egress port %v is not one of the proxy's defaults", pv)
		}
	}
}

func TestEgressProxyIsHardenedAndRequiresTheCredential(t *testing.T) {
	docs := loadEgress(t, "egress.yaml")
	sa := only(t, docs, "ServiceAccount", svcNS)
	dep := only(t, docs, "Deployment", svcNS)
	svc := only(t, docs, "Service", svcNS)
	pod := get(dep, "spec", "template", "spec")
	if get(sa, "automountServiceAccountToken") != false || get(pod, "automountServiceAccountToken") != false ||
		str(pod, "serviceAccountName") != str(sa, "metadata", "name") || str(sa, "metadata", "name") == "idp-sandbox" {
		t.Error("the proxy has, or shares, a credential for the Kubernetes API")
	}
	if get(pod, "securityContext", "runAsNonRoot") != true || str(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" ||
		get(pod, "hostNetwork") != nil || get(pod, "enableServiceLinks") != false {
		t.Errorf("pod %v", get(pod, "securityContext"))
	}
	cs := list(pod, "containers")
	if len(cs) != 1 {
		t.Fatalf("%d containers", len(cs))
	}
	c := cs[0]
	sc := get(c, "securityContext")
	if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true || !eq(strs(list(sc, "capabilities", "drop")), []string{"ALL"}) ||
		get(c, "resources", "limits", "memory") == nil || get(c, "resources", "limits", "cpu") == nil {
		t.Errorf("container %v", c)
	}
	env := envOf(c)
	if env["IDP_EGRESS_KEY_FILE"] == "" || env["IDP_EGRESS_TENANTS_FILE"] == "" {
		t.Errorf("the proxy would serve any pod that reaches it, as one: %v", env)
	}
	if _, ok := env["IDP_EGRESS_ALLOW_PRIVATE"]; ok {
		t.Error("IDP_EGRESS_ALLOW_PRIVATE is for tests")
	}
	mounts := map[string]string{}
	for _, m := range list(c, "volumeMounts") {
		if get(m, "readOnly") != true {
			t.Errorf("mount %v is writable", m)
		}
		mounts[str(m, "name")] = str(m, "mountPath")
	}
	var secret, tenants string
	for _, v := range list(pod, "volumes") {
		if s := str(v, "secret", "secretName"); s != "" {
			secret = s
			if !strings.HasPrefix(env["IDP_EGRESS_KEY_FILE"], mounts[str(v, "name")]+"/") {
				t.Errorf("the key file %s is not in the secret's mount %s", env["IDP_EGRESS_KEY_FILE"], mounts[str(v, "name")])
			}
		}
		if cm := str(v, "configMap", "name"); cm != "" {
			tenants = cm
			if !strings.HasPrefix(env["IDP_EGRESS_TENANTS_FILE"], mounts[str(v, "name")]+"/") {
				t.Errorf("the tenants file %s is not in the ConfigMap's mount", env["IDP_EGRESS_TENANTS_FILE"])
			}
		}
	}
	k := loadEgress(t, "kustomization.yaml")[0]
	gen := list(k, "configMapGenerator")
	if len(gen) != 1 || str(gen[0], "name") != tenants || str(gen[0], "namespace") != svcNS ||
		filepath.Base(env["IDP_EGRESS_TENANTS_FILE"]) != list(gen[0], "files")[0].(string) {
		t.Errorf("the tenants ConfigMap %q is not the one the kustomization generates: %v", tenants, gen)
	}
	// the Service the runs are sent to selects the proxy and nothing else of the namespace
	labels, _ := get(dep, "spec", "template", "metadata", "labels").(doc)
	selector, _ := get(svc, "spec", "selector").(doc)
	if len(selector) != 1 || selector["app.kubernetes.io/component"] != "egress" || labels["app.kubernetes.io/component"] != "egress" {
		t.Errorf("service selector %v, pod labels %v", selector, labels)
	}
	if ports := list(svc, "spec", "ports"); len(ports) != 1 || get(ports[0], "port") != 3128 {
		t.Errorf("service ports %v", ports)
	}

	// the sandbox service is told that Service, and signs with the same secret
	patch := loadEgress(t, "sandbox-patch.yaml")[0]
	if name(patch) != "Deployment/idp-sandbox" || ns(patch) != svcNS {
		t.Fatalf("the patch is for %s in %q", name(patch), ns(patch))
	}
	pc := list(patch, "spec", "template", "spec", "containers")[0]
	if str(pc, "name") != "sandbox" {
		t.Errorf("the patch is for container %q", str(pc, "name"))
	}
	penv := envOf(pc)
	if want := str(svc, "metadata", "name") + "." + svcNS + ".svc.cluster.local"; penv["IDP_SANDBOX_EGRESS_PROXY"] != want || penv["IDP_SANDBOX_EGRESS_PORT"] != "3128" {
		t.Errorf("the service is sent to %s:%s, the proxy is %s:3128", penv["IDP_SANDBOX_EGRESS_PROXY"], penv["IDP_SANDBOX_EGRESS_PORT"], want)
	}
	vols := list(patch, "spec", "template", "spec", "volumes")
	pm := list(pc, "volumeMounts")
	if len(vols) != 1 || len(pm) != 1 || str(vols[0], "secret", "secretName") != secret || secret == "" || get(pm[0], "readOnly") != true ||
		!strings.HasPrefix(penv["IDP_SANDBOX_EGRESS_KEY_FILE"], str(pm[0], "mountPath")+"/") ||
		filepath.Base(penv["IDP_SANDBOX_EGRESS_KEY_FILE"]) != filepath.Base(env["IDP_EGRESS_KEY_FILE"]) {
		t.Errorf("the service's key (%v, %v) is not the proxy's (%s)", vols, penv, secret)
	}
}

func TestShippedAllowListAllowsNothing(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(egressDir, "tenants.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def, tenants, err := egress.ParseTenants(b)
	if err != nil {
		t.Fatal(err)
	}
	if def == nil || len(def.Hosts) != 0 || len(tenants) != 0 {
		t.Errorf("as shipped the proxy allows %v and %v: an operator who applies the overlay unread opens that", def, tenants)
	}
}
