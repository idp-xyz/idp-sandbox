package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"
)

// Language is a runtime the caller may pick. The images are the operator's; a caller never names one.
type Language struct {
	Image string
	// Command returns the command line that runs the file at /work/<file>.
	Command func(file string) []string
	File    string
	// Description is what the tool tells the model about this language.
	Description string
}

// OfficeLanguage is the office run profile (ADR-0001 D14): a POSIX shell script in the operator's office image, which has
// officecli on its PATH for .docx, .xlsx and .pptx files in the workspace. Same isolation and limits as any other run.
func OfficeLanguage(image string) Language {
	return Language{Image: image, File: "main.sh", Command: func(f string) []string { return []string{"sh", f} },
		Description: "a POSIX shell script with the officecli command on PATH, for reading, editing and creating .docx, .xlsx and .pptx files in the workspace (officecli help lists the commands)"}
}

// Config of the manager.
type Config struct {
	Limits        Limits
	Languages     map[string]Language
	MaxWorkspaces int
	MaxConcurrent int // runs at the same time
	MaxPerOwner   int // workspaces one caller may hold
	// RequireOwner refuses calls that carry no caller identity (the default); without it every anonymous
	// call shares one owner and a workspace id is a bearer capability, as in ADR-0001 §5 before D11.
	RequireOwner bool
	// EgressEnabled lets a run ask for network=egress: access to the operator's allow-listed hosts through the
	// egress proxy. Off unless the operator configured a proxy.
	EgressEnabled bool
	IdleTTL       time.Duration // a workspace nobody used for this long is removed
	MaxLifetime   time.Duration
	MaxCodeBytes  int
	MaxFileBytes  int64
	Namespace     string // prefix of workspace names
}

// DefaultConfig is conservative: 256 MiB, one CPU, 64 processes, 20 s, 64 KiB of output, 64 MiB workspace.
func DefaultConfig(pythonImage, shellImage string) Config {
	return Config{
		Limits: Limits{MemoryBytes: 256 << 20, CPUs: 1, Pids: 64, Timeout: 20 * time.Second,
			OutputBytes: 64 << 10, WorkspaceBytes: 64 << 20, TmpBytes: 16 << 20},
		Languages: map[string]Language{
			"python": {Image: pythonImage, File: "main.py", Command: func(f string) []string {
				return []string{"python", "-I", "-B", f}
			}, Description: "a Python 3 program (standard library only)"},
			"shell": {Image: shellImage, File: "main.sh", Command: func(f string) []string {
				return []string{"sh", f}
			}, Description: "a POSIX shell script"},
		},
		MaxWorkspaces: 64, MaxPerOwner: 8, RequireOwner: true, MaxConcurrent: 8, IdleTTL: 30 * time.Minute, MaxLifetime: 4 * time.Hour,
		MaxCodeBytes: 256 << 10, MaxFileBytes: 1 << 20, Namespace: "idp-sbx-",
	}
}

type workspace struct {
	owner         string
	created, used time.Time
	busy          bool
}

// Manager owns the workspaces and applies the limits and the admission rules.
type Manager struct {
	cfg Config
	be  Backend
	log *slog.Logger
	now func() time.Time

	mu         sync.Mutex
	workspaces map[string]*workspace
	slots      chan struct{}
}

// New creates a manager and removes whatever an earlier process left behind.
func New(ctx context.Context, cfg Config, be Backend, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := be.Sweep(ctx); err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}
	return &Manager{cfg: cfg, be: be, log: log, now: time.Now, workspaces: map[string]*workspace{},
		slots: make(chan struct{}, cfg.MaxConcurrent)}, nil
}

// Config returns the effective configuration (for the tool descriptions).
func (m *Manager) Config() Config { return m.cfg }

// Ready reports whether the isolation layer answers.
func (m *Manager) Ready(ctx context.Context) error { return m.be.Ping(ctx) }

// Create makes an empty workspace for owner. The id is 128 random bits, and every later call must come
// from the same owner: a workspace of another caller is indistinguishable from one that does not exist.
func (m *Manager) Create(ctx context.Context, owner string) (id string, expires time.Time, err error) {
	if err := m.checkOwner(owner); err != nil {
		return "", time.Time{}, err
	}
	m.mu.Lock()
	if len(m.workspaces) >= m.cfg.MaxWorkspaces {
		m.mu.Unlock()
		return "", time.Time{}, ErrCapacity
	}
	held := 0
	for _, w := range m.workspaces {
		if w.owner == owner {
			held++
		}
	}
	if held >= m.cfg.MaxPerOwner {
		m.mu.Unlock()
		return "", time.Time{}, ErrOwnerQuota
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		m.mu.Unlock()
		return "", time.Time{}, err
	}
	id = hex.EncodeToString(b[:])
	now := m.now()
	m.workspaces[id] = &workspace{owner: owner, created: now, used: now}
	m.mu.Unlock()
	if err := m.be.CreateWorkspace(ctx, m.cfg.Namespace+id, m.cfg.Limits.WorkspaceBytes); err != nil {
		m.forget(id)
		return "", time.Time{}, err
	}
	return id, now.Add(m.cfg.MaxLifetime), nil
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.workspaces, id)
	m.mu.Unlock()
}

// acquire marks the workspace used; a run additionally needs it exclusively.
func (m *Manager) checkOwner(owner string) error {
	if owner == "" && m.cfg.RequireOwner {
		return ErrNoCaller
	}
	return nil
}

