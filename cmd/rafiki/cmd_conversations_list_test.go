package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

func TestConversationsListCmd_FlagsRegistered(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newConversationsListCmd()
	for _, name := range []string{
		"limit", "cursor", "sort", "parent", "query", "state", "kind",
		"name", "cwd", "label", "since", "until", "all",
	} {
		c.NotNil(cmd.Flags().Lookup(name), "flag --%s not registered", name)
	}
	c.Require().NotNil(cmd.Args, "Args validator not set")
	c.Error(cmd.Args(cmd, []string{"extra"}), "expected error for a positional arg (list takes none)")
	c.NoError(cmd.Args(cmd, nil), "no args should validate")

	// "list" and its "ls" alias must resolve to the LISTING verb: search gave
	// those names up when list arrived, and cobra resolves a sibling match by
	// add order, so a leftover search alias would shadow this command while
	// Find still reports no error.
	root := newRootCmd()
	for _, arg := range []string{"list", "ls"} {
		got, _, err := root.Find([]string{"conversations", arg})
		c.Require().NoError(err, "conversations %s not reachable from the command tree", arg)
		c.Eq("list", got.Name(), "conversations %s resolved to %q", arg, got.Name())
	}
}

// ─── the Connect stub ───────────────────────────────────────────────────────

// listStub serves ListConversations and records every request, so the tests
// assert what reached the wire rather than what a mock echoed. Pages are
// served in the order stubbed; a call past the last one answers an empty page
// (the follow loop's stop condition) instead of hanging.
type listStub struct {
	rafikiv1connect.UnimplementedControlHandler
	mu    sync.Mutex
	pages []*rafikiv1.ListConversationsResponse
	calls []*rafikiv1.ListConversationsRequest
}

func (s *listStub) ListConversations(
	_ context.Context,
	req *connect.Request[rafikiv1.ListConversationsRequest],
) (*connect.Response[rafikiv1.ListConversationsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req.Msg)
	i := len(s.calls) - 1
	if i >= len(s.pages) {
		return connect.NewResponse(&rafikiv1.ListConversationsResponse{}), nil
	}
	return connect.NewResponse(s.pages[i]), nil
}

func (s *listStub) listed() []*rafikiv1.ListConversationsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.ListConversationsRequest(nil), s.calls...)
}

// newConversationsListHarness wires an isolated profile at a Connect stub
// served on the profile's own socket, and points the process at it. Same
// shape as newConversationsHarness, for this verb's own stub.
func newConversationsListHarness(t *testing.T) *listStub {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp with a short prefix, not t.TempDir(): the harness serves a
	// unix socket, whose path must fit sun_path (104 bytes on darwin).
	dir, err := os.MkdirTemp("", "cvl")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	stub := &listStub{}
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	sock := filepath.Join(dir, "controller.sock")
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
	return stub
}

// captureListOutput swaps os.Stdout and os.Stderr around run and returns what
// each stream received. The verb renders to os.Stdout directly and pages to
// os.Stderr, so the real streams are the seam a harness-driven test reads
// them through.
func captureListOutput(t *testing.T, run func()) (string, string) {
	t.Helper()
	c := assert.NewAborting(t)
	or, ow, err := os.Pipe()
	c.Require().NoError(err, "stdout pipe")
	er, ew, err := os.Pipe()
	c.Require().NoError(err, "stderr pipe")
	t.Cleanup(func() { or.Close(); er.Close(); ow.Close(); ew.Close() })

	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	var outBuf, errBuf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&outBuf, or)
		_, _ = io.Copy(&errBuf, er)
		close(done)
	}()

	run()

	os.Stdout, os.Stderr = oldOut, oldErr
	ow.Close()
	ew.Close()
	<-done
	return outBuf.String(), errBuf.String()
}

