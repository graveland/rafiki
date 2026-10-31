package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"
)

// The tag verbs on fakeSpawner are no-ops: the tool's argument parsing is what
// these tests pin, and a test that needs to see what reached the spawner uses
// tagSpawner below. They exist so the in-memory fake satisfies AgentSpawner
// for every other test in this package.
func (f *fakeSpawner) Tag(context.Context, TagSpec) (TagInfo, error) { return TagInfo{}, nil }
func (f *fakeSpawner) Untag(context.Context, string) error           { return nil }
func (f *fakeSpawner) Tags(context.Context, TagQuery) ([]TagInfo, error) {
	return nil, nil
}

// tagSpawner records what the tag tools handed the daemon adapter.
type tagSpawner struct {
	fakeSpawner
	tagged   []TagSpec
	untagged []string
	queries  []TagQuery

	rows     []TagInfo
	tagErr   error
	untagErr error
	tagsErr  error
}

func (s *tagSpawner) Tag(_ context.Context, spec TagSpec) (TagInfo, error) {
	if s.tagErr != nil {
		return TagInfo{}, s.tagErr
	}
	s.tagged = append(s.tagged, spec)
	if len(s.rows) > 0 {
		return s.rows[0], nil
	}
	return TagInfo{TagID: "t_1", Name: spec.Name, Scope: spec.Scope, ChildID: spec.ChildID, Ordinal: 7, Current: true}, nil
}

func (s *tagSpawner) Untag(_ context.Context, tagID string) error {
	if s.untagErr != nil {
		return s.untagErr
	}
	s.untagged = append(s.untagged, tagID)
	return nil
}

func (s *tagSpawner) Tags(_ context.Context, q TagQuery) ([]TagInfo, error) {
	if s.tagsErr != nil {
		return nil, s.tagsErr
	}
	s.queries = append(s.queries, q)
	return s.rows, nil
}

func runTagTool(t *testing.T, sp AgentSpawner, name, args string) (string, error) {
	t.Helper()
	reg, ctx := newAgentTools(t, sp)
	res, err := reg.Execute(ctx, name, json.RawMessage(args))
	return res.Text, err
}

func TestAgentTagCreateAndMoveArgs(t *testing.T) {
	c := assert.NewCollecting(t)

	t.Run("create carries every field through", func(t *testing.T) {
		sp := &tagSpawner{}
		out, err := runTagTool(t, sp, "agent_tag",
			`{"name":"build-ok","scope":"tree","child_id":"c_other","cursor":"m:7","note":"green"}`)
		c.Require().NoError(err, "agent_tag create")
		c.Len(sp.tagged, 1, "agent_tag handed the spawner %d spec(s)", len(sp.tagged))
		c.EqDeep(TagSpec{Name: "build-ok", Scope: "tree", ChildID: "c_other", Cursor: "m:7", Note: "green"}, sp.tagged[0],
			"create spec")
		// The row is rendered with the fork cursor, so the model can hand the
		// same value back to agent_spawn.
		for _, want := range []string{"build-ok", "tree", "t_1", "c_other", "m:7"} {
			c.StrContains(out, want, "agent_tag output missing a field")
		}
	})

	t.Run("a move names tag_id and expect_node_id", func(t *testing.T) {
		sp := &tagSpawner{}
		_, err := runTagTool(t, sp, "agent_tag", `{"tag_id":"t_9","expect_node_id":"node-4","cursor":"m:12"}`)
		c.Require().NoError(err, "agent_tag move")
		c.Len(sp.tagged, 1, "agent_tag handed the spawner %d spec(s)", len(sp.tagged))
		c.EqDeep(TagSpec{TagID: "t_9", ExpectNodeID: "node-4", Cursor: "m:12"}, sp.tagged[0], "move spec")
	})

	t.Run("neither a name nor a tag id is refused at the tool", func(t *testing.T) {
		sp := &tagSpawner{}
		_, err := runTagTool(t, sp, "agent_tag", `{"scope":"tree"}`)
		c.Error(err, "agent_tag with no name and no tag_id")
		c.Len(sp.tagged, 0, "a refused call must reach no spawner")
	})

	t.Run("agent_untag passes the tag id", func(t *testing.T) {
		sp := &tagSpawner{}
		_, err := runTagTool(t, sp, "agent_untag", `{"tag_id":"t_3"}`)
		c.Require().NoError(err, "agent_untag")
		c.EqDiff([]string{"t_3"}, sp.untagged, "untag ids")
	})

	t.Run("agent_tags passes the query and renders history markers", func(t *testing.T) {
		sp := &tagSpawner{rows: []TagInfo{
			{TagID: "t_1", Name: "first", Scope: "lineage", ChildID: "c_a", Ordinal: 3, Current: false},
			{TagID: "t_1", Name: "first", Scope: "lineage", ChildID: "c_a", Ordinal: 9, Current: true},
		}}
		out, err := runTagTool(t, sp, "agent_tags", `{"name_prefix":"fir","limit":5,"tag_id":"t_1"}`)
		c.Require().NoError(err, "agent_tags")
		c.EqDeep(TagQuery{NamePrefix: "fir", Limit: 5, TagID: "t_1"}, sp.queries[0], "tag query")
		c.StrContains(out, "m:3", "history row's cursor")
		c.StrContains(out, "m:9", "live row's cursor")
		c.StrContains(out, "(not current)", "a superseded version must be marked")
	})

	t.Run("a spawner error is wrapped, not swallowed", func(t *testing.T) {
		sp := &tagSpawner{tagErr: errors.New("tags are unavailable on this daemon (no database)")}
		_, err := runTagTool(t, sp, "agent_tag", `{"name":"x","child_id":"c_a"}`)
		c.Error(err, "agent_tag with a failing spawner")
		c.StrContains(err.Error(), "agent_tag", "the refusal must name the tool")
		c.StrContains(err.Error(), "no database", "the refusal must carry the daemon's reason")
	})
}

