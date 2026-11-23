// Package egress is the sandbox's allow-listed HTTP(S) forward proxy: the only way code in a sandbox can
// reach anything outside it, and only to the hosts the operator listed (ADR-0001 D13), for the tenant the run belongs
// to when the operator keeps lists per tenant (D21).
//
// It speaks CONNECT (HTTPS) and plain-HTTP forwarding. The decision is made on the host name the client asks
// for; the connection is then made to an address the proxy resolved itself and checked: loopback, private,
// link-local (cloud metadata), multicast and every other address the IANA special-purpose registries mark as not globally
// reachable are refused (D22), so an allow-listed name that
// resolves to the inside of the operator's network, or a DNS rebinding answer, cannot be used to get there.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Policy decides who may be reached.
type Policy struct {
	// Hosts are exact names ("api.example.com") or suffix wildcards ("*.example.com", which does not match
	// "example.com" itself). Matching is case-insensitive on the name without port.
	Hosts []string
	// Ports the proxy connects to (default 443 and 80).
	Ports []int
	// AllowPrivate disables the address check. Tests only. It is the proxy's (Proxy.Policy), not a tenant's.
	AllowPrivate bool
}

// Allowed reports whether host:port may be requested.
func (p Policy) Allowed(host string, port int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	ports := p.Ports
	if len(ports) == 0 {
		ports = []int{443, 80}
	}
	portOK := false
	for _, x := range ports {
		portOK = portOK || x == port
	}
	if !portOK {
		return false
	}
	for _, h := range p.Hosts {
		h = strings.ToLower(h)
		if strings.HasPrefix(h, "*.") {
			if strings.HasSuffix(host, h[1:]) && len(host) > len(h)-1 {
				return true
			}
			continue
		}
		if host == h {
			return true
		}
	}
	return false
}

// notGloballyReachable is every block the proxy refuses to connect to (D22). It is not a list someone remembered: it
// is the IANA IPv4 Special-Purpose Address Registry and the IANA IPv6 Special-Purpose Address Registry, as of
// 2026-11-19, every entry with "Globally Reachable: False" (or "N/A"), plus the blocks that are not in those tables
// but are not unicast destinations either (multicast, the reserved 224.0.0.0/3, site-local fec0::/10). A review found
// that the earlier list missed most of 0.0.0.0/8, 240.0.0.0/4, 198.18.0.0/15, 192.0.0.0/24, the documentation
// networks and every IPv6 block that carries an IPv4 address.
//
// A block that has small globally reachable exceptions is refused whole: 192.0.0.0/24 (192.0.0.9 and 192.0.0.10) and
// 2001::/23 (PCP and TURN anycast, AMT, AS112, drone remote ID). Nothing a run should reach lives there, and one rule
// without holes is easier to check than a rule with them. The other globally reachable special-purpose blocks
// (192.31.196.0/24, 192.52.193.0/24, 192.175.48.0/24, 2620:4f:8000::/48) are left alone: they are public anycast.
//
// IPv6 blocks that embed an IPv4 address (IPv4-mapped, SIIT and NAT64, 6to4, Teredo) are refused whole, for the
// address they would reach, whatever it is: 64:ff9b::7f00:1 and 2002:7f00:1:: reach 127.0.0.1 where such translation
// exists. EmbeddedIPv4 shows the address.
var notGloballyReachable = func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range []string{
		// IPv4 Special-Purpose Address Registry
		"0.0.0.0/8",       // "this network" (RFC 791)
		"10.0.0.0/8",      // private use (RFC 1918)
		"100.64.0.0/10",   // shared address space, carrier-grade NAT (RFC 6598)
		"127.0.0.0/8",     // loopback (RFC 1122)
		"169.254.0.0/16",  // link local, which holds the cloud metadata address 169.254.169.254 (RFC 3927)
		"172.16.0.0/12",   // private use (RFC 1918)
		"192.0.0.0/24",    // IETF protocol assignments, DS-Lite, NAT64 discovery, whole (RFC 6890)
		"192.0.2.0/24",    // documentation TEST-NET-1 (RFC 5737)
		"192.88.99.0/24",  // deprecated 6to4 relay anycast (RFC 7526); the registry marks it deprecated, refused as a transition block
		"192.168.0.0/16",  // private use (RFC 1918)
		"198.18.0.0/15",   // benchmarking (RFC 2544)
		"198.51.100.0/24", // documentation TEST-NET-2 (RFC 5737)
		"203.0.113.0/24",  // documentation TEST-NET-3 (RFC 5737)
		"224.0.0.0/3",     // multicast 224.0.0.0/4 and reserved 240.0.0.0/4 with limited broadcast 255.255.255.255 (RFC 1112, RFC 919)
		// IPv6 Special-Purpose Address Registry
		"::/8",          // reserved by IETF: unspecified ::, loopback ::1, IPv4-compatible ::/96, IPv4-mapped ::ffff:0:0/96, SIIT ::ffff:0:0:0/96, NAT64 64:ff9b::/96 and 64:ff9b:1::/48
		"100::/64",      // discard-only (RFC 6666)
		"2001::/23",     // IETF protocol assignments, whole: Teredo 2001::/32, benchmarking 2001:2::/48, ORCHID, PCP, AMT, AS112 (RFC 6890)
		"2001:db8::/32", // documentation (RFC 3849)
		"2002::/16",     // 6to4 (RFC 3056)
		"3fff::/20",     // documentation (RFC 9637)
		"5f00::/16",     // segment routing SIDs (RFC 9602)
		"fc00::/7",      // unique local (RFC 4193)
		"fe80::/10",     // link local (RFC 4291)
		"fec0::/10",     // deprecated site-local (RFC 3879)
		"ff00::/8",      // multicast (RFC 4291)
	} {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

// EmbeddedIPv4 returns the IPv4 address an IPv6 address would reach through a translation mechanism: the IPv4-mapped
// ::ffff:a.b.c.d, NAT64 64:ff9b::/96 and 64:ff9b:1::/48 (RFC 6052, RFC 8215), 6to4 2002::/16 and the client address of
// Teredo 2001::/32. The check on those blocks does not depend on it (Blocked refuses them whole); it shows what each
// would have reached.
func EmbeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	a = a.WithZone("") // Prefix.Contains is false for a zoned address
	if a.Is4In6() {
		return a.Unmap(), true
	}
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case netip.MustParsePrefix("64:ff9b::/96").Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case netip.MustParsePrefix("64:ff9b:1::/48").Contains(a): // the /48 layout of RFC 6052: bits 64 to 71 are zero
		return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
	case netip.MustParsePrefix("2002::/16").Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case netip.MustParsePrefix("2001::/32").Contains(a): // the client's address is stored inverted
		return netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}), true
	}
	return netip.Addr{}, false
}

