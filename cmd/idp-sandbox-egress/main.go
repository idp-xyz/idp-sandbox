// idp-sandbox-egress: the allow-listed forward proxy sandboxes use to reach the hosts an operator approved.
//
//	IDP_EGRESS_LISTEN   address of the proxy (default 0.0.0.0:3128)
//	IDP_EGRESS_HOSTS    comma-separated allow-list: exact names or *.suffix (required; empty denies everything)
//	IDP_EGRESS_PORTS    comma-separated destination ports (default 443,80)
//	IDP_EGRESS_ALLOW_PRIVATE=true   disables the refusal of private, loopback and link-local addresses: tests only
//	                    (with it an allow-listed name can reach the operator's internal network)
//	IDP_EGRESS_KEY_FILE the key shared with the sandbox service (its IDP_SANDBOX_EGRESS_KEY_FILE; at least 32 bytes, for
//	                    example openssl rand -hex 32): every request must then carry the credential the service made for the
//	                    run, which says the run's tenant (ADR-0001 D21). Unset: any run may use the proxy, as one
//	IDP_EGRESS_DIAL_FAMILY prefer-ipv4 (default), dual, ipv4-only or ipv6-only: which address families the proxy dials and which
//	                    goes first (ADR-0001 D24). An unknown value is a startup error
//	IDP_EGRESS_TENANTS_FILE   allow-lists per tenant (YAML: default: {hosts, ports}, tenants: {<tenant id>: {hosts, ports}};
//	                    internal/egress/tenants.go). A tenant named there gets its own list and nothing of the default;
//	                    the others get default, or IDP_EGRESS_HOSTS / IDP_EGRESS_PORTS when the file has none. Needs the key
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"idp-sandbox/internal/egress"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var hosts []string
	for _, h := range strings.Split(os.Getenv("IDP_EGRESS_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	var ports []int
	for _, s := range strings.Split(os.Getenv("IDP_EGRESS_PORTS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 65535 {
				fmt.Fprintln(os.Stderr, "invalid IDP_EGRESS_PORTS")
				os.Exit(2)
			}
			ports = append(ports, n)
		}
	}
	listen := os.Getenv("IDP_EGRESS_LISTEN")
	if listen == "" {
		listen = "0.0.0.0:3128"
	}
	p := &egress.Proxy{Policy: egress.Policy{Hosts: hosts, Ports: ports, AllowPrivate: os.Getenv("IDP_EGRESS_ALLOW_PRIVATE") == "true"}, Log: log}
	family, err := egress.ParseDialFamily(os.Getenv("IDP_EGRESS_DIAL_FAMILY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "IDP_EGRESS_DIAL_FAMILY:", err)
		os.Exit(2)
	}
	p.DialFamily = family
	if f := os.Getenv("IDP_EGRESS_KEY_FILE"); f != "" {
		key, err := egress.LoadKey(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "IDP_EGRESS_KEY_FILE:", err)
			os.Exit(2)
		}
		p.Key = key
	}
	if f := os.Getenv("IDP_EGRESS_TENANTS_FILE"); f != "" {
		if p.Key == nil { // without the credential every run looks the same
			fmt.Fprintln(os.Stderr, "IDP_EGRESS_TENANTS_FILE needs IDP_EGRESS_KEY_FILE: the proxy cannot tell tenants apart without the runs' credentials")
			os.Exit(2)
		}
		def, tenants, err := egress.LoadTenants(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "IDP_EGRESS_TENANTS_FILE:", err)
			os.Exit(2)
		}
		if def != nil {
			def.AllowPrivate = p.Policy.AllowPrivate
			p.Policy = *def
		}
		p.Tenants = tenants
	}
	names := make([]string, 0, len(p.Tenants))
	for id := range p.Tenants {
		names = append(names, id)
	}
	sort.Strings(names)
	log.Info("idp-sandbox-egress listening", "addr", listen, "hosts", p.Policy.Hosts, "dialFamily", string(p.DialFamily), "credentialRequired", p.Key != nil, "tenantsWithTheirOwnList", names)
	srv := &http.Server{Addr: listen, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
