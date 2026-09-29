// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// registryCount reads how many streams are registered, under the registry's
// own lock — this test is in-package, so it sees the map directly instead of
// inventing a counter the production code would have to carry.
func registryCount(reg *streamRegistry) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	n := 0
	for _, m := range reg.byToken {
		n += len(m)
	}
	return n
}

// fakeStreamConn is a minimal connect.StreamingHandlerConn for the direct
// interceptor tests: the interceptor reads Spec and the identity from ctx,
// and the handler underneath reads nothing.
type fakeStreamConn struct {
	spec connect.Spec
	hdr  http.Header
}

func (c *fakeStreamConn) Spec() connect.Spec           { return c.spec }
func (c *fakeStreamConn) Peer() connect.Peer           { return connect.Peer{} }
func (c *fakeStreamConn) Receive(any) error            { return io.EOF }
func (c *fakeStreamConn) RequestHeader() http.Header   { return c.hdr }
func (c *fakeStreamConn) Send(any) error               { return nil }
func (c *fakeStreamConn) ResponseHeader() http.Header  { return http.Header{} }
func (c *fakeStreamConn) ResponseTrailer() http.Header { return http.Header{} }

func TestStreamRegistryRevokeToken(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	var first, second, other bool
	removeFirst := reg.add("tok-1", "u1", func() { first = true })
	removeSecond := reg.add("tok-1", "u1", func() { second = true })
	reg.add("tok-2", "u1", func() { other = true })

	c.Eq(2, reg.revokeToken("tok-1"), "both streams under tok-1 must be counted")
	c.True(first && second, "tok-1's streams must be cancelled")
	c.False(other, "tok-2's stream must not be touched by tok-1's revocation")

	// Idempotent: the second revoke finds nothing and cancels nothing.
	c.Eq(0, reg.revokeToken("tok-1"))
	c.True(first && second, "a repeat revoke must not cancel a second time")

	// remove after revoke: the entry is already gone; still no panic, and no
	// resurrection — and tok-2's unrelated registration is untouched.
	removeFirst()
	removeSecond()
	c.Eq(1, registryCount(reg), "only tok-2's stream remains after tok-1 is revoked and removed")

	c.Eq(1, reg.revokeToken("tok-2"))
	c.True(other, "tok-2's stream must be cancelled by its own revocation")
	c.Eq(0, registryCount(reg), "an emptied token must leave no registry residue")
}

func TestStreamRegistryRevokeUser(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	var first, second, foreign bool
	reg.add("tok-1", "u1", func() { first = true })
	reg.add("tok-2", "u1", func() { second = true })
	reg.add("tok-3", "u2", func() { foreign = true })

	c.Eq(2, reg.revokeUser("u1"), "every credential's stream the user holds must be counted")
	c.True(first && second, "u1's streams must be cancelled")
	c.False(foreign, "another user's stream must survive")

	c.Eq(0, reg.revokeUser("u1"), "a repeat revoke finds nothing")
	// An empty userID can match nothing registered — a user credential always
	// has a non-empty id — so it must never become a mass-cut.
	c.Eq(0, reg.revokeUser(""))
	c.False(foreign, "an empty-owner revoke must not cut other users' streams")
	c.Eq(1, registryCount(reg), "u2's stream stays registered")
}

func TestStreamRegistryRemoveIdempotent(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	fired := false
	remove := reg.add("tok-1", "u1", func() { fired = true })

	c.Eq(1, registryCount(reg))
	remove()
	c.Eq(0, registryCount(reg))
	remove() // idempotent: no panic, no state change, no cancel
	c.Eq(0, registryCount(reg))
	c.False(fired, "remove deregisters; only revocation cancels")

	// remove-after-revoke: the revoke already deleted the entry.
	remove2 := reg.add("tok-2", "u1", func() { fired = true })
	c.Eq(1, reg.revokeToken("tok-2"))
	c.True(fired, "revocation cancels")
	remove2()
	c.Eq(0, registryCount(reg))

	// A stream removed on normal completion leaves no empty bucket behind,
	// so the token key cannot linger as a phantom revocation target.
	remove3 := reg.add("tok-3", "u1", func() {})
	remove3()
	reg.mu.Lock()
	_, exists := reg.byToken["tok-3"]
	reg.mu.Unlock()
	c.False(exists, "an emptied token's bucket must be deleted")
}

