// Package e2e drives idp-sandbox behind a real idp-mcp gateway (deploy/e2e.sh): the sandbox is registered
// as an upstream, discovered, reviewed and granted; from then on it is used only through the gateway with
// an API key, so authorization, argument validation and audit are the gateway's.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

type env struct {
	t                            *testing.T
	registry, gateway, gatewayID string
	admin, operator, reviewer    string
	http                         *http.Client
}

func (e *env) api(tok, method, path string, body any, want int) map[string]any {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.registry+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: HTTP %d (want %d): %s", method, path, resp.StatusCode, want, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"idp-sandbox-e2e","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`

func (e *env) mcp(key, method, params, name string) (int, map[string]any) {
	if params == "" {
		params = "{" + meta + "}"
	}
	req, _ := http.NewRequest("POST", e.gateway+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"`+method+`","params":`+params+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (e *env) tool(key, name string, args map[string]any) (int, map[string]any) {
	b, _ := json.Marshal(args)
	return e.mcp(key, "tools/call", `{"name":"`+name+`","arguments":`+string(b)+`,`+meta+`}`, name)
}

func result(m map[string]any) map[string]any { r, _ := m["result"].(map[string]any); return r }

func structured(m map[string]any) map[string]any {
	s, _ := result(m)["structuredContent"].(map[string]any)
	return s
}

func TestSandboxBehindTheGateway(t *testing.T) {
	e := &env{t: t, registry: os.Getenv("IDP_E2E_REGISTRY"), gateway: os.Getenv("IDP_E2E_GATEWAY"), gatewayID: os.Getenv("IDP_E2E_GATEWAY_ID"),
		admin: os.Getenv("IDP_E2E_ADMIN"), operator: os.Getenv("IDP_E2E_OPERATOR"), reviewer: os.Getenv("IDP_E2E_REVIEWER"),
		http: &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{Proxy: nil}}}
	if e.registry == "" {
		t.Skip("run through deploy/e2e.sh")
	}
	var failed []string
	check := func(id string, ok bool, detail ...any) {
		t.Helper()
		if !ok {
			failed = append(failed, id)
			t.Errorf("FAIL %s %v", id, detail)
		}
	}

	// register, discover, review
	srv := e.api(e.operator, "POST", "/api/v1/servers", map[string]string{"displayName": "sandbox", "targetName": "sandbox", "endpoint": "http://sandbox:8570/mcp"}, 201)
	id := srv["serverId"].(string)
	disc := e.api(e.operator, "POST", "/api/v1/servers/"+id+"/discover", nil, 200)
	var discovered []string
	for _, n := range disc["toolNames"].([]any) {
		discovered = append(discovered, n.(string))
	}
	sort.Strings(discovered)
	check("discovered-tools", strings.Join(discovered, ",") == "sandbox_create,sandbox_delete,sandbox_list_files,sandbox_read_file,sandbox_run,sandbox_write_file", discovered)
	e.api(e.reviewer, "POST", "/api/v1/servers/"+id+"/approve", nil, 200)
	// the operator role may use every tool except deleting; the "guest" role gets read-only tools only
	for _, g := range []map[string]any{
		{"serverId": id, "tools": []string{"sandbox_create", "sandbox_run", "sandbox_write_file", "sandbox_read_file", "sandbox_list_files"}, "roles": []string{"operator"}},
		{"serverId": id, "tools": []string{"sandbox_list_files"}, "roles": []string{"guest"}},
	} {
		gr := e.api(e.operator, "POST", "/api/v1/grants", g, 201)
		e.api(e.reviewer, "POST", "/api/v1/grants/"+gr["grantId"].(string)+"/approve", nil, 200)
	}
	opKey := e.api(e.admin, "POST", "/api/v1/gateways/"+e.gatewayID+"/api-keys", map[string]any{"subject": "agent-host", "roles": []string{"operator"}}, 201)["key"].(string)
	guestKey := e.api(e.admin, "POST", "/api/v1/gateways/"+e.gatewayID+"/api-keys", map[string]any{"subject": "visitor", "roles": []string{"guest"}}, 201)["key"].(string)
	rel := e.api(e.operator, "POST", "/api/v1/releases", map[string]string{"gatewayId": e.gatewayID}, 201)
	rid := rel["releaseId"].(string)
	e.api(e.reviewer, "POST", "/api/v1/releases/"+rid+"/approve", nil, 200)
	e.api(e.admin, "POST", "/api/v1/releases/"+rid+"/deploy", nil, 200)

	// what the operator may see and what a guest may
	names := func(key string) string {
		_, l := e.mcp(key, "tools/list", "", "")
		var out []string
		if r := result(l); r != nil {
			for _, tl := range r["tools"].([]any) {
				out = append(out, tl.(map[string]any)["name"].(string))
			}
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	check("operator-catalog", names(opKey) == "sandbox__sandbox_create,sandbox__sandbox_list_files,sandbox__sandbox_read_file,sandbox__sandbox_run,sandbox__sandbox_write_file", names(opKey))
	check("guest-catalog", names(guestKey) == "sandbox__sandbox_list_files", names(guestKey))
	st, _ := e.mcp("", "tools/list", "", "")
	check("anonymous-denied", st == 401, st)

	// the agent's path: create, write, run, read - all through the gateway
	st, c := e.tool(opKey, "sandbox__sandbox_create", map[string]any{})
	ws, _ := structured(c)["workspaceId"].(string)
	check("create", st == 200 && len(ws) == 32, st, c)
	st, w := e.tool(opKey, "sandbox__sandbox_write_file", map[string]any{"workspaceId": ws, "path": "data/in.csv", "content": "a,b\n1,2\n3,4\n"})
	check("write", st == 200 && result(w)["isError"] != true, st, w)
	st, r := e.tool(opKey, "sandbox__sandbox_run", map[string]any{"workspaceId": ws, "language": "python",
		"code": "import csv\nrows = list(csv.DictReader(open('data/in.csv')))\ns = sum(int(r['a']) + int(r['b']) for r in rows)\nopen('out.txt','w').write(str(s))\nprint('sum', s)"})
	check("run", st == 200 && structured(r)["exitCode"] == float64(0) && strings.Contains(fmt.Sprint(structured(r)["stdout"]), "sum 10"), st, r)
	st, rd := e.tool(opKey, "sandbox__sandbox_read_file", map[string]any{"workspaceId": ws, "path": "out.txt"})
	check("read-result", st == 200 && fmt.Sprint(result(rd)["content"]) == "[map[text:10 type:text]]", st, rd)

	// the sandbox holds against a hostile agent, through the same path
	st, hr := e.tool(opKey, "sandbox__sandbox_run", map[string]any{"workspaceId": ws, "language": "python",
		"code": "import socket\ntry:\n    socket.create_connection(('1.1.1.1', 53), 3); print('CONNECTED')\nexcept OSError: print('no network')\nimport os\nprint('uid', os.getuid())\ntry:\n    open('/etc/x','w')\nexcept OSError: print('read-only root')"})
	out := fmt.Sprint(structured(hr)["stdout"])
	check("hostile-code-contained", st == 200 && strings.Contains(out, "no network") && strings.Contains(out, "uid 65534") && strings.Contains(out, "read-only root") && !strings.Contains(out, "CONNECTED"), st, hr)
	st, oom := e.tool(opKey, "sandbox__sandbox_run", map[string]any{"workspaceId": ws, "language": "python", "code": "x=[]\nwhile True: x.append(bytearray(16<<20))"})
	check("memory-bomb-killed", st == 200 && structured(oom)["outOfMemory"] == true, st, oom)

	// the gateway's own checks: arguments are validated against the pinned schema before the sandbox sees them
	for name, args := range map[string]map[string]any{
		"path-injection-id": {"workspaceId": "../../etc", "language": "python", "code": "print(1)"},
		"extra-field":       {"workspaceId": ws, "language": "python", "code": "print(1)", "image": "attacker/image"},
		"unlisted-language": {"workspaceId": ws, "language": "ruby", "code": "puts 1"},
	} {
		st, m := e.tool(opKey, "sandbox__sandbox_run", args)
		er, _ := m["error"].(map[string]any)
		check("gateway-rejects-"+name, er != nil && er["code"] == float64(-32602), st, m)
	}
	// authorization: a guest cannot run code, cannot see the delete tool, and an ungranted tool does not exist for the operator
	st, m := e.tool(guestKey, "sandbox__sandbox_run", map[string]any{"workspaceId": ws, "language": "python", "code": "print(1)"})
	check("guest-cannot-run", st != 200 || m["error"] != nil, st, m)
	st, m = e.tool(opKey, "sandbox__sandbox_delete", map[string]any{"workspaceId": ws})
	check("operator-has-no-delete", st != 200 || m["error"] != nil, st, m)

	// audit: the gateway recorded the calls, without the code in clear
	deadline := time.Now().Add(10 * time.Second)
	var inv []any
	for time.Now().Before(deadline) {
		if inv = e.api(e.reviewer, "GET", "/api/v1/invocations", nil, 200)["items"].([]any); len(inv) >= 6 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	check("invocations-recorded", len(inv) >= 6 && !strings.Contains(fmt.Sprint(inv), "csv.DictReader"), len(inv))
	t.Logf("sandbox behind the gateway: %d checks failed %v", len(failed), failed)
}