// Blocked reports an address the proxy must never connect to: one that is not globally reachable (D22). An
// IPv4-mapped address is judged as the IPv4 address it carries.
func Blocked(a netip.Addr) bool {
	a = a.Unmap()
	// A zone names a local interface and Prefix.Contains is false for a zoned address, so a zoned address would pass
	// every entry of the table: refuse it outright.
	if !a.IsValid() || a.Zone() != "" {
		return true
	}
	for _, p := range notGloballyReachable {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Proxy is the forward proxy.
type Proxy struct {
	// Policy is the allow-list of every run, or with Tenants of the runs whose tenant has no list of its own.
	Policy Policy
	// Key, when set, makes every request present the credential the sandbox service made for the run (credential.go):
	// without a valid one the answer is 407. It says which tenant the run belongs to.
	Key []byte
	// Tenants are the allow-lists of the tenants that have their own (D21). A tenant in it gets nothing of Policy.
	Tenants map[string]Policy

	Log         *slog.Logger
	MaxConns    int           // concurrent tunnels (default 64)
	DialTimeout time.Duration // default 10 s
	DialFamily  DialFamily    // which address families are dialed, and which first (IDP_EGRESS_DIAL_FAMILY, ADR-0001 D24); default prefer-ipv4
	MaxLifetime time.Duration // a tunnel is cut after this long (default 5 min)
	MaxBytes    int64         // per direction and tunnel (default 64 MiB)
	Resolver    interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}

	now    func() time.Time                                                     // tests
	dialFn func(ctx context.Context, network, address string) (net.Conn, error) // tests: replaces the TCP dial after the check
	active atomic.Int64
	once   sync.Once
}

func (p *Proxy) init() {
	p.once.Do(func() {
		if p.Log == nil {
			p.Log = slog.Default()
		}
		if p.MaxConns == 0 {
			p.MaxConns = 64
		}
		if p.DialTimeout == 0 {
			p.DialTimeout = 10 * time.Second
		}
		if p.MaxLifetime == 0 {
			p.MaxLifetime = 5 * time.Minute
		}
		if p.MaxBytes == 0 {
			p.MaxBytes = 64 << 20
		}
		if p.Resolver == nil {
			p.Resolver = net.DefaultResolver
		}
		if p.now == nil {
			p.now = time.Now
		}
	})
}

var errDenied = errors.New("denied")

// dial resolves host itself and connects to one of the acceptable addresses (dialRace: Happy Eyeballs, ADR-0001 D24).
func (p *Proxy) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		var err error
		if addrs, err = p.Resolver.LookupNetIP(ctx, "ip", host); err != nil {
			return nil, fmt.Errorf("resolve: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, p.DialTimeout)
	defer cancel()
	var allowed []netip.Addr
	for _, a := range addrs {
		if p.Policy.AllowPrivate || !Blocked(a) {
			allowed = append(allowed, a)
		}
	}
	if len(allowed) == 0 {
		return nil, errDenied
	}
	dial := p.dialFn
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return dialRace(ctx, dial, p.DialFamily, allowed, port)
}

func splitHostPort(hostport, defaultPort string) (string, int, error) {
	if !strings.Contains(hostport, ":") || strings.HasSuffix(hostport, "]") {
		hostport = strings.Trim(hostport, "[]") + ":" + defaultPort
	}
	h, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", 0, err
	}
	var port int
	if _, err := fmt.Sscanf(ps, "%d", &port); err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("bad port")
	}
	return h, port, nil
}