func (m *Manager) acquire(owner, id string, exclusive bool) error {
	if err := m.checkOwner(owner); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workspaces[id]
	if w == nil || w.owner != owner || m.now().Sub(w.created) > m.cfg.MaxLifetime {
		return ErrNotFound
	}
	if exclusive {
		if w.busy {
			return ErrBusy
		}
		w.busy = true
	}
	w.used = m.now()
	return nil
}

func (m *Manager) release(id string) {
	m.mu.Lock()
	if w := m.workspaces[id]; w != nil {
		w.busy = false
		w.used = m.now()
	}
	m.mu.Unlock()
}

// Delete removes a workspace and everything in it.
func (m *Manager) Delete(ctx context.Context, owner, id string) error {
	if err := m.checkOwner(owner); err != nil {
		return err
	}
	m.mu.Lock()
	w := m.workspaces[id]
	if w == nil || w.owner != owner {
		m.mu.Unlock()
		return ErrNotFound
	}
	if w.busy {
		m.mu.Unlock()
		return ErrBusy
	}
	delete(m.workspaces, id)
	m.mu.Unlock()
	return m.be.DeleteWorkspace(ctx, m.cfg.Namespace+id)
}

// Run executes code in the workspace. timeout 0 means the default; it is capped at the configured one.
func (m *Manager) Run(ctx context.Context, owner, id, language, code string, timeout time.Duration, egress bool) (RunResult, error) {
	if egress && !m.cfg.EgressEnabled {
		return RunResult{}, ErrNoEgress
	}
	lang, ok := m.cfg.Languages[language]
	if !ok {
		return RunResult{}, fmt.Errorf("%w: language %q is not offered", ErrInvalid, language)
	}
	if code == "" || len(code) > m.cfg.MaxCodeBytes {
		return RunResult{}, fmt.Errorf("%w: code must be 1..%d bytes", ErrInvalid, m.cfg.MaxCodeBytes)
	}
	if timeout <= 0 || timeout > m.cfg.Limits.Timeout {
		timeout = m.cfg.Limits.Timeout
	}
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	default:
		return RunResult{}, ErrCapacity
	}
	if err := m.acquire(owner, id, true); err != nil {
		return RunResult{}, err
	}
	defer m.release(id)
	name := m.cfg.Namespace + id
	file := ".idp/" + lang.File
	if err := m.be.WriteFile(ctx, name, file, []byte(code)); err != nil {
		return RunResult{}, err
	}
	res, err := m.be.Run(ctx, RunSpec{Workspace: name, Image: lang.Image, Cmd: lang.Command("/work/" + file),
		Limits: m.cfg.Limits, Timeout: timeout, Egress: egress})
	return res, err
}

// WriteFile stores text in the workspace.
func (m *Manager) WriteFile(ctx context.Context, owner, id, p string, data []byte) error {
	p, err := CleanPath(p)
	if err != nil {
		return err
	}
	if int64(len(data)) > m.cfg.MaxFileBytes {
		return ErrFileTooLarge
	}
	if err := m.acquire(owner, id, false); err != nil {
		return err
	}
	return m.be.WriteFile(ctx, m.cfg.Namespace+id, p, data)
}

// ReadFile returns a regular file of the workspace.
func (m *Manager) ReadFile(ctx context.Context, owner, id, p string) ([]byte, error) {
	p, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	if err := m.acquire(owner, id, false); err != nil {
		return nil, err
	}
	return m.be.ReadFile(ctx, m.cfg.Namespace+id, p, m.cfg.MaxFileBytes)
}

// ListFiles lists the regular files of the workspace (the runner's own scratch directory excluded).
func (m *Manager) ListFiles(ctx context.Context, owner, id string) ([]FileInfo, error) {
	if err := m.acquire(owner, id, false); err != nil {
		return nil, err
	}
	all, err := m.be.ListFiles(ctx, m.cfg.Namespace+id, 1000)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, f := range all {
		if f.Path != ".idp" && !strings.HasPrefix(f.Path, ".idp/") {
			out = append(out, f)
		}
	}
	return out, nil
}

// Reap removes workspaces that are idle or past their lifetime. Run it periodically.
func (m *Manager) Reap(ctx context.Context) int {
	m.mu.Lock()
	var gone []string
	now := m.now()
	for id, w := range m.workspaces {
		if !w.busy && (now.Sub(w.used) > m.cfg.IdleTTL || now.Sub(w.created) > m.cfg.MaxLifetime) {
			gone = append(gone, id)
			delete(m.workspaces, id)
		}
	}
	m.mu.Unlock()
	for _, id := range gone {
		if err := m.be.DeleteWorkspace(ctx, m.cfg.Namespace+id); err != nil {
			m.log.Warn("reap", "workspace", id[:8], "err", err)
		}
	}
	return len(gone)
}

// Count is the number of live workspaces.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.workspaces)
}

// CleanPath validates a caller's file path: relative, inside the workspace, not the runner's directory.
func CleanPath(p string) (string, error) {
	if p == "" || len(p) > 255 || strings.ContainsAny(p, "\x00\\") {
		return "", fmt.Errorf("%w: path must be 1..255 characters without NUL or backslash", ErrInvalid)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: path must be relative to the workspace", ErrInvalid)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: path must stay inside the workspace", ErrInvalid)
		}
	}
	c := path.Clean(p)
	if c == "." || c == ".idp" || strings.HasPrefix(c, ".idp/") {
		return "", fmt.Errorf("%w: reserved path", ErrInvalid)
	}
	if strings.Count(c, "/") > 8 {
		return "", fmt.Errorf("%w: path is nested too deep", ErrInvalid)
	}
	return c, nil
}
