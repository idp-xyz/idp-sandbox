package office_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The documents of these tests are made by testdata/genoffice.py with the standard library (invented text, no business
// documents), and read by deploy/office/ooxml2md as the office image runs it. The documents officecli makes and the PDFs
// md2pdf makes are tried in tests/integration (they need the image).

func genoffice(t *testing.T, dir, kind, name string, n ...int) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	out := filepath.Join(dir, name)
	args := []string{filepath.Join(filepath.Dir(file), "testdata", "genoffice.py"), kind, out}
	for _, x := range n {
		args = append(args, strconv.Itoa(x))
	}
	if code, o := exits(t, args...); code != 0 {
		t.Fatalf("genoffice %s: exit %d: %s", kind, code, o)
	}
	return out
}

// convert runs ooxml2md and returns the exit code, what it printed, and the Markdown (empty when there is no output file).
func convert(t *testing.T, in string, extra ...string) (int, string, string) {
	t.Helper()
	out := in + ".md"
	args := append([]string{tool(t, "ooxml2md"), in, "-o", out}, extra...)
	code, printed := exits(t, args...)
	md, err := os.ReadFile(out)
	if err != nil {
		md = nil
	}
	if _, err := os.Stat(out + ".part"); err == nil {
		t.Errorf("a temporary file is left behind after %v", args)
	}
	return code, printed, string(md)
}

func mustConvert(t *testing.T, in string, extra ...string) string {
	t.Helper()
	code, printed, md := convert(t, in, extra...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, printed)
	}
	return md
}

// in asserts that the lines appear in the text in this order.
func inOrder(t *testing.T, md string, lines ...string) {
	t.Helper()
	at := 0
	for _, l := range lines {
		i := strings.Index(md[at:], l)
		if i < 0 {
			t.Fatalf("%q is missing, or out of order, in:\n%s", l, md)
		}
		at += i + len(l)
	}
}

// A Word file: headings with their levels (by outline level, English and Chinese style names, and through basedOn),
// paragraphs, numbered and bulleted lists with nesting, tables with their cell text (a merged cell keeps the columns),
// explicit page breaks, a content control, a text box (once, not its fallback copy), and deleted text left out.
func TestDocxHasHeadingsListsTablesAndPages(t *testing.T) {
	python(t)
	md := mustConvert(t, genoffice(t, t.TempDir(), "docx", "a.docx"))
	inOrder(t, md,
		"<!-- page: 1 -->", "# 制度手册", "本制度适用于全体员工。限额 1,280.50 元，生效日期 2026-07-21。", "## 差旅",
		"<!-- table: 3 rows x 4 cols -->", "| 类别 | 一线城市 |  | 备注 |", "|---|---|---|---|", "| 住宿 | 600.00 | 400.00 | 凭发票 |", "| 餐补 | 100 | 80 | 每日 |",
		"### 条件", "1. 第一项\n  1. 第一项的子项\n2. 第二项", "- 要点甲\n  - 要点乙",
		"\\# 看起来像标题的正文", "跨页前的文字", "<!-- page: 2 -->", "跨页后的文字", "<!-- page: 3 -->", "## 自定义标题", "内容控件里的段落", "文本框里的字", "保留的字")
	if n := strings.Count(md, "文本框里的字"); n != 1 {
		t.Errorf("the text box is written %d times", n)
	}
	if strings.Contains(md, "被删除的字") {
		t.Error("deleted text is in the result")
	}
	if strings.Contains(md, "<!-- page: 4 -->") {
		t.Error("a page that does not exist")
	}
}

func TestPptxHasSlidesInPresentationOrder(t *testing.T) {
	python(t)
	md := mustConvert(t, genoffice(t, t.TempDir(), "pptx", "a.pptx"))
	// slide2.xml comes first in the presentation, so it is slide 1
	inOrder(t, md, "<!-- slide: 1 -->", "# 目录", "- 第一部分", "- 第二部分", "<!-- notes: slide 1 -->", "备注文字：先讲目录",
		"<!-- slide: 2 -->", "# 季度回顾", "- 收入同比增长 12.5%", "<!-- table: 2 rows x 2 cols -->", "| 项目 | 金额 |", "| 收入 | 1,000.00 |", "组里的文字")
}

