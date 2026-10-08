// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func init() {
	DefaultBlueprint.Register(&SandboxSyncBlueprint{})
	DefaultBlueprint.Register(&SandboxSyncRepoBlueprint{})
}

// sandbox_sync and sandbox_sync_repo are the daemon-brokered transfer verbs:
// the same per-child sandbox seam as sandbox_create/list/remove (they
// materialize only when ToolOpts.Sandboxes is non-nil), and the same binding
// rule — the daemon closes over the caller's child id and its owner's NON-admin
// identity, so a tool argument can never name another caller's executor. The
// daemon side (the bound SandboxManager) does every authority check; these
// tools only decode, validate the caller's own input, and render.

// --- sandbox_sync ----------------------------------------------------------

const sandboxSyncDescription = `Copy a file or directory between two executors you can reach (typically your host executor and a sandbox), brokered by the daemon. Paths are absolute and executor-local. overwrite replaces the destination and is only allowed on container executors.`

type SandboxSyncBlueprint struct{}

func (SandboxSyncBlueprint) Name() string        { return "sandbox_sync" }
func (SandboxSyncBlueprint) Description() string { return sandboxSyncDescription }
func (SandboxSyncBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "src_executor", Type: "string",
				Description: "Executor to copy FROM, by machine label or executor id. Use sandbox_list for the sandboxes you can reach."},
			{Name: "src_path", Type: "string",
				Description: "Absolute, executor-local path to copy from: a single file or a directory."},
			{Name: "dst_executor", Type: "string",
				Description: "Executor to copy TO, by machine label or executor id."},
			{Name: "dst_path", Type: "string",
				Description: "Absolute, executor-local path to write to."},
			{Name: "overwrite", Type: "boolean",
				Description: "true to replace the destination if it already exists. Only allowed when the destination executor's row says isolation=container."},
			{Name: "max_bytes", Type: "integer",
				Description: "Refuse the copy if it would move more than this many bytes. Omit for no caller cap; a present value must be greater than zero."},
		},
		Required: []string{"src_executor", "src_path", "dst_executor", "dst_path"},
	}
}

func (SandboxSyncBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (SandboxSyncBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Sandboxes == nil {
		return nil, nil
	}
	return &sandboxSyncTool{sandboxes: opts.Sandboxes}, nil
}

type sandboxSyncTool struct {
	SandboxSyncBlueprint
	sandboxes SandboxManager
}

func (t *sandboxSyncTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var params struct {
		SrcExecutor string `json:"src_executor"`
		SrcPath     string `json:"src_path"`
		DstExecutor string `json:"dst_executor"`
		DstPath     string `json:"dst_path"`
		Overwrite   bool   `json:"overwrite,omitempty"`
		MaxBytes    *int64 `json:"max_bytes,omitempty"`
	}
	if err := in.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_sync: invalid input: %w", err)
	}
	if err := requireSandboxSyncFields("sandbox_sync", []syncField{
		{"src_executor", params.SrcExecutor},
		{"src_path", params.SrcPath},
		{"dst_executor", params.DstExecutor},
		{"dst_path", params.DstPath},
	}); err != nil {
		return ToolResult{}, err
	}
	// max_bytes is optional on the wire: absent is nil (no caller cap) and a
	// present value must be > 0 — zero meaning "unlimited" is a zero-value trap,
	// so it is refused here before the daemon is asked.
	if params.MaxBytes != nil && *params.MaxBytes <= 0 {
		return ToolResult{}, errors.New("sandbox_sync: max_bytes must be greater than zero")
	}

	res, err := t.sandboxes.Sync(ctx, protocol.SyncPathRequest{
		Src:       protocol.SyncEndpoint{Executor: params.SrcExecutor, Path: params.SrcPath},
		Dst:       protocol.SyncEndpoint{Executor: params.DstExecutor, Path: params.DstPath},
		Overwrite: params.Overwrite,
		MaxBytes:  params.MaxBytes,
	})
	if err != nil {
		// Return the ERROR, never its text as a successful result.
		return ToolResult{}, fmt.Errorf("sandbox_sync: %w", err)
	}
	return NewTextResult(fmt.Sprintf("copied %d files, %d bytes", res.Files, res.Bytes)), nil
}

// --- sandbox_sync_repo -----------------------------------------------------

