// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// This file is the interactive half of daemon-brokered MCP OAuth: one explicit
// login per owner and MCP endpoint, driven by a human, bounded in every
// direction, and durable only once the credential has been proved to work.
//
// The authorization-code engine is the SDK's own auth.AuthorizationCodeHandler.
// Nothing here duplicates the flow: the SDK discovers the metadata, registers
// the client, builds the authorization URL with PKCE, validates state and the
// RFC 9207 iss parameter, exchanges the code and reports the granted scopes.
// What the daemon adds is everything the SDK deliberately leaves to its caller:
//
//   - Two calls, one flow. Begin publishes the first consent URL; Complete
//     accepts one callback and answers with either the finished login or the
//     next consent URL. The SDK handler and its MCP session live across both, so
//     no round re-registers, re-discovers or re-consents on its own.
//   - Everything waits on a context. A cancelled Begin (a client that hung up
//     before the URL came back), a cancelled Complete, a logout, an owner
//     removal, an expired flow and daemon shutdown all abort the same way: the
//     flow's context dies, the SDK's HTTP request is cancelled, and the flow is
//     gone. Nothing is swept by a polling loop and nothing waits without a
//     cancel path.
//   - Nothing is durable until it works. The exchanged token and the DCR
//     registration stay in memory, the MCP session is opened with them and its
//     tools are listed, and only then does CommitLogin install a grant — fenced
//     by persisted generations, so a logout or a newer login that happened
//     meanwhile wins and this one is discarded.
//
// The discovery the SDK does not hand back (the resource and the exact issuer)
// is recovered from its own recorded traffic by discovery.go.

// The bounds of one login. They are constants, not settings: an operator cannot
// widen them from config, and a project entry cannot widen them at all.
const (
	// PendingTTL is how long a login may wait for a human. Once Begin has handed
	// out a consent URL, the daemon owns the flow for exactly this long; when it
	// elapses the flow is cancelled and its task torn down, whether or not
	// anything calls Complete again.
	PendingTTL = 10 * time.Minute
	// MaxPending is how many logins the daemon tracks at once, across all owners.
	MaxPending = 1024
	// MaxPendingPerOwner caps one owner, so a single client cannot fill the
	// global table and deny everyone else a login.
	MaxPendingPerOwner = 8
	// MaxCallbackQueryBytes caps the callback query a caller may hand back. A
	// consent round's callback is a few hundred bytes; the cap bounds what an
	// untrusted caller can push into the daemon's parser.
	MaxCallbackQueryBytes = 16 << 10
	// RevocationTimeout bounds the best-effort upstream revoke that follows a
	// local logout. It is deliberately short: the local invalidation has already
	// committed and must not wait on a server that may be gone.
	RevocationTimeout = 5 * time.Second

	// CallbackPort, CallbackPath and CallbackHost are the fixed loopback
	// redirect this daemon advertises to every authorization server. They are an
	// implementation choice, not a vendor requirement: a fixed value means one
	// registered redirect per login and no silent substitution when a port is
	// busy (a busy port is the client's error to report, with retry
	// instructions).
	CallbackPort = 1456
	CallbackHost = "127.0.0.1"
	CallbackPath = "/mcp/oauth/callback"
	// CallbackURL is the redirect URL exactly as it is registered and advertised.
	CallbackURL = "http://" + CallbackHost + ":1456" + CallbackPath

	// mcpOAuthClientName is what the daemon calls itself to the servers it
	// registers with and connects to.
	mcpOAuthClientName = "rafiki"
)

var (
	// ErrClosed reports that the engine has been closed and can start no new
	// logins. Flows already running are cancelled by Close.
	ErrClosed = errors.New("mcpoauth: login engine is closed")

	// ErrPendingLimit reports that too many logins are already pending — the
	// daemon-wide cap or this owner's. It is retryable once a pending login
	// finishes, expires or is cancelled, and the message says so.
	ErrPendingLimit = errors.New("mcpoauth: too many pending logins; finish or cancel one and try again")

	// ErrCallbackReplayed reports a callback query that has already been
	// accepted for this login. A consent round takes exactly one callback: a
	// second delivery of the same one is refused rather than answered twice.
	ErrCallbackReplayed = errors.New("mcpoauth: this authorization callback was already used")

	// ErrCallbackTooLarge reports a callback query past MaxCallbackQueryBytes.
	ErrCallbackTooLarge = fmt.Errorf("mcpoauth: the authorization callback query exceeds %d bytes", MaxCallbackQueryBytes)

	// ErrInvalidCallback reports a callback query that is not a query string, or
	// arrives when no consent round is waiting for one.
	ErrInvalidCallback = errors.New("mcpoauth: invalid authorization callback")

	// ErrConsentRejected reports that a consent round did not produce a token —
	// the state was wrong, the issuer did not match, the exchange was refused —
	// and the SDK asked for another one. The SDK retries a rejected request after
	// a failed authorization, so a second consent round can be requested without
	// any human error to report; serving it would silently restart the flow with
	// the same damaged input. The login is over: it must be started again.
	ErrConsentRejected = errors.New("mcpoauth: the consent round was rejected")
)

