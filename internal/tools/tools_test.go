package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"idp-sandbox/internal/mcp"
	"idp-sandbox/internal/sandbox"
)

type fake struct {
	files map[string][]byte
	fail  error
}

func (f *fake) Ping(context.Context) error                           { return nil }
func (f *fake) CreateWorkspace(context.Context, string, int64) error { return f.fail }
func (f *fake) DeleteWorkspace(context.Context, string) error        { return nil }
func (f *fake) Sweep(context.Context) error                          { return nil }
func (f *fake) WriteFile(_ context.Context, ws, p string, d []byte) error {
	f.files[ws+"/"+p] = d
	return nil
}
func (f *fake) ListFiles(context.Context, string, int) ([]sandbox.FileInfo, error) {
	return []sandbox.FileInfo{{Path: ".idp/main.py", Size: 3}, {Path: "a.txt", Size: 1}}, nil
}
func (f *fake) ReadFile(_ context.Context, ws, p string, _ int64) ([]byte, error) {
	if d, ok := f.files[ws+"/"+p]; ok {
		return d, nil
	}
	return nil, sandbox.ErrNoFile
}
func (f *fake) Run(_ context.Context, s sandbox.RunSpec) (sandbox.RunResult, error) {
	return sandbox.RunResult{ExitCode: 0, Stdout: "out \xff", Stderr: "err", Duration: time.Millisecond}, f.fail
}

type nometrics struct{}

func (nometrics) Observe(string, string, time.Duration) {}

func server(t *testing.T, f *fake) *httptest.Server {
	t.Helper()
	m, err := sandbox.New(context.Background(), sandbox.DefaultConfig("py", "sh"), f, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(mcp.New("t", "1", "", New(m, slog.New(slog.DiscardHandler), nometrics{})))
}

func call(t *testing.T, ts *httptest.Server, tool string, args map[string]any) map[string]any {
	t.Helper()
	return callAs(t, ts, "alice", tool, args)
}

func callAs(t *testing.T, ts *httptest.Server, subject, tool string, args map[string]any) map[string]any {
	t.Helper()
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	if subject != "" {
		meta["io.idp.telos/caller"] = map[string]any{"subject": subject, "tenant": "acme", "roles": []string{"operator"}}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args, "_meta": meta}})
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(string(b)))
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if m["error"] != nil {
		t.Fatalf("%s: %v", tool, m["error"])
	}
	return m["result"].(map[string]any)
}

func TestWorkflow(t *testing.T) {
	f := &fake{files: map[string][]byte{}}
	ts := server(t, f)
	defer ts.Close()
	id := call(t, ts, "sandbox_create", nil)["structuredContent"].(map[string]any)["workspaceId"].(string)
	if len(id) != 32 {
		t.Fatalf("id %q", id)
	}
	r := call(t, ts, "sandbox_run", map[string]any{"workspaceId": id, "language": "python", "code": "print(1)"})
	if r["isError"] != nil || r["structuredContent"].(map[string]any)["exitCode"] != float64(0) || !strings.Contains(r["structuredContent"].(map[string]any)["stdout"].(string), "out �") {
		t.Fatalf("%v", r)
	}
	if r := call(t, ts, "sandbox_write_file", map[string]any{"workspaceId": id, "path": "a.txt", "content": "x"}); r["isError"] != nil {
		t.Fatalf("%v", r)
	}
	if r := call(t, ts, "sandbox_read_file", map[string]any{"workspaceId": id, "path": "a.txt"}); r["content"].([]any)[0].(map[string]any)["text"] != "x" {
		t.Fatalf("%v", r)
	}
	l := call(t, ts, "sandbox_list_files", map[string]any{"workspaceId": id})["structuredContent"].(map[string]any)["files"].([]any)
	if len(l) != 1 || l[0].(map[string]any)["path"] != "a.txt" {
		t.Fatalf("the runner's scratch directory is listed: %v", l)
	}
	if r := call(t, ts, "sandbox_delete", map[string]any{"workspaceId": id}); r["isError"] != nil {
		t.Fatalf("%v", r)
	}
	if r := call(t, ts, "sandbox_read_file", map[string]any{"workspaceId": id, "path": "a.txt"}); r["isError"] != true {
		t.Fatalf("a deleted workspace was readable: %v", r)
	}
}

