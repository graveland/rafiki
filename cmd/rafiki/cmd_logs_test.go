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

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
)

// ── the stub daemon ──────────────────────────────────────────────────────────

// stubControl serves the Control RPCs the event commands use and records what
// arrived. Unoverridden RPCs answer Unimplemented via the embed; the call
// counters let a test prove a verb dialled nothing.
type stubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	mu sync.Mutex

	events          []*rafikiv1.Event // GetHistory's response
	streamEvents    []*rafikiv1.Event // what StreamEvents sends before ending
	streamErr       error             // what StreamEvents returns after sending
	historyNotFound bool              // GetHistory answers NotFound (no conversation)
	getChildOK      bool              // GetChild answers with a named child when set

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
	_ context.Context,
	req *connect.Request[rafikiv1.StreamEventsRequest],
	stream *connect.ServerStream[rafikiv1.Event],
) error {
	s.mu.Lock()
	s.streamCalls++
	s.streamReqs = append(s.streamReqs, req.Msg)
	events := append([]*rafikiv1.Event(nil), s.streamEvents...)
	streamErr := s.streamErr
	s.mu.Unlock()

	for _, ev := range events {
		if err := stream.Send(ev); err != nil {
			return err
		}
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

// serveStubControl isolates profiles, then serves stub over the connect.sock
// sibling of a profile's control socket — the same wiring production uses, so
// a test exercises newConnectEndpoint's real dial path.
func serveStubControl(t *testing.T, stub *stubControl) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// A short directory, not t.TempDir(): unix socket paths are capped at
	// ~104 bytes (sizeof sun_path on darwin), and t.TempDir() nests under the
	// full test name, which alone can exceed that.
	dir, err := os.MkdirTemp("", "e4")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	controlSock := filepath.Join(dir, "controller.sock")
	connectSock := filepath.Join(dir, "connect.sock") // sibling, per connectSocketFor

	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, connectSock, routePath, handler)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: controlSock},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

// serveConnectOnUnixSocket serves a Connect handler over an h2c unix socket at
// path, matching the client half in connectclient.go's connectHTTPClient
// (AllowHTTP h2 over a plain unix dial).
func serveConnectOnUnixSocket(t *testing.T, path string, handlerPath string, handler http.Handler) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
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
	cmd := newLogsCmd()
	n, err := cmd.Flags().GetInt("tail")
	if err != nil || n != -1 {
		t.Fatalf("logs tail default = %d (err %v), want -1 (all)", n, err)
	}
	if f, err := cmd.Flags().GetBool("follow"); err != nil || f {
		t.Fatalf("logs follow default = %v (err %v), want false", f, err)
	}
	if raw, err := cmd.Flags().GetBool("raw"); err != nil || raw {
		t.Fatalf("logs raw default = %v (err %v), want false", raw, err)
	}
	for _, flag := range []string{"stdin", "stderr", "all", "path", "types", "all-types"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("logs is missing the %q flag", flag)
		}
	}
	if !containsAlias(cmd.Aliases, "log") {
		t.Errorf("logs lost its 'log' alias: %v", cmd.Aliases)
	}
}

// The flags the framed plane needed are gone, not merely defaulted.
func TestLogsRemovedFlagsAreGone(t *testing.T) {
	cmd := newLogsCmd()
	for _, flag := range []string{"profile", "include", "exclude", "no-deltas", "verbose"} {
		if cmd.Flags().Lookup(flag) != nil {
			t.Errorf("logs still declares the retired %q flag", flag)
		}
	}
	cmd = newTailCmd()
	for _, flag := range []string{"profile", "include", "exclude", "no-deltas", "verbose"} {
		if cmd.Flags().Lookup(flag) != nil {
			t.Errorf("tail still declares the retired %q flag", flag)
		}
	}
}

// The snapshot flags refuse to combine — cobra validates the group before
// RunE, so no dial happens on a bad flag set.
func TestLogsSnapshotFlagsAreMutuallyExclusive(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	cmd := newLogsCmd()
	_, _, err := runCmd(t, cmd, "--stdin", "--stderr", "c_1")
	if err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("logs --stdin --stderr = %v, want cobra's mutually-exclusive error naming the group", err)
	}
}

// --types is validated against the native vocabulary before anything dials:
// an unknown name errors instead of silently selecting nothing.
func TestLogsRejectsAnUnknownEventType(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	cmd := newLogsCmd()
	_, _, err := runCmd(t, cmd, "--types", "message_end", "c_1")
	if err == nil || !strings.Contains(err.Error(), "unknown event type") {
		t.Fatalf("logs --types message_end = %v, want an unknown-type error", err)
	}
}

// logs prints the history served by the profile's OWN connect.sock — the pin
// that used to hold for `history` holds for its replacement: a socket
// profile's logs must not query whatever daemon happens to listen elsewhere.
func TestLogsReachesTheSocketProfilesOwnDaemon(t *testing.T) {
	stub := &stubControl{events: []*rafikiv1.Event{
		userMessageEvent("c_1", 0, "served by the profile's own socket"),
	}}
	serveStubControl(t, stub)

	cmd := newLogsCmd()
	out, _, err := runCmd(t, cmd, "c_1")
	if err != nil {
		t.Fatalf("rafiki logs: %v", err)
	}
	if stub.historyCalls != 1 {
		t.Fatalf("GetHistory called %d times, want 1", stub.historyCalls)
	}
	if !strings.Contains(out, "served by the profile's own socket") {
		t.Fatalf("logs output does not show the event served by the profile's OWN socket:\n%s", out)
	}
	if !strings.Contains(out, "user_message") {
		t.Fatalf("logs output lost the event's type column:\n%s", out)
	}
}