// AuthorizationError reports that the authorization server answered the
// redirect with an OAuth error instead of a code — the user declined, the
// request expired, the scope was refused. Only the protocol error code is kept;
// the free-text description and the query itself are never echoed into an error
// or a log, because both are attacker-influenced and the query's other
// parameters are not ours to repeat.
type AuthorizationError struct {
	Code string
}

func (e *AuthorizationError) Error() string {
	return fmt.Sprintf("mcpoauth: authorization was not granted (%s)", e.Code)
}

// LoginDescriptor is one consent round's browser hand-off: the URL to open and
// the loopback redirect the daemon has registered for it.
type LoginDescriptor struct {
	LoginID       string
	AuthorizeURL  string
	CallbackPath  string
	RedirectHosts []string
	// RedirectPort is a uint32 to match the wire field it is copied into.
	RedirectPort uint32
}

// BeginRequest starts one explicit login.
type BeginRequest struct {
	// OwnerUserID is the user the grant belongs to. It is never taken from a
	// child, a project entry or a header: the caller derives it from the
	// authenticated connection.
	OwnerUserID string
	// Server is the configured server name, used for human-facing messages.
	Server string
	// Endpoint is the MCP endpoint URL as configured. It is canonicalised here;
	// the canonical spelling is what the grant is bound to.
	Endpoint string
	// Scopes overrides the scopes discovery would request. Empty means the
	// challenged scopes, else the resource's advertised scopes.
	Scopes []string
}

// CompleteResult is the outcome of one Complete: exactly one of the final fields
// or Continuation is set.
type CompleteResult struct {
	Server    string
	Resource  string
	AccountID string
	// Tools are the tool names the verified session served, sorted.
	Tools []string
	// Continuation is the next consent round when the authorization server
	// requires one (a step-up, or an issuer that redirected to another
	// authorization server). The login id is unchanged.
	Continuation *LoginDescriptor
}

// DeleteSelector names what a logout removes. Exactly one field is set: an
// endpoint (a configured server, which fences even an unbound pending login) or
// a canonical resource (a grant, for a user whose project config is gone).
type DeleteSelector struct {
	Endpoint string
	Resource string
}

// EngineOptions are the seams a daemon wires once. Store is required; the rest
// have production defaults and exist so tests never sleep on a real clock.
type EngineOptions struct {
	// Store is the durable credential store. It is required.
	Store Store
	// Policy is the operator's [mcp_oauth] destination policy. A nil policy is
	// the default — public HTTPS only — never a wider one.
	Policy *Policy
	// Now and After are the clock seams. After returns a channel that fires once
	// after the duration; it is how a pending flow's TTL is armed, so a test can
	// expire a login without waiting ten minutes.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Logger receives operational events (a flow expiring, a logout cancelling a
	// login). It never receives a token, a client secret, a callback query or an
	// authorization code.
	Logger *slog.Logger
	// pendingTotal and pendingPerOwner override the caps. Only tests set them.
	pendingTotal    int
	pendingPerOwner int
}

// Engine owns the daemon's interactive MCP logins.
//
// Concurrency: every method is safe to call concurrently. The engine's own
// mutex guards the flow table only — never network or database I/O — and each
// flow's mutex guards its round bookkeeping only.
type Engine struct {
	ctx    context.Context
	cancel context.CancelFunc

	store  Store
	policy *Policy
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
	log    *slog.Logger
	rand   io.Reader

	limits struct {
		total    int
		perOwner int
	}

	wg sync.WaitGroup

	mu       sync.Mutex
	flows    map[string]*flow
	perOwner map[string]int
	closed   bool
}

