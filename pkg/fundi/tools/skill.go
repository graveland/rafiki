package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	skillspkg "go.graveland.dev/rafiki/pkg/skills"
)

const (
	skillDescription = "Load a skill's full instructions into the conversation. Skills are " +
		"markdown playbooks for specific tasks; their names and one-line " +
		"descriptions are listed in the system prompt's skills inventory. Call " +
		"this with the skill's name when its description matches what you're " +
		"about to do."
	skillSchema = `{
		"type": "object",
		"properties": {
			"skill": {"type": "string", "description": "The name of the skill to load, as it appears in the skills inventory."}
		},
		"required": ["skill"]
	}`
)

func init() { DefaultBlueprint.Register(&SkillBlueprint{}) }

type SkillBlueprint struct{}

func (SkillBlueprint) Name() string        { return "skill" }
func (SkillBlueprint) Description() string { return skillDescription }
func (SkillBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "skill", Type: "string", Description: "The name of the skill to load, as it appears in the skills inventory."},
		},
		Required: []string{"skill"},
	}
}
func (SkillBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (SkillBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	byName := make(map[string]skillspkg.SkillMeta, len(opts.Skills))
	names := make([]string, 0, len(opts.Skills))
	for _, s := range opts.Skills {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return &skillTool{SkillBlueprint: SkillBlueprint{}, byName: byName, names: names}, nil
}

type skillTool struct {
	SkillBlueprint
	byName map[string]skillspkg.SkillMeta
	names  []string
}

func (st *skillTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in skillInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("skill: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	s, ok := st.byName[in.Skill]
	if !ok {
		return ToolResult{}, fmt.Errorf("skill: unknown skill %q; available skills: %s", in.Skill, strings.Join(st.names, ", "))
	}

	body, err := skillspkg.SkillBody(s.Path)
	if err != nil {
		return ToolResult{}, fmt.Errorf("skill: %w", err)
	}

	return NewTextResult(fmt.Sprintf("Base directory for this skill: %s\n\n%s", s.Dir, body)), nil
}

type skillInput struct {
	Skill string `json:"skill"`
}

