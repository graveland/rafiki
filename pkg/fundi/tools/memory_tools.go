// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.graveland.dev/rafiki/pkg/recall"
)

const memoryPutDescription = "Save or replace a memory at path/name. Organize by " +
	"category with the path so a whole category can be fetched with memory_tree. " +
	"Replacing keeps one live version."

const memoryGetDescription = "Fetch one memory you saved: its full body, metadata " +
	"and timestamps. Errors if no memory lives at that path and name."

const memoryTreeDescription = "Fetch every memory under a path, full bodies. If the " +
	"subtree is too large, returns paths and first lines and asks you to narrow the path."

const memoryDeleteDescription = "Remove a memory from your recall results: it stops " +
	"appearing in recall, memory_get and memory_tree. The memory is tombstoned, " +
	"not destroyed."

func init() {
	DefaultBlueprint.Register(&MemoryPutBlueprint{})
	DefaultBlueprint.Register(&MemoryGetBlueprint{})
	DefaultBlueprint.Register(&MemoryTreeBlueprint{})
	DefaultBlueprint.Register(&MemoryDeleteBlueprint{})
}

// memoryNotFoundError reports a missing memory with the tool-facing sentence
// while wrapping recall.ErrNotFound, so a caller can errors.Is against the
// domain sentinel without the sentinel's text leaking into the message.
type memoryNotFoundError struct {
	tool string
	path string
	name string
}

func (e *memoryNotFoundError) Error() string {
	return fmt.Sprintf("%s: no memory %s/%s", e.tool, e.path, e.name)
}
func (e *memoryNotFoundError) Unwrap() error { return recall.ErrNotFound }

// memoryJSON is one memory as memory_get returns it. meta is always present
// JSON — the store normalizes an absent meta to "{}" (omitempty only guards
// a zero value no store path produces); timestamps are RFC3339 UTC, as
// preset_get renders its own.
type memoryJSON struct {
	ID        string          `json:"id"`
	Path      string          `json:"path"`
	Name      string          `json:"name"`
	Body      string          `json:"body"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func memoryJSONOf(m recall.Memory) memoryJSON {
	return memoryJSON{
		ID:        m.ID,
		Path:      m.Path,
		Name:      m.Name,
		Body:      m.Body,
		Meta:      m.Meta,
		CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: m.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

type MemoryPutBlueprint struct{}

func (MemoryPutBlueprint) Name() string        { return "memory_put" }
func (MemoryPutBlueprint) Description() string { return memoryPutDescription }
func (MemoryPutBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string",
				Description: "Dot-separated labels of letters, digits, _ and -, e.g. projects.rafiki.gotchas."},
			{Name: "name", Type: "string",
				Description: "Name of the memory within the path, e.g. dial-timeout."},
			{Name: "body", Type: "string",
				Description: "The memory's text."},
			{Name: "meta", Type: "object",
				Description: "Optional JSON object with arbitrary metadata for the memory."},
		},
		Required: []string{"path", "name", "body"},
	}
}
func (MemoryPutBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (MemoryPutBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Recall == nil {
		return nil, nil
	}
	return &memoryPutTool{MemoryPutBlueprint: MemoryPutBlueprint{}, recall: opts.Recall}, nil
}

type memoryPutTool struct {
	MemoryPutBlueprint
	recall RecallBinding
}

type memoryPutInput struct {
	Path string          `json:"path"`
	Name string          `json:"name"`
	Body string          `json:"body"`
	Meta json.RawMessage `json:"meta"`
}

func (t *memoryPutTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in memoryPutInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("memory_put: invalid input: %w", err)
	}
	if in.Path == "" {
		return ToolResult{}, errors.New("memory_put: path is required")
	}
	if in.Name == "" {
		return ToolResult{}, errors.New("memory_put: name is required")
	}
	if in.Body == "" {
		return ToolResult{}, errors.New("memory_put: body is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if _, err := t.recall.MemoryPut(ctx, in.Path, in.Name, in.Body, in.Meta); err != nil {
		return ToolResult{}, fmt.Errorf("memory_put: %w", err)
	}
	return NewTextResult(fmt.Sprintf("saved memory %s/%s", in.Path, in.Name)), nil
}

type MemoryGetBlueprint struct{}

func (MemoryGetBlueprint) Name() string        { return "memory_get" }
func (MemoryGetBlueprint) Description() string { return memoryGetDescription }
func (MemoryGetBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "The memory's path, as given to memory_put."},
			{Name: "name", Type: "string", Description: "The memory's name, as given to memory_put."},
		},
		Required: []string{"path", "name"},
	}
}
func (MemoryGetBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (MemoryGetBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Recall == nil {
		return nil, nil
	}
	return &memoryGetTool{MemoryGetBlueprint: MemoryGetBlueprint{}, recall: opts.Recall}, nil
}

type memoryGetTool struct {
	MemoryGetBlueprint
	recall RecallBinding
}

type memoryGetInput struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (t *memoryGetTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in memoryGetInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("memory_get: invalid input: %w", err)
	}
	if in.Path == "" {
		return ToolResult{}, errors.New("memory_get: path is required")
	}
	if in.Name == "" {
		return ToolResult{}, errors.New("memory_get: name is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	m, err := t.recall.MemoryGet(ctx, in.Path, in.Name)
	if err != nil {
		return memoryToolStoreError("memory_get", in.Path, in.Name, err)
	}
	b, err := json.MarshalIndent(memoryJSONOf(m), "", "  ")
	if err != nil {
		return ToolResult{}, fmt.Errorf("memory_get: %w", err)
	}
	return NewTextResult(string(b)), nil
}

type MemoryTreeBlueprint struct{}

func (MemoryTreeBlueprint) Name() string        { return "memory_tree" }
func (MemoryTreeBlueprint) Description() string { return memoryTreeDescription }
func (MemoryTreeBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string",
				Description: "Path prefix whose whole subtree to fetch, e.g. projects.rafiki."},
			{Name: "depth", Type: "integer",
				Description: "How deep under the path to fetch; 0 = unlimited."},
		},
		Required: []string{"path"},
	}
}
func (MemoryTreeBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (MemoryTreeBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Recall == nil {
		return nil, nil
	}
	return &memoryTreeTool{MemoryTreeBlueprint: MemoryTreeBlueprint{}, recall: opts.Recall}, nil
}

type memoryTreeTool struct {
	MemoryTreeBlueprint
	recall RecallBinding
}

type memoryTreeInput struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`
}

