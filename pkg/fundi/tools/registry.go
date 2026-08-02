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
)

// ToolFunc executes one tool call given its raw JSON input and returns the
// text result the model sees. Returning a non-nil error is normal control
// flow, not an exception path: rafiki's agentloop converts it into an
// is_error tool result the model can react to, so a descriptive error for
// bad input (missing file, stale read, bad regex, ...) is correct behavior.
type ToolFunc func(ctx context.Context, input json.RawMessage) (string, error)

// Registry is a name -> (definition, executor) map implementing rafiki's
// agentloop.ToolSet. The loop runs a tool batch concurrently (errgroup,
// concurrency limit 6), so every Registry method is safe to call from
// multiple goroutines at once; individual ToolFunc implementations are
// responsible for their own concurrency safety over any state they share
// (see FileTracker).
type Registry struct {
	mu   sync.RWMutex
	defs map[string]anthropic.ToolUnionParam
	fns  map[string]ToolFunc
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		defs: make(map[string]anthropic.ToolUnionParam),
		fns:  make(map[string]ToolFunc),
	}
}

// Register adds one tool under the name carried by def. def must be the
// "custom" tool variant built by Def — Register panics on any other variant,
// since that indicates a programming error in the caller (a hand-built
// ToolUnionParam using some other SDK-provided tool type) rather than a
// runtime condition.
func (r *Registry) Register(def anthropic.ToolUnionParam, fn ToolFunc) {
	if def.OfTool == nil {
		panic("tools: Register requires a custom tool definition built by Def")
	}
	name := def.OfTool.Name
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[name] = def
	r.fns[name] = fn
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

// Execute runs the named tool's ToolFunc. An unknown name is a returned
// error, not a panic — agentloop converts it to an is_error tool result the
// model can see.
//
// A panic inside a ToolFunc is recovered here and converted into that same
// returned error. This is the containment boundary for the ENTIRE tool
// surface — the file tools, bash, skill, and every MCP tool, since all of
// them are registered as ToolFuncs on this Registry — and it is the only
// place that containment can live: agentloop runs each tool call on its own
// errgroup goroutine (one g.Go per tool_use block) and errgroup deliberately
// does not recover. Without this, a panic in any tool body unwinds a
// goroutine nothing owns and kills the whole daemon, taking every unrelated
// conversation with it.
//
// Converting the panic to an error rather than re-raising it is the right
// shape, not a convenience: agentloop turns a tool error into an is_error
// tool_result the model can see and react to, so the turn survives and the
// model is told what happened. No partial result is preserved — a tool that
// panicked mid-write has no output worth showing.
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

// Def builds a custom-tool definition from a name, a model-facing
// description, and a JSON Schema (draft 2020-12) object describing its
// input. jsonSchema is always a literal baked in by the caller (see the
// per-tool *Schema consts in read.go, write.go, etc.) — a malformed literal
// is a programmer error caught immediately by any test that calls
// RegisterFileTools, so Def panics rather than threading a startup error
// through every registration call site.
func Def(name, description, jsonSchema string) anthropic.ToolUnionParam {
	var schema anthropic.ToolInputSchemaParam
	if err := json.Unmarshal([]byte(jsonSchema), &schema); err != nil {
		panic(fmt.Sprintf("tools: invalid json schema for %q: %v", name, err))
	}
	def := anthropic.ToolUnionParamOfTool(schema, name)
	def.OfTool.Description = anthropic.String(description)
	return def
}

// RegisterFileTools registers read, write, edit, glob, and grep against r,
// sharing tr as their read-before-write tracking state.
func RegisterFileTools(r *Registry, tr *FileTracker) {
	r.Register(Def("read", readDescription, readSchema), newReadTool(tr))
	r.Register(Def("write", writeDescription, writeSchema), newWriteTool(tr))
	r.Register(Def("edit", editDescription, editSchema), newEditTool(tr))
	r.Register(Def("glob", globDescription, globSchema), newGlobTool())
	r.Register(Def("grep", grepDescription, grepSchema), newGrepTool())
}
