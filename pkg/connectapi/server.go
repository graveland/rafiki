// SPDX-License-Identifier: Apache-2.0

// Package connectapi serves rafikid's Connect control plane. Handlers mount on
// the same http.ServeMux as the proxy faces, so there is one listener, one TLS
// config and one route to operate.
package connectapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/eventconv"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/store"
)

// HistoryLoader is the narrow slice of store.Messages this package needs.
// Depending on the interface rather than the concrete type keeps the handler
// testable without a database.
type HistoryLoader interface {
	LoadPage(ctx context.Context, conversationID string, page store.HistoryPage) ([]store.Message, error)
}

// ConversationResolver maps a daemon-assigned child id (a ULID, "c_...") to
// the persisted conversation's UUID — the id store.Messages.Load actually
// expects. These are different identifiers: the child id is runtime-only,
// minted per spawn, while the conversation id is what conversation_message
// rows are keyed by. false means the child is unknown, has exited and been
// forgotten, or is not a fundi child (only fundi children have a conversation
// UUID as their session id — see childstore.Snapshot.SessionID).
type ConversationResolver interface {
	ConversationID(childID string) (string, bool)
}

// Server implements the Control service.
//
// Every dependency below is attached AFTER construction, because the daemon's
// Controller — the only thing that can answer any of them — is built after the
// proxy face that owns this Server. They are atomic.Pointer rather than plain
// fields because the HTTP listener is already serving by the time main.go
// wires them: a plain field write there is a data race by the Go memory model,
// even though the lost-race outcome is a benign fail-closed.
type Server struct {
	history HistoryLoader

	events         atomic.Pointer[EventSource]
	lineageLn      atomic.Pointer[eventlog.Lineage]
	evlog          atomic.Pointer[eventlog.Store]
	resolver       atomic.Pointer[ConversationResolver]
	inbox          atomic.Pointer[inbox.Accepter]
	children       atomic.Pointer[ChildLister]
	lifecycle      atomic.Pointer[ChildLifecycle]
	taskLister     atomic.Pointer[TaskLister]
	modelLister    atomic.Pointer[ModelLister]
	execLister     atomic.Pointer[ExecutorLister]
	skills         atomic.Pointer[SkillManager]
	notifier       atomic.Pointer[Notifier]
	sendSteps      atomic.Pointer[SendStepRunner]
	pymodules      atomic.Pointer[PymoduleManager]
	presets        atomic.Pointer[PresetManager]
	modelAliases   atomic.Pointer[ModelAliasManager]
	recall         atomic.Pointer[RecallManager]
	repos          atomic.Pointer[RepoManager]
	quota          atomic.Pointer[QuotaReader]
	providerBans   atomic.Pointer[ProviderBanManager]
	routes         atomic.Pointer[RouteManager]
	routeExplainer atomic.Pointer[RouteExplainer]
	conversations  atomic.Pointer[ConversationInsights]
	reviewer       atomic.Pointer[ConversationReviewer]
	findingsReader atomic.Pointer[ConversationFindingsReader]
	daraja         atomic.Pointer[*darajaHandlers]
	scopes         atomic.Pointer[ChildScopeSource]
	guards         atomic.Pointer[OwnerGuardSource]
	scripts        atomic.Pointer[ScriptHub]
	childOps       atomic.Pointer[ChildOps]
	execAdmin      atomic.Pointer[ExecutorAdmin]
	userAdmin      atomic.Pointer[UserAdmin]
	login          atomic.Pointer[LoginBackend]
	providerLogin  atomic.Pointer[ProviderLoginBackend]
	rawIO          atomic.Pointer[RawChildIO]
	execSessions   atomic.Pointer[ExecutorSessions]
	sandboxes      atomic.Pointer[SandboxManager]
	syncers        atomic.Pointer[PathSyncer]

	// stopOnce guards closing stopped, the shutdown signal long-lived
	// handlers select on. NewServer creates the channel; the zero value's
	// nil channel simply blocks forever in a select, which is the same as
	// having never been stopped.
	stopOnce sync.Once
	stopped  chan struct{}
}

func NewServer(h HistoryLoader) *Server {
	return &Server{history: h, stopped: make(chan struct{})}
}

// Stop unblocks long-lived handlers (ExecutorSession streams) so daemon
// shutdown is not held by them: the loopback face is shut down with
// http.Server.Shutdown, which waits for active requests without cancelling
// their contexts, so a parked ExecutorSession stream would otherwise hold
// shutdown for the stream's remaining lifetime. Safe to call multiple times —
// every call after the first is a no-op. Handlers that return because of Stop
// still release their executors: net/http cancels a request's context once
// its handler returns, which is the eviction trigger.
//
// Only a Server from NewServer has a channel to close: Stop on a zero-value
// Server is a misuse and panics on the nil channel.
func (s *Server) Stop() {
	s.stopOnce.Do(func() { close(s.stopped) })
}

// SetEventSource attaches the live-event source. Without one, StreamEvents
// serves the durable replay and then ends the stream.
func (s *Server) SetEventSource(src EventSource) { s.events.Store(&src) }

