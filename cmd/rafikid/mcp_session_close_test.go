package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/server"
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

// A request inside an existing session is answered with the shared version
// probe, not a freshly built tool server: the SDK asks getServer on every
// request and reads only the protocol versions from the answer. A request
// without a session header still gets its own tool-bearing server, and an
// uncredentialed caller is refused exactly as before.
func TestGetServerAnswersSessionRequestsWithTheVersionProbe(t *testing.T) {
	c := assert.NewAborting(t)
	face, _ := mcpFaceFixture(t)

	withSession := func(r *http.Request) *http.Request {
		r.Header.Set(mcpSessionHeader, "sid-1")
		return r
	}
	for name, mk := range map[string]func() *http.Request{
		"user":  func() *http.Request { return mcpRequestFor("u1") },
		"child": func() *http.Request { return mcpChildRequest("u1", "c_kid", false) },
	} {
		first := face.getServer(withSession(mk()))
		second := face.getServer(withSession(mk()))
		c.True(first != nil && first == second, "%s: session requests share one server", name)
		c.True(first == face.versionProbe, "%s: and it is the version probe", name)

		fresh := face.getServer(mk())
		c.True(fresh != nil && fresh != face.versionProbe, "%s: an initialize request builds a real server", name)
		c.True(fresh != face.getServer(mk()), "%s: each initialize request gets its own server", name)
	}

	// The probe carries no tools.
	probeTools := mcpToolNames(t, mcpConnect(t, face.versionProbe))
	c.Eq(0, len(probeTools), "the probe has no tools")

	// An identity that is neither a user nor a child credential is refused
	// whether or not it names a session.
	stranger := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	stranger.Header.Set(mcpSessionHeader, "sid-1")
	c.True(face.getServer(stranger) == nil, "a request with no identity gets no server")
}

// fakeClock is a settable clock for the face's idle bookkeeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// childFaceServer serves the face's real handler over HTTP as child c_kid.
func childFaceServer(t *testing.T, face *mcpFace) (*httptest.Server, string) {
	t.Helper()
	path, h := face.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(server.WithIdentity(r.Context(), &server.Identity{
			UserID: "u1", Username: "u1", ChildID: "c_kid", Via: server.ProvenanceChildToken,
		}))
		h.ServeHTTP(w, r)
	}))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, path
}

func rawMCPPost(t *testing.T, ts *httptest.Server, path, sid, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	assert.NewAborting(t).NoError(err, "new request")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set(mcpSessionHeader, sid)
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	}
	resp, err := http.DefaultClient.Do(req)
	assert.NewAborting(t).NoError(err, "do")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func faceInflight(f *mcpFace, sid string) int {
	f.sessionsMu.Lock()
	defer f.sessionsMu.Unlock()
	if a := f.activity[sid]; a != nil {
		return a.inflight
	}
	return -1
}

// The sweep closes a child session that has no connection and no recent use,
// and only that one: a session holding an open stream is a connected client and
// stays however long it is quiet, and a freshly used one stays too. A closed
// session answers 404, which is what makes its client start a new one.
func TestSweepClosesAChildSessionOnlyOnceItHasNoConnectionAndNoRecentUse(t *testing.T) {
	c := assert.NewAborting(t)
	face, _ := mcpFaceFixture(t)
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	face.now = clock.Now
	ts, path := childFaceServer(t, face)

	initResp, _ := rawMCPPost(t, ts, path, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"sweep-test","version":"1"}}}`)
	sid := initResp.Header.Get(mcpSessionHeader)
	c.NotEq("", sid, "initialize names a session")
	rawMCPPost(t, ts, path, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	waitUntil(t, "the session to be tracked", func() bool { return trackedChildren(face) == 1 })

	// Just used: not swept.
	c.Eq(0, face.sweepIdleChildSessions(mcpChildSessionIdle), "a fresh session is not idle")

	// An open stream is a connection: the session survives any quiet period.
	streamCtx, dropStream := context.WithCancel(context.Background())
	streamReq, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, ts.URL+path, nil)
	streamReq.Header.Set("Accept", "text/event-stream")
	streamReq.Header.Set(mcpSessionHeader, sid)
	streamReq.Header.Set("MCP-Protocol-Version", "2025-06-18")
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		if resp, err := http.DefaultClient.Do(streamReq); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitUntil(t, "the stream to be in flight", func() bool { return faceInflight(face, sid) == 1 })
	clock.Advance(3 * mcpChildSessionIdle)
	c.Eq(0, face.sweepIdleChildSessions(mcpChildSessionIdle), "a session with an open stream is connected, however quiet")
	resp, _ := rawMCPPost(t, ts, path, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	c.Eq(http.StatusOK, resp.StatusCode, "the connected session still answers")

	// The client goes away. The stream ends, but the session was just used.
	dropStream()
	<-streamDone
	waitUntil(t, "the stream request to finish", func() bool { return faceInflight(face, sid) == 0 })
	c.Eq(0, face.sweepIdleChildSessions(mcpChildSessionIdle), "its last request just finished")

	clock.Advance(mcpChildSessionIdle + time.Second)
	c.Eq(1, face.sweepIdleChildSessions(mcpChildSessionIdle), "no connection and quiet for the idle period")

	waitUntil(t, "the closed session to leave the tracking map", func() bool { return trackedChildren(face) == 0 })
	resp, _ = rawMCPPost(t, ts, path, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	c.Eq(http.StatusNotFound, resp.StatusCode, "a closed session answers 404 so its client starts a new one")
	c.False(face.sessionKnown(sid), "the face forgot the id")
	c.Eq(-1, faceInflight(face, sid), "and its activity record")

	// A new session works, and a user's session is never swept.
	again, _ := rawMCPPost(t, ts, path, "", `{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"sweep-test","version":"1"}}}`)
	c.Eq(http.StatusOK, again.StatusCode, "re-initialize after the 404")
}

// A server-side close by the face (a child's exit) also forgets the session
// id, so a late request gets the 404 that starts a new session rather than a
// 403 for an id the face no longer holds.
func TestChildExitCloseForgetsTheSessionIDAndAnswers404(t *testing.T) {
	c := assert.NewAborting(t)
	face, _ := mcpFaceFixture(t)
	ts, path := childFaceServer(t, face)

	initResp, _ := rawMCPPost(t, ts, path, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"exit-test","version":"1"}}}`)
	sid := initResp.Header.Get(mcpSessionHeader)
	rawMCPPost(t, ts, path, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	waitUntil(t, "the session to be tracked", func() bool { return trackedChildren(face) == 1 })

	face.closeChildSessions("c_kid")

	waitUntil(t, "the id to be forgotten", func() bool { return !face.sessionKnown(sid) })
	resp, _ := rawMCPPost(t, ts, path, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	c.Eq(http.StatusNotFound, resp.StatusCode, "a session closed at exit answers 404")
}
