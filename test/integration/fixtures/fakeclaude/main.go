// SPDX-License-Identifier: Apache-2.0

// Command fakeclaude is a CONTROLLED stand-in for Claude Code's headless
// stream-json transport (claude -p --input-format stream-json --output-format
// stream-json --verbose --replay-user-messages), selected with CLAUDE_BINARY.
//
// It exists because the delivery acknowledgement is a property of the REAL
// transport loop: the daemon writes a user frame with a uuid, the binary echoes
// that uuid back with isReplay:true when it accepts the message into a turn, and
// the daemon publishes a receipt. A synthetic bus event proves nothing about
// that loop, and the real binary costs money and needs a network, so the
// integration harness gets this instead: a process the daemon launches for
// real, speaks the real stdio dialect with, and parses with the real
// claudeProvider.
//
// What it does, exactly as the binary does (measured on 2.1.290):
//
//   - every stdin user frame is ECHOED immediately, before any turn work, as
//     {type:"user", message:{...}, session_id, parent_tool_use_id:null,
//     uuid:<the uuid sent>, timestamp, isReplay:true}. That echo is the whole
//     acceptance signal; it is emitted on read, so it arrives even while a
//     previous turn is still in flight.
//   - with FAKE_CLAUDE_TURNS=1 it then drives a real /v1/messages request at
//     ANTHROPIC_BASE_URL (the daemon's proxy face), carrying ANTHROPIC_AUTH_TOKEN
//     and the ANTHROPIC_CUSTOM_HEADERS the daemon handed it — which is what makes
//     the request attributable to the child and therefore CAPTURED. The SSE
//     message id it sees becomes the assistant frame's message.id, which is what
//     arms the child's message gate until the capture persists that row.
//   - a message that arrives while a turn is already in flight is echoed at once
//     and then carried into the NEXT request wrapped in Claude Code's mid-turn
//     preamble ("The user sent a new message while you were working: …"), which
//     is the whole reason the acknowledgement cannot be matched on text: the
//     captured row does not contain what the user sent.
//
// It REJECTS the headless flag deliberately. `--replay-user-messages` is a
// required capability (claudeargv.ReplayUserMessages): a fixture that quietly
// tolerated its absence would make every test here pass against a launch shape
// that cannot acknowledge anything. So a launch without it exits 2 with the
// commander-style refusal the real binary emits, and FAKE_CLAUDE_REJECT_REPLAY=1
// reproduces an OLD binary (one that does not know the flag) even when the
// daemon passes it — which is how the launch-failure path is exercised end to
// end without downgrading a real install.
//
// Two adversarial knobs, both of them things the real binary does:
//
//	FAKE_CLAUDE_REQUEST_MODEL=m  the model written into the request body, when
//	                          the spawn model is provider-qualified
//	FAKE_CLAUDE_HOLD_REQUEST=<file>  do not send the turn's /v1/messages
//	                          request until <file> exists. This is the
//	                          "deliberately blocked capture": the daemon has
//	                          written the message, the child has acknowledged
//	                          it, and the request that would carry it is still
//	                          not on the wire.
//	FAKE_CLAUDE_ECHO_TIMES=n  echo each accepted message n times (the echo is
//	                          at-least-once; a duplicate must yield ONE receipt)
//	--resume                  replays the PREVIOUS process's last echoed uuid
//	                          before reading stdin, exactly as `--resume`
//	                          replays an old frame. That uuid belongs to a
//	                          retired launch generation, so it must publish
//	                          nothing.
//
// FAKE_CLAUDE_DIR is the observation directory: one events-<pid>.jsonl per
// process, one JSON object per line, flushed as it happens. The tests wait on
// those lines (never on a sleep) to know what the child has actually done.
//
//	{"event":"launch","pid":…,"argv":[…],"replay_flag":true}
//	{"event":"echo","uuid":"…","text":"…","mid_turn":false}
//	{"event":"stale_echo","uuid":"…"}                 (a resumed launch's replay)
//	{"event":"request","n":1,"messages":[…]}          (before the POST)
//	{"event":"turn_end","n":1,"message_id":"…","status":"ok"}
//	{"event":"exit","code":0}
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// replayFlag is the capability this fixture refuses to run without. Its
	// absence is a launch failure, never a silently degraded child.
	replayFlag = "--replay-user-messages"

	// midTurnPreamble is Claude Code's own wording for a message that reached it
	// while a turn was running (design §1). The captured request carries THIS
	// text, never the message the user typed, which is what makes a
	// text-matched acknowledgement retire the wrong row.
	midTurnPreamble = "The user sent a new message while you were working:\n"
	midTurnSuffix   = "\n\nThis is how Claude Code surfaces messages the user sends mid-turn — " +
		"within the running turn, often alongside the next tool result, rather than as a " +
		"separate conversation turn. Address the message above as you continue this turn."
)