// listingRows is one page exercising every cell shape the table renders: a
// fully-linked agent row, an agent known only by id, and a conversation with
// no agent at all.
func listingRows() []*rafikiv1.ConversationListing {
	return []*rafikiv1.ConversationListing{
		{
			Id: "conv-1", Name: "root job", Model: "claude-sonnet-5", Owner: "alice",
			CreatedAt:    timestamppb.New(time.Unix(1716000000, 0)),
			LastActivity: timestamppb.New(time.Unix(1716003600, 0)),
			Turns:        34, CostUsd: 0.042, ChildCount: 12,
			ChildId: "c_1", ChildName: "coordinator", Kind: "fundi",
			State: rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
		},
		{
			Id: "conv-2", Name: "ledger",
			CreatedAt:    timestamppb.New(time.Unix(1716000060, 0)),
			LastActivity: timestamppb.New(time.Unix(1716003660, 0)),
			ChildId:      "c_9",
			State:        rafikiv1.ConversationState_CONVERSATION_STATE_EXITED,
		},
		{
			Id:           "conv-3",
			CreatedAt:    timestamppb.New(time.Unix(1716000120, 0)),
			LastActivity: timestamppb.New(time.Unix(1716003720, 0)),
			State:        rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT,
		},
	}
}

// listingRowLine returns the rendered table's line for one conversation id.
func listingRowLine(out, id string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, id) {
			return ln
		}
	}
	return ""
}

// ─── flags onto the wire ────────────────────────────────────────────────────

// Every filter flag must reach the request verbatim: --state accepts both
// spellings (repeatable and comma-separated) in one invocation, labels land
// in the request's map, and --since resolves to a present Timestamp while an
// unset --until stays absent.
func TestConversationsListSendsFiltersOverConnect(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsListHarness(t)
	stub.pages = []*rafikiv1.ListConversationsResponse{{Rows: listingRows()}}

	cmd := newConversationsListCmd()
	cmd.SetArgs([]string{
		"--limit", "7", "--cursor", "cur-0", "--sort", "created",
		"--parent", "conv-root", "--query", "migrate",
		"--state", "open,no-agent", "--state", "closed",
		"--kind", "fundi", "--kind", "claude",
		"--name", "slot", "--cwd", "/Users/brent",
		"--label", "wave=three", "--label", "seat=implementer",
		"--since", "24h", "--until", "2026-01-02T03:04:05Z",
	})
	c.Require().NoError(cmd.Execute(), "conversations list")

	calls := stub.listed()
	c.Require().Len(calls, 1, "got %d ListConversations calls, want 1", len(calls))
	req := calls[0]
	c.Eq(int32(7), req.GetLimit(), "Limit")
	c.Eq("cur-0", req.GetCursor(), "Cursor")
	c.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED, req.GetSort(), "Sort")
	c.Eq("conv-root", req.GetParentConversationId(), "ParentConversationId")
	c.Eq("migrate", req.GetQuery(), "Query")
	c.Eq("slot", req.GetNameContains(), "NameContains")
	c.Eq("/Users/brent", req.GetCwdContains(), "CwdContains")
	c.EqDeep(
		[]rafikiv1.ConversationState{
			rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
			rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT,
			rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED,
		},
		req.GetStates(), "States")
	c.EqDeep([]string{"fundi", "claude"}, req.GetKinds(), "Kinds")
	c.EqDeep(map[string]string{"wave": "three", "seat": "implementer"}, req.GetLabels(), "Labels")

	c.Require().NotNil(req.GetSince(), "Since on the wire")
	gotSince := req.GetSince().AsTime()
	wantSince := time.Now().Add(-24 * time.Hour)
	within := gotSince.After(wantSince.Add(-2*time.Minute)) && gotSince.Before(wantSince.Add(2*time.Minute))
	c.True(within, "Since = %v, want the resolved --since 24h (~%v)", gotSince, wantSince)

	until, err := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	c.Require().NoError(err)
	c.Require().NotNil(req.GetUntil(), "Until on the wire")
	c.Eq(until.Unix(), req.GetUntil().AsTime().Unix(), "Until")
}