// NewEngine builds the login engine. ctx is the daemon's lifetime: closing it
// (or calling Close) terminates every pending login.
func NewEngine(ctx context.Context, opts EngineOptions) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("mcpoauth: a credential store is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	engineCtx, cancel := context.WithCancel(ctx)
	e := &Engine{
		ctx:      engineCtx,
		cancel:   cancel,
		store:    opts.Store,
		policy:   opts.Policy.orDefault(),
		now:      opts.Now,
		after:    opts.After,
		log:      opts.Logger,
		rand:     rand.Reader,
		flows:    make(map[string]*flow),
		perOwner: make(map[string]int),
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.after == nil {
		e.after = time.After
	}
	if e.log == nil {
		e.log = slog.New(discardHandler{})
	}
	e.limits.total, e.limits.perOwner = MaxPending, MaxPendingPerOwner
	if opts.pendingTotal > 0 {
		e.limits.total = opts.pendingTotal
	}
	if opts.pendingPerOwner > 0 {
		e.limits.perOwner = opts.pendingPerOwner
	}
	return e, nil
}

// discardHandler keeps a logger that was never configured from writing
// anywhere: the engine logs operational events, and a nil-seam default that
// printed to stderr would be noise in every test and every embedder.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

// Begin starts one explicit login and returns its first consent round.
//
// It starts the login under a fresh endpoint fence before anything reaches the
// network, and it does not return until either a consent URL exists or the login
// has already failed. If ctx is cancelled before the URL comes back the flow is
// cancelled with it; once Begin has returned, the daemon's TTL owns the flow
// instead, because a client that stops calling Complete must not leave a task
// running forever.
func (e *Engine) Begin(ctx context.Context, req BeginRequest) (LoginDescriptor, error) {
	owner := strings.ToLower(strings.TrimSpace(req.OwnerUserID))
	if owner == "" {
		return LoginDescriptor{}, errors.New("mcpoauth: begin login: an owner is required")
	}
	endpoint, err := Canonicalize(req.Endpoint)
	if err != nil {
		return LoginDescriptor{}, fmt.Errorf("mcpoauth: begin login: endpoint: %w", err)
	}
	if err := e.reserve(owner); err != nil {
		return LoginDescriptor{}, err
	}

	// One owner's endpoint has one pending login. The store's BeginFence already
	// supersedes an older fence on the same endpoint, so a local flow left
	// waiting would be a browser prompt that can never commit; drop it now.
	e.dropFlows(func(f *flow) bool { return f.owner == owner && f.endpoint == endpoint })

	id, err := e.newLoginID()
	if err != nil {
		return LoginDescriptor{}, err
	}

	// The fence is started here, not in the goroutine: a login that cannot even
	// begin (a tombstoned owner, a database outage) is refused to the caller
	// rather than surfacing later as a failed consent round.
	fence, err := e.store.BeginFence(ctx, owner, endpoint)
	if err != nil {
		return LoginDescriptor{}, err
	}

	flowCtx, cancel := context.WithCancel(e.ctx)
	f := &flow{
		engine:    e,
		id:        id,
		owner:     owner,
		server:    req.Server,
		endpoint:  endpoint,
		scopes:    append([]string(nil), req.Scopes...),
		fence:     fence,
		ctx:       flowCtx,
		cancel:    cancel,
		events:    make(chan flowEvent, 1),
		callbacks: make(chan callbackDelivery, 1),
		consumed:  make(map[string]bool),
	}
	// The recorder is a per-flow discovery recorder under the guarded client.
	// Wrapping the transport keeps every redirect and destination check the
	// policy client installed.
	guarded := e.policy.Client()
	f.recorder = newRecordRequest(guarded.Transport, flowCtx)
	f.client = f.recorder.wrap(guarded)

	if err := e.register(f); err != nil {
		cancel()
		return LoginDescriptor{}, err
	}

	e.wg.Add(2)
	go e.expire(f)
	go e.runLogin(f)

	ev, err := e.awaitEvent(ctx, f)
	if err != nil {
		e.dropFlow(f)
		return LoginDescriptor{}, err
	}
	if ev.kind != eventConsent {
		e.dropFlow(f)
		if ev.err != nil {
			return LoginDescriptor{}, ev.err
		}
		return LoginDescriptor{}, errors.New("mcpoauth: the login finished without asking for consent")
	}
	return ev.descriptor, nil
}

// Complete accepts one authorization callback and answers with the finished
// login or the next consent round.
//
// Unknown, expired and other-owner login ids all answer ErrMissing and nothing
// else: a caller cannot tell a login that expired from one that never existed,
// nor from one belonging to somebody else — and probing another owner's id
// changes nothing about that owner's flow.
func (e *Engine) Complete(ctx context.Context, owner, loginID, callbackQuery string) (CompleteResult, error) {
	f, err := e.lookup(strings.ToLower(strings.TrimSpace(owner)), loginID)
	if err != nil {
		return CompleteResult{}, err
	}
	if len(callbackQuery) > MaxCallbackQueryBytes {
		return CompleteResult{}, ErrCallbackTooLarge
	}
	values, err := url.ParseQuery(callbackQuery)
	if err != nil {
		// The parse error would quote the offending substring, which may be part
		// of a code; the caller only needs to know it was not a query string.
		return CompleteResult{}, fmt.Errorf("%w: it is not a query string", ErrInvalidCallback)
	}
	if err := e.deliver(ctx, f, values); err != nil {
		return CompleteResult{}, err
	}

	ev, err := e.awaitEvent(ctx, f)
	if err != nil {
		e.dropFlow(f)
		return CompleteResult{}, err
	}
	switch ev.kind {
	case eventConsent:
		next := ev.descriptor
		return CompleteResult{Server: f.server, Continuation: &next}, nil
	case eventDone:
		e.dropFlow(f)
		return ev.result, nil
	default:
		e.dropFlow(f)
		if ev.err != nil {
			return CompleteResult{}, ev.err
		}
		return CompleteResult{}, errors.New("mcpoauth: the login failed without a reason")
	}
}

// Delete logs one owner out. It cancels the daemon's own pending flows for the
// selector first, then invalidates through the store, then best-effort revokes
// the detached token upstream.
//
// The store's generations are what actually protect the result: a flow in
// another daemon instance cannot be cancelled from here, and it does not need to
// be — its CommitLogin finds the advanced fence and refuses.
func (e *Engine) Delete(ctx context.Context, owner string, sel DeleteSelector) error {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" {
		return errors.New("mcpoauth: delete credential: an owner is required")
	}
	switch {
	case sel.Endpoint == "" && sel.Resource == "":
		return errors.New("mcpoauth: delete credential: an endpoint or a resource is required")
	case sel.Endpoint != "" && sel.Resource != "":
		return errors.New("mcpoauth: delete credential: an endpoint and a resource are mutually exclusive")
	}

	var revocation Revocation
	var err error
	if sel.Endpoint != "" {
		endpoint, cerr := Canonicalize(sel.Endpoint)
		if cerr != nil {
			return fmt.Errorf("mcpoauth: delete credential: endpoint: %w", cerr)
		}
		e.dropFlows(func(f *flow) bool { return f.owner == owner && f.endpoint == endpoint })
		revocation, err = e.store.InvalidateEndpoint(ctx, owner, endpoint)
	} else {
		resource, cerr := Canonicalize(sel.Resource)
		if cerr != nil {
			return fmt.Errorf("mcpoauth: delete credential: resource: %w", cerr)
		}
		e.dropFlows(func(f *flow) bool { return f.owner == owner && f.boundResource() == resource })
		revocation, err = e.store.InvalidateResource(ctx, owner, resource)
	}
	if err != nil {
		return err
	}
	e.revokeDetached(ctx, revocation)
	return nil
}