func main() {
	code := run()
	record(map[string]any{"event": "exit", "code": code})
	os.Exit(code)
}

func run() int {
	argv := os.Args[1:]
	replay := hasFlag(argv, replayFlag)
	record(map[string]any{"event": "launch", "pid": os.Getpid(), "argv": argv, "replay_flag": replay})

	// The deliberate rejection. An old binary dies on argv; so does this one, in
	// both shapes: the flag genuinely absent, or the flag present but the build
	// is told to pretend it predates it.
	if !replay || os.Getenv("FAKE_CLAUDE_REJECT_REPLAY") == "1" {
		fmt.Fprintf(os.Stderr, "error: unknown option '%s'\n", replayFlag)
		return 2
	}

	f := &fixture{
		sessionID: "fake-session-" + strconv.Itoa(os.Getpid()),
		model:     flagValue(argv, "--model"),
		turns:     os.Getenv("FAKE_CLAUDE_TURNS") == "1",
		baseURL:   os.Getenv("ANTHROPIC_BASE_URL"),
		token:     os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		headers:   parseCustomHeaders(os.Getenv("ANTHROPIC_CUSTOM_HEADERS")),
		http:      &http.Client{Timeout: 120 * time.Second},
		done:      make(chan struct{}),
	}
	if f.model == "" {
		f.model = os.Getenv("ANTHROPIC_CUSTOM_MODEL_OPTION")
	}
	// FAKE_CLAUDE_REQUEST_MODEL overrides the model written into the request
	// body. The spawn model is provider-qualified ("anthropic/sonnet-latest"),
	// and the proxy's Messages face routes a SLASHED id through OpenRouter — a
	// bare Anthropic id is what reaches the native-Anthropic upstream, which is
	// the route a claude child actually takes.
	if m := os.Getenv("FAKE_CLAUDE_REQUEST_MODEL"); m != "" {
		f.model = m
	}

	f.replayStaleOnResume(hasFlag(argv, "--resume"))
	f.readLineLoop()
	<-f.done
	return 0
}

