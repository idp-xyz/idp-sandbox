package office_test

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Cell values of a constructed workbook. A plain string is a shared string, I an inline string, an int or float64 a number.
type (
	I string   // inline string
	D float64  // date serial (styled yyyy-mm-dd, or with a time when it has a fraction)
	F struct { // a formula with its cached value
		Formula string
		Cached  any
	}
	B bool
)

type sheetSpec struct {
	Name   string
	State  string
	Rows   [][]any // nil is an empty cell
	Merges []string
	Rowsat map[int][]any // rows placed at an explicit row number, after Rows
}

type bookSpec struct {
	Sheets        []sheetSpec
	ExternalLink  bool
	Macro         bool
	Date1904      bool
	sharedStrings []string
}

func colName(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s
}

func (b *bookSpec) shared(s string) int {
	for i, x := range b.sharedStrings {
		if x == s {
			return i
		}
	}
	b.sharedStrings = append(b.sharedStrings, s)
	return len(b.sharedStrings) - 1
}

func (b *bookSpec) cellXML(ref string, v any) string {
	num := func(x any) string { return fmt.Sprint(x) }
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return fmt.Sprintf(`<c r="%s" t="s"><v>%d</v></c>`, ref, b.shared(x))
	case I:
		return fmt.Sprintf(`<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, html.EscapeString(string(x)))
	case int, int64, float64:
		return fmt.Sprintf(`<c r="%s"><v>%s</v></c>`, ref, num(x))
	case D:
		style := 1
		if float64(x) != float64(int(x)) {
			style = 2
		}
		return fmt.Sprintf(`<c r="%s" s="%d"><v>%v</v></c>`, ref, style, float64(x))
	case B:
		n := 0
		if x {
			n = 1
		}
		return fmt.Sprintf(`<c r="%s" t="b"><v>%d</v></c>`, ref, n)
	case F:
		switch c := x.Cached.(type) {
		case string:
			return fmt.Sprintf(`<c r="%s" t="str"><f>%s</f><v>%s</v></c>`, ref, html.EscapeString(x.Formula), html.EscapeString(c))
		case nil:
			return fmt.Sprintf(`<c r="%s"><f>%s</f></c>`, ref, html.EscapeString(x.Formula))
		default:
			return fmt.Sprintf(`<c r="%s"><f>%s</f><v>%s</v></c>`, ref, html.EscapeString(x.Formula), num(c))
		}
	}
	panic(fmt.Sprintf("cell %T", v))
}

func (b *bookSpec) sheetXML(s sheetSpec) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	write := func(r int, row []any) {
		var cells strings.Builder
		for c, v := range row {
			cells.WriteString(b.cellXML(colName(c+1)+strconv.Itoa(r), v))
		}
		if cells.Len() > 0 {
			fmt.Fprintf(&sb, `<row r="%d">%s</row>`, r, cells.String())
		}
	}
	for i, row := range s.Rows {
		write(i+1, row)
	}
	sb.WriteString(`</sheetData>`)
	if len(s.Merges) > 0 {
		fmt.Fprintf(&sb, `<mergeCells count="%d">`, len(s.Merges))
		for _, m := range s.Merges {
			fmt.Fprintf(&sb, `<mergeCell ref="%s"/>`, m)
		}
		sb.WriteString(`</mergeCells>`)
	}
	sb.WriteString(`</worksheet>`)
	return sb.String()
}

func (b *bookSpec) write(t *testing.T, path string) {
	t.Helper()
	files := map[string]string{}
	var sheets, rels, types strings.Builder
	for i, s := range b.Sheets {
		n := i + 1
		state := ""
		if s.State != "" {
			state = fmt.Sprintf(` state="%s"`, s.State)
		}
		fmt.Fprintf(&sheets, `<sheet name="%s" sheetId="%d"%s r:id="rId%d"/>`, html.EscapeString(s.Name), n, state, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
		files[fmt.Sprintf("xl/worksheets/sheet%d.xml", n)] = b.sheetXML(s)
	}
	_ = types
	pr := ""
	if b.Date1904 {
		pr = `<workbookPr date1904="1"/>`
	}
	files["xl/workbook.xml"] = `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` + pr + `<sheets>` + sheets.String() + `</sheets></workbook>`
	files["xl/_rels/workbook.xml.rels"] = `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() + `</Relationships>`
	var ss strings.Builder
	ss.WriteString(`<?xml version="1.0" encoding="UTF-8"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for _, s := range b.sharedStrings {
		fmt.Fprintf(&ss, `<si><t xml:space="preserve">%s</t></si>`, html.EscapeString(s))
	}
	ss.WriteString(`</sst>`)
	files["xl/sharedStrings.xml"] = ss.String()
	files["xl/styles.xml"] = `<?xml version="1.0" encoding="UTF-8"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><numFmts count="1"><numFmt numFmtId="164" formatCode="yyyy\-mm\-dd\ hh:mm:ss"/></numFmts><cellXfs count="3"><xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="164"/></cellXfs></styleSheet>`
	if b.ExternalLink {
		files["xl/externalLinks/externalLink1.xml"] = `<?xml version="1.0"?><externalLink xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`
		files["xl/externalLinks/_rels/externalLink1.xml.rels"] = `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="x" Target="http://127.0.0.1:1/never.xlsx" TargetMode="External"/></Relationships>`
	}
	if b.Macro {
		files["xl/vbaProject.bin"] = "not a real macro"
	}
	writeXLSX(t, path, files)
}

func scriptPath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "office", name)
}

func py(t *testing.T, dir string, env []string, tool string, args ...string) (string, string, int) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3")
	}
	cmd := exec.Command("python3", append([]string{"-W", "error", scriptPath(t, tool)}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return so.String(), se.String(), code
}

func mustRun(t *testing.T, dir string, tool string, args ...string) string {
	t.Helper()
	out, errOut, code := py(t, dir, nil, tool, args...)
	if code != 0 || errOut != "" {
		t.Fatalf("%s %v: exit %d\n%s\n%s", tool, args, code, out, errOut)
	}
	return out
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
}

func unescapeCell(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 't':
				sb.WriteByte('\t')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			default:
				sb.WriteByte(s[i+1])
			}
			i++
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// readExport reads a sheet's TSV as a script without the workbook would: row k is line k, column k is field k.
func readExport(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var row []string
		if line != "" {
			for _, f := range strings.Split(line, "\t") {
				row = append(row, unescapeCell(f))
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func priceBook() *bookSpec {
	// three blocks: a rate table, a second table to its right, a notes block below both, and a title above
	rows := make([][]any, 20)
	rows[0] = []any{"价格表 2026"}
	rows[2] = []any{"重量", "A区", "B区", nil, "项目", "金额"}
	rows[3] = []any{1, 10.5, 11.25, nil, "燃油", 0.125}
	rows[4] = []any{2, 12, 13.5, nil, "偏远", 30}
	rows[5] = []any{3, 14, 15, nil, "旺季", 5}
	rows[9] = []any{"说明", "一、价格含税"}
	rows[10] = []any{"二、以实际重量为准"}
	return &bookSpec{Sheets: []sheetSpec{{Name: "运价", Rows: rows, Merges: []string{"A1:F1"}}}}
}

func TestSheetmapFindsBlocksAndExportsTheSheets(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "价格.xlsx")
	priceBook().write(t, x)
	out := mustRun(t, dir, "sheetmap", x)
	mustContain(t, out,
		"价格.xlsx: 1 sheets, 4 blocks",
		"1. 运价  A1:F11",
		"1.2  A3:C6  4x3",
		"header r3: 重量 | A区 | B区",
		"1.3  E3:F6  4x2",
		"header r3: 项目 | 金额",
		"1: 1 blocks of 1-2 cells: A1:F1", // the merged title is a block of its own
	)
	tsv := readExport(t, filepath.Join(dir, "价格.sheets", "s01.tsv"))
	if len(tsv) != 11 || tsv[0][0] != "价格表 2026" || tsv[2][1] != "A区" || tsv[3][2] != "11.25" || tsv[3][5] != "0.125" || tsv[10][0] != "二、以实际重量为准" {
		t.Fatalf("export %q", tsv)
	}
	if len(tsv[3]) != 6 || tsv[3][3] != "" {
		t.Fatalf("columns do not line up: %q", tsv[3])
	}
	blocks, _ := os.ReadFile(filepath.Join(dir, "价格.sheets", "blocks.tsv"))
	mustContain(t, string(blocks), "1.1\t1\tA1:F1\t1\t6\t1\t", "1.2\t1\tA3:C6\t4\t3\t12\t", "1.4\t1\tA10:B11")
	index, _ := os.ReadFile(filepath.Join(dir, "价格.sheets", "index.tsv"))
	mustContain(t, string(index), "1\t运价\tvisible\tA1:F11\t11\t6\t", "s01.tsv")
	merges, _ := os.ReadFile(filepath.Join(dir, "价格.sheets", "merges.tsv"))
	mustContain(t, string(merges), "1\tA1:F1")
	// the one-sheet view
	one := mustRun(t, dir, "sheetmap", x, "--sheet", "运价")
	mustContain(t, one, "1.4  A10:B11")
}

func TestSheetmapExportKeepsTypesAndEscapesAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "m.xlsx")
	book := &bookSpec{Sheets: []sheetSpec{{Name: "t", Rows: [][]any{
		{"shared", I("inline"), 3, 0.1, D(46345), D(46345.5), B(true)},
		{F{"SUM(D1:D2)", 0.30000000000000004}, F{"A1&\"x\"", "sharedx"}, F{"NOW()", nil}, I("tab\there"), I("line\nbreak"), I(`back\slash`), I("  padded  ")},
		{nil, nil, "00123", "1,234.50", "12%"},
	}}}}
	book.write(t, x)
	out := mustRun(t, dir, "sheetmap", x, "-o", "ex")
	mustContain(t, out, "3 numeric-looking text", "3 formulas")
	got := readExport(t, filepath.Join(dir, "ex", "s01.tsv"))
	want := [][]string{
		{"shared", "inline", "3", "0.1", "2026-11-19", "2026-11-19 12:00:00", "TRUE"},
		{"0.3", "sharedx", "", "tab\there", "line\nbreak", `back\slash`, "  padded  "},
		{"", "", "00123", "1,234.50", "12%"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	f, _ := os.ReadFile(filepath.Join(dir, "ex", "s01.formulas.tsv"))
	mustContain(t, string(f), "A2\tSUM(D1:D2)\t0.3", "B2\tA1&\"x\"\tsharedx", "C2\tNOW()\t")
	// a second run into the same directory replaces the files
	mustRun(t, dir, "sheetmap", x, "-o", "ex")
}

func TestSheetmapOddWorkbooks(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "odd.xlsx")
	col := make([][]any, 40)
	for i := range col {
		col[i] = []any{fmt.Sprintf("line %d", i)}
	}
	book := &bookSpec{ExternalLink: true, Macro: true, Sheets: []sheetSpec{
		{Name: "empty"},
		{Name: "secret", State: "hidden", Rows: [][]any{{"h1", "h2"}, {1, 2}, {3, 4}}},
		{Name: "one column", Rows: col},
		{Name: "visible", Rows: [][]any{{"a", "b"}, {1, 2}}},
	}}
	book.write(t, x)
	out := mustRun(t, dir, "sheetmap", x)
	mustContain(t, out, "4 sheets (1 hidden)", "1. empty  empty", "2. secret  A1:B3", "[hidden]", "3. one column  A1:A40", "header: one column",
		"! macros (vbaProject.bin) present: ignored, never run", "! 1 external link part(s): ignored, never followed")
	if _, err := os.Stat(filepath.Join(dir, "odd.sheets", "s01.tsv")); err == nil {
		t.Error("an empty sheet got a file")
	}
	idx, _ := os.ReadFile(filepath.Join(dir, "odd.sheets", "index.tsv"))
	mustContain(t, string(idx), "2\tsecret\thidden")
	// a file that is not a workbook
	bad := filepath.Join(dir, "bad.xlsx")
	os.WriteFile(bad, []byte("plain text"), 0o644)
	if _, _, code := py(t, dir, nil, "sheetmap", bad); code != 1 {
		t.Errorf("a file that is not a zip exited %d", code)
	}
	if _, _, code := py(t, dir, nil, "sheetmap", filepath.Join(dir, "missing.xlsx")); code != 1 {
		t.Errorf("a missing file exited %d", code)
	}
	if _, _, code := py(t, dir, nil, "sheetmap"); code != 2 {
		t.Errorf("no arguments exited %d", code)
	}
}

func TestSheetmapIsFastAndBoundedOnLargeSheets(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "big.xlsx")
	big := make([][]any, 30000)
	for i := range big {
		big[i] = []any{fmt.Sprintf("item-%d", i), i, float64(i) * 1.5, I("note"), i % 7}
	}
	// a second sheet with a blank row after every second row: 10,000 blocks
	many := make([][]any, 30000)
	for i := range many {
		if i%3 != 2 {
			many[i] = []any{fmt.Sprintf("r%d", i), i}
		}
	}
	// and one hundred small sheets
	sheets := []sheetSpec{{Name: "big", Rows: big}, {Name: "many", Rows: many}}
	for i := 0; i < 100; i++ {
		sheets = append(sheets, sheetSpec{Name: fmt.Sprintf("s%d", i), Rows: [][]any{{"h", "k"}, {i, 2}, {3, 4}, nil, {"x"}}})
	}
	(&bookSpec{Sheets: sheets}).write(t, x)
	start := time.Now()
	out := mustRun(t, dir, "sheetmap", x)
	if d := time.Since(start); d > 12*time.Second {
		t.Errorf("30,000 rows twice took %v", d)
	}
	if n := len(out); n > 12000 {
		t.Errorf("stdout is %d bytes", n)
	}
	mustContain(t, out, "1. big  A1:E30000  150000 cells  1 block", "export:")
	// the first 1,200 bytes still say what the file is and where the data is
	head := out[:1200]
	mustContain(t, head, "sheets", "big.sheets/", "1. big")
	one := mustRun(t, dir, "sheetmap", x, "--sheet", "2")
	if len(one) > 12000 || !strings.Contains(one, "2. many  10000 blocks") {
		t.Errorf("one sheet of 10,000 blocks: %d bytes\n%.400s", len(one), one)
	}
	rows := readExport(t, filepath.Join(dir, "big.sheets", "s01.tsv"))
	if len(rows) != 30000 || rows[29999][0] != "item-29999" || rows[29999][2] != "44998.5" {
		t.Errorf("rows %d", len(rows))
	}
}

func TestSheetmapKeepsMergedTitlesWholeAndHandlesMergedHeaders(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "mh.xlsx")
	book := &bookSpec{Sheets: []sheetSpec{{Name: "m", Merges: []string{"B1:E1", "B2:C2", "D2:E2", "A2:A3"}, Rows: [][]any{
		{nil, "年度价格"},
		{"区域", "商业", nil, "住宅"},
		{nil, "kg", "price", "kg", "price"},
		{"东", 1, 9.5, 1, 11},
		{"西", 2, 8, 2, 10},
	}}}}
	book.write(t, x)
	out := mustRun(t, dir, "sheetmap", x)
	mustContain(t, out, "1 blocks", "1.1  A1:E5", "merged B1:E1 B2:C2 D2:E2 +1")
	tsv := readExport(t, filepath.Join(dir, "mh.sheets", "s01.tsv"))
	if tsv[1][2] != "" || tsv[1][1] != "商业" {
		t.Fatalf("merged cells are not filled: %q", tsv[1])
	}
}

func TestSheetskelStillPrintsItsOldOutputWithoutWarnings(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "s.xlsx")
	priceBook().write(t, x)
	out := mustRun(t, dir, "sheetskel", x)
	mustContain(t, out, "1. 运价", "7 rows, 5 columns", "header: 价格表 2026")
}

// ---- sheetcheck ----

func fakeOfficecli(t *testing.T, script string) []string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "officecli"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func goodOutput() *bookSpec {
	return &bookSpec{Sheets: []sheetSpec{{Name: "价格", Rows: [][]any{
		{"重量", "A区", "B区", "项目", "金额"},
		{1, 10.5, 11.25, "燃油", 0.125},
		{2, 12, 13.5, "偏远", 30},
		{3, 14, 15, "旺季", 5},
	}}}}
}

func TestSheetcheckOnAGoodOutputSaysWhatItChecked(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "out.xlsx")
	goodOutput().write(t, x)
	out, errOut, code := py(t, dir, fakeOfficecli(t, `echo "Validation passed: no errors found."`), "sheetcheck", x)
	if code != 0 || errOut != "" {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	mustContain(t, out, "out.xlsx: 1 sheet(s), 20 cells",
		"1. 价格  A1:E4  4 rows x 5 cols  20 cells  1 block(s)  0 merged",
		"== READ", "== EMPTY CELLS INSIDE BLOCKS", "== MERGED RANGES", "== NUMBERS STORED AS TEXT", "== HEADERS AND COLUMNS",
		"== VALIDATE (officecli validate)", "exit 0:", "Validation passed", "== RECONCILIATION with the source",
		"no --source was given", "findings: 0")
	for _, section := range []string{"READ", "EMPTY CELLS INSIDE BLOCKS", "MERGED RANGES", "NUMBERS STORED AS TEXT", "HEADERS AND COLUMNS", "VALIDATE", "RECONCILIATION"} {
		i := strings.Index(out, "== "+section)
		if i < 0 || !strings.Contains(out[i:min(i+700, len(out))], "checked:") || !strings.Contains(out[i:min(i+900, len(out))], "not checked:") {
			t.Errorf("section %s does not say what it checked and did not", section)
		}
	}
	// without officecli, or with it failing, the report says so and still exits 0
	out, _, code = py(t, dir, []string{"PATH=/nonexistent"}, "sheetcheck", x)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	mustContain(t, out, "validate NOT RUN: officecli is not on PATH", "findings: 1")
	out, _, code = py(t, dir, fakeOfficecli(t, `echo "Error: broken part"; exit 3`), "sheetcheck", x)
	mustContain(t, out, "exit 3:", "Error: broken part", "officecli validate exited 3")
	if code != 0 {
		t.Fatalf("a failing validate must not fail the report: %d", code)
	}
	out, _, _ = py(t, dir, nil, "sheetcheck", x, "--no-validate")
	mustContain(t, out, "validate NOT RUN (--no-validate)")
	// not readable: exit 1
	bad := filepath.Join(dir, "bad.xlsx")
	os.WriteFile(bad, []byte("x"), 0o644)
	if _, _, code := py(t, dir, nil, "sheetcheck", bad); code != 1 {
		t.Errorf("unreadable exit %d", code)
	}
	if _, _, code := py(t, dir, nil, "sheetcheck"); code != 2 {
		t.Errorf("no arguments exit %d", code)
	}
}

func TestSheetcheckFindsOverlapsTextNumbersAndHoles(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "bad.xlsx")
	rows := [][]any{
		{"名称", "价格", nil, "备注"},
		{"a", "12.5", nil, "x"},
		{"b", "007", 3, nil},
		{"c", 4, 5, "y"},
		{nil, 4, 5, nil},
		{"e", 4, nil, "z"},
	}
	book := &bookSpec{Sheets: []sheetSpec{{Name: "坏", Rows: rows, Merges: []string{"A1:B2", "B2:C3", "D4:D4", "A5:B5"}}, {Name: "Sheet1"}}}
	book.write(t, x)
	out, _, code := py(t, dir, nil, "sheetcheck", x, "--no-validate")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	mustContain(t, out,
		"merged ranges A1:B2 and B2:C3 overlap",
		"is a merge of one cell",
		"2 text cell(s) read as numbers (1 with a leading zero)",
		"sheet 2 'Sheet1' has no cell values",
		"data under an empty header cell",
		"empty of")
	if !strings.Contains(out, "under a merge hold a value that is not shown") {
		t.Errorf("a value under a merge went unreported:\n%s", out)
	}
}

func TestSheetcheckReconcilesBothDirections(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.xlsx")
	srcRows := [][]any{{"区", "价"}, {"A", 1.5}, {"B", 2.5}, {"C", 3.5}, {"D", 4.5}, {"E", 5.5}, {"F", 6.5}, {"G", 7.5}, {"H", 8.5}}
	(&bookSpec{Sheets: []sheetSpec{{Name: "s", Rows: srcRows}}}).write(t, src)

	// a faithful copy, reorganised (columns swapped): everything is found
	same := filepath.Join(dir, "same.xlsx")
	var swapped [][]any
	for _, r := range srcRows {
		swapped = append(swapped, []any{r[1], r[0]})
	}
	(&bookSpec{Sheets: []sheetSpec{{Name: "n", Rows: swapped}}}).write(t, same)
	out, _, _ := py(t, dir, nil, "sheetcheck", same, "--no-validate", "--source", src)
	mustContain(t, out, "numeric cells: source 8, output 8", "0 absent", "8 of 8 (100%)", "output numbers (distinct 8) that no source cell has: 0",
		"every source block with 6 or more numbers has all its distinct numbers")
	if strings.Contains(out, "absent from the output") {
		t.Errorf("false alarm:\n%s", out)
	}

	// source to output: two numbers dropped, one changed; a block missing by size
	lost := filepath.Join(dir, "lost.xlsx")
	(&bookSpec{Sheets: []sheetSpec{{Name: "n", Rows: [][]any{{"区", "价"}, {"A", 1.5}, {"B", 2.5}, {"C", 3.5}, {"D", 4.9}}}}}).write(t, lost)
	out, _, _ = py(t, dir, nil, "sheetcheck", lost, "--no-validate", "--source", src)
	mustContain(t, out, "numeric cells: source 8, output 4", "source numbers absent from the output: 4.5 (x1), 5.5 (x1), 6.5 (x1), 7.5 (x1), 8.5 (x1)",
		"numbers only in the output: 4.9 (x1)", "source texts absent from the output: 'E'")
	tiny := filepath.Join(dir, "tiny.xlsx")
	(&bookSpec{Sheets: []sheetSpec{{Name: "n", Rows: [][]any{{"区", "价"}, {"A", 1.5}}}}}).write(t, tiny)
	out, _, _ = py(t, dir, nil, "sheetcheck", tiny, "--no-validate", "--source", src)
	mustContain(t, out, "has no output block at least half its size")
	if strings.Contains(out, "absent from the output: 1.5") {
		t.Errorf("a number that is there was reported absent:\n%s", out)
	}

	// a number moved into text counts as found only as text
	txt := filepath.Join(dir, "txt.xlsx")
	(&bookSpec{Sheets: []sheetSpec{{Name: "n", Rows: [][]any{{"区", "价"}, {"A", I("1.5")}, {"B", 2.5}, {"C", 3.5}, {"D", 4.5}, {"E", 5.5}, {"F", 6.5}, {"G", 7.5}, {"H", 8.5}}}}}).write(t, txt)
	out, _, _ = py(t, dir, nil, "sheetcheck", txt, "--no-validate", "--source", src)
	mustContain(t, out, "1 only as text", "text cell(s) read as numbers")

	// the source as sheetmap's export directory
	mustRun(t, dir, "sheetmap", src, "-o", "exp")
	out, _, _ = py(t, dir, nil, "sheetcheck", lost, "--no-validate", "--source", "exp")
	mustContain(t, out, "the source is an export directory", "numeric cells: source 8, output 4", "absent from the output: 4.5")
	// a source that cannot be read
	if _, _, code := py(t, dir, nil, "sheetcheck", lost, "--source", filepath.Join(dir, "none.xlsx")); code != 1 {
		t.Errorf("unreadable source exit %d", code)
	}
}

func TestSheetcheckOutputIsBounded(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "many.xlsx")
	var sheets []sheetSpec
	for i := 0; i < 60; i++ {
		sheets = append(sheets, sheetSpec{Name: fmt.Sprintf("sheet %d", i), Merges: []string{"A1:B2", "B2:C3"},
			Rows: [][]any{{"h1", nil, "h3", "h3"}, {"1", 2, nil, 4}, {"2", 3, 4, nil}, {"3", 4, nil, 6}}})
	}
	(&bookSpec{Sheets: sheets}).write(t, x)
	out, _, code := py(t, dir, nil, "sheetcheck", x, "--no-validate", "--source", x)
	if code != 0 || len(out) > 10000 || !utf8.ValidString(out) {
		t.Fatalf("exit %d, %d bytes", code, len(out))
	}
	mustContain(t, out, "findings:")
}

func TestOfficeImageInstallsTheReadingTools(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(scriptPath(t, "sheetmap")), p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	docker, build := read("Dockerfile"), read("build.sh")
	for _, tool := range []string{"sheetmap", "sheetcheck", "sheetlib.py"} {
		if !strings.Contains(docker, tool) {
			t.Errorf("Dockerfile does not copy %s", tool)
		}
		if !strings.Contains(build, tool) {
			t.Errorf("build.sh does not install %s", tool)
		}
	}
	for _, tool := range []string{"sheetmap", "sheetcheck", "sheetskel"} {
		st, err := os.Stat(scriptPath(t, tool))
		if err != nil || st.Mode()&0o111 == 0 {
			t.Errorf("%s is not an executable file: %v", tool, err)
		}
	}
}
