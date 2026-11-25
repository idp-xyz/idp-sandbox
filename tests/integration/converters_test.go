package integration

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"idp-sandbox/internal/sandbox"
)

// The converters of the knowledge-base ingestion (ADR-0001 D25) in the office profile, under the isolation and the limits
// of a default run (20 s, 256 MiB): documents that officecli and md2pdf make, and a workbook with 120,000 rows that a
// run writes itself, become Markdown with locators; what cannot be converted fails with its own exit code and no file.
// Skipped unless IDP_SANDBOX_OFFICE_IMAGE names the image of deploy/office/build.sh.
func TestConvertersInTheOfficeProfile(t *testing.T) {
	img := os.Getenv("IDP_SANDBOX_OFFICE_IMAGE")
	if img == "" {
		t.Skip("IDP_SANDBOX_OFFICE_IMAGE not set (bash deploy/office/build.sh)")
	}
	m := setup(t, func(c *sandbox.Config) {
		c.Languages["office"] = sandbox.OfficeLanguage(img)
		c.Limits.Timeout, c.Limits.MemoryBytes = 60*time.Second, 256<<20
		c.Limits.WorkspaceBytes = 64 << 20
		c.Limits.OutputBytes = 64 << 10
	})
	id := ws(t, m)
	ctx := context.Background()
	gen, err := os.ReadFile(filepath.Join("..", "..", "internal", "office", "testdata", "genoffice.py"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutBytes(ctx, owner, id, "gen/genoffice.py", gen); err != nil {
		t.Fatal(err)
	}

	// officecli's own documents: a Word file with headings, paragraphs and a table, a deck, a workbook
	r := run(t, m, id, "office", `set -e
cat > m.md <<'MD'
# 制度手册

## 第1章 差旅

住宿限额 600.00 元，生效日期 2026-07-21。

| 项目 | 类别 | 限额 |
|---|---|---|
| 住宿 | 差旅 | 600.00 |
| 餐补 | 差旅 | 100.00 |

### 1.1 例外

经理批准。
MD
officecli create a.docx >/dev/null
officecli add a.docx /body --type markdown --prop src=m.md >/dev/null
officecli create d.pptx >/dev/null
officecli add d.pptx / --type slide >/dev/null
officecli add d.pptx "/slide[1]" --type shape --prop text="第三季度汇报" >/dev/null
officecli add d.pptx / --type slide >/dev/null
officecli add d.pptx "/slide[2]" --type shape --prop text="下一步计划" >/dev/null
officecli create s.xlsx >/dev/null
officecli batch s.xlsx --commands '[{"command":"set","path":"/Sheet1/A1","props":{"value":"营收"}},{"command":"set","path":"/Sheet1/B1","props":{"value":"120"}}]' >/dev/null
for f in a.docx d.pptx s.xlsx; do ooxml2md $f -o ${f%.*}.out.md; done
cat a.out.md d.out.md s.out.md`)
	if r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"# 制度手册", "## 第1章 差旅", "### 1.1 例外", "<!-- table: 3 rows x 3 cols -->", "| 住宿 | 差旅 | 600.00 |",
		"<!-- slide: 1 -->", "第三季度汇报", "<!-- slide: 2 -->", "下一步计划", "<!-- sheet: Sheet1 range: A1:B1 -->", "| 1 | 营收 | 120 |"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("%q missing: %s", want, r.Stdout)
		}
	}

	// PDFs from md2pdf: a page marker for every page, and a page without text is flagged; a scan, a protected and a broken file fail
	r = run(t, m, id, "office", `python3 gen/genoffice.py docx word.docx
for i in $(seq 1 40); do printf "## 第 $i 章\n\n本章规定 $i 的限额 %d.50 元。\n\n" $i; done > long.md
md2pdf long.md long.pdf >/dev/null
pdftext long.pdf --markdown long.out.md; echo "long=$?"
python3 - <<'PY'
import pypdf
from PIL import Image as Picture
from reportlab.platypus import Image, SimpleDocTemplate, Paragraph, PageBreak
from reportlab.lib.styles import getSampleStyleSheet
Picture.new("RGB", (400, 150), "white").save("page.png")
SimpleDocTemplate("scan.pdf").build([Image("page.png", width=300, height=110)])
st = getSampleStyleSheet()["Normal"]
SimpleDocTemplate("mixed.pdf").build([Paragraph("page one has text 321.50", st), PageBreak(), Image("page.png", width=300, height=110), PageBreak(), Paragraph("page three has text", st)])
w = pypdf.PdfWriter(clone_from="long.pdf")
w.encrypt(user_password="s3cret", owner_password="owner", algorithm="AES-256")
w.write("locked.pdf")
PY
head -c 1500 long.pdf > cut.pdf
pdftext mixed.pdf --markdown mixed.out.md; echo "mixed=$?"
pdftext scan.pdf --markdown scan.out.md; echo "scan=$?"
pdftext locked.pdf --markdown locked.out.md; echo "locked=$?"
pdftext locked.pdf --password s3cret --markdown unlocked.out.md; echo "unlocked=$?"
pdftext cut.pdf --markdown cut.out.md; echo "cut=$?"
pdftext nothing.pdf --markdown nothing.out.md; echo "missing=$?"
pdftext long.pdf --pages 9 --markdown bad.out.md; echo "range=$?"
ls *.out.md | tr '\n' ' '; echo
grep -c '^<!-- page: ' long.out.md; cat mixed.out.md`)
	if r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"long=0", "mixed=0", "scan=6", "locked=3", "unlocked=0", "cut=4", "missing=1", "range=2"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("%q missing: %+v", want, r)
		}
	}
	if regexp.MustCompile(`(?m)^(scan|locked|cut|bad|nothing)\.out\.md`).MatchString(r.Stdout) {
		t.Errorf("a failed conversion left a file: %s", r.Stdout)
	}
	for _, want := range []string{"<!-- page: 1 -->\npage one has text 321.50", "<!-- page: 2 -->\n<!-- no-text: page 2, no text on this page", "<!-- page: 3 -->\npage three has text"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("%q missing: %s", want, r.Stdout)
		}
	}
	for _, want := range []string{"pdftext: error: empty: no text on any of the 1 page", "pdftext: error: unsupported: locked.pdf is protected", "pdftext: error: corrupt: cut.pdf is not a PDF"} {
		if !strings.Contains(r.Stderr, want) {
			t.Fatalf("%q missing: %s", want, r.Stderr)
		}
	}
	if strings.Count(r.Stdout, "<!-- page: ") < 3 {
		t.Fatalf("no page markers: %s", r.Stdout)
	}

	// 120,000 rows in a default run: officecli reads this into 736 MB and is killed at 256 MiB with an empty result and exit 0;
	// ooxml2md writes the first 10,000 rows with the notice
	r = run(t, m, id, "office", `python3 gen/genoffice.py bigxlsx big.xlsx 120000 && ls -l big.xlsx | awk '{print $5}' && python3 -c "
import resource, subprocess
r = subprocess.run(['ooxml2md', 'big.xlsx', '-o', 'big.md'], capture_output=True, text=True)
print('exit', r.returncode, 'rss_mb', resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss // 1024)
" && tail -n 2 big.md && grep -c '^| ' big.md`)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "exit 0") || !strings.Contains(r.Stdout, "<!-- truncated: sheet 订单, rows kept 10000 of 120000 -->") || r.OOMKilled || r.TimedOut {
		t.Fatalf("%+v", r)
	}
	// a zip bomb is refused by the transfer check, and the converter would not survive it either: nothing is left running
	r = run(t, m, id, "office", `python3 gen/genoffice.py bomb bomb.docx 300; ooxml2md bomb.docx -o bomb.md; echo "bomb=$?"; ls bomb.md 2>&1 | head -1`)
	if !strings.Contains(r.Stdout, "bomb=5") || strings.Contains(r.Stdout, "\nbomb.md") || r.OOMKilled {
		t.Fatalf("%+v", r)
	}
}