// TestStreamRegistryConcurrent drives add, remove, revokeToken and revokeUser
// from many goroutines at once. The race detector is the real assertion; the
// watchdog is so a future deadlock fails instead of hanging the suite, and
// the final count is so a lost or duplicated entry cannot hide behind the
// deadline.
func TestStreamRegistryConcurrent(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				// Atomic because a concurrent revokeUser from another goroutine
				// may be the one to cancel this stream.
				var fired atomic.Bool
				tokenID := fmt.Sprintf("tok-%c-%d", 'a'+i%6, j%4)
				remove := reg.add(tokenID, fmt.Sprintf("u-%c", 'a'+i%6), func() { fired.Store(true) })
				if j%3 == 0 {
					reg.revokeToken(tokenID)
					if !fired.Load() {
						t.Errorf("revokeToken returned without cancelling the stream it counted")
					}
				}
				remove()
			}
		}(i)
	}
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				reg.revokeUser(fmt.Sprintf("u-%c", 'a'+k))
			}
		}(k)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("registry operations did not settle: deadlock under contention")
	}
	c.Eq(0, registryCount(reg), "every entry must have been removed or revoked exactly once")
}

// TestStreamInterceptorSkipsNonUser pins what registers and what does not:
// only a real user credential with a non-empty TokenID becomes cuttable. The
// child credentials are checked by PROVENANCE, not by TokenID emptiness —
// both child cases below carry a TokenID and must still be skipped — and a
// user credential without a token row must not register under the empty key,
// where revokeToken("") would one day cut it as a bucket.
func TestStreamInterceptorSkipsNonUser(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	ic := streamRevocationInterceptor(reg)
	spec := connect.Spec{Procedure: "/rafiki.v1.Control/ExecutorSession", StreamType: connect.StreamTypeServer}

	for _, tc := range []struct {
		name     string
		id       *server.Identity
		register int
	}{
		{"nil identity", nil, 0},
		{"per-child secret", &server.Identity{UserID: "u1", ChildID: "c1", Via: server.ProvenanceChildToken, TokenID: "tok-1"}, 0},
		{"per-boot attributed", &server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed, TokenID: "tok-1"}, 0},
		{"user without a token row", &server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser}, 0},
		{"user credential", &server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser, TokenID: "tok-1"}, 1},
	} {
		seen := -1
		handler := ic.WrapStreamingHandler(connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
			seen = registryCount(reg)
			return nil
		}))
		ctx := context.Background()
		if tc.id != nil {
			ctx = server.WithIdentity(ctx, tc.id)
		}
		if err := handler(ctx, &fakeStreamConn{spec: spec, hdr: http.Header{}}); err != nil {
			t.Errorf("%s: handler err = %v", tc.name, err)
		}
		c.Eq(tc.register, seen, "%s: streams registered when the handler ran", tc.name)
	}
	c.Eq(0, registryCount(reg), "the registered case must deregister on return")
}

// TestStreamInterceptorAfterPolicy pins the ORDER: the stream-revocation
// interceptor sits behind the policy gate, so a refused stream never
// registers. ExecutorSession is userOnly and server-streaming, and the
// server has no session backend wired — if the gate did not run first, the
// call would answer Unavailable rather than PermissionDenied, so the code
// proves which layer refused.
func TestStreamInterceptorAfterPolicy(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	auth := server.NewUserTokenAuth(
		fakeUserStore{token: proxyUserToken, id: users.Identity{UserID: "u1", Username: "brent", TokenID: "tok-1"}},
		proxyBootToken,
		server.DefaultAuthCacheTTL,
	)
	auth.SetChildTokenLookup(func(token string) (childID, ownerUserID string, ok bool) {
		if token == proxyChildToken {
			return "c_child", "u1", true
		}
		return "", "", false
	})
	auth.SetChildOwnerLookup(func(childID string) (string, bool) {
		if childID == "c_child" {
			return "u1", true
		}
		return "", false
	})

	srv := connectapi.NewServer(store.NewMessages(nil))
	h := &server.Handler{}
	h.ControlPath, h.Control = connectControlRoute(srv, reg)
	mux := http.NewServeMux()
	h.Mount(mux, auth.Middleware)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	client := rafikiv1connect.NewControlClient(ts.Client(), ts.URL)

	req := connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "watcher"})
	req.Header().Set("Authorization", "Bearer "+proxyChildToken)
	stream, callErr := client.ExecutorSession(context.Background(), req)
	var code connect.Code
	if callErr != nil {
		code = connect.CodeOf(callErr)
	} else {
		if !stream.Receive() {
			code = connect.CodeOf(stream.Err())
		}
	}
	c.Eq(connect.CodePermissionDenied, connect.Code(code), "the gate must refuse the child credential, got %v", code)
	c.Eq(0, registryCount(reg), "a policy-refused stream must never register")
}

