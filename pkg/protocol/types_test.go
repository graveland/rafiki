package protocol_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestSpawnRequest_RoundTrip(t *testing.T) {
	req := protocol.SpawnRequest{
		Name:               "afk-impl",
		Cwd:                "/Users/brent/ts/dev",
		Provider:           "anthropic",
		Model:              "claude-sonnet-4",
		Thinking:           "medium",
		APIKey:             "sk-ant-test",
		NoSession:          true,
		SessionDir:         "/tmp/sessions",
		ResumeSession:      "/tmp/session.jsonl",
		ForkSession:        "/tmp/fork.jsonl",
		Tools:              "bash,read",
		NoTools:            true,
		NoBuiltinTools:     true,
		Extensions:         []string{"/ext/a", "/ext/b"},
		NoExtensions:       true,
		Skills:             []string{"/skill/a"},
		NoSkills:           true,
		SkillsDirs:         []string{"/skill-dir/a", "/skill-dir/b"},
		MCPConfig:          "/c/.mcp.json",
		PromptTemplates:    []string{"/tpl/a"},
		NoPromptTemplates:  true,
		Themes:             []string{"/theme/dark"},
		NoThemes:           true,
		NoContextFiles:     true,
		SystemPrompt:       "You are a helpful assistant.",
		AppendSystemPrompt: "Extra instructions.",
		Verbose:            true,
		PiBinary:           "/usr/local/bin/pi",
		Env:                map[string]string{"FOO": "bar", "BAZ": "qux"},
		EnvOverride:        true,
		ExtraArgs:          []string{"--debug", "--log-level=trace"},
	}
	roundTrip(t, req, &protocol.SpawnRequest{})
}

func TestSpawnRequestNewFieldsUseCamelCaseAndRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	req := protocol.SpawnRequest{
		SkillsDirs: []string{"/a", "/b"},
		MCPConfig:  "/c/.mcp.json",
	}
	b, err := json.Marshal(req)
	c.Require().NoError(err)
	got := string(b)
	for _, want := range []string{`"skillsDirs"`, `"mcpConfig"`} {
		c.StrContains(got, want, "marshalled SpawnRequest missing")
	}
	for _, bad := range []string{`"skills_dirs"`, `"mcp_config"`} {
		c.NotStrContains(got, bad, "marshalled SpawnRequest still contains snake_case")
	}

	var back protocol.SpawnRequest
	c.Require().NoError(json.Unmarshal(b, &back))
	c.False(!reflect.DeepEqual(back.SkillsDirs, req.SkillsDirs) || back.MCPConfig != req.MCPConfig, "round-trip lost data: %+v", back)
}

func TestSpawnRequestNewFieldsOmitWhenEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	b, err := json.Marshal(protocol.SpawnRequest{})
	c.Require().NoError(err)
	for _, absent := range []string{"skillsDirs", "mcpConfig"} {
		c.NotStrContains(string(b), absent, "%s must be omitempty so older daemons ignore it: %s", absent, b)
	}
}

func TestStatusConstants(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want string
	}{
		{"StatusSpawning", string(protocol.StatusSpawning), "spawning"},
		{"StatusIdle", string(protocol.StatusIdle), "idle"},
		{"StatusStreaming", string(protocol.StatusStreaming), "streaming"},
		{"StatusToolRunning", string(protocol.StatusToolRunning), "tool_running"},
		{"StatusCompacting", string(protocol.StatusCompacting), "compacting"},
		{"StatusBlockedUI", string(protocol.StatusBlockedUI), "blocked_ui"},
		{"StatusShuttingDown", string(protocol.StatusShuttingDown), "shutting_down"},
		{"StatusExited", string(protocol.StatusExited), "exited"},
	}
	for _, tc := range cases {
		assert.NewCollecting(t).Eq(tc.want, tc.val, "%s = %q, want", tc.name, tc.val)
	}
}