// ServeHTTP handles CONNECT and absolute-URI forwarding.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	if p.active.Add(1) > int64(p.MaxConns) {
		p.active.Add(-1)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer p.active.Add(-1)
	who, ok := p.authenticate(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r, who)
		return
	}
	p.forward(w, r, who)
}

// authenticate finds out which run is asking. Without a key every run is the same and nothing is asked of it.
func (p *Proxy) authenticate(w http.ResponseWriter, r *http.Request) (Claims, bool) {
	if len(p.Key) == 0 {
		return Claims{}, true
	}
	// the credential is the password of Proxy-Authorization: Basic; BasicAuth parses the Authorization header
	_, credential, _ := (&http.Request{Header: http.Header{"Authorization": r.Header["Proxy-Authorization"]}}).BasicAuth()
	var local netip.Addr
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if ap, err := netip.ParseAddrPort(a.String()); err == nil {
			local = ap.Addr()
		}
	}
	who, err := Verify(p.Key, credential, p.now(), local)
	if err != nil {
		p.Log.Info("egress", "decision", "deny", "host", r.Host, "method", r.Method, "why", err.Error(), "client", r.RemoteAddr)
		w.Header().Set("Proxy-Authenticate", `Basic realm="idp-sandbox-egress"`)
		http.Error(w, "this proxy serves sandbox runs: "+err.Error(), http.StatusProxyAuthRequired)
		return Claims{}, false
	}
	return who, true
}

// policyFor is the allow-list of the run's tenant: its own, else the default.
func (p *Proxy) policyFor(who Claims) Policy {
	if len(p.Key) == 0 { // nobody said who they are
		return p.Policy
	}
	if t, ok := p.Tenants[who.Tenant]; ok {
		return t
	}
	return p.Policy
}

// run is what the log says about who asked (nothing without a key: every run is the same then).
func (p *Proxy) run(who Claims) []any {
	if len(p.Key) == 0 {
		return nil
	}
	return []any{"tenant", who.Tenant, "workspace", who.Workspace}
}

func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, who Claims, host string, port int, why string) {
	p.Log.Info("egress", append([]any{"decision", "deny", "host", host, "port", port, "method", r.Method, "why", why, "client", r.RemoteAddr}, p.run(who)...)...)
	http.Error(w, "the sandbox may not reach "+host, http.StatusForbidden)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, who Claims) {
	host, port, err := splitHostPort(r.Host, "443")
	if err != nil || !p.policyFor(who).Allowed(host, port) {
		p.deny(w, r, who, host, port, "not on the allow-list")
		return
	}
	up, err := p.dial(r.Context(), host, port)
	if err != nil {
		p.deny(w, r, who, host, port, "no acceptable address")
		return
	}
	defer up.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	c, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	start := time.Now()
	deadline := time.Now().Add(p.MaxLifetime)
	_ = c.SetDeadline(deadline)
	_ = up.SetDeadline(deadline)
	var in, out int64
	done := make(chan struct{}, 2)
	go func() { // client -> origin; the client finishing sending is a half-close, the origin may still answer
		out, _ = io.Copy(up, io.LimitReader(c, p.MaxBytes))
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() { // origin -> client; the origin finishing ends the whole tunnel
		in, _ = io.Copy(c, io.LimitReader(up, p.MaxBytes))
		_ = c.Close()
		_ = up.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
	p.Log.Info("egress", append([]any{"decision", "allow", "host", host, "port", port, "method", "CONNECT", "bytesOut", out, "bytesIn", in, "ms", time.Since(start).Milliseconds()}, p.run(who)...)...)
}

var hopHeaders = []string{"Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, who Claims) {
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "this is a proxy: use an absolute http:// URI or CONNECT", http.StatusBadRequest)
		return
	}
	host, port, err := splitHostPort(r.URL.Host, "80")
	if err != nil || !p.policyFor(who).Allowed(host, port) {
		p.deny(w, r, who, host, port, "not on the allow-list")
		return
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		h, po, err := splitHostPort(addr, "80")
		if err != nil {
			return nil, err
		}
		return p.dial(ctx, h, po)
	}, DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		p.deny(w, r, who, host, port, "upstream failed")
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, io.LimitReader(resp.Body, p.MaxBytes))
	p.Log.Info("egress", append([]any{"decision", "allow", "host", host, "port", port, "method", r.Method, "status", resp.StatusCode, "bytesIn", n}, p.run(who)...)...)
}