func (s *Server) eventSource() EventSource {
	if p := s.events.Load(); p != nil {
		return *p
	}
	return nil
}

// SetLineage attaches the lineage provider.
func (s *Server) SetLineage(ln eventlog.Lineage) { s.lineageLn.Store(&ln) }

func (s *Server) lineage() eventlog.Lineage {
	if p := s.lineageLn.Load(); p != nil {
		return *p
	}
	return nil
}

// SetEventLog attaches the durable event log store.
func (s *Server) SetEventLog(l eventlog.Store) { s.evlog.Store(&l) }

func (s *Server) eventLog() eventlog.Store {
	if p := s.evlog.Load(); p != nil {
		return *p
	}
	return nil
}

// SetChildResolver attaches the child-id-to-conversation-id resolver. It is a
// post-construction setter, not a constructor argument, because the daemon's
// Controller (the only thing that knows this mapping) is constructed AFTER
// the proxy face that owns this Server — see cmd/rafikid/main.go, which wires
// it the same way it wires ctrl.SetProxy(face.URL, face.Token). Until it is
// set, every call that needs it fails closed (CodeUnavailable) rather than
// passing the child id through as if it were already a conversation id — the
// exact bug this resolver exists to close.
func (s *Server) SetChildResolver(r ConversationResolver) { s.resolver.Store(&r) }

// SetInbox attaches the inbound-message sink. Until it is set, Send fails
// closed rather than reporting success for a message that reached nothing.
//
// The parameter is inbox.Accepter, not the whole queue: this face may submit
// and must not be able to deliver, mark or reset. That narrowing is the only
// thing stopping a Connect handler from reaching a child directly.
func (s *Server) SetInbox(in inbox.Accepter) { s.inbox.Store(&in) }

// resolveConversation turns a request's child_id into the conversation id
// store.Messages.Load needs. Centralized here because GetHistory and
// StreamEvents both need it with identical semantics.
func (s *Server) resolveConversation(childID string) (string, error) {
	p := s.resolver.Load()
	if p == nil {
		return "", connect.NewError(connect.CodeUnavailable,
			errors.New("child resolver not yet wired"))
	}
	conversationID, ok := (*p).ConversationID(childID)
	if !ok {
		return "", connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no conversation for child %q", childID))
	}
	return conversationID, nil
}

// Routes returns the mux path prefix and handler for the Control service,
// with the given interceptors applied. An empty-token interceptor admits
// everything — see NewAuthInterceptor.
func (s *Server) Routes(interceptors ...connect.Interceptor) (string, http.Handler) {
	return rafikiv1connect.NewControlHandler(s, connect.WithInterceptors(interceptors...))
}

// GetHistory serves the durable tier: persisted messages converted to native
// events, each carrying its row's created_at as ts, bounded by the request's
// three optional fields — after_ordinal (strictly greater; the exact
// resumption cursor a follower replays from), before (ts strictly earlier;
// how a caller pages backwards) and limit (the newest N left, never splitting
// events that share a ts, so a page may hold more than N). The bounds are
// applied in the database by store.Messages.LoadPage, which owns their
// semantics, so a page costs the rows it returns.
func (s *Server) GetHistory(
	ctx context.Context,
	req *connect.Request[rafikiv1.GetHistoryRequest],
) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	// Checked before the conversation resolves: a malformed limit is refused
	// regardless of the child named, and never costs a storage read.
	if req.Msg.Limit != nil && req.Msg.GetLimit() <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("limit must be positive"))
	}
	// childScoped: the subtree boundary runs BEFORE the conversation
	// resolves, so a caller learns nothing about a child it may not read —
	// not even whether one exists (Authorize refuses unknown ids with the
	// same not-a-descendant answer).
	if sc := s.childScope(ctx); sc != nil {
		if err := sc.Authorize(childID); err != nil {
			return nil, err
		}
	}

	// ownerGuard: a non-admin user credential acts only on children it owns;
	// foreign, unknown and unowned ids refuse identically, before any side
	// effect.
	if g := s.ownerGuard(ctx); g != nil {
		if err := g.Authorize(childID); err != nil {
			return nil, err
		}
	}

	conversationID, err := s.resolveConversation(childID)
	if err != nil {
		return nil, err
	}

	var page store.HistoryPage
	if req.Msg.AfterOrdinal != nil {
		after := int(req.Msg.GetAfterOrdinal())
		page.AfterOrdinal = &after
	}
	if before := req.Msg.GetBefore(); before != nil {
		cutoff := before.AsTime()
		page.Before = &cutoff
	}
	if req.Msg.Limit != nil {
		limit := int(req.Msg.GetLimit())
		page.Limit = &limit
	}
	msgs, err := s.history.LoadPage(ctx, conversationID, page)
	if err != nil {
		return nil, unmappedErr(err, "rpc", "GetHistory", "conversationId", conversationID)
	}
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{
		Events: eventconv.EventsFromMessages(childID, msgs),
	}), nil
}

// Compile-time proof that the production type satisfies the test seam.
var _ HistoryLoader = (*store.Messages)(nil)