func TestErrorCodeConstants(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want string
	}{
		{"ErrChildNotFound", protocol.ErrChildNotFound, "child_not_found"},
		{"ErrChildExited", protocol.ErrChildExited, "child_exited"},
		{"ErrChildInGrace", protocol.ErrChildInGrace, "child_in_grace"},
		{"ErrChildShuttingDown", protocol.ErrChildShuttingDown, "child_shutting_down"},
		{"ErrNotResumable", protocol.ErrNotResumable, "not_resumable"},
		{"ErrNotExited", protocol.ErrNotExited, "not_exited"},
		{"ErrSessionFileMissing", protocol.ErrSessionFileMissing, "session_file_missing"},
		{"ErrBackpressure", protocol.ErrBackpressure, "backpressure"},
		{"ErrInvalidArgs", protocol.ErrInvalidArgs, "invalid_args"},
		{"ErrSpawnFailed", protocol.ErrSpawnFailed, "spawn_failed"},
		{"ErrAuthRequired", protocol.ErrAuthRequired, "auth_required"},
		{"ErrAuthInvalid", protocol.ErrAuthInvalid, "auth_invalid"},
		{"ErrNotFound", protocol.ErrNotFound, "not_found"},
		{"ErrFailedPrecondition", protocol.ErrFailedPrecondition, "failed_precondition"},
		{"ErrInternal", protocol.ErrInternal, "internal"},
	}
	for _, tc := range cases {
		assert.NewCollecting(t).Eq(tc.want, tc.val, "%s = %q, want", tc.name, tc.val)
	}
}

func TestSpawnRequest_OmitEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	req := protocol.SpawnRequest{
		Cwd: "/tmp",
	}
	b, err := json.Marshal(req)
	c.Require().NoError(err)
	raw := string(b)
	for _, absent := range []string{
		`"id"`, `"name"`, `"provider"`, `"model"`, `"thinking"`, `"apiKey"`,
		`"noSession"`, `"sessionDir"`, `"resumeSession"`, `"forkSession"`,
		`"tools"`, `"noTools"`, `"noBuiltinTools"`, `"extensions"`, `"noExtensions"`,
		`"skills"`, `"noSkills"`, `"skillsDirs"`, `"mcpConfig"`, `"promptTemplates"`, `"noPromptTemplates"`,
		`"themes"`, `"noThemes"`, `"noContextFiles"`,
		`"systemPrompt"`, `"appendSystemPrompt"`, `"verbose"`,
		`"piBinary"`, `"env"`, `"envOverride"`, `"extraArgs"`,
	} {
		c.NotStrContains(raw, absent, "field")
	}
}

func TestSpawnRequestParentChildID(t *testing.T) {
	c := assert.NewAborting(t)
	req := protocol.SpawnRequest{
		Cwd:           "/tmp",
		ParentChildID: "c_parent",
	}
	b, err := json.Marshal(req)
	c.NoError(err, "marshal")
	c.StrContains(string(b), `"parentChildId":"c_parent"`, "ParentChildID missing or misspelled in JSON: %s", b)

	// Omitted when empty — an absent parent must not appear as a null or "".
	b2, err := json.Marshal(protocol.SpawnRequest{Cwd: "/tmp"})
	c.NoError(err, "marshal empty")
	c.NotStrContains(string(b2), "parentChildId", "empty ParentChildID should be omitted, got: %s", b2)

	var back protocol.SpawnRequest
	c.NoError(json.Unmarshal(b, &back), "unmarshal")
	c.Eq("c_parent", back.ParentChildID, "round-trip lost ParentChildID: got")
}

func TestSpawnRequestExecutorRefRoundTrips(t *testing.T) {
	c := assert.NewAborting(t)
	req := protocol.SpawnRequest{ExecutorRef: "greyshift"}
	b, err := json.Marshal(req)
	c.NoError(err)
	var got protocol.SpawnRequest
	c.NoError(json.Unmarshal(b, &got))
	c.Eq("greyshift", got.ExecutorRef, "want ExecutorRef=greyshift, got")
}

func TestChildSummary_NullPID(t *testing.T) {
	c := assert.NewCollecting(t)
	cs := protocol.ChildSummary{
		ChildID:      "c_01HX...",
		PID:          nil,
		Cwd:          "/tmp",
		Status:       "exited",
		StartedAt:    time.Unix(1716636789, 0),
		LastActivity: time.Unix(1716636890, 0),
	}
	b, err := json.Marshal(cs)
	c.Require().NoError(err)
	raw := string(b)
	c.StrContains(raw, `"pid":null`, "expected pid:null in")
	c.StrContains(raw, `"exitCode":null`, "expected exitCode:null in")
}

// roundTrip marshals v and unmarshals it back into a fresh value, failing
// when the data does not survive.
func roundTrip[T any](t *testing.T, src T, dst *T) {
	t.Helper()
	c := assert.NewCollecting(t)
	b, err := json.Marshal(src)
	c.Require().NoError(err, "marshal")
	c.Require().NoError(json.Unmarshal(b, dst), "unmarshal")
	c.EqDeep(*dst, src, "round-trip lost data:\n sent")
}
