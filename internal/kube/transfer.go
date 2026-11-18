package kube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"idp-sandbox/internal/sandbox"
)

// Files of any content and of up to the transfer limit, on a backend that has neither exec nor a byte stream to a pod
// (ADR-0001 D19). In: the file is cut into pieces of chunkIn bytes, each a ConfigMap, all mounted into one Job that
// concatenates them. Out: Jobs print pieces of chunkOut bytes as base64 into their logs, which are text; the pieces are
// put together and checked against the SHA-256 the first Job reported. Nothing but what the backend already may do:
// ConfigMaps, Jobs and pod logs.
const (
	// chunkIn is under the API server's limit for a ConfigMap (1 MiB of keys and values).
	chunkIn = 1000 * 1000
	// chunkOut: base64 of this, with the container runtime's per-line prefix, stays well under the kubelet's default
	// log file size (10 MiB), so no piece is lost to log rotation.
	chunkOut = 3 << 20
	// readJobs is how many pieces are read at a time.
	readJobs = 3
	// transferTimeout bounds one Job of a transfer (mounting tens of ConfigMaps takes longer than a small file's one).
	transferTimeout = 60 * time.Second
)

// big serializes large writes: each holds tens of ConfigMaps for a moment, and the runs namespace has a quota of them.
var big sync.Mutex

// BinaryFiles: this backend moves files byte for byte (sandbox.BinaryFiles).
func (k *Kube) BinaryFiles() bool { return true }

func (k *Kube) transferOp(ctx context.Context, workspace string, cmd []string, chunks [][]byte, outLimit int) (sandbox.RunResult, error) {
	return k.execute(ctx, jobSpec{role: "file", workspace: workspace, image: k.cfg.HelperImage, userCmd: cmd, chunks: chunks,
		limits: sandbox.Limits{MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32, OutputBytes: outLimit,
			WorkspaceBytes: k.cfg.WorkspaceBytes, TmpBytes: 4 << 20}}, transferTimeout)
}

// writeChunked stores data (larger than one ConfigMap holds) at /work/<p>. A file that could not be written whole is
// removed: a document cut short is worse than none.
func (k *Kube) writeChunked(ctx context.Context, workspace, p string, data []byte) error {
	var chunks [][]byte
	for off := 0; off < len(data); off += chunkIn {
		chunks = append(chunks, data[off:min(off+chunkIn, len(data))])
	}
	big.Lock()
	defer big.Unlock()
	const script = `mkdir -p "$(dirname "$1")" && cat /in/c* > "$1" && [ "$(stat -c %s "$1")" = "$2" ] || { c=$?; rm -f "$1"; [ $c -ne 0 ] || c=1; exit $c; }`
	res, err := k.transferOp(ctx, workspace, []string{"sh", "-c", script, "sh", "/work/" + p, strconv.Itoa(len(data))}, chunks, 4096)
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

// regular is the Docker backend's check: a regular file inside the workspace, never a link followed out of it.
const regular = `[ -f "$1" ] && [ ! -L "$1" ] || exit 44
case "$(readlink -f "$1")" in /work/*) ;; *) exit 45;; esac
`

// ReadBytes returns a regular file of up to max bytes exactly as it is stored (sandbox.BinaryReader). ReadFile takes a
// file's bytes straight from the pod log, which is only right for text.
func (k *Kube) ReadBytes(ctx context.Context, workspace, p string, max int64) ([]byte, error) {
	// The first Job reports the size and the checksum, and prints the file itself when it is one piece or less: most
	// documents are, and a Job costs seconds.
	first := regular + `s=$(stat -c %s "$1") && echo "$s" && sha256sum < "$1" | cut -d' ' -f1 && if [ "$s" -le "$2" ]; then base64 "$1"; fi`
	res, err := k.transferOp(ctx, workspace, []string{"sh", "-c", first, "sh", "/work/" + p, strconv.Itoa(chunkOut)}, nil, base64Room(chunkOut)+256)
	if err != nil {
		return nil, err
	}
	switch {
	case res.ExitCode == 44 || res.ExitCode == 45:
		return nil, sandbox.ErrNoFile
	case res.ExitCode != 0 || res.TimedOut || res.Truncated:
		return nil, errors.New("reading the file failed")
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) < 2 || len(fields[1]) != 64 {
		return nil, errors.New("reading the file failed: its size and checksum could not be read")
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || size < 0 {
		return nil, errors.New("reading the file failed: its size could not be read")
	}
	if size > max {
		return nil, sandbox.ErrFileTooLarge
	}
	if size <= chunkOut {
		whole, err := base64.StdEncoding.DecodeString(strings.Join(fields[2:], ""))
		if sum := sha256.Sum256(whole); err != nil || int64(len(whole)) != size || hex.EncodeToString(sum[:]) != fields[1] {
			return nil, fmt.Errorf("%w: the file changed while it was being read", sandbox.ErrInvalid)
		}
		return whole, nil
	}
	out := make([]byte, size)
	pieces := int((size + chunkOut - 1) / chunkOut)
	errs := make(chan error, pieces)
	slots := make(chan struct{}, readJobs)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < pieces; i++ {
		go func(i int) {
			slots <- struct{}{}
			defer func() { <-slots }()
			if ctx.Err() != nil {
				errs <- ctx.Err()
				return
			}
			off := int64(i) * chunkOut
			want := min(int64(chunkOut), size-off)
			piece, err := k.readPiece(ctx, workspace, p, off, want)
			if err == nil {
				copy(out[off:], piece)
			}
			errs <- err
		}(i)
	}
	// every piece is waited for, also after one has failed: a reader that is still at work would leave its Job behind
	var failed error
	for i := 0; i < pieces; i++ {
		if err := <-errs; err != nil && failed == nil {
			failed = err
			cancel()
		}
	}
	if failed != nil {
		return nil, failed
	}
	if sum := sha256.Sum256(out); hex.EncodeToString(sum[:]) != fields[1] {
		return nil, fmt.Errorf("%w: the file changed while it was being read", sandbox.ErrInvalid)
	}
	return out, nil
}

// base64Room is how much output n bytes need as base64: 4/3 of them, plus a newline per line of whatever width this
// image's base64 prints.
func base64Room(n int) int { return n*4/3 + n/32 + 4096 }

// readPiece reads want bytes at off (a multiple of 1 MiB) as base64 from a Job's log.
func (k *Kube) readPiece(ctx context.Context, workspace, p string, off, want int64) ([]byte, error) {
	blocks := (want + (1 << 20) - 1) >> 20
	limit := base64Room(int(blocks << 20))
	res, err := k.transferOp(ctx, workspace, []string{"sh", "-c", regular + `dd if="$1" bs=1048576 skip="$2" count="$3" 2>/dev/null | base64`,
		"sh", "/work/" + p, strconv.FormatInt(off>>20, 10), strconv.FormatInt(blocks, 10)}, nil, limit)
	if err != nil {
		return nil, err
	}
	switch {
	case res.ExitCode == 44 || res.ExitCode == 45:
		return nil, sandbox.ErrNoFile
	case res.ExitCode != 0 || res.TimedOut || res.Truncated:
		return nil, errors.New("reading the file failed")
	}
	text := bytes.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' {
			return -1
		}
		return r
	}, []byte(res.Stdout))
	piece := make([]byte, base64.StdEncoding.DecodedLen(len(text)))
	n, err := base64.StdEncoding.Decode(piece, text)
	if err != nil || int64(n) != want {
		return nil, fmt.Errorf("%w: the file changed while it was being read", sandbox.ErrInvalid)
	}
	return piece[:n], nil
}
