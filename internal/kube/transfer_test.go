package kube

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"idp-sandbox/internal/sandbox"
)

var (
	_ sandbox.BinaryFiles  = (*Kube)(nil)
	_ sandbox.BinaryReader = (*Kube)(nil)
)

// shell is what the helper image's shell does with the transfer scripts, on a file system of the test's own: it puts
// the mounted pieces together, reports size and checksum, and prints pieces as base64 in lines, like base64(1).
type shell struct {
	f     *fakeAPI
	files map[string][]byte
	// mutate, if set, changes what a reading Job sees (the file changing under the reader)
	mutate func(piece int, b []byte) []byte
	reads  int
}

func (s *shell) behave(job obj, cmd []string) outcome {
	script, args := cmd[6], cmd[8:] // sh -c <wrapper> sh sh -c <script> sh <args...>
	switch {
	case strings.Contains(script, "cat /in/c*"):
		type piece struct {
			path string
			data []byte
		}
		var pieces []piece
		for _, v := range at(job, "spec", "template", "spec", "volumes").([]any) {
			if v.(obj)["name"] != "in" {
				continue
			}
			for _, src := range at(v, "projected", "sources").([]any) {
				name := at(src, "configMap", "name").(string)
				cm, ok := s.f.objs["configmaps"][name]
				if !ok {
					return outcome{exit: 1, stderr: "the ConfigMap " + name + " is not there"}
				}
				raw, _ := base64.StdEncoding.DecodeString(at(cm, "binaryData", "data").(string))
				pieces = append(pieces, piece{at(src, "configMap", "items").([]any)[0].(obj)["path"].(string), raw})
			}
		}
		sort.Slice(pieces, func(i, j int) bool { return pieces[i].path < pieces[j].path }) // the shell's glob order
		var all []byte
		for _, p := range pieces {
			all = append(all, p.data...)
		}
		if strconv.Itoa(len(all)) != args[1] {
			return outcome{exit: 1}
		}
		s.files[args[0]] = all
		return outcome{}
	case strings.Contains(script, "sha256sum"):
		b, ok := s.files[args[0]]
		if !ok {
			return outcome{exit: 44}
		}
		sum := sha256.Sum256(b)
		out := strconv.Itoa(len(b)) + "\n" + hex.EncodeToString(sum[:]) + "\n"
		if whole, _ := strconv.Atoi(args[1]); len(b) <= whole { // a file of one piece or less comes with its checksum
			if s.mutate != nil {
				b = s.mutate(s.reads, b)
			}
			s.reads++
			out += lines76(base64.StdEncoding.EncodeToString(b))
		}
		return outcome{stdout: out}
	case strings.Contains(script, "dd if="):
		b, ok := s.files[args[0]]
		if !ok {
			return outcome{exit: 44}
		}
		skip, _ := strconv.Atoi(args[1])
		count, _ := strconv.Atoi(args[2])
		piece := b[min(skip<<20, len(b)):min((skip+count)<<20, len(b))]
		if s.mutate != nil {
			piece = s.mutate(s.reads, piece)
		}
		s.reads++
		return outcome{stdout: lines76(base64.StdEncoding.EncodeToString(piece))}
	}
	return outcome{exit: 2, stderr: "the test's shell does not know this script"}
}

// lines76 is base64(1)'s output: lines of 76 characters.
func lines76(enc string) string {
	var out strings.Builder
	for len(enc) > 76 {
		out.WriteString(enc[:76] + "\n")
		enc = enc[76:]
	}
	out.WriteString(enc + "\n")
	return out.String()
}

func transferSetup(t *testing.T, maxWrite int64) (*Kube, *fakeAPI, *shell) {
	t.Helper()
	sh := &shell{files: map[string][]byte{}}
	f := newFake(t, nil)
	sh.f = f
	f.behave = sh.behave
	k := f.newKube(t, func(c *Config) { c.MaxWriteBytes = maxWrite })
	if err := k.CreateWorkspace(context.Background(), ws, 64<<20); err != nil {
		t.Fatal(err)
	}
	return k, f, sh
}

// D19: a file of any content, larger than one ConfigMap and larger than one log read, goes in and comes back unchanged.
func TestBinaryFilesGoInAndOutInPieces(t *testing.T) {
	k, f, sh := transferSetup(t, 16<<20)
	data := make([]byte, 7<<20+12345) // 8 pieces in, 3 pieces out
	_, _ = rand.Read(data)
	copy(data, []byte{0, 0xff, '\r', '\n', 0xc3, 0x28}) // NUL, a non-UTF-8 byte, CRLF, an invalid sequence
	if err := k.WriteFile(context.Background(), ws, "in/报价单.docx", data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sh.files["/work/in/报价单.docx"], data) {
		t.Fatal("what the Job put together is not the file")
	}
	cms := f.created("configmaps")
	if want := (len(data) + chunkIn - 1) / chunkIn; len(cms) != want {
		t.Fatalf("%d ConfigMaps for %d bytes, want %d", len(cms), len(data), want)
	}
	for _, cm := range cms {
		raw, _ := base64.StdEncoding.DecodeString(at(cm, "binaryData", "data").(string))
		// under the API server's limit of 1 MiB for the keys and values of a ConfigMap
		if len(raw)+len("data") >= 1<<20 || at(cm, "metadata", "labels", LabelKey) != "me" || at(cm, "metadata", "labels", RoleLabel) != "file" ||
			at(cm, "metadata", "labels", WorkspaceLabel) != ws {
			t.Fatalf("a piece of %d bytes labelled %v", len(raw), at(cm, "metadata", "labels"))
		}
	}
	job := f.created("jobs")[0]
	pod := at(job, "spec", "template", "spec").(obj)
	c := pod["containers"].([]any)[0].(obj)
	if at(c, "securityContext", "readOnlyRootFilesystem") != true || at(pod, "automountServiceAccountToken") != false || at(c, "securityContext", "runAsUser") != float64(sandboxUID) {
		t.Error("the Job that writes the file is not hardened like a run")
	}
	assertClean(t, f) // every piece is deleted with its Job

	got, err := k.ReadBytes(context.Background(), ws, "in/报价单.docx", 16<<20)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the file came back different: %d bytes, %v", len(got), err)
	}
	if jobs := len(f.created("jobs")); jobs != 1+1+3 { // the write, the size and checksum, three pieces
		t.Errorf("%d Jobs", jobs)
	}
	for _, j := range f.created("jobs")[1:] {
		if cmd := cmdOf(j); !strings.Contains(cmd[6], `[ -f "$1" ] && [ ! -L "$1" ] || exit 44`) || !strings.Contains(cmd[6], `/work/*) ;; *) exit 45;; esac`) {
			t.Errorf("a reading Job does not refuse links: %s", cmd[6])
		}
	}
	assertClean(t, f)
}

