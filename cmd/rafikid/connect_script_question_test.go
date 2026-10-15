// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// questionSentence is the exact sentence the plan appends to the agent_report
// description on BOTH faces (fundi blueprint and MCP surface). The two must
// agree; this is the string both tests look for.
const questionSentence = ` With no parent, your note reaches the operator instead; there, kind "question" means you are stopping until they answer, so send it and end your turn.`

// scriptReports drains a subscription channel of the script_report payloads
// currently queued for it.
func scriptReports(ch <-chan *rafikiv1.Event) []*rafikiv1.ScriptReport {
	var out []*rafikiv1.ScriptReport
	for {
		select {
		case ev := <-ch:
			if sr := ev.GetScriptReport(); sr != nil {
				out = append(out, sr)
			}
		default:
			return out
		}
	}
}

// TestReportQuestionTopLevelSetsFieldAndPublishes: a top-level fundi child's
// `question` sets the stored PendingQuestion (decoded from the JSON string),
// publishes the existing script_report event, and drives the display state to
// waiting/operator once the refresh worker drains.
func TestReportQuestionTopLevelSetsFieldAndPublishes(t *testing.T) {
	ck := assert.NewAborting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_top", "", "", protocol.StatusIdle)

	ch, cancel := c.native.Subscribe("c_top")
	defer cancel()

	hub := c.connectScriptHub()
	ck.NoError(hub.Report(context.Background(), "c_top", "question", `"should I proceed?"`), "Report")

	snap, ok := c.st.Get("c_top")
	ck.Require().True(ok, "child vanished")
	ck.Eq("should I proceed?", snap.PendingQuestion, "PendingQuestion must be the decoded note")

	reports := scriptReports(ch)
	ck.Len(reports, 1, "exactly one script_report event publishes")
	ck.Eq("question", reports[0].GetKind(), "script_report kind")
	ck.Eq(`"should I proceed?"`, reports[0].GetDataJson(), "script_report carries the raw JSON")

	c.drainDisplayRefresh()
	last, ok := displayLastOf(c, "c_top")
	ck.Require().True(ok, "an operator state was published")
	ck.Eq(protocol.DisplayWaiting, last.state, "a top-level question is waiting")
	ck.Eq(protocol.WaitingOnOperator, last.on, "waiting on the operator")
}

// TestReportQuestionParentedChildSetsNothing: a child WITH a parent pushes its
// question to the parent's event buffer (its existing path) and must NOT set
// the stored field.
func TestReportQuestionParentedChildSetsNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	c, cap, _, clk := scriptHubFixture(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_coord", Name: "coord", Status: protocol.StatusIdle,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_kid", Name: "kid", Status: protocol.StatusStreaming,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
		Labels: map[string]string{
			childstore.LabelParent: "c_coord",
			childstore.LabelRoot:   "c_coord",
		},
	})

	hub := c.connectScriptHub()
	ck.NoError(hub.Report(context.Background(), "c_kid", "question", `"help?"`), "Report")

	snap, _ := c.st.Get("c_kid")
	ck.Eq("", snap.PendingQuestion, "a parented child's question must not set the field")

	clk.Advance(6 * time.Second)
	batches := cap.batches()
	ck.False(len(batches) != 1 || len(batches[0].fragments) != 1, "want 1 batch of 1 fragment, got %+v", batches)
	ck.StrContains(batches[0].fragments[0], "help?", "the parent's fragment carries the question")
	ck.StrContains(batches[0].fragments[0], "question", "the parent's fragment names the kind")
}

// TestReportQuestionScriptChildSetsNothing: a top-level SCRIPT child publishes
// the script_report (its durable record) and sets nothing — a script has no
// turns to clear the field.
func TestReportQuestionScriptChildSetsNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, elog, _ := scriptHubFixture(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_script", Name: "gen", Status: protocol.StatusStreaming,
		Kind: protocol.KindScript, StartedAt: time.Now(),
	})

	hub := c.connectScriptHub()
	ck.NoError(hub.Report(context.Background(), "c_script", "question", `"which branch?"`), "Report")

	snap, _ := c.st.Get("c_script")
	ck.Eq("", snap.PendingQuestion, "a script child's question must not set the field")

	recs, err := elog.Read(context.Background(), "c_script", -1, 10)
	ck.NoError(err, "event log read")
	ck.Len(recs, 1, "the script_report is still published")
	var ev rafikiv1.Event
	ck.NoError(protojson.Unmarshal(recs[0].Payload, &ev), "unmarshal payload")
	ck.Eq("question", ev.GetScriptReport().GetKind(), "script_report kind")
}

