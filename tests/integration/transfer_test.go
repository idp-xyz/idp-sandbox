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
	"regexp"
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

// PDF and Markdown in the office profile (ADR-0001 D17), under the isolation and the default limits of a run: a member's
// Markdown comes in, md2pdf sets it as a PDF with Chinese and a table, the PDF goes out as a file the transfer check
// accepts, and comes back in to be read by pdftext in a later run. A protected file, a page without text and a broken
// file are told apart.
func TestPDFAndMarkdownRoundTrip(t *testing.T) {
	img := os.Getenv("IDP_SANDBOX_OFFICE_IMAGE")
	if img == "" {
		t.Skip("IDP_SANDBOX_OFFICE_IMAGE not set (bash deploy/office/build.sh)")
	}
	m := setup(t, func(c *sandbox.Config) {
		c.Languages["office"] = sandbox.OfficeLanguage(img)
		c.Limits.Timeout, c.Limits.MemoryBytes = 20*time.Second, 256<<20 // what a deployment gives a run by default
		c.MaxTransferBytes = 6 << 20
	})
	id := ws(t, m)
	ctx := context.Background()
	md := "# 报价单\n\n含税总价 **1,280.50** 元，明细见下表。\n\n| 渠道 | 重量段 | 价格 |\n|:--|:-:|--:|\n| 空运 | 1 LB | 12.50 |\n| 海运 | 150 LB | 7.25 |\n"
	if err := files.Check("报价单.md", []byte(md)); err != nil {
		t.Fatalf("the Markdown is refused: %v", err)
	}
	if err := m.PutBytes(ctx, owner, id, "in/报价单.md", []byte(md)); err != nil {
		t.Fatal(err)
	}
	r := run(t, m, id, "office", `md2pdf in/报价单.md out/报价单.pdf && pdftext --check out/报价单.pdf && ls -A /tmp | wc -l`)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "wrote out/报价单.pdf: 1 page") || !strings.Contains(r.Stdout, "\n报价单.pdf: 1 page\n") ||
		!strings.HasSuffix(strings.TrimSpace(r.Stdout), "0") {
		t.Fatalf("%+v", r)
	}
	pdf, err := m.GetBytes(ctx, owner, id, "out/报价单.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Check("报价单.pdf", pdf); err != nil || len(pdf) < 5000 { // the font's glyphs are in the file
		t.Fatalf("the exported PDF: %d bytes, %v", len(pdf), err)
	}
	// it comes back as a member's attachment, and a later run reads it with each row of the table on one line
	if err := m.PutBytes(ctx, owner, id, "in/来件.pdf", pdf); err != nil {
		t.Fatal(err)
	}
	r = run(t, m, id, "office", `pdftext in/来件.pdf --layout`)
	for _, want := range []string{"来件.pdf: 1 page", "--- page 1 ---", "报价单", "含税总价", "1,280.50"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("%q missing: %+v", want, r)
		}
	}
	for _, row := range []string{`空运\s+1 LB\s+12\.50`, `海运\s+150 LB\s+7\.25`} {
		if !regexp.MustCompile(row).MatchString(r.Stdout) {
			t.Fatalf("no row %s: %+v", row, r)
		}
	}
	// a file with a password, a page that is only a picture, and a file cut short
	r = run(t, m, id, "office", `python3 - <<'PY'
import pypdf
from PIL import Image as Picture
from reportlab.platypus import Image, SimpleDocTemplate
Picture.new("RGB", (400, 150), "white").save("page.png")
SimpleDocTemplate("scan.pdf").build([Image("page.png", width=300, height=110)])
w = pypdf.PdfWriter(clone_from="out/报价单.pdf")
w.encrypt(user_password="s3cret", owner_password="owner", algorithm="AES-256")
w.write("locked.pdf")
PY
pdftext locked.pdf; echo "locked=$?"
pdftext locked.pdf --password s3cret --max-bytes 400 | head -3
pdftext scan.pdf
head -c 2000 out/报价单.pdf > cut.pdf; pdftext --check cut.pdf; echo "cut=$?"`)
	for _, want := range []string{"locked=1", "locked.pdf: 1 page\n--- page 1 ---\n报价单", "no text on this page: it is a picture (a scan)", "cut=1"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("%q missing: %+v", want, r)
		}
	}
	for _, want := range []string{"locked.pdf is protected by a password; give it with --password", "cut.pdf is not a PDF file that can be read"} {
		if !strings.Contains(r.Stderr, want) {
			t.Fatalf("%q missing: %+v", want, r)
		}
	}
}
