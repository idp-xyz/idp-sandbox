package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// raceDial is a dial function whose behaviour per address a test writes, and which records what it did.
type raceDial struct {
	mu        sync.Mutex
	asked     []string
	cancelled []string
	open      atomic.Int64 // connections made and not closed
	behave    map[string]func(ctx context.Context) (net.Conn, error)
}

func (d *raceDial) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, address)
	b := d.behave[address]
	d.mu.Unlock()
	if network != "tcp" || b == nil {
		return nil, errors.New("unexpected dial " + address)
	}
	c, err := b(ctx)
	if ctx.Err() != nil {
		d.mu.Lock()
		d.cancelled = append(d.cancelled, address)
		d.mu.Unlock()
	}
	return c, err
}

func (d *raceDial) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

type countedConn struct {
	net.Conn
	d    *raceDial
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.d.open.Add(-1) })
	return c.Conn.Close()
}

// connects returns a connection (one end of a pipe) after wait, or the context's error if it is cancelled first.
func (d *raceDial) connects(wait time.Duration) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		select {
		case <-time.After(wait):
			a, b := net.Pipe()
			_ = b
			d.open.Add(1)
			return &countedConn{Conn: a, d: d}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ignoresCancel connects after wait whatever the context says: a dial that completes late.
func (d *raceDial) ignoresCancel(wait time.Duration) func(ctx context.Context) (net.Conn, error) {
	return func(context.Context) (net.Conn, error) {
		time.Sleep(wait)
		a, _ := net.Pipe()
		d.open.Add(1)
		return &countedConn{Conn: a, d: d}, nil
	}
}

func fails(msg string) func(ctx context.Context) (net.Conn, error) {
	return func(context.Context) (net.Conn, error) { return nil, errors.New(msg) }
}

func ips(s ...string) []netip.Addr {
	var out []netip.Addr
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

const (
	v6a = "2600:1417::1"
	v6b = "2600:1417::2"
	v4a = "93.184.216.34"
	v4b = "93.184.216.35"
)

func addrOf(ip string) string { return netip.AddrPortFrom(netip.MustParseAddr(ip), 443).String() }

func TestHangingFirstAddressIsCancelledAndTheSecondConnectsAfterTheAttemptDelay(t *testing.T) {
	d := &raceDial{}
	d.behave = map[string]func(context.Context) (net.Conn, error){
		addrOf(v6a): func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() },
		addrOf(v4a): d.connects(0),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	c, err := dialRace(ctx, d.dial, Dual, ips(v6a, v4a), 443)
	took := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if took < connectionAttemptDelay-20*time.Millisecond || took > connectionAttemptDelay+time.Second {
		t.Errorf("connected after %v, want about %v", took, connectionAttemptDelay)
	}
	if got := d.list(); !reflect.DeepEqual(got, []string{addrOf(v6a), addrOf(v4a)}) {
		t.Errorf("dialed %v", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		n := len(d.cancelled)
		d.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("the hanging attempt was not cancelled: %v", d.cancelled)
}

func TestAnAttemptThatFailsStartsTheNextAtOnce(t *testing.T) {
	d := &raceDial{}
	d.behave = map[string]func(context.Context) (net.Conn, error){
		addrOf(v6a): fails("network unreachable"),
		addrOf(v4a): d.connects(0),
	}
	t0 := time.Now()
	c, err := dialRace(context.Background(), d.dial, Dual, ips(v6a, v4a), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if took := time.Since(t0); took > connectionAttemptDelay/2 {
		t.Errorf("took %v: the second address waited for the delay", took)
	}
}

func TestAConnectionThatCompletesAfterTheRaceIsClosed(t *testing.T) {
	d := &raceDial{}
	d.behave = map[string]func(context.Context) (net.Conn, error){
		addrOf(v6a): d.ignoresCancel(connectionAttemptDelay + 300*time.Millisecond), // wins nothing, connects late
		addrOf(v4a): d.connects(0),
	}
	c, err := dialRace(context.Background(), d.dial, Dual, ips(v6a, v4a), 443)
	if err != nil {
		t.Fatal(err)
	}
	if n := d.open.Load(); n != 1 {
		t.Fatalf("%d connections open right after the race, want the winner", n)
	}
	// wait until the late attempt has connected (and been closed)
	time.Sleep(connectionAttemptDelay + 600*time.Millisecond)
	if n := d.open.Load(); n != 1 {
		t.Errorf("%d connections open, want only the winner: the late one leaked", n)
	}
	c.Close()
	if n := d.open.Load(); n != 0 {
		t.Errorf("%d connections open after closing the winner", n)
	}
}

func TestInterleavingStartsWithTheFamilyOfTheFirstAddressAndKeepsTheResolversOrder(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{v6a, v6b, v4a, v4b}, []string{v6a, v4a, v6b, v4b}},
		{[]string{v4a, v6a, v6b, v4b}, []string{v4a, v6a, v4b, v6b}},
		{[]string{v6a, v6b}, []string{v6a, v6b}},
		{[]string{v4a, v4b, v6a}, []string{v4a, v6a, v4b}},
		{[]string{"::ffff:93.184.216.34", v6a}, []string{v4a, v6a}},
	} {
		var got []string
		for _, ip := range Dual.order(ips(tc.in...)) {
			got = append(got, ip.String())
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: %v, want %v", tc.in, got, tc.want)
		}
	}
	// and it is the order of the dials
	d := &raceDial{behave: map[string]func(context.Context) (net.Conn, error){}}
	for _, a := range []string{v6a, v6b, v4a, v4b} {
		d.behave[addrOf(a)] = fails("no")
	}
	if _, err := dialRace(context.Background(), d.dial, Dual, ips(v6a, v6b, v4a, v4b), 443); err == nil {
		t.Fatal("no error")
	}
	if got, want := d.list(), []string{addrOf(v6a), addrOf(v4a), addrOf(v6b), addrOf(v4b)}; !reflect.DeepEqual(got, want) {
		t.Errorf("dialed %v, want %v", got, want)
	}
}

func TestWhenEveryAttemptFailsTheErrorNamesTheLastWithinTheDeadline(t *testing.T) {
	d := &raceDial{}
	d.behave = map[string]func(context.Context) (net.Conn, error){
		addrOf(v6a): fails("first"),
		addrOf(v4a): fails("second"),
	}
	_, err := dialRace(context.Background(), d.dial, Dual, ips(v6a, v4a), 443)
	if err == nil || !strings.Contains(err.Error(), "second") {
		t.Errorf("error %v, want the last", err)
	}
	// hanging attempts end with the overall deadline, not later
	d = &raceDial{}
	hang := func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	d.behave = map[string]func(context.Context) (net.Conn, error){addrOf(v6a): hang, addrOf(v4a): hang}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err = dialRace(ctx, d.dial, Dual, ips(v6a, v4a), 443)
	if err == nil {
		t.Fatal("no error")
	}
	if took := time.Since(t0); took > 600*time.Millisecond+500*time.Millisecond {
		t.Errorf("took %v, past the deadline", took)
	}
}

func TestOnlyTheGivenAddressesAreDialedOnTheGivenPort(t *testing.T) {
	d := &raceDial{}
	d.behave = map[string]func(context.Context) (net.Conn, error){
		addrOf(v6a): fails("x"), addrOf(v4a): fails("x"), addrOf(v4b): fails("x"),
	}
	_, _ = dialRace(context.Background(), d.dial, Dual, ips(v6a, v4a, v4b), 443)
	want := map[string]bool{addrOf(v6a): true, addrOf(v4a): true, addrOf(v4b): true}
	got := d.list()
	if len(got) != 3 {
		t.Errorf("dialed %v", got)
	}
	for _, a := range got {
		if !want[a] {
			t.Errorf("dialed %s, which was not given", a)
		}
	}
	if _, err := dialRace(context.Background(), d.dial, Dual, nil, 443); err == nil {
		t.Error("no address, no error")
	}
}

// Through the proxy: the first allowed address never answers, the second is a local server; a blocked address among the
// resolved ones is skipped as before and never dialed. The tunnel works through the second address and the first
// attempt is cancelled.
func TestProxyTunnelsThroughTheSecondAddressWhenTheFirstDoesNotAnswer(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "from the origin") }))
	defer origin.Close()
	d := &raceDial{}
	var cancelled atomic.Int64
	p := &Proxy{Policy: Policy{Hosts: []string{"dual.test"}}, Resolver: resolver{
		"dual.test": {netip.MustParseAddr("2606:4700::1"), netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("93.184.216.34")}}}
	p.DialFamily = Dual // the hang is the first attempt
	p.dialFn = func(ctx context.Context, network, address string) (net.Conn, error) {
		d.mu.Lock()
		d.asked = append(d.asked, address)
		d.mu.Unlock()
		if strings.HasPrefix(address, "[2606:4700::1]") {
			<-ctx.Done()
			cancelled.Add(1)
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}
	ps := serve(p)
	defer ps.Close()
	t0 := time.Now()
	resp, c, br := connect(t, ps.URL, "dual.test:443")
	defer c.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if took := time.Since(t0); took > 3*time.Second {
		t.Errorf("took %v", took)
	}
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: dual.test\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(br)
	if !strings.Contains(string(body), "from the origin") {
		t.Fatalf("%q", body)
	}
	for _, a := range d.list() {
		if a != "[2606:4700::1]:443" && a != "93.184.216.34:443" {
			t.Errorf("dialed %s, which is blocked or was not resolved", a)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cancelled.Load() == 0 {
		t.Error("the hanging attempt was not cancelled")
	}
}

// Only blocked addresses: refused, nothing is dialed.
func TestProxyDialsNothingWhenEveryAddressIsBlocked(t *testing.T) {
	d := &raceDial{}
	p := &Proxy{Policy: Policy{Hosts: []string{"in.test"}}, Resolver: resolver{
		"in.test": {netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("127.0.0.1")}}}
	p.dialFn = d.dial
	p.init()
	if _, err := p.dial(context.Background(), "in.test", 443); err != errDenied {
		t.Errorf("error %v, want denied", err)
	}
	if len(d.list()) != 0 {
		t.Errorf("dialed %v", d.list())
	}
}

func TestEachFamilySettingGivesItsOrder(t *testing.T) {
	for _, tc := range []struct {
		f    DialFamily
		in   []string
		want []string
	}{
		{"", []string{v6a, v6b, v4a, v4b}, []string{v4a, v6a, v4b, v6b}}, // the zero value is the default
		{PreferIPv4, []string{v6a, v6b, v4a, v4b}, []string{v4a, v6a, v4b, v6b}},
		{PreferIPv4, []string{v6a, v4a, v4b}, []string{v4a, v6a, v4b}},
		{Dual, []string{v6a, v6b, v4a, v4b}, []string{v6a, v4a, v6b, v4b}},
		{Dual, []string{v4a, v6a, v6b}, []string{v4a, v6a, v6b}},
		{IPv4Only, []string{v6a, v4a, v6b, v4b}, []string{v4a, v4b}},
		{IPv6Only, []string{v6a, v4a, v6b, v4b}, []string{v6a, v6b}},
	} {
		var got []string
		for _, ip := range tc.f.order(ips(tc.in...)) {
			got = append(got, ip.String())
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q %v: %v, want %v", tc.f, tc.in, got, tc.want)
		}
	}
}

func TestAFamilyWithNoAddressIsAnErrorWithoutDialing(t *testing.T) {
	d := &raceDial{behave: map[string]func(context.Context) (net.Conn, error){addrOf(v6a): d0(), addrOf(v4a): d0()}}
	for _, tc := range []struct {
		f  DialFamily
		in []string
	}{{IPv4Only, []string{v6a, v6b}}, {IPv6Only, []string{v4a}}} {
		if _, err := dialRace(context.Background(), d.dial, tc.f, ips(tc.in...), 443); err == nil || !strings.Contains(err.Error(), "no address") {
			t.Errorf("%s with %v: %v", tc.f, tc.in, err)
		}
	}
	if len(d.list()) != 0 {
		t.Errorf("dialed %v", d.list())
	}
}

func d0() func(context.Context) (net.Conn, error) { return fails("must not be dialed") }

func TestDialFamilySettingIsValidated(t *testing.T) {
	for s, want := range map[string]DialFamily{"": PreferIPv4, "prefer-ipv4": PreferIPv4, "dual": Dual, "ipv4-only": IPv4Only, "ipv6-only": IPv6Only} {
		if got, err := ParseDialFamily(s); err != nil || got != want {
			t.Errorf("%q: %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"ipv4", "IPv4-only", "both", " dual", "prefer-ipv6"} {
		if _, err := ParseDialFamily(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

// An IPv6 path that connects and then says nothing, ahead of IPv4 in the resolver's answer: with the default the IPv4
// attempt starts first and wins, and IPv6 is never dialed at all.
func TestPreferIPv4NeverLetsAStallingIPv6PathWin(t *testing.T) {
	d := &raceDial{}
	d.behave = map[string]func(context.Context) (net.Conn, error){
		addrOf(v6a): d.connects(0), // connects at once, and a caller would then wait for data that never comes
		addrOf(v4a): d.connects(20 * time.Millisecond),
	}
	c, err := dialRace(context.Background(), d.dial, "", ips(v6a, v4a), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := d.list(); !reflect.DeepEqual(got, []string{addrOf(v4a)}) {
		t.Errorf("dialed %v, want only IPv4", got)
	}
}

// With the default (prefer-ipv4) a path whose IPv6 connects and then stalls is never used: the tunnel goes over IPv4 and
// no IPv6 address is dialed.
func TestProxyUsesIPv4WhenIPv6ConnectsAndThenStalls(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "from the origin") }))
	defer origin.Close()
	d := &raceDial{}
	p := &Proxy{Policy: Policy{Hosts: []string{"stall.test"}}, Resolver: resolver{
		"stall.test": {netip.MustParseAddr("2606:4700::1"), netip.MustParseAddr("93.184.216.34")}}}
	p.dialFn = func(ctx context.Context, network, address string) (net.Conn, error) {
		d.mu.Lock()
		d.asked = append(d.asked, address)
		d.mu.Unlock()
		if strings.HasPrefix(address, "[") {
			a, b := net.Pipe()
			t.Cleanup(func() { a.Close(); b.Close() })
			return a, nil
		}
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}
	ps := serve(p)
	defer ps.Close()
	resp, c, br := connect(t, ps.URL, "stall.test:443")
	defer c.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: stall.test\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(br)
	if !strings.Contains(string(body), "from the origin") {
		t.Fatalf("%q", body)
	}
	for _, a := range d.list() {
		if strings.HasPrefix(a, "[") {
			t.Errorf("dialed %s", a)
		}
	}
}
