package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// Ticket returns an unsigned ticket-shaped string with these claims (the sandbox never verifies the signature).
func Ticket(tenant, sub, kind string) string {
	b, _ := json.Marshal(map[string]string{"tenant_id": tenant, "sub": sub, "kind": kind})
	return "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func ooxml(t *testing.T, parts map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, d := range parts {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(d)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTicketsAreTheCallersAndOfTheirKind(t *testing.T) {
	r := Ticket("acme", "bob", "read")
	if err := For(r, "read", "acme/bob"); err != nil {
		t.Fatal(err)
	}
	if err := For(r, "write", "acme/bob"); !errors.Is(err, ErrTicket) {
		t.Fatalf("a read ticket was taken for a write: %v", err)
	}
	if err := For(r, "read", "acme/alice"); !errors.Is(err, ErrNotYours) {
		t.Fatalf("someone else's ticket was taken: %v", err)
	}
	if err := For(r, "read", "other/bob"); !errors.Is(err, ErrNotYours) {
		t.Fatalf("another tenant's ticket was taken: %v", err)
	}
	for _, bad := range []string{"attachment:1", "output", "a.b", "a.!!.c", Ticket("acme", "", "read"), Ticket("acme", "bob", "admin"), strings.Repeat("a", 8193)} {
		if err := For(bad, "read", "acme/bob"); !errors.Is(err, ErrTicket) {
			t.Fatalf("%.40q was taken: %v", bad, err)
		}
	}
}

func TestCheckRefusesWhatThePlatformDoesNotMove(t *testing.T) {
	doc := ooxml(t, map[string][]byte{"[Content_Types].xml": []byte("<Types/>"), "word/document.xml": []byte("<w:document/>")})
	if err := Check("报价单.docx", doc); err != nil {
		t.Fatalf("a docx was refused: %v", err)
	}
	if err := Check("a.DOCX", doc); err != nil {
		t.Fatalf("the extension's case matters: %v", err)
	}
	bomb := ooxml(t, map[string][]byte{"word/document.xml": bytes.Repeat([]byte{'a'}, 8<<20)})
	cases := map[string][]byte{
		"a.docm": doc, // macro-enabled format
		"a.exe":  []byte("MZ"),
		"a.xlsx": []byte("not a zip"),
		"b.docx": ooxml(t, map[string][]byte{"word/vbaProject.bin": {1}}),
		"c.docx": ooxml(t, map[string][]byte{"../evil": {1}}),
		"d.docx": bomb,
		"a.pdf":  []byte("PK\x03\x04"),
		"a.png":  []byte("GIF89a"),
		"a.txt":  []byte("text\x00with a nul"),
		"a.csv":  []byte{0xff, 0xfe, 'a'},
		"noext":  []byte("x"),
	}
	for name, data := range cases {
		if err := Check(name, data); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, data := range map[string][]byte{"a.pdf": []byte("%PDF-1.7"), "a.png": []byte("\x89PNG\r\n\x1a\nx"), "a.jpg": {0xff, 0xd8, 0xff, 0xe0}, "a.md": []byte("# 标题")} {
		if err := Check(name, data); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

func TestSafeNameKeepsOneElement(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", `C:\x\报价单.docx`: "报价单.docx", "a\x00b?.txt": "ab.txt", ".hidden.md": "hidden.md", " out.xlsx ": "out.xlsx"} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeName(strings.Repeat("长", 100) + ".docx"); len(got) > 200 || !strings.HasSuffix(got, ".docx") || !utf8.ValidString(got) {
		t.Errorf("a long name is kept at %d bytes: %q", len(got), got)
	}
}

func TestClientPresentsTheTicketAndReadsTheFile(t *testing.T) {
	ticket := Ticket("acme", "bob", "read")
	var posted []byte
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+ticket {
			http.Error(w, `{"error":{"message":"bad ticket"}}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/f/content":
			w.Header().Set("Idp-File-Ref", "file-1")
			w.Header().Set("Idp-File-Sha256", "abc")
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''%E7%AC%94%E8%AE%B0.txt`)
			_, _ = w.Write([]byte("hello\x00\xff"))
		case r.Method == http.MethodPost && r.URL.Path == "/f":
			posted, _ = io.ReadAll(r.Body)
			if r.Header.Get("Content-Disposition") == "" {
				http.Error(w, "no name", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(File{Ref: "file-2", Name: "out.txt", Size: int64(len(posted))})
		default:
			http.Error(w, `{"error":{"message":"no such route"}}`, http.StatusNotFound)
		}
	}))
	defer ws.Close()
	c := NewClient(ws.URL+"/f/", 1<<20)
	f, data, err := c.Get(context.Background(), ticket)
	if err != nil || string(data) != "hello\x00\xff" || f.Name != "笔记.txt" || f.Ref != "file-1" {
		t.Fatalf("%+v %q %v", f, data, err)
	}
	out, err := c.Put(context.Background(), ticket, "out.txt", "text/plain", []byte{0, 1, 2})
	if err != nil || out.Ref != "file-2" || !bytes.Equal(posted, []byte{0, 1, 2}) {
		t.Fatalf("%+v %v %v", out, posted, err)
	}
	if _, _, err := c.Get(context.Background(), Ticket("acme", "eve", "read")); !errors.Is(err, ErrTicket) {
		t.Fatalf("a refused ticket: %v", err)
	}
	small := NewClient(ws.URL+"/f", 4)
	if _, _, err := small.Get(context.Background(), ticket); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a file over the limit: %v", err)
	}
}
