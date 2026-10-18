package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skillspkg "go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
)

// writeSkill creates <dir>/<name>/SKILL.md with the given frontmatter and
// body, mirroring the layout DiscoverSkills expects.
func writeSkill(t *testing.T, dir, name, description, body string) skillspkg.SkillMeta {
	t.Helper()
	c := assert.NewAborting(t)
	skillDir := filepath.Join(dir, name)
	c.NoError(os.MkdirAll(skillDir, 0o755))
	path := filepath.Join(skillDir, "SKILL.md")
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	c.NoError(os.WriteFile(path, []byte(content), 0o644))
	return skillspkg.SkillMeta{Name: name, Description: description, Dir: skillDir, Path: path}
}

// TestSkillToolReturnsBodyAndBaseDir covers the tool's success shape: the
// base-dir line, then the SKILL.md body with frontmatter stripped.
func TestSkillToolReturnsBodyAndBaseDir(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	meta := writeSkill(t, dir, "reviewer", "reviews code", "REVIEWER_BODY_MARKER\nstep one\n")

	r := NewRegistry()
	skillT, _ := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: []skillspkg.SkillMeta{meta}})
	r.Register(skillT)

	outRes, err := r.Execute(context.Background(), "skill", json.RawMessage(`{"skill":"reviewer"}`))
	out := outRes.Text
	c.NoError(err, "unexpected error")

	wantPrefix := "Base directory for this skill: " + meta.Dir + "\n\n"
	if !strings.HasPrefix(out, wantPrefix) {
		t.Fatalf("expected output to start with %q, got %q", wantPrefix, out)
	}
	c.StrContains(out, "REVIEWER_BODY_MARKER", "expected body content in output, got")
	c.NotStrContains(out, "description: reviews code", "expected frontmatter stripped from output, got")
}

// TestSkillToolUnknownNameListsAvailable covers the required-recovery path:
// an unknown skill name is a tool error (is_error result) whose message
// lists the available names so the model can self-correct.
func TestSkillToolUnknownNameListsAvailable(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	m1 := writeSkill(t, dir, "alpha", "alpha desc", "alpha body")
	m2 := writeSkill(t, dir, "beta", "beta desc", "beta body")

	r := NewRegistry()
	skillT, _ := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: []skillspkg.SkillMeta{m1, m2}})
	r.Register(skillT)

	outRes, err := r.Execute(context.Background(), "skill", json.RawMessage(`{"skill":"nonexistent"}`))
	out := outRes.Text
	c.Error(err, "expected an error for an unknown skill, got output %q", out)
	c.False(!strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta"), "expected error to list available skill names, got %v", err)
}

// TestSkillToolRegistersUnderName asserts the tool is registered as "skill"
// and appears in Definitions(). It must materialize with a non-empty skill
// set: Materialize declines (returns a nil Tool) for an empty one, which
// TestSkillBlueprintDeclinesWithoutSkills covers.
func TestSkillToolRegistersUnderName(t *testing.T) {
	r := NewRegistry()
	meta := writeSkill(t, t.TempDir(), "reviewer", "reviews code", "body\n")
	skillT, _ := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: []skillspkg.SkillMeta{meta}})
	r.Register(skillT)

	found := false
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "skill" {
			found = true
		}
	}
	assert.NewAborting(t).True(found, "expected a \"skill\" tool to be registered")
}

// TestSkillBlueprintDeclinesWithoutSkills pins the contract that keeps a
// useless skill tool out of the model's tools[]: with no skills discovered
// (also how --no-skills arrives here), Materialize returns a nil Tool and a
// nil error rather than a tool whose every call answers "unknown skill".
func TestSkillBlueprintDeclinesWithoutSkills(t *testing.T) {
	for _, tc := range []struct {
		name   string
		skills []skillspkg.SkillMeta
	}{
		{"nil", nil},
		{"empty", []skillspkg.SkillMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: tc.skills})
			assert.NewAborting(t).NoError(err, "Materialize returned an error")
			if got != nil {
				t.Fatalf("Materialize returned a tool %q, want nil (declined)", got.Name())
			}
		})
	}
}

// TestSkillToolServesAnInlineBody covers the database-backed arm: a skill
// marked Inline resolves by QUALIFIED name and fetches its body through
// ToolOpts.InlineSkillBody, which is handed the inventory meta itself — its ID
// names the row, so a name shadowed in a higher layer cannot resolve to the
// wrong one.
func TestSkillToolServesAnInlineBody(t *testing.T) {
	c := assert.NewCollecting(t)
	var got skillspkg.SkillMeta
	meta := skillspkg.SkillMeta{ID: "row-1", Namespace: "rafiki", Name: "coordinating", Description: "d", Inline: true}
	tool, err := SkillBlueprint{}.Materialize(ToolOpts{
		Skills: []skillspkg.SkillMeta{meta},
		InlineSkillBody: func(_ context.Context, s skillspkg.SkillMeta) (string, string, error) {
			got = s
			return "the body", "", nil
		},
	})
	c.Require().NoError(err, "materialize")
	res, err := tool.Execute(context.Background(), ToolInput(json.RawMessage(`{"skill":"rafiki:coordinating"}`)))
	c.Require().NoError(err, "execute")
	c.Eq("row-1", got.ID, "fetcher got meta with ID")
	c.Eq("rafiki:coordinating", got.QualifiedName(), "fetcher got meta named")
	c.StrContains(res.Text, "the body", "result did not carry the body: %v", res)
}

