// SPDX-License-Identifier: Apache-2.0

package connectapi

// The MCP OAuth verbs at the handler layer: identity, request shape, the
// continuation outcome, and error mapping. The daemon-side behaviour (config
// resolution, passive dials, owner cleanup) is pinned in cmd/rafikid.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// fakeMcpLogin is an McpLoginBackend that records the owner it was handed, so a
// test can pin that the handler passes the CALLER's user id — never a value from
// the request, which carries no owner at all.
type fakeMcpLogin struct {
	beginErr error
	compRes  McpLoginComplete
	compErr  error
	delErr   error

	owner       string
	configPath  string
	server      string
	scopes      []string
	loginID     string
	query       string
	deletions   []McpDeleteSelector
	beginCalled int
	compCalled  int
	delCalled   int
}

func (f *fakeMcpLogin) BeginMcpLogin(_ context.Context, ownerUserID, configPath, serverName string, scopes []string) (ProviderLoginBegin, error) {
	f.owner, f.configPath, f.server, f.scopes = ownerUserID, configPath, serverName, scopes
	f.beginCalled++
	if f.beginErr != nil {
		return ProviderLoginBegin{}, f.beginErr
	}
	return ProviderLoginBegin{
		LoginID:       "ml-1",
		AuthorizeURL:  "https://auth.example.net/authorize?client_id=rafiki",
		RedirectPort:  1456,
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{"127.0.0.1"},
	}, nil
}

func (f *fakeMcpLogin) CompleteMcpLogin(_ context.Context, ownerUserID, loginID, callbackQuery string) (McpLoginComplete, error) {
	f.owner, f.loginID, f.query = ownerUserID, loginID, callbackQuery
	f.compCalled++
	if f.compErr != nil {
		return McpLoginComplete{}, f.compErr
	}
	return f.compRes, nil
}

func (f *fakeMcpLogin) DeleteMcpCredential(_ context.Context, ownerUserID string, sel McpDeleteSelector) error {
	f.owner = ownerUserID
	f.delCalled++
	if f.delErr != nil {
		return f.delErr
	}
	f.deletions = append(f.deletions, sel)
	return nil
}

// mcpLoginUserCtx is a real user credential: the only caller shape these verbs
// admit.
func mcpLoginUserCtx() context.Context {
	return server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
}

// TestMcpOAuthSetBackendNilIsRefused pins the shared setter rule: a nil backend
// leaves the seam unwired rather than storing &b for a nil interface, which
// would defeat the Unavailable path and nil-panic the first handler call.
func TestMcpOAuthSetBackendNilIsRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	s.SetMcpLoginBackend(nil)

	_, err := s.BeginMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{ConfigPath: "/c.json", Server: "linear"}))
	c.Require().Error(err, "BeginMcpLogin after SetMcpLoginBackend(nil)")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetMcpLoginBackend(nil): want Unavailable")
}

// TestMcpOAuthUnavailableWhenUnwired: every verb answers Unavailable until the
// daemon wires its engine, rather than panicking or reporting an empty success.
func TestMcpOAuthUnavailableWhenUnwired(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	ctx := mcpLoginUserCtx()

	_, err := s.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{ConfigPath: "/c.json", Server: "linear"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "BeginMcpLogin unwired")

	_, err = s.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: "l1"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "CompleteMcpLogin unwired")

	_, err = s.DeleteMcpCredential(ctx, connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{Resource: "https://mcp.example.net/mcp"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "DeleteMcpCredential unwired")
}

