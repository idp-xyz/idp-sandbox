package kube

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"idp-sandbox/internal/sandbox"
)

// fileOp runs a short command in the workspace as the sandbox user, with the same hardening as a run.
func (k *Kube) fileOp(ctx context.Context, workspace string, cmd []string, input []byte, outLimit int) (sandbox.RunResult, error) {
	return k.execute(ctx, jobSpec{role: "file", workspace: workspace, image: k.cfg.HelperImage, userCmd: cmd, input: input,
		limits: sandbox.Limits{MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32, OutputBytes: outLimit,
			WorkspaceBytes: k.cfg.WorkspaceBytes, TmpBytes: 4 << 20}}, 20*time.Second)
}

// WriteFile stores a regular file at /work/<p>. The bytes are mounted from a ConfigMap at /in/data and
// copied by a short Job (there is no exec to stream them in).
func (k *Kube) WriteFile(ctx context.Context, workspace, p string, data []byte) error {
	if data == nil {
		data = []byte{}
	}
	res, err := k.fileOp(ctx, workspace, []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat ` + inputPath + ` > "$1"`, "sh", "/work/" + p}, data, 4096)
	if err != nil {
		return err
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

// ReadFile returns a regular file; a symlink, directory or device is refused, never followed. The
// script is the Docker backend's.
func (k *Kube) ReadFile(ctx context.Context, workspace, p string, max int64) ([]byte, error) {
	const script = `[ -f "$1" ] && [ ! -L "$1" ] || exit 44
case "$(readlink -f "$1")" in /work/*) ;; *) exit 45;; esac
cat "$1"`
	res, err := k.fileOp(ctx, workspace, []string{"sh", "-c", script, "sh", "/work/" + p}, nil, int(max)+1)
	if err != nil {
		return nil, err
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
func (k *Kube) ListFiles(ctx context.Context, workspace string, max int) ([]sandbox.FileInfo, error) {
	res, err := k.fileOp(ctx, workspace, []string{"sh", "-c", fmt.Sprintf(`cd /work && find . -type f -exec stat -c '%%s %%n' {} + | head -n %d`, max)}, nil, 1<<20)
	if err != nil {
		return nil, err
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
