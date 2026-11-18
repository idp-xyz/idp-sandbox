package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPolicyMatching(t *testing.T) {
	p := Policy{Hosts: []string{"api.example.com", "*.pypi.example"}}
	for host, want := range map[string]bool{
		"api.example.com": true, "API.Example.com": true, "api.example.com.": true,
		"example.com": false, "evil-api.example.com": false, "api.example.com.evil.net": false,
		"files.pypi.example": true, "a.b.pypi.example": true, "pypi.example": false, "xpypi.example": false, "": false,
	} {
		if got := p.Allowed(host, 443); got != want {
			t.Errorf("%q: %v", host, got)
		}
	}
	if p.Allowed("api.example.com", 22) || p.Allowed("api.example.com", 8443) {
		t.Error("a port outside the list was allowed")
	}
	if !(Policy{Hosts: []string{"x"}, Ports: []int{8443}}).Allowed("x", 8443) {
		t.Error("a configured port was refused")
	}
	if (Policy{}).Allowed("anything", 443) {
		t.Error("an empty allow-list allowed something")
	}
}

func TestBlockedAddresses(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.9", "172.31.255.255", "192.168.1.1", "169.254.169.254", "169.254.0.1",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "fe80::1", "fc00::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::"} {
		if !Blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s was not blocked", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1"} {
		if Blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s was blocked", a)
		}
	}
}

type resolver map[string][]netip.Addr

func (r resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("no such host %s", host)
}

func serve(p *Proxy) *httptest.Server {
	p.Log = slog.New(slog.DiscardHandler)
	return httptest.NewServer(p)
}

func connect(t *testing.T, proxy, target string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	return resp, c, br
}

func TestConnectTunnelsToAnAllowedHost(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "from the origin") }))
	defer origin.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	var portN int
	fmt.Sscanf(port, "%d", &portN)
	ps := serve(&Proxy{Policy: Policy{Hosts: []string{"origin.test"}, Ports: []int{portN}, AllowPrivate: true},
		Resolver: resolver{"origin.test": {netip.MustParseAddr(host)}}})
	defer ps.Close()
	resp, c, br := connect(t, ps.URL, "origin.test:"+port)
	defer c.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: origin.test\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(br)
	if !strings.Contains(string(body), "from the origin") {
		t.Fatalf("%q", body)
	}
}

func TestDeniesWhatIsNotListedAndWhatResolvesInside(t *testing.T) {
	ps := serve(&Proxy{Policy: Policy{Hosts: []string{"ok.test", "rebind.test", "meta.test"}}, Resolver: resolver{
		"ok.test":     {netip.MustParseAddr("127.0.0.1")},                                  // an allowed name that points inside
		"rebind.test": {netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("127.0.0.1")}, // every answer is inside
		"meta.test":   {netip.MustParseAddr("169.254.169.254")},
		"evil.test":   {netip.MustParseAddr("1.1.1.1")}}})
	defer ps.Close()
	for _, target := range []string{"evil.test:443", "ok.test:443", "rebind.test:443", "meta.test:443", "169.254.169.254:80", "127.0.0.1:443", "[::1]:443", "ok.test:22", "localhost:443"} {
		resp, c, _ := connect(t, ps.URL, target)
		c.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("CONNECT %s -> %d, want 403", target, resp.StatusCode)
		}
	}
	// plain HTTP forwarding obeys the same rules
	req, _ := http.NewRequest("GET", "http://evil.test/", nil)
	proxyURL := ps.URL
	cl := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*urlT, error) { return parse(proxyURL) }}}
	resp, err := cl.Do(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("forward of an unlisted host: %v %v", resp, err)
	}
}

func TestForwardsPlainHTTPToAnAllowedHost(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello ", r.Host, " ", r.Header.Get("Proxy-Authorization") == "")
	}))
	defer origin.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	var portN int
	fmt.Sscanf(port, "%d", &portN)
	ps := serve(&Proxy{Policy: Policy{Hosts: []string{"origin.test"}, Ports: []int{portN}, AllowPrivate: true}, Resolver: resolver{"origin.test": {netip.MustParseAddr(host)}}})
	defer ps.Close()
	cl := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*urlT, error) { return parse(ps.URL) }}}
	resp, err := cl.Get("http://origin.test:" + port + "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hello origin.test:"+port) {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
}

func TestConnectionLimit(t *testing.T) {
	p := &Proxy{MaxConns: 1, Policy: Policy{Hosts: []string{"x"}}}
	p.init()
	p.active.Store(1)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "http://x/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", w.Code)
	}
}

type urlT = url.URL

func parse(s string) (*urlT, error) { return url.Parse(s) }

func urlUserPassword(user, password string) *url.Userinfo { return url.UserPassword(user, password) }

