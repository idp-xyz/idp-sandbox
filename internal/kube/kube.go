package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"idp-sandbox/internal/sandbox"
)

// LabelKey marks everything this service creates, so a restart can sweep it (same key as the Docker backend).
const LabelKey = "idp.sandbox"

// Labels beyond LabelKey. RoleLabel is what the NetworkPolicy of the runs namespace selects.
const (
	RoleLabel      = "idp.sandbox/role" // "run" or "file"
	WorkspaceLabel = "idp.sandbox/workspace"
)

// Config of the backend.
type Config struct {
	// APIServer is an explicit base URL (tests, out-of-cluster); empty means in-cluster.
	APIServer string
	Token     string // explicit bearer token; or TokenFile
	TokenFile string // default in-cluster: the service-account token
	CAFile    string // default in-cluster: the service-account CA

	// Namespace holds workspaces and runs. Default: the pod's own namespace, else "idp-sandbox".
	Namespace    string
	StorageClass string // optional storageClassName of the workspace PVCs
	RuntimeClass string // optional runtimeClassName of run pods (gVisor, Kata)
	Owner        string // label value that scopes the sweep; default "default"
	// HelperImage runs the file operations (a shell image).
	HelperImage string
	// WorkspaceBytes is the usual size of a workspace (informational; CreateWorkspace receives the real one).
	WorkspaceBytes int64
	// Poll is the Job status polling interval (default 300 ms).
	Poll time.Duration
	// Grace is added to a run's timeout for the Job's activeDeadlineSeconds: scheduling and image pull
	// happen inside it (default 30 s). The timeout itself is enforced from the moment the pod runs.
	Grace time.Duration
}

// Kube implements sandbox.Backend.
type Kube struct {
	c   *client
	cfg Config
}

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
)

// New builds the backend and validates the configuration; it does not contact the cluster (Ping does).
func New(cfg Config) (*Kube, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = "idp-sandbox"
		if b, err := os.ReadFile(saNamespaceFi); err == nil && strings.TrimSpace(string(b)) != "" {
			cfg.Namespace = strings.TrimSpace(string(b))
		}
	}
	if cfg.Owner == "" {
		cfg.Owner = "default"
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 300 * time.Millisecond
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 30 * time.Second
	}
	if cfg.HelperImage == "" {
		return nil, errors.New("no helper (shell) image configured")
	}
	if !dnsLabel.MatchString(cfg.Namespace) || len(cfg.Namespace) > 63 {
		return nil, fmt.Errorf("invalid namespace %q", cfg.Namespace)
	}
	if !labelValue.MatchString(cfg.Owner) || len(cfg.Owner) > 63 {
		return nil, fmt.Errorf("invalid owner label %q", cfg.Owner)
	}
	if cfg.RuntimeClass != "" && !dnsLabel.MatchString(cfg.RuntimeClass) {
		return nil, fmt.Errorf("invalid runtime class %q", cfg.RuntimeClass)
	}
	c, err := newClient(cfg.APIServer, cfg.Token, cfg.TokenFile, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	return &Kube{c: c, cfg: cfg}, nil
}

func (k *Kube) ns(resource string) string {
	switch resource {
	case "jobs":
		return "/apis/batch/v1/namespaces/" + k.cfg.Namespace + "/jobs"
	default:
		return "/api/v1/namespaces/" + k.cfg.Namespace + "/" + resource
	}
}

func (k *Kube) labels() map[string]string { return map[string]string{LabelKey: k.cfg.Owner} }

func (k *Kube) ownerSelector() url.Values {
	return url.Values{"labelSelector": {LabelKey + "=" + k.cfg.Owner}}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Ping checks that the API server is ready and that this service account can use its namespace.
func (k *Kube) Ping(ctx context.Context) error {
	resp, err := k.c.raw(ctx, http.MethodGet, "/readyz", nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kubernetes readyz: HTTP %d", resp.StatusCode)
	}
	q := url.Values{"limit": {"1"}}
	return k.c.json(ctx, http.MethodGet, k.ns("persistentvolumeclaims"), q, nil, nil)
}

// CreateWorkspace makes the PersistentVolumeClaim. Its size is enforced by the volume (CSI quota, the
// filesystem's size); the claim is ReadWriteOnce, so one run at a time mounts it, as the manager already ensures.
func (k *Kube) CreateWorkspace(ctx context.Context, name string, size int64) error {
	if !dnsLabel.MatchString(name) || len(name) > 63 {
		return fmt.Errorf("%w: invalid workspace name", sandbox.ErrInvalid)
	}
	if size <= 0 {
		return fmt.Errorf("%w: invalid workspace size", sandbox.ErrInvalid)
	}
	spec := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]string{"storage": fmt.Sprint(size)}},
		"volumeMode":  "Filesystem",
	}
	if k.cfg.StorageClass != "" {
		spec["storageClassName"] = k.cfg.StorageClass
	}
	return k.c.json(ctx, http.MethodPost, k.ns("persistentvolumeclaims"), nil, map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name, "labels": k.labels()},
		"spec":     spec,
	}, nil)
}

type item struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

func (k *Kube) list(ctx context.Context, resource string, q url.Values) ([]item, error) {
	var l struct {
		Items []item `json:"items"`
	}
	if err := k.c.json(ctx, http.MethodGet, k.ns(resource), q, nil, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// delete removes one object; what is already gone is not an error. Jobs are deleted with Background
// propagation so the Job controller's garbage collection also removes their pods.
func (k *Kube) delete(ctx context.Context, resource, name string) error {
	var body any
	q := url.Values{}
	if resource == "jobs" {
		body = map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Background"}
		q.Set("propagationPolicy", "Background")
	}
	err := k.c.json(ctx, http.MethodDelete, k.ns(resource)+"/"+name, q, body, nil)
	if err != nil && !isStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

// deleteBy removes every object of a resource matching the label selector.
func (k *Kube) deleteBy(ctx context.Context, resource string, q url.Values) error {
	items, err := k.list(ctx, resource, q)
	if err != nil {
		return err
	}
	var first error
	for _, it := range items {
		if err := k.delete(ctx, resource, it.Metadata.Name); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// DeleteWorkspace removes the claim and any Jobs, ConfigMaps and pods left over from it; what is
// already gone is not an error.
func (k *Kube) DeleteWorkspace(ctx context.Context, name string) error {
	if !dnsLabel.MatchString(name) || len(name) > 63 {
		return nil // nothing of this name can exist
	}
	q := url.Values{"labelSelector": {LabelKey + "=" + k.cfg.Owner + "," + WorkspaceLabel + "=" + name}}
	var first error
	for _, r := range []string{"jobs", "configmaps", "pods"} {
		if err := k.deleteBy(ctx, r, q); err != nil && first == nil {
			first = err
		}
	}
	if err := k.delete(ctx, "persistentvolumeclaims", name); err != nil && first == nil {
		first = err
	}
	return first
}

// Sweep removes this owner's Jobs, pods, ConfigMaps and claims (what a crashed service left behind).
func (k *Kube) Sweep(ctx context.Context) error {
	for _, r := range []string{"jobs", "pods", "configmaps", "persistentvolumeclaims"} {
		if err := k.deleteBy(ctx, r, k.ownerSelector()); err != nil {
			return err
		}
	}
	return nil
}

var _ sandbox.Backend = (*Kube)(nil)
