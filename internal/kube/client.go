// Package kube is the Kubernetes backend of the sandbox: the same isolation model as the Docker backend
// (no network, read-only root, no capabilities, an unprivileged user, resource limits) expressed as Jobs,
// PersistentVolumeClaims and ConfigMaps, driven over the Kubernetes REST API with net/http only. There is
// no exec, attach or websocket: code and files travel in ConfigMaps, results come back through pod logs.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// In-cluster locations (the service account the pod runs as).
const (
	saDir         = "/var/run/secrets/kubernetes.io/serviceaccount"
	saTokenFile   = saDir + "/token"
	saCAFile      = saDir + "/ca.crt"
	saNamespaceFi = saDir + "/namespace"
)

// apiError is a non-2xx answer of the API server. Message is kept for the logs only.
type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (a *apiError) Error() string {
	return fmt.Sprintf("kubernetes: HTTP %d %s: %s", a.Status, a.Reason, a.Message)
}

func isStatus(err error, status int) bool {
	var a *apiError
	return errors.As(err, &a) && a.Status == status
}

type client struct {
	base      string
	http      *http.Client
	token     string // explicit token, else read from tokenFile
	tokenFile string

	mu      sync.Mutex
	cached  string
	readAt  time.Time
	maxBody int64
}

// newClient builds the client. apiServer empty means in-cluster (KUBERNETES_SERVICE_HOST/PORT, the
// service-account token and CA); otherwise apiServer is a base URL and token/tokenFile/caFile are explicit.
func newClient(apiServer, token, tokenFile, caFile string) (*client, error) {
	c := &client{token: token, tokenFile: tokenFile, maxBody: 8 << 20}
	if apiServer == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("not in a cluster (KUBERNETES_SERVICE_HOST/PORT unset) and no IDP_SANDBOX_KUBE_API given")
		}
		apiServer = "https://" + net.JoinHostPort(host, port)
		if c.token == "" && c.tokenFile == "" {
			c.tokenFile = saTokenFile
		}
		if caFile == "" {
			caFile = saCAFile
		}
	}
	u, err := url.Parse(apiServer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid Kubernetes API address %q", apiServer)
	}
	c.base = strings.TrimRight(apiServer, "/")
	tr := &http.Transport{MaxIdleConns: 8, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	if u.Scheme == "https" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("reading the cluster CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("the cluster CA file holds no certificate")
			}
			cfg.RootCAs = pool
		}
		tr.TLSClientConfig = cfg
	}
	c.http = &http.Client{Transport: tr}
	if _, err := c.bearer(); err != nil {
		return nil, err
	}
	return c, nil
}

// bearer returns the token; a file is re-read every 30 s because bound service-account tokens rotate.
func (c *client) bearer() (string, error) {
	if c.token != "" {
		return c.token, nil
	}
	if c.tokenFile == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" && time.Since(c.readAt) < 30*time.Second {
		return c.cached, nil
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("reading the service-account token: %w", err)
	}
	c.cached, c.readAt = strings.TrimSpace(string(b)), time.Now()
	return c.cached, nil
}

func (c *client) raw(ctx context.Context, method, p string, q url.Values, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	u := c.base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return c.http.Do(req)
}

func readError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var st struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &st)
	return &apiError{Status: resp.StatusCode, Reason: st.Reason, Message: st.Message}
}

// json performs a request and decodes a JSON answer into out (nil to discard it).
func (c *client) json(ctx context.Context, method, p string, q url.Values, in, out any) error {
	resp, err := c.raw(ctx, method, p, q, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// text fetches a plain-text body of at most max bytes (pod logs).
func (c *client) text(ctx context.Context, p string, q url.Values, max int64) (string, error) {
	resp, err := c.raw(ctx, http.MethodGet, p, q, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", readError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return string(raw), err
}