func TestXlsxHasSheetsRangesAndRowNumbers(t *testing.T) {
	python(t)
	md := mustConvert(t, genoffice(t, t.TempDir(), "xlsx", "a.xlsx"))
	inOrder(t, md,
		"<!-- sheet: 价格 range: A1:C5 -->", "| row | A | B | C |", "| 1 | 名称 | 金额 | 日期 |", "| 2 | 空运 | 1280.5 | 2026-10-18 |", "| 3 | 海运 | 3 | 2026-10-19 |",
		"| 5 |  |  | 跨过空行 |", "<!-- skipped: sheet 隐藏, hidden -->", "<!-- sheet: 空 empty -->", "<!-- sheet: A--&gt;B range: A1:B1 -->", "| 1 | x | True |")
	if strings.Contains(md, "秘密") {
		t.Error("a hidden sheet is converted")
	}
	if strings.Contains(md, "| 4 |") {
		t.Error("an empty row is written")
	}
}

// Text in a file cannot pass for a marker: the markers are the converter's alone.
func TestTextInTheFileCannotForgeAMarker(t *testing.T) {
	python(t)
	md := mustConvert(t, genoffice(t, t.TempDir(), "markers", "m.docx"))
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "<!--") && !strings.HasPrefix(line, "<!-- table: 1 rows x 2 cols -->") {
			t.Errorf("a marker that the converter did not write: %q", line)
		}
	}
	for _, want := range []string{"&lt;!-- page: 99 -->", "&lt;!-- slide: 7 -->", "正常的字 &lt;!-- table: 5 rows x 5 cols --> 之后"} {
		if !strings.Contains(md, want) {
			t.Errorf("%q is missing in:\n%s", want, md)
		}
	}
}

// A sheet is cut at the row and column caps, with an explicit notice, and the cut is not a failure.
func TestSheetCapsAreAnnouncedNotSilent(t *testing.T) {
	python(t)
	dir := t.TempDir()
	rows := genoffice(t, dir, "bigxlsx", "rows.xlsx", 2500)
	md := mustConvert(t, rows, "--max-rows", "1000")
	inOrder(t, md, "<!-- sheet: 订单 range: A1:H1000 -->", "| 1000 | ORD0001000 |", "<!-- truncated: sheet 订单, rows kept 1000 of 2500 -->")
	if strings.Contains(md, "ORD0001001") {
		t.Error("a row over the cap is written")
	}
	md = mustConvert(t, rows) // the default cap is 10,000, which 2,500 rows do not reach
	if strings.Contains(md, "truncated") || !strings.Contains(md, "ORD0002500") {
		t.Error("a sheet under the cap is cut")
	}
	md = mustConvert(t, genoffice(t, dir, "wide", "wide.xlsx", 300))
	inOrder(t, md, "<!-- sheet: 宽 range: A1:GR2 -->", "| row | A | B |", " GR |", "<!-- truncated: sheet 宽, columns kept 200 of 300 -->")
	if strings.Contains(md, "| GS |") || strings.Contains(md, " GS ") {
		t.Error("a column over the cap is written")
	}
}

// 120,000 rows are read as a stream: the memory of the process stays small and does not grow with the sheet. The limit
// the process gets here is what the ingest instance's runs get at the smallest, and the whole run is a few seconds.
func TestHugeSheetIsStreamedInBoundedMemory(t *testing.T) {
	python(t)
	if testing.Short() {
		t.Skip("120,000 rows")
	}
	dir := t.TempDir()
	in := genoffice(t, dir, "bigxlsx", "big.xlsx", 120000)
	const probe = `
import resource, subprocess, sys
r = subprocess.run([sys.executable, sys.argv[1], sys.argv[2], "-o", sys.argv[3], "--max-memory-mb", "256"], capture_output=True, text=True)
print(r.returncode, resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss // 1024, r.stderr.strip())
`
	code, out := exits(t, "-c", probe, tool(t, "ooxml2md"), in, in+".md")
	f := strings.Fields(out)
	if code != 0 || len(f) < 2 || f[0] != "0" {
		t.Fatalf("exit %d: %s", code, out)
	}
	if mb, _ := strconv.Atoi(f[1]); mb > 100 {
		t.Errorf("120,000 rows took %d MB", mb)
	}
	md, _ := os.ReadFile(in + ".md")
	if !strings.Contains(string(md), "<!-- truncated: sheet 订单, rows kept 10000 of 120000 -->") {
		t.Errorf("no notice of the cut")
	}
}

