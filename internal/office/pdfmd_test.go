package office_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// tool is the path of a command of the office image (deploy/office), as the repository holds it.
func tool(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "office", name)
}

func python(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3")
	}
}

// exits runs a command and returns its exit code and what it wrote.
func exits(t *testing.T, args ...string) (int, string) {
	t.Helper()
	out, err := exec.Command("python3", args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatal(err)
	return 0, ""
}

var markdown = strings.Join([]string{
	"---", "title: 说明", "---", "",
	"# 价格 *说明* #", "",
	"第一行中文，", "第二行中文。  ", "hard break here", "and a second", "English line.", "", // two spaces end a line for good
	"**粗** `rate()` `含中文` [链](https://e.com/a?b=1&c=2) \\*not\\* <b>x</b> ~~旧~~ a_b_c", "",
	"1. one", "2. two", "   - 子项", "     continued", "3. three", "", "- [ ] todo", "",
	"| 渠道 | 价格 | 备注 |", "|:--|--:|:-:|", "| A\\|B | 1.5 | **x** |", "| C | 2 |", "",
	"```python", "x = 1  # **not bold**", "```", "",
	"> 引用 *一*", "> - 项", "",
	"***", "",
	"Setext", "------", "",
}, "\n")

// What md2pdf reads in a Markdown file, before it sets a page (so this needs no reportlab): front matter is not content,
// two Chinese lines join without a space and two English ones with one, a hard break stays, marks become the markup of a
// paragraph with everything else escaped, list items carry their depth and number, a table its alignment, code is verbatim.
func TestMd2pdfReadsMarkdown(t *testing.T) {
	python(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "说明.md")
	if err := os.WriteFile(in, []byte(markdown), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := exits(t, tool(t, "md2pdf"), "--dump", in)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	const want = `[
 {"type":"h","level":1,"text":"价格 *说明*","markup":"价格 <i>说明</i>"},
 {"type":"p","text":"第一行中文，第二行中文。\nhard break here and a second English line.","markup":"第一行中文，第二行中文。<br/>hard break here and a second English line."},
 {"type":"p","text":"**粗** ` + "`rate()` `含中文`" + ` [链](https://e.com/a?b=1&c=2) \\*not\\* <b>x</b> ~~旧~~ a_b_c",
  "markup":"<b>粗</b> <font face=\"Courier\" backColor=\"#f0f0f0\">rate()</font> <font backColor=\"#f0f0f0\">含中文</font> <a href=\"https://e.com/a?b=1&amp;c=2\" color=\"#1a5fb4\">链</a> *not* &lt;b&gt;x&lt;/b&gt; <strike>旧</strike> a_b_c"},
 {"type":"list","items":[
  {"level":0,"marker":"1.","text":"one","markup":"one"},
  {"level":0,"marker":"2.","text":"two","markup":"two"},
  {"level":1,"marker":"","text":"子项 continued","markup":"子项 continued"},
  {"level":0,"marker":"3.","text":"three","markup":"three"},
  {"level":0,"marker":"","text":"[ ] todo","markup":"[ ] todo"}]},
 {"type":"table","align":["left","right","center"],"header":["渠道","价格","备注"],"rows":[["A|B","1.5","**x**"],["C","2",""]]},
 {"type":"code","lang":"python","text":"x = 1  # **not bold**"},
 {"type":"quote","blocks":[
  {"type":"p","text":"引用 *一*","markup":"引用 <i>一</i>"},
  {"type":"list","items":[{"level":0,"marker":"","text":"项","markup":"项"}]}]},
 {"type":"hr"},
 {"type":"h","level":2,"text":"Setext","markup":"Setext"}
]`
	var got, expected any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("read as\n%s", out)
	}

	// a rule that opens the text is a rule: only "key: value" lines between two rules are front matter
	rule := filepath.Join(dir, "rule.md")
	if err := os.WriteFile(rule, []byte("---\n# T\n\ntext\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out = exits(t, tool(t, "md2pdf"), "--dump", rule)
	var blocks []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &blocks) != nil || len(blocks) != 3 || blocks[0]["type"] != "hr" || blocks[1]["text"] != "T" {
		t.Fatalf("exit %d: %s", code, out)
	}

	for name, args := range map[string][]string{"no arguments": {}, "one file": {in}, "--dump with two": {"--dump", in, "x.pdf"}, "an unknown option": {in, "x.pdf", "--portrait"}} {
		if code, out := exits(t, append([]string{tool(t, "md2pdf")}, args...)...); code != 2 || !strings.Contains(out, "usage: md2pdf IN.md OUT.pdf") {
			t.Errorf("%s: exit %d: %s", name, code, out)
		}
	}
	if code, out := exits(t, tool(t, "md2pdf"), "--dump", filepath.Join(dir, "missing.md")); code != 1 || !strings.Contains(out, "missing.md") {
		t.Errorf("a missing file: exit %d: %s", code, out)
	}
	notText := filepath.Join(dir, "bytes.md")
	if err := os.WriteFile(notText, []byte{0xff, 0xfe, 0x00, 0x41}, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := exits(t, tool(t, "md2pdf"), "--dump", notText); code != 1 || !strings.Contains(out, "is not UTF-8 text") {
		t.Errorf("a file that is not text: exit %d: %s", code, out)
	}
}

// What pdftext makes of its arguments, before it opens a file (so this needs no pypdf): the pages --pages names, the
// same pages written back for the "Next:" hint, and a cut that never splits a character.
func TestPdftextReadsItsArguments(t *testing.T) {
	python(t)
	const probe = `
import importlib.machinery, importlib.util, json, sys
loader = importlib.machinery.SourceFileLoader("pdftext", sys.argv[1])
m = importlib.util.module_from_spec(importlib.util.spec_from_loader("pdftext", loader))
loader.exec_module(m)
out = {}
for spec, n in (("", 3), ("3", 5), ("2-4", 5), ("4-", 5), ("-2", 5), ("1,4,9-12", 12), ("2-99", 5), (" 2 - 3 ", 5)):
    out[spec] = [p + 1 for p in m.parse_pages(spec, n)]
for bad in ("9", "x", "3-1", "0", "1,,2", "-"):
    try:
        m.parse_pages(bad, 5)
        out["bad " + bad] = "accepted"
    except m.Failure as err:
        out["bad " + bad] = str(err)
out["spans"] = [m.spans([1, 2, 3, 7]), m.spans([]), m.spans([5]), m.spans([1, 3, 5, 6])]
out["cut"] = [m.cut("中文abc", 7), m.cut("中文abc", 5), m.cut("abc", 10)]
print(json.dumps(out, ensure_ascii=False))
`
	code, out := exits(t, "-c", probe, tool(t, "pdftext"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	list := func(v ...float64) []any {
		out := []any{}
		for _, x := range v {
			out = append(out, x)
		}
		return out
	}
	format := "write pages as 3, 2-5, 7- or 1,4,9-12"
	want := map[string]any{
		"": list(1, 2, 3), "3": list(3), "2-4": list(2, 3, 4), "4-": list(4, 5), "-2": list(1, 2), "1,4,9-12": list(1, 4, 9, 10, 11, 12),
		"2-99": list(2, 3, 4, 5), " 2 - 3 ": list(2, 3),
		"bad 9": "there is no page 9: the file has 5 pages", "bad x": "--pages x: " + format, "bad 3-1": "--pages 3-1: " + format,
		"bad 0": "--pages 0: " + format, "bad 1,,2": "--pages 1,,2: " + format, "bad -": "--pages -: " + format,
		"spans": []any{"1-3,7", "", "5", "1,3,5-6"},
		"cut":   []any{"中文a", "中", "abc"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s", out)
	}
	for name, args := range map[string][]string{"no arguments": {}, "two files": {"a.pdf", "b.pdf"}, "a negative limit": {"a.pdf", "--max-bytes", "-1"}, "an unknown option": {"a.pdf", "--ocr"}} {
		if code, out := exits(t, append([]string{tool(t, "pdftext")}, args...)...); code != 2 || !strings.Contains(out, "usage: pdftext FILE.pdf") {
			t.Errorf("%s: exit %d: %s", name, code, out)
		}
	}
	if code, out := exits(t, tool(t, "pdftext"), "--help"); code != 0 || !strings.Contains(out, "--layout") || !strings.Contains(out, "--check") {
		t.Errorf("--help: exit %d: %s", code, out)
	}
}
