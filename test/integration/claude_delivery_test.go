// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end proof of the claude delivery acknowledgement: a REAL daemon
// subprocess launches a REAL child process (the fakeclaude fixture), writes a
// real stream-json user frame into it, PARSES the echo the child writes back
// with the real claudeProvider, resolves the daemon-minted uuid through
// Controller.claudeDeliveries, appends a durable event and streams it to a real
// Connect subscriber.
//
// That chain is the whole point of this file. Every other layer of the
// acknowledgement has a unit test that injects the event it needs (the registry
// in cmd/rafikid, the parse in pkg/child, the ledger in pkg/tui), and all of
// them stay green if the seam between them is broken — an uninstalled callback,
// a uuid written into the wrong field, an echo the parser classifies as
// content. So nothing here reaches the bus directly: the only way an event
// appears is by a real child process writing a real stdout frame.
//
// The fixture (test/integration/fixtures/fakeclaude) is a controlled stand-in
// for the binary: it echoes every accepted message the way 2.1.290 does
// (isReplay:true, the uuid returned verbatim), replays a previous launch's
// uuid on --resume, can echo a message twice, and REFUSES to run without
// --replay-user-messages so no test here can pass against a launch shape that
// cannot acknowledge anything.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── harness ─────────────────────────────────────────────────────────────────

var (
	fakeClaudeOnce sync.Once
	fakeClaudePath string
	fakeClaudeErr  error
)

// buildFakeClaude builds the fixture once per test binary and returns its path.
// Every fixture source is already in listBuildInputs (the walk over
// test/integration/fixtures), so an edit invalidates the cached test result.
func buildFakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeclaude-build")
		if err != nil {
			fakeClaudeErr = err
			return
		}
		out := filepath.Join(dir, "fakeclaude")
		// Built from this package's directory, the same way buildMCPFixture
		// builds ./fixtures/mcpstdio: the test binary's cwd is test/integration.
		cmd := exec.Command("go", "build", "-o", out, "./fixtures/fakeclaude")
		if b, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeErr = fmt.Errorf("build fakeclaude: %v\n%s", err, b)
			return
		}
		fakeClaudePath = out
	})
	assert.NewAborting(t).NoError(fakeClaudeErr, "build the fakeclaude fixture")
	return fakeClaudePath
}

// claudeDeliveryDaemon boots a DB-backed daemon whose claude children are the
// fakeclaude fixture, and returns it with the fixture's observation directory.
//
// Executors are off, so a claude spawn takes the local-subprocess path the
// fixture observes (the same topology bootMCPChildDaemon uses). The fixture's
// knobs ride the daemon's environment, which buildEnv passes to every child.
func claudeDeliveryDaemon(t *testing.T, env ...string) (*daemon, string) {
	t.Helper()
	events := t.TempDir()
	extra := append([]string{
		"RAFIKI_EXECUTORS_ENABLED=0",
		"CLAUDE_BINARY=" + buildFakeClaude(t),
		"FAKE_CLAUDE_DIR=" + events,
	}, env...)
	d := bootDaemonDB(t, nextDaemonID(), extra...)
	t.Cleanup(func() {
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})
	return d, events
}

// fakeEvent is one observation line the fixture wrote.
type fakeEvent struct {
	Event    string          `json:"event"`
	UUID     string          `json:"uuid"`
	Text     string          `json:"text"`
	MidTurn  bool            `json:"mid_turn"`
	Argv     []string        `json:"argv"`
	Messages json.RawMessage `json:"messages"`
	Pid      int             `json:"pid"`
}

// fakeEvents reads every observation line the fixture has written so far, in
// file order. A line is written with one open/close per event, so a torn read
// is a partial FINAL line, which is skipped rather than decoded.
func fakeEvents(t *testing.T, dir string) []fakeEvent {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	out := make([]fakeEvent, 0, 8)
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev fakeEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			out = append(out, ev)
		}
	}
	return out
}

