package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// connectionAttemptDelay is how long an attempt may be neither connected nor failed before the next address is tried
// alongside it (RFC 8305 section 5, "Connection Attempt Delay"; the RFC's recommended value).
const connectionAttemptDelay = 250 * time.Millisecond

// DialFamily is which address families a checked dial uses, and which goes first. The zero value is PreferIPv4.
type DialFamily string

const (
	// PreferIPv4 races as Dual does but starts with IPv4: an IPv6 path that connects and then stalls cannot win the
	// race by connecting first. IPv6 is still tried, 250 ms later, so an IPv6-only network costs that delay.
	PreferIPv4 DialFamily = "prefer-ipv4"
	// Dual is RFC 8305 as written: the family of the first address the resolver returned goes first.
	Dual DialFamily = "dual"
	// IPv4Only and IPv6Only drop the other family before the race.
	IPv4Only DialFamily = "ipv4-only"
	IPv6Only DialFamily = "ipv6-only"
)

// ParseDialFamily reads a setting; the empty string is the default, PreferIPv4. Anything else it does not know is an error.
func ParseDialFamily(s string) (DialFamily, error) {
	switch f := DialFamily(s); f {
	case "":
		return PreferIPv4, nil
	case PreferIPv4, Dual, IPv4Only, IPv6Only:
		return f, nil
	}
	return "", fmt.Errorf("%q is not one of prefer-ipv4, dual, ipv4-only, ipv6-only", s)
}

// order returns the addresses to try, in the order to start them: the family setting applied to ips.
func (f DialFamily) order(ips []netip.Addr) []netip.Addr {
	if len(ips) == 0 {
		return nil
	}
	switch f {
	case IPv4Only, IPv6Only:
		var out []netip.Addr
		for _, ip := range ips {
			if ip.Unmap().Is4() == (f == IPv4Only) {
				out = append(out, ip.Unmap())
			}
		}
		return out
	case Dual:
		return interleave(ips, ips[0].Unmap().Is4())
	}
	return interleave(ips, true)
}

// interleave orders addresses as RFC 8305 section 4 asks: one family first (v4First), then the other, alternating; within
// a family the resolver's order is kept. Addresses are compared after Unmap, so a v4-mapped address is a v4 address, as
// it is when dialed.
func interleave(ips []netip.Addr, v4First bool) []netip.Addr {
	var first, second []netip.Addr
	for _, ip := range ips {
		ip = ip.Unmap()
		if ip.Is4() == v4First {
			first = append(first, ip)
		} else {
			second = append(second, ip)
		}
	}
	out := make([]netip.Addr, 0, len(ips))
	for i := 0; i < len(first) || i < len(second); i++ {
		if i < len(first) {
			out = append(out, first[i])
		}
		if i < len(second) {
			out = append(out, second[i])
		}
	}
	return out
}

// dialRace connects to one of ips (all of which the caller has already checked; nothing here resolves or adds an
// address) on port, by Happy Eyeballs (RFC 8305 sections 4 and 5, TCP only), in the order family gives. The first attempt starts at once; when an
// attempt has neither connected nor failed within connectionAttemptDelay the next starts while it goes on, and when an
// attempt fails the next starts at once. The first connection wins; every other attempt is cancelled, and a connection
// that completes after that is closed. Every attempt is bounded by ctx. When all fail the error is the last one.
func dialRace(ctx context.Context, dial func(ctx context.Context, network, address string) (net.Conn, error), family DialFamily, ips []netip.Addr, port int) (net.Conn, error) {
	order := family.order(ips)
	if len(order) == 0 {
		if len(ips) == 0 {
			return nil, errors.New("no address to connect to")
		}
		return nil, fmt.Errorf("no address of the family %s among those checked", family)
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c   net.Conn
		err error
	}
	results := make(chan result, len(order)) // buffered: an attempt that ends after the race never blocks
	started, pending := 0, 0
	start := func() {
		address := netip.AddrPortFrom(order[started], uint16(port)).String()
		started++
		pending++
		go func() {
			c, err := dial(raceCtx, "tcp", address)
			results <- result{c, err}
		}()
	}
	// whatever is still running is waited for in the background; a connection it makes is closed
	reap := func() {
		cancel()
		n := pending
		go func() {
			for ; n > 0; n-- {
				if r := <-results; r.c != nil {
					r.c.Close()
				}
			}
		}()
	}
	timer := time.NewTimer(connectionAttemptDelay)
	defer timer.Stop()
	start()
	var last error
	for {
		select {
		case r := <-results:
			pending--
			if r.err == nil {
				reap()
				return r.c, nil
			}
			last = r.err
			if started < len(order) {
				start()
				timer.Reset(connectionAttemptDelay)
			} else if pending == 0 {
				return nil, last
			}
		case <-timer.C:
			if started < len(order) {
				start()
				timer.Reset(connectionAttemptDelay)
			}
		case <-ctx.Done():
			reap()
			if last == nil {
				last = ctx.Err()
			}
			return nil, last
		}
	}
}
