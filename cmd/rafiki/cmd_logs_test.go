// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// ── the stub daemon ──────────────────────────────────────────────────────────

// stubControl serves the Control RPCs the event commands use and records what
// arrived. Unoverridden RPCs answer Unimplemented via the embed; the call
// counters let a test prove a verb dialled nothing.
type stubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	mu sync.Mutex

	events          []*rafikiv1.Event // GetHistory's response
	streamEvents    []*rafikiv1.Event // what the FIRST StreamEvents call (the replay) sends
	followEvents    []*rafikiv1.Event // what the SECOND call (the follow) sends; nil = streamEvents again
	streamErr       error             // what StreamEvents returns after sending
	streamBlocks    bool              // the replay call blocks until the caller's ctx is cancelled — the real server's contract (it replays, then follows live)
	followBlocks    bool              // the follow call blocks too (by default it ends after its events: the client ends the command on the child's own child_exited)
	scriptLatest    *int32            // GetChild's latest_ordinal for a script child (nil = unset)
	historyNotFound bool              // GetHistory answers NotFound (no conversation)
	getChildOK      bool              // GetChild answers with a named child when set
	scriptChild     bool              // GetChild answers kind=script when set

	streamCtxDone int // StreamEvents invocations ended by the caller cancelling the stream ctx

	historyCalls int
	streamCalls  int
	sendCalls    int
	listCalls    int

	streamReqs []*rafikiv1.StreamEventsRequest // captured StreamEvents requests
	sendFrames []*rafikiv1.SendFrameRequest    // captured SendFrame requests
}

func (s *stubControl) GetHistory(
	_ context.Context,
	req *connect.Request[rafikiv1.GetHistoryRequest],
) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyCalls++
	if s.historyNotFound {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no conversation for child"))
	}
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{Events: s.events}), nil
}

func (s *stubControl) StreamEvents(
	ctx context.Context,
	req *connect.Request[rafikiv1.StreamEventsRequest],
	stream *connect.ServerStream[rafikiv1.Event],
) error {
	s.mu.Lock()
	s.streamCalls++
	s.streamReqs = append(s.streamReqs, req.Msg)
	events := append([]*rafikiv1.Event(nil), s.streamEvents...)
	streamErr := s.streamErr
	blocks := s.streamBlocks
	if s.streamCalls > 1 && s.followEvents != nil {
		events = append([]*rafikiv1.Event(nil), s.followEvents...)
		blocks = s.followBlocks
	}
	s.mu.Unlock()

	// The server applies the request's Types server-side (filter.Match in the
	// stream's follow loop): an event of a type the request did not ask for
	// is never sent. Simulating that is what makes a wrong type filter drop
	// lines here the way the real daemon does.
	if len(req.Msg.GetTypes()) > 0 {
		want := make(map[string]bool, len(req.Msg.GetTypes()))
		for _, t := range req.Msg.GetTypes() {
			want[t] = true
		}
		filtered := make([]*rafikiv1.Event, 0, len(events))
		for _, ev := range events {
			if want[eventlog.TypeName(ev)] {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
	}

	for _, ev := range events {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	if blocks {
		// The real server replays, then FOLLOWS live until the stream ctx is
		// cancelled — it never ends the stream on its own (for an exited script
		// Subscribe creates a fresh bus nobody closes). A stub that ends here
		// gives the client a contract the server does not.
		<-ctx.Done()
		s.mu.Lock()
		s.streamCtxDone++
		s.mu.Unlock()
		return ctx.Err()
	}
	if streamErr != nil {
		return streamErr
	}
	return nil
}

func (s *stubControl) GetChild(
	_ context.Context,
	req *connect.Request[rafikiv1.GetChildRequest],
) (*connect.Response[rafikiv1.GetChildResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getChildOK {
		return connect.NewResponse(&rafikiv1.GetChildResponse{Child: &rafikiv1.ChildSummary{
			ChildId: req.Msg.GetChildId(), Name: "seeded-" + req.Msg.GetChildId(), Status: "idle",
		}}), nil
	}
	if s.scriptChild {
		return connect.NewResponse(&rafikiv1.GetChildResponse{Child: &rafikiv1.ChildSummary{
			ChildId: req.Msg.GetChildId(), Name: "seeded-" + req.Msg.GetChildId(), Kind: "script", Status: "running",
			LatestOrdinal: s.scriptLatest,
		}}), nil
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("no child source in this stub"))
}

func (s *stubControl) SendFrame(
	_ context.Context,
	req *connect.Request[rafikiv1.SendFrameRequest],
) (*connect.Response[rafikiv1.SendFrameResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendCalls++
	s.sendFrames = append(s.sendFrames, req.Msg)
	return connect.NewResponse(&rafikiv1.SendFrameResponse{}), nil
}

func (s *stubControl) ListChildren(
	_ context.Context,
	_ *connect.Request[rafikiv1.ListChildrenRequest],
) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return connect.NewResponse(&rafikiv1.ListChildrenResponse{}), nil
}

// serveStubControl isolates profiles, then serves stub on a profile's own
// socket — the one path the client dials — so a test exercises
// newConnectEndpoint's real dial path.
func serveStubControl(t *testing.T, stub *stubControl) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// A short directory, not t.TempDir(): unix socket paths are capped at
	// ~104 bytes (sizeof sun_path on darwin), and t.TempDir() nests under the
	// full test name, which alone can exceed that.
	dir, err := os.MkdirTemp("", "e4")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
}

// serveConnectOnUnixSocket serves a Connect handler over an h2c unix socket at
// path, matching the client half in connectclient.go's connectHTTPClient
// (AllowHTTP h2 over a plain unix dial).
func serveConnectOnUnixSocket(t *testing.T, path string, handlerPath string, handler http.Handler) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	assert.NewAborting(t).NoError(err, "listen %s", path)
	mux := http.NewServeMux()
	mux.Handle(handlerPath, handler)
	protos := &http.Protocols{}
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protos}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})
}

