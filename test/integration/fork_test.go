// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end proof of the two fork entry points a model actually reaches: a
// spawn that borrows another agent's history (fork_from), and one that starts
// from a named tag (from_tag) created by a child's own agent_tag tool.
//
// The tools themselves are exercised for real — a fundi child runs in the
// daemon, so the fake upstream below answers a marked prompt with a streamed
// tool_use block and the daemon executes agent_tag / agent_spawn in that
// child's own binding. Assertions are on the durable history (GetHistory) and
// on the recorded upstream request bodies, because "the fork happened" means
// the source's rows are in the new child's transcript AND on the wire.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// forkSeat is a stand-in upstream for these tests: it records every
// /v1/messages body and answers a plain end_turn text turn, except for a
// request whose text carries marker — that one is answered with a streamed
// tool_use call, so the child runs a real tool. A request already carrying a
// tool_result is always answered with text: that is the turn AFTER the call.
type forkSeat struct {
	srv    *httptest.Server
	marker string
	tool   string
	input  map[string]any

	mu     sync.Mutex
	bodies [][]byte
	// inputTokens is the prompt size an ordinary reply reports; 0 means the
	// fixture default of 10. It is the number the compact-on-fork detection
	// reads off the source's last turn_end, so a test that needs an
	// OVER-WINDOW source raises it rather than growing the transcript.
	inputTokens int64
}

func newForkSeat(t *testing.T) *forkSeat {
	t.Helper()
	s := &forkSeat{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

// reportUsage sets the input_tokens an ordinary reply reports — the prompt size
// the compact-on-fork detection measures.
func (s *forkSeat) reportUsage(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputTokens = n
}

// script makes the seat answer any request carrying marker with a tool_use
// call. It is guarded by the same lock handle reads under: a test re-scripts
// the seat mid-run, while requests may be in flight.
func (s *forkSeat) script(marker, tool string, input map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marker, s.tool, s.input = marker, tool, input
}

func (s *forkSeat) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	marker, tool, input, usage := s.marker, s.tool, s.input, s.inputTokens
	s.mu.Unlock()
	if usage == 0 {
		usage = 10
	}

	text := string(body)
	switch {
	case strings.Contains(text, compactionPromptFirstLine):
		// A compaction summary call. It is NON-streaming (llm.compact clears
		// the stream handler), so the reply must be a plain JSON Message, and
		// it must carry a <summary> block or the compaction writes nothing and
		// no boundary ever appears.
		s.replySummary(w)
	case strings.Contains(text, `"tool_result"`):
		s.replyText(w, "noted", usage)
	case marker != "" && strings.Contains(text, marker):
		s.replyToolUse(w, tool, input)
	default:
		s.replyText(w, "done", usage)
	}
}

// replySummary answers a compaction summary call with a complete non-streaming
// Messages response carrying a <summary> block.
func (s *forkSeat) replySummary(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body, _ := json.Marshal(map[string]any{
		"id":          "msg_fork_compact",
		"type":        "message",
		"role":        "assistant",
		"model":       "mini",
		"content":     []map[string]any{{"type": "text", "text": "<analysis>handoff</analysis><summary>handoff summary</summary>"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 5},
	})
	_, _ = w.Write(body)
}

func (s *forkSeat) requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.bodies))
	copy(out, s.bodies)
	return out
}