// waitFakeEvent blocks until the fixture has written an event matching
// predicate, and returns it. It is a barrier on the CHILD's own observation
// log, never a sleep: the test only proceeds once the child has actually done
// the thing it is about to assert on.
func waitFakeEvent(t *testing.T, dir, what string, predicate func(fakeEvent) bool) fakeEvent {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range fakeEvents(t, dir) {
			if predicate(ev) {
				return ev
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the fake claude child to %s; observed:\n%s", what, dumpFakeEvents(t, dir))
	return fakeEvent{}
}

func dumpFakeEvents(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range fakeEvents(t, dir) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(&sb, "  %s\n", b)
	}
	if sb.Len() == 0 {
		return "  (none)"
	}
	return sb.String()
}

// spawnClaudeChild spawns one claude child on the fake fixture and waits for it
// to be idle (claude readiness is process-up, so this is not a turn).
func spawnClaudeChild(t *testing.T, d *daemon) string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:       "/tmp",
		NoSession: true,
		Kind:      protocol.KindClaude,
		Model:     "anthropic/sonnet-latest",
	}))
	c.NoError(err, "spawn claude child")
	c.NotEq("", resp.Msg.GetChildId(), "spawn returned empty childId")
	return resp.Msg.GetChildId()
}

// sendPromptRow sends one PROMPT over Connect and returns the inbox row id the
// daemon assigned it — the id every receipt for that message must name.
func sendPromptRow(t *testing.T, client rafikiv1connect.ControlClient, childID, text string) string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}}}},
	}))
	c.NoError(err, "Send(%q)", text)
	c.NotEq("", resp.Msg.GetMessageId(), "Send returned no inbox row id; nothing could acknowledge it")
	return resp.Msg.GetMessageId()
}

// deliveredEvent matches a durable message_delivered event for childID naming
// messageID ("" matches any row).
func deliveredEvent(childID, messageID string) func(*rafikiv1.Event) bool {
	return func(ev *rafikiv1.Event) bool {
		md := ev.GetMessageDelivered()
		return ev.GetChildId() == childID && md != nil && (messageID == "" || md.GetMessageId() == messageID)
	}
}

// ─── the receipt ─────────────────────────────────────────────────────────────

// TestClaudeDeliveryReceiptNamesTheInboxRow is the end-to-end acknowledgement:
// the row id Send returned is the id the receipt carries, and it arrives from
// the child's own stdout echo — parsed by the real provider, resolved by the
// real registry, appended to the real event log.
func TestClaudeDeliveryReceiptNamesTheInboxRow(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	d, events := claudeDeliveryDaemon(t)
	client := d.control(t)

	childID := spawnClaudeChild(t, d)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))

	const prompt = "hello from the receipt test"
	rowID := sendPromptRow(t, client, childID, prompt)

	ev := stream.waitEvent(t, deliveredEvent(childID, rowID), 60*time.Second)
	c.Eq(rowID, ev.GetMessageDelivered().GetMessageId(), "the receipt names the row Send returned")
	c.Require().NotNil(ev.Ordinal, "the receipt must be DURABLE (it replays); got an event with no ordinal")

	// The child's own observation log proves the receipt came from the echo and
	// not from anything the daemon invented: the uuid is one the child was
	// handed, and it names exactly the message the receipt names.
	echo := waitFakeEvent(t, events, "echo the sent message", func(e fakeEvent) bool {
		return e.Event == "echo" && e.Text == prompt
	})
	c.NotEq("", echo.UUID, "the fixture echoed no uuid; the daemon wrote a frame without one")

	// Nothing was invented for the child beyond what it earned: the echo adds no
	// transcript row of its own. A claude child's rows come from the capture, and
	// this fixture posts nothing in echo mode, so there is no conversation at all
	// — the echo must not have created one, and must not have written a row into
	// one.
	hist, err := client.GetHistory(context.Background(), connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childID}))
	if err == nil {
		for _, e := range hist.Msg.GetEvents() {
			c.False(e.GetUserMessage() != nil, "the echo produced a transcript user row: %v", e)
		}
	} else {
		c.Eq(connect.CodeNotFound, connect.CodeOf(err), "GetHistory for a capture-less claude child")
	}
}