func (t *memoryTreeTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in memoryTreeInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("memory_tree: invalid input: %w", err)
	}
	if in.Path == "" {
		return ToolResult{}, errors.New("memory_tree: path is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	text, err := t.recall.MemoryTree(ctx, in.Path, in.Depth)
	if err != nil {
		return memoryToolStoreError("memory_tree", in.Path, "", err)
	}
	return NewTextResult(text), nil
}

type MemoryDeleteBlueprint struct{}

func (MemoryDeleteBlueprint) Name() string        { return "memory_delete" }
func (MemoryDeleteBlueprint) Description() string { return memoryDeleteDescription }
func (MemoryDeleteBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "The memory's path."},
			{Name: "name", Type: "string", Description: "The memory's name."},
		},
		Required: []string{"path", "name"},
	}
}
func (MemoryDeleteBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (MemoryDeleteBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Recall == nil {
		return nil, nil
	}
	return &memoryDeleteTool{MemoryDeleteBlueprint: MemoryDeleteBlueprint{}, recall: opts.Recall}, nil
}

type memoryDeleteTool struct {
	MemoryDeleteBlueprint
	recall RecallBinding
}

type memoryDeleteInput struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (t *memoryDeleteTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in memoryDeleteInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("memory_delete: invalid input: %w", err)
	}
	if in.Path == "" {
		return ToolResult{}, errors.New("memory_delete: path is required")
	}
	if in.Name == "" {
		return ToolResult{}, errors.New("memory_delete: name is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if err := t.recall.MemoryDelete(ctx, in.Path, in.Name); err != nil {
		return memoryToolStoreError("memory_delete", in.Path, in.Name, err)
	}
	return NewTextResult(fmt.Sprintf("deleted memory %s/%s", in.Path, in.Name)), nil
}

// memoryToolStoreError wraps a memory store error for the model: a missing
// memory reads as the tool's own sentence while keeping the recall.ErrNotFound
// sentinel errors.Is-able; every other error — recall.ErrInvalidPath among
// them, whose message names the offending label — passes through under the
// tool's prefix.
func memoryToolStoreError(tool, path, name string, err error) (ToolResult, error) {
	if errors.Is(err, recall.ErrNotFound) {
		return ToolResult{}, &memoryNotFoundError{tool: tool, path: path, name: name}
	}
	return ToolResult{}, fmt.Errorf("%s: %w", tool, err)
}