// Without any flag the request is the flag defaults: limit 50, the recent
// sort, no narrowing anywhere, and unset since/until riding ABSENT (nil
// pointers, never zero Timestamps).
func TestConversationsListDefaultsRideTheWire(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsListHarness(t)
	stub.pages = []*rafikiv1.ListConversationsResponse{{}}

	cmd := newConversationsListCmd()
	cmd.SetArgs(nil)
	c.Require().NoError(cmd.Execute(), "conversations list")

	calls := stub.listed()
	c.Require().Len(calls, 1, "got %d ListConversations calls, want 1", len(calls))
	req := calls[0]
	c.Eq(int32(50), req.GetLimit(), "Limit default")
	c.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_RECENT, req.GetSort(), "Sort default")
	c.Eq("", req.GetCursor(), "Cursor")
	c.Eq("", req.GetParentConversationId(), "ParentConversationId")
	c.Eq("", req.GetQuery(), "Query")
	c.Eq("", req.GetNameContains(), "NameContains")
	c.Eq("", req.GetCwdContains(), "CwdContains")
	c.Nil(req.GetStates(), "States")
	c.Nil(req.GetKinds(), "Kinds")
	c.Nil(req.GetLabels(), "Labels")
	c.Nil(req.GetSince(), "an unset --since must ride absent")
	c.Nil(req.GetUntil(), "an unset --until must ride absent")
}

// -q is --query's shorthand and reaches the same request field.
func TestConversationsListQueryShorthand(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsListHarness(t)
	stub.pages = []*rafikiv1.ListConversationsResponse{{}}

	cmd := newConversationsListCmd()
	cmd.SetArgs([]string{"-q", "ledger"})
	c.Require().NoError(cmd.Execute(), "conversations list -q")

	calls := stub.listed()
	c.Require().Len(calls, 1, "got %d ListConversations calls, want 1", len(calls))
	c.Eq("ledger", calls[0].GetQuery(), "Query")
}

// --state no-agent maps to CONVERSATION_STATE_NO_AGENT (the wire refuses an
// UNSPECIFIED inside the list, so the mapping must be exact), every other
// member of the closed set maps too, comma-separated values split, and an
// unknown value is refused with the flag's name in the message.
func TestConversationsListStateFlagMappings(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		vals []string
		want []rafikiv1.ConversationState
	}{
		{[]string{"no-agent"}, []rafikiv1.ConversationState{rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT}},
		{[]string{"open"}, []rafikiv1.ConversationState{rafikiv1.ConversationState_CONVERSATION_STATE_OPEN}},
		{[]string{"exited"}, []rafikiv1.ConversationState{rafikiv1.ConversationState_CONVERSATION_STATE_EXITED}},
		{[]string{"closed"}, []rafikiv1.ConversationState{rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED}},
		{[]string{"open,closed"}, []rafikiv1.ConversationState{
			rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
			rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED,
		}},
		{[]string{"open", "no-agent"}, []rafikiv1.ConversationState{
			rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
			rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT,
		}},
	} {
		cmd := newConversationsListCmd()
		var args []string
		for _, v := range tc.vals {
			args = append(args, "--state", v)
		}
		c.Require().NoError(cmd.Flags().Parse(args), "parse --state %v", tc.vals)
		got, err := conversationListStatesFlag(cmd)
		c.Require().NoError(err, "states %v", tc.vals)
		c.EqDeep(tc.want, got, "states %v", tc.vals)
	}
	for _, bad := range []string{"running", "NO-AGENT", "no_agent"} {
		cmd := newConversationsListCmd()
		c.Require().NoError(cmd.Flags().Parse([]string{"--state", bad}), "parse %q", bad)
		_, err := conversationListStatesFlag(cmd)
		c.Require().Error(err, "state %q: expected an error", bad)
		c.StrContains(err.Error(), "--state", "state %q", bad)
	}
	// A trailing comma leaves an empty member behind, which is refused: the
	// daemon widens on an UNSPECIFIED rather than dropping it, so the client
	// refuses the shape that would ask for one. A bare empty value, by
	// contrast, never reaches this code: pflag parses --state "" as NO value
	// at all, and the wire reads an empty list as "all".
	cmd := newConversationsListCmd()
	c.Require().NoError(cmd.Flags().Parse([]string{"--state", "open,"}), "parse trailing comma")
	_, err := conversationListStatesFlag(cmd)
	c.Require().Error(err, "trailing comma: expected an error")
	c.StrContains(err.Error(), "--state", "trailing comma")

	cmd = newConversationsListCmd()
	c.Require().NoError(cmd.Flags().Parse([]string{"--state", ""}), "parse empty")
	got, err := conversationListStatesFlag(cmd)
	c.Require().NoError(err, "empty value")
	c.Empty(got, "an empty --state value is pflag's no-value, not a state")
}