// CancelOwner terminates every pending login one owner has and returns how many
// flows it cancelled. It is local-only and needs no live user: removing a user
// must be able to stop the logins they left behind even when the durable cleanup
// that follows fails. The store's fences and live-user checks remain the
// authority; this only stops work that is already running.
func (e *Engine) CancelOwner(owner string) int {
	return e.dropFlows(func(f *flow) bool { return f.owner == strings.ToLower(strings.TrimSpace(owner)) })
}

// Close terminates every pending login and waits for their tasks to finish. It
// takes no lock while it waits: the flows are cancelled first, then the wait
// runs against the groups those cancellations released.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	flows := make([]*flow, 0, len(e.flows))
	for _, f := range e.flows {
		flows = append(flows, f)
	}
	e.flows = make(map[string]*flow)
	e.perOwner = make(map[string]int)
	e.mu.Unlock()

	e.cancel()
	for _, f := range flows {
		f.cancel()
	}
	e.wg.Wait()
	return nil
}

// reserve refuses a login the engine has no room for, before the store is
// touched: a Begin that is going to be refused must not supersede another
// pending login's fence on its way out.
func (e *Engine) reserve(owner string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if len(e.flows) >= e.limits.total || e.perOwner[owner] >= e.limits.perOwner {
		return ErrPendingLimit
	}
	return nil
}

