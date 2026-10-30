package msgtest

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"go.graveland.dev/rafiki/pkg/msg"
)

// Case is one fixed request in the corpus. Name is the golden file stem and
// matches [a-z0-9-]+; Req is the provider-independent request; Extra is the
// OpenRouter-style top-level body fields a case adds, or nil.
type Case struct {
	Name  string
	Req   *msg.Request
	Extra map[string]any
}

// Corpus returns the fixed requests every codec is measured against. Each call
// builds fresh values, so a caller may mutate what it receives.
func Corpus() []Case {
	return []Case{
		{
			Name: "plain",
			Req: &msg.Request{
				Model:     "test-model",
				MaxTokens: 1024,
				System:    []msg.Text{{Text: "You are terse."}},
				Messages:  []msg.Message{userText("hello")},
				Cache:     defaultCache(),
			},
		},
		{
			Name: "tools-multiturn",
			Req: &msg.Request{
				Model:     "test-model",
				MaxTokens: 1024,
				Tools:     tools(),
				Messages: []msg.Message{
					userText("read a"),
					assistant(
						msg.Text{Text: "looking"},
						msg.ToolUse{ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
					),
					user(msg.ToolResult{
						ToolUseID: "toolu_1",
						Content:   []msg.Block{msg.Text{Text: "contents"}},
					}),
				},
				Temperature: f64(0.2),
				Cache:       defaultCache(),
			},
		},
		{
			Name: "force-tool",
			Req: &msg.Request{
				Model:     "test-model",
				MaxTokens: 1024,
				Tools:     tools(),
				Messages:  []msg.Message{userText("read a")},
				ForceTool: "read_file",
				Cache:     defaultCache(),
			},
		},
		{
			Name: "thinking-anthropic",
			Req: &msg.Request{
				Model:          "test-model",
				MaxTokens:      4096,
				ThinkingBudget: 2048,
				Messages: []msg.Message{
					assistant(
						msg.Thinking{Text: "hmm", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig-anthropic"}},
						msg.Text{Text: "ok"},
					),
					userText("continue"),
				},
				Cache: defaultCache(),
			},
		},
		{
			Name: "thinking-foreign",
			Req: &msg.Request{
				Model:     "test-model",
				MaxTokens: 1024,
				Messages: []msg.Message{
					assistant(
						msg.Thinking{Text: "a", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig-a"}},
						msg.Thinking{Text: "b", Sig: msg.Signature{Dialect: msg.DialectResponsesAPI, Value: "openai-responses:api:rs_1:blob-api"}},
						msg.Thinking{Text: "c", Sig: msg.Signature{Dialect: msg.DialectResponsesChatGPT, Value: "openai-responses:chatgpt:rs_2:blob-cg"}},
						msg.Text{Text: "done"},
					),
					userText("continue"),
				},
				Cache: defaultCache(),
			},
		},
		{
			Name: "images",
			Req: &msg.Request{
				Model:     "test-model",
				MaxTokens: 1024,
				Messages: []msg.Message{
					user(
						msg.Image{MediaType: "image/png", Data: onePixelPNG()},
						msg.Text{Text: "what is this"},
					),
					assistant(msg.ToolUse{ID: "toolu_img", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)}),
					user(msg.ToolResult{
						ToolUseID: "toolu_img",
						Content:   []msg.Block{msg.Image{MediaType: "image/png", Data: onePixelPNG()}},
					}),
				},
				Cache: defaultCache(),
			},
		},
		{
			Name: "ext-caller",
			Req: &msg.Request{
				Model:     "test-model",
				MaxTokens: 1024,
				Messages: []msg.Message{
					userText("start"),
					assistant(msg.Text{
						Text: "cited",
						Ext: msg.Ext{
							Dialect: msg.DialectAnthropic,
							Raw:     `{"citations":[{"type":"char_location","cited_text":"c","document_index":0,"start_char_index":0,"end_char_index":1}]}`,
						},
					}),
					userText("go on"),
					assistant(msg.ToolUse{
						ID:    "toolu_9",
						Name:  "read_file",
						Input: json.RawMessage(`{"path":"b"}`),
						Ext:   msg.Ext{Dialect: msg.DialectAnthropic, Raw: `{"caller":{"type":"direct"}}`},
					}),
					user(msg.ToolResult{ToolUseID: "toolu_9", Content: []msg.Block{msg.Text{Text: "x"}}}),
				},
				Cache: defaultCache(),
			},
		},
		{
			Name: "extras",
			Req: &msg.Request{
				Model:     "test-model",
				MaxTokens: 1024,
				System:    []msg.Text{{Text: "You are terse."}},
				Messages:  []msg.Message{userText("hello")},
				Cache:     defaultCache(),
			},
			Extra: map[string]any{
				"provider":   map[string]any{"only": []any{"fireworks"}, "ignore": []any{"novita"}},
				"transforms": []any{"middle-out"},
			},
		},
		{
			Name: "long-history",
			Req:  longHistoryRequest(),
		},
	}
}

// GoldenPath returns the corpus file at name, relative to this package's own
// directory so it resolves from any package's test working directory.
func GoldenPath(name string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("msgtest: cannot locate the corpus directory")
	}
	return filepath.Join(filepath.Dir(file), "testdata", name)
}

// Golden reads the corpus file at name. It panics on error: this is a test
// helper, and a missing golden is a broken corpus rather than a condition to
// handle.
func Golden(name string) []byte {
	b, err := os.ReadFile(GoldenPath(name))
	if err != nil {
		panic("msgtest: read golden " + name + ": " + err.Error())
	}
	return b
}

// onePixelPNG is a one-pixel PNG, the payload of every image in the corpus.
const onePixelPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func onePixelPNG() []byte {
	data, err := base64.StdEncoding.DecodeString(onePixelPNGBase64)
	if err != nil {
		panic("msgtest: decode the fixture image: " + err.Error())
	}
	return data
}

func tools() []msg.ToolDef {
	return []msg.ToolDef{
		{
			Name:        "read_file",
			Description: "Read a file from disk.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
		{
			Name:        "list_dir",
			Description: "List a directory.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}
}

func longHistoryRequest() *msg.Request {
	messages := make([]msg.Message, 0, 40)
	for i := 0; i < 20; i++ {
		n := strconv.Itoa(i)
		messages = append(messages, userText("u"+n))
		messages = append(messages, assistant(msg.Text{Text: "a" + n}))
	}
	return &msg.Request{
		Model:     "test-model",
		MaxTokens: 1024,
		Messages:  messages,
		Cache:     defaultCache(),
	}
}

func userText(s string) msg.Message {
	return user(msg.Text{Text: s})
}

func user(blocks ...msg.Block) msg.Message {
	return msg.Message{Role: msg.User, Content: blocks}
}

func assistant(blocks ...msg.Block) msg.Message {
	return msg.Message{Role: msg.Assistant, Content: blocks}
}

func defaultCache() msg.CachePolicy {
	return msg.CachePolicy{SystemTTL: msg.Cache5m, MessagesTTL: msg.Cache5m, Breakpoints: 2}
}

func f64(v float64) *float64 { return &v }
