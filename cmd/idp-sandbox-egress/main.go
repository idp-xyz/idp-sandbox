// idp-sandbox-egress: the allow-listed forward proxy sandboxes use to reach the hosts an operator approved.
//
//	IDP_EGRESS_LISTEN   address of the proxy (default 0.0.0.0:3128)
//	IDP_EGRESS_HOSTS    comma-separated allow-list: exact names or *.suffix (required; empty denies everything)
//	IDP_EGRESS_PORTS    comma-separated destination ports (default 443,80)
//	IDP_EGRESS_ALLOW_PRIVATE=true   disables the refusal of private, loopback and link-local addresses: tests only
//	                    (with it an allow-listed name can reach the operator's internal network)
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	log.Info("idp-sandbox-egress listening", "addr", listen, "hosts", hosts)
	srv := &http.Server{Addr: listen, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
