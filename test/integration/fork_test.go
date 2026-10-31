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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
}

func newForkSeat(t *testing.T) *forkSeat {
	t.Helper()
	s := &forkSeat{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
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
	marker, tool, input := s.marker, s.tool, s.input
	s.mu.Unlock()

	text := string(body)
	switch {
	case strings.Contains(text, `"tool_result"`):
		s.replyText(w, "noted")
	case marker != "" && strings.Contains(text, marker):
		s.replyToolUse(w, tool, input)
	default:
		s.replyText(w, "done")
	}
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

func (s *forkSeat) replyText(w http.ResponseWriter, text string) {
	delta, _ := json.Marshal(text)
	s.writeEvents(w, []string{
		"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_fork","type":"message","role":"assistant","model":"mini","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n",
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

// forkDaemon boots a DB-backed daemon whose only provider is the given seat.
func forkDaemon(t *testing.T, seat *forkSeat) *daemon {
	t.Helper()
	providers := writeRecordingProviders(t, seat.srv.URL)
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