// runBounded runs a command with its production context (Background — no
// deadline) and fails the test if it has not returned within the bound: the
// script replay tests pin a HANG, so a deadline on the command's own context
// would mask the bug by "returning" at the deadline with everything collected.
func runBounded(t *testing.T, cmd *cobra.Command, args ...string) (out, notes string) {
	t.Helper()
	type result struct {
		out, notes string
		err        error
	}
	ch := make(chan result, 1)
	go func() {
		out, notes, err := runCmd(t, cmd, args...)
		ch <- result{out, notes, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("command failed: %v\nstdout:\n%s\nnotes:\n%s", r.err, r.out, r.notes)
		}
		return r.out, r.notes
	case <-time.After(10 * time.Second):
		t.Fatal("the command hung — the replay was never bounded")
		return "", ""
	}
}

// waitStreamCancel waits for a blocking stub's StreamEvents handler to observe
// the client's ctx cancel and return — the client's cancel races the test's
// assertions.
func waitStreamCancel(t *testing.T, stub *stubControl) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stub.mu.Lock()
		done := stub.streamCtxDone
		stub.mu.Unlock()
		if done > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stub StreamEvents was never cancelled by the caller")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runCmd executes a command in-process and returns what it wrote to its
// stdout/stderr (the stderr buffer is where the engine's notes go).
func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

// userMessageEvent builds a durable user_message with the given ordinal.
func userMessageEvent(id string, ordinal int32, text string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: id,
		Ordinal: proto.Int32(ordinal),
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0, Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
		}},
	}
}

// ── logs ─────────────────────────────────────────────────────────────────────

func TestLogsFlagDefaults(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newLogsCmd()
	n, err := cmd.Flags().GetInt("tail")
	c.Require().False(err != nil || n != -1, "logs tail default = %d (err %v), want -1 (all)", n, err)
	if f, err := cmd.Flags().GetBool("follow"); err != nil || f {
		t.Fatalf("logs follow default = %v (err %v), want false", f, err)
	}
	if raw, err := cmd.Flags().GetBool("raw"); err != nil || raw {
		t.Fatalf("logs raw default = %v (err %v), want false", raw, err)
	}
	for _, flag := range []string{"stdin", "stderr", "all", "path", "types", "all-types"} {
		c.NotNil(cmd.Flags().Lookup(flag), "logs is missing the %q flag", flag)
	}
	if !containsAlias(cmd.Aliases, "log") {
		t.Errorf("logs lost its 'log' alias: %v", cmd.Aliases)
	}
}

