// Package tools implements fundi's native agent tool layer: a Registry that
// satisfies rafiki's agentloop.ToolSet, and the core file-manipulation tools
// (read, write, edit, glob, grep) the model calls through it.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"

	skillspkg "go.graveland.dev/rafiki/pkg/skills"
)

// ToolInput is the raw JSON input a tool receives from the model. It is a
// named type wrapping json.RawMessage so the tool interface is
// self-documenting.
type ToolInput json.RawMessage

// Unmarshal decodes this input into v using encoding/json.
func (i ToolInput) Unmarshal(v any) error {
	return json.Unmarshal(json.RawMessage(i), v)
}

// ToolResult is the text result a tool hands back to the model. A zero-value
// ToolResult has Text == ""; IsEmpty reports true.
type ToolResult struct {
	Text string
}

// IsEmpty reports whether this result carries no text.
func (r ToolResult) IsEmpty() bool { return r.Text == "" }

// NewTextResult builds a ToolResult from a plain string.
func NewTextResult(s string) ToolResult { return ToolResult{Text: s} }

// NewErrorResult builds a ToolResult from an error value's message.
func NewErrorResult(err error) ToolResult {
	if err == nil {
		return ToolResult{}
	}
	return ToolResult{Text: err.Error()}
}

// Tool is the interface every agent tool implements. A tool value that
// implements the full interface can be registered on a Registry.
type Tool interface {
	Name() string
	Description() string
	InputSchema() Schema
	Execute(ctx context.Context, input ToolInput) (ToolResult, error)
}

// ToolOpts carries the per-agent runtime state a Materializer needs to
// build a concrete Tool from a blueprint that carries only its static
// metadata (name, description, input schema).
type ToolOpts struct {
	Cwd          string
	FileTracker  *FileTracker
	OutputPolicy OutputPolicy
	Skills       []skillspkg.SkillMeta
}

// Materializer is an optional extension of Tool that a blueprint implements
// when its concrete Execute needs runtime state. Blueprints without runtime
// state (glob, grep) are their own concrete tools and do not implement
// Materializer.
//
// Returning (nil, nil) declines the tool for these opts: MaterializeAll leaves
// it out of the Registry entirely. Use that instead of returning a tool that
// can only fail when opts lack what it needs.
type Materializer interface {
	Materialize(opts ToolOpts) (Tool, error)
}

// Registry is a name -> (definition, executor) map implementing rafiki's
// agentloop.ToolSet. The loop runs a tool batch concurrently (ergroup,
// concurrency limit 6), so every Registry method is safe to call from
// multiple goroutines at once.
type Registry struct {
	mu   sync.RWMutex
	defs map[string]anthropic.ToolUnionParam
	fns  map[string]func(context.Context, json.RawMessage) (string, error)
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		defs: make(map[string]anthropic.ToolUnionParam),
		fns:  make(map[string]func(context.Context, json.RawMessage) (string, error)),
	}
}

// Register adds a Tool to the registry. The tool's Anthropic definition is
// built from its Name(), Description(), and InputSchema().JSON().
func (r *Registry) Register(t Tool) {
	def := BuildDef(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[def.OfTool.Name] = def
	r.fns[def.OfTool.Name] = func(ctx context.Context, input json.RawMessage) (string, error) {
		result, err := t.Execute(ctx, ToolInput(input))
		if err != nil {
			return "", err
		}
		return result.Text, nil
	}
}

// Definitions returns every registered tool's definition, sorted by name.
// The sort is load-bearing, not cosmetic: Definitions backs the tools array
// sent on every turn, and a reordered array silently busts the Anthropic
// prompt-cache prefix (see rafiki's agentloop.ToolSet).
func (r *Registry) Definitions() []anthropic.ToolUnionParam {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.defs))
	for name := range r.defs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]anthropic.ToolUnionParam, 0, len(names))
	for _, name := range names {
		out = append(out, r.defs[name])
	}
	return out
}

// Execute runs the named tool. An unknown name is a returned error, not a
// panic — agentloop converts it to an is_error tool result the model can see.
//
// A panic inside a tool's Execute is recovered here and converted into that
// same returned error. This is the containment boundary for the ENTIRE tool
// surface — the file tools, bash, skill, and every MCP tool — and it is the
// only place that containment can live: agentloop runs each tool call on its
// own errgroup goroutine (one g.Go per tool_use block) and errgroup
// deliberately does not recover. Without this, a panic in any tool body
// unwinds a goroutine nothing owns and kills the whole daemon, taking every
// unrelated conversation with it.
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (result string, err error) {
	r.mu.RLock()
	fn, ok := r.fns[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	defer func() {
		if v := recover(); v != nil {
			slog.Error("tools: tool panicked; reporting a failed tool result to the model",
				"tool", name, "panic", v, "stack", string(debug.Stack()))
			result = ""
			err = fmt.Errorf("tool %q panicked: %v", name, v)
		}
	}()
	return fn(ctx, input)
}
