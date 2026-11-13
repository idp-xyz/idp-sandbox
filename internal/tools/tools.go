// Package tools defines the sandbox's MCP tools on top of the sandbox manager.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"idp-sandbox/internal/mcp"
	"idp-sandbox/internal/sandbox"
)

// Metrics counts calls by tool and outcome (read by the /metrics endpoint).
type Metrics interface {
	Observe(tool, outcome string, d time.Duration)
}

type api struct {
	m   *sandbox.Manager
	log *slog.Logger
	met Metrics
}

func decode(args map[string]json.RawMessage, into any) error {
	b, _ := json.Marshal(args)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", sandbox.ErrInvalid, err)
	}
	return nil
}

// failure turns an error into a tool error the model can act on; internals stay in the log.
func failure(err error) mcp.Result {
	switch {
	case errors.Is(err, sandbox.ErrNotFound), errors.Is(err, sandbox.ErrBusy), errors.Is(err, sandbox.ErrCapacity),
		errors.Is(err, sandbox.ErrInvalid), errors.Is(err, sandbox.ErrOwnerQuota), errors.Is(err, sandbox.ErrNoCaller), errors.Is(err, sandbox.ErrNoEgress), errors.Is(err, sandbox.ErrFileTooLarge), errors.Is(err, sandbox.ErrNoFile):
		return mcp.Result{Text: err.Error(), IsError: true}
	}
	return mcp.Result{Text: "the sandbox failed to complete the request", IsError: true}
}

func (a *api) wrap(name string, f func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string)) func(*http.Request, map[string]json.RawMessage) mcp.Result {
	return func(r *http.Request, args map[string]json.RawMessage) mcp.Result {
		start := time.Now()
		caller := mcp.CallerFrom(r)
		res, detail := f(r.Context(), caller.Owner(), args)
		if caller.Subject != "" {
			detail += " caller=" + short(caller.Subject)
		}
		outcome := "ok"
		if res.IsError {
			outcome = "error"
		}
		a.met.Observe(name, outcome, time.Since(start))
		// the audit line names the tool and the outcome, never the code or file contents
		a.log.Info("tool", "tool", name, "outcome", outcome, "ms", time.Since(start).Milliseconds(), "detail", detail)
		return res
	}
}

func idProp() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$", "description": "The workspace id returned by sandbox_create."}
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func short(id string) string {
	if len(id) < 8 {
		return ""
	}
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:4])
}

