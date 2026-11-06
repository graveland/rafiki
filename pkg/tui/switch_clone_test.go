// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func switchFormFor(t *testing.T, name string) *Cockpit {
	t.Helper()
	c := profiledCockpit("switch-clone", Options{})
	sum := summaryFor("c_src", name, 0)
	sum.Kind = protocol.KindFundi
	sum.Model = "fakeor/mini"
	c.rail.Seed([]*rafikiv1.ChildSummary{sum})
	c.selected = "c_src"
	c.switchModelSelected()
	assert.NewAborting(t).Require().NotNil(c.form, "the form must be open")
	c.form.inputs[fieldModel].SetValue("openrouter/z-ai/glm-5.3-flash")
	return c
}

// The name row decides the operation: as it stands the fork replaces the
// source; changed or cleared, it branches from it and the source keeps running.
func TestSwitchFormNameRowSelectsSwitchOrClone(t *testing.T) {
	for name, tc := range map[string]struct {
		edit      func(*spawnForm)
		wantClone bool
		wantTitle string
	}{
		"untouched replaces":  {func(*spawnForm) {}, false, "switch model of c_src"},
		"renamed branches":    {func(f *spawnForm) { f.inputs[fieldName].SetValue("scout-2") }, true, "clone of c_src"},
		"cleared branches":    {func(f *spawnForm) { f.inputs[fieldName].SetValue("") }, true, "clone of c_src"},
		"padding is not edit": {func(f *spawnForm) { f.inputs[fieldName].SetValue("  scout ") }, false, "switch model of c_src"},
	} {
		t.Run(name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c := switchFormFor(t, "scout")
			tc.edit(c.form)
			p, problem := c.form.params(c.currency)
			ck.Eq("", problem, "params")
			ck.Eq(tc.wantClone, p.keepSource, "keepSource")
			ck.Eq(tc.wantClone, p.tailSource() == "", "a clone has no close-and-rename tail")
			ck.StrContains(c.form.view(100, 24, c.modelView, nil, c.currency), tc.wantTitle, "title names the operation")
		})
	}
}

// A clone spawns under the form's name and leaves the source alone; a switch
// spawns unnamed (the source holds the name until its close lands) and hands
// the tail the source to close.
func TestSwitchAndCloneSpawnRequests(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_x")
	client := newSwitchClient()
	c.client = client

	sw := spawnParams{
		name: "scout", kind: protocol.KindFundi, model: "m", cwd: "/x",
		switchSource: "c_src", forkFrom: &protocol.ForkFrom{ChildID: "c_src"},
	}
	sm, _ := switchMsg(t, c, sw)
	ck.Eq("c_src", sm.switchSource, "a switch's tail closes the source")
	ck.Eq("", client.spawned[0].GetName(), "a switch's fork spawns unnamed")

	client.spawned = nil
	client.events = nil
	cl := sw
	cl.name, cl.keepSource = "scout-2", true
	got, ok := c.switchSpawnCmd(cl)().(spawnedMsg)
	ck.Require().True(ok, "spawnedMsg")
	ck.Eq("", got.switchSource, "a clone has no tail")
	ck.Eq("scout-2", client.spawned[0].GetName(), "a clone spawns under the form's name")
	ck.Eq("/x", client.spawned[0].GetCwd(), "a clone uses the form's cwd")
	ck.EqDiff([]string{"getchild:c_src", "spawn"}, client.events, "a clone never stops or closes the source")
}

// /model is the cockpit's own line: it opens the switch form on the viewed
// child (with the argument as the model) and nothing is sent to the daemon.
func TestModelLineOpensTheSwitchForm(t *testing.T) {
	for line, wantModel := range map[string]string{
		"/model":                 "",
		"/model  openrouter/foo": "openrouter/foo",
	} {
		ck := assert.NewCollecting(t)
		c := newTestCockpit("c_1")
		sum := fundiSummary("c_1", "one", 0)
		c.rail.Seed([]*rafikiv1.ChildSummary{sum})
		c.ta.SetValue(line)

		_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

		ck.NotNil(cmd, "%q: the form fetches its model list", line)
		ck.Require().NotNil(c.form, "%q: the switch form must open", line)
		ck.Eq("c_1", c.form.switchSource, "%q: switch source", line)
		ck.Eq(wantModel, c.form.inputs[fieldModel].Value(), "%q: model row", line)
		ck.Eq("one", c.form.inputs[fieldName].Value(), "%q: name row", line)
		ck.Eq("", c.ta.Value(), "%q: the line is consumed, not sent", line)
	}
}

func TestModelLineRecognition(t *testing.T) {
	for line, want := range map[string]bool{
		"/model": true, "/model x": true, "/model x y": false,
		"/modelx": false, "model": false, "please /model": false, "": false,
	} {
		_, ok := modelLine(line)
		assert.NewCollecting(t).Eq(want, ok, "modelLine(%q)", line)
	}
}

func claudeSwitchCockpit(t *testing.T, model string, exited bool) *Cockpit {
	t.Helper()
	c := profiledCockpit("switch-claude", Options{})
	sum := summaryFor("c_src", "scout", 0)
	sum.Kind = protocol.KindClaude
	sum.Model = model
	if exited {
		sum.Status = "exited"
	}
	c.rail.Seed([]*rafikiv1.ChildSummary{sum})
	return c
}

