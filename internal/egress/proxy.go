// Package egress is the sandbox's allow-listed HTTP(S) forward proxy: the only way code in a sandbox can
// reach anything outside it, and only to the hosts the operator listed (ADR-0001 D12).
//
// It speaks CONNECT (HTTPS) and plain-HTTP forwarding. The decision is made on the host name the client asks
// for; the connection is then made to an address the proxy resolved itself and checked: loopback, private,
// link-local (cloud metadata), multicast and unspecified addresses are refused, so an allow-listed name that
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
	// AllowPrivate disables the address check. Tests only.
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

// Blocked reports an address the proxy must never connect to.
func Blocked(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() ||
		// carrier-grade NAT 100.64.0.0/10 and the cloud metadata address
		netip.MustParsePrefix("100.64.0.0/10").Contains(a) || a == netip.MustParseAddr("169.254.169.254")
}

// Proxy is the forward proxy.
type Proxy struct {
	Policy      Policy
	Log         *slog.Logger
	MaxConns    int           // concurrent tunnels (default 64)
	DialTimeout time.Duration // default 10 s
	MaxLifetime time.Duration // a tunnel is cut after this long (default 5 min)
	MaxBytes    int64         // per direction and tunnel (default 64 MiB)
	Resolver    interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}

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
	})
}

var errDenied = errors.New("denied")

// dial resolves host itself and connects to the first acceptable address.
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
	var last error = errDenied
	for _, a := range addrs {
		if !p.Policy.AllowPrivate && Blocked(a) {
			continue
		}
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
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
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	p.forward(w, r)
}

func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, host string, port int, why string) {
	p.Log.Info("egress", "decision", "deny", "host", host, "port", port, "method", r.Method, "why", why, "client", r.RemoteAddr)
	http.Error(w, "the sandbox may not reach "+host, http.StatusForbidden)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitHostPort(r.Host, "443")
	if err != nil || !p.Policy.Allowed(host, port) {
		p.deny(w, r, host, port, "not on the allow-list")
		return
	}
	up, err := p.dial(r.Context(), host, port)
	if err != nil {
		p.deny(w, r, host, port, "no acceptable address")
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
	p.Log.Info("egress", "decision", "allow", "host", host, "port", port, "method", "CONNECT", "bytesOut", out, "bytesIn", in, "ms", time.Since(start).Milliseconds())
}

var hopHeaders = []string{"Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "this is a proxy: use an absolute http:// URI or CONNECT", http.StatusBadRequest)
		return
	}
	host, port, err := splitHostPort(r.URL.Host, "80")
	if err != nil || !p.Policy.Allowed(host, port) {
		p.deny(w, r, host, port, "not on the allow-list")
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
		p.deny(w, r, host, port, "upstream failed")
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
	p.Log.Info("egress", "decision", "allow", "host", host, "port", port, "method", r.Method, "status", resp.StatusCode, "bytesIn", n)
}