func (s *forkSeat) waitForRequests(t *testing.T, n int, timeout time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		if got := s.requests(); len(got) >= n {
			return got
		} else {
			last = len(got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("fake upstream saw %d request(s), want at least %d", last, n)
	return nil
}

func (s *forkSeat) writeEvents(w http.ResponseWriter, events []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, ev := range events {
		_, _ = io.WriteString(w, ev)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (s *forkSeat) replyText(w http.ResponseWriter, text string, inputTokens int64) {
	delta, _ := json.Marshal(text)
	s.writeEvents(w, []string{
		"event: message_start\n" + fmt.Sprintf(`data: {"type":"message_start","message":{"id":"msg_fork","type":"message","role":"assistant","model":"mini","content":[],"usage":{"input_tokens":%d,"output_tokens":1}}}`, inputTokens) + "\n\n",
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		"event: content_block_delta\n" + fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, delta) + "\n\n",
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n",
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
	})
}

func (s *forkSeat) replyToolUse(w http.ResponseWriter, tool string, input map[string]any) {
	args, _ := json.Marshal(input)
	s.writeEvents(w, []string{
		"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_fork_tu","type":"message","role":"assistant","model":"mini","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n",
		"event: content_block_start\n" + fmt.Sprintf(
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_fork","name":%q,"input":{}}}`, tool) + "\n\n",
		"event: content_block_delta\n" + fmt.Sprintf(
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, string(args)) + "\n\n",
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n",
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
	})
}

// The compact-on-fork destination: a DECLARED model with a genuinely small
// window, so a source of a few thousand tokens is over-window without a real
// catalog. Declaring it in providers.toml is what lets the daemon price it —
// Controller.declContextWindow reads the provider registry before the
// OpenRouter catalog, and ModelInfo is how the CLI asks (the listing carries a
// window only for models the catalog knows).
const (
	tinyForkModel    = "fakeor/tiny"
	tinyForkWindow   = 1000
	sourceForkTokens = 5000
)

// writeForkProviders writes this file's providers fixture: the recording seat,
// plus the tiny declared destination above. fakeor/mini — the model the sources
// run on — is deliberately NOT declared, so the source's own window stays
// unknown and only the destination's is measurable.
func writeForkProviders(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.toml")
	body := fmt.Sprintf(`default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.fakeor]
kind = "anthropic-openrouter"
base_url = %q

[providers.fakeor.models.tiny]
id = "tiny"
context_window = %d
`, baseURL, tinyForkWindow)
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(body), 0o600), "write fork providers fixture")
	return path
}

// forkDaemon boots a DB-backed daemon whose only provider is the given seat.
func forkDaemon(t *testing.T, seat *forkSeat) *daemon {
	t.Helper()
	providers := writeForkProviders(t, seat.srv.URL)
	d := bootDaemonDB(t, nextDaemonID(), append(noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"RAFIKI_PROVIDERS="+providers,
	)...)
	t.Cleanup(func() { os.RemoveAll(d.homeDir) })
	return d
}

// forkSpawn spawns through the daemon's control socket, as token ("" is the
// unix socket's local trust), and returns the error rather than failing the
// test so the refusal arms can read it.
func forkSpawn(t *testing.T, client rafikiv1connect.ControlClient, token string, req *rafikiv1.SpawnRequest) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.Spawn(ctx, authorize(token, connect.NewRequest(req)))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetChildId(), nil
}

// forkChild spawns one fundi child on the fake model and fails on refusal.
func forkChild(t *testing.T, client rafikiv1connect.ControlClient, token, parent string) string {
	t.Helper()
	id, err := forkSpawn(t, client, token, &rafikiv1.SpawnRequest{
		Cwd:           "/tmp",
		Kind:          protocol.KindFundi,
		Model:         "fakeor/mini",
		ParentChildId: parent,
	})
	assert.NewAborting(t).NoError(err, "spawn a fundi child")
	return id
}

// waitAssistantTurn polls a child's durable history until an assistant message
// containing want is stored — the proof its turn COMPLETED, which is what a
// fork needs and what a still-streaming turn would race.
func waitAssistantTurn(t *testing.T, client rafikiv1connect.ControlClient, childID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []*rafikiv1.Event
	for time.Now().Before(deadline) {
		last = getHistoryEvents(t, client, childID)
		for _, ev := range last {
			if ev.GetAssistantMessage() != nil && strings.Contains(eventText(ev), want) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("child %s never stored an assistant turn containing %q; history:\n%v", childID, want, last)
}

// userTexts returns every user message's text in history order.
func userTexts(events []*rafikiv1.Event) []string {
	var out []string
	for _, ev := range events {
		if ev.GetUserMessage() != nil {
			out = append(out, eventText(ev))
		}
	}
	return out
}

// waitChildUnder polls the child list until a live child is parented by parent
// and returns it.
func waitChildUnder(t *testing.T, client rafikiv1connect.ControlClient, parent string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var seen []string
	for time.Now().Before(deadline) {
		seen = seen[:0]
		for _, c := range listChildren(t, client) {
			seen = append(seen, c.GetChildId())
			if c.GetLabels()["rafiki/parent"] == parent {
				return c.GetChildId()
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no child appeared under %s within %s; saw %v", parent, timeout, seen)
	return ""
}

// TestForkEndToEndBorrowsHistory is fork_from end to end: A takes one turn,
// B is spawned from A's last completed turn, and B's own turn both REPLAYS A's
// rows in its durable history and SENDS them to the provider.
func TestForkEndToEndBorrowsHistory(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	seat := newForkSeat(t)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	a := forkChild(t, client, "", "")
	aPrompt := "alpha-" + uniqueSuffix()
	sendPrompt(t, client, a, aPrompt)
	seat.waitForRequests(t, 1, 60*time.Second)
	waitAssistantTurn(t, client, a, "done", 30*time.Second)

	b, err := forkSpawn(t, client, "", &rafikiv1.SpawnRequest{
		Cwd:      "/tmp",
		Kind:     protocol.KindFundi,
		Model:    "fakeor/mini",
		ForkFrom: &rafikiv1.SpawnRequest_ForkFrom{ChildId: a},
	})
	c.Require().NoError(err, "spawn a child forked from A")
	c.NotEq("", b, "the fork returned no child id")

	bPrompt := "bravo-" + uniqueSuffix()
	sendPrompt(t, client, b, bPrompt)
	bodies := seat.waitForRequests(t, 2, 60*time.Second)

	// B's request carries A's turn: the borrowed segment reaches the provider,
	// not merely the stored transcript.
	c.StrContains(string(bodies[len(bodies)-1]), aPrompt, "B's upstream request does not carry A's history")
	waitAssistantTurn(t, client, b, "done", 30*time.Second)

	// And B's durable history is A's rows then B's own.
	got := userTexts(getHistoryEvents(t, client, b))
	c.EqDiff([]string{aPrompt, bPrompt}, got, "B's history is A's user rows then B's own")

	killChildren(t, client, a, b)
}

// TestTagEndToEndTreeScope is from_tag end to end through the TOOLS: A1 tags
// itself `tree` with its own agent_tag call, and its sibling A2 then spawns
// with agent_spawn from_tag — the scope is what makes a cousin's fork legal.
func TestTagEndToEndTreeScope(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	tagName := "tree-" + uniqueSuffix()
	tagMarker := "TAGMARK-" + uniqueSuffix()
	forkMarker := "FORKMARK-" + uniqueSuffix()

	// One seat, two scripted turns: a request carrying the tag marker calls
	// agent_tag, one carrying the fork marker calls agent_spawn from_tag.
	seat := newForkSeat(t)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	a := forkChild(t, client, "", "")
	a1 := forkChild(t, client, "", a)
	a2 := forkChild(t, client, "", a)

	// A1 needs a completed turn before it can tag itself: a fork starts after
	// one.
	sendPrompt(t, client, a1, "warm-"+uniqueSuffix())
	seat.waitForRequests(t, 1, 60*time.Second)
	waitAssistantTurn(t, client, a1, "done", 30*time.Second)

	// A1 tags itself, through its own agent_tag tool.
	seat.script(tagMarker, "agent_tag", map[string]any{"name": tagName, "scope": "tree"})
	sendPrompt(t, client, a1, tagMarker)
	waitTag(t, client, tagName, a1, 60*time.Second)

	// A2 — a sibling, not a descendant of A1 — forks from the tag.
	seat.script(forkMarker, "agent_spawn", map[string]any{
		"prompt":   "start-" + uniqueSuffix(),
		"model":    "fakeor/mini",
		"from_tag": tagName,
	})
	sendPrompt(t, client, a2, forkMarker)

	forked := waitChildUnder(t, client, a2, 60*time.Second)
	c.NotEq("", forked, "the tree-scoped tag produced no child under A2")

	killChildren(t, client, forked, a2, a1, a)
}

// waitTag polls ListTags until name is visible with creatorChildID as its
// creator, proving the child's own tool call — not the test — created it.
func waitTag(t *testing.T, client rafikiv1connect.ControlClient, name, creatorChildID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []*rafikiv1.TagRow
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := client.ListTags(ctx, connect.NewRequest(&rafikiv1.ListTagsRequest{NamePrefix: name}))
		cancel()
		assert.NewAborting(t).NoError(err, "ListTags")
		last = resp.Msg.GetRows()
		for _, row := range last {
			if row.GetName() == name && row.GetCreatorChildId() == creatorChildID {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("tag %q created by %s never appeared; last: %v", name, creatorChildID, last)
}

// TestTagEndToEndCrossOwnerRefused pins the owner boundary on the tag plane: a
// tag one user created is invisible to another user's spawn, which is refused
// rather than resolved, while the owner itself can still fork from it.
func TestTagEndToEndCrossOwnerRefused(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	tagName := "owner-" + uniqueSuffix()
	seat := newForkSeat(t)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	tokenA := d.createMCPUser(t)
	tokenB := d.createMCPUser(t)

	// The owner's child needs a completed turn before it can be tagged.
	a := forkChild(t, client, tokenA, "")
	aPrompt := "owner-turn-" + uniqueSuffix()
	sendPrompt(t, client, a, aPrompt)
	seat.waitForRequests(t, 1, 60*time.Second)
	waitAssistantTurn(t, client, a, "done", 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := client.Tag(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.TagRequest{
		Name:    tagName,
		Scope:   rafikiv1.TagScope_TAG_SCOPE_OWNER,
		ChildId: a,
	})))
	c.Require().NoError(err, "owner A tags its own child")

	// The owner may fork from it.
	ownFork, err := forkSpawn(t, client, tokenA, &rafikiv1.SpawnRequest{
		Cwd:     "/tmp",
		Kind:    protocol.KindFundi,
		Model:   "fakeor/mini",
		FromTag: tagName,
	})
	c.Require().NoError(err, "the tag's own owner forks from it")
	c.NotEq("", ownFork, "owner fork returned no child id")

	// Another user may not: the tag is not theirs, and the refusal is the one
	// permission error rather than a distinguishable not-found.
	_, err = forkSpawn(t, client, tokenB, &rafikiv1.SpawnRequest{
		Cwd:     "/tmp",
		Kind:    protocol.KindFundi,
		Model:   "fakeor/mini",
		FromTag: tagName,
	})
	c.Require().Error(err, "a foreign tag must not resolve")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "foreign from_tag = %v", err)
	c.StrContains(err.Error(), "not visible", "the refusal must not reveal that the tag exists")

	killChildren(t, client, ownFork, a)
}

// killChildren stops the test's children so the daemon's shutdown does not
// have to wait them out.
func killChildren(t *testing.T, client rafikiv1connect.ControlClient, ids ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, id := range ids {
		if id == "" {
			continue
		}
		_, _ = client.Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: id}))
	}
}

// TestCLIModelsSwitchForksClosesAndRenames runs `rafiki models switch` end to
// end against a real daemon: the switched conversation is a NEW child on the
// new model whose history is the source's, the source is closed, and the new
// child answers to the source's name — the clone-path model switch, with the
// routing pin replayed so an operator's steering survives.
func TestCLIModelsSwitchForksClosesAndRenames(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	seat := newForkSeat(t)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	name := "switchme-" + uniqueSuffix()
	// A coordinator above the source, so the test pins the part the CLI and
	// TUI compositions must carry: a SUBAGENT's switch stays under its
	// coordinator (rafiki/parent copied, rename judged against the same
	// sibling set).
	coord, err := forkSpawn(t, client, "", &rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  protocol.KindFundi,
		Model: "fakeor/mini",
		Name:  "coord-" + uniqueSuffix(),
	})
	c.Require().NoError(err, "spawn the coordinator")
	a, err := forkSpawn(t, client, "", &rafikiv1.SpawnRequest{
		Cwd:           "/tmp",
		Kind:          protocol.KindFundi,
		Model:         "fakeor/mini",
		Name:          name,
		ParentChildId: coord,
	})
	c.Require().NoError(err, "spawn the source child under the coordinator")

	aPrompt := "alpha-" + uniqueSuffix()
	sendPrompt(t, client, a, aPrompt)
	seat.waitForRequests(t, 1, 60*time.Second)
	waitAssistantTurn(t, client, a, "done", 30*time.Second)

	// An operator pin set BEFORE the switch; the new child must inherit it.
	rtCtx, rtCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rtCancel()
	if _, err := client.SetRouting(rtCtx, connect.NewRequest(&rafikiv1.SetRoutingRequest{
		ChildId: a,
		Delta:   "sort=price",
	})); err != nil {
		t.Fatalf("set routing on the source: %v", err)
	}

	out, err := cliCmd(t, d, "models", "switch", name, "fakeor/mini").CombinedOutput()
	c.NoError(err, "models switch failed: %v\noutput: %s", err, out)

	// The renamed survivor: a child answering to the source's name that is
	// NOT the source. The rename rides the switch's own RPCs, so a short poll
	// absorbs the close-before-rename ordering.
	newID := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, ch := range listChildren(t, client) {
			if ch.GetName() == name && ch.GetChildId() != a {
				newID = ch.GetChildId()
			}
		}
		if newID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.NotEq("", newID, "no child inherited the source's name %q; output: %s", name, out)

	// The survivor carries the borrowed history, the new model and the pin.
	got := userTexts(getHistoryEvents(t, client, newID))
	c.EqDiff([]string{aPrompt}, got, "the switched child's history is not the source's")

	rtCtx2, rtCancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer rtCancel2()
	get, err := client.GetChild(rtCtx2, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: newID}))
	c.Require().NoError(err, "get the switched child")
	c.StrContains(get.Msg.GetChild().GetModel(), "fakeor/mini", "the switched child is not on the requested model")
	c.StrContains(get.Msg.GetChild().GetRouting(), "sort=price", "the source's routing pin did not survive the switch")
	// Provenance: the daemon stamps rafiki/forked-from at spawn, which is
	// what `rafiki get`/`list` render in the FORKED FROM column.
	c.Eq(a, get.Msg.GetChild().GetLabels()["rafiki/forked-from"],
		"the switched child must name its source in rafiki/forked-from")
	// And the switch keeps the subagent under its coordinator: dropping the
	// parent would move it out of the coordinator's lineage and rename it
	// against the wrong sibling set.
	c.Eq(coord, get.Msg.GetChild().GetLabels()["rafiki/parent"],
		"the switched child must stay under the source's coordinator")

	// The source is gone from the live list (closed, not merely stopped).
	for _, ch := range listChildren(t, client) {
		c.NotEq(a, ch.GetChildId(), "the source child is still listed after the switch")
	}

	killChildren(t, client, newID)
}

