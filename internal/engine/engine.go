// Package engine is the Docker Engine backend of the sandbox: every run is a fresh container with no
// network, a read-only root, no capabilities, no new privileges, an unprivileged user, and memory, CPU,
// process, time, output and disk limits. It speaks the Engine API over its unix socket (net/http only).
package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"idp-sandbox/internal/sandbox"
)

const apiVersion = "v1.43"

// LabelKey marks everything this service creates, so a restart can sweep it.
const LabelKey = "idp.sandbox"

// Engine implements sandbox.Backend.
type Engine struct {
	http *http.Client
	// HelperImage is only ever created, never started: it provides the container that carries archive
	// reads and writes of a workspace volume.
	HelperImage string
	// Runtime selects an OCI runtime for runs ("runsc" for gVisor, "kata"); empty is the daemon's default.
	Runtime string
	// Owner labels the containers and volumes so that several services can share one daemon.
	Owner string
	// WorkspaceBytes bounds the size of one file a helper command may write (the volume's own size
	// limit is the real quota).
	WorkspaceBytes int64
	// EgressProxy is the name of the running egress proxy container (cmd/idp-sandbox-egress) and EgressPort its
	// port. A run that asks for egress gets its own internal network with the proxy attached and nothing else.
	EgressProxy string
	EgressPort  int
}

// New connects to host: "unix:///var/run/docker.sock" or "tcp://127.0.0.1:2375".
func New(host, helperImage string) (*Engine, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{MaxIdleConns: 4}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
	case "tcp":
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
		}
	default:
		return nil, fmt.Errorf("unsupported docker host %q", host)
	}
	return &Engine{http: &http.Client{Transport: tr}, HelperImage: helperImage, Owner: "default", WorkspaceBytes: 64 << 20}, nil
}

func (e *Engine) do(ctx context.Context, method, p string, q url.Values, body io.Reader, ctype string) (*http.Response, error) {
	u := "http://docker/" + apiVersion + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return e.http.Do(req)
}

type apiError struct {
	Status  int
	Message string
}

func (a *apiError) Error() string { return fmt.Sprintf("docker: HTTP %d: %s", a.Status, a.Message) }

func (e *Engine) json(ctx context.Context, method, p string, q url.Values, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := e.do(ctx, method, p, q, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &m)
		return &apiError{resp.StatusCode, m.Message}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func isStatus(err error, status int) bool {
	var a *apiError
	return errors.As(err, &a) && a.Status == status
}

// Ping checks the daemon.
func (e *Engine) Ping(ctx context.Context) error {
	resp, err := e.do(ctx, http.MethodGet, "/_ping", nil, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("docker ping: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (e *Engine) labels() map[string]string { return map[string]string{LabelKey: e.Owner} }

// CreateWorkspace makes a volume whose size the kernel enforces: a tmpfs volume with a size limit. The
// files are in memory, owned by the unprivileged user, and gone when the volume is. The local volume
// driver unmounts a tmpfs volume (and loses its content) when no container uses it, so an anchor
// container, `sleep` with a few pids and megabytes, holds the workspace between runs.
func (e *Engine) CreateWorkspace(ctx context.Context, name string, size int64) error {
	err := e.json(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{
		"Name": name, "Driver": "local", "Labels": e.labels(),
		"DriverOpts": map[string]string{"type": "tmpfs", "device": "tmpfs",
			"o": fmt.Sprintf("size=%d,uid=65534,gid=65534,mode=0700,nosuid,nodev", size)},
	}, nil)
	if err != nil {
		return err
	}
	var created struct{ Id string }
	err = e.json(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name + "-anchor"}}, map[string]any{
		"Image": e.HelperImage, "Cmd": []string{"sleep", "infinity"}, "User": "65534:65534", "Labels": e.labels(),
		"NetworkDisabled": true,
		"HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges"}, "Init": true, "Memory": 16 << 20, "MemorySwap": 16 << 20, "PidsLimit": 4,
			"Mounts": []any{workspaceMount(name)}},
	}, &created)
	if err == nil {
		err = e.json(ctx, http.MethodPost, "/containers/"+created.Id+"/start", nil, nil, nil)
	}
	if err != nil {
		_ = e.DeleteWorkspace(context.Background(), name)
		return err
	}
	return nil
}

