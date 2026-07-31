package protocol_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestListRequest_RoundTrip(t *testing.T) {
	req := protocol.ListRequest{
		Type: protocol.TypeCtrlList,
		ID:   "req-1",
		Filter: &protocol.ListFilter{
			Status:       "streaming",
			Name:         "afk-impl",
			NameContains: "afk",
			CwdContains:  "/savannah",
			Since:        1716000000,
		},
	}
	roundTrip(t, req, &protocol.ListRequest{})
}

func TestGetRequest_RoundTrip(t *testing.T) {
	req := protocol.GetRequest{
		Type:    protocol.TypeCtrlGet,
		ID:      "req-2",
		ChildID: "c_01HX...",
	}
	roundTrip(t, req, &protocol.GetRequest{})
}

func TestSpawnRequest_RoundTrip(t *testing.T) {
	req := protocol.SpawnRequest{
		Type:               protocol.TypeCtrlSpawn,
		ID:                 "req-3",
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

// TestSpawnRequestNewFieldsUseCamelCaseAndRoundTrip locks down that
// SkillsDirs/MCPConfig follow the protocol's camelCase convention, matching
// every other SpawnRequest field and the wire spec (docs/reference/pi-controller-protocol.md §6.3).
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

// TestSpawnRequestNewFieldsOmitWhenEmpty locks down omitempty so an older
// daemon that predates these fields sees a request it fully understands.
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

func TestResumeRequest_RoundTrip(t *testing.T) {
	req := protocol.ResumeRequest{
		Type:    protocol.TypeCtrlResume,
		ID:      "req-4",
		ChildID: "c_01HX...",
		APIKey:  "sk-ant-resume",
	}
	roundTrip(t, req, &protocol.ResumeRequest{})
}

func TestKillRequest_RoundTrip(t *testing.T) {
	req := protocol.KillRequest{
		Type:              protocol.TypeCtrlKill,
		ID:                "req-5",
		ChildID:           "c_01HX...",
		ShutdownTimeoutMs: 180000,
		KillTimeoutMs:     30000,
	}
	roundTrip(t, req, &protocol.KillRequest{})
}

func TestAuthRequest_RoundTrip(t *testing.T) {
	req := protocol.AuthRequest{
		Type:  protocol.TypeCtrlAuth,
		ID:    "req-6",
		Token: "secret-token",
	}
	roundTrip(t, req, &protocol.AuthRequest{})
}

func TestSubscribeRequest_RoundTrip(t *testing.T) {
	req := protocol.SubscribeRequest{
		Type:    protocol.TypeCtrlSubscribe,
		ID:      "req-7",
		ChildID: "c_01HX...",
		Filter: &protocol.SubscribeFilter{
			Profile: "coarse",
			Include: []string{"turn_end", "agent_end"},
			Exclude: []string{"message_update"},
		},
	}
	roundTrip(t, req, &protocol.SubscribeRequest{})
}

func TestUnsubscribeRequest_RoundTrip(t *testing.T) {
	req := protocol.UnsubscribeRequest{
		Type:    protocol.TypeCtrlUnsubscribe,
		ID:      "req-8",
		ChildID: "c_01HX...",
	}
	roundTrip(t, req, &protocol.UnsubscribeRequest{})
}

func TestGlobalSubscribeRequest_RoundTrip(t *testing.T) {
	req := protocol.GlobalSubscribeRequest{
		Type: protocol.TypeCtrlGlobalSubscribe,
		ID:   "req-9",
	}
	roundTrip(t, req, &protocol.GlobalSubscribeRequest{})
}

func TestGlobalUnsubscribeRequest_RoundTrip(t *testing.T) {
	req := protocol.GlobalUnsubscribeRequest{
		Type: protocol.TypeCtrlGlobalUnsubscribe,
		ID:   "req-10",
	}
	roundTrip(t, req, &protocol.GlobalUnsubscribeRequest{})
}

func TestGetRecentRequest_RoundTrip(t *testing.T) {
	req := protocol.GetRecentRequest{
		Type:    protocol.TypeCtrlGetRecent,
		ID:      "req-11",
		ChildID: "c_01HX...",
		Limit:   50,
		Since:   1716636789,
		Include: []string{"turn_end", "tool_execution_end"},
		Exclude: []string{"message_update"},
	}
	roundTrip(t, req, &protocol.GetRecentRequest{})
}

func TestSendRequest_RoundTrip(t *testing.T) {
	req := protocol.SendRequest{
		Type:    protocol.TypeCtrlSend,
		ID:      "req-12",
		ChildID: "c_01HX...",
		Frame:   json.RawMessage(`{"type":"prompt","message":"Hello","id":"p1"}`),
	}
	roundTrip(t, req, &protocol.SendRequest{})
}

func TestForgetRequest_RoundTrip(t *testing.T) {
	req := protocol.ForgetRequest{
		Type:    protocol.TypeCtrlForget,
		ID:      "req-13",
		ChildID: "c_01HX...",
	}
	roundTrip(t, req, &protocol.ForgetRequest{})
}

func TestForgetAllExitedRequest_RoundTrip(t *testing.T) {
	req := protocol.ForgetAllExitedRequest{
		Type:        protocol.TypeCtrlForgetAllExited,
		ID:          "req-14",
		OlderThanMs: 3600000,
	}
	roundTrip(t, req, &protocol.ForgetAllExitedRequest{})
}

func TestSearchRequest_RoundTrip(t *testing.T) {
	req := protocol.SearchRequest{
		Type:    protocol.TypeCtrlSearch,
		ID:      "req-15",
		Query:   "ublk_register",
		Regex:   true,
		Limit:   50,
		Context: 2,
		SessionFilter: &protocol.SearchSessionFilter{
			CwdContains:  "/savannah",
			NameContains: "afk",
			Since:        1716000000,
		},
	}
	roundTrip(t, req, &protocol.SearchRequest{})
}

func TestStatusRequest_RoundTrip(t *testing.T) {
	req := protocol.StatusRequest{
		Type: protocol.TypeCtrlStatus,
		ID:   "req-16",
	}
	roundTrip(t, req, &protocol.StatusRequest{})
}

// roundTrip marshals src to JSON, unmarshals into dst, and asserts deep equality.
// dst must be a pointer to the same type as src.
func roundTrip[T any](t *testing.T, src T, dst *T) {
	t.Helper()

	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("unmarshal: %v (json: %s)", err, b)
	}

	if !reflect.DeepEqual(src, *dst) {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v\n json %s", *dst, src, b)
	}
}

// TestTypeConstants verifies that type constant strings match the protocol spec.
func TestTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want string
	}{
		{"TypeCtrlList", protocol.TypeCtrlList, "ctrl_list"},
		{"TypeCtrlGet", protocol.TypeCtrlGet, "ctrl_get"},
		{"TypeCtrlSpawn", protocol.TypeCtrlSpawn, "ctrl_spawn"},
		{"TypeCtrlResume", protocol.TypeCtrlResume, "ctrl_resume"},
		{"TypeCtrlKill", protocol.TypeCtrlKill, "ctrl_kill"},
		{"TypeCtrlAuth", protocol.TypeCtrlAuth, "ctrl_auth"},
		{"TypeCtrlSubscribe", protocol.TypeCtrlSubscribe, "ctrl_subscribe"},
		{"TypeCtrlUnsubscribe", protocol.TypeCtrlUnsubscribe, "ctrl_unsubscribe"},
		{"TypeCtrlGlobalSubscribe", protocol.TypeCtrlGlobalSubscribe, "ctrl_global_subscribe"},
		{"TypeCtrlGlobalUnsubscribe", protocol.TypeCtrlGlobalUnsubscribe, "ctrl_global_unsubscribe"},
		{"TypeCtrlGetRecent", protocol.TypeCtrlGetRecent, "ctrl_get_recent"},
		{"TypeCtrlSend", protocol.TypeCtrlSend, "ctrl_send"},
		{"TypeCtrlForget", protocol.TypeCtrlForget, "ctrl_forget"},
		{"TypeCtrlForgetAllExited", protocol.TypeCtrlForgetAllExited, "ctrl_forget_all_exited"},
		{"TypeCtrlSearch", protocol.TypeCtrlSearch, "ctrl_search"},
		{"TypeCtrlStatus", protocol.TypeCtrlStatus, "ctrl_status"},
		{"TypeCtrlResponse", protocol.TypeCtrlResponse, "ctrl_response"},
		{"TypeCtrlEvent", protocol.TypeCtrlEvent, "ctrl_event"},
		{"TypeCtrlChildSpawned", protocol.TypeCtrlChildSpawned, "ctrl_child_spawned"},
		{"TypeCtrlChildExited", protocol.TypeCtrlChildExited, "ctrl_child_exited"},
		{"TypeCtrlChildStatus", protocol.TypeCtrlChildStatus, "ctrl_child_status"},
		{"TypeCtrlChildRenamed", protocol.TypeCtrlChildRenamed, "ctrl_child_renamed"},
	}
	for _, tc := range cases {
		if tc.val != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.val, tc.want)
		}
	}
}

