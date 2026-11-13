package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idp-sandbox/internal/files"
	"idp-sandbox/internal/mcp"
	"idp-sandbox/internal/sandbox"
)

type binfake struct{ *fake }

func (binfake) BinaryFiles() bool { return true }

func ticket(tenant, sub, kind string) string {
	b, _ := json.Marshal(map[string]string{"tenant_id": tenant, "sub": sub, "kind": kind})
	return "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func docx(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte("<w:document>" + text + "</w:document>"))
	_ = zw.Close()
	return buf.Bytes()
}

// workspaceFiles is the Workspace's internal file endpoint as the sandbox sees it: a read ticket gets one file, a write
// ticket delivers one.
type workspaceFiles struct {
	in        []byte
	inName    string
	delivered []byte
	name      string
}

func (w *workspaceFiles) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	c, err := files.Peek(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		http.Error(rw, "", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && c.Kind == "read":
		rw.Header().Set("Content-Disposition", `attachment; filename="`+w.inName+`"`)
		rw.Header().Set("Idp-File-Ref", "file-in")
		_, _ = rw.Write(w.in)
	case r.Method == http.MethodPost && c.Kind == "write":
		w.delivered, _ = io.ReadAll(r.Body)
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
		w.name = params["filename"]
		_ = json.NewEncoder(rw).Encode(files.File{Ref: "file-out", Name: w.name, Size: int64(len(w.delivered)), MediaType: r.Header.Get("Content-Type"), SHA256: "00ff"})
	default:
		http.Error(rw, "", http.StatusForbidden)
	}
}

func transferServer(t *testing.T, be sandbox.Backend, ws *workspaceFiles) (*httptest.Server, *httptest.Server) {
	t.Helper()
	m, err := sandbox.New(context.Background(), sandbox.DefaultConfig("py", "sh"), be, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	wts := httptest.NewServer(ws)
	return httptest.NewServer(mcp.New("t", "1", "", NewWithFiles(m, slog.New(slog.DiscardHandler), nometrics{}, files.NewClient(wts.URL, 25<<20)))), wts
}

func toolNames(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
	req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(b))
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func TestFilesComeInAndGoOutByteForByte(t *testing.T) {
	f := &fake{files: map[string][]byte{}}
	in := docx(t, "报价 \x00\xff")
	ws := &workspaceFiles{in: in, inName: "quote.docx"}
	ts, wts := transferServer(t, binfake{f}, ws)
	defer ts.Close()
	defer wts.Close()
	if l := toolNames(t, ts); !strings.Contains(l, "sandbox_import_file") || !strings.Contains(l, "sandbox_export_file") || !strings.Contains(l, `"format":"idp-attachment"`) || !strings.Contains(l, `"format":"idp-delivery"`) {
		t.Fatalf("the transfer tools are not offered: %s", l)
	}
	id := callAs(t, ts, "bob", "sandbox_create", nil)["structuredContent"].(map[string]any)["workspaceId"].(string)
	r := callAs(t, ts, "bob", "sandbox_import_file", map[string]any{"workspaceId": id, "source": ticket("acme", "bob", "read"), "path": "in/quote.docx"})
	if r["isError"] != nil {
		t.Fatalf("%v", r)
	}
	if got := f.files["idp-sbx-"+id+"/in/quote.docx"]; !bytes.Equal(got, in) {
		t.Fatalf("the imported bytes differ: %d vs %d", len(got), len(in))
	}
	out := docx(t, "修订版")
	f.files["idp-sbx-"+id+"/out/quote-v2.docx"] = out
	r = callAs(t, ts, "bob", "sandbox_export_file", map[string]any{"workspaceId": id, "path": "out/quote-v2.docx", "target": ticket("acme", "bob", "write"), "name": "../报价单-修订.docx"})
	if r["isError"] != nil {
		t.Fatalf("%v", r)
	}
	if !bytes.Equal(ws.delivered, out) {
		t.Fatalf("the delivered bytes differ")
	}
	d := r["structuredContent"].(map[string]any)["deliveredFile"].(map[string]any)
	if d["ref"] != "file-out" || d["size"] != float64(len(out)) || ws.name != "报价单-修订.docx" {
		t.Fatalf("%v", d)
	}
}

func TestTransfersRefuseOthersTicketsAndForbiddenFiles(t *testing.T) {
	f := &fake{files: map[string][]byte{}}
	ws := &workspaceFiles{in: docx(t, "x"), inName: "macro.docm"}
	ts, wts := transferServer(t, binfake{f}, ws)
	defer ts.Close()
	defer wts.Close()
	id := callAs(t, ts, "bob", "sandbox_create", nil)["structuredContent"].(map[string]any)["workspaceId"].(string)
	text := func(r map[string]any) string { return r["content"].([]any)[0].(map[string]any)["text"].(string) }
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"sandbox_import_file", map[string]any{"workspaceId": id, "source": "attachment:1", "path": "a.docx"}, "not valid"},
		{"sandbox_import_file", map[string]any{"workspaceId": id, "source": ticket("acme", "alice", "read"), "path": "a.docx"}, "someone else"},
		{"sandbox_import_file", map[string]any{"workspaceId": id, "source": ticket("acme", "bob", "write"), "path": "a.docx"}, "not valid"},
		{"sandbox_import_file", map[string]any{"workspaceId": id, "source": ticket("acme", "bob", "read"), "path": "a.docx"}, "macros"},
		{"sandbox_export_file", map[string]any{"workspaceId": id, "path": "x.docx", "target": ticket("acme", "bob", "read")}, "not valid"},
		{"sandbox_export_file", map[string]any{"workspaceId": id, "path": "missing.docx", "target": ticket("acme", "bob", "write")}, "no such file"},
	} {
		r := callAs(t, ts, "bob", c.tool, c.args)
		if r["isError"] != true || !strings.Contains(text(r), c.want) {
			t.Errorf("%s %v: %v", c.tool, c.args["source"], r)
		}
	}
	f.files["idp-sbx-"+id+"/run.sh"] = []byte("#!/bin/sh")
	r := callAs(t, ts, "bob", "sandbox_export_file", map[string]any{"workspaceId": id, "path": "run.sh", "target": ticket("acme", "bob", "write")})
	if r["isError"] != true || ws.delivered != nil {
		t.Fatalf("a script was delivered: %v", r)
	}
	f.files["idp-sbx-"+id+"/fake.docx"] = []byte("plain text")
	r = callAs(t, ts, "bob", "sandbox_export_file", map[string]any{"workspaceId": id, "path": "fake.docx", "target": ticket("acme", "bob", "write")})
	if r["isError"] != true || ws.delivered != nil {
		t.Fatalf("a file that is not what its name says was delivered: %v", r)
	}
}

func TestTransferToolsNeedABinaryBackendAndAnEndpoint(t *testing.T) {
	f := &fake{files: map[string][]byte{}}
	ts, wts := transferServer(t, f, &workspaceFiles{}) // the plain fake does not move bytes unchanged
	defer ts.Close()
	defer wts.Close()
	if l := toolNames(t, ts); strings.Contains(l, "sandbox_import_file") {
		t.Fatal("transfer tools on a backend that cannot move bytes unchanged")
	}
	plain := server(t, &fake{files: map[string][]byte{}})
	defer plain.Close()
	if l := toolNames(t, plain); strings.Contains(l, "sandbox_export_file") {
		t.Fatal("transfer tools without the Workspace's endpoint")
	}
}