func TestErrorsReachTheModelButInternalsDoNot(t *testing.T) {
	f := &fake{files: map[string][]byte{}}
	ts := server(t, f)
	defer ts.Close()
	r := call(t, ts, "sandbox_run", map[string]any{"workspaceId": strings.Repeat("0", 32), "language": "python", "code": "x"})
	if r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "not found") {
		t.Fatalf("%v", r)
	}
	f.fail = errors.New("docker: HTTP 500: dial unix /var/run/docker.sock: secret-detail")
	r = call(t, ts, "sandbox_create", nil)
	txt := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if r["isError"] != true || strings.Contains(txt, "docker") || strings.Contains(txt, "secret") {
		t.Fatalf("an internal error reached the model: %q", txt)
	}
}

func TestStrictArguments(t *testing.T) {
	ts := server(t, &fake{files: map[string][]byte{}})
	defer ts.Close()
	id := call(t, ts, "sandbox_create", nil)["structuredContent"].(map[string]any)["workspaceId"].(string)
	for name, args := range map[string]map[string]any{
		"unknown field":   {"workspaceId": id, "language": "python", "code": "x", "image": "evil"},
		"wrong type":      {"workspaceId": id, "language": "python", "code": 7},
		"unlisted lang":   {"workspaceId": id, "language": "ruby", "code": "x"},
		"traversal path":  {"workspaceId": id, "path": "../etc/passwd", "content": "x"},
		"reserved path":   {"workspaceId": id, "path": ".idp/main.py", "content": "x"},
		"missing content": {"workspaceId": id, "path": "a"},
	} {
		tool := "sandbox_run"
		if strings.Contains(name, "path") || name == "missing content" {
			tool = "sandbox_write_file"
		}
		if r := call(t, ts, tool, args); r["isError"] != true {
			t.Errorf("%s was accepted: %v", name, r)
		}
	}
}

func TestEveryToolHasASchemaTheGatewayCanPin(t *testing.T) {
	m, _ := sandbox.New(context.Background(), sandbox.DefaultConfig("py", "sh"), &fake{files: map[string][]byte{}}, nil)
	for _, tool := range New(m, slog.New(slog.DiscardHandler), nometrics{}) {
		if !strings.HasPrefix(tool.Name, "sandbox_") || tool.Description == "" || tool.InputSchema["type"] != "object" || tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: %v", tool.Name, tool.InputSchema)
		}
		if _, err := json.Marshal(tool.InputSchema); err != nil {
			t.Errorf("%s: %v", tool.Name, err)
		}
	}
}

func TestWorkspacesBelongToTheForwardedCaller(t *testing.T) {
	f := &fake{files: map[string][]byte{}}
	ts := server(t, f)
	defer ts.Close()
	id := callAs(t, ts, "alice", "sandbox_create", nil)["structuredContent"].(map[string]any)["workspaceId"].(string)
	callAs(t, ts, "alice", "sandbox_write_file", map[string]any{"workspaceId": id, "path": "a.txt", "content": "private"})
	// another caller, holding the id, gets "not found" - the same answer as for an id that never existed
	other := callAs(t, ts, "bob", "sandbox_read_file", map[string]any{"workspaceId": id, "path": "a.txt"})
	missing := callAs(t, ts, "bob", "sandbox_read_file", map[string]any{"workspaceId": strings.Repeat("1", 32), "path": "a.txt"})
	if other["isError"] != true || fmt.Sprint(other["content"]) != fmt.Sprint(missing["content"]) {
		t.Fatalf("%v vs %v", other, missing)
	}
	// a request without identity is refused (the server was not registered with identity=forward)
	r := callAs(t, ts, "", "sandbox_create", nil)
	if r["isError"] != true || !strings.Contains(fmt.Sprint(r["content"]), "no caller identity") {
		t.Fatalf("%v", r)
	}
}