// TestReportQuestionOtherKindsDoNotSet: only `question` sets the field; every
// other kind on a top-level fundi child leaves it empty.
func TestReportQuestionOtherKindsDoNotSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := scriptHubFixture(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_top", Name: "top", Status: protocol.StatusStreaming,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
	})

	hub := c.connectScriptHub()
	ctx := context.Background()
	for _, kind := range []string{"warning", "message", "progress"} {
		ck.NoError(hub.Report(ctx, "c_top", kind, `"a note"`), "Report(%s)", kind)
		snap, _ := c.st.Get("c_top")
		ck.Eq("", snap.PendingQuestion, "kind %s must not set the field", kind)
	}
}

// TestAgentReportDescriptionMentionsOperatorOnMCPSurface: the MCP surface text
// for agent_report is built from the fundi blueprint, so it inherits the exact
// sentence the plan appends — both faces agree.
func TestAgentReportDescriptionMentionsOperatorOnMCPSurface(t *testing.T) {
	ck := assert.NewAborting(t)
	desc := mcpToolDescriptions["agent_report"]
	ck.True(desc != "", "the MCP surface overrides agent_report")
	ck.True(strings.Contains(desc, questionSentence),
		"the MCP agent_report text must carry the operator sentence; got %q", desc)
}

// TestReportQuestionParentedChildWithoutBufferSetsNothing pins the LOAD-BEARING
// parent clause of the guard. A PARENTED fundi child whose controller has no
// event buffer falls through to the script_report publish (the nil-buffer
// fallback), so `(!hasParent || parent == "")` is the ONLY thing keeping it from
// setting the field on that path. Assert the field stays empty AND the report
// still publishes durably.
func TestReportQuestionParentedChildWithoutBufferSetsNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	elog := eventlog.NewMemory()
	c := &Controller{st: childstore.New(), cm: newChildManager(), evlog: elog}
	c.st.Insert(&childstore.Session{
		ChildID: "c_coord", Name: "coord", Status: protocol.StatusIdle,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_kid", Name: "kid", Status: protocol.StatusStreaming,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
		Labels: map[string]string{
			childstore.LabelParent: "c_coord",
			childstore.LabelRoot:   "c_coord",
		},
	})

	hub := c.connectScriptHub()
	ck.NoError(hub.Report(context.Background(), "c_kid", "question", `"help?"`), "Report")

	snap, _ := c.st.Get("c_kid")
	ck.Eq("", snap.PendingQuestion, "a parented child with no buffer must not set the field")

	recs, err := elog.Read(context.Background(), "c_kid", -1, 10)
	ck.NoError(err, "event log read")
	ck.Len(recs, 1, "the script_report is still published")
}

// TestReportQuestionEmptyPayloadKeepsStoredQuestion: an empty rendered question
// is not a question — it must not CLEAR a pending question already on the row.
// The durable script_report still publishes either way.
func TestReportQuestionEmptyPayloadKeepsStoredQuestion(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := scriptHubFixture(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_top", Name: "top", Status: protocol.StatusStreaming,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
	})

	hub := c.connectScriptHub()
	ctx := context.Background()
	ck.NoError(hub.Report(ctx, "c_top", "question", `"are you there?"`), "first question")
	snap, _ := c.st.Get("c_top")
	ck.Eq("are you there?", snap.PendingQuestion, "the first question is stored")

	ck.NoError(hub.Report(ctx, "c_top", "question", `""`), "empty question")
	snap, _ = c.st.Get("c_top")
	ck.Eq("are you there?", snap.PendingQuestion, "an empty question must not clear the stored one")
}