// TestClaudeDeliveryDuplicateEchoPublishesOneReceipt pins the at-least-once
// half: the binary may replay an echo, and the registry's take-and-remove means
// the second copy publishes nothing.
func TestClaudeDeliveryDuplicateEchoPublishesOneReceipt(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	d, events := claudeDeliveryDaemon(t, "FAKE_CLAUDE_ECHO_TIMES=3")
	client := d.control(t)

	childID := spawnClaudeChild(t, d)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))

	rowID := sendPromptRow(t, client, childID, "echo this three times")
	stream.waitEvent(t, deliveredEvent(childID, rowID), 60*time.Second)
	waitFakeEvent(t, events, "write every duplicate echo", func(fakeEvent) bool {
		return countFakeEchoes(t, events) >= 3
	})

	// ORDERING BARRIER, not a sleep: the child's stdout is drained by ONE
	// goroutine, so a receipt for a LATER send proves every earlier frame — all
	// three echoes — has already been parsed and resolved. Only then is counting
	// meaningful.
	laterRow := sendPromptRow(t, client, childID, "a later message proves the echoes were drained")
	stream.waitEvent(t, deliveredEvent(childID, laterRow), 60*time.Second)

	// A single receipt is the contract; a second one would retire another ghost.
	c.Eq(1, countDeliveries(stream, childID, rowID), "a repeated echo must publish exactly one receipt")
}

// countFakeEchoes counts the echo observations the fixture has written.
func countFakeEchoes(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, ev := range fakeEvents(t, dir) {
		if ev.Event == "echo" {
			n++
		}
	}
	return n
}

// countDeliveries counts buffered live receipts naming one row. The stream
// never consumes, so this reads everything the daemon published for the child.
func countDeliveries(stream *eventStream, childID, rowID string) int {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	n := 0
	for _, ev := range stream.events {
		if stream.isLive(ev) && deliveredEvent(childID, rowID)(ev) {
			n++
		}
	}
	return n
}

// TestClaudeDeliveryStaleReplayOnResumePublishesNothing is the §8 restart case:
// a resumed child replays a frame from the RETIRED launch. Its uuid names no
// delivery this launch minted, so nothing may publish — otherwise a resume
// would retire a ghost for a message nobody sent again.
func TestClaudeDeliveryStaleReplayOnResumePublishesNothing(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	d, events := claudeDeliveryDaemon(t)
	client := d.control(t)

	childID := spawnClaudeChild(t, d)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))

	firstRow := sendPromptRow(t, client, childID, "the message the resumed launch will replay")
	stream.waitEvent(t, deliveredEvent(childID, firstRow), 60*time.Second)
	firstEcho := waitFakeEvent(t, events, "echo the first message", func(e fakeEvent) bool {
		return e.Event == "echo" && e.Text == "the message the resumed launch will replay"
	})

	// Kill (resumable) and resume: the new process replays the old uuid before
	// it reads stdin.
	killCtx, killCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer killCancel()
	_, err := client.Kill(killCtx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childID}))
	c.Require().NoError(err, "Kill")
	waitForStatus(t, d, childID, string(protocol.StatusExited), 30*time.Second)

	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer resumeCancel()
	_, err = client.Resume(resumeCtx, connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: childID}))
	c.Require().NoError(err, "Resume")
	waitForStatus(t, d, childID, string(protocol.StatusIdle), 30*time.Second)

	stale := waitFakeEvent(t, events, "replay the retired launch's uuid", func(e fakeEvent) bool {
		return e.Event == "stale_echo"
	})
	c.Eq(firstEcho.UUID, stale.UUID, "the fixture must replay the uuid the previous launch echoed")

	// The resumed launch must publish no receipt for it. Wait for a fresh send to
	// prove the new launch DOES acknowledge (so a silent child cannot pass this),
	// then assert the stale uuid produced nothing.
	secondRow := sendPromptRow(t, client, childID, "a message from the new launch")
	stream.waitEvent(t, deliveredEvent(childID, secondRow), 60*time.Second)

	// Exactly ONE receipt names the first row: the one the first launch earned.
	// A second would be the retired launch's uuid being honoured by the new one,
	// which is the bug this case exists to catch.
	c.Eq(1, countDeliveries(stream, childID, firstRow),
		"the stale replay re-acknowledged the retired launch's row")
}