const sandboxSyncRepoDescription = `Fetch one git branch from a repository on one executor into a repository on another (git bundle underneath). Only committed state travels. Fast-forward only unless force; a branch that is checked out at the destination is refused.`

type SandboxSyncRepoBlueprint struct{}

func (SandboxSyncRepoBlueprint) Name() string        { return "sandbox_sync_repo" }
func (SandboxSyncRepoBlueprint) Description() string { return sandboxSyncRepoDescription }
func (SandboxSyncRepoBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "src_executor", Type: "string",
				Description: "Executor holding the repository to fetch FROM, by machine label or executor id."},
			{Name: "src_repo", Type: "string",
				Description: "Absolute, executor-local path of the source repository directory."},
			{Name: "dst_executor", Type: "string",
				Description: "Executor holding the repository to fetch INTO, by machine label or executor id."},
			{Name: "dst_repo", Type: "string",
				Description: "Absolute, executor-local path of the destination repository directory."},
			{Name: "branch", Type: "string",
				Description: "Branch to fetch, e.g. \"main\". A branch that is checked out at the destination is refused."},
			{Name: "force", Type: "boolean",
				Description: "true to allow a non-fast-forward update at the destination. A branch that is checked out there is refused regardless."},
		},
		Required: []string{"src_executor", "src_repo", "dst_executor", "dst_repo", "branch"},
	}
}

func (SandboxSyncRepoBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (SandboxSyncRepoBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Sandboxes == nil {
		return nil, nil
	}
	return &sandboxSyncRepoTool{sandboxes: opts.Sandboxes}, nil
}

type sandboxSyncRepoTool struct {
	SandboxSyncRepoBlueprint
	sandboxes SandboxManager
}

func (t *sandboxSyncRepoTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var params struct {
		SrcExecutor string `json:"src_executor"`
		SrcRepo     string `json:"src_repo"`
		DstExecutor string `json:"dst_executor"`
		DstRepo     string `json:"dst_repo"`
		Branch      string `json:"branch"`
		Force       bool   `json:"force,omitempty"`
	}
	if err := in.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_sync_repo: invalid input: %w", err)
	}
	if err := requireSandboxSyncFields("sandbox_sync_repo", []syncField{
		{"src_executor", params.SrcExecutor},
		{"src_repo", params.SrcRepo},
		{"dst_executor", params.DstExecutor},
		{"dst_repo", params.DstRepo},
		{"branch", params.Branch},
	}); err != nil {
		return ToolResult{}, err
	}

	res, err := t.sandboxes.SyncRepo(ctx, protocol.SyncRepoRequest{
		Src:    protocol.SyncEndpoint{Executor: params.SrcExecutor, Path: params.SrcRepo},
		Dst:    protocol.SyncEndpoint{Executor: params.DstExecutor, Path: params.DstRepo},
		Branch: params.Branch,
		Force:  params.Force,
	})
	if err != nil {
		// Return the ERROR, never its text as a successful result.
		return ToolResult{}, fmt.Errorf("sandbox_sync_repo: %w", err)
	}
	return NewTextResult(renderSyncRepoResult(res)), nil
}

// renderSyncRepoResult is the repo verb's success text: a created repository, an
// unchanged branch, or the abbreviated before..after object ids. The daemon's
// result carries full oids; only the first 12 hex digits are shown, as git
// itself abbreviates.
func renderSyncRepoResult(res protocol.SyncRepoResult) string {
	switch {
	case res.CreatedRepo:
		return "created repo"
	case res.UpToDate:
		return "up to date"
	default:
		return shortOID(res.OldOID) + ".." + shortOID(res.NewOID)
	}
}

// shortOID abbreviates a full object id to 12 characters, or returns it whole
// when it is already shorter (an empty destination tip, say).
func shortOID(oid string) string {
	const n = 12
	if len(oid) > n {
		return oid[:n]
	}
	return oid
}

// syncField is one named required argument, in the order the error should name
// them.
type syncField struct {
	name  string
	value string
}

// requireSandboxSyncFields refuses an omitted required argument. The schema
// marks them required too; a tool that trusted only the schema would hand the
// daemon an empty executor or path, so it is checked here as well.
func requireSandboxSyncFields(tool string, fields []syncField) error {
	for _, f := range fields {
		if f.value == "" {
			return fmt.Errorf("%s: %s is required", tool, f.name)
		}
	}
	return nil
}
