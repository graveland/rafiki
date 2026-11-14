package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ExecutorArgDescription is the schema description of the `executor` argument
// the registry adds to every workspace-tier tool. It is one constant, and one
// property name, for every child: the tool schemas are part of the prompt
// prefix the provider caches, so a per-child or per-tool wording would bust
// that cache for no benefit.
const ExecutorArgDescription = "Which executor runs this call: a machine name or executor id. Optional when you can reach exactly one executor; required when you can reach several (the error lists them). Applies to bash_output and bash_kill too: name the executor that started the job."

// executorArgName is the argument name the wrapper owns on every workspace
// tool. No blueprint may declare it — TestNoWorkspaceToolDeclaresExecutor
// keeps that true, because the wrapper deletes the key before the inner tool
// sees the input.
const executorArgName = "executor"

// ExecutorTarget carries one workspace call's executor choice from the tool
// wrapper down to the ExecutorClient, and the resolution back up. The wrapper
// sets Requested; the client fills the rest. A client that ignores it (every
// test fake, the in-process executor) leaves the outputs zero, and the
// wrapper then changes nothing about the result.
type ExecutorTarget struct {
	Requested string // the call's `executor` argument; "" when omitted
	Resolved  string // display name of the executor that served the call
	Multi     bool   // the agent could reach 2+ executors at resolution time
	Note      string // one-time note for the first call on a fresh binding, already prefixed
}

type executorTargetKey struct{}

// WithExecutorTarget returns a context carrying t, which the tool wrapper
// hands to the inner tool — and through it, to the ExecutorClient.
func WithExecutorTarget(ctx context.Context, t *ExecutorTarget) context.Context {
	return context.WithValue(ctx, executorTargetKey{}, t)
}

// ExecutorTargetFrom returns the ExecutorTarget carried by ctx, or nil when
// the call did not come through the registry wrapper (a daemon-tier tool, the
// executor's own registry, an MCP tool).
func ExecutorTargetFrom(ctx context.Context) *ExecutorTarget {
	if t, ok := ctx.Value(executorTargetKey{}).(*ExecutorTarget); ok {
		return t
	}
	return nil
}

// executorTargeted wraps a workspace tool so it accepts a per-call `executor`
// argument.
//
// The wrap lives here, at the registry layer, rather than in each tool: every
// workspace tool gains the same optional property and the same
// strip-before-execute rule, and the tool bodies — which run on either side of
// the executor boundary — never see the argument at all. The property is
// appended to the inner schema and never to Required, so the schema the model
// sees is identical for every child.
type executorTargeted struct{ tool Tool }

func (p *executorTargeted) Name() string        { return p.tool.Name() }
func (p *executorTargeted) Description() string { return p.tool.Description() }

func (p *executorTargeted) InputSchema() Schema {
	s := p.tool.InputSchema()
	// Copy rather than append in place: the inner tool's slice may share its
	// backing array with the blueprint's static schema.
	props := make([]SchemaProperty, 0, len(s.Properties)+1)
	props = append(props, s.Properties...)
	props = append(props, SchemaProperty{
		Name:        executorArgName,
		Type:        "string",
		Description: ExecutorArgDescription,
	})
	s.Properties = props
	return s
}

func (p *executorTargeted) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	requested, rest, err := splitExecutorArg(input)
	if err != nil {
		return ToolResult{}, err
	}

	t := &ExecutorTarget{Requested: requested}
	res, err := p.tool.Execute(WithExecutorTarget(ctx, t), ToolInput(rest))
	if err != nil {
		// A call that could have gone to several machines must say which one
		// refused it: the model's next move is to name another.
		if t.Multi && t.Resolved != "" {
			return ToolResult{}, fmt.Errorf("[%s] %w", t.Resolved, err)
		}
		return ToolResult{}, err
	}

	prefix := ""
	if t.Note != "" {
		prefix = t.Note + "\n"
	}
	if t.Multi {
		prefix += "[" + t.Resolved + "] "
	}
	if prefix == "" {
		return res, nil
	}
	if len(res.Blocks) > 0 {
		blocks := make([]ContentBlock, 0, len(res.Blocks)+1)
		blocks = append(blocks, TextBlock{Text: prefix})
		blocks = append(blocks, res.Blocks...)
		res.Blocks = blocks
		return res, nil
	}
	res.Text = prefix + res.Text
	return res, nil
}

// splitExecutorArg removes the wrapper-owned `executor` key from a tool call's
// input. It returns the requested executor ("" when omitted) and the input the
// inner tool should see.
//
// Input that is empty, not a JSON object, or carries no `executor` key is
// returned byte-identical: the wrapper is invisible to every call that does
// not use the argument.
func splitExecutorArg(input ToolInput) (requested string, rest json.RawMessage, err error) {
	if len(input) == 0 {
		return "", json.RawMessage(input), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil || obj == nil {
		return "", json.RawMessage(input), nil
	}
	raw, ok := obj[executorArgName]
	if !ok {
		return "", json.RawMessage(input), nil
	}
	if err := json.Unmarshal(raw, &requested); err != nil {
		return "", nil, errors.New("executor must be a string (a machine name or executor id)")
	}
	delete(obj, executorArgName)
	stripped, err := json.Marshal(obj)
	if err != nil {
		return "", nil, err
	}
	return requested, stripped, nil
}