// DeleteWorkspace removes the anchor and the volume; what is already gone is not an error.
func (e *Engine) DeleteWorkspace(ctx context.Context, name string) error {
	err := e.json(ctx, http.MethodDelete, "/containers/"+name+"-anchor", url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if err != nil && !isStatus(err, 404) {
		return err
	}
	err = e.json(ctx, http.MethodDelete, "/volumes/"+name, url.Values{"force": {"1"}}, nil, nil)
	if err != nil && !isStatus(err, 404) {
		return err
	}
	return nil
}

func workspaceMount(name string) map[string]any {
	return map[string]any{"Type": "volume", "Source": name, "Target": "/work",
		"VolumeOptions": map[string]any{"NoCopy": true}}
}

func (e *Engine) remove(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = e.json(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
}

// fileOp runs a short command in the workspace as the sandbox user, with the same hardening as a run.
func (e *Engine) fileOp(ctx context.Context, workspace string, cmd []string, stdin []byte, outLimit int) (sandbox.RunResult, error) {
	return e.Run(ctx, sandbox.RunSpec{Workspace: workspace, Image: e.HelperImage, Cmd: cmd, Stdin: stdin, Timeout: 20 * time.Second,
		Limits: sandbox.Limits{MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32, OutputBytes: outLimit, WorkspaceBytes: e.WorkspaceBytes, TmpBytes: 1 << 20}})
}

func workspaceExists(res sandbox.RunResult, err error) error {
	var a *apiError
	if errors.As(err, &a) && a.Status == 404 {
		return sandbox.ErrNotFound
	}
	return err
}

// WriteFile stores a regular file at /work/<p>. The bytes travel on the container's stdin: a tmpfs
// workspace is only visible inside containers that mount it, so the archive API cannot reach it.
func (e *Engine) WriteFile(ctx context.Context, workspace, p string, data []byte) error {
	res, err := e.fileOp(ctx, workspace, []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", "/work/" + p}, data, 4096)
	if err != nil {
		return workspaceExists(res, err)
	}
	switch {
	case res.TimedOut:
		return errors.New("writing the file timed out")
	case res.ExitCode != 0 && strings.Contains(res.Stderr, "No space"):
		return fmt.Errorf("%w: the workspace is full", sandbox.ErrInvalid)
	case res.ExitCode != 0:
		return fmt.Errorf("%w: cannot write there (%s)", sandbox.ErrInvalid, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ReadFile returns a regular file; a symlink, directory or device is refused, never followed.
func (e *Engine) ReadFile(ctx context.Context, workspace, p string, max int64) ([]byte, error) {
	const script = `[ -f "$1" ] && [ ! -L "$1" ] || exit 44
case "$(readlink -f "$1")" in /work/*) ;; *) exit 45;; esac
cat "$1"`
	res, err := e.fileOp(ctx, workspace, []string{"sh", "-c", script, "sh", "/work/" + p}, nil, int(max)+1)
	if err != nil {
		return nil, workspaceExists(res, err)
	}
	switch {
	case res.ExitCode == 44 || res.ExitCode == 45:
		return nil, sandbox.ErrNoFile
	case res.Truncated || int64(len(res.Stdout)) > max:
		return nil, sandbox.ErrFileTooLarge
	case res.ExitCode != 0:
		return nil, errors.New("reading the file failed")
	}
	return []byte(res.Stdout), nil
}

// ListFiles lists regular files (symlinks are not listed) with their sizes.
func (e *Engine) ListFiles(ctx context.Context, workspace string, max int) ([]sandbox.FileInfo, error) {
	res, err := e.fileOp(ctx, workspace, []string{"sh", "-c", fmt.Sprintf(`cd /work && find . -type f -exec stat -c '%%s %%n' {} + | head -n %d`, max)}, nil, 1<<20)
	if err != nil {
		return nil, workspaceExists(res, err)
	}
	if res.ExitCode != 0 {
		return nil, errors.New("listing the workspace failed")
	}
	var out []sandbox.FileInfo
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		size, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, sandbox.FileInfo{Path: strings.TrimPrefix(name, "./"), Size: n})
	}
	return out, nil
}

// Run starts the command in a hardened container and waits for it, the timeout, or the output cap.
func (e *Engine) Run(ctx context.Context, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	lim := spec.Limits
	var created struct{ Id string }
	network, env := "none", []string{"HOME=/tmp", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1"}
	if spec.Egress {
		if e.EgressProxy == "" {
			return sandbox.RunResult{}, sandbox.ErrNoEgress
		}
		net, err := e.runNetwork(ctx)
		if err != nil {
			return sandbox.RunResult{}, err
		}
		defer net.remove(e)
		network = net.name
		proxy := fmt.Sprintf("http://%s:%d", e.EgressProxy, e.EgressPort)
		env = append(env, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "http_proxy="+proxy, "https_proxy="+proxy)
	}
	cfg := map[string]any{
		"Image": spec.Image, "Cmd": spec.Cmd, "User": "65534:65534", "WorkingDir": "/work",
		"Env":             env,
		"NetworkDisabled": network == "none", "Labels": e.labels(), "AttachStdin": spec.Stdin != nil, "OpenStdin": spec.Stdin != nil, "StdinOnce": spec.Stdin != nil, "Tty": false,
		"HostConfig": map[string]any{
			"NetworkMode": network, "ReadonlyRootfs": true, "CapDrop": []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges"}, "Privileged": false, "Init": true, "IpcMode": "private",
			"Memory": lim.MemoryBytes, "MemorySwap": lim.MemoryBytes, "NanoCpus": int64(lim.CPUs * 1e9), "PidsLimit": lim.Pids,
			"Mounts": []any{workspaceMount(spec.Workspace)},
			"Tmpfs":  map[string]string{"/tmp": fmt.Sprintf("rw,noexec,nosuid,nodev,size=%d", lim.TmpBytes)},
			"Ulimits": []map[string]any{{"Name": "nofile", "Soft": 256, "Hard": 256}, {"Name": "core", "Soft": 0, "Hard": 0},
				{"Name": "fsize", "Soft": lim.WorkspaceBytes, "Hard": lim.WorkspaceBytes}},
			"LogConfig":  map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m", "max-file": "1"}},
			"AutoRemove": false, "PublishAllPorts": false,
		},
	}
	if e.Runtime != "" {
		cfg["HostConfig"].(map[string]any)["Runtime"] = e.Runtime
	}
	if err := e.json(ctx, http.MethodPost, "/containers/create", nil, cfg, &created); err != nil {
		if isStatus(err, 404) {
			return sandbox.RunResult{}, fmt.Errorf("image %s or the workspace is missing: %w", spec.Image, err)
		}
		return sandbox.RunResult{}, err
	}
	defer e.remove(created.Id)
	return e.execute(ctx, created.Id, spec)
}

// execute starts the container and reads its log from the beginning (nothing is missed even if it exits
// at once), enforcing the timeout and the output cap.
func (e *Engine) execute(ctx context.Context, id string, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	lim := spec.Limits
	started := time.Now()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type streams struct {
		out, err  bytes.Buffer
		truncated bool
	}
	var st streams
	var stdinConn net.Conn
	if spec.Stdin != nil {
		c, err := e.attachStdin(ctx, id)
		if err != nil {
			return sandbox.RunResult{}, err
		}
		stdinConn = c
		defer c.Close()
	}
	if err := e.json(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil); err != nil {
		return sandbox.RunResult{}, err
	}
	if stdinConn != nil {
		go func() { // feed the command and close its stdin; the container's output is read from the log
			_, _ = stdinConn.Write(spec.Stdin)
			if cw, ok := stdinConn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			_, _ = io.Copy(io.Discard, stdinConn)
		}()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := e.do(runCtx, http.MethodGet, "/containers/"+id+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}, "follow": {"1"}}, nil, "")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		st.truncated, _ = demux(resp.Body, &st.out, &st.err, lim.OutputBytes, func() { _ = e.kill(id) })
	}()
	res := sandbox.RunResult{}
	waitCh := make(chan int, 1)
	go func() {
		var w struct{ StatusCode int }
		if err := e.json(runCtx, http.MethodPost, "/containers/"+id+"/wait", url.Values{"condition": {"not-running"}}, nil, &w); err != nil {
			w.StatusCode = -1
		}
		waitCh <- w.StatusCode
	}()
	select {
	case res.ExitCode = <-waitCh:
	case <-time.After(spec.Timeout):
		res.TimedOut = true
		_ = e.kill(id)
		select {
		case res.ExitCode = <-waitCh:
		case <-time.After(10 * time.Second):
			res.ExitCode = -1
		}
	case <-ctx.Done():
		_ = e.kill(id)
		return sandbox.RunResult{}, ctx.Err()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
	}
	var insp struct {
		State struct{ OOMKilled bool }
	}
	ictx, icancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer icancel()
	_ = e.json(ictx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &insp)
	res.OOMKilled = insp.State.OOMKilled
	res.Stdout, res.Stderr, res.Truncated = st.out.String(), st.err.String(), st.truncated
	res.Duration = time.Since(started)
	return res, nil
}

func (e *Engine) kill(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := e.json(ctx, http.MethodPost, "/containers/"+id+"/kill", nil, nil, nil)
	if err != nil && (isStatus(err, 409) || isStatus(err, 404)) {
		return nil // already stopped or gone
	}
	return err
}

// demux splits the Engine's multiplexed log stream into stdout and stderr, keeping at most limit bytes
// of each. When one overflows it calls onFull (which kills the container) and reports truncation.
func demux(r io.Reader, stdout, stderr *bytes.Buffer, limit int, onFull func()) (truncated bool, err error) {
	hdr := make([]byte, 8)
	for {
		if _, err = io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.Canceled) {
				err = nil
			}
			return truncated, err
		}
		n := int(uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7]))
		dst := stdout
		if hdr[0] == 2 {
			dst = stderr
		}
		room := limit - dst.Len()
		take := n
		if take > room {
			take = room
		}
		if take > 0 {
			if _, err = io.CopyN(dst, r, int64(take)); err != nil {
				return truncated, err
			}
		}
		if n > take {
			if _, err = io.CopyN(io.Discard, r, int64(n-take)); err != nil {
				return truncated, err
			}
			if !truncated {
				truncated = true
				onFull()
			}
		}
	}
}

