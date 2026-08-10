package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
)

// BlueprintRegistry holds every static Tool blueprint registered at init()
// time. It is the discovery layer: tools self-register here and are later
// materialized into per-agent Registries.
type BlueprintRegistry struct {
	mu   sync.RWMutex
	defs []Tool
}

// DefaultBlueprint is populated by init() in each tool file.
var DefaultBlueprint = &BlueprintRegistry{}

// Register adds a blueprint to the registry. It panics if a tool with the
// same Name() is already registered — duplicate names are a programmer error.
func (br *BlueprintRegistry) Register(t Tool) {
	br.mu.Lock()
	defer br.mu.Unlock()
	for _, existing := range br.defs {
		if existing.Name() == t.Name() {
			panic(fmt.Sprintf("tools: blueprint %q already registered", t.Name()))
		}
	}
	br.defs = append(br.defs, t)
}

// All returns a snapshot of every registered blueprint, in registration order.
func (br *BlueprintRegistry) All() []Tool {
	br.mu.RLock()
	defer br.mu.RUnlock()
	out := make([]Tool, len(br.defs))
	copy(out, br.defs)
	return out
}

// BuildDef converts a Tool to the Anthropic SDK tool definition. It panics if
// the tool's InputSchema().JSON() produces invalid JSON — a programmer error
// caught at registration time.
func BuildDef(t Tool) anthropic.ToolUnionParam {
	schemaJSON := t.InputSchema().JSON()
	var schema anthropic.ToolInputSchemaParam
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		panic(fmt.Sprintf("tools: BuildDef: invalid schema for %q: %v", t.Name(), err))
	}
	def := anthropic.ToolUnionParamOfTool(schema, t.Name())
	def.OfTool.Description = anthropic.String(t.Description())
	return def
}

// MaterializeAll iterates every registered blueprint and builds a per-agent
// Registry. Blueprints that implement Materializer are materialized with opts;
// blueprints that don't are registered directly as their own concrete tools.
//
// A Materializer may decline to produce a tool for a given opts by returning a
// nil Tool and a nil error; the blueprint is then simply absent from the
// returned Registry. That is how a tool whose whole purpose depends on data
// opts does not carry (SkillBlueprint with zero skills) keeps itself out of
// the model's tools[] instead of advertising an operation that can only fail.
func (br *BlueprintRegistry) MaterializeAll(opts ToolOpts) *Registry {
	r := NewRegistry()
	routeSet := routedSet()
	for _, bp := range br.All() {
		var t Tool
		if m, ok := bp.(Materializer); ok {
			var err error
			t, err = m.Materialize(opts)
			if err != nil {
				panic(fmt.Sprintf("tools: MaterializeAll: %q: %v", bp.Name(), err))
			}
			if t == nil {
				continue
			}
		} else {
			t = bp
		}

		// When an executor is configured, route machine-local tools to it.
		// The proxy keeps the original name, description, and input schema
		// — the model sees an identical surface — but Execute forwards to
		// the executor process instead of running in-process.
		if opts.Executor != nil && routeSet[t.Name()] {
			t = &executorProxy{tool: t, client: opts.Executor}
		}

		r.Register(t)
	}
	return r
}

// routedSet returns the set of tool names forwarded to an executor.
func routedSet() map[string]bool {
	s := make(map[string]bool, 9)
	for _, name := range RoutedToExecutor() {
		s[name] = true
	}
	return s
}

// executorProxy wraps a Tool so its Execute call is forwarded to an
// ExecutorClient while the model sees the same definition.
type executorProxy struct {
	tool   Tool
	client ExecutorClient
}

func (p *executorProxy) Name() string        { return p.tool.Name() }
func (p *executorProxy) Description() string { return p.tool.Description() }
func (p *executorProxy) InputSchema() Schema { return p.tool.InputSchema() }
func (p *executorProxy) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	result, err := p.client.Execute(ctx, p.tool.Name(), json.RawMessage(input))
	if err != nil {
		return NewErrorResult(err), nil
	}
	return NewTextResult(result), nil
}