// TestClaudeDeliveryReceiptReplaysAfterResubscribe pins that the receipt is
// DURABLE, not a live-only courtesy: a subscriber that disconnects and resumes
// from the ordinal it last saw gets the same receipt, naming the same row.
func TestClaudeDeliveryReceiptReplaysAfterResubscribe(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	d, _ := claudeDeliveryDaemon(t)
	client := d.control(t)

	childID := spawnClaudeChild(t, d)

	// First subscription: watch from the child's current tail.
	watermark := eventWatermark(t, client, childID)
	first := d.openEvents(t, childEventsSubject(childID), map[string]int32{childID: watermark}, watermark)
	rowID := sendPromptRow(t, client, childID, "a message that must survive a resubscribe")
	live := first.waitEvent(t, deliveredEvent(childID, rowID), 60*time.Second)
	c.Require().NotNil(live.Ordinal, "the live receipt carries no ordinal")

	// Drop the subscription, then open a NEW one from just BELOW the receipt's
	// ordinal: a cursor is exclusive, so the receipt is inside the replay window
	// a resuming follower asks for.
	first.stream.Close()
	below := live.GetOrdinal() - 1
	second := d.openEvents(t, childEventsSubject(childID), map[string]int32{childID: below}, below)
	replayed := second.waitEvent(t, deliveredEvent(childID, rowID), 60*time.Second)
	c.Eq(rowID, replayed.GetMessageDelivered().GetMessageId(), "the replayed receipt names the same row")
	c.Eq(live.GetOrdinal(), replayed.GetOrdinal(), "the replay is the SAME event, not a second receipt")
}

// TestClaudeDeliveryRejectsTheReplayFlag drives the launch-failure path end to
// end: the fixture refuses the headless flag the way an old binary does, and the
// daemon reports it with the upgrade hint — through the real spawn path, not a
// unit test of the event builder.
func TestClaudeDeliveryRejectsTheReplayFlag(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	d, events := claudeDeliveryDaemon(t, "FAKE_CLAUDE_REJECT_REPLAY=1")
	client := d.control(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/tmp", NoSession: true, Kind: protocol.KindClaude, Model: "anthropic/sonnet-latest",
	}))
	c.Require().NoError(err, "Spawn (the daemon reports the refusal as an event, not an RPC error)")
	childID := resp.Msg.GetChildId()
	c.Require().NotEq("", childID, "spawn returned empty childId")

	stream := d.openEvents(t, childEventsSubject(childID), nil, 0)
	ev := stream.waitEvent(t, func(ev *rafikiv1.Event) bool {
		e := ev.GetError()
		return ev.GetChildId() == childID && e != nil && e.GetCode() == "claude_launch_failed"
	}, 60*time.Second)
	c.StrContains(ev.GetError().GetMessage(), "unknown option", "the error carries the binary's own words")
	c.StrContains(ev.GetError().GetMessage(), "--replay-user-messages", "the error names the flag to upgrade for")

	// The fixture really was launched with the flag and refused it deliberately:
	// its own launch record is the proof, and it keeps this test from passing on
	// a binary that merely crashed for another reason.
	launch := waitFakeEvent(t, events, "record its launch", func(e fakeEvent) bool { return e.Event == "launch" })
	c.Contains(launch.Argv, "--replay-user-messages", "the daemon must launch headless claude with the replay flag")
}