// The flags the framed plane needed are gone, not merely defaulted.
func TestLogsRemovedFlagsAreGone(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newLogsCmd()
	for _, flag := range []string{"profile", "include", "exclude", "no-deltas", "verbose"} {
		c.Nil(cmd.Flags().Lookup(flag), "logs still declares the retired %q flag", flag)
	}
	cmd = newTailCmd()
	for _, flag := range []string{"profile", "include", "exclude", "no-deltas", "verbose"} {
		c.Nil(cmd.Flags().Lookup(flag), "tail still declares the retired %q flag", flag)
	}
}

// The snapshot flags refuse to combine — cobra validates the group before
// RunE, so no dial happens on a bad flag set.
func TestLogsSnapshotFlagsAreMutuallyExclusive(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	cmd := newLogsCmd()
	_, _, err := runCmd(t, cmd, "--stdin", "--stderr", "c_1")
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "stdin"), "logs --stdin --stderr = %v, want cobra's mutually-exclusive error naming the group", err)
}

// --types is validated against the native vocabulary before anything dials:
// an unknown name errors instead of silently selecting nothing.
func TestLogsRejectsAnUnknownEventType(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	cmd := newLogsCmd()
	_, _, err := runCmd(t, cmd, "--types", "message_end", "c_1")
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "unknown event type"), "logs --types message_end = %v, want an unknown-type error", err)
}

// logs prints the history served by the profile's OWN socket — the pin
// that used to hold for `history` holds for its replacement: a socket
// profile's logs must not query whatever daemon happens to listen elsewhere.
func TestLogsReachesTheSocketProfilesOwnDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &stubControl{events: []*rafikiv1.Event{
		userMessageEvent("c_1", 0, "served by the profile's own socket"),
	}}
	serveStubControl(t, stub)

	cmd := newLogsCmd()
	out, _, err := runCmd(t, cmd, "c_1")
	c.NoError(err, "rafiki logs")
	c.Eq(1, stub.historyCalls, "GetHistory called")
	c.StrContains(out, "served by the profile's own socket", "logs output does not show the event served by the profile's OWN socket:\n")
	c.StrContains(out, "user_message", "logs output lost the event's type column:\n")
}

// logs -f prints the history and then follows with a cursor just after the
// last history ordinal — the resume point the brief pins.
func TestLogsFollowResumesFromTheLastHistoryOrdinal(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &stubControl{
		getChildOK: true,
		events: []*rafikiv1.Event{
			userMessageEvent("c_1", 0, "first"),
			userMessageEvent("c_1", 1, "second"),
			userMessageEvent("c_1", 2, "third"),
		},
		streamEvents: []*rafikiv1.Event{statusFor("c_1", "idle")},
	}
	serveStubControl(t, stub)

	cmd := newLogsCmd()
	out, notes, err := runCmd(t, cmd, "-f", "c_1")
	c.Require().NoError(err, "rafiki logs -f")
	if stub.historyCalls != 1 || stub.streamCalls != 1 {
		t.Fatalf("calls = history %d, stream %d; want 1 and 1", stub.historyCalls, stub.streamCalls)
	}

	req := stub.streamReqs[0]
	c.Eq(2, req.GetCursor().GetOrdinals()["c_1"], "cursor ordinal for c_1")
	// Single-child subject, conversation+lifecycle types, durable tier.
	c.Eq("c_1", req.GetSubject().GetChild(), "subject = %+v, want child c_1", req.GetSubject().GetScope())
	for _, want := range []string{"agent_status", "user_message", "compaction_boundary", "child_exited"} {
		c.StrContains(strings.Join(req.GetTypes(), ","), want, "default single-child types missing %q: %v", want, req.GetTypes())
	}
	for _, banned := range []string{"content_block_delta", "message_update"} {
		for _, got := range req.GetTypes() {
			c.NotEq(banned, got, "default types include")
		}
	}
	c.Eq(rafikiv1.EventTier_EVENT_TIER_DURABLE, req.GetTier(), "tier")

	// The backfill printed all three history events, then the live event.
	for _, want := range []string{"first", "second", "third", "idle"} {
		c.StrContains(out, want, "output missing")
	}
	c.NotStrContains(notes, "unavailable", "unexpected notes")
}