// expiringUserStore models a credential whose expiry has passed: it no longer
// authenticates. An open stream is never re-authenticated, which is exactly
// the point the test pins.
type expiringUserStore struct {
	users.Store
	token   string
	id      users.Identity
	expired atomic.Bool
}

func (s *expiringUserStore) Authenticate(_ context.Context, token string) (users.Identity, error) {
	if s.expired.Load() || token != s.token {
		return users.Identity{}, users.ErrNotFound
	}
	return s.id, nil
}

// fakeEventSource is an EventSource the test pushes into. Subscribe marks the
// subscription atomically so the test can send only once the handler is
// actually listening.
type fakeEventSource struct {
	subscribed atomic.Bool

	mu sync.Mutex
	ch []chan *rafikiv1.Event
}

func (s *fakeEventSource) Subscribe(string) (<-chan *rafikiv1.Event, func()) {
	return s.subscribe()
}

func (s *fakeEventSource) SubscribeAll() (<-chan *rafikiv1.Event, func()) {
	return s.subscribe()
}

func (s *fakeEventSource) subscribe() (<-chan *rafikiv1.Event, func()) {
	s.subscribed.Store(true)
	ch := make(chan *rafikiv1.Event, 8)
	s.mu.Lock()
	s.ch = append(s.ch, ch)
	s.mu.Unlock()
	return ch, func() { close(ch) }
}

func (s *fakeEventSource) send(ev *rafikiv1.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.ch {
		ch <- ev
	}
}

// stubLineage satisfies eventlog.Lineage; with an empty subject selector the
// stream's filter never consults it, so every child matches.
type stubLineage struct{}

func (stubLineage) DescendantDepth(ancestorID, candidateID string) int { return -1 }
func (stubLineage) Labels(childID string) (map[string]string, bool)    { return nil, false }

// openEvents opens a StreamEvents client under a watchdog. The connect
// client's open call blocks until the server writes response headers, which
// StreamEvents only does on its first event — so the caller must already have
// arranged for one to be sent (see the test below), and a handler that never
// subscribes must fail the test rather than hang it.
func openEvents(t *testing.T, client rafikiv1connect.ControlClient, req *connect.Request[rafikiv1.StreamEventsRequest]) *connect.ServerStreamForClient[rafikiv1.Event] {
	t.Helper()
	type opened struct {
		stream *connect.ServerStreamForClient[rafikiv1.Event]
		err    error
	}
	ch := make(chan opened, 1)
	go func() {
		stream, err := client.StreamEvents(context.Background(), req)
		ch <- opened{stream: stream, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("open stream: %v", r.err)
		}
		return r.stream
	case <-time.After(10 * time.Second):
		t.Fatal("the server never accepted the event stream")
		return nil
	}
}