// Sweep removes this owner's containers and volumes (after a crash), by label.
func (e *Engine) Sweep(ctx context.Context) error {
	filter, _ := json.Marshal(map[string][]string{"label": {LabelKey + "=" + e.Owner}})
	var cs []struct{ Id string }
	if err := e.json(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filter)}}, nil, &cs); err != nil {
		return err
	}
	for _, c := range cs {
		e.remove(c.Id)
	}
	var ns []struct{ Id string }
	if err := e.json(ctx, http.MethodGet, "/networks", url.Values{"filters": {string(filter)}}, nil, &ns); err == nil {
		for _, n := range ns {
			_ = e.json(ctx, http.MethodPost, "/networks/"+n.Id+"/disconnect", nil, map[string]any{"Container": e.EgressProxy, "Force": true}, nil)
			_ = e.json(ctx, http.MethodDelete, "/networks/"+n.Id, nil, nil, nil)
		}
	}
	var vs struct{ Volumes []struct{ Name string } }
	if err := e.json(ctx, http.MethodGet, "/volumes", url.Values{"filters": {string(filter)}}, nil, &vs); err != nil {
		return err
	}
	for _, v := range vs.Volumes {
		if err := e.DeleteWorkspace(ctx, v.Name); err != nil {
			return err
		}
	}
	return nil
}

