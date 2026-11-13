package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"idp-sandbox/internal/files"
	"idp-sandbox/internal/sandbox"
)

// Files a member attaches or receives move byte for byte (ADR-0001 D15): binary content, larger than the log driver keeps
// (1 MB), comes in on stdin and goes out on the attach stream unchanged.
func TestBinaryFilesMoveUnchanged(t *testing.T) {
	m := setup(t, func(c *sandbox.Config) { c.MaxTransferBytes = 6 << 20 })
	if !m.TransfersSupported() {
		t.Fatal("the docker engine should move files byte for byte")
	}
	id := ws(t, m)
	data := make([]byte, 3<<20)
	_, _ = rand.Read(data)
	copy(data, "\x00\r\n\xff\xfe\x1b[2J") // NUL, CRLF, invalid UTF-8, a terminal escape
	if err := m.PutBytes(context.Background(), owner, id, "in/blob.bin", data); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	r := run(t, m, id, "python", `import hashlib
d = open('in/blob.bin','rb').read()
print(hashlib.sha256(d).hexdigest())
open('out.bin','wb').write(d[::-1])`)
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != hex.EncodeToString(sum[:]) {
		t.Fatalf("the run saw other bytes: %+v", r)
	}
	got, err := m.GetBytes(context.Background(), owner, id, "out.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("%d bytes came back of %d", len(got), len(data))
	}
	for i := range got {
		if got[i] != data[len(data)-1-i] {
			t.Fatalf("byte %d differs", i)
		}
	}
	// over the transfer limit
	if r := run(t, m, id, "shell", `rm in/blob.bin out.bin && head -c 7340032 /dev/zero > big.bin`); r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := m.GetBytes(context.Background(), owner, id, "big.bin"); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Fatalf("a file over the limit: %v", err)
	}
	if err := m.PutBytes(context.Background(), owner, id, "x.bin", make([]byte, 6<<20+1)); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Fatalf("a file over the limit came in: %v", err)
	}
	// someone else cannot take a file out of the workspace
	if _, err := m.GetBytes(context.Background(), "acme/mallory", id, "out.bin"); err == nil {
		t.Fatal("another caller read the workspace")
	}
}

// A document made in the office profile goes out as a valid package and comes back in usable.
func TestOfficeDocumentsRoundTrip(t *testing.T) {
	img := os.Getenv("IDP_SANDBOX_OFFICE_IMAGE")
	if img == "" {
		t.Skip("IDP_SANDBOX_OFFICE_IMAGE not set (bash deploy/office/build.sh)")
	}
	m := setup(t, func(c *sandbox.Config) {
		c.Languages["office"] = sandbox.OfficeLanguage(img)
		c.Limits.Timeout, c.Limits.MemoryBytes = 30*time.Second, 256<<20
		c.MaxTransferBytes = 6 << 20
	})
	id := ws(t, m)
	if r := run(t, m, id, "office", `set -e
officecli create q.docx
officecli add q.docx /body --type paragraph --prop text="报价：第一版"`); r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	doc, err := m.GetBytes(context.Background(), owner, id, "q.docx")
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Check("q.docx", doc); err != nil {
		t.Fatalf("the exported document is refused: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		found = found || f.Name == "word/document.xml"
	}
	if !found {
		t.Fatal("no word/document.xml")
	}
	// the member's edited copy comes back under another name and is edited again
	if err := m.PutBytes(context.Background(), owner, id, "in/q-member.docx", doc); err != nil {
		t.Fatal(err)
	}
	r := run(t, m, id, "office", `set -e
officecli set in/q-member.docx "/body/p[1]" --prop text="报价：第二版"
officecli validate in/q-member.docx
officecli view in/q-member.docx text`)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "报价：第二版") {
		t.Fatalf("%+v", r)
	}
}