// --sort is a closed set: anything else, including an explicitly empty value,
// errors naming the flag, and nothing dials.
func TestConversationsListSortRefusedBeforeDial(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, sort := range []string{"bogus", "", "RECENT", "newest"} {
		stub := newConversationsListHarness(t)
		cmd := newConversationsListCmd()
		cmd.SetArgs([]string{"--sort", sort})
		err := cmd.Execute()
		c.Require().Error(err, "sort %q: expected an error", sort)
		c.StrContains(err.Error(), "--sort", "sort %q", sort)
		c.Empty(stub.listed(), "sort %q: a rejected request must not reach the wire", sort)
	}
}

// A label member without '=' or with an empty key is refused before any dial;
// a value may be empty and may itself carry '='.
func TestConversationsListLabelFlagRefusals(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, bad := range []string{"wave", "=v", "="} {
		cmd := newConversationsListCmd()
		c.Require().NoError(cmd.Flags().Parse([]string{"--label", bad}), "parse %q", bad)
		_, err := conversationListLabelsFlag(cmd)
		c.Require().Error(err, "label %q: expected an error", bad)
		c.StrContains(err.Error(), "--label", "label %q", bad)
	}
	for _, tc := range []struct {
		vals []string
		want map[string]string
	}{
		{[]string{"seat=implementer"}, map[string]string{"seat": "implementer"}},
		{[]string{"k="}, map[string]string{"k": ""}},
		{[]string{"a=b=c", "k="}, map[string]string{"a": "b=c", "k": ""}},
	} {
		cmd := newConversationsListCmd()
		var args []string
		for _, v := range tc.vals {
			args = append(args, "--label", v)
		}
		c.Require().NoError(cmd.Flags().Parse(args), "parse %v", tc.vals)
		got, err := conversationListLabelsFlag(cmd)
		c.Require().NoError(err, "labels %v", tc.vals)
		c.EqDeep(tc.want, got, "labels %v", tc.vals)
	}
}

// --limit is narrowed to the proto's int32 and must not be negative; a value
// that would truncate (or dip below zero) errors rather than silently sending
// the wrong number.
func TestConversationsListLimitRefusedBeforeDial(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, limit := range []string{"-1", "9999999999"} {
		stub := newConversationsListHarness(t)
		cmd := newConversationsListCmd()
		cmd.SetArgs([]string{"--limit", limit})
		err := cmd.Execute()
		c.Require().Error(err, "limit %q: expected an error", limit)
		c.StrContains(err.Error(), "--limit", "limit %q", limit)
		c.Empty(stub.listed(), "limit %q: a rejected request must not reach the wire", limit)
	}
}

// ─── paging ─────────────────────────────────────────────────────────────────

// --all follows next_cursor until a page comes back without one, chaining
// each page's cursor into the next call with the caller's filters intact,
// and prints every row.
func TestConversationsListFollowsPagesWithAll(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsListHarness(t)
	rows := listingRows()
	stub.pages = []*rafikiv1.ListConversationsResponse{
		{Rows: rows[:1], NextCursor: "cur-1"},
		{Rows: rows[1:2], NextCursor: "cur-2"},
		{Rows: rows[2:3]},
	}

	cmd := newConversationsListCmd()
	cmd.SetArgs([]string{"--all", "--limit", "2"})
	out, errOut := captureListOutput(t, func() {
		c.Require().NoError(cmd.Execute(), "conversations list --all")
	})

	calls := stub.listed()
	c.Require().Len(calls, 3, "got %d ListConversations calls, want 3", len(calls))
	c.Eq("", calls[0].GetCursor(), "page 1 cursor")
	c.Eq("cur-1", calls[1].GetCursor(), "page 2 cursor")
	c.Eq("cur-2", calls[2].GetCursor(), "page 3 cursor")
	// The cursor is valid only for a request whose sort and parent match the
	// page that produced it, so every follow-up keeps the caller's filters.
	for i, call := range calls {
		c.Eq(int32(2), call.GetLimit(), "page %d limit", i+1)
	}
	for _, id := range []string{"conv-1", "conv-2", "conv-3"} {
		c.StrContains(out, id, "--all must print every row")
	}
	c.Eq("", errOut, "--all must not print the more: hint")
}

