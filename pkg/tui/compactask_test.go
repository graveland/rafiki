// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	rafikiv1connect "go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// compactClient is the ControlClient a compacting fork reaches. It records
// every call in ORDER, because the composition's shape is the assertion:
// spawn the intermediate, send it /compact, spawn the fork from it, stop it,
// close it.
//
// Two things here are deliberate rather than decoration, because a fake that
// lets them slide lets the bug back in:
//
//   - the history carries a compaction boundary only AFTER /compact was sent.
//     compactfork reads a watermark before the send precisely because a
//     boundary already present satisfies the wait instantly with a stale event.
//   - Close REFUSES a child that is still live, the way the daemon does. Close
//     finalizes an EXITED child and answers ErrNotExited otherwise, so a fake
//     that closed anything made the composition's stop step look optional —
//     the sequence that failed every real run at its very last step.
type compactClient struct {
	rafikiv1connect.ControlClient

	mu      sync.Mutex
	events  []string
	spawned []*rafikiv1.SpawnRequest
	// live holds the children Spawn has handed out that no Kill has stopped
	// yet; Close refuses those.
	live map[string]bool
	// compacted is set once /compact has been sent to the intermediate.
	compacted bool
}

func newCompactClient() *compactClient {
	return &compactClient{live: map[string]bool{}}
}

func (f *compactClient) record(ev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *compactClient) GetChild(_ context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
	f.record("getchild:" + req.Msg.GetChildId())
	return connect.NewResponse(&rafikiv1.GetChildResponse{Child: &rafikiv1.ChildSummary{
		ChildId: req.Msg.GetChildId(),
		Name:    "scout",
		Cwd:     "/work/scout",
		Labels:  map[string]string{"rafiki/parent": "c_coord", "team": "x"},
		Routing: "sort=price",
	}}), nil
}

func (f *compactClient) Spawn(_ context.Context, req *connect.Request[rafikiv1.SpawnRequest]) (*connect.Response[rafikiv1.SpawnResponse], error) {
	f.mu.Lock()
	f.spawned = append(f.spawned, req.Msg)
	id := fmt.Sprintf("c_new%d", len(f.spawned))
	f.live[id] = true
	f.mu.Unlock()
	f.record("spawn")
	return connect.NewResponse(&rafikiv1.SpawnResponse{ChildId: id}), nil
}

func (f *compactClient) Send(_ context.Context, req *connect.Request[rafikiv1.SendRequest]) (*connect.Response[rafikiv1.SendResponse], error) {
	f.mu.Lock()
	f.compacted = true
	f.mu.Unlock()
	f.record("send:" + req.Msg.GetChildId())
	return connect.NewResponse(&rafikiv1.SendResponse{}), nil
}

func (f *compactClient) GetHistory(_ context.Context, req *connect.Request[rafikiv1.GetHistoryRequest]) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	f.mu.Lock()
	compacted := f.compacted
	f.mu.Unlock()
	if !compacted {
		return connect.NewResponse(&rafikiv1.GetHistoryResponse{}), nil
	}
	ord := int32(1)
	post := int32(4000)
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{Events: []*rafikiv1.Event{{
		ChildId: req.Msg.GetChildId(),
		Ordinal: &ord,
		Payload: &rafikiv1.Event_CompactionBoundary{CompactionBoundary: &rafikiv1.CompactionBoundary{
			PostTokens: &post,
		}},
	}}}), nil
}

func (f *compactClient) Close(_ context.Context, req *connect.Request[rafikiv1.CloseRequest]) (*connect.Response[rafikiv1.CloseResponse], error) {
	f.mu.Lock()
	live := f.live[req.Msg.GetChildId()]
	f.mu.Unlock()
	if live {
		return nil, rpcreason.Attach(
			connect.NewError(connect.CodeFailedPrecondition, errors.New("child is still running")),
			protocol.ErrNotExited)
	}
	f.record("close:" + req.Msg.GetChildId())
	return connect.NewResponse(&rafikiv1.CloseResponse{}), nil
}

func (f *compactClient) Kill(_ context.Context, req *connect.Request[rafikiv1.KillRequest]) (*connect.Response[rafikiv1.KillResponse], error) {
	f.mu.Lock()
	f.live[req.Msg.GetChildId()] = false
	f.mu.Unlock()
	f.record("kill:" + req.Msg.GetChildId())
	return connect.NewResponse(&rafikiv1.KillResponse{}), nil
}

func (f *compactClient) Rename(_ context.Context, req *connect.Request[rafikiv1.RenameRequest]) (*connect.Response[rafikiv1.RenameResponse], error) {
	f.record("rename:" + req.Msg.GetName())
	return connect.NewResponse(&rafikiv1.RenameResponse{Name: req.Msg.GetName()}), nil
}

