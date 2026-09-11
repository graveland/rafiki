package claudethread

import "testing"

const realParentSystem = `You are Claude Code.
x-anthropic-billing-header: cc_version=2.1.259.b07; cc_entrypoint=cli; cch=42c51; cc_prev_req=req_011CefhtWhZTvjE51gZAqXTG; cc_prompt_id=9139dce9-4ba8-4a85-898f-b743016fb58e;
`

const realSubagentSystem = `You are Claude Code.
x-anthropic-billing-header: cc_version=2.1.267.019; cc_entrypoint=sdk-cli; cch=6d579; cc_is_subagent=true; cc_prev_req=req_011Cev1T3UA8yhHDZxKuGutn; cc_prompt_id=989e877e-bce4-4908-8224-0f81ce4dd64c;
`

const realRootSystem = `You are Claude Code.
x-anthropic-billing-header: cc_version=2.1.259.cf7; cc_entrypoint=cli; cch=d05e5;
`

func TestParseBillingHeader(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          string
		wantOK      bool
		isSubagent  bool
		prevReq     string
		entrypoint  string
		wantVersion string
		wantPrompt  string
	}{
		{"parent", realParentSystem, true, false, "req_011CefhtWhZTvjE51gZAqXTG", "cli", "2.1.259.b07", "9139dce9-4ba8-4a85-898f-b743016fb58e"},
		{"subagent", realSubagentSystem, true, true, "req_011Cev1T3UA8yhHDZxKuGutn", "sdk-cli", "2.1.267.019", "989e877e-bce4-4908-8224-0f81ce4dd64c"},
		{"root turn has no prev_req", realRootSystem, true, false, "", "cli", "2.1.259.cf7", ""},
		{"no header at all", "You are a helpful assistant.", false, false, "", "", "", ""},
		{"empty", "", false, false, "", "", "", ""},
		// A duplicate key must not clear a set flag, whatever the later value.
		{"duplicate subagent key keeps true", "x-anthropic-billing-header: cc_is_subagent=true; cc_is_subagent=false;", true, true, "", "", "", ""},
		// The prefix is line-anchored: prose quoting the literal mid-line is
		// not the header.
		{"literal quoted mid-line is not the header", "Prose mentions x-anthropic-billing-header: cc_is_subagent=true in passing.\nx-anthropic-billing-header: cc_version=9;", true, false, "", "", "9", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseBillingHeader(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got.IsSubagent != tc.isSubagent {
				t.Errorf("IsSubagent = %v, want %v", got.IsSubagent, tc.isSubagent)
			}
			if got.PrevReq != tc.prevReq {
				t.Errorf("PrevReq = %q, want %q", got.PrevReq, tc.prevReq)
			}
			if got.Entrypoint != tc.entrypoint {
				t.Errorf("Entrypoint = %q, want %q", got.Entrypoint, tc.entrypoint)
			}
			if got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
			}
			if got.PromptID != tc.wantPrompt {
				t.Errorf("PromptID = %q, want %q", got.PromptID, tc.wantPrompt)
			}
		})
	}
}

func TestIsSubagentIsPresentOnlyWhenTrue(t *testing.T) {
	// cc_is_subagent=false appears zero times in 4922 measured turns. A parser
	// that keys on the literal "false" would therefore never fire, and one that
	// treats absence as unknown would classify every parent turn unknown.
	b, ok := ParseBillingHeader(realParentSystem)
	if !ok {
		t.Fatal("expected a header")
	}
	if b.IsSubagent {
		t.Error("absent cc_is_subagent must mean parent, not subagent")
	}
}

func TestBillingFromRequestReadsSystemBlockZero(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","system":[{"type":"text","text":"` +
		`x-anthropic-billing-header: cc_version=1; cc_entrypoint=sdk-cli; cc_is_subagent=true;"},` +
		`{"type":"text","text":"ignored"}],"messages":[]}`)
	got, ok := BillingFromRequest(body)
	if !ok {
		t.Fatal("expected a header from system block 0")
	}
	if !got.IsSubagent {
		t.Error("IsSubagent = false, want true")
	}
}

func TestBillingFromRequestToleratesEveryShape(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no system key", `{"model":"m","messages":[]}`},
		{"system is null", `{"system":null,"messages":[]}`},
		{"system is a string", `{"system":"you are helpful","messages":[]}`},
		{"system is an empty array", `{"system":[],"messages":[]}`},
		{"block 0 has no text", `{"system":[{"type":"image"}],"messages":[]}`},
		{"not json at all", `{{{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := BillingFromRequest([]byte(tc.body)); ok {
				t.Errorf("ok = true, want false for %s", tc.name)
			}
		})
	}
}

func TestPreviousMessageID(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"present", `{"diagnostics":{"previous_message_id":"msg_011Cev1KMNTKZBg156cMKuSm"},"messages":[]}`, "msg_011Cev1KMNTKZBg156cMKuSm"},
		{"diagnostics null", `{"diagnostics":null,"messages":[]}`, ""},
		{"no diagnostics", `{"messages":[]}`, ""},
		{"diagnostics without the field", `{"diagnostics":{"other":1},"messages":[]}`, ""},
		{"not json", `not json`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreviousMessageID([]byte(tc.body)); got != tc.want {
				t.Errorf("PreviousMessageID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeclaresClientTools pins the discriminator that separates a Task
// subagent from Claude Code's per-tool-call model helpers. Both carry
// cc_is_subagent and both found a thread; only one can act.
//
// The bodies are the real measured shapes: the web-search helper's lone tool
// is SERVER-executed and schema-less, which is why "tools is non-empty" would
// be the wrong test.
func TestDeclaresClientTools(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"task subagent declares client tools",
			`{"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}},
			           {"name":"Read","input_schema":{"type":"object"}}]}`, true},
		{"webfetch summarizer declares none", `{"tools":[]}`, false},
		{"webfetch summarizer omits the key", `{"model":"claude-haiku-4-5-20251001"}`, false},
		{"websearch helper declares only a server tool",
			`{"tools":[{"name":"web_search","type":"web_search_20250305","max_uses":8}]}`, false},
		{"one client tool among server tools",
			`{"tools":[{"name":"web_search","type":"web_search_20250305"},
			           {"name":"Grep","input_schema":{"type":"object"}}]}`, true},
		{"input_schema null is not a client tool",
			`{"tools":[{"name":"Broken","input_schema":null}]}`, false},
		{"tools null", `{"tools":null}`, false},
		{"not json", `{{{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeclaresClientTools([]byte(tc.body)); got != tc.want {
				t.Errorf("DeclaresClientTools = %v, want %v", got, tc.want)
			}
		})
	}
}