// TestAgentSpawnForkParamsReachSpawnSpec pins the tool's fork arguments onto
// the SpawnSpec the daemon adapter builds its request from: the tool parses
// them, and the spawners copy them onto protocol.SpawnRequest.
func TestAgentSpawnForkParamsReachSpawnSpec(t *testing.T) {
	c := assert.NewCollecting(t)

	t.Run("fork_from", func(t *testing.T) {
		sp := &fakeSpawner{}
		_, err := runTagTool(t, sp, "agent_spawn",
			`{"prompt":"go","fork_from":{"child_id":"c_src","cursor":"m:4"}}`)
		c.Require().NoError(err, "agent_spawn with fork_from")
		c.Len(sp.spawned, 1, "agent_spawn handed the spawner %d spec(s)", len(sp.spawned))
		c.EqDeep(&ForkFromSpec{ChildID: "c_src", Cursor: "m:4"}, sp.spawned[0].ForkFrom, "fork_from")
		c.Eq("", sp.spawned[0].FromTag, "from_tag must stay empty")
	})

	t.Run("fork_from without a cursor takes the last completed turn", func(t *testing.T) {
		sp := &fakeSpawner{}
		_, err := runTagTool(t, sp, "agent_spawn", `{"prompt":"go","fork_from":{"child_id":"c_src"}}`)
		c.Require().NoError(err, "agent_spawn with a bare fork_from")
		c.EqDeep(&ForkFromSpec{ChildID: "c_src"}, sp.spawned[0].ForkFrom, "fork_from")
	})

	t.Run("from_tag", func(t *testing.T) {
		sp := &fakeSpawner{}
		_, err := runTagTool(t, sp, "agent_spawn", `{"prompt":"go","from_tag":"build-ok"}`)
		c.Require().NoError(err, "agent_spawn with from_tag")
		c.Eq("build-ok", sp.spawned[0].FromTag, "from_tag")
		c.Nil(sp.spawned[0].ForkFrom, "fork_from must stay nil")
	})

	t.Run("a plain spawn carries neither", func(t *testing.T) {
		sp := &fakeSpawner{}
		_, err := runTagTool(t, sp, "agent_spawn", `{"prompt":"go"}`)
		c.Require().NoError(err, "agent_spawn")
		c.Nil(sp.spawned[0].ForkFrom, "fork_from")
		c.Eq("", sp.spawned[0].FromTag, "from_tag")
	})
}

// TestAgentSpawnForkFromRequiresChildID pins the one validation the tool does
// itself: a fork_from block with no child_id would reach the daemon as a fork
// from nobody, which it answers as "not visible" — a message the model cannot
// act on.
func TestAgentSpawnForkFromRequiresChildID(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := &fakeSpawner{}
	_, err := runTagTool(t, sp, "agent_spawn", `{"prompt":"go","fork_from":{"cursor":"m:2"}}`)
	c.Error(err, "agent_spawn with fork_from and no child_id")
	c.StrContains(err.Error(), "child_id", "the refusal must name the missing field")
	c.Len(sp.spawned, 0, "a refused spawn must reach no spawner")

	// The same shape is refused when the block arrives as an empty object.
	_, err = runTagTool(t, sp, "agent_spawn", `{"prompt":"go","fork_from":{}}`)
	c.Error(err, "agent_spawn with an empty fork_from")
	c.Len(sp.spawned, 0, "a refused spawn must reach no spawner")
}

// TestAgentTagToolsDeclineWithoutSpawner keeps the tag verbs on the same
// nil-means-decline rule as the rest of the agent family: a caller with no
// spawner binding has no tree to tag.
func TestAgentTagToolsDeclineWithoutSpawner(t *testing.T) {
	reg := DefaultBlueprint.MaterializeAll(ToolOpts{Cwd: t.TempDir()})
	for _, name := range []string{"agent_tag", "agent_untag", "agent_tags"} {
		if _, err := reg.Execute(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s must not be registered without a spawner", name)
		}
	}
}