// TestMcpOAuthNeedsAUserCredential: a caller with no identity (the unix
// socket's local trust) or one that names no user id is refused
// FailedPrecondition by every verb, and the backend is never called — there is
// no user to bind a grant to.
func TestMcpOAuthNeedsAUserCredential(t *testing.T) {
	c := assert.NewCollecting(t)

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"nil identity", context.Background()},
		{"no user id", server.WithIdentity(context.Background(), &server.Identity{})},
	} {
		f := &fakeMcpLogin{}
		s := &Server{}
		s.SetMcpLoginBackend(f)

		_, err := s.BeginMcpLogin(tc.ctx,
			connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{ConfigPath: "/c.json", Server: "linear"}))
		c.Require().Error(err, "%s: BeginMcpLogin", tc.name)
		c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "%s: BeginMcpLogin code", tc.name)

		_, err = s.CompleteMcpLogin(tc.ctx,
			connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: "l1"}))
		c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "%s: CompleteMcpLogin code", tc.name)

		_, err = s.DeleteMcpCredential(tc.ctx,
			connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{Resource: "https://mcp.example.net/mcp"}))
		c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "%s: DeleteMcpCredential code", tc.name)

		c.Eq("", f.owner, "%s: backend must not be reached without a user id", tc.name)
	}
}

// TestMcpOAuthRefusesAChildCredential is the handler-layer half of the security
// boundary. Every identity below CARRIES a real UserID, so a UserID-shaped check
// cannot tell it from a user: a per-child secret carries its owner's id
// (pkg/server/usertoken.go), so does the per-boot+session shape, and a {notify}
// token names the user whose agents it may notify. Each is refused
// PermissionDenied by every verb, and the backend is never called — an MCP grant
// is spendable by the owner's CHILDREN, so a child that could begin one would be
// planting a credential its siblings then use.
func TestMcpOAuthRefusesAChildCredential(t *testing.T) {
	c := assert.NewCollecting(t)

	for _, tc := range []struct {
		name string
		id   *server.Identity
	}{
		{"per-child secret", &server.Identity{UserID: "u1", ChildID: "c_child", Via: server.ProvenanceChildToken}},
		{"per-boot + session", &server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed}},
		{"notify-scoped token", &server.Identity{UserID: "u1", TokenID: "tok-1", Via: server.ProvenanceNotify}},
		{"unknown provenance with a user id", &server.Identity{UserID: "u1"}},
	} {
		ctx := server.WithIdentity(context.Background(), tc.id)
		f := &fakeMcpLogin{}
		s := &Server{}
		s.SetMcpLoginBackend(f)

		_, err := s.BeginMcpLogin(ctx,
			connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{ConfigPath: "/c.json", Server: "linear"}))
		c.Require().Error(err, "%s: BeginMcpLogin", tc.name)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s: BeginMcpLogin code", tc.name)

		_, err = s.CompleteMcpLogin(ctx,
			connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: "l1"}))
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s: CompleteMcpLogin code", tc.name)

		_, err = s.DeleteMcpCredential(ctx,
			connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{Resource: "https://mcp.example.net/mcp"}))
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s: DeleteMcpCredential code", tc.name)

		c.Eq("", f.owner, "%s: a non-user credential must never reach the backend", tc.name)
	}
}

// TestMcpOAuthBeginMapsTheBackendAnswer pins the request/response mapping, that
// the backend receives the CALLER's user id (the request carries none), that
// repeated scopes pass through verbatim, and that a malformed request never
// reaches the backend.
func TestMcpOAuthBeginMapsTheBackendAnswer(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeMcpLogin{}
	s := &Server{}
	s.SetMcpLoginBackend(f)

	_, err := s.BeginMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{Server: "linear"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty config_path")
	c.Eq(0, f.beginCalled, "an empty config_path must not reach the backend")

	_, err = s.BeginMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{ConfigPath: "/srv/app/.mcp.json"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty server")
	c.Eq(0, f.beginCalled, "an empty server must not reach the backend")

	resp, err := s.BeginMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
			ConfigPath: "/srv/app/.mcp.json",
			Server:     "linear",
			Scopes:     []string{"tools:read", "tools:write"},
		}))
	c.Require().NoError(err, "BeginMcpLogin")
	c.Eq("u1", f.owner, "backend sees the caller's user id, not one from the request")
	c.Eq("/srv/app/.mcp.json", f.configPath, "config_path passed through")
	c.Eq("linear", f.server, "server passed through")
	c.EqDeep([]string{"tools:read", "tools:write"}, f.scopes, "scopes passed through")
	c.Eq("ml-1", resp.Msg.GetLoginId(), "login_id")
	c.Eq("https://auth.example.net/authorize?client_id=rafiki", resp.Msg.GetAuthorizeUrl(), "authorize_url")
	c.Eq(uint32(1456), resp.Msg.GetRedirectPort(), "redirect_port")
	c.Eq("/mcp/oauth/callback", resp.Msg.GetCallbackPath(), "callback_path")
	c.EqDeep([]string{"127.0.0.1"}, resp.Msg.GetRedirectHosts(), "redirect_hosts")
}