// TestStatusConstants verifies all §10 status values are defined.
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

// TestErrorCodeConstants verifies all §8 error codes are defined.
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

// TestListRequest_OmitEmpty verifies that absent optional fields don't appear in JSON.
func TestListRequest_OmitEmpty(t *testing.T) {
	req := protocol.ListRequest{Type: protocol.TypeCtrlList}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	// id and filter should be absent, not null / empty-string.
	raw := string(b)
	for _, absent := range []string{`"id"`, `"filter"`} {
		if strings.Contains(raw, absent) {
			t.Errorf("field %s should be absent from %s", absent, raw)
		}
	}
}

// TestSpawnRequest_OmitEmpty verifies optional SpawnRequest fields are absent when zero.
func TestSpawnRequest_OmitEmpty(t *testing.T) {
	req := protocol.SpawnRequest{
		Type: protocol.TypeCtrlSpawn,
		Cwd:  "/tmp",
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

// TestSubscribeRequest_NilFilter verifies filter is absent when nil.
func TestSubscribeRequest_NilFilter(t *testing.T) {
	req := protocol.SubscribeRequest{
		Type:    protocol.TypeCtrlSubscribe,
		ChildID: "c_01HX...",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"filter"`) {
		t.Errorf(`"filter" should be absent when nil: %s`, b)
	}
}

// TestChildSummary_NullPID verifies that *int PID and ExitCode serialize as null when nil.
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