// Without --all one page is fetched, and a non-empty next_cursor is reported
// as a ready-to-paste cursor hint on stderr, after the table on stdout.
func TestConversationsListMoreHintOnStderr(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsListHarness(t)
	stub.pages = []*rafikiv1.ListConversationsResponse{
		{Rows: listingRows()[:1], NextCursor: "cur-1"},
	}

	cmd := newConversationsListCmd()
	cmd.SetArgs([]string{"--limit", "1"})
	out, errOut := captureListOutput(t, func() {
		c.Require().NoError(cmd.Execute(), "conversations list")
	})
	c.StrContains(out, "conv-1", "stdout must carry the table")
	c.Eq("more: --cursor cur-1\n", errOut, "stderr hint")

	// A last page (empty next_cursor) prints no hint.
	stub = newConversationsListHarness(t)
	stub.pages = []*rafikiv1.ListConversationsResponse{{Rows: listingRows()[:1]}}
	cmd = newConversationsListCmd()
	cmd.SetArgs(nil)
	_, errOut = captureListOutput(t, func() {
		c.Require().NoError(cmd.Execute(), "conversations list (last page)")
	})
	c.Eq("", errOut, "a last page must not print the hint")
}

// ─── rendering ──────────────────────────────────────────────────────────────

// The table carries the design's columns; the AGENT cell prefers the linked
// agent's name, falls back to its id, and shows "-" with no agent, and the
// timestamps render in the viewer's local time like every other listing.
func TestConversationsListRendersTable(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	c.Require().NoError(renderConversationsList(&buf, outputTable, listingRows(), ""), "render")
	out := buf.String()

	for _, want := range []string{
		"ID", "NAME", "AGENT", "KIND", "STATE", "KIDS", "TURNS", "COST", "LAST ACTIVITY",
		"conv-1", "root job", "fundi", "open", "$0.04",
		"exited", "no-agent",
	} {
		c.StrContains(out, want, "listing table missing %q:\n%s", want, out)
	}
	c.StrContains(listingRowLine(out, "conv-1"), "coordinator", "AGENT must prefer the child name")
	c.StrContains(listingRowLine(out, "conv-2"), "c_9", "AGENT must fall back to the child id")
	// The empty cells render as "-", never as bare gaps.
	c.StrContains(listingRowLine(out, "conv-3"), "-", "an agentless row must render '-'")
	// Timestamps go through the same Local().Format the other listings use,
	// so the expectation is computed the same TZ-dependent way.
	c.StrContains(listingRowLine(out, "conv-1"),
		time.Unix(1716003600, 0).Local().Format("2006-01-02 15:04"), "LAST ACTIVITY")

	var empty bytes.Buffer
	c.Require().NoError(renderConversationsList(&empty, outputTable, nil, ""), "empty render")
	c.Eq("no conversations found\n", empty.String(), "empty table rendered")
}

// -j is the canonical protojson of the whole response message: the rows plus
// the cursor that continues past them, under the wire's own field names. -J
// is one compact record per conversation row.
func TestConversationsListJSONModes(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := listingRows()

	var pretty bytes.Buffer
	c.Require().NoError(renderConversationsList(&pretty, outputJSON, rows, "cur-9"), "-j render")
	var back struct {
		Rows       []map[string]any `json:"rows"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(pretty.Bytes(), &back); err != nil {
		t.Fatalf("-j output is not valid JSON: %v\n%s", err, pretty.String())
	}
	c.Eq("cur-9", back.NextCursor, "-j must carry the cursor that continues past the rows")
	c.Require().Len(back.Rows, 3, "-j rows")
	c.Eq("conv-1", back.Rows[0]["id"], "row identity rides the wire's own field names")
	c.Eq("coordinator", back.Rows[0]["childName"], "row agent name")

	var lines bytes.Buffer
	c.Require().NoError(renderConversationsList(&lines, outputJSONL, rows, "cur-9"), "-J render")
	got := strings.Split(strings.TrimSpace(lines.String()), "\n")
	c.Len(got, 3, "-J renders one record per row, got %d:\n%s", len(got), lines.String())
	var first map[string]any
	c.Require().NoError(json.Unmarshal([]byte(got[0]), &first), "first -J record")
	c.Eq("conv-1", first["id"], "first -J record")
}