// TestMcpOAuthCompleteMapsFinalAndContinuation is the exactly-one-outcome rule.
// A continuation round must arrive as a continuation and NOTHING else: no
// server, no resource, no tool list. A finished round carries the verified names
// and no continuation. The raw callback query rides the request verbatim.
func TestMcpOAuthCompleteMapsFinalAndContinuation(t *testing.T) {
	c := assert.NewCollecting(t)

	f := &fakeMcpLogin{compRes: McpLoginComplete{
		Server:    "linear",
		Resource:  "https://mcp.example.net/mcp",
		AccountID: "",
		Tools:     []string{"create_issue", "list_issues"},
	}}
	s := &Server{}
	s.SetMcpLoginBackend(f)

	_, err := s.CompleteMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: ""}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty login_id")
	c.Eq(0, f.compCalled, "an empty login_id must not reach the backend")

	resp, err := s.CompleteMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
			LoginId:       "ml-1",
			CallbackQuery: "code=abc&state=xyz",
		}))
	c.Require().NoError(err, "CompleteMcpLogin")
	c.Eq("u1", f.owner, "backend sees the caller's user id")
	c.Eq("ml-1", f.loginID, "login id passed through")
	c.Eq("code=abc&state=xyz", f.query, "callback query passed through verbatim")
	c.True(resp.Msg.GetContinuation() == nil, "a finished round carries no continuation")
	c.Eq("linear", resp.Msg.GetServer(), "server")
	c.Eq("https://mcp.example.net/mcp", resp.Msg.GetResource(), "resource")
	c.Eq("", resp.Msg.GetAccountId(), "account id")
	c.EqDeep([]string{"create_issue", "list_issues"}, resp.Msg.GetTools(), "tool names")

	// A continuation: the descriptor is the whole answer.
	f = &fakeMcpLogin{compRes: McpLoginComplete{
		Server:    "linear",
		Resource:  "https://mcp.example.net/mcp",
		AccountID: "acct-1",
		Tools:     []string{"should-not-appear"},
		Continuation: &ProviderLoginBegin{
			LoginID:       "ml-1",
			AuthorizeURL:  "https://auth.example.net/authorize?scope=more",
			RedirectPort:  1456,
			CallbackPath:  "/mcp/oauth/callback",
			RedirectHosts: []string{"127.0.0.1"},
		},
	}}
	s = &Server{}
	s.SetMcpLoginBackend(f)

	resp, err = s.CompleteMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: "ml-1"}))
	c.Require().NoError(err, "CompleteMcpLogin (continuation)")
	cont := resp.Msg.GetContinuation()
	c.Require().NotNil(cont, "continuation")
	c.Eq("ml-1", cont.GetLoginId(), "the same login id continues")
	c.Eq("https://auth.example.net/authorize?scope=more", cont.GetAuthorizeUrl(), "continuation authorize_url")
	c.Eq(uint32(1456), cont.GetRedirectPort(), "continuation redirect_port")
	c.Eq("/mcp/oauth/callback", cont.GetCallbackPath(), "continuation callback_path")
	c.EqDeep([]string{"127.0.0.1"}, cont.GetRedirectHosts(), "continuation redirect_hosts")
	c.Eq("", resp.Msg.GetServer(), "a continuation carries no server, even when the backend set one")
	c.Eq("", resp.Msg.GetResource(), "a continuation carries no resource")
	c.Eq("", resp.Msg.GetAccountId(), "a continuation carries no account id")
	c.Eq(0, len(resp.Msg.GetTools()), "a continuation carries no tool list")
}