// An inline skill with no materialized directory must NOT claim one — there is
// nothing on disk for the model to read.
func TestInlineSkillResultHasNoBaseDirectoryLine(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := SkillBlueprint{}.Materialize(ToolOpts{
		Skills: []skillspkg.SkillMeta{
			{Namespace: "rafiki", Name: "coordinating", Inline: true},
		},
		InlineSkillBody: func(context.Context, skillspkg.SkillMeta) (string, string, error) { return "b", "", nil },
	})
	c.Require().NoError(err, "materialize")
	res, err := tool.Execute(context.Background(), ToolInput(json.RawMessage(`{"skill":"rafiki:coordinating"}`)))
	c.Require().NoError(err, "execute")
	c.NotStrContains(res.Text, "Base directory", "inline result claimed a base directory: %v", res)
}

// TestSkillToolInlineWithDirHasBaseDirectory covers an inline skill whose
// supporting files the daemon materialized onto this agent's machine: the
// result names the directory exactly as the executor-backed arm does, so the
// model reads files the same way whichever tier the skill came from.
func TestSkillToolInlineWithDirHasBaseDirectory(t *testing.T) {
	c := assert.NewCollecting(t)
	const dir = "/cache/skill-cache/abc123"
	tool, err := SkillBlueprint{}.Materialize(ToolOpts{
		Skills: []skillspkg.SkillMeta{
			{Namespace: "rafiki", Name: "coordinating", Inline: true, HasFiles: true},
		},
		InlineSkillBody: func(context.Context, skillspkg.SkillMeta) (string, string, error) {
			return "FILE_BODY_MARKER", dir, nil
		},
	})
	c.Require().NoError(err, "materialize")
	res, err := tool.Execute(context.Background(), ToolInput(json.RawMessage(`{"skill":"rafiki:coordinating"}`)))
	c.Require().NoError(err, "execute")
	c.Eq("Base directory for this skill: "+dir+"\n\nFILE_BODY_MARKER", res.Text, "inline result with a directory")
}

// TestSkillToolInlineWithoutDirIsBodyOnly is the other half: no directory
// means no directory line, never an empty one.
func TestSkillToolInlineWithoutDirIsBodyOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := SkillBlueprint{}.Materialize(ToolOpts{
		Skills: []skillspkg.SkillMeta{
			{Namespace: "rafiki", Name: "coordinating", Inline: true},
		},
		InlineSkillBody: func(context.Context, skillspkg.SkillMeta) (string, string, error) {
			return "BODY_ONLY_MARKER", "", nil
		},
	})
	c.Require().NoError(err, "materialize")
	res, err := tool.Execute(context.Background(), ToolInput(json.RawMessage(`{"skill":"rafiki:coordinating"}`)))
	c.Require().NoError(err, "execute")
	c.Eq("BODY_ONLY_MARKER", res.Text, "inline result without a directory")
}

// A body-fetch failure is the tool's error: a skill the model asked for and
// could not get must not look like an empty skill.
func TestSkillToolInlineBodyErrorSurfaces(t *testing.T) {
	c := assert.NewCollecting(t)
	boom := errors.New("store is gone")
	tool, err := SkillBlueprint{}.Materialize(ToolOpts{
		Skills: []skillspkg.SkillMeta{
			{Namespace: "rafiki", Name: "coordinating", Inline: true},
		},
		InlineSkillBody: func(context.Context, skillspkg.SkillMeta) (string, string, error) {
			return "", "", boom
		},
	})
	c.Require().NoError(err, "materialize")
	_, err = tool.Execute(context.Background(), ToolInput(json.RawMessage(`{"skill":"rafiki:coordinating"}`)))
	c.Require().ErrorIs(err, boom, "execute: got")
}

func TestUnknownSkillErrorListsQualifiedNames(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := SkillBlueprint{}.Materialize(ToolOpts{
		Skills: []skillspkg.SkillMeta{
			{Namespace: "rafiki", Name: "coordinating", Inline: true},
		},
		InlineSkillBody: func(context.Context, skillspkg.SkillMeta) (string, string, error) { return "b", "", nil },
	})
	c.Require().NoError(err, "materialize")
	_, err = tool.Execute(context.Background(), ToolInput(json.RawMessage(`{"skill":"coordinating"}`)))
	c.Require().Error(err, "bare name resolved; want an error naming the qualified form")
	c.StrContains(err.Error(), "rafiki:coordinating", "error does not offer the qualified name: %v", err)
}
