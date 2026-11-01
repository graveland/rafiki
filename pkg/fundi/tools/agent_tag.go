package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func init() {
	DefaultBlueprint.Register(&AgentTagBlueprint{})
	DefaultBlueprint.Register(&AgentUntagBlueprint{})
	DefaultBlueprint.Register(&AgentTagsBlueprint{})
}

const agentTagDescription = "Tag a point in an agent's conversation: a tag is a named handle others in its " +
	"scope can fork from with agent_spawn from_tag, so a worker's finding can be picked up by " +
	"anyone entitled to it without you pasting a transcript.\n\n" +
	"`child_id` names the agent whose history the tag marks (default: you, when you are an agent; a " +
	"user credential must name one) and `cursor` a node in it (\"m:<ordinal>\", the cursor agent_view " +
	"prints; default: that agent's last completed turn). " +
	"`scope` decides who can fork from it and who can see it: \"lineage\" (the default) is the " +
	"tagged agent's lineage — itself, its descendants and its ancestors; \"tree\" is the whole " +
	"agent tree it belongs to, so cousins may fork from it; \"owner\" is every child of its owner. " +
	"A name already live in that scope is refused unless you pass `replace`, which takes the name " +
	"over and which only a user credential may do.\n\n" +
	"To MOVE a tag, pass `tag_id` and `expect_node_id` — the node id agent_tags prints as " +
	"`node <id>`, which is what the tag points at — plus the new `cursor`. A stale expect_node_id " +
	"is refused rather than silently repointing the tag. Only the tag's creator, an ancestor of the " +
	"creator, or a user credential may move or untag it.\n\n" +
	"A tag is not retroactive: an agent that already forked from it keeps the node it was given, " +
	"so moving a tag only changes what the NEXT fork starts from."

const agentUntagDescription = "Delete a tag. It stops appearing in agent_tags and can no longer be forked " +
	"from; agents already forked from it keep the node they were given. Only the tag's creator, an " +
	"ancestor of the creator, or a user credential may untag it. Pass `tag_id`, which agent_tags prints."

const agentTagsDescription = "List tags: the ones you can fork from, nearest scope first — your own lineage " +
	"before your tree before your owner's, and a tag rooted at one of your descendants last — each with " +
	"its name, scope, tag_id, the agent whose history it marks, the cursor it points at (m:<ordinal>, " +
	"which agent_view also prints), the node id a move's expect_node_id takes, and its note. Pass " +
	"`tag_id` for one tag's whole history instead, oldest first, which is how you see where it has been " +
	"moved. Narrow with `name_prefix`, and cap the rows with `limit`."

// --- agent_tag ---

type AgentTagBlueprint struct{}

func (AgentTagBlueprint) Name() string        { return "agent_tag" }
func (AgentTagBlueprint) Description() string { return agentTagDescription }
func (AgentTagBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "name", Type: "string",
				Description: "The tag's name: 1-64 of [a-z0-9._-], starting with a letter or digit. Required to create; a move takes the name from the tag."},
			{Name: "scope", Type: "string", Enum: []string{"lineage", "tree", "owner"},
				Description: "\"lineage\" (default) limits the tag to the tagged agent's lineage, \"tree\" to its whole agent tree, \"owner\" to every child of its owner."},
			{Name: "child_id", Type: "string",
				Description: "The agent whose conversation the tag marks. Defaults to you when you are an agent; a user credential must name one. A move that names no child keeps the tag in the conversation it already points into."},
			{Name: "cursor", Type: "string",
				Description: "The node to mark, as agent_view prints it (m:<ordinal>). Omit for that agent's last completed turn. On a move an omitted cursor resolves to that agent's last completed turn as well — it never means \"leave the tag where it is\"; pass the node you want."},
			{Name: "note", Type: "string",
				Description: "Optional note shown next to the tag in agent_tags, for whoever picks it up (what is at this point, and why fork from it)."},
			{Name: "tag_id", Type: "string",
				Description: "Set with expect_node_id to MOVE an existing tag instead of creating one."},
			{Name: "expect_node_id", Type: "string",
				Description: "On a move, the node id agent_tags prints (`node <id>`) — the node you believe the tag points at. A stale value refuses the move."},
			{Name: "replace", Type: "boolean",
				Description: "Take over a live name in the same scope. Only a user credential may."},
		},
	}
}