// TestMcpOAuthDeleteSelectors pins selector exclusivity at the handler:
// both forms, neither form, and a half-filled entry form are InvalidArgument
// and never reach the backend. The two valid forms do.
func TestMcpOAuthDeleteSelectors(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeMcpLogin{}
	s := &Server{}
	s.SetMcpLoginBackend(f)

	for _, tc := range []struct {
		name string
		req  *rafikiv1.DeleteMcpCredentialRequest
	}{
		{"both forms", &rafikiv1.DeleteMcpCredentialRequest{ConfigPath: "/c.json", Server: "linear", Resource: "https://mcp.example.net/mcp"}},
		{"neither form", &rafikiv1.DeleteMcpCredentialRequest{}},
		{"config without server", &rafikiv1.DeleteMcpCredentialRequest{ConfigPath: "/c.json"}},
		{"server without config", &rafikiv1.DeleteMcpCredentialRequest{Server: "linear"}},
	} {
		_, err := s.DeleteMcpCredential(mcpLoginUserCtx(), connect.NewRequest(tc.req))
		c.Require().Error(err, "%s", tc.name)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: code", tc.name)
	}
	c.Eq(0, f.delCalled, "a malformed selector must not reach the backend")

	_, err := s.DeleteMcpCredential(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{ConfigPath: "/srv/app/.mcp.json", Server: "linear"}))
	c.Require().NoError(err, "entry form")
	c.Eq("u1", f.owner, "backend sees the caller's user id")
	c.Eq("/srv/app/.mcp.json", f.deletions[0].ConfigPath, "config_path")
	c.Eq("linear", f.deletions[0].Server, "server")
	c.Eq("", f.deletions[0].Resource, "no resource on the entry form")

	_, err = s.DeleteMcpCredential(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{Resource: "https://mcp.example.net/mcp"}))
	c.Require().NoError(err, "resource form")
	c.Eq("https://mcp.example.net/mcp", f.deletions[1].Resource, "resource")
	c.Eq("", f.deletions[1].ConfigPath, "no config path on the resource form")
}

// TestMcpOAuthErrorMapping pins the error paths a backend can take: a login id
// that is unknown, expired or another owner's arrives as NotFound (one
// identical answer), a config/entry problem as InvalidArgument, the login
// "start again" sentinels as InvalidArgument too (the login is over, and a
// second consent URL must not be minted from a rejected round), an
// already-coded refusal keeps its code, and an uncoded failure is redacted to
// internal rather than forwarding text the daemon did not author.
func TestMcpOAuthErrorMapping(t *testing.T) {
	c := assert.NewCollecting(t)

	for _, tc := range []struct {
		name string
		err  error
		want connect.Code
	}{
		{"unknown login id", &ControllerError{Code: protocol.ErrNotFound, Message: "unknown or expired MCP login"}, connect.CodeNotFound},
		{"config problem", &ControllerError{Code: protocol.ErrInvalidArgs, Message: "cannot read the MCP config"}, connect.CodeInvalidArgument},
		{"store unavailable", &ControllerError{Code: protocol.ErrUnavailable, Message: "no credential store"}, connect.CodeUnavailable},
		{"already coded", connect.NewError(connect.CodePermissionDenied, errors.New("refused")), connect.CodePermissionDenied},
	} {
		f := &fakeMcpLogin{compErr: tc.err}
		s := &Server{}
		s.SetMcpLoginBackend(f)
		_, err := s.CompleteMcpLogin(mcpLoginUserCtx(),
			connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: "l1"}))
		c.Eq(tc.want, connect.CodeOf(err), "%s", tc.name)
	}

	f := &fakeMcpLogin{compErr: errors.New("dial tcp 10.0.0.1:5432: connect: refused")}
	s := &Server{}
	s.SetMcpLoginBackend(f)
	_, err := s.CompleteMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: "l1"}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "an uncoded failure is internal")
	c.Eq(connect.NewError(connect.CodeInternal, errors.New(internalErrText)).Error(), err.Error(),
		"an uncoded failure's text is redacted")
}