func (e *Engine) newLoginID() (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(e.rand, raw); err != nil {
		return "", fmt.Errorf("mcpoauth: generating a login id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (e *Engine) register(f *flow) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if len(e.flows) >= e.limits.total || e.perOwner[f.owner] >= e.limits.perOwner {
		return ErrPendingLimit
	}
	e.flows[f.id] = f
	e.perOwner[f.owner]++
	return nil
}

func (e *Engine) lookup(owner, loginID string) (*flow, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.flows[loginID]
	if !ok || f.owner != owner {
		return nil, ErrMissing
	}
	return f, nil
}

// dropFlow removes one flow from the table and cancels it. It is idempotent and
// takes no lock while anything blocks.
func (e *Engine) dropFlow(f *flow) {
	e.mu.Lock()
	if cur, ok := e.flows[f.id]; ok && cur == f {
		delete(e.flows, f.id)
		if n := e.perOwner[f.owner]; n > 1 {
			e.perOwner[f.owner] = n - 1
		} else {
			delete(e.perOwner, f.owner)
		}
	}
	e.mu.Unlock()
	f.cancel()
}

// dropFlows cancels every flow the predicate selects and returns how many it
// removed.
func (e *Engine) dropFlows(match func(*flow) bool) int {
	e.mu.Lock()
	var victims []*flow
	for id, f := range e.flows {
		if !match(f) {
			continue
		}
		victims = append(victims, f)
		delete(e.flows, id)
		if n := e.perOwner[f.owner]; n > 1 {
			e.perOwner[f.owner] = n - 1
		} else {
			delete(e.perOwner, f.owner)
		}
	}
	e.mu.Unlock()
	for _, f := range victims {
		f.cancel()
	}
	return len(victims)
}

// expire arms the flow's TTL. It is a task, not a sweep: a login that nobody
// comes back to is torn down on its own schedule, without needing another Begin
// to notice it.
func (e *Engine) expire(f *flow) {
	defer e.wg.Done()
	select {
	case <-f.ctx.Done():
		return
	case <-e.after(PendingTTL):
	}
	e.log.Info("mcp oauth login expired", "server", f.server, "endpoint", displayURL(f.endpoint))
	e.dropFlow(f)
}

// awaitEvent waits for the flow's next event, giving up when either context
// ends. An event that is already buffered wins over a cancellation: a login that
// completed just as its TTL fired has a result, and reporting it is not a lie.
func (e *Engine) awaitEvent(ctx context.Context, f *flow) (flowEvent, error) {
	for {
		select {
		case ev := <-f.events:
			return ev, nil
		default:
		}
		select {
		case ev := <-f.events:
			return ev, nil
		case <-ctx.Done():
			return flowEvent{}, ctx.Err()
		case <-f.ctx.Done():
			return flowEvent{}, ErrMissing
		case <-e.ctx.Done():
			return flowEvent{}, ErrClosed
		}
	}
}

// deliver hands one callback to the consent round that is waiting for it.
//
// The per-round state is what makes a callback single-use: each round's state
// value is recorded when it is consumed, so the same callback can never answer a
// second round, and a callback that belongs to no round is refused rather than
// fed to the SDK as if it might match.
func (e *Engine) deliver(ctx context.Context, f *flow, values url.Values) error {
	state := values.Get("state")
	f.mu.Lock()
	round := f.awaiting
	if round == 0 {
		f.mu.Unlock()
		return fmt.Errorf("%w: no consent round is waiting for a callback", ErrInvalidCallback)
	}
	if state != "" {
		if f.consumed[state] {
			f.mu.Unlock()
			return ErrCallbackReplayed
		}
		f.consumed[state] = true
	}
	f.mu.Unlock()

	select {
	case f.callbacks <- callbackDelivery{round: round, values: values}:
		return nil
	case <-ctx.Done():
		e.dropFlow(f)
		return ctx.Err()
	case <-f.ctx.Done():
		return ErrMissing
	case <-e.ctx.Done():
		return ErrClosed
	}
}

// runLogin is the flow's task: the whole SDK-driven login, ending in one event.
func (e *Engine) runLogin(f *flow) {
	defer e.wg.Done()
	result, err := e.login(f)
	if err != nil {
		_ = f.fail(err)
		return
	}
	_ = f.publish(flowEvent{kind: eventDone, result: result})
}

// login runs the SDK's handler to completion and commits only if the credential
// it produced actually opens the MCP session.
func (e *Engine) login(f *flow) (result CompleteResult, err error) {
	defer func() {
		if err == nil {
			return
		}
		// Whatever went wrong, token material this login obtained belongs to a
		// grant that will never exist here. Offering it back is best-effort and
		// bounded; the local record is already nothing.
		if reg, tok, _, _ := f.credentials(); tok.AccessToken != "" || tok.RefreshToken != "" {
			e.revokeProvisional(f, reg, tok)
		}
	}()
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: registrationMetadata(),
		},
		RedirectURL: CallbackURL,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return e.fetch(f, args)
		},
		ScopeFilter:         scopeFilter(f.scopes),
		RequestRefreshToken: true,
		Client:              f.client,
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			return e.capture(f, cfg, tok)
		},
	})
	if err != nil {
		return CompleteResult{}, fmt.Errorf("mcpoauth: configuring the authorization flow: %w", err)
	}

	// The MCP session is the proof. It is opened with the login's own provisional
	// token source, over the guarded client, and it is closed on every way out.
	transport := &mcp.StreamableClientTransport{
		Endpoint:     f.endpoint,
		HTTPClient:   f.client,
		OAuthHandler: handler,
		// A login makes two calls and lives for seconds. Holding a server-push
		// stream open for it would be a resource nobody reads and a connection
		// that outlives its purpose.
		DisableStandaloneSSE: true,
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: mcpOAuthClientName, Version: "1"}, nil).
		Connect(f.ctx, transport, nil)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("mcpoauth: authenticating to %s: %w", displayURL(f.endpoint), scrubAuthError(err))
	}
	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(f.ctx, nil)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("mcpoauth: verifying the tools of %s: %w", displayURL(f.endpoint), scrubAuthError(err))
	}
	return e.commit(f, tools)
}

