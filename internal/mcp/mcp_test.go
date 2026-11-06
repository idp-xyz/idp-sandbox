package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func echo() *Server {
	return New("t", "1", "", []Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"},
		Call: func(_ *http.Request, a map[string]json.RawMessage) Result {
			return Result{Text: string(a["x"]), Structured: map[string]any{"x": string(a["x"])}}
		}}})
}

func post(t *testing.T, s *Server, headers map[string]string, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w, m
}

var ok = map[string]string{"MCP-Protocol-Version": ProtocolVersion, "Content-Type": "application/json"}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func TestDiscoverIsStrictlyModernAndOffersTools(t *testing.T) {
	w, m := post(t, echo(), ok, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`)
	res, _ := m["result"].(map[string]any)
	if w.Code != 200 || res["resultType"] != "complete" || len(res["supportedVersions"].([]any)) != 1 || res["supportedVersions"].([]any)[0] != "2026-07-28" {
		t.Fatalf("%d %v", w.Code, m)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("no tools capability")
	}
	info := res["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if info["name"] != "t" {
		t.Fatalf("%v", info)
	}
}

func TestToolsListAndCall(t *testing.T) {
	_, m := post(t, echo(), ok, `{"jsonrpc":"2.0","id":"a","method":"tools/list","params":{`+meta+`}}`)
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("%v", m)
	}
	h := map[string]string{"MCP-Protocol-Version": ProtocolVersion, "Mcp-Method": "tools/call", "Mcp-Name": "echo"}
	w, m := post(t, echo(), h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"x":5},`+meta+`}}`)
	res := m["result"].(map[string]any)
	if w.Code != 200 || res["resultType"] != "complete" || res["content"].([]any)[0].(map[string]any)["text"] != "5" || res["structuredContent"] == nil {
		t.Fatalf("%d %v", w.Code, m)
	}
}

func TestAdmissionRules(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		body    string
		status  int
		code    float64
	}{
		{"no version header", nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`, 400, CodeUnsupported},
		{"old version", map[string]string{"MCP-Protocol-Version": "2025-06-18"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`, 400, CodeUnsupported},
		{"legacy initialize", ok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` + meta + `}}`, 404, CodeNotFound},
		{"batch", ok, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, 400, CodeInvalidRequest},
		{"garbage", ok, `{`, 400, CodeParse},
		{"method header mismatch", map[string]string{"MCP-Protocol-Version": ProtocolVersion, "Mcp-Method": "tools/call"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`, 400, CodeMismatch},
		{"no _meta", ok, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, 400, CodeInvalidParams},
		{"body version differs", ok, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18","io.modelcontextprotocol/clientCapabilities":{}}}}`, 400, CodeMismatch},
		{"no capabilities", ok, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, 400, CodeInvalidParams},
		{"ping is removed", ok, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{` + meta + `}}`, 404, CodeNotFound},
		{"fractional id", ok, `{"jsonrpc":"2.0","id":1.5,"method":"tools/list","params":{` + meta + `}}`, 400, CodeInvalidRequest},
		{"name header mismatch", map[string]string{"MCP-Protocol-Version": ProtocolVersion, "Mcp-Name": "other"}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + meta + `}}`, 400, CodeMismatch},
	}
	for _, c := range cases {
		w, m := post(t, echo(), c.headers, c.body)
		e, _ := m["error"].(map[string]any)
		if w.Code != c.status || e == nil || e["code"] != c.code {
			t.Errorf("%s: %d %v", c.name, w.Code, m)
		}
	}
}

func TestUnknownToolAndBadArguments(t *testing.T) {
	_, m := post(t, echo(), ok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope",`+meta+`}}`)
	if m["error"].(map[string]any)["code"] != float64(CodeInvalidParams) {
		t.Fatalf("%v", m)
	}
	_, m = post(t, echo(), ok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":[1],`+meta+`}}`)
	if m["error"].(map[string]any)["code"] != float64(CodeInvalidParams) {
		t.Fatalf("%v", m)
	}
}

func TestOnlyPostAndNotificationsAreAccepted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	echo().ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("%d", w.Code)
	}
	w, _ = post(t, echo(), ok, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`)
	if w.Code != 202 {
		t.Fatalf("%d", w.Code)
	}
}