// A child with no conversation is empty history, not an error: logs -f must
// still follow, with no cursor (there is no history to resume from).
func TestLogsFollowSurvivesAMissingConversation(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &stubControl{
		historyNotFound: true,
		streamEvents:    []*rafikiv1.Event{statusFor("c_1", "streaming")},
	}
	serveStubControl(t, stub)

	cmd := newLogsCmd()
	out, _, err := runCmd(t, cmd, "-f", "c_1")
	c.Require().NoError(err, "rafiki logs -f on a child with no conversation")
	c.StrContains(out, "streaming", "follow output missing the live event:\n")
	if req := stub.streamReqs[0]; req.GetCursor() != nil {
		t.Errorf("empty history must not carry a cursor, got %+v", req.GetCursor())
	}
}

// The single-child tail backfills 20 by default and exits on the child's own
// child_exited event.
func TestTailChildBackfillsAndExitsOnChildExited(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &stubControl{
		events: []*rafikiv1.Event{
			userMessageEvent("c_1", 0, "hello"),
			userMessageEvent("c_1", 1, "again"),
		},
		streamEvents: []*rafikiv1.Event{exitedFor("c_1", proto32(0), "")},
	}
	serveStubControl(t, stub)

	cmd := newTailCmd()
	out, _, err := runCmd(t, cmd, "c_1")
	c.Require().NoError(err, "rafiki tail c_1")
	if stub.historyCalls != 1 || stub.streamCalls != 1 {
		t.Fatalf("calls = history %d, stream %d; want 1 and 1", stub.historyCalls, stub.streamCalls)
	}
	req := stub.streamReqs[0]
	c.Eq(1, req.GetCursor().GetOrdinals()["c_1"], "cursor ordinal")
	for _, want := range []string{"hello", "again", "exit"} {
		c.StrContains(out, want, "output missing")
	}
}

// ── script children: the event log IS the transcript ────────────────────────

func scriptOutputEvent(id string, ordinal int32, stream, text string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: id,
		Ordinal: proto.Int32(ordinal),
		Payload: &rafikiv1.Event_ScriptOutput{ScriptOutput: &rafikiv1.ScriptOutput{
			Stream: stream, Text: text,
		}},
	}
}

// A script child's output renders as its text, with stderr lines prefixed
// `stderr| ` — plain, no colour.
func TestLogsRendersScriptOutput(t *testing.T) {
	c := assert.NewAborting(t)
	r := newEventRenderer()
	got := r.observe(withTS(scriptOutputEvent("c_s", 3, "stdout", "line one"), atClock(9, 0, 1.0)), atClock(9, 0, 1.0))
	c.StrContains(got, "line one", "stdout line missing from the rendered view:\n")
	c.NotStrContains(got, "stderr|", "stdout must not carry the stderr prefix:\n")

	got = r.observe(withTS(scriptOutputEvent("c_s", 4, "stderr", "boom: no such file"), atClock(9, 0, 2.0)), atClock(9, 0, 2.0))
	c.StrContains(got, "stderr| boom: no such file", "stderr line missing its stderr| prefix:\n")
}