// TestClaudeDeliveryReceiptPrecedesAFailedCapture is the ordering claim on the
// FAILURE path: the receipt arrives while the turn's own /v1/messages request
// is still blocked, and the capture's report — a failed turn — is a strictly
// later event.
//
// The block is real, not simulated: the fixture holds the request until the
// test creates a gate file, and the request it eventually sends is one the
// proxy refuses (a provider-qualified model with no OpenRouter key), so the
// whole capture path runs — begin, report, gate release, turn_failed — without
// a network or an upstream. Its SUCCESS-path sibling, with a real upstream and
// a real capture, is TestClaudeDeliveryCapturePublishesOnlyAfterTheReceipt.
func TestClaudeDeliveryReceiptPrecedesAFailedCapture(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)

	gate := filepath.Join(t.TempDir(), "release-request")
	d, events := claudeDeliveryDaemon(t,
		"FAKE_CLAUDE_TURNS=1",
		"FAKE_CLAUDE_REQUEST_MODEL=fakellm/mini",
		"FAKE_CLAUDE_HOLD_REQUEST="+gate,
	)
	client := d.control(t)
	childID := spawnClaudeChild(t, d)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))

	const prompt = "a message whose request is still blocked"
	rowID := sendPromptRow(t, client, childID, prompt)

	// The fixture is parked in front of its request: the capture cannot have
	// happened, because nothing has reached the proxy.
	waitFakeEvent(t, events, "park in front of its request", func(e fakeEvent) bool {
		return e.Event == "holding_request"
	})

	// The receipt arrives anyway — it is published from the child's stdout, not
	// from the capture.
	ev := stream.waitEvent(t, deliveredEvent(childID, rowID), 60*time.Second)
	c.Eq(rowID, ev.GetMessageDelivered().GetMessageId(), "the receipt names the row Send returned")

	// ...and nothing from the capture has been published yet.
	stream.mu.Lock()
	for _, e := range stream.events {
		c.False(e.GetUserMessage() != nil, "a transcript row appeared while the request was still blocked: %v", e)
		c.False(e.GetError() != nil, "the turn reported an error before its request was released: %v", e)
	}
	stream.mu.Unlock()

	// Release the capture. It fails upstream (no OpenRouter key), and the
	// daemon reports the failed turn — which proves the report is a LATER event
	// than the receipt, i.e. the acknowledgement never waited for it.
	c.NoError(os.WriteFile(gate, nil, 0o600), "release the held request")
	failed := stream.waitEvent(t, func(e *rafikiv1.Event) bool {
		er := e.GetError()
		return e.GetChildId() == childID && er != nil && er.GetCode() == "turn_failed"
	}, 90*time.Second)
	c.Require().NotNil(failed.GetOrdinal(), "the turn failure must be durable")
	// Less(want, got) asserts got < want: read it as "less than the report".
	c.Less(failed.GetOrdinal(), ev.GetOrdinal(),
		"the receipt (ordinal %d) must precede the capture's report (ordinal %d)", ev.GetOrdinal(), failed.GetOrdinal())

	// The failed turn stores no row, and the echo stored none either: the
	// acknowledged message exists as a receipt and nothing else.
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for _, e := range stream.events {
		c.False(e.GetUserMessage() != nil, "a transcript row appeared for a turn whose capture failed: %v", e)
	}
}

// TestClaudeDeliveryKeepsFundiUntouched is the scope pin: a fundi child has no
// echo and no delivery uuid, so no receipt may appear for one — the seam is
// installed for claude launches only.
func TestClaudeDeliveryKeepsFundiUntouched(t *testing.T) {
	t.Parallel()
	d, _ := claudeDeliveryDaemon(t)
	client := d.control(t)

	childID := d.spawnChild(t)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))
	sendPromptRow(t, client, childID, "fundi has no echo")

	// A fundi child's acknowledgement is its persisted user row, never a
	// receipt. Wait for that row (the positive marker that the send was taken and
	// its turn opened), then assert the claude-only event never appeared.
	stream.waitEvent(t, func(ev *rafikiv1.Event) bool {
		um := ev.GetUserMessage()
		return ev.GetChildId() == childID && um != nil
	}, 60*time.Second)

	stream.mu.Lock()
	defer stream.mu.Unlock()
	for _, ev := range stream.events {
		if md := ev.GetMessageDelivered(); md != nil {
			t.Fatalf("a fundi child published a claude delivery receipt: %v", ev)
		}
	}
}