// attachStdin opens the Engine's raw attach stream (an HTTP upgrade, which net/http does not expose)
// to the created container's stdin.
func (e *Engine) attachStdin(ctx context.Context, id string) (net.Conn, error) {
	tr, ok := e.http.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		return nil, errors.New("no dialer")
	}
	conn, err := tr.DialContext(ctx, "", "")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) { conn.Close(); return nil, err }
	req := fmt.Sprintf("POST /%s/containers/%s/attach?stream=1&stdin=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n", apiVersion, id)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fail(fmt.Errorf("docker attach: HTTP %d", resp.StatusCode))
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() == 0 {
		return conn, nil
	}
	return &bufConn{Conn: conn, r: br}, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// runNet is a run's own internal network: no route out, only the egress proxy attached. Per run, so that
// concurrent runs of different callers cannot reach each other (an internal network has no gateway, and a
// shared one would let containers talk to one another).
type runNet struct{ id, name string }

func (e *Engine) runNetwork(ctx context.Context) (*runNet, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	name := "idp-sbx-net-" + hex.EncodeToString(b[:])
	var out struct{ Id string }
	if err := e.json(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "Internal": true, "Labels": e.labels(), "CheckDuplicate": true,
		"Options": map[string]string{"com.docker.network.bridge.enable_icc": "true"},
	}, &out); err != nil {
		return nil, err
	}
	n := &runNet{id: out.Id, name: name}
	if err := e.json(ctx, http.MethodPost, "/networks/"+n.id+"/connect", nil, map[string]any{"Container": e.EgressProxy}, nil); err != nil {
		n.remove(e)
		return nil, fmt.Errorf("egress proxy %s: %w", e.EgressProxy, err)
	}
	return n, nil
}

func (n *runNet) remove(e *Engine) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = e.json(ctx, http.MethodPost, "/networks/"+n.id+"/disconnect", nil, map[string]any{"Container": e.EgressProxy, "Force": true}, nil)
	_ = e.json(ctx, http.MethodDelete, "/networks/"+n.id, nil, nil, nil)
}