// What cannot be converted fails with a code of its own, writes no file, and says why; and the default of a bad or
// hostile file is to fail, never to produce an empty or partial result with exit 0.
func TestFailuresHaveCodesAndLeaveNoFile(t *testing.T) {
	python(t)
	dir := t.TempDir()
	cases := []struct {
		name, kind string
		n          int
		ext        string
		code       int
		says       string
	}{
		{"truncated zip", "corrupt", 0, "docx", 4, "corrupt"},
		{"not a zip", "notzip", 0, "docx", 4, "not a zip package"},
		{"broken XML", "badxml", 0, "docx", 4, "not well-formed"},
		{"legacy binary file", "legacy", 0, "docx", 3, "old binary Office file"},
		{"no text", "empty-docx", 0, "docx", 6, "no text"},
		{"content controls 5000 deep", "nested", 5000, "docx", 5, "nesting deeper"},
		{"a 300 MiB text node", "bomb", 300, "docx", 5, "limits"},
	}
	for _, c := range cases {
		in := genoffice(t, dir, c.kind, strings.ReplaceAll(c.name, " ", "_")+"."+c.ext, c.n)
		code, out, md := convert(t, in)
		if code != c.code || !strings.Contains(out, c.says) || !strings.Contains(out, "ooxml2md: error: ") || md != "" {
			t.Errorf("%s: exit %d (want %d), output %q, file %q", c.name, code, c.code, out, md)
		}
	}
	// an extension that is not supported, a missing file, and the command line
	doc := filepath.Join(dir, "old.doc")
	if err := os.WriteFile(doc, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := convert(t, doc); code != 3 || !strings.Contains(out, "Word 97-2003") {
		t.Errorf(".doc: exit %d: %s", code, out)
	}
	if code, out, _ := convert(t, filepath.Join(dir, "missing.docx")); code != 1 || !strings.Contains(out, "no such file") {
		t.Errorf("missing: exit %d: %s", code, out)
	}
	for name, args := range map[string][]string{"no arguments": {}, "no output": {doc}, "a zero cap": {doc, "-o", "x.md", "--max-rows", "0"}} {
		if code, out := exits(t, append([]string{tool(t, "ooxml2md")}, args...)...); code != 2 || !strings.Contains(out, "usage: ooxml2md") {
			t.Errorf("%s: exit %d: %s", name, code, out)
		}
	}
	if code, out := exits(t, tool(t, "ooxml2md"), "--help"); code != 0 || !strings.Contains(out, "exit: 0 written") {
		t.Errorf("--help: exit %d: %s", code, out)
	}
	// the output cap: a result over it is refused whole
	big := genoffice(t, dir, "bigxlsx", "cap.xlsx", 20000)
	if code, out, md := convert(t, big, "--max-rows", "20000", "--max-output-mb", "1"); code != 5 || md != "" || !strings.Contains(out, "larger than 1 MiB") {
		t.Errorf("over the output cap: exit %d: %s %d", code, out, len(md))
	}
	if code, out, md := convert(t, big, "--max-rows", "20000"); code != 0 || md == "" {
		t.Errorf("under the output cap: exit %d: %s", code, out)
	}
}

// No marker the converter writes is one that CMI would not know: the set of markers is the contract of
// docs/adr/kb3-converter-format.md.
func TestOnlyTheAgreedMarkersAreWritten(t *testing.T) {
	python(t)
	dir := t.TempDir()
	known := regexp.MustCompile(`^<!-- (page: \d+|slide: \d+|notes: slide \d+|table: \d+ rows x \d+ cols|sheet: .+ range: [A-Z]+\d+(:[A-Z]+\d+)?|sheet: .+ empty|truncated: sheet .+, (rows|columns) kept \d+ of \d+|skipped: sheet .+, hidden) -->$`)
	for _, k := range [][2]string{{"docx", "a.docx"}, {"pptx", "a.pptx"}, {"xlsx", "a.xlsx"}} {
		md := mustConvert(t, genoffice(t, dir, k[0], k[1]))
		for _, line := range strings.Split(md, "\n") {
			if strings.HasPrefix(line, "<!--") && !known.MatchString(line) {
				t.Errorf("%s: a marker outside the agreed set: %q", k[0], line)
			}
		}
	}
}