// commit installs the grant and answers with what the verified session served.
func (e *Engine) commit(f *flow, tools *mcp.ListToolsResult) (CompleteResult, error) {
	reg, token, scopes, discovery := f.credentials()
	if discovery.Resource == "" {
		return CompleteResult{}, errors.New("mcpoauth: the login finished without validated discovery")
	}
	grant := Grant{
		OwnerUserID:  f.owner,
		Resource:     discovery.Resource,
		Issuer:       discovery.Issuer,
		Registration: reg,
		Scopes:       scopes,
		RedirectURL:  CallbackURL,
		Token:        token,
	}
	stored, err := e.store.CommitLogin(f.ctx, f.fenceCopy(), grant)
	if err != nil {
		// The staging is owned by the caller (login's deferred cleanup): the
		// token is bound to a client registration this owner may not keep, and
		// no local record refers to it.
		return CompleteResult{}, err
	}
	names := make([]string, 0, len(tools.Tools))
	for _, t := range tools.Tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return CompleteResult{
		Server:   f.server,
		Resource: stored.Resource,
		// AccountID stays empty: no validated source for one exists here, and
		// decoding an unverified token to invent one is exactly what the design
		// forbids.
		Tools: names,
	}, nil
}

// fetch is the SDK's AuthorizationCodeFetcher: publish the URL, validate what
// the SDK discovered from recorded evidence, fence the login to it, and wait for
// exactly one callback.
func (e *Engine) fetch(f *flow, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	round, mark := f.nextRound()
	// Everything this round's discovery reads is recorded from here on.
	defer f.endRound()
	if err := f.beginRound(round); err != nil {
		return nil, err
	}

	discovery, err := captureDiscovery(f.ctx, discoveryInput{
		Endpoint:         f.endpoint,
		AuthorizationURL: args.URL,
		Recorder:         f.recorder,
		Mark:             mark,
		Policy:           e.policy,
	})
	if err != nil {
		return nil, f.fail(err)
	}
	// The fence moves to the resource and exact issuer BEFORE a browser is told
	// where to go: a logout that lands afterwards advances the fence and this
	// login can no longer commit.
	fence, err := e.store.BindFence(f.ctx, f.fenceCopy(), discovery.Resource, discovery.Issuer)
	if err != nil {
		return nil, f.fail(err)
	}
	f.setDiscovery(fence, discovery, args.URL)

	descriptor := LoginDescriptor{
		LoginID:       f.id,
		AuthorizeURL:  args.URL,
		CallbackPath:  CallbackPath,
		RedirectHosts: []string{CallbackHost},
		RedirectPort:  CallbackPort,
	}
	if err := f.publish(flowEvent{kind: eventConsent, round: round, descriptor: descriptor}); err != nil {
		return nil, err
	}

	values, err := f.awaitCallback(f.ctx, round)
	if err != nil {
		return nil, f.fail(err)
	}
	if code := values.Get("error"); code != "" {
		return nil, f.fail(&AuthorizationError{Code: safeOAuthCode(code)})
	}
	code := values.Get("code")
	if code == "" {
		return nil, f.fail(fmt.Errorf("%w: it carries neither a code nor an error", ErrInvalidCallback))
	}
	// The raw parsed fields go to the SDK so ITS state comparison, ITS RFC 9207
	// iss check and ITS PKCE verifier are the ones that decide.
	return &auth.AuthorizationResult{
		Code:  code,
		State: values.Get("state"),
		Iss:   values.Get("iss"),
	}, nil
}

// capture is the SDK's NewTokenSource hook: the post-DCR configuration and the
// exchanged token, kept provisional.
func (e *Engine) capture(f *flow, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
	if cfg == nil || tok == nil || tok.AccessToken == "" {
		return nil, errors.New("mcpoauth: the authorization server returned no usable token")
	}
	if cfg.ClientID == "" {
		return nil, errors.New("mcpoauth: the client registration produced no client id")
	}
	discovery := f.boundDiscovery()
	if discovery.Issuer == "" {
		return nil, errors.New("mcpoauth: the token source was built before discovery was validated")
	}
	// The SDK built this configuration from the metadata it fetched; if it does
	// not agree with the metadata this flow captured, the capture is wrong and
	// the credential must not be stored against the wrong issuer.
	if cfg.Endpoint.AuthURL != discovery.AuthorizationEndpoint || cfg.Endpoint.TokenURL != discovery.TokenEndpoint {
		return nil, fmt.Errorf("mcpoauth: the client configuration does not match the authorization server %s", discovery.Issuer)
	}
	reg := Registration{
		ClientID:              cfg.ClientID,
		ClientSecret:          cfg.ClientSecret,
		AuthStyle:             authStyleOf(cfg.Endpoint.AuthStyle),
		AuthorizationEndpoint: cfg.Endpoint.AuthURL,
		TokenEndpoint:         cfg.Endpoint.TokenURL,
		RevocationEndpoint:    discovery.RevocationEndpoint,
	}
	// What the authorization server says it granted, else what the last consent
	// URL asked for — the same choice the SDK's own granted-scope bookkeeping
	// makes, from public fields.
	f.countCapture()
	extra, _ := tok.Extra("scope").(string)
	f.setCredentials(reg, Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}, scopesFromScopeParam(extra))
	// A login-owned source, not oauth2's refreshing one: a refresh here would
	// spend the single-use token behind a grant that does not exist yet.
	return oauth2.StaticTokenSource(tok), nil
}