func (f *compactClient) SetRouting(_ context.Context, req *connect.Request[rafikiv1.SetRoutingRequest]) (*connect.Response[rafikiv1.SetRoutingResponse], error) {
	f.record("routing:" + req.Msg.GetDelta())
	return connect.NewResponse(&rafikiv1.SetRoutingResponse{ChildId: req.Msg.GetChildId(), Routing: req.Msg.GetDelta()}), nil
}

func (f *compactClient) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(make([]string, 0, len(f.events)), f.events...)
}

func (f *compactClient) requests() []*rafikiv1.SpawnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*rafikiv1.SpawnRequest(nil), f.spawned...)
}

// The source model and the destination window the tests use: a conversation
// far past the smaller model's window.
const (
	compactSourceModel = "openrouter/big/model"
	compactDestModel   = "openrouter/small/model"
	compactDestWindow  = 8_000
	compactSourceToks  = 120_000
)

// compactSource seeds a rail row the way a live child appears: its model from
// ListChildren, its prompt size from the last turn_end's usage.
func compactSource(c *Cockpit, id string) {
	c.rail.Seed([]*rafikiv1.ChildSummary{{
		ChildId:       id,
		Name:          "scout",
		Status:        "idle",
		Model:         compactSourceModel,
		Labels:        map[string]string{},
		LatestOrdinal: i32p(0),
	}})
	c.rail.Apply(turnEndWithUsageFor(id, 1, compactSourceToks, 0, 0))
}

// compactForm is a fork form on c_1 pointed at the small model, with the
// catalog that knows its window.
func compactForm(t *testing.T, c *Cockpit) {
	t.Helper()
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "scout", 0)})
	c.handleKey(keyMsg("n"))
	assert.NewAborting(t).Require().NotNil(c.form, "n did not open the create form")
	compactSource(c, "c_1")
	c.form.setForkFrom("c_1")
	c.form.inputs[fieldCwd].SetValue("/work/scout")
	c.form.inputs[fieldModel].SetValue(compactDestModel)
	seedModels(c, c.form.kind(), []*rafikiv1.ModelRow{
		{Id: compactDestModel, ContextWindow: i32p(compactDestWindow)},
	})
}

// A fork whose borrowed history is larger than the destination model's window
// asks before it spends anything: the fork would be refused by the provider on
// its first call, and the history can be compacted first.
func TestCompactAskOpensWhenOverWindow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("")
	compactForm(t, c)

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.Nil(cmd, "an oversized fork spawned instead of asking")
	ck.Require().NotNil(c.compactAsk, "no question was raised")
	ck.Eq("c_1", c.compactAsk.sourceChildID, "source id")
	ck.Eq("scout", c.compactAsk.sourceName, "source name")
	ck.Eq(compactSourceModel, c.compactAsk.sourceModel, "the intermediate runs on the SOURCE's model")
	ck.Eq(compactDestModel, c.compactAsk.destModel, "destination model")
	ck.Eq(int64(compactSourceToks), c.compactAsk.tokens, "the source's prompt size")
	ck.Eq(int32(compactDestWindow), c.compactAsk.window, "the destination's window")
	ck.False(c.compactAsk.switchMode, "a plain fork is not a switch")
	ck.Require().NotNil(c.form, "the form must stay open behind the question")
	ck.False(c.form.busy, "the form is not busy while the question is pending")
	ck.StrContains(c.notice, "y = compact on "+compactSourceModel, "the prompt names where the compaction runs")
}

// The affirmative runs the composition, and the cockpit's EXISTING spawn
// handling finishes it: no second close/rename lives here.
func TestCompactAskYesRunsTheComposition(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("")
	client := newCompactClient()
	c.client = client
	compactForm(t, c)

	_, cmd := c.handleKey(keyMsg("enter"))
	ck.Require().NotNil(c.compactAsk, "no question was raised")
	_, cmd = c.handleKey(keyMsg("y"))
	ck.Require().True(cmd != nil, "the affirmative runs the composition")
	ck.Nil(c.compactAsk, "answering must close the question")

	sm, ok := cmd().(spawnedMsg)
	ck.Require().True(ok, "the composition reports a spawnedMsg so applySpawned can finish it")
	ck.Nil(sm.err, "composition error")
	ck.Eq("c_new2", sm.childID, "the FORK's id, not the intermediate's")
	ck.Eq("", sm.switchSource, "a plain fork has no switch tail")

	ck.EqDeep([]string{
		"spawn", "send:c_new1", "spawn", "kill:c_new1", "close:c_new1",
	}, client.seen(), "spawn the intermediate, compact it, fork from it, stop it, then close it — a live child cannot be closed")

	reqs := client.requests()
	ck.Require().Eq(2, len(reqs), "spawn requests")
	ck.Eq("c_1", reqs[0].GetForkFrom().GetChildId(), "the intermediate forks from the SOURCE")
	ck.Eq(compactSourceModel, reqs[0].GetModel(), "the intermediate runs on the source's model")
	ck.Eq("c_new1", reqs[1].GetForkFrom().GetChildId(), "the fork borrows the COMPACTED history")
	ck.Eq(compactDestModel, reqs[1].GetModel(), "the fork runs on the destination model")

	// The existing path takes it from here: a successful spawn closes the form.
	c.applySpawned(sm)
	ck.Nil(c.form, "applySpawned closed the form")
}