// A script child has no conversation, so `logs` backfills from the DURABLE
// EVENT LOG (StreamEvents replay from ordinal 0), never GetHistory — a script
// has no conversation rows, and GetHistory's NotFound would otherwise land in
// the "no history yet" dead end for a child whose whole output is in the log.
// The stub's StreamEvents BLOCKS after its events until the caller cancels,
// the real server's contract — so a client that reads until the stream ends
// hangs, and this test can only pass if the replay is bounded by the child's
// latest_ordinal watermark.
func TestLogsScriptBackfillsFromEventLog(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &stubControl{
		scriptChild:     true,
		historyNotFound: true,
		scriptLatest:    proto.Int32(2),
		streamBlocks:    true,
		streamEvents: []*rafikiv1.Event{
			scriptOutputEvent("c_s", 0, "stdout", "building..."),
			scriptOutputEvent("c_s", 1, "stderr", "warn: slow disk"),
			exitedFor("c_s", proto32(2), ""),
		},
	}
	serveStubControl(t, stub)

	out, notes := runBounded(t, newLogsCmd(), "c_s")
	c.Eq(0, stub.historyCalls, "GetHistory must never be dialled for a script child")
	c.Eq(1, stub.streamCalls, "StreamEvents called once, for the replay")
	c.StrContains(out, "building...", "script_output backfill missing from logs:\n")
	c.StrContains(out, "stderr| warn: slow disk", "stderr output missing its prefix:\n")
	c.NotStrContains(notes, "no history yet", "the script dead end leaked through:\n")

	// The replay read until the watermark, then the client cancelled the
	// stream — the server was following live and would never end it itself.
	waitStreamCancel(t, stub)
	c.Eq(1, stub.streamCtxDone, "the replay stream must be cancelled once the watermark is reached")

	// The replay request: from the log head at DURABLE tier, and NO Types —
	// latest_ordinal is the highest ordinal of ANY durable type, so a
	// server-side filter could hide the event that reaches the watermark and
	// hang the replay; the transcript cut is client-side instead. (The stub
	// applies Types server-side like the real daemon, so the empty Types is
	// also what lets this replay see all three events.)
	req := stub.streamReqs[0]
	c.Eq(int32(-1), req.GetCursor().GetOrdinals()["c_s"], "replay must start at ordinal -1 (from the log head)")
	c.Eq(0, len(req.GetTypes()), "the replay must not filter server-side, got %v", req.GetTypes())
	c.Eq(rafikiv1.EventTier_EVENT_TIER_DURABLE, req.GetTier(), "replay tier")
	c.Eq("c_s", req.GetSubject().GetChild(), "replay subject")
}

// The follow phase of a script child's logs/tail must be able to show LIVE
// script output: the subject-shaped default filter (conversation types +
// lifecycle) contains neither script_output nor script_report, so with it on
// the follow request the server drops every live script line and the command
// shows nothing after the backfill until the child exits. With no explicit
// --types/--all-types the follow therefore carries the replay vocabulary —
// the types a script child actually produces — and child_exited with them,
// so the follow still ends when the script does.
func TestLogsScriptFollowCarriesTheReplayTypes(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &stubControl{
		scriptChild:  true,
		scriptLatest: proto.Int32(1),
		streamBlocks: true,
		streamEvents: []*rafikiv1.Event{
			scriptOutputEvent("c_s", 0, "stdout", "backfill line"),
			scriptOutputEvent("c_s", 1, "stdout", "watermark line"),
		},
		followEvents: []*rafikiv1.Event{
			// Live, past the watermark: only a follow whose Types admit
			// script_output can show it — the stub filters server-side.
			scriptOutputEvent("c_s", 2, "stdout", "live line"),
			exitedFor("c_s", proto32(0), ""),
		},
	}
	serveStubControl(t, stub)

	out, notes := runBounded(t, newLogsCmd(), "-f", "c_s")
	c.StrContains(out, "backfill line", "backfill missing from logs -f:\n")
	c.StrContains(out, "live line", "the follow dropped the live script_output — its Types cannot show a script transcript:\n")
	c.NotStrContains(notes, "no history yet", "the script dead end leaked through:\n")

	// The follow request's Types admit the transcript — and carry neither of
	// the conversation types that would have dropped it.
	c.Require().Eq(2, stub.streamCalls, "calls = replay + follow")
	follow := stub.streamReqs[1]
	for _, want := range []string{"script_output", "script_report"} {
		c.StrContains(strings.Join(follow.GetTypes(), ","), want, "follow types missing %q: %v", want, follow.GetTypes())
	}
	for _, banned := range []string{"user_message", "assistant_message", "compaction_boundary"} {
		for _, got := range follow.GetTypes() {
			c.NotEq(banned, got, "the script follow must not carry the conversation default")
		}
	}
	c.Eq(int32(1), follow.GetCursor().GetOrdinals()["c_s"], "the follow resumes from the watermark")
	c.Eq(rafikiv1.EventTier_EVENT_TIER_DURABLE, follow.GetTier(), "follow tier")

	// The replay was bounded by the watermark and cancelled; the follow ended
	// on the child's own child_exited (the command returned above).
	waitStreamCancel(t, stub)
	c.Eq(1, stub.streamCtxDone, "the replay stream must be cancelled once the watermark is reached")
}