// registrationMetadata is the dynamic client registration this daemon asks for:
// a native (loopback-redirect) public client using the authorization-code and
// refresh-token grants. It is a fresh registration for every explicit login —
// requirement (1.2)'s store records it so a refresh can use it, and nothing here
// reuses an old registration across a new login.
func registrationMetadata() *oauthex.ClientRegistrationMetadata {
	return &oauthex.ClientRegistrationMetadata{
		RedirectURIs:            []string{CallbackURL},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		ApplicationType:         "native",
		ClientName:              mcpOAuthClientName,
	}
}

// scopeFilter applies an explicit scope override. Empty means no filter at all,
// which leaves the SDK's own precedence — the WWW-Authenticate challenge, else
// the resource's advertised scopes — untouched, and still lets it add
// offline_access when the authorization server supports it.
func scopeFilter(override []string) func([]string) []string {
	if len(override) == 0 {
		return nil
	}
	scopes := append([]string(nil), override...)
	return func([]string) []string { return scopes }
}

// authStyleOf names the stored auth style from the SDK's resolved one. The store
// keys on the NAME, so an unknown value is refused rather than reinterpreted.
func authStyleOf(style oauth2.AuthStyle) AuthStyle {
	switch style {
	case oauth2.AuthStyleInParams:
		return AuthStyleInParams
	case oauth2.AuthStyleInHeader:
		return AuthStyleInHeader
	default:
		return AuthStyleAuto
	}
}

// safeOAuthCode keeps an OAuth protocol error code and nothing else: a short
// token of the characters the specification allows. Anything else — which an
// authorization server is free to send, because it echoes what the user's
// browser handed it — becomes "unknown".
// displayURL is a URL safe to name in an error or a log line: scheme, host and
// path, with any query or userinfo dropped. A configured endpoint may carry a
// credential in its query, and an error message is not a place to repeat one.
func displayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

func safeOAuthCode(code string) string {
	if code == "" || len(code) > 64 {
		return "unknown"
	}
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return "unknown"
		}
	}
	return code
}

// scrubAuthError replaces an oauth2 token-endpoint failure with a message that
// carries the status and the protocol error code only. oauth2's own error
// quotes the response body, which is attacker-influenced text that may repeat
// the authorization code or a token.
func scrubAuthError(err error) error {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		status := 0
		if retrieve.Response != nil {
			status = retrieve.Response.StatusCode
		}
		return fmt.Errorf("mcpoauth: the authorization server refused the token request (status %d, error %q)", status, safeOAuthCode(retrieve.ErrorCode))
	}
	return err
}

// eventKind is what a flow's task has to report to a caller.
type eventKind int

const (
	// eventConsent says a consent URL is ready and one callback is being waited
	// for.
	eventConsent eventKind = iota + 1
	// eventDone says the login finished and the grant is stored.
	eventDone
	// eventFailed says the login will not finish; err says why.
	eventFailed
)

// flowEvent is one report from the flow's task to the caller waiting on it.
type flowEvent struct {
	kind       eventKind
	round      int
	descriptor LoginDescriptor
	result     CompleteResult
	err        error
}

// callbackDelivery is one parsed callback query, addressed to the consent round
// that was waiting when it arrived.
type callbackDelivery struct {
	round  int
	values url.Values
}

// flow is one pending login: the SDK session being driven, the round it is on,
// and the provisional credential material the login has not committed yet.
//
// mu guards the round bookkeeping and that provisional state only. It is never
// held across a network call, a database call, or a channel operation that can
// block — the two channels are the only hand-off points, and every wait on them
// selects the flow's own cancellation.
type flow struct {
	engine   *Engine
	id       string
	owner    string
	server   string
	endpoint string
	// scopes is the explicit override, empty when the SDK's own discovery
	// ordering decides.
	scopes []string

	ctx    context.Context
	cancel context.CancelFunc

	recorder *recordRequest
	client   *http.Client

	events    chan flowEvent
	callbacks chan callbackDelivery

	mu sync.Mutex
	// fence is the persisted fence this login commits against: the endpoint
	// generation from Begin, then the bound resource, issuer and grant
	// generation from discovery.
	fence LoginFence
	// mark is the recorder position the NEXT consent round starts at.
	mark  int
	round int
	// awaiting is the round number waiting for a callback, 0 when none is.
	awaiting int
	// consumed records the state values already accepted, so one callback can
	// never answer two rounds.
	consumed      map[string]bool
	discovery     validatedDiscovery
	registration  Registration
	token         Token
	scopesGranted []string
	// lastScopes is the scope list the last consent URL asked for, used when the
	// token response does not say what was granted.
	lastScopes []string
	// captures counts the rounds that completed a token exchange. A round that
	// did not capture cannot be followed by another consent round.
	captures int
	// fatal, once set, is the reason this login is over. It is returned to every
	// later fetcher call so the SDK's own retries cannot restart the flow.
	fatal error
}