// Any other key cancels, and cancelling costs nothing: the form keeps every
// field, so the answer to "it does not fit" can be a different model.
func TestCompactAskNoSpawnsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("")
	client := newCompactClient()
	c.client = client
	compactForm(t, c)

	_, cmd := c.handleKey(keyMsg("enter"))
	ck.Require().NotNil(c.compactAsk, "no question was raised")
	before := c.form.inputs[fieldModel].Value()

	_, cmd = c.handleKey(keyMsg("n"))

	ck.Nil(cmd, "a cancel must not run anything")
	ck.Nil(c.compactAsk, "any other key answers the question")
	ck.EqDeep([]string{}, client.seen(), "nothing reached the daemon")
	ck.Require().NotNil(c.form, "the form survives a cancel")
	ck.Eq(before, c.form.inputs[fieldModel].Value(), "the key did not edit the form")
	ck.StrContains(c.notice, "cancelled", "the cancel is reported")
}

// An unknown window is not "cannot fit". The question is skipped and the spawn
// goes ahead exactly as it did before this existed — for a model the catalog
// has no row for, and for a catalog that has not arrived.
func TestCompactAskSkippedWhenTheWindowIsUnknown(t *testing.T) {
	for name, rows := range map[string][]*rafikiv1.ModelRow{
		"no context window": {{Id: compactDestModel}},
		"no row at all":     {{Id: "some/other/model", ContextWindow: i32p(compactDestWindow)}},
		"nothing loaded":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c := newTestCockpit("")
			compactForm(t, c)
			seedModels(c, c.form.kind(), rows)

			_, cmd := c.handleKey(keyMsg("enter"))

			ck.Nil(c.compactAsk, "an unknown window must not raise the question")
			ck.Require().True(cmd != nil, "the spawn goes ahead")
			ck.True(c.form.busy, "the form is busy with the spawn it started")
		})
	}
}

// A switch that does not fit asks too, and the composition hands back the
// fields switchFinishCmd needs — so the close/rename tail is the SAME one a
// plain switch runs, not a copy.
func TestCompactAskSwitchReusesTheSwitchTail(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("")
	client := newCompactClient()
	c.client = client
	compactForm(t, c)
	c.form.switchSource = "c_1"

	_, cmd := c.handleKey(keyMsg("enter"))
	ck.Require().NotNil(c.compactAsk, "no question was raised")
	ck.True(c.compactAsk.switchMode, "a switch is named as one")
	_, cmd = c.handleKey(keyMsg("y"))
	ck.Require().True(cmd != nil, "the affirmative runs the composition")

	sm, ok := cmd().(spawnedMsg)
	ck.Require().True(ok, "the composition reports a spawnedMsg")
	ck.Nil(sm.err, "composition error")
	ck.Eq("c_1", sm.switchSource, "the source the tail must close")
	ck.Eq("scout", sm.sourceName, "the name the tail must take")
	ck.Eq("sort=price", sm.sourceRouting, "the routing the tail must replay")

	// Exactly what applySpawned dispatches for a switch.
	sw, ok := c.switchFinishCmd(sm.switchSource, sm.childID, sm.sourceName, sm.sourceRouting)().(switchedMsg)
	ck.Require().True(ok, "the switch tail answers a switchedMsg")
	ck.Nil(sw.err, "switch tail error")
	ck.Eq("c_new2", sw.newID, "the fork took the source's place")

	ck.EqDeep([]string{
		"getchild:c_1", "spawn", "send:c_new1", "spawn", "kill:c_new1", "close:c_new1",
		"kill:c_1", "close:c_1", "rename:scout", "routing:sort=price",
	}, client.seen(), "the source is read, the history compacted and its helper stopped then closed, then the ordinary switch tail")
	ck.Eq(protocol.KindFundi, client.requests()[1].GetKind(), "the fork is a fundi child")
}