// logs -f prints the history and then follows with a cursor just after the
// last history ordinal — the resume point the brief pins.
func TestLogsFollowResumesFromTheLastHistoryOrdinal(t *testing.T) {
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
	if err != nil {
		t.Fatalf("rafiki logs -f: %v", err)
	}
	if stub.historyCalls != 1 || stub.streamCalls != 1 {
		t.Fatalf("calls = history %d, stream %d; want 1 and 1", stub.historyCalls, stub.streamCalls)
	}

	req := stub.streamReqs[0]
	if got := req.GetCursor().GetOrdinals()["c_1"]; got != 2 {
		t.Errorf("cursor ordinal for c_1 = %d, want 2 (the last history ordinal)", got)
	}
	// Single-child subject, conversation+lifecycle types, durable tier.
	if req.GetSubject().GetChild() != "c_1" {
		t.Errorf("subject = %+v, want child c_1", req.GetSubject().GetScope())
	}
	for _, want := range []string{"agent_status", "user_message", "compaction_boundary", "child_exited"} {
		if !strings.Contains(strings.Join(req.GetTypes(), ","), want) {
			t.Errorf("default single-child types missing %q: %v", want, req.GetTypes())
		}
	}
	for _, banned := range []string{"content_block_delta", "message_update"} {
		for _, got := range req.GetTypes() {
			if got == banned {
				t.Errorf("default types include %q", banned)
			}
		}
	}
	if req.GetTier() != rafikiv1.EventTier_EVENT_TIER_DURABLE {
		t.Errorf("tier = %v, want DURABLE", req.GetTier())
	}

	// The backfill printed all three history events, then the live event.
	for _, want := range []string{"first", "second", "third", "idle"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(notes, "unavailable") {
		t.Errorf("unexpected notes: %q", notes)
	}
}

// A child with no conversation is empty history, not an error: logs -f must
// still follow, with no cursor (there is no history to resume from).
func TestLogsFollowSurvivesAMissingConversation(t *testing.T) {
	stub := &stubControl{
		historyNotFound: true,
		streamEvents:    []*rafikiv1.Event{statusFor("c_1", "streaming")},
	}
	serveStubControl(t, stub)

	cmd := newLogsCmd()
	out, _, err := runCmd(t, cmd, "-f", "c_1")
	if err != nil {
		t.Fatalf("rafiki logs -f on a child with no conversation: %v", err)
	}
	if !strings.Contains(out, "streaming") {
		t.Errorf("follow output missing the live event:\n%s", out)
	}
	if req := stub.streamReqs[0]; req.GetCursor() != nil {
		t.Errorf("empty history must not carry a cursor, got %+v", req.GetCursor())
	}
}

// The single-child tail backfills 20 by default and exits on the child's own
// child_exited event.
func TestTailChildBackfillsAndExitsOnChildExited(t *testing.T) {
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
	if err != nil {
		t.Fatalf("rafiki tail c_1: %v", err)
	}
	if stub.historyCalls != 1 || stub.streamCalls != 1 {
		t.Fatalf("calls = history %d, stream %d; want 1 and 1", stub.historyCalls, stub.streamCalls)
	}
	req := stub.streamReqs[0]
	if got := req.GetCursor().GetOrdinals()["c_1"]; got != 1 {
		t.Errorf("cursor ordinal = %d, want 1 (the last history ordinal)", got)
	}
	for _, want := range []string{"hello", "again", "exit"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// ── completion ───────────────────────────────────────────────────────────────

// Child completion on logs — the coverage TestCompleteHistoryOffersChildren
// held for the folded-in history verb, which now lives here.
func TestCompleteLogsOffersChildren(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	seedChildrenCompletionCache(t)

	cmd := newLogsCmd()
	if cmd.ValidArgsFunction == nil {
		t.Fatal("ValidArgsFunction not set — `rafiki logs <TAB>` completes nothing")
	}
	got, directive := cmd.ValidArgsFunction(cmd, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
	for _, want := range []string{"c_01HXABC", "alpha", "beta"} {
		if !containsCandidate(got, want) {
			t.Errorf("candidates %v missing %q (ids and names both target logs)", got, want)
		}
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
	for _, newCmd := range []func() *cobra.Command{newLogsCmd, newTailCmd} {
		if newCmd().Flags().Lookup("types") == nil {
			t.Fatal("--types flag missing")
		}
	}
	got := prefixCompletions(allNativeTypes(), "agent_")
	if !containsCandidate(got, "agent_status") {
		t.Errorf("--types candidates %v missing agent_status", got)
	}
	for _, c := range allNativeTypes() {
		if strings.Contains(c, "ctrl_") || strings.HasPrefix(c, "message_") {
			t.Errorf("--types candidate %q is framed-era vocabulary", c)
		}
	}
}