// connectAs is connect with the credential of a run as curl, Python and Go send it: Proxy-Authorization: Basic run:<credential>.
func connectAs(t *testing.T, proxy, target, credential string) *http.Response {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	auth := ""
	if credential != "" {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(User+":"+credential)) + "\r\n"
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, auth)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// With the key every run says its tenant, and each tenant reaches its own list: a tenant that is named gets nothing of the
// default, a tenant named with no hosts gets nothing at all, and a request without a good credential gets 407 (D21).
func TestEachTenantReachesItsOwnAllowList(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "from ", r.Host, " ", r.Header.Get("Proxy-Authorization") == "")
	}))
	defer origin.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	var portN int
	fmt.Sscanf(port, "%d", &portN)
	addr := netip.MustParseAddr(host)
	var logged strings.Builder
	p := &Proxy{Key: testKey, Policy: Policy{Hosts: []string{"shared.test"}, Ports: []int{portN}, AllowPrivate: true},
		Tenants:  map[string]Policy{"acme": {Hosts: []string{"acme.test"}, Ports: []int{portN}}, "globex": {}},
		Resolver: resolver{"shared.test": {addr}, "acme.test": {addr}}}
	ps := httptest.NewServer(p)
	defer ps.Close()
	p.Log = slog.New(slog.NewTextHandler(&logged, nil))
	local := netip.MustParseAddrPort(strings.TrimPrefix(ps.URL, "http://")).Addr().String()
	mint := func(c Claims) string {
		t.Helper()
		if c.Proxy == "" {
			c.Proxy = local
		}
		if c.Expires == 0 {
			c.Expires = time.Now().Add(time.Minute).Unix()
		}
		s, err := Mint(testKey, c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	acme, initech, globex, none := mint(Claims{Tenant: "acme", Workspace: "aaaa1111"}), mint(Claims{Tenant: "initech"}), mint(Claims{Tenant: "globex"}), mint(Claims{})
	for _, c := range []struct {
		who, cred, target string
		want              int
	}{
		{"acme, its own host", acme, "acme.test:" + port, 200},
		{"acme, the default's host", acme, "shared.test:" + port, 403},
		{"a tenant without a list, the default's host", initech, "shared.test:" + port, 200},
		{"a tenant without a list, acme's host", initech, "acme.test:" + port, 403},
		{"a caller without a tenant, the default's host", none, "shared.test:" + port, 200},
		{"a tenant named with nothing", globex, "shared.test:" + port, 403},
		{"no credential", "", "shared.test:" + port, 407},
		{"a password that is not a credential", "hunter2", "shared.test:" + port, 407},
		{"an expired credential", mint(Claims{Tenant: "acme", Expires: time.Now().Add(-time.Second).Unix()}), "acme.test:" + port, 407},
		{"a credential of another run's network", mint(Claims{Tenant: "acme", Proxy: "172.30.0.2"}), "acme.test:" + port, 407},
	} {
		resp := connectAs(t, ps.URL, c.target, c.cred)
		if resp.StatusCode != c.want {
			t.Errorf("%s: CONNECT %s -> %d, want %d", c.who, c.target, resp.StatusCode, c.want)
		}
		if c.want == 407 && !strings.HasPrefix(resp.Header.Get("Proxy-Authenticate"), "Basic") {
			t.Errorf("%s: no Proxy-Authenticate", c.who)
		}
	}
	// plain HTTP through a client that takes the credential from the proxy URL, as HTTP_PROXY gives it to a run; the
	// credential stops at the proxy
	get := func(cred, url string) (int, string) {
		t.Helper()
		pu, _ := parse(ps.URL)
		pu.User = urlUserPassword(User, cred)
		cl := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*urlT, error) { return pu, nil }}}
		resp, err := cl.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(acme, "http://acme.test:"+port+"/"); code != 200 || body != "from acme.test:"+port+" true" {
		t.Errorf("acme over plain HTTP: %d %q", code, body)
	}
	if code, _ := get(initech, "http://acme.test:"+port+"/"); code != 403 {
		t.Errorf("another tenant on acme's host over plain HTTP: %d", code)
	}
	if code, _ := get("nonsense", "http://shared.test:"+port+"/"); code != 407 {
		t.Errorf("a bad credential over plain HTTP: %d", code)
	}
	if l := logged.String(); !strings.Contains(l, "tenant=acme") || !strings.Contains(l, "workspace=aaaa1111") || strings.Contains(l, acme) {
		t.Errorf("the log names the tenant and the workspace of a decision, and never the credential:\n%s", l)
	}
}

// Without the key nothing is asked of a run and the tenants' lists are not used: the proxy cannot know who asks.
func TestWithoutAKeyEveryRunGetsTheOneList(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	var portN int
	fmt.Sscanf(port, "%d", &portN)
	addr := netip.MustParseAddr(host)
	ps := serve(&Proxy{Policy: Policy{Hosts: []string{"shared.test"}, Ports: []int{portN}, AllowPrivate: true},
		Tenants:  map[string]Policy{"": {Hosts: []string{"other.test"}, Ports: []int{portN}}},
		Resolver: resolver{"shared.test": {addr}, "other.test": {addr}}})
	defer ps.Close()
	if resp := connectAs(t, ps.URL, "shared.test:"+port, ""); resp.StatusCode != 200 {
		t.Errorf("the one list: %d", resp.StatusCode)
	}
	if resp := connectAs(t, ps.URL, "other.test:"+port, ""); resp.StatusCode != 403 {
		t.Errorf("a tenant's list applied without a credential: %d", resp.StatusCode)
	}
}
