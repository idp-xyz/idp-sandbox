package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"idp-sandbox/internal/files"
	"idp-sandbox/internal/mcp"
	"idp-sandbox/internal/sandbox"
)

// transferTools move a member's files between the Workspace and a workspace (ADR-0001 D15; platform ADR-0002 F4). The
// model writes attachment:N or output; idp-host puts the Workspace's ticket in its place before the call reaches the
// gateway (the properties' format, idp-attachment and idp-delivery, says where), so neither the model nor the gateway's records ever hold a ticket, and the bytes travel only between this
// server and the Workspace. Both directions check the file (files.Check).
func transferTools(a *api, m *sandbox.Manager, fc *files.Client) []mcp.Tool {
	cfg := m.Config()
	// the format tells idp-host which property takes a member's file: it puts the run's ticket in place of what the model
	// wrote there (idp-host ADR-0001 D37)
	ticketProp := func(format, desc string) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": 8192, "format": format, "description": desc}
	}
	pathProp := map[string]any{"type": "string", "minLength": 1, "maxLength": 255, "description": "A path relative to the workspace, for example in/report.docx."}
	return []mcp.Tool{
		{Name: "sandbox_import_file",
			Description: fmt.Sprintf("Copy a file the member attached to this request into a workspace (up to %d MiB; .docx .xlsx .pptx .pdf .csv .txt .md .png .jpg). source is the attachment's reference as you were given it: attachment:1, attachment:2, ...", cfg.MaxTransferBytes>>20),
			InputSchema: obj(map[string]any{"workspaceId": idProp(), "source": ticketProp("idp-attachment", "attachment:1, attachment:2, ... (the member's attachments, in the order you were told)"), "path": pathProp},
				"workspaceId", "source", "path"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
			Call: a.wrap("sandbox_import_file", func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string) {
				var in struct {
					WorkspaceID string `json:"workspaceId"`
					Source      string `json:"source"`
					Path        string `json:"path"`
				}
				if err := decode(args, &in); err != nil {
					return failure(err), ""
				}
				detail := "ws=" + short(in.WorkspaceID)
				if err := files.For(in.Source, "read", owner); err != nil {
					return mcp.Result{Text: err.Error(), IsError: true}, detail + " ticket=refused"
				}
				f, data, err := fc.Get(ctx, in.Source)
				if err != nil {
					a.log.Warn("import", "err", err)
					return transferFailure(err), detail
				}
				name := f.Name
				if name == "" {
					name = path.Base(in.Path)
				}
				if err := files.Check(name, data); err != nil {
					return mcp.Result{Text: err.Error(), IsError: true}, detail + " check=refused"
				}
				sum := sha256.Sum256(data)
				if err := m.PutBytes(ctx, owner, in.WorkspaceID, in.Path, data); err != nil {
					return failure(err), detail
				}
				return mcp.Result{Text: fmt.Sprintf("imported %s (%d bytes) to %s", name, len(data), in.Path),
					Structured: map[string]any{"path": in.Path, "name": name, "size": len(data), "sha256": hex.EncodeToString(sum[:])}}, detail + " sha256=" + hex.EncodeToString(sum[:6])
			})},
		{Name: "sandbox_export_file",
			Description: fmt.Sprintf("Deliver a file from a workspace to the member: they see it on this request and can download it (up to %d MiB; .docx .xlsx .pptx .pdf .csv .txt .md .png .jpg). Set target to output. name is the file name they see (default: the file's own name).", cfg.MaxTransferBytes>>20),
			InputSchema: obj(map[string]any{"workspaceId": idProp(), "path": pathProp, "target": ticketProp("idp-delivery", "output"),
				"name": map[string]any{"type": "string", "minLength": 1, "maxLength": 200, "description": "The file name the member sees, for example 报价单-修订.docx."}},
				"workspaceId", "path", "target"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
			Call: a.wrap("sandbox_export_file", func(ctx context.Context, owner string, args map[string]json.RawMessage) (mcp.Result, string) {
				var in struct {
					WorkspaceID string `json:"workspaceId"`
					Path        string `json:"path"`
					Target      string `json:"target"`
					Name        string `json:"name"`
				}
				if err := decode(args, &in); err != nil {
					return failure(err), ""
				}
				detail := "ws=" + short(in.WorkspaceID)
				if err := files.For(in.Target, "write", owner); err != nil {
					return mcp.Result{Text: err.Error(), IsError: true}, detail + " ticket=refused"
				}
				name := files.SafeName(in.Name)
				if name == "" {
					name = files.SafeName(path.Base(in.Path))
				}
				data, err := m.GetBytes(ctx, owner, in.WorkspaceID, in.Path)
				if err != nil {
					return failure(err), detail
				}
				if err := files.Check(name, data); err != nil {
					return mcp.Result{Text: err.Error(), IsError: true}, detail + " check=refused"
				}
				f, err := fc.Put(ctx, in.Target, name, files.MediaType(name), data)
				if err != nil {
					a.log.Warn("export", "err", err)
					return transferFailure(err), detail
				}
				return mcp.Result{Text: fmt.Sprintf("delivered %s (%d bytes) to the member", f.Name, f.Size),
					Structured: map[string]any{"deliveredFile": f}}, detail + " sha256=" + short(f.SHA256)
			})},
	}
}

func transferFailure(err error) mcp.Result {
	switch {
	case errors.Is(err, files.ErrTicket), errors.Is(err, files.ErrNotYours), errors.Is(err, files.ErrTooLarge):
		return mcp.Result{Text: err.Error(), IsError: true}
	case errors.Is(err, sandbox.ErrFileTooLarge):
		return failure(err)
	}
	return mcp.Result{Text: "the file could not be moved: " + err.Error(), IsError: true}
}