// TestCLITagVerbsAndForkFromCreate drives the CLIENT surfaces the fork work
// added: `rafiki tag add/list/rm`, and `rafiki create --fork-from` producing a
// child whose durable history is the source's. The daemon's fork machinery is
// pinned by TestForkEndToEndBorrowsHistory; this pins the CLI wiring in front
// of it, execing the real binary like every CLI test here.
func TestCLITagVerbsAndForkFromCreate(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	seat := newForkSeat(t)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	name := "cli-fork-src-" + uniqueSuffix()
	a, err := forkSpawn(t, client, "", &rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  protocol.KindFundi,
		Model: "fakeor/mini",
		Name:  name,
	})
	c.Require().NoError(err, "spawn the source child")
	aPrompt := "alpha-" + uniqueSuffix()
	sendPrompt(t, client, a, aPrompt)
	seat.waitForRequests(t, 1, 60*time.Second)
	waitAssistantTurn(t, client, a, "done", 30*time.Second)

	// add → list shows it; the name resolves for fork time.
	out, err := cliCmd(t, d, "tag", "add", name, "checkpoint-"+uniqueSuffix()).CombinedOutput()
	c.NoError(err, "tag add failed: %v\noutput: %s", err, out)

	tagName := ""
	for _, row := range listTags(t, client) {
		if strings.HasPrefix(row.GetName(), "checkpoint-") {
			tagName = row.GetName()
		}
	}
	c.Require().NotEq("", tagName, "the created tag is listed")

	// --fork-from through the real create: the new child borrows the source's
	// history, which is the proof the flag reached the wire request.
	// --no-local-executor: this daemon runs with its executor pool disabled
	// (forkDaemon), so the create pre-flight must not try to join this
	// machine as a workspace — the direct-RPC spawns above prove the
	// daemon's own fallback hosts the child.
	out, err = cliCmd(t, d, "create", "-d", "--no-local-executor", "--fork-from", name, "--model", "fakeor/mini").CombinedOutput()
	c.NoError(err, "create --fork-from failed: %v\noutput: %s", err, out)

	// The create output IS the record ("childId: c_..."), so the assertion
	// targets the child this invocation made rather than scanning every row
	// the shared test database holds — other tests run in parallel, and their
	// children are none of this one's business.
	newID := ""
	for _, line := range strings.Split(string(out), "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "childId: "); ok {
			newID = strings.TrimSpace(id)
		}
	}
	c.Require().NotEq("", newID, "create printed no childId; output: %s", out)

	// rm tombstones by name; the name no longer resolves anywhere.
	out, err = cliCmd(t, d, "tag", "rm", tagName).CombinedOutput()
	c.NoError(err, "tag rm failed: %v\noutput: %s", err, out)
	for _, row := range listTags(t, client) {
		c.NotEq(tagName, row.GetName(), "the tag survives its rm")
	}

	killChildren(t, client, a, newID)
}