func TestBinaryReadsRefuseWhatTheyCannotReturnWhole(t *testing.T) {
	k, f, sh := transferSetup(t, 16<<20)
	ctx := context.Background()
	data := make([]byte, 4<<20)
	_, _ = rand.Read(data)
	sh.files["/work/a.bin"], sh.files["/work/empty"] = data, []byte{}

	if b, err := k.ReadBytes(ctx, ws, "empty", 1<<20); err != nil || len(b) != 0 {
		t.Errorf("an empty file: %d %v", len(b), err)
	}
	// a file of one piece or less takes one Job, not two: most documents are that small
	small := make([]byte, 200<<10)
	_, _ = rand.Read(small)
	sh.files["/work/small.docx"] = small
	jobs := len(f.created("jobs"))
	if b, err := k.ReadBytes(ctx, ws, "small.docx", 1<<20); err != nil || !bytes.Equal(b, small) || len(f.created("jobs"))-jobs != 1 {
		t.Errorf("a small file: %d bytes in %d Jobs, %v", len(b), len(f.created("jobs"))-jobs, err)
	}
	sh.mutate = func(_ int, b []byte) []byte { c := append([]byte(nil), b...); c[7] ^= 1; return c }
	if _, err := k.ReadBytes(ctx, ws, "small.docx", 1<<20); !errors.Is(err, sandbox.ErrInvalid) {
		t.Errorf("a small file whose bytes are not the ones the checksum is of: %v", err)
	}
	sh.mutate = nil
	before := len(f.created("jobs"))
	if _, err := k.ReadBytes(ctx, ws, "a.bin", 1<<20); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("a file over the limit: %v", err)
	}
	if n := len(f.created("jobs")) - before; n != 1 {
		t.Errorf("%d Jobs for a file that is refused for its size (want the one that reports it)", n)
	}
	if _, err := k.ReadBytes(ctx, ws, "missing", 1<<20); !errors.Is(err, sandbox.ErrNoFile) {
		t.Errorf("no such file: %v", err)
	}
	// the file changes between the checksum and a piece: bytes replaced, or the file cut short
	sh.reads = 0
	sh.mutate = func(piece int, b []byte) []byte {
		if piece == 1 {
			b = append([]byte(nil), b...)
			b[0] ^= 0xff
		}
		return b
	}
	if _, err := k.ReadBytes(ctx, ws, "a.bin", 16<<20); !errors.Is(err, sandbox.ErrInvalid) || !strings.Contains(err.Error(), "changed") {
		t.Errorf("a file rewritten under the reader: %v", err)
	}
	sh.mutate = func(_ int, b []byte) []byte { return b[:len(b)/2] }
	if _, err := k.ReadBytes(ctx, ws, "a.bin", 16<<20); !errors.Is(err, sandbox.ErrInvalid) {
		t.Errorf("a file cut short under the reader: %v", err)
	}
	assertClean(t, f)
}

func TestBinaryWritesAreBoundedAndLeaveNothingBehind(t *testing.T) {
	k, f, sh := transferSetup(t, 4<<20)
	ctx := context.Background()
	before := f.called("POST")
	if err := k.WriteFile(ctx, ws, "big", make([]byte, 4<<20+1)); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if f.called("POST") != before {
		t.Error("a file over the limit reached the cluster")
	}
	// a workspace that is full: the error says so, and the pieces are deleted all the same
	f.mu.Lock()
	f.behave = fixed(outcome{exit: 1, stderr: "cat: write error: No space left on device\n"})
	f.mu.Unlock()
	if err := k.WriteFile(ctx, ws, "full", make([]byte, 3<<20)); !errors.Is(err, sandbox.ErrInvalid) || !strings.Contains(err.Error(), "full") {
		t.Errorf("a full workspace: %v", err)
	}
	if n := len(f.created("configmaps")); n != 4 {
		t.Errorf("%d pieces", n)
	}
	assertClean(t, f)
	// what fits one ConfigMap still travels in one, as before
	f.mu.Lock()
	f.behave = fixed(outcome{})
	f.mu.Unlock()
	n := len(f.created("configmaps"))
	if err := k.WriteFile(ctx, ws, "small.png", []byte{0x89, 'P', 'N', 'G', 0, 0xff}); err != nil {
		t.Fatal(err)
	}
	if got := f.created("configmaps"); len(got) != n+1 || at(got[n], "binaryData", "data") != base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0, 0xff}) {
		t.Errorf("a small file: %d ConfigMaps", len(got)-n)
	}
	_ = sh
	assertClean(t, f)
}
