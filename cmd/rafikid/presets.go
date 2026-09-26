package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// errPresetInvalid marks a putPreset validation failure, so a surface can
// tell it from a store failure.
var errPresetInvalid = errors.New("invalid preset")

// applyPreset resolves req.Preset against the owner's presets and returns
// the request with the preset applied. It runs once, at spawn: resume
// rebuilds from childstore.Session (which holds the RESOLVED fields) and
// never comes back here. A request without a preset is returned unchanged
// with a nil record.
func (c *Controller) applyPreset(ctx context.Context, req protocol.SpawnRequest, ownerUserID string) (protocol.SpawnRequest, *presets.Record, error) {
	if req.Preset == "" {
		return req, nil, nil
	}
	if c.presetStore == nil {
		return req, nil, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "presets unavailable: this daemon has no database",
		}
	}
	rec, err := c.presetStore.Get(ctx, ownerUserID, req.Preset)
	if err != nil {
		if !errors.Is(err, presets.ErrNotFound) {
			return req, nil, &control.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: fmt.Sprintf("preset %q: %s", req.Preset, err),
			}
		}
		// Suggest the presets of the SAME group only — there is no fallback
		// to another group, so listing any other group would suggest a
		// resolution that cannot run.
		group := presets.Group(req.Preset)
		others, listErr := c.presetStore.List(ctx, ownerUserID, group)
		if listErr != nil {
			return req, nil, &control.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: fmt.Sprintf("preset %q: %s", req.Preset, listErr),
			}
		}
		names := make([]string, 0, len(others))
		for _, o := range others {
			names = append(names, o.Name)
		}
		shown := strings.Join(names, ", ")
		if len(names) == 0 {
			shown = "(none)"
		}
		var where string
		if group == "" {
			where = "presets: "
		} else {
			where = fmt.Sprintf("presets in %q: ", group)
		}
		return req, nil, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("no preset %q; %s%s", req.Preset, where, shown),
		}
	}

	// Kind: the preset supplies it, the request may confirm it, and it may
	// not contradict it — later steps branch on the resolved kind.
	switch {
	case req.Kind == "":
		req.Kind = rec.Kind
	case req.Kind != rec.Kind:
		return req, nil, &control.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("kind %q conflicts with preset %q (kind %q)",
				req.Kind, req.Preset, rec.Kind),
		}
	}

	// A claude preset fixes nothing but model/provider/executor/labels/
	// prompts/budgets: every field that only a fundi child can honour is
	// refused, in this order, so a request that sets several reports the
	// first.
	if rec.Kind == presets.KindClaude || rec.Kind == presets.KindScript {
		for _, bad := range []struct {
			field string
			set   bool
		}{
			{"thinking", req.Thinking != ""},
			{"tools", req.Tools != ""},
			{"tools", req.NoBuiltinTools},
			{"skills", len(req.Skills) > 0},
			{"skills", req.NoSkills},
			{"mcp_servers", len(req.MCPServers) > 0},
			{"mcp_servers", req.NoMCP},
			{"context_files", req.NoContextFiles},
			{"system_prompt", req.SystemPrompt != ""},
		} {
			if bad.set {
				return req, nil, &control.ControllerError{
					Code:    protocol.ErrInvalidArgs,
					Message: fmt.Sprintf("field %q does not apply to a %s preset", bad.field, rec.Kind),
				}
			}
		}
	}
	// A script preset additionally fixes no model/provider: a script child has
	// no LLM to point them at. The request fields are refused by
	// validateScriptSpawn after this runs; a preset could still try to fill
	// them, so refuse here too, where the offending row is named.
	if rec.Kind == presets.KindScript && (rec.Model != "" || rec.Provider != "") {
		return req, nil, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("model/provider does not apply to a %s preset", presets.KindScript),
		}
	}

	// Model/provider/thinking: the request wins when non-empty, else the
	// preset's.
	if req.Provider == "" {
		req.Provider = rec.Provider
	}
	if req.Model == "" {
		req.Model = rec.Model
	}
	if req.Thinking == "" {
		req.Thinking = rec.Thinking
	}

	// Executor: fill only a request that names nothing at all. A request
	// selector or ref survives; confinement narrowing runs later, unchanged.
	if req.ExecutorSelector == "" && req.ExecutorRef == "" {
		req.ExecutorSelector = rec.Executor
	}

	// Labels: preset labels first, request keys win. A fresh map — neither
	// rec.Labels nor the caller's map may be mutated.
	req.Labels = mergePresetLabels(rec.Labels, req.Labels)

	// System prompt: fixed by the preset. The request may append, not replace.
	if req.SystemPrompt != "" {
		return req, nil, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("system_prompt is fixed by preset %q", req.Preset),
		}
	}
	req.SystemPrompt = rec.SystemPrompt
	req.AppendSystemPrompt = joinNonEmpty(rec.AppendSystemPrompt, req.AppendSystemPrompt)

	// Budgets: a nil request pointer takes a COPY of the preset's — never the
	// record's pointer, which a later mutation of the request would follow
	// back into the record. A non-nil request value is kept as-is; the grant
	// rules later bound it.
	if req.MaxCost == nil && rec.MaxCost != nil {
		v := *rec.MaxCost
		req.MaxCost = &v
	}
	if req.MaxDepth == nil && rec.MaxDepth != nil {
		v := *rec.MaxDepth
		req.MaxDepth = &v
	}
	if req.MaxChildren == nil && rec.MaxChildren != nil {
		v := *rec.MaxChildren
		req.MaxChildren = &v
	}

	// Allowlists. Each is tri-state on the preset (nil = all, non-nil empty =
	// none, else exactly those) and may only be NARROWED by the request.
	toolList, toolOff, err := narrowAllowlist("tools", req.Preset, rec.Tools, splitComma(req.Tools), req.NoBuiltinTools)
	if err != nil {
		return req, nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	if rec.Tools == nil && len(toolList) > 0 {
		// An open preset leaves the kind's full tool surface available; a
		// request may pick from it, but only by names that exist.
		known := knownToolNames()
		for _, name := range toolList {
			if !known[name] {
				return req, nil, &control.ControllerError{
					Code:    protocol.ErrInvalidArgs,
					Message: fmt.Sprintf("unknown tool %q", name),
				}
			}
		}
	}
	req.Tools = strings.Join(toolList, ",")
	req.NoBuiltinTools = toolOff

	skillList, skillOff, err := narrowAllowlist("skills", req.Preset, rec.Skills, req.Skills, req.NoSkills)
	if err != nil {
		return req, nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	req.Skills = skillList
	req.NoSkills = skillOff

	mcpList, mcpOff, err := narrowAllowlist("mcp_servers", req.Preset, rec.MCPServers, req.MCPServers, req.NoMCP)
	if err != nil {
		return req, nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	req.MCPServers = mcpList
	req.NoMCP = mcpOff

	// Context files: a preset that disables them cannot be re-enabled by a
	// request; a request that disabled them stays disabled.
	req.NoContextFiles = req.NoContextFiles || (rec.ContextFiles != nil && !*rec.ContextFiles)

	// req.Preset stays set: harmless, nothing downstream reads it.
	return req, &rec, nil
}

// mergePresetLabels returns a fresh map with preset's entries overlaid by
// req's (request keys win). Neither input is mutated.
func mergePresetLabels(preset, req map[string]string) map[string]string {
	out := make(map[string]string, len(preset)+len(req))
	for k, v := range preset {
		out[k] = v
	}
	for k, v := range req {
		out[k] = v
	}
	return out
}

// joinNonEmpty joins a and b with a blank line, preset text first; empty
// parts are skipped so neither side alone yields a leading or trailing
// separator.
func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}

// narrowAllowlist combines a preset's TRI-STATE allowlist (nil = all,
// non-nil empty = none, else exactly those) with a spawn's request
// (reqOff = none; non-empty reqList = exactly those; neither = no request).
// A request may only NARROW: every requested name must be in a non-nil
// preset list. Returns (list, off): off = none; nil list and !off = all.
func narrowAllowlist(field, preset string, presetList, reqList []string, reqOff bool) ([]string, bool, error) {
	if reqOff {
		return nil, true, nil
	}
	if len(reqList) > 0 {
		if presetList == nil {
			// The preset leaves the kind's default open; the request picks.
			return reqList, false, nil
		}
		for _, name := range reqList {
			if !slices.Contains(presetList, name) {
				allows := "none"
				if len(presetList) > 0 {
					allows = strings.Join(presetList, ", ")
				}
				return nil, false, fmt.Errorf("%s %q is not allowed by preset %q (allows: %s)", field, name, preset, allows)
			}
		}
		return reqList, false, nil
	}
	// No request: the preset's list stands.
	if presetList == nil {
		return nil, false, nil
	}
	if len(presetList) == 0 {
		return nil, true, nil
	}
	return presetList, false, nil
}

// stampPresetLabels records which preset (and which row of it) a child
// was born from. A nil rec stamps nothing.
func stampPresetLabels(labels map[string]string, rec *presets.Record) {
	if rec == nil {
		return
	}
	labels["rafiki/preset"] = rec.Name
	labels["rafiki/preset-id"] = strconv.FormatInt(rec.ID, 10)
}

// knownToolNames is the set of built-in tool names (tools.DefaultBlueprint).
func knownToolNames() map[string]bool {
	out := make(map[string]bool)
	for _, t := range tools.DefaultBlueprint.All() {
		out[t.Name()] = true
	}
	return out
}

// putPreset validates and stores spec for ownerUserID, stamping childID as
// written_by_child ("" for the operator). Validation failures wrap
// errPresetInvalid so a surface can tell them from store failures.
func (c *Controller) putPreset(ctx context.Context, ownerUserID, childID string, spec presets.Spec) (presets.Record, error) {
	if c.presetStore == nil {
		return presets.Record{}, errors.New("presets unavailable: this daemon has no database")
	}
	rec := spec.Record()
	if err := presets.Validate(rec); err != nil {
		return presets.Record{}, fmt.Errorf("%w: %w", errPresetInvalid, err)
	}
	if rec.Tools != nil {
		known := knownToolNames()
		for _, name := range rec.Tools {
			if !known[name] {
				return presets.Record{}, fmt.Errorf("%w: unknown tool %q", errPresetInvalid, name)
			}
		}
	}
	rec.WrittenByChild = childID
	return c.presetStore.Put(ctx, ownerUserID, rec)
}

// presetBinding is tools.PresetStore bound to one owner (and, for an
// agent caller, its child id as write attribution). Wave 3 hands it to
// fundi children and to the MCP face.
type presetBinding struct {
	c       *Controller
	owner   string
	childID string
}

func newPresetBinding(c *Controller, ownerUserID, childID string) presetBinding {
	return presetBinding{c: c, owner: ownerUserID, childID: childID}
}

func (b presetBinding) List(ctx context.Context, prefix string) ([]presets.Record, error) {
	if b.c.presetStore == nil {
		return nil, errors.New("presets unavailable: this daemon has no database")
	}
	return b.c.presetStore.List(ctx, b.owner, prefix)
}

func (b presetBinding) Get(ctx context.Context, name string) (presets.Record, error) {
	if b.c.presetStore == nil {
		return presets.Record{}, errors.New("presets unavailable: this daemon has no database")
	}
	return b.c.presetStore.Get(ctx, b.owner, name)
}

func (b presetBinding) History(ctx context.Context, name string) ([]presets.Record, error) {
	if b.c.presetStore == nil {
		return nil, errors.New("presets unavailable: this daemon has no database")
	}
	return b.c.presetStore.History(ctx, b.owner, name)
}

func (b presetBinding) Put(ctx context.Context, spec presets.Spec) (presets.Record, error) {
	return b.c.putPreset(ctx, b.owner, b.childID, spec)
}

func (b presetBinding) Delete(ctx context.Context, name string) error {
	if b.c.presetStore == nil {
		return errors.New("presets unavailable: this daemon has no database")
	}
	return b.c.presetStore.Delete(ctx, b.owner, name)
}

// childPresetBinding is what a per-child MCP caller gets instead of the
// owner's binding: the read verbs pass through — preset names and metadata
// are the same read-only, non-scoped facts Connect's anyCaller grants a child
// credential on ListPresets/GetPreset — but AUTHORING refuses. Review-0's F1:
// the owner-scoped put/delete let a child shadow, latest-live-wins, whatever
// preset the operator's next spawn would resolve. Attribution is not
// authorization, so wave 1 removes the write rather than stamping it more
// honestly.
//
// The refusal is an ERROR, not a declined tool: the tool stays listed so the
// model reads the rule it violated, and every surface wrapping this binding
// sees one sentence naming who may author presets.
type childPresetBinding struct {
	presetBinding
}

var _ tools.PresetStore = childPresetBinding{}

// errPresetChildAuthoring is the child-caller refusal for preset
// put/delete. It names the rule, never the caller's credential value.
var errPresetChildAuthoring = errors.New("presets are operator-authored: a per-child credential may read presets but never put or delete them")

func (b childPresetBinding) Put(ctx context.Context, spec presets.Spec) (presets.Record, error) {
	return presets.Record{}, errPresetChildAuthoring
}

func (b childPresetBinding) Delete(ctx context.Context, name string) error {
	return errPresetChildAuthoring
}
