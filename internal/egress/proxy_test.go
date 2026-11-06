package egress

import (
	"bufio"
	"context"
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
