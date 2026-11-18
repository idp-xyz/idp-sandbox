package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"idp-sandbox/internal/egress"
	"idp-sandbox/internal/sandbox"
)

// Egress on this backend (ADR-0001 D22). A run that asks for the network is a pod with one more label
// (NetworkLabel); a NetworkPolicy of the runs namespace lets pods with it reach the egress proxy's pods and nothing
// else, and a NetworkPolicy of the proxy lets only them in. The service creates and changes no policy: it only decides
// which pods carry the label. A run pod has no DNS (a resolver would be a way out), so it is told the proxy's address,
// the cluster IP of the proxy's Service, which this service looks up.

// credentialSlack is how much longer than the run's own time its egress credential lasts, beyond the time the pod may
// take to be scheduled and to pull its image (Config.Grace).
const credentialSlack = 60 * time.Second

// proxyAddr is the address runs reach the proxy on.
func (k *Kube) proxyAddr(ctx context.Context) (netip.Addr, error) {
	if k.cfg.EgressProxy == "" {
		return netip.Addr{}, sandbox.ErrNoEgress
	}
	if a, err := netip.ParseAddr(k.cfg.EgressProxy); err == nil {
		return a.Unmap(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := k.cfg.Resolve(ctx, k.cfg.EgressProxy)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("the egress proxy %s: %w", k.cfg.EgressProxy, err)
	}
	var first netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is4() { // a dual-stack Service: the address every image's clients can use
			return a, nil
		}
		if !first.IsValid() {
			first = a
		}
	}
	if !first.IsValid() {
		return netip.Addr{}, fmt.Errorf("the egress proxy %s has no address", k.cfg.EgressProxy)
	}
	return first, nil
}

// proxyURL is what an egress run's HTTP clients are told to use: the proxy by address, with the run's credential as
// the password when the service and the proxy share a key (D21). The credential names no address of the proxy: every
// run of the cluster reaches it on the same one.
func (k *Kube) proxyURL(ctx context.Context, spec sandbox.RunSpec, timeout time.Duration) (string, error) {
	addr, err := k.proxyAddr(ctx)
	if err != nil {
		return "", err
	}
	hostport := net.JoinHostPort(addr.String(), strconv.Itoa(k.cfg.EgressPort))
	if len(k.cfg.EgressKey) == 0 {
		return "http://" + hostport, nil
	}
	ws := sha256.Sum256([]byte(spec.Workspace))
	cred, err := egress.Mint(k.cfg.EgressKey, egress.Claims{Tenant: spec.Tenant,
		Expires: time.Now().Add(timeout + k.cfg.Grace + credentialSlack).Unix(), Workspace: hex.EncodeToString(ws[:4])})
	if err != nil {
		return "", fmt.Errorf("egress credential: %w", err)
	}
	return "http://" + egress.User + ":" + cred + "@" + hostport, nil
}