// fail ends the flow with err and reports it to the caller exactly once.
//
// Reporting from here rather than only from the task's exit matters: the SDK
// retries a request whose authorization failed, so waiting for it to give up
// would leave the person at the browser waiting for a login nobody is going to
// ask anything about.
func (f *flow) fail(err error) error {
	f.mu.Lock()
	if f.fatal != nil {
		first := f.fatal
		f.mu.Unlock()
		return first
	}
	f.fatal = err
	f.mu.Unlock()
	_ = f.publish(flowEvent{kind: eventFailed, err: err})
	return err
}

// beginRound refuses a consent round that follows one which produced no token.
//
// The SDK retries the request that failed its authorization, which reaches the
// fetcher again with no human action in between. A round that exchanged no token
// cannot be followed by a fresh consent URL: that would silently restart the
// login against the same input that was just rejected, and the caller would see
// a second browser prompt instead of the failure it needs to report.
func (f *flow) beginRound(round int) error {
	f.mu.Lock()
	fatal := f.fatal
	exchanged := f.captures
	f.mu.Unlock()
	if fatal != nil {
		return fatal
	}
	if round > 1 && exchanged < round-1 {
		return f.fail(fmt.Errorf("%w: consent round %d completed no token exchange", ErrConsentRejected, round-1))
	}
	return nil
}

// publish hands one event to the caller, unless the flow is already over.
func (f *flow) publish(ev flowEvent) error {
	select {
	case f.events <- ev:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	case <-f.engine.ctx.Done():
		return ErrClosed
	}
}

// nextRound starts a consent round and returns its number together with the
// recorder position it begins at.
func (f *flow) nextRound() (round, mark int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.round++
	f.awaiting = f.round
	return f.round, f.mark
}

// endRound moves the recorder boundary past everything this round fetched, so
// the next round's evidence is its own.
func (f *flow) endRound() {
	mark := f.recorder.mark()
	f.mu.Lock()
	f.mark = mark
	f.mu.Unlock()
}

// awaitCallback waits for the one callback this round accepts.
func (f *flow) awaitCallback(sdkCtx context.Context, round int) (url.Values, error) {
	select {
	case d := <-f.callbacks:
		f.mu.Lock()
		if f.awaiting == round {
			f.awaiting = 0
		}
		f.mu.Unlock()
		if d.round != round {
			return nil, fmt.Errorf("%w: the callback belongs to another consent round", ErrInvalidCallback)
		}
		return d.values, nil
	case <-sdkCtx.Done():
		return nil, sdkCtx.Err()
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

func (f *flow) fenceCopy() LoginFence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fence
}

func (f *flow) setDiscovery(fence LoginFence, d validatedDiscovery, authURL string) {
	var scopes []string
	if u, err := url.Parse(authURL); err == nil {
		scopes = strings.Fields(u.Query().Get("scope"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fence = fence
	f.discovery = d
	f.lastScopes = scopes
}

func (f *flow) boundDiscovery() validatedDiscovery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discovery
}

// boundResource is the canonical resource this flow was fenced to, or empty
// before discovery validated one.
func (f *flow) boundResource() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discovery.Resource
}

// countCapture records that this round exchanged its code for a token.
func (f *flow) countCapture() {
	f.mu.Lock()
	f.captures++
	f.mu.Unlock()
}

func (f *flow) setCredentials(reg Registration, tok Token, granted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registration = reg
	f.token = tok
	f.scopesGranted = granted
}

// credentials returns the provisional material a successful verification is
// allowed to commit.
func (f *flow) credentials() (Registration, Token, []string, validatedDiscovery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scopes := f.scopesGranted
	if scopes == nil {
		scopes = f.lastScopes
	}
	return f.registration, f.token, append([]string(nil), scopes...), f.discovery
}

// scopesFromScopeParam splits an OAuth scope parameter into its values.
func scopesFromScopeParam(scope string) []string {
	return strings.Fields(scope)
}

// mergeContexts returns a context that ends when either input does, together
// with a release func for the registration it made. It is how a per-flow client
// stays cancellable by the flow even for requests the SDK issues with a context
// of its own — the SDK's connection context is deliberately detached from the
// caller, so relying on it would leave a discovery request running after the
// login it belongs to had been abandoned.
func mergeContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	if a == nil {
		a = context.Background()
	}
	if b == nil {
		return context.WithCancel(a)
	}
	merged, cancel := context.WithCancel(a)
	if b.Err() != nil {
		cancel()
		return merged, func() {}
	}
	stop := context.AfterFunc(b, cancel)
	return merged, func() {
		stop()
		cancel()
	}
}