// New returns the tools.
func New(m *sandbox.Manager, log *slog.Logger, met Metrics) []mcp.Tool {
	a := &api{m: m, log: log, met: met}
	cfg := m.Config()
	langs := []string{}
	for k := range cfg.Languages {
		langs = append(langs, k)
	}
	sortStrings(langs)
	var langDescription []string
	for _, k := range langs {
		if d := cfg.Languages[k].Description; d != "" {
			langDescription = append(langDescription, k+": "+d)
		}
	}
	networks, networkDescription := []string{"none"}, "Always none here: code has no network."
	if cfg.EgressEnabled {
		networks, networkDescription = []string{"none", "egress"}, "none (default): no network. egress: only the hosts the operator approved, through a proxy (HTTP_PROXY and HTTPS_PROXY are set)."
	}
	pathProp := map[string]any{"type": "string", "minLength": 1, "maxLength": 255, "description": "A path relative to the workspace, for example data/input.csv."}
	return []mcp.Tool{
		{Name: "sandbox_create",
			Description: fmt.Sprintf("Create an empty, private workspace for running code. It has no network, %d MiB of memory per run, %d MiB of files, and is deleted after %s without use. Returns the workspace id every other sandbox tool needs.",
				cfg.Limits.MemoryBytes>>20, cfg.Limits.WorkspaceBytes>>20, cfg.IdleTTL),
			InputSchema: obj(map[string]any{}),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
			Call: a.wrap("sandbox_create", func(ctx context.Context, owner string, _ map[string]json.RawMessage) (mcp.Result, string) {
				id, exp, err := m.Create(ctx, owner)
				if err != nil {
					a.log.Warn("create", "err", err)
					return failure(err), ""
				}
				return mcp.Result{Text: "workspace " + id + " created, expires " + exp.UTC().Format(time.RFC3339),
					Structured: map[string]any{"workspaceId": id, "expiresAt": exp.UTC().Format(time.RFC3339)}}, "ws=" + short(id)
			})},
		{Name: "sandbox_run",
			Description: fmt.Sprintf("Run code in a workspace and return its exit code, stdout and stderr. Files written under the working directory persist in the workspace for later runs. No network unless network=egress is offered (see that parameter), no installs, at most %s per run, %d KiB of output. Only one run at a time per workspace.",
				cfg.Limits.Timeout, cfg.Limits.OutputBytes>>10),
			InputSchema: obj(map[string]any{
				"workspaceId":    idProp(),
				"language":       map[string]any{"type": "string", "enum": langs, "description": strings.Join(langDescription, ". ") + "."},
				"code":           map[string]any{"type": "string", "minLength": 1, "maxLength": cfg.MaxCodeBytes},
				"timeoutSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": int(cfg.Limits.Timeout.Seconds())},
				"network":        map[string]any{"type": "string", "enum": networks, "description": networkDescription},
			}, "workspaceId", "language", "code"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
			Call: a.wrap("sandbox_run", func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string) {
				var in struct {
					WorkspaceID    string `json:"workspaceId"`
					Language       string `json:"language"`
					Code           string `json:"code"`
					TimeoutSeconds int    `json:"timeoutSeconds"`
					Network        string `json:"network"`
				}
				if err := decode(args, &in); err != nil {
					return failure(err), ""
				}
				sum := sha256.Sum256([]byte(in.Code))
				detail := fmt.Sprintf("ws=%s lang=%s code_sha256=%s", short(in.WorkspaceID), in.Language, hex.EncodeToString(sum[:6]))
				if in.Network != "" && in.Network != "none" && in.Network != "egress" {
					return failure(fmt.Errorf("%w: network must be none or egress", sandbox.ErrInvalid)), detail
				}
				detail += " network=" + map[bool]string{false: "none", true: "egress"}[in.Network == "egress"]
				res, err := m.Run(ctx, owner, in.WorkspaceID, in.Language, in.Code, time.Duration(in.TimeoutSeconds)*time.Second, in.Network == "egress")
				if err != nil {
					a.log.Warn("run", "err", err)
					return failure(err), detail
				}
				out := map[string]any{"exitCode": res.ExitCode, "stdout": text(res.Stdout), "stderr": text(res.Stderr),
					"timedOut": res.TimedOut, "outOfMemory": res.OOMKilled, "outputTruncated": res.Truncated,
					"durationMs": res.Duration.Milliseconds()}
				summary := fmt.Sprintf("exit code %d", res.ExitCode)
				if res.TimedOut {
					summary = "timed out"
				} else if res.OOMKilled {
					summary = "killed: out of memory"
				}
				body := summary + "\n--- stdout ---\n" + text(res.Stdout) + "\n--- stderr ---\n" + text(res.Stderr)
				if res.Truncated {
					body += "\n(output truncated)"
				}
				// a failing program is a result the model should read, not a tool failure
				return mcp.Result{Text: body, Structured: out}, detail + fmt.Sprintf(" exit=%d timedOut=%v oom=%v ms=%d", res.ExitCode, res.TimedOut, res.OOMKilled, res.Duration.Milliseconds())
			})},
		{Name: "sandbox_write_file",
			Description: fmt.Sprintf("Write a text file into a workspace (up to %d KiB), replacing an existing file.", cfg.MaxFileBytes>>10),
			InputSchema: obj(map[string]any{"workspaceId": idProp(), "path": pathProp,
				"content": map[string]any{"type": "string", "maxLength": int(cfg.MaxFileBytes)}}, "workspaceId", "path", "content"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
			Call: a.wrap("sandbox_write_file", func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string) {
				var in struct{ WorkspaceID, Path, Content string }
				if err := decodeFile(args, &in.WorkspaceID, &in.Path, &in.Content); err != nil {
					return failure(err), ""
				}
				if err := m.WriteFile(ctx, owner, in.WorkspaceID, in.Path, []byte(in.Content)); err != nil {
					a.log.Warn("write", "err", err)
					return failure(err), "ws=" + short(in.WorkspaceID)
				}
				return mcp.Result{Text: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)}, "ws=" + short(in.WorkspaceID)
			})},
		{Name: "sandbox_read_file",
			Description: "Read a text file from a workspace.",
			InputSchema: obj(map[string]any{"workspaceId": idProp(), "path": pathProp}, "workspaceId", "path"),
			Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
			Call: a.wrap("sandbox_read_file", func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string) {
				var ws, p string
				if err := decodeFile(args, &ws, &p, nil); err != nil {
					return failure(err), ""
				}
				b, err := m.ReadFile(ctx, owner, ws, p)
				if err != nil {
					if !errors.Is(err, sandbox.ErrNoFile) {
						a.log.Warn("read", "err", err)
					}
					return failure(err), "ws=" + short(ws)
				}
				if !utf8.Valid(b) {
					return mcp.Result{Text: "the file is not text", IsError: true}, "ws=" + short(ws)
				}
				return mcp.Result{Text: string(b), Structured: map[string]any{"path": p, "size": len(b)}}, "ws=" + short(ws)
			})},
		{Name: "sandbox_list_files",
			Description: "List the files in a workspace with their sizes.",
			InputSchema: obj(map[string]any{"workspaceId": idProp()}, "workspaceId"),
			Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
			Call: a.wrap("sandbox_list_files", func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string) {
				var in struct {
					WorkspaceID string `json:"workspaceId"`
				}
				if err := decode(args, &in); err != nil {
					return failure(err), ""
				}
				files, err := m.ListFiles(ctx, owner, in.WorkspaceID)
				if err != nil {
					a.log.Warn("list", "err", err)
					return failure(err), "ws=" + short(in.WorkspaceID)
				}
				if files == nil {
					files = []sandbox.FileInfo{}
				}
				txt := fmt.Sprintf("%d files", len(files))
				for _, f := range files {
					txt += fmt.Sprintf("\n%8d  %s", f.Size, f.Path)
				}
				return mcp.Result{Text: txt, Structured: map[string]any{"files": files}}, "ws=" + short(in.WorkspaceID)
			})},
		{Name: "sandbox_delete",
			Description: "Delete a workspace and all its files now.",
			InputSchema: obj(map[string]any{"workspaceId": idProp()}, "workspaceId"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
			Call: a.wrap("sandbox_delete", func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string) {
				var in struct {
					WorkspaceID string `json:"workspaceId"`
				}
				if err := decode(args, &in); err != nil {
					return failure(err), ""
				}
				if err := m.Delete(ctx, owner, in.WorkspaceID); err != nil {
					a.log.Warn("delete", "err", err)
					return failure(err), "ws=" + short(in.WorkspaceID)
				}
				return mcp.Result{Text: "deleted"}, "ws=" + short(in.WorkspaceID)
			})},
	}
}

// decodeFile reads workspaceId, path and (optionally) content.
func decodeFile(args map[string]json.RawMessage, ws, p, content *string) error {
	var in struct {
		WorkspaceID string  `json:"workspaceId"`
		Path        string  `json:"path"`
		Content     *string `json:"content"`
	}
	if err := decode(args, &in); err != nil {
		return err
	}
	*ws, *p = in.WorkspaceID, in.Path
	if content != nil {
		if in.Content == nil {
			return fmt.Errorf("%w: content is required", sandbox.ErrInvalid)
		}
		*content = *in.Content
	}
	return nil
}

// text makes program output safe for JSON: invalid UTF-8 becomes U+FFFD.
func text(s string) string { return string(bytes.ToValidUTF8([]byte(s), []byte("�"))) }

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