// replayStaleOnResume emits the previous process's last echoed uuid, which is
// what a `--resume` replay of an old frame looks like on stdout. The uuid names
// a message the RETIRED launch minted, so the daemon must drop it.
func (f *fixture) replayStaleOnResume(resumed bool) {
	if !resumed {
		return
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_CLAUDE_DIR"), "last-echo.json"))
	if err != nil {
		return
	}
	var prev struct {
		UUID string `json:"uuid"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &prev); err != nil || prev.UUID == "" {
		return
	}
	f.write(map[string]any{
		"type": "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{
			{"type": "text", "text": prev.Text},
		}},
		"session_id":         f.sessionID,
		"parent_tool_use_id": nil,
		"uuid":               prev.UUID,
		"timestamp":          time.Now().UTC().Format(time.RFC3339Nano),
		"isReplay":           true,
	})
	record(map[string]any{"event": "stale_echo", "uuid": prev.UUID})
}

// fixture is the per-process transport state.
type fixture struct {
	sessionID string
	model     string
	turns     bool
	baseURL   string
	token     string
	headers   map[string]string
	http      *http.Client

	mu      sync.Mutex
	history []accepted // every message this process accepted, in order
	pending []accepted // accepted while a turn was in flight, not yet carried
	inTurn  bool
	turnN   int
	inited  bool

	// done closes when the last in-flight turn finishes after stdin EOF, so the
	// process exits the way the real binary does: after its work, not before it.
	done chan struct{}
}

// accepted is one user message this process took into a turn.
type accepted struct {
	uuid    string
	text    string
	midTurn bool
}

// readLineLoop reads the daemon's frames until EOF, then waits for the last
// turn to drain.
func (f *fixture) readLineLoop() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var in struct {
			Type    string `json:"type"`
			UUID    string `json:"uuid"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &in); err != nil {
			continue
		}
		if in.Type != "user" {
			// claude accepts only user messages on this stream; anything else is
			// dropped, as the binary does.
			continue
		}
		text := contentText(in.Message.Content)

		f.mu.Lock()
		midTurn := f.inTurn
		f.history = append(f.history, accepted{uuid: in.UUID, text: text, midTurn: midTurn})
		f.mu.Unlock()

		// The echo, on read and before any turn work: this is the acceptance
		// signal, and it must not wait for a turn that is still in flight.
		f.ensureInit()
		for range echoTimes() {
			f.write(map[string]any{
				"type": "user",
				// The echo returns the message it was HANDED, content shape and
				// all: a text-only prompt goes out as a plain string and comes
				// back as one (measured on 2.1.290 — see
				// testdata/claude-capture-delivery). A fixture that "helpfully"
				// re-shaped it into a block array would hide a parser that only
				// accepts arrays, which is exactly the bug this fixture caught.
				"message":            map[string]any{"role": "user", "content": in.Message.Content},
				"session_id":         f.sessionID,
				"parent_tool_use_id": nil,
				"uuid":               in.UUID,
				"timestamp":          time.Now().UTC().Format(time.RFC3339Nano),
				"isReplay":           true,
			})
			record(map[string]any{"event": "echo", "uuid": in.UUID, "text": text, "mid_turn": midTurn})
		}
		rememberLastEcho(in.UUID, text)

		if !f.turns {
			continue
		}
		if midTurn {
			// Queued behind the running turn; the next request carries it.
			f.mu.Lock()
			f.pending = append(f.pending, accepted{uuid: in.UUID, text: text, midTurn: true})
			f.mu.Unlock()
			continue
		}
		f.startTurn()
	}

	if f.turns {
		f.drain()
	}
	close(f.done)
}

// drain runs the turns queued behind the last one, so a mid-turn message sent
// just before stdin closed is still carried by a request.
func (f *fixture) drain() {
	for {
		f.mu.Lock()
		queued := len(f.pending)
		f.mu.Unlock()
		if queued == 0 {
			return
		}
		f.startTurn()
	}
}