func TestNetworkParameterFollowsTheOperatorsConfiguration(t *testing.T) {
	f := &fake{files: map[string][]byte{}}
	ts := server(t, f)
	defer ts.Close()
	id := call(t, ts, "sandbox_create", nil)["structuredContent"].(map[string]any)["workspaceId"].(string)
	// egress is not configured: the schema offers only "none" and a request for egress is refused
	m, _ := sandbox.New(context.Background(), sandbox.DefaultConfig("py", "sh"), f, slog.New(slog.DiscardHandler))
	for _, tool := range New(m, slog.New(slog.DiscardHandler), nometrics{}) {
		if tool.Name == "sandbox_run" {
			enum := tool.InputSchema["properties"].(map[string]any)["network"].(map[string]any)["enum"].([]string)
			if len(enum) != 1 || enum[0] != "none" {
				t.Fatalf("%v", enum)
			}
		}
	}
	r := call(t, ts, "sandbox_run", map[string]any{"workspaceId": id, "language": "python", "code": "x", "network": "egress"})
	if r["isError"] != true || !strings.Contains(fmt.Sprint(r["content"]), "not enabled") {
		t.Fatalf("%v", r)
	}
	r = call(t, ts, "sandbox_run", map[string]any{"workspaceId": id, "language": "python", "code": "x", "network": "host"})
	if r["isError"] != true {
		t.Fatalf("an unknown network was accepted: %v", r)
	}
	// with egress configured the schema offers it
	cfg := sandbox.DefaultConfig("py", "sh")
	cfg.EgressEnabled = true
	m2, _ := sandbox.New(context.Background(), cfg, f, slog.New(slog.DiscardHandler))
	for _, tool := range New(m2, slog.New(slog.DiscardHandler), nometrics{}) {
		if tool.Name == "sandbox_run" {
			enum := tool.InputSchema["properties"].(map[string]any)["network"].(map[string]any)["enum"].([]string)
			if len(enum) != 2 || enum[1] != "egress" {
				t.Fatalf("%v", enum)
			}
		}
	}
}

// The office run profile (ADR-0001 D14) is offered only when the operator configured its image, and the schema says what each
// language is, so the model can pick the right one.
func TestOfficeLanguageFollowsTheOperatorsConfiguration(t *testing.T) {
	langs := func(cfg sandbox.Config) ([]string, string) {
		m, _ := sandbox.New(context.Background(), cfg, &fake{files: map[string][]byte{}}, slog.New(slog.DiscardHandler))
		for _, tool := range New(m, slog.New(slog.DiscardHandler), nometrics{}) {
			if tool.Name == "sandbox_run" {
				p := tool.InputSchema["properties"].(map[string]any)["language"].(map[string]any)
				return p["enum"].([]string), p["description"].(string)
			}
		}
		t.Fatal("no sandbox_run")
		return nil, ""
	}
	enum, desc := langs(sandbox.DefaultConfig("py", "sh"))
	if fmt.Sprint(enum) != "[python shell]" || strings.Contains(desc, "officecli") || !strings.Contains(desc, "python: a Python 3 program") {
		t.Fatalf("%v %q", enum, desc)
	}
	cfg := sandbox.DefaultConfig("py", "sh")
	cfg.Languages["office"] = sandbox.OfficeLanguage("idp-sandbox-office:dev")
	enum, desc = langs(cfg)
	if fmt.Sprint(enum) != "[office python shell]" || !strings.Contains(desc, "office: a POSIX shell script with the officecli command") {
		t.Fatalf("%v %q", enum, desc)
	}
	if cfg.Languages["office"].Image != "idp-sandbox-office:dev" || fmt.Sprint(cfg.Languages["office"].Command("main.sh")) != "[sh main.sh]" {
		t.Fatalf("%+v", cfg.Languages["office"])
	}
}
