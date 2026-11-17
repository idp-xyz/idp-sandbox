package office_test

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSheetskelPrintsNamesRangeAndHeader(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3")
	}
	dir := t.TempDir()
	xlsx := filepath.Join(dir, "报价.xlsx")
	writeXLSX(t, xlsx, map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets>
    <sheet name="运价" sheetId="1" r:id="rId1"/>
    <sheet name="附加费" sheetId="2" r:id="rId2"/>
  </sheets>
</workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/>
</Relationships>`,
		"xl/sharedStrings.xml": `<?xml version="1.0" encoding="UTF-8"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <si><t>渠道</t></si>
  <si><t>价格</t></si>
  <si><t>项目</t></si>
</sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <dimension ref="A1:B3"/>
  <sheetData>
    <row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
    <row r="2"><c r="A2" t="inlineStr"><is><t>快递</t></is></c><c r="B2"><v>12.5</v></c></row>
    <row r="3"><c r="A3" t="inlineStr"><is><t>空运</t></is></c><c r="B3"><v>9</v></c></row>
  </sheetData>
</worksheet>`,
		"xl/worksheets/sheet2.xml": `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <dimension ref="A1:A2"/>
  <sheetData>
    <row r="1"><c r="A1" t="s"><v>2</v></c></row>
    <row r="2"><c r="A2"><v>3</v></c></row>
  </sheetData>
</worksheet>`,
	})
	out, err := exec.Command("python3", sheetskelPath(t), xlsx).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"报价.xlsx",
		"1. 运价  A1:B3  3 rows, 2 columns",
		"   header: 渠道 | 价格",
		"   next: 快递 | 12.5",
		"2. 附加费  A1:A2  2 rows, 1 column",
		"   header: 项目",
		"   next: 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "空运") {
		t.Fatalf("a later row was printed:\n%s", got)
	}

	bad := exec.Command("python3", sheetskelPath(t), filepath.Join(dir, "missing.xlsx"))
	if err := bad.Run(); err == nil {
		t.Fatal("a missing file exited 0")
	}
	usage := exec.Command("python3", sheetskelPath(t))
	if err := usage.Run(); err == nil {
		t.Fatal("no arguments exited 0")
	}
	help, err := exec.Command("python3", sheetskelPath(t), "--help").Output()
	if err != nil || !strings.Contains(string(help), "FILE.xlsx") {
		t.Fatalf("help %v %q", err, help)
	}
}

func sheetskelPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "office", "sheetskel")
}

func writeXLSX(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z := zip.NewWriter(f)
	for name, body := range files {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
}