// startTurn runs one request synchronously, carrying every message accepted so
// far — the mid-turn ones wrapped the way Claude Code wraps them.
func (f *fixture) startTurn() {
	f.mu.Lock()
	f.turnN++
	n := f.turnN
	f.inTurn = true
	f.pending = nil
	history := append([]accepted(nil), f.history...)
	f.mu.Unlock()

	body := map[string]any{
		"model":      f.model,
		"max_tokens": 64,
		"stream":     true,
		// One client tool with a schema: the proxy's capture treats a request
		// that declares no client tool as a per-tool HELPER (a titler, a quota
		// probe) and publishes none of its rows.
		"tools": []map[string]any{{
			"name":         "Bash",
			"description":  "fixture tool",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		"messages": f.messages(history),
	}
	record(map[string]any{"event": "request", "n": n, "messages": body["messages"]})

	f.holdIfAsked(n)
	status, id, text := f.post(body)

	f.mu.Lock()
	f.inTurn = false
	f.mu.Unlock()

	if status != "ok" {
		record(map[string]any{"event": "turn_end", "n": n, "message_id": id, "status": status})
		// A failed request still ends the turn for the binary's purposes; the
		// daemon sees the proxy's own error and publishes it.
		return
	}
	f.writeAssistant(id, text)
	f.write(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"duration_ms": 1, "num_turns": 1, "result": text,
		"session_id": f.sessionID, "total_cost_usd": 0,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	record(map[string]any{"event": "turn_end", "n": n, "message_id": id, "status": status})
}

// messages renders the request's message list: every message this process has
// accepted, in order, mid-turn ones wrapped the way Claude Code wraps them.
func (f *fixture) messages(history []accepted) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, a := range history {
		text := a.text
		if a.midTurn {
			text = midTurnPreamble + text + midTurnSuffix
		}
		out = append(out, map[string]any{"role": "user", "content": text})
	}
	return out
}

// holdIfAsked parks the turn before its request goes out, when the test asked
// for it. The gate file is polled rather than watched: this is a fixture, and a
// 20ms poll over a temp path is cheaper than an inotify dependency.
func (f *fixture) holdIfAsked(n int) {
	gate := os.Getenv("FAKE_CLAUDE_HOLD_REQUEST")
	if gate == "" {
		return
	}
	record(map[string]any{"event": "holding_request", "n": n})
	for i := 0; i < 6000; i++ { // 120s cap, so a wedged test cannot hang the suite
		if _, err := os.Stat(gate); err == nil {
			record(map[string]any{"event": "request_released", "n": n})
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// post sends one /v1/messages request to the daemon's proxy face and reads the
// SSE stream back. It returns "ok" plus the response's message id and text, or a
// failure status for the test to see.
func (f *fixture) post(body map[string]any) (status, id, text string) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "encode failed", "", ""
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(f.baseURL, "/")+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return "request failed", "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	for k, v := range f.headers {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return "transport failed", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "upstream status " + strconv.Itoa(resp.StatusCode), "", ""
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			id = ev.Message.ID
		case "content_block_delta":
			sb.WriteString(ev.Delta.Text)
		}
	}
	if id == "" {
		return "stream carried no message id", "", ""
	}
	return "ok", id, sb.String()
}

// ensureInit emits system/init once, before the first frame that follows a
// message. The real binary announces its session id on the first turn, which is
// what lets the daemon store it and pass `--resume <id>` when the child is
// resumed — the shape the stale-replay case needs.
func (f *fixture) ensureInit() {
	f.mu.Lock()
	already := f.inited
	f.inited = true
	f.mu.Unlock()
	if already {
		return
	}
	f.write(map[string]any{
		"type": "system", "subtype": "init",
		"session_id": f.sessionID, "model": f.model,
		"tools": []string{"Bash"}, "slash_commands": []string{},
	})
}

// writeAssistant emits the turn's assistant frame. Its message.id is what arms
// the child's message gate, so it must be the id the capture will report.
func (f *fixture) writeAssistant(id, text string) {
	f.ensureInit()
	f.write(map[string]any{
		"type":               "assistant",
		"message":            assistantMessage(id, f.model, text),
		"session_id":         f.sessionID,
		"parent_tool_use_id": nil,
		"uuid":               newUUID(),
		"timestamp":          time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func assistantMessage(id, model, text string) map[string]any {
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
}

// write emits one stdout frame. Every write is flushed: the daemon's reader
// must see the echo the instant it exists, not when a buffer fills.
func (f *fixture) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = os.Stdout.Write(b)
}

// record appends one observation line to the events file, flushing it. The
// tests wait on these lines rather than on a clock.
func record(v any) {
	dir := os.Getenv("FAKE_CLAUDE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	path := filepath.Join(dir, "events-"+strconv.Itoa(os.Getpid())+".jsonl")
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = fh.Write(append(b, '\n'))
	_ = fh.Close()
}

// echoTimes is how many times each echo is written (default one).
func echoTimes() int {
	n, err := strconv.Atoi(os.Getenv("FAKE_CLAUDE_ECHO_TIMES"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// rememberLastEcho records the uuid this process last acknowledged, so a
// `--resume` of the NEXT process can replay it as stale traffic.
func rememberLastEcho(uuid, text string) {
	dir := os.Getenv("FAKE_CLAUDE_DIR")
	if dir == "" || uuid == "" {
		return
	}
	b, err := json.Marshal(map[string]string{"uuid": uuid, "text": text})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "last-echo.json"), b, 0o600)
}

// hasFlag reports whether argv carries flag as a whole element or as the
// "--flag=value" prefix form.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// flagValue returns the element following flag, or "".
func flagValue(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
	}
	return ""
}

// parseCustomHeaders decodes ANTHROPIC_CUSTOM_HEADERS, whose separator is a
// literal newline and nothing else (proxyenv.FormatHeaders).
func parseCustomHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// contentText flattens a user message's content, which the daemon writes as a
// plain string for a text-only prompt and as a block array when images ride
// along.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// newUUID mints a v4 uuid for the assistant frame's own uuid — the daemon
// ignores it, but a frame without one is not a frame the real binary emits.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