// A running script (no child_exited yet) stops its replay at the watermark —
// latest_ordinal — rather than reading forever, and the client cancels the
// stream once there.
func TestLogsScriptReplayStopsAtWatermarkWhileRunning(t *testing.T) {
	c := assert.NewAborting(t)
	st := &stubControl{
		scriptChild:  true,
		scriptLatest: proto.Int32(2),
		streamBlocks: true,
		streamEvents: []*rafikiv1.Event{
			scriptOutputEvent("c_s", 0, "stdout", "building..."),
			scriptOutputEvent("c_s", 1, "stdout", "still going"),
			// An event past the watermark: the replay must stop at 2, before
			// reading this, and leave it to the follow phase.
			scriptOutputEvent("c_s", 3, "stdout", "past the watermark"),
		},
	}
	serveStubControl(t, st)

	out, _ := runBounded(t, newLogsCmd(), "c_s")
	c.StrContains(out, "building...", "replay missing")
	c.NotStrContains(out, "past the watermark", "events past the watermark belong to the follow, not the backfill\n")
	waitStreamCancel(t, st)
	c.Eq(1, st.streamCtxDone, "the replay stream must be cancelled at the watermark")
}

// A script child with NO latest_ordinal has nothing to replay: the replay
// call is skipped entirely, and `logs` returns instead of hanging.
func TestLogsScriptWithoutLatestOrdinalSkipsTheReplay(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &stubControl{
		scriptChild:  true,
		streamBlocks: true,
		streamEvents: []*rafikiv1.Event{scriptOutputEvent("c_s", 0, "stdout", "late output")},
	}
	serveStubControl(t, stub)

	out, _ := runBounded(t, newLogsCmd(), "c_s")
	c.Eq(0, stub.streamCalls, "nothing to replay — StreamEvents must not be dialled")
	c.Eq("", out, "no replay, no output")
}

// latest_ordinal 0 is a SET watermark, not an unset one: ordinals start at 0
// (the eventlog.Store contract), so a log whose only event is ordinal 0 is a
// real log. Conflating 0 with "unset" skips the replay entirely, leaves the
// cursor nil, and the event is lost. (The stub blocks after its events, so
// the replay is only bounded if the watermark stop runs.)
func TestLogsScriptZeroOrdinalReplays(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &stubControl{
		scriptChild:  true,
		scriptLatest: proto.Int32(0),
		streamBlocks: true,
		streamEvents: []*rafikiv1.Event{scriptOutputEvent("c_s", 0, "stdout", "the only event")},
	}
	serveStubControl(t, stub)

	out, notes := runBounded(t, newLogsCmd(), "c_s")
	c.Eq(1, stub.streamCalls, "a SET latest_ordinal of 0 must still run the replay")
	c.StrContains(out, "the only event", "ordinal 0 is missing from the replay:\n")
	c.NotStrContains(notes, "no history yet", "a one-event log is not an empty one:\n")
	waitStreamCancel(t, stub)
	c.Eq(1, stub.streamCtxDone, "the replay must still be bounded at the watermark")
}