// listTags returns the live tags visible to the socket's local trust.
func listTags(t *testing.T, client rafikiv1connect.ControlClient) []*rafikiv1.TagRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.ListTags(ctx, connect.NewRequest(&rafikiv1.ListTagsRequest{}))
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	return resp.Msg.GetRows()
}

// TestCLIModelsSwitchAsTokenUserKeepsTheParent pins finding 3's admission
// through a REAL user credential: the ordinary switch test runs over the
// local-trust socket (nil identity), which every policy passes, so it cannot
// answer "may a user credential name a parent it owns on Spawn". Here the
// whole tree — coordinator, source, and the switch itself — is created and
// driven by a minted user's bearer token, and the switched child must come
// out still parented under that user's coordinator.
func TestCLIModelsSwitchAsTokenUserKeepsTheParent(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	seat := newForkSeat(t)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	// Mint the user through the CLI: `rafiki user create` dials token-less,
	// the UDS admits it anonymously, and the token lands in profile "it"'s
	// token file, which every later cliCmdIn call presents.
	configDir := t.TempDir()
	userCmd := cliCmdIn(t, d, configDir, "--output", "json", "user", "create", operatorName())
	var userStderr bytes.Buffer
	userCmd.Stderr = &userStderr
	userOut, err := userCmd.Output()
	c.NoError(err, "user create failed: %v\nstdout: %s\nstderr: %s", err, userOut, userStderr.String())
	tokenPath := filepath.Join(configDir, "rafiki", "profiles", "it", "token")
	tokenRaw, err := os.ReadFile(tokenPath)
	c.Require().NoError(err, "read the minted token")
	token := strings.TrimSpace(string(tokenRaw))
	c.NotEq("", token, "the minted token is empty")

	// The tree is the USER's: both spawns present its bearer token.
	coord, err := forkSpawn(t, client, token, &rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  protocol.KindFundi,
		Model: "fakeor/mini",
		Name:  "ucoord-" + uniqueSuffix(),
	})
	c.Require().NoError(err, "spawn the user's coordinator")
	name := "uswitchme-" + uniqueSuffix()
	a, err := forkSpawn(t, client, token, &rafikiv1.SpawnRequest{
		Cwd:           "/tmp",
		Kind:          protocol.KindFundi,
		Model:         "fakeor/mini",
		Name:          name,
		ParentChildId: coord,
	})
	c.Require().NoError(err, "spawn the user's source child")
	aPrompt := "user-alpha-" + uniqueSuffix()
	sendPrompt(t, client, a, aPrompt)
	seat.waitForRequests(t, 1, 60*time.Second)
	waitAssistantTurn(t, client, a, "done", 30*time.Second)

	out, err := cliCmdIn(t, d, configDir, "models", "switch", name, "fakeor/mini").CombinedOutput()
	c.NoError(err, "models switch as the owning user failed: %v\noutput: %s", err, out)

	newID := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && newID == "" {
		for _, ch := range listChildren(t, client) {
			if ch.GetName() == name && ch.GetChildId() != a {
				newID = ch.GetChildId()
			}
		}
		if newID == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	c.NotEq("", newID, "no switched child inherited the name; output: %s", out)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	get, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: newID}))
	c.Require().NoError(err, "get the switched child")
	c.Eq(coord, get.Msg.GetChild().GetLabels()["rafiki/parent"],
		"the user's switched child must stay under the user's coordinator")
	c.Eq(a, get.Msg.GetChild().GetLabels()["rafiki/forked-from"],
		"the switched child must name its source")

	killChildren(t, client, newID, coord)
}