func (AgentTagBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentTagBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentTagTool{agents: opts.Agents}, nil
}

type agentTagTool struct {
	AgentTagBlueprint
	agents AgentSpawner
}

func (t *agentTagTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Name         string `json:"name,omitempty"`
		Scope        string `json:"scope,omitempty"`
		ChildID      string `json:"child_id,omitempty"`
		Cursor       string `json:"cursor,omitempty"`
		Note         string `json:"note,omitempty"`
		TagID        string `json:"tag_id,omitempty"`
		ExpectNodeID string `json:"expect_node_id,omitempty"`
		Replace      bool   `json:"replace,omitempty"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_tag: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.TagID == "" && params.Name == "" {
		return ToolResult{}, errors.New("agent_tag: name is required to create a tag — or pass tag_id and expect_node_id to move one")
	}
	info, err := t.agents.Tag(ctx, TagSpec{
		Name:         params.Name,
		Scope:        params.Scope,
		ChildID:      params.ChildID,
		Cursor:       params.Cursor,
		Note:         params.Note,
		TagID:        params.TagID,
		ExpectNodeID: params.ExpectNodeID,
		Replace:      params.Replace,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_tag: %w", err)
	}
	return NewTextResult(RenderTags([]TagInfo{info})), nil
}

// --- agent_untag ---

type AgentUntagBlueprint struct{}

func (AgentUntagBlueprint) Name() string        { return "agent_untag" }
func (AgentUntagBlueprint) Description() string { return agentUntagDescription }
func (AgentUntagBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "tag_id", Type: "string",
				Description: "The tag to delete, as agent_tags prints it."},
		},
		Required: []string{"tag_id"},
	}
}

func (AgentUntagBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentUntagBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentUntagTool{agents: opts.Agents}, nil
}

type agentUntagTool struct {
	AgentUntagBlueprint
	agents AgentSpawner
}

func (t *agentUntagTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		TagID string `json:"tag_id"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_untag: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.TagID == "" {
		return ToolResult{}, errors.New("agent_untag: tag_id is required")
	}
	if err := t.agents.Untag(ctx, params.TagID); err != nil {
		return ToolResult{}, fmt.Errorf("agent_untag: %w", err)
	}
	return NewTextResult(fmt.Sprintf("untagged %s\n", params.TagID)), nil
}

// --- agent_tags ---

type AgentTagsBlueprint struct{}

func (AgentTagsBlueprint) Name() string        { return "agent_tags" }
func (AgentTagsBlueprint) Description() string { return agentTagsDescription }
func (AgentTagsBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "name_prefix", Type: "string",
				Description: "Only tags whose name starts with this."},
			{Name: "limit", Type: "integer",
				Description: "Cap the rows returned. Omit for the daemon's default of 50; the cap is 500."},
			{Name: "tag_id", Type: "string",
				Description: "List this tag's whole history (every version, oldest first) instead of the tags you can fork from."},
		},
	}
}

func (AgentTagsBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentTagsBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentTagsTool{agents: opts.Agents}, nil
}

type agentTagsTool struct {
	AgentTagsBlueprint
	agents AgentSpawner
}

func (t *agentTagsTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		NamePrefix string `json:"name_prefix,omitempty"`
		Limit      int    `json:"limit,omitempty"`
		TagID      string `json:"tag_id,omitempty"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_tags: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	rows, err := t.agents.Tags(ctx, TagQuery{
		NamePrefix: params.NamePrefix,
		Limit:      params.Limit,
		TagID:      params.TagID,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_tags: %w", err)
	}
	return NewTextResult(RenderTags(rows)), nil
}

// RenderTags formats a tag list for the model: one line per tag, carrying the
// name, the scope, the id a later agent_untag or move needs, the agent whose
// history it marks, the cursor to fork at, the node id a move's expect_node_id
// must carry, and the note.
func RenderTags(rows []TagInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d tag(s)\n", len(rows))
	for _, t := range rows {
		fmt.Fprintf(&sb, "%s  %s  %s  %s  m:%d  node %s", orDash(t.Name), orDash(t.Scope), t.TagID, orDash(t.ChildID), t.Ordinal, orDash(t.NodeID))
		if t.Note != "" {
			fmt.Fprintf(&sb, "  %s", t.Note)
		}
		if !t.Current {
			sb.WriteString("  (not current)")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
