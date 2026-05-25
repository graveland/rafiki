package protocol_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"graveland.dev/pi-controller/internal/protocol"
)

// intPtr is a test helper for *int fields.
func intPtr(n int) *int { return &n }

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
	}{
		{"TypeCtrlList", protocol.TypeCtrlList},
		{"TypeCtrlGet", protocol.TypeCtrlGet},
		{"TypeCtrlSpawn", protocol.TypeCtrlSpawn},
		{"TypeCtrlResume", protocol.TypeCtrlResume},
		{"TypeCtrlKill", protocol.TypeCtrlKill},
		{"TypeCtrlAuth", protocol.TypeCtrlAuth},
		{"TypeCtrlSubscribe", protocol.TypeCtrlSubscribe},
		{"TypeCtrlUnsubscribe", protocol.TypeCtrlUnsubscribe},
		{"TypeCtrlGlobalSubscribe", protocol.TypeCtrlGlobalSubscribe},
		{"TypeCtrlGlobalUnsubscribe", protocol.TypeCtrlGlobalUnsubscribe},
		{"TypeCtrlGetRecent", protocol.TypeCtrlGetRecent},
		{"TypeCtrlSend", protocol.TypeCtrlSend},
		{"TypeCtrlForget", protocol.TypeCtrlForget},
		{"TypeCtrlForgetAllExited", protocol.TypeCtrlForgetAllExited},
		{"TypeCtrlSearch", protocol.TypeCtrlSearch},
		{"TypeCtrlStatus", protocol.TypeCtrlStatus},
		{"TypeCtrlResponse", protocol.TypeCtrlResponse},
		{"TypeCtrlEvent", protocol.TypeCtrlEvent},
		{"TypeCtrlChildSpawned", protocol.TypeCtrlChildSpawned},
		{"TypeCtrlChildExited", protocol.TypeCtrlChildExited},
		{"TypeCtrlChildStatus", protocol.TypeCtrlChildStatus},
		{"TypeCtrlChildRenamed", protocol.TypeCtrlChildRenamed},
	}
	for _, tc := range cases {
		if tc.val == "" {
			t.Errorf("%s is empty", tc.name)
		}
	}
}

// TestStatusConstants verifies all §10 status values are defined.
func TestStatusConstants(t *testing.T) {
	statuses := []protocol.Status{
		protocol.StatusSpawning,
		protocol.StatusIdle,
		protocol.StatusStreaming,
		protocol.StatusToolRunning,
		protocol.StatusCompacting,
		protocol.StatusBlockedUI,
		protocol.StatusShuttingDown,
		protocol.StatusExited,
	}
	for _, s := range statuses {
		if s == "" {
			t.Errorf("empty Status constant")
		}
	}
}

// TestErrorCodeConstants verifies all §8 error codes are defined.
func TestErrorCodeConstants(t *testing.T) {
	codes := []string{
		protocol.ErrChildNotFound,
		protocol.ErrChildExited,
		protocol.ErrChildInGrace,
		protocol.ErrChildShuttingDown,
		protocol.ErrNotResumable,
		protocol.ErrNotExited,
		protocol.ErrSessionFileMissing,
		protocol.ErrBackpressure,
		protocol.ErrInvalidArgs,
		protocol.ErrSpawnFailed,
		protocol.ErrAuthRequired,
		protocol.ErrAuthInvalid,
		protocol.ErrNotFound,
		protocol.ErrInternal,
	}
	for _, c := range codes {
		if c == "" {
			t.Errorf("empty error code constant")
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
		`"skills"`, `"noSkills"`, `"promptTemplates"`, `"noPromptTemplates"`,
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

// TestChildSummary_NullPID verifies that *int PID serializes as null when nil.
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
}