// ─── compact on fork ─────────────────────────────────────────────────────────
//
// The compact-on-fork layer, driven through the real CLI: an over-window fork
// is compacted on the source's own model through a TEMPORARY helper child, and
// the continuation borrows the compacted result. What these tests pin is the
// part the CLI owns — that the flag exists, that the refusal names both
// numbers, and that only `models switch` closes the source.

// waitAssistantTurns polls a child's durable history until it holds at least n
// assistant messages — the proof n turns COMPLETED. A fork starts from a
// completed turn and a compaction needs several working rows, so both need the
// turns actually finished, not merely requested.
func waitAssistantTurns(t *testing.T, client rafikiv1connect.ControlClient, childID string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []*rafikiv1.Event
	for time.Now().Before(deadline) {
		last = getHistoryEvents(t, client, childID)
		got := 0
		for _, ev := range last {
			if ev.GetAssistantMessage() != nil {
				got++
			}
		}
		if got >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("child %s never stored %d assistant turn(s); history:\n%v", childID, n, last)
}

// forkSourceWithTurns spawns a named fundi source on the fake model and drives
// turns completed turns through it. A compaction needs minCompactableRows (4)
// working rows, so a source with a single turn would have nothing to summarise.
func forkSourceWithTurns(t *testing.T, client rafikiv1connect.ControlClient, seat *forkSeat, name string, turns int) string {
	t.Helper()
	c := assert.NewAborting(t)
	src, err := forkSpawn(t, client, "", &rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  protocol.KindFundi,
		Model: "fakeor/mini",
		Name:  name,
	})
	c.Require().NoError(err, "spawn the compact source")
	for i := 1; i <= turns; i++ {
		sendPrompt(t, client, src, fmt.Sprintf("alpha-%d-%s", i, uniqueSuffix()))
		seat.waitForRequests(t, i, 60*time.Second)
		waitAssistantTurns(t, client, src, i, 60*time.Second)
	}
	return src
}

