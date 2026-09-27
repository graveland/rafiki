package protocol_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
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
	req := protocol.SpawnRequest{
		SkillsDirs: []string{"/a", "/b"},
		MCPConfig:  "/c/.mcp.json",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"skillsDirs"`, `"mcpConfig"`} {
		if !strings.Contains(got, want) {
			t.Errorf("marshalled SpawnRequest missing %s; protocol is camelCase and the wire spec documents it that way: %s", want, got)
		}
	}
	for _, bad := range []string{`"skills_dirs"`, `"mcp_config"`} {
		if strings.Contains(got, bad) {
			t.Errorf("marshalled SpawnRequest still contains snake_case %s", bad)
		}
	}

	var back protocol.SpawnRequest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.SkillsDirs, req.SkillsDirs) || back.MCPConfig != req.MCPConfig {
		t.Errorf("round-trip lost data: %+v", back)
	}
}

func TestSpawnRequestNewFieldsOmitWhenEmpty(t *testing.T) {
	b, err := json.Marshal(protocol.SpawnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"skillsDirs", "mcpConfig"} {
		if strings.Contains(string(b), absent) {
			t.Errorf("%s must be omitempty so older daemons ignore it: %s", absent, b)
		}
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
		if tc.val != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.val, tc.want)
		}
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
		{"ErrInternal", protocol.ErrInternal, "internal"},
	}
	for _, tc := range cases {
		if tc.val != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.val, tc.want)
		}
	}
}

func TestSpawnRequest_OmitEmpty(t *testing.T) {
	req := protocol.SpawnRequest{
		Cwd: "/tmp",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
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
		if strings.Contains(raw, absent) {
			t.Errorf("field %s should be absent from %s", absent, raw)
		}
	}
}

func TestSpawnRequestParentChildID(t *testing.T) {
	req := protocol.SpawnRequest{
		Cwd:           "/tmp",
		ParentChildID: "c_parent",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"parentChildId":"c_parent"`) {
		t.Fatalf("ParentChildID missing or misspelled in JSON: %s", b)
	}

	// Omitted when empty — an absent parent must not appear as a null or "".
	b2, err := json.Marshal(protocol.SpawnRequest{Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(b2), "parentChildId") {
		t.Fatalf("empty ParentChildID should be omitted, got: %s", b2)
	}

	var back protocol.SpawnRequest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ParentChildID != "c_parent" {
		t.Fatalf("round-trip lost ParentChildID: got %q", back.ParentChildID)
	}
}

func TestSpawnRequestExecutorRefRoundTrips(t *testing.T) {
	req := protocol.SpawnRequest{ExecutorRef: "greyshift"}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.SpawnRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExecutorRef != "greyshift" {
		t.Fatalf("want ExecutorRef=greyshift, got %q", got.ExecutorRef)
	}
}

func TestChildSummary_NullPID(t *testing.T) {
	cs := protocol.ChildSummary{
		ChildID:      "c_01HX...",
		PID:          nil,
		Cwd:          "/tmp",
		Status:       "exited",
		StartedAt:    1716636789,
		LastActivity: 1716636890,
	}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	if !strings.Contains(raw, `"pid":null`) {
		t.Errorf("expected pid:null in %s", raw)
	}
	if !strings.Contains(raw, `"exitCode":null`) {
		t.Errorf("expected exitCode:null in %s", raw)
	}
}

func TestDarajaHelloWireNames(t *testing.T) {
	b, err := json.Marshal(protocol.DarajaHelloRequest{
		Type: "daraja_hello", ChildID: "c1", Ticket: "t", PID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type"`, `"childId"`, `"ticket"`, `"pid"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshalled %s, missing %s", b, want)
		}
	}
	if strings.Contains(string(b), `"credential"`) {
		t.Error("an unset credential must be omitted, not sent empty")
	}
}

// roundTrip marshals v and unmarshals it back into a fresh value, failing
// when the data does not survive.
func roundTrip[T any](t *testing.T, src T, dst *T) {
	t.Helper()
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(src, *dst) {
		t.Errorf("round-trip lost data:\n sent: %+v\n  got: %+v", src, *dst)
	}
}
