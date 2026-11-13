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
	// Egress gives the run a path to the operator's allow-listed proxy and nothing else (ADR-0001 D12).
	Egress bool
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

// Backend is the isolation layer. The Docker backend (internal/engine) hardens runc containers;
// a gVisor or Kata runtime is a configuration of the same backend, a Kubernetes Job backend would
// be another implementation of this interface.
// BinaryFiles is a Backend that moves any bytes, of up to the transfer limit, in and out of a workspace: the import and
// export tools are offered only on one (ADR-0001 D15). The Kubernetes backend is not one: its files travel in ConfigMaps
// (about 1 MiB) and come back through pod logs.
type BinaryFiles interface {
	BinaryFiles() bool
}

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
