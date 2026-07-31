package tools

import (
	"context"
	"encoding/json"
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

type skillInput struct {
	Skill string `json:"skill"`
}

// RegisterSkillTool registers the "skill" tool against r, backed by the
// given already-discovered skills (see skillspkg.DiscoverSkills). Invoking it
// with an unknown name is a returned error listing the available names -
// agentloop turns that into an is_error tool result the model can read and
// recover from, so no special-casing is needed here.
func RegisterSkillTool(r *Registry, skills []skillspkg.SkillMeta) {
	byName := make(map[string]skillspkg.SkillMeta, len(skills))
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	sort.Strings(names)

	r.Register(Def("skill", skillDescription, skillSchema), newSkillTool(byName, names))
}

func newSkillTool(byName map[string]skillspkg.SkillMeta, names []string) ToolFunc {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		var in skillInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("skill: invalid input: %w", err)
		}

		s, ok := byName[in.Skill]
		if !ok {
			return "", fmt.Errorf("skill: unknown skill %q; available skills: %s", in.Skill, strings.Join(names, ", "))
		}

		body, err := skillspkg.SkillBody(s.Path)
		if err != nil {
			return "", fmt.Errorf("skill: %w", err)
		}

		return fmt.Sprintf("Base directory for this skill: %s\n\n%s", s.Dir, body), nil
	}
}