// The replay must observe every ordinal up to the watermark, whatever type
// the events carry: latest_ordinal is the highest ordinal of ANY durable
// type, so a replay that filters server-side to the transcript vocabulary
// would never see the event that reaches the watermark on a log whose newest
// entry is another durable type — and would hang reading live forever. The
// client-side cut still keeps that type out of the printed view.
func TestLogsScriptReplaySeesANonScriptWatermarkEvent(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &stubControl{
		scriptChild:  true,
		scriptLatest: proto.Int32(2),
		streamBlocks: true,
		streamEvents: []*rafikiv1.Event{
			scriptOutputEvent("c_s", 0, "stdout", "building..."),
			scriptOutputEvent("c_s", 1, "stdout", "still going"),
			// A durable type outside the replay vocabulary, AT the
			// watermark: the replay must stop here and must not print it.
			userMessageEvent("c_s", 2, "a conversation line at the watermark"),
		},
	}
	serveStubControl(t, stub)

	out, _ := runBounded(t, newLogsCmd(), "c_s")
	c.Eq(0, len(stub.streamReqs[0].GetTypes()), "the replay must not filter server-side, got %v", stub.streamReqs[0].GetTypes())
	c.StrContains(out, "building...", "replay missing:\n")
	c.NotStrContains(out, "a conversation line at the watermark", "a non-transcript type must not print:\n")
	waitStreamCancel(t, stub)
	c.Eq(1, stub.streamCtxDone, "the replay must be bounded at the watermark, not left reading live")
}

// The script follow's type override keys off "the user declared a filter",
// derived by equality with the subject-shaped default (there is no flag to
// carry on eventQuery — the commands resolve the filter before the engine
// runs). Pin the derivation: the defaults read as NOT explicit, every
// declared shape as explicit.
func TestEventQueryTypesExplicit(t *testing.T) {
	single := eventQuery{childID: "c_1", types: defaultTypeSet(true)}
	if single.typesExplicit() {
		t.Errorf("the single-child default must not read as explicit: %v", single.types)
	}
	multi := eventQuery{types: defaultTypeSet(false)}
	if multi.typesExplicit() {
		t.Errorf("the multi-child default must not read as explicit: %v", multi.types)
	}
	for _, tc := range []struct {
		name string
		q    eventQuery
	}{
		{"all-types (nil types)", eventQuery{childID: "c_1"}},
		{"--types script_output", eventQuery{childID: "c_1", types: []string{"script_output"}}},
		{"--types a subset of the default", eventQuery{childID: "c_1", types: []string{"user_message", "agent_status", "child_exited"}}},
	} {
		if !tc.q.typesExplicit() {
			t.Errorf("%s must read as explicit", tc.name)
		}
	}
}

// ── completion ───────────────────────────────────────────────────────────────

// Child completion on logs — the coverage TestCompleteHistoryOffersChildren
// held for the folded-in history verb, which now lives here.
func TestCompleteLogsOffersChildren(t *testing.T) {
	c := assert.NewCollecting(t)
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	seedChildrenCompletionCache(t)

	cmd := newLogsCmd()
	c.Require().NotNil(cmd.ValidArgsFunction, "ValidArgsFunction not set — `rafiki logs <TAB>` completes nothing")
	got, directive := cmd.ValidArgsFunction(cmd, nil, "")
	c.Eq(cobra.ShellCompDirectiveNoFileComp, directive, "directive")
	for _, want := range []string{"c_01HXABC", "alpha", "beta"} {
		c.True(containsCandidate(got, want), "candidates %v missing %q (ids and names both target logs)", got, want)
	}
	// One target is all the verb takes; past it there is nothing to offer.
	if got, _ := cmd.ValidArgsFunction(cmd, []string{"c_01HXABC"}, ""); len(got) != 0 {
		t.Errorf("past the single target got %v, want none", got)
	}
}

// --types completion offers the native vocabulary, never framed-era names.
// The registered handler is prefixCompletions over allNativeTypes, so the
// candidates are asserted through it directly (cobra keeps its completion
// registry private).
func TestCompleteTypesOffersNativeNames(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, newCmd := range []func() *cobra.Command{newLogsCmd, newTailCmd} {
		ck.Require().NotNil(newCmd().Flags().Lookup("types"), "--types flag missing")
	}
	got := prefixCompletions(allNativeTypes(), "agent_")
	ck.True(containsCandidate(got, "agent_status"), "--types candidates %v missing agent_status", got)
	for _, c := range allNativeTypes() {
		ck.False(strings.HasPrefix(c, "message_"), "--types candidate %q is framed-era vocabulary", c)
	}
}
