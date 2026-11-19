package main

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/multigres/testkit/assert"
)

// childSessionPair connects one real SDK server session to an in-memory client.
func childSessionPair(t *testing.T) (*mcp.ServerSession, *mcp.ClientSession) {
	t.Helper()
	c := assert.NewAborting(t)
	st, ct := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "rafiki-test", Version: "test"}, nil)
	ss, err := srv.Connect(context.Background(), st, nil)
	c.NoError(err, "server connect")
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "test"}, nil).Connect(context.Background(), ct, nil)
	c.NoError(err, "client connect")
	t.Cleanup(func() { _ = ss.Close() })
	t.Cleanup(func() { _ = cs.Close() })
	return ss, cs
}

func waitClosed(t *testing.T, ss *mcp.ServerSession) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = ss.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func trackedChildren(f *mcpFace) int {
	f.childSessionsMu.Lock()
	defer f.childSessionsMu.Unlock()
	return len(f.childSessions)
}

// A child's exit closes its sessions and no one else's: the SDK keeps an
// initialized session until its client disconnects, and a child that dies
// without a DELETE would otherwise leave it open for the daemon's lifetime.
func TestForgetMCPTokenClosesTheChildsSessions(t *testing.T) {
	c := assert.NewAborting(t)
	face := newMCPFace(discardLogger(), nil, nil, "test")
	ctrl := &Controller{}
	ctrl.SetMCPSessionCloser(face.closeChildSessions)

	dying1, _ := childSessionPair(t)
	dying2, _ := childSessionPair(t)
	other, _ := childSessionPair(t)
	face.trackChildSession("c_dying", dying1)
	face.trackChildSession("c_dying", dying1) // reachable from both hooks: tracked once
	face.trackChildSession("c_dying", dying2)
	face.trackChildSession("c_other", other)

	ctrl.forgetMCPToken("c_dying")

	c.True(waitClosed(t, dying1), "the exited child's first session must close")
	c.True(waitClosed(t, dying2), "the exited child's second session must close")
	face.childSessionsMu.Lock()
	_, stillTracked := face.childSessions["c_dying"]
	_, otherTracked := face.childSessions["c_other"]
	face.childSessionsMu.Unlock()
	c.False(stillTracked, "the exited child must not stay tracked")
	c.True(otherTracked, "another child's session must stay tracked")

	closedOther := make(chan struct{})
	go func() { _ = other.Wait(); close(closedOther) }()
	select {
	case <-closedOther:
		t.Fatal("another child's session was closed")
	case <-time.After(200 * time.Millisecond):
	}
}

// A session whose client disconnects on its own drops out of the tracking map,
// so a long-lived child that reconnects does not accumulate dead entries.
func TestTrackedChildSessionRemovesItselfOnDisconnect(t *testing.T) {
	c := assert.NewAborting(t)
	face := newMCPFace(discardLogger(), nil, nil, "test")
	ss, cs := childSessionPair(t)
	face.trackChildSession("c_live", ss)
	c.Eq(1, trackedChildren(face), "tracked while connected")

	c.NoError(cs.Close(), "client disconnect")
	deadline := time.Now().Add(5 * time.Second)
	for trackedChildren(face) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	c.Eq(0, trackedChildren(face), "a disconnected session must be dropped")
}

// The face's own hooks register a child caller's session, and the child's exit
// ends it. A user caller's sessions are never tracked: they outlive any agent.
func TestMCPFaceTracksChildSessionsAndExitClosesThem(t *testing.T) {
	c := assert.NewAborting(t)
	face, _ := mcpFaceFixture(t)
	ctrl := face.controller()
	ctrl.SetMCPSessionCloser(face.closeChildSessions)

	childCS := mcpConnect(t, face.getServer(mcpChildRequest("u1", "c_kid", false)))
	_, err := childCS.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_list"})
	c.NoError(err, "child tool call")
	userCS := mcpConnect(t, face.getServer(mcpRequestFor("u1")))
	_, err = userCS.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_list"})
	c.NoError(err, "user tool call")

	face.childSessionsMu.Lock()
	tracked := len(face.childSessions["c_kid"])
	total := len(face.childSessions)
	face.childSessionsMu.Unlock()
	c.Eq(1, tracked, "the child's session must be tracked after its first tool call")
	c.Eq(1, total, "a user caller's session must not be tracked")

	ctrl.forgetMCPToken("c_kid")

	done := make(chan struct{})
	go func() { _ = childCS.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the exited child's MCP session was not closed")
	}
	_, err = userCS.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_list"})
	c.NoError(err, "the user's session must survive a child's exit")
}