// hasCompactionBoundary reports whether a history carries a compaction
// boundary — the durable marker of a compaction the daemon actually wrote.
func hasCompactionBoundary(events []*rafikiv1.Event) bool {
	for _, ev := range events {
		if ev.GetCompactionBoundary() != nil {
			return true
		}
	}
	return false
}

// childIDFromCreateOutput reads the child id off `rafiki create -d`'s text
// record ("childId: c_..."), so a test asserts on the child ITS invocation
// made rather than scanning every row the shared database holds.
func childIDFromCreateOutput(t *testing.T, out []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(out), "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "childId: "); ok {
			return strings.TrimSpace(id)
		}
	}
	t.Fatalf("create printed no childId; output: %s", out)
	return ""
}

// childIDSet returns the live children's ids, sorted, for a before/after
// comparison that does not depend on row counts.
func childIDSet(t *testing.T, client rafikiv1connect.ControlClient) []string {
	t.Helper()
	var ids []string
	for _, ch := range listChildren(t, client) {
		ids = append(ids, ch.GetChildId())
	}
	sort.Strings(ids)
	return ids
}

// watchForkedChildren records every live child's rafiki/forked-from label until
// the returned stop function is called, then hands back what it saw.
//
// It exists because the compact fork's INTERMEDIATE is live only while its
// summary call runs: compactfork closes it once the fork exists, before the
// switch returns, so a post-hoc read cannot see it at all. A 20ms poll cannot
// miss it — the layer waits for the compaction boundary on a 500ms ticker, so
// the intermediate is live for at least that long.
func watchForkedChildren(client rafikiv1connect.ControlClient) (stop func() map[string]string) {
	var (
		mu   sync.Mutex
		seen = map[string]string{}
	)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-quit:
				return
			case <-time.After(20 * time.Millisecond):
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := client.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
			cancel()
			if err != nil {
				continue
			}
			mu.Lock()
			for _, ch := range resp.Msg.GetChildren() {
				if from := ch.GetLabels()["rafiki/forked-from"]; from != "" {
					seen[ch.GetChildId()] = from
				}
			}
			mu.Unlock()
		}
	}()
	return func() map[string]string {
		close(quit)
		<-done
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]string, len(seen))
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