// TestExpiredTokenStreamContinues pins the design line verbatim: a stream
// opened before its token expired keeps running — deliberately. The
// credential stops authenticating, and the open stream still delivers;
// nothing in the daemon re-checks a stream's credential, and expiry is a
// property of time, not an act. The deliberate act — revokeToken — still
// cuts it afterwards.
func TestExpiredTokenStreamContinues(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	ustore := &expiringUserStore{
		token: proxyUserToken,
		id:    users.Identity{UserID: "u1", Username: "brent", TokenID: "tok-1"},
	}
	src := &fakeEventSource{}
	srv := connectapi.NewServer(store.NewMessages(nil))
	srv.SetEventSource(src)
	srv.SetLineage(stubLineage{})
	h := &server.Handler{}
	h.ControlPath, h.Control = connectControlRoute(srv, reg)
	mux := http.NewServeMux()
	h.Mount(mux, server.NewUserTokenAuth(ustore, proxyBootToken, server.DefaultAuthCacheTTL).Middleware)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	client := rafikiv1connect.NewControlClient(ts.Client(), ts.URL)

	req := connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c1"}},
	})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)

	// The connect client's open call blocks until response headers arrive,
	// and StreamEvents writes those only with its first event — so the first
	// event goes out from a goroutine the moment the handler subscribes.
	event := func() *rafikiv1.Event {
		return &rafikiv1.Event{ChildId: "c1", Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{}}}
	}
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if src.subscribed.Load() {
				src.send(event())
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	stream := openEvents(t, client, req)
	c.Eq(1, registryCount(reg), "the open stream is registered under its credential")

	if !stream.Receive() {
		t.Fatalf("first event: %v", stream.Err())
	}

	ustore.expired.Store(true)
	src.send(event())
	if !stream.Receive() {
		t.Fatalf("event after the token expired: %v", stream.Err())
	}
	c.Eq(1, registryCount(reg), "an expired credential's open stream must stay registered")

	reg.revokeToken("tok-1")
	got := make(chan error, 1)
	go func() {
		for stream.Receive() {
		}
		got <- stream.Err()
	}()
	select {
	case err := <-got:
		c.Eq(connect.CodeCanceled, connect.CodeOf(err), "the revoked stream must end Canceled, got %v", err)
	case <-time.After(5 * time.Second):
		t.Error("the revoked stream never ended")
	}
}

// rmUserStore tracks the ORDER of LookupUsername and Delete, whose sequence
// the user-removal cut depends on.
type rmUserStore struct {
	users.Store
	mu      sync.Mutex
	order   []string
	ids     map[string]string
	removed map[string]bool
}

func (s *rmUserStore) LookupUsername(_ context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "lookup")
	id, ok := s.ids[name]
	if !ok {
		return "", users.ErrNotFound
	}
	return id, nil
}

func (s *rmUserStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "delete")
	if _, ok := s.ids[name]; !ok || s.removed[name] {
		return users.ErrNotFound
	}
	s.removed[name] = true
	return nil
}

func (s *rmUserStore) snapshotOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// TestUserRmDisconnectsExecutors pins the user-removal cut end to end: the id
// is resolved before the tombstone, the user's open streams are cancelled,
// and the executors the user owns are disconnected while everyone else's —
// above all every unowned one — survive. An unknown user is an answer that
// cuts nothing.
func TestUserRmDisconnectsExecutors(t *testing.T) {
	c := assert.NewAborting(t)
	store := &rmUserStore{ids: map[string]string{"bob": "u-bob"}, removed: map[string]bool{}}
	reg := newStreamRegistry()
	var bobStream, otherStream bool
	reg.add("tok-bob", "u-bob", func() { bobStream = true })
	reg.add("tok-other", "u-other", func() { otherStream = true })
	pool := newSyncEvictPool(
		execpool.LiveExecutor{Executor: executors.Executor{ID: "exec-bob", OwnerUserID: "u-bob"}},
		execpool.LiveExecutor{Executor: executors.Executor{ID: "exec-other", OwnerUserID: "u-other"}},
		execpool.LiveExecutor{Executor: executors.Executor{ID: "exec-free"}},
	)
	ctrl := &Controller{users: store, streamRevoke: reg, execPool: pool}

	c.NoError(ctrl.UserRm(context.Background(), "bob"))

	c.EqDiff([]string{"lookup", "delete"}, store.snapshotOrder(),
		"LookupUsername must run BEFORE Delete: after the tombstone the id is unresolvable")
	c.True(bobStream, "the removed user's open stream must be cut")
	c.True(pool.wasEvicted("exec-bob"), "the removed user's executor must be disconnected")
	c.False(pool.wasEvicted("exec-other"), "another user's executor must survive")
	c.False(pool.wasEvicted("exec-free"), "unowned executors must never be mass-disconnected")
	c.False(otherStream, "only the removed user's streams are cut")

	c.ErrorIs(ctrl.UserRm(context.Background(), "ghost"), users.ErrNotFound, "an unknown user is an answer")
	c.False(otherStream, "a failed removal must cut nothing")
	c.Eq(1, registryCount(reg), "the surviving stream stays registered")
}
