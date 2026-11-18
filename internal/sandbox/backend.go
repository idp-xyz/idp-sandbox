// Package sandbox runs code an agent wrote in an isolated, resource-bounded, network-less container
// and keeps the files it leaves in a quota-bounded workspace (ADR-0001 D2-D7).
package sandbox

import (
	"context"
	"errors"
	"time"
)

// Limits bound one run. They are set by the operator; the caller can only shorten the timeout.
type Limits struct {
	MemoryBytes    int64
	CPUs           float64
	Pids           int64
	Timeout        time.Duration // default and ceiling of one run
	OutputBytes    int           // stdout and stderr each
	WorkspaceBytes int64         // size of a workspace (kernel-enforced: a size-limited tmpfs volume)
	TmpBytes       int64
}

// RunSpec is one execution: the command runs in Image with the workspace mounted at /work.
type RunSpec struct {
	Workspace string
	Image     string
	Cmd       []string
	Limits    Limits
	Timeout   time.Duration
	Stdin     []byte // fed to the command, then closed; nil attaches nothing
	// RawOutput reads stdout and stderr from the attach stream instead of the container's log: byte for byte, whatever the
	// content and size (a log driver re-encodes and rotates). File reads use it (ADR-0001 D15).
	RawOutput bool
	// Egress gives the run a path to the operator's allow-listed proxy and nothing else (ADR-0001 D13).
	Egress bool
	// Tenant is the caller's tenant: the proxy applies that tenant's allow-list when the operator keeps one per tenant (D21).
	Tenant string
}

// RunResult is what a run left behind.
type RunResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
	TimedOut  bool
	OOMKilled bool
	Duration  time.Duration
}

// FileInfo is one regular file of a workspace.
type FileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// BinaryFiles is a Backend that moves any bytes, of up to the transfer limit, in and out of a workspace: the import and
// export tools are offered only on one (ADR-0001 D15). Both backends are: Docker over the attach stream, Kubernetes in
// pieces through ConfigMaps and, base64-encoded, pod logs (D19).
type BinaryFiles interface {
	BinaryFiles() bool
}

// BinaryReader is a Backend whose ReadFile is only right for text (the Kubernetes backend reads a pod log) and which
// reads any bytes another way. The manager uses it for a file that is exported.
type BinaryReader interface {
	ReadBytes(ctx context.Context, workspace, path string, max int64) ([]byte, error)
}

// Backend is the isolation layer. The Docker backend (internal/engine) hardens runc containers, and a gVisor or Kata
// runtime is a configuration of it; the Kubernetes backend (internal/kube) runs each execution as a Job.
type Backend interface {
	Ping(ctx context.Context) error
	CreateWorkspace(ctx context.Context, name string, sizeBytes int64) error
	DeleteWorkspace(ctx context.Context, name string) error
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
	WriteFile(ctx context.Context, workspace, path string, data []byte) error
	ReadFile(ctx context.Context, workspace, path string, max int64) ([]byte, error)
	ListFiles(ctx context.Context, workspace string, max int) ([]FileInfo, error)
	// Sweep removes what a crashed service left behind (containers and workspaces it labelled).
	Sweep(ctx context.Context) error
}

var (
	ErrNotFound     = errors.New("workspace not found or expired")
	ErrBusy         = errors.New("the workspace is running another command")
	ErrCapacity     = errors.New("the sandbox is at capacity; try again later")
	ErrOwnerQuota   = errors.New("you already hold the maximum number of workspaces; delete one first")
	ErrNoCaller     = errors.New("the request carries no caller identity")
	ErrNoEgress     = errors.New("network access is not enabled on this sandbox")
	ErrInvalid      = errors.New("invalid argument")
	ErrFileTooLarge = errors.New("file too large")
	ErrNoFile       = errors.New("no such file")
)