// TestCLICompactForkLeavesTheSourceRunning is the design's central claim on the
// create path: an over-window fork is compacted on the source's model through a
// temporary helper child, the fork's borrowed history is the COMPACTED result,
// and the source is neither mutated nor closed.
func TestCLICompactForkLeavesTheSourceRunning(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	seat := newForkSeat(t)
	// The source's turns report more prompt tokens than the destination's
	// window can hold, which is what makes the fork over-window.
	seat.reportUsage(sourceForkTokens)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	name := "compact-fork-" + uniqueSuffix()
	src := forkSourceWithTurns(t, client, seat, name, 3)

	out, err := cliCmd(t, d, "create", "-d", "--no-local-executor",
		"--fork-from", name, "--model", tinyForkModel, "--compact").CombinedOutput()
	c.NoError(err, "create --compact failed: %v\noutput: %s", err, out)
	forked := childIDFromCreateOutput(t, out)

	// The source is untouched: still live, still listed.
	listed := false
	for _, ch := range listChildren(t, client) {
		if ch.GetChildId() == src {
			listed = true
		}
	}
	c.True(listed, "the source must still be live after a compact fork; output: %s", out)

	// The borrowed history is the compacted one: a boundary is what the
	// compaction wrote, and a fork of the raw transcript would have none.
	c.True(hasCompactionBoundary(getHistoryEvents(t, client, forked)),
		"the fork's history carries no compaction boundary")

	// And it runs on the requested destination.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	get, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: forked}))
	c.Require().NoError(err, "get the compact fork")
	c.StrContains(get.Msg.GetChild().GetModel(), tinyForkModel, "the fork is not on the requested model")

	killChildren(t, client, forked, src)
}

