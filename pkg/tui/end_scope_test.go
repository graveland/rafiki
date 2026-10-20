// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// recordingControl records the scope every Kill and Close arrives with.
type recordingControl struct {
	rafikiv1connect.UnimplementedControlHandler
	mu    sync.Mutex
	calls []string
	scope map[string]rafikiv1.DescendantScope
}

func (r *recordingControl) note(verb string, scope rafikiv1.DescendantScope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, verb)
	if r.scope == nil {
		r.scope = map[string]rafikiv1.DescendantScope{}
	}
	r.scope[verb] = scope
}

func (r *recordingControl) Kill(_ context.Context, req *connect.Request[rafikiv1.KillRequest]) (*connect.Response[rafikiv1.KillResponse], error) {
	r.note("kill", req.Msg.GetScope())
	return connect.NewResponse(&rafikiv1.KillResponse{ChildId: req.Msg.GetChildId()}), nil
}

func (r *recordingControl) Close(_ context.Context, req *connect.Request[rafikiv1.CloseRequest]) (*connect.Response[rafikiv1.CloseResponse], error) {
	r.note("close", req.Msg.GetScope())
	return connect.NewResponse(&rafikiv1.CloseResponse{ChildId: req.Msg.GetChildId()}), nil
}

func endScopeCockpit(t *testing.T, summaries ...*rafikiv1.ChildSummary) (*Cockpit, *recordingControl) {
	t.Helper()
	rc := &recordingControl{}
	mux := http.NewServeMux()
	mux.Handle(rafikiv1connect.NewControlHandler(rc))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewCockpit(Options{BaseURL: srv.URL, ChildID: "c_a"})
	c.rail.Seed(summaries)
	c.focus = focusRail
	c.selected = "c_a"
	return c, rc
}

func exitedSummary(s *rafikiv1.ChildSummary) *rafikiv1.ChildSummary {
	s.Status = "exited"
	return s
}

func pressKey(c *Cockpit, key string) tea.Cmd {
	_, cmd := c.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
	return cmd
}

// askEnd drives x twice, leaving the end question pending.
func askEnd(t *testing.T, c *Cockpit) {
	t.Helper()
	pressX(c)
	pressX(c)
	assert.NewAborting(t).True(c.endAsk != nil, "the end question is pending")
}

func TestEndAskKeysSelectTheScope(t *testing.T) {
	for key, want := range map[string]rafikiv1.DescendantScope{
		"a": rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_AND_DESCENDANTS,
		"o": rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY,
		"k": rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY,
	} {
		t.Run("stop "+key, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c, rc := endScopeCockpit(t, summaryFor("c_a", "alpha", 0), childOf("c_b", "bravo", "c_a"))
			askEnd(t, c)
			cmd := pressKey(c, key)
			ck.Require().True(cmd != nil, "the answer acts")
			cmd()
			ck.EqDeep([]string{"kill"}, rc.calls, "calls")
			ck.Eq(want, rc.scope["kill"], "scope sent")
		})
	}
}

func TestEndAskCloseOffersExitedOnlyWhenThereAreExitedSubagents(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _ := endScopeCockpit(t,
		exitedSummary(summaryFor("c_a", "alpha", 0)),
		exitedSummary(childOf("c_b", "bravo", "c_a")),
		childOf("c_c", "charlie", "c_a"))
	askEnd(t, c)
	ck.Eq(1, c.endAsk.exitedKids, "one exited subagent counted")
	ck.StrContains(c.notice, "e = close only its exited ones", "prompt offers it")
	ck.StrContains(c.notice, "k = only its subagents", "prompt offers kids-only")

	live, _ := endScopeCockpit(t, summaryFor("c_a", "alpha", 0), childOf("c_b", "bravo", "c_a"))
	askEnd(t, live)
	ck.False(strings.Contains(live.notice, "exited ones"), "a stop never offers exited-only")
}

func TestEndAskExitedAnswerClosesWithoutStoppingAnything(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, rc := endScopeCockpit(t,
		exitedSummary(summaryFor("c_a", "alpha", 0)),
		exitedSummary(childOf("c_b", "bravo", "c_a")),
		childOf("c_c", "charlie", "c_a"))
	askEnd(t, c)
	cmd := pressKey(c, "e")
	ck.Require().True(cmd != nil, "the answer acts")
	msg := cmd()
	ck.EqDeep([]string{"close"}, rc.calls, "exited-only close stops nothing")
	ck.Eq(rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY, rc.scope["close"], "scope sent")
	cm, ok := msg.(closedMsg)
	ck.Require().True(ok, "a closedMsg comes back")
	ck.True(cm.keptTarget, "the target is kept")
}

func TestEndAskExitedAnswerIsIgnoredWithoutExitedSubagents(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, rc := endScopeCockpit(t, summaryFor("c_a", "alpha", 0), childOf("c_b", "bravo", "c_a"))
	askEnd(t, c)
	ck.True(pressKey(c, "e") == nil, "e is not an answer for a stop")
	ck.Nil(c.endAsk, "the question is gone")
	ck.Eq(0, len(rc.calls), "nothing was sent")
}

func TestEndAskKidsOnCloseStopsLiveSubagentsThenClosesThemOnly(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, rc := endScopeCockpit(t,
		exitedSummary(summaryFor("c_a", "alpha", 0)),
		childOf("c_b", "bravo", "c_a"))
	askEnd(t, c)
	cmd := pressKey(c, "k")
	ck.Require().True(cmd != nil, "the answer acts")
	cmd()
	ck.EqDeep([]string{"kill", "close"}, rc.calls, "live subagents are stopped before the close")
	ck.Eq(rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY, rc.scope["kill"], "kill scope")
	ck.Eq(rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY, rc.scope["close"], "close scope")
}

func TestApplyClosedKeepsTheTargetWhenOnlySubagentsWereClosed(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_a", "alpha", 0), childOf("c_b", "bravo", "c_a")})
	c.applyClosed(closedMsg{childID: "c_a", name: "alpha", descendants: []string{"c_b"}, keptTarget: true})
	_, hasTarget := c.rail.Get("c_a")
	_, hasKid := c.rail.Get("c_b")
	ck.True(hasTarget, "the target row stays")
	ck.False(hasKid, "the closed subagent row goes")
	ck.StrContains(c.notice, "closed 1 subagent of alpha", "notice")
}
