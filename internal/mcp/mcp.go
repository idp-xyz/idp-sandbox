// Package mcp serves the sandbox as an MCP 2026-07-28 modern server over Streamable HTTP (JSON
// responses): server/discover, tools/list and tools/call. The admission rules are those of idp-mcp's
// gateway (copied, ADR-0001 D6): the gateway is the only client this server is meant to have.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// ProtocolVersion is the only revision spoken.
const ProtocolVersion = "2026-07-28"

const (
	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaCaps    = "io.modelcontextprotocol/clientCapabilities"
	metaServer  = "io.modelcontextprotocol/serverInfo"
	maxBody     = 1 << 20
)

// JSON-RPC and MCP error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeNotFound       = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeMismatch       = -32020
	CodeUnsupported    = -32022
)

// Error is a JSON-RPC error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Tool is one callable. Call gets the validated arguments object.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Annotations map[string]any
	Call        func(r *http.Request, args map[string]json.RawMessage) Result
}

// Result is a tool result: text for the model, structured data for programs.
type Result struct {
	Text       string
	Structured any
	IsError    bool
}

// MetaCaller is where idp-mcp's gateway puts the verified caller for a server approved to receive it
// (idp-mcp ADR-0001 D40). The gateway removes a client's own value of this key, so it can be trusted
// as far as the gateway is.
const MetaCaller = "io.idp.telos/caller"

// Caller is the verified caller the gateway forwarded.
type Caller struct {
	Subject string   `json:"subject"`
	Tenant  string   `json:"tenant"`
	Roles   []string `json:"roles"`
	Client  string   `json:"client,omitempty"`
	Actor   string   `json:"actor,omitempty"`
}

// Owner is the stable identity workspaces are bound to: tenant and subject.
func (c Caller) Owner() string {
	if c.Subject == "" {
		return ""
	}
	return c.Tenant + "/" + c.Subject
}

type callerKey struct{}

// CallerFrom returns the caller of a tool request (zero value if none was forwarded).
func CallerFrom(r *http.Request) Caller {
	c, _ := r.Context().Value(callerKey{}).(Caller)
	return c
}

// Server is the MCP endpoint.
type Server struct {
	Name, Version string
	Instructions  string
	Tools         []Tool
	byName        map[string]*Tool
}

// New builds the server.
func New(name, version, instructions string, tools []Tool) *Server {
	s := &Server{Name: name, Version: version, Instructions: instructions, Tools: tools, byName: map[string]*Tool{}}
	for i := range s.Tools {
		s.byName[s.Tools[i].Name] = &s.Tools[i]
	}
	return s
}

func write(w http.ResponseWriter, status int, id json.RawMessage, result any, e *Error) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(msg)
}

func validID(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	switch x := v.(type) {
	case string:
		return raw
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return raw
		}
	}
	return nil
}

// ServeHTTP handles POST /mcp.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		write(w, http.StatusMethodNotAllowed, nil, nil, &Error{CodeInvalidRequest, "Use POST", nil})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		write(w, http.StatusRequestEntityTooLarge, nil, nil, &Error{CodeInvalidRequest, "Request body too large", nil})
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		write(w, 400, nil, nil, &Error{CodeInvalidRequest, "JSON-RPC batches are not supported", nil})
		return
	}
	var req request
	if json.Unmarshal(trimmed, &req) != nil {
		write(w, 400, nil, nil, &Error{CodeParse, "Parse error", nil})
		return
	}
	id := validID(req.ID)
	if req.JSONRPC != "2.0" || req.Method == "" || (len(req.ID) > 0 && string(bytes.TrimSpace(req.ID)) != "null" && id == nil) {
		write(w, 400, id, nil, &Error{CodeInvalidRequest, "Invalid JSON-RPC request", nil})
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != ProtocolVersion {
		write(w, 400, id, nil, &Error{CodeUnsupported, "Unsupported protocol version",
			map[string]any{"supported": []string{ProtocolVersion}}})
		return
	}
	if m := r.Header.Get("Mcp-Method"); m != "" && m != req.Method {
		write(w, 400, id, nil, &Error{CodeMismatch, "Mcp-Method header does not match the body method", nil})
		return
	}
	if len(id) == 0 { // a notification: accepted, no body
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(req.Params, &params) != nil || params == nil {
		write(w, 400, id, nil, &Error{CodeInvalidParams, "params must be an object", nil})
		return
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil || meta == nil {
		write(w, 400, id, nil, &Error{CodeInvalidParams, "params._meta must be an object", nil})
		return
	}
	var ver string
	if json.Unmarshal(meta[metaVersion], &ver) != nil || ver != ProtocolVersion {
		write(w, 400, id, nil, &Error{CodeMismatch, "MCP header/body protocol version mismatch", nil})
		return
	}
	var caps map[string]json.RawMessage
	if json.Unmarshal(meta[metaCaps], &caps) != nil || caps == nil {
		write(w, 400, id, nil, &Error{CodeInvalidParams, "params._meta is missing " + metaCaps, nil})
		return
	}
	switch req.Method {
	case "server/discover":
		res := map[string]any{"supportedVersions": []string{ProtocolVersion}, "resultType": "complete",
			"capabilities": map[string]any{"tools": map[string]any{}},
			"_meta":        map[string]any{metaServer: map[string]any{"name": s.Name, "version": s.Version}}}
		if s.Instructions != "" {
			res["instructions"] = s.Instructions
		}
		write(w, 200, id, res, nil)
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
			if t.Annotations != nil {
				m["annotations"] = t.Annotations
			}
			tools = append(tools, m)
		}
		write(w, 200, id, map[string]any{"tools": tools, "resultType": "complete"}, nil)
	case "tools/call":
		s.call(w, r, id, params)
	default:
		write(w, 404, id, nil, &Error{CodeNotFound, "Method not found: " + req.Method, nil})
	}
}

func (s *Server) call(w http.ResponseWriter, r *http.Request, id json.RawMessage, params map[string]json.RawMessage) {
	var name string
	if json.Unmarshal(params["name"], &name) != nil || name == "" {
		write(w, 400, id, nil, &Error{CodeInvalidParams, "params.name must be a non-empty string", nil})
		return
	}
	if h := r.Header.Get("Mcp-Name"); h != "" && h != name {
		write(w, 400, id, nil, &Error{CodeMismatch, "invalid MCP routing header: Mcp-Name", nil})
		return
	}
	t := s.byName[name]
	if t == nil {
		write(w, 200, id, nil, &Error{CodeInvalidParams, "Unknown tool: " + name, nil})
		return
	}
	args := map[string]json.RawMessage{}
	if raw, ok := params["arguments"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		if json.Unmarshal(raw, &args) != nil || args == nil {
			write(w, 200, id, nil, &Error{CodeInvalidParams, "arguments must be an object", nil})
			return
		}
	}
	var c Caller
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) == nil {
		_ = json.Unmarshal(meta[MetaCaller], &c)
	}
	res := t.Call(r.WithContext(context.WithValue(r.Context(), callerKey{}, c)), args)
	out := map[string]any{"resultType": "complete", "content": []map[string]any{{"type": "text", "text": res.Text}}}
	if res.Structured != nil {
		out["structuredContent"] = res.Structured
	}
	if res.IsError {
		out["isError"] = true
	}
	write(w, 200, id, out, nil)
}