// TestCLICompactSwitchClosesTheSource pins the layering: the compact layer
// never closes the source, and `models switch` does — once, after the fork
// exists. The provenance chain is S → I → N, so the switched child names the
// INTERMEDIATE in rafiki/forked-from, not the source.
func TestCLICompactSwitchClosesTheSource(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	seat := newForkSeat(t)
	seat.reportUsage(sourceForkTokens)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	name := "compact-switch-" + uniqueSuffix()
	src := forkSourceWithTurns(t, client, seat, name, 3)

	// The intermediate has to be read while it is live (see
	// watchForkedChildren): the switch closes it before it returns.
	stopWatch := watchForkedChildren(client)
	out, err := cliCmd(t, d, "models", "switch", name, tinyForkModel, "--compact").CombinedOutput()
	watched := stopWatch()
	c.NoError(err, "models switch --compact failed: %v\noutput: %s", err, out)

	// N: the survivor answering to the source's name.
	newID := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && newID == "" {
		for _, ch := range listChildren(t, client) {
			if ch.GetName() == name && ch.GetChildId() != src {
				newID = ch.GetChildId()
			}
		}
		if newID == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	c.Require().NotEq("", newID, "no child inherited the source's name %q; output: %s", name, out)

	// The source is gone from the live list (closed, not merely stopped).
	for _, ch := range listChildren(t, client) {
		c.NotEq(src, ch.GetChildId(), "the source child is still listed after the switch")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	get, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: newID}))
	c.Require().NoError(err, "get the switched child")

	intermediate := get.Msg.GetChild().GetLabels()["rafiki/forked-from"]
	c.Require().NotEq("", intermediate, "the switched child names no fork source")
	c.NotEq(src, intermediate, "the switch forked the source directly: nothing was compacted")
	c.Eq(src, watched[intermediate],
		"the intermediate %q must itself be a fork of the source %q (seen while live: %v)", intermediate, src, watched)

	c.True(hasCompactionBoundary(getHistoryEvents(t, client, newID)),
		"the switched history carries no compaction boundary")

	killChildren(t, client, newID)
}

// TestCLICompactRefusalNamesBothNumbers pins the refusal: an over-window fork
// without --compact is refused with BOTH numbers, and nothing is spawned — the
// flag is explicit, so no path compacts on its own.
func TestCLICompactRefusalNamesBothNumbers(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	seat := newForkSeat(t)
	seat.reportUsage(sourceForkTokens)
	d := forkDaemon(t, seat)
	client := d.connectClient()

	name := "compact-refuse-" + uniqueSuffix()
	src := forkSourceWithTurns(t, client, seat, name, 3)
	before := childIDSet(t, client)

	out, err := cliCmd(t, d, "create", "-d", "--no-local-executor",
		"--fork-from", name, "--model", tinyForkModel).CombinedOutput()
	c.Require().Error(err, "an over-window fork without --compact must be refused; output: %s", out)
	c.StrContains(string(out), fmt.Sprintf("~%d tokens", sourceForkTokens),
		"the refusal must name the borrowed size; output: %s", out)
	c.StrContains(string(out), fmt.Sprintf("window is %d", tinyForkWindow),
		"the refusal must name the window; output: %s", out)
	c.StrContains(string(out), tinyForkModel, "the refusal must name the destination; output: %s", out)
	c.StrContains(string(out), "--compact", "the refusal must say what to pass; output: %s", out)

	c.EqDiff(before, childIDSet(t, client), "the refusal must not spawn a child")
	killChildren(t, client, src)
}