// A claude-kind switch restarts the live child in place between Anthropic ids;
// every other pairing says why not, naming the way out, instead of quietly
// forking into another runtime.
func TestRestartPlan(t *testing.T) {
	sw := func(model string) spawnParams {
		return spawnParams{model: model, kind: protocol.KindClaude, switchSource: "c_src", forkFrom: &protocol.ForkFrom{ChildID: "c_src"}}
	}
	for name, tc := range map[string]struct {
		srcModel string
		exited   bool
		p        spawnParams
		wantWhy  string
	}{
		"anthropic to anthropic": {"claude-opus-5", false, sw("anthropic/claude-sonnet-5"), ""},
		"bare id":                {"claude-opus-5", false, sw("claude-sonnet-5"), ""},
		"to another provider":    {"claude-opus-5", false, sw("openrouter/z-ai/glm-5.3-flash"), "set kind to fundi"},
		"to a tier alias":        {"claude-opus-5", false, sw("anthropic/opus-latest"), "set kind to fundi"},
		"from a non-anthropic":   {"z-ai/glm-5.3-flash", false, sw("claude-sonnet-5"), "set kind to fundi"},
		"exited source":          {"claude-opus-5", true, sw("claude-sonnet-5"), "has exited"},
		"clone": {"claude-opus-5", false, func() spawnParams {
			p := sw("claude-sonnet-5")
			p.keepSource = true
			return p
		}(), "a clone is a fork"},
	} {
		t.Run(name, func(t *testing.T) {
			c := claudeSwitchCockpit(t, tc.srcModel, tc.exited)
			src, why := c.restartPlan(tc.p)
			ck := assert.NewCollecting(t)
			if tc.wantWhy == "" {
				ck.Eq("c_src", src, "the child to restart")
				ck.Eq("", why, "no refusal")
				return
			}
			ck.Eq("", src, "nothing to restart")
			ck.StrContains(why, tc.wantWhy, "the refusal names the way out")
		})
	}
}

// A claude source's switch form opens on kind claude with the row unlocked; a
// fundi source's stays locked to its fork.
func TestSwitchFormKindRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeSwitchCockpit(t, "claude-opus-5", false)
	c.selected = "c_src"
	c.switchModelSelected()
	ck.Require().NotNil(c.form, "form")
	ck.Eq(protocol.KindClaude, c.form.kind(), "a claude source keeps claude by default")
	ck.False(c.form.kindLocked(), "the kind row is a choice")
	ck.Eq("claude-opus-5", c.form.inputs[fieldModel].Value(), "model prefilled")

	// Flipping to fundi blanks the model (the source's id belongs to claude),
	// and flipping back restores it; the rest of the prefill is untouched.
	c.form.inputs[fieldName].SetValue("scout")
	c.form.cycleKind(+1)
	c.kindChanged()
	ck.Eq(protocol.KindFundi, c.form.kind(), "kind")
	ck.Eq("", c.form.inputs[fieldModel].Value(), "the claude id is not a fundi model")
	ck.Eq("scout", c.form.inputs[fieldName].Value(), "name kept")
	c.form.cycleKind(+1)
	c.kindChanged()
	ck.Eq("claude-opus-5", c.form.inputs[fieldModel].Value(), "model restored on the source's own kind")

	ck.True(switchFormFor(t, "scout").form.kindLocked(), "a fundi source's fork is fundi only")
}

// Kind claude with a model a restart cannot carry stays in the form with the
// reason; flipping the kind to fundi forks it, as before.
func TestClaudeSwitchToOpenRouterNeedsFundiKind(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeSwitchCockpit(t, "claude-opus-5", false)
	client := &sendClient{}
	c.client = client
	c.selected = "c_src"
	c.switchModelSelected()
	c.form.inputs[fieldModel].SetValue("openrouter/z-ai/glm-5.3-flash")

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ck.Nil(cmd, "nothing is sent")
	ck.Require().NotNil(c.form, "the form stays open")
	ck.StrContains(c.form.err, "set kind to fundi", "reason")
	ck.Len(client.sent, 0, "no /model")

	c.form.setKind(protocol.KindFundi)
	p, problem := c.form.params(c.currency)
	ck.Eq("", problem, "params")
	ck.Eq(protocol.KindFundi, p.kind, "the fork runs as fundi")
	ck.Eq("c_src", p.tailSource(), "and replaces the source")
}

type sendClient struct {
	switchClient
	sent []*rafikiv1.SendRequest
}

func (f *sendClient) Send(_ context.Context, req *connect.Request[rafikiv1.SendRequest]) (*connect.Response[rafikiv1.SendResponse], error) {
	f.sent = append(f.sent, req.Msg)
	return connect.NewResponse(&rafikiv1.SendResponse{}), nil
}

// Submitting a claude→claude switch sends the daemon `/model <id>` as a prompt
// to the source and spawns nothing.
func TestClaudeSwitchSubmitSendsTheModelCommand(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeSwitchCockpit(t, "claude-opus-5", false)
	client := &sendClient{}
	c.client = client
	c.selected = "c_src"
	c.switchModelSelected()
	ck.Require().NotNil(c.form, "form")
	c.form.inputs[fieldModel].SetValue("anthropic/claude-sonnet-5")

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ck.Require().NotNil(cmd, "submit issues a command")
	cmd()

	ck.Nil(c.form, "the form closes")
	ck.Require().Len(client.sent, 1, "one send")
	ck.Eq("c_src", client.sent[0].GetChildId(), "to the source")
	ck.Eq(rafikiv1.SendMode_SEND_MODE_PROMPT, client.sent[0].GetMode(), "as a prompt, so the daemon interprets it")
	ck.Require().Len(client.sent[0].GetBlocks(), 1, "one block")
	ck.Eq("/model anthropic/claude-sonnet-5", client.sent[0].GetBlocks()[0].GetText().GetText(), "the command")
	ck.Len(client.spawned, 0, "nothing is forked")
}