// TestMcpOAuthResponsesCarryNoCredential makes the token-free promise explicit
// for EVERY message of the three verbs — request and response, Begin, Complete
// and Delete — and by VALUE as well as by name.
//
// The name walk catches a field anyone might add later (a "bearer", a "jwt", a
// "client_secret"); it is evidence only because it covers the whole
// request/response set. The value walk is the other half: a credential smuggled
// into a generically-named field (a string called "account_id", a repeated field
// called "tools") would pass a name check, so every string and every repeated
// string the handler actually produced is compared against the values it was
// built from.
func TestMcpOAuthResponsesCarryNoCredential(t *testing.T) {
	c := assert.NewCollecting(t)

	// Values a leak would carry: the callback the caller sent, and the material
	// the backend never receives at all.
	const (
		callbackCode = "SEKRIT-callback-code"
		secret       = "SEKRIT-client-secret"
	)
	f := &fakeMcpLogin{compRes: McpLoginComplete{
		Server:   "linear",
		Resource: "https://mcp.example.net/mcp",
		Tools:    []string{"list_issues"},
	}}
	s := &Server{}
	s.SetMcpLoginBackend(f)

	begin, err := s.BeginMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{ConfigPath: "/c.json", Server: "linear"}))
	c.Require().NoError(err, "BeginMcpLogin")
	complete, err := s.CompleteMcpLogin(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{LoginId: "ml-1", CallbackQuery: "code=" + callbackCode}))
	c.Require().NoError(err, "CompleteMcpLogin")
	deleted, err := s.DeleteMcpCredential(mcpLoginUserCtx(),
		connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{ConfigPath: "/c.json", Server: "linear"}))
	c.Require().NoError(err, "DeleteMcpCredential")

	// Every request/response message of the three verbs, by descriptor.
	forbidden := []string{"token", "secret", "code", "password", "authorization", "bearer", "jwt"}
	for _, msg := range []proto.Message{
		&rafikiv1.BeginMcpLoginRequest{}, &rafikiv1.BeginMcpLoginResponse{},
		&rafikiv1.CompleteMcpLoginRequest{}, &rafikiv1.CompleteMcpLoginResponse{},
		&rafikiv1.DeleteMcpCredentialRequest{}, &rafikiv1.DeleteMcpCredentialResponse{},
		begin.Msg, complete.Msg, deleted.Msg,
	} {
		fields := msg.ProtoReflect().Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			name := string(fields.Get(i).Name())
			for _, bad := range forbidden {
				c.False(containsFold(name, bad), "%T field %q must not name a credential", msg, name)
			}
		}
	}

	// And by value: nothing the handlers produced may carry the callback code or
	// a secret, whichever field it hides in.
	for _, msg := range []proto.Message{begin.Msg, complete.Msg, deleted.Msg} {
		msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if fd.IsList() {
				list := v.List()
				for i := 0; i < list.Len(); i++ {
					val := list.Get(i).String()
					c.False(strings.Contains(val, callbackCode) || strings.Contains(val, secret),
						"%T.%s carries a credential: %q", msg, fd.Name(), val)
				}
				return true
			}
			if fd.Kind() != protoreflect.StringKind {
				return true
			}
			val := v.String()
			c.False(strings.Contains(val, callbackCode) || strings.Contains(val, secret),
				"%T.%s carries a credential: %q", msg, fd.Name(), val)
			return true
		})
	}

	// The one place the callback legitimately goes: into the backend, verbatim,
	// as an input.
	c.Eq("code="+callbackCode, f.query, "the callback query is an INPUT only; it never comes back")
}

// containsFold is a tiny ASCII case-insensitive substring test, so this file
// needs no strings import for one comparison.
func containsFold(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	if len(n) == 0 {
		return true
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		ok := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
