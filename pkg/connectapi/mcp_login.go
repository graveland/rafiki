// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/server"
)

// McpLoginComplete is the outcome of one MCP consent round.
//
// Exactly ONE outcome is ever described. When Continuation is non-nil it is the
// whole answer — the next round's descriptor for the SAME login id — and the
// final fields (Server/Resource/AccountID/Tools) must not be read as a success:
// nothing is stored yet, and the login is only finished when a round answers a
// continuation-free result.
type McpLoginComplete struct {
	Server    string
	Resource  string
	AccountID string
	// Tools are the tool NAMES the verified session served, sorted. Never the
	// tool descriptions, never a response body.
	Tools []string
	// Continuation is the next consent round (a step-up scope challenge), or
	// nil when the login finished.
	Continuation *ProviderLoginBegin
}

// McpDeleteSelector names what a logout removes: a configured entry
// (ConfigPath+Server) or a canonical resource. Exactly one form is set, and the
// handler refuses a request that sets both or neither.
type McpDeleteSelector struct {
	ConfigPath string
	Server     string
	Resource   string
}

// McpLoginBackend is the daemon's MCP OAuth login engine. ownerUserID is never
// empty and always names a REAL user credential: the handler refuses an
// identity-less caller (FailedPrecondition) and a non-user credential such as a
// child's (PermissionDenied) before calling any method.
//
// The backend binds a login to the owner that began it. CompleteMcpLogin
// answers NotFound for a login id that is unknown, expired, or was begun by a
// DIFFERENT owner: one identical answer for all three, so a caller cannot learn
// that another user's login exists. A config path the daemon cannot read, a
// server name the file does not declare, and an entry whose shape cannot log in
// (a stdio server, a via_executor server, an entry with an explicit
// Authorization header) are protocol.ErrInvalidArgs (InvalidArgument on the
// wire), never NotFound.
//
// Nothing secret crosses this seam: Begin answers a consent URL, Complete
// answers which resource was verified and the tool names it serves, and every
// token stays in the daemon (sealed at rest, never returned by any RPC).
type McpLoginBackend interface {
	BeginMcpLogin(ctx context.Context, ownerUserID, configPath, server string, scopes []string) (ProviderLoginBegin, error)
	CompleteMcpLogin(ctx context.Context, ownerUserID, loginID, callbackQuery string) (McpLoginComplete, error)
	DeleteMcpCredential(ctx context.Context, ownerUserID string, sel McpDeleteSelector) error
}

// SetMcpLoginBackend attaches the MCP OAuth login engine. Post-construction
// setter, same reason as SetProviderLoginBackend: the controller that owns the
// engine is built after this Server, while the listener is already serving. A
// nil backend is refused rather than stored: storing &b for a nil interface
// would make Load() return a non-nil pointer to a nil interface, defeating
// mcpLoginBackend's Unavailable path and nil-panicking the first handler call.
func (s *Server) SetMcpLoginBackend(b McpLoginBackend) {
	if b == nil {
		return
	}
	s.mcpLogin.Store(&b)
}

func (s *Server) mcpLoginBackend() (McpLoginBackend, error) {
	b := s.mcpLogin.Load()
	if b == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("MCP OAuth login backend not yet wired"))
	}
	return *b, nil
}

// mcpLoginOwner is the caller's own user id, the only owner an MCP grant is
// ever stored against.
//
// The provenance clause is defence in depth, not the primary boundary: the
// policy gate (connectPolicyTable, all three verbs policyUserOnly) runs first
// and refuses every non-user credential. But a child credential CARRIES its
// owner's UserID (pkg/server/usertoken.go), so a UserID-shaped check alone
// cannot tell a user from a child holding its owner's identity — and an MCP
// grant is spendable by that owner's children, which is exactly the authority a
// child must not be able to plant.
//
// The two refusals say different things on purpose: an identity with no user id
// at all (the unix socket's nil local trust) has nothing to bind a grant to,
// which is FailedPrecondition, while a real identity of the wrong KIND is a
// refusal of authority, which is PermissionDenied.
func mcpLoginOwner(ctx context.Context) (string, error) {
	id := server.IdentityFromContext(ctx)
	if id == nil || id.UserID == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			errors.New("MCP login needs a user credential; log in with `rafiki login` or use a profile with a user token"))
	}
	if !id.IsUserCredential() {
		return "", connect.NewError(connect.CodePermissionDenied,
			errors.New("MCP login is a user-credential surface; a child credential cannot begin, complete or delete its owner's MCP logins"))
	}
	return id.UserID, nil
}

// mcpLoginError maps a backend error. An error the daemon already coded keeps
// its code; anything else takes the package's standard fallthrough, so an
// uncoded failure is redacted to internal and logged. Three reasons carry the
// contract: protocol.ErrInvalidArgs for a config path, server name or entry
// shape the daemon cannot log in with, protocol.ErrNotFound for a login id that
// is unknown, expired or another owner's (one identical answer) and for a
// logout that names a grant the caller does not have, and protocol.ErrUnavailable
// when the credential store is not configured.
func mcpLoginError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return unmappedErr(err, "mapper", "mcpLoginError")
}

func mcpBeginResponse(begin ProviderLoginBegin) *rafikiv1.BeginMcpLoginResponse {
	return &rafikiv1.BeginMcpLoginResponse{
		LoginId:       begin.LoginID,
		AuthorizeUrl:  begin.AuthorizeURL,
		RedirectPort:  begin.RedirectPort,
		CallbackPath:  begin.CallbackPath,
		RedirectHosts: begin.RedirectHosts,
	}
}

func (s *Server) BeginMcpLogin(
	ctx context.Context, req *connect.Request[rafikiv1.BeginMcpLoginRequest],
) (*connect.Response[rafikiv1.BeginMcpLoginResponse], error) {
	b, err := s.mcpLoginBackend()
	if err != nil {
		return nil, err
	}
	owner, err := mcpLoginOwner(ctx)
	if err != nil {
		return nil, err
	}
	configPath := req.Msg.GetConfigPath()
	if configPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("config_path is required: the daemon reads an absolute path on its own host"))
	}
	serverName := req.Msg.GetServer()
	if serverName == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("server is required: name the .mcp.json entry to log in to"))
	}
	begin, err := b.BeginMcpLogin(ctx, owner, configPath, serverName, req.Msg.GetScopes())
	if err != nil {
		return nil, mcpLoginError(err)
	}
	return connect.NewResponse(mcpBeginResponse(begin)), nil
}

func (s *Server) CompleteMcpLogin(
	ctx context.Context, req *connect.Request[rafikiv1.CompleteMcpLoginRequest],
) (*connect.Response[rafikiv1.CompleteMcpLoginResponse], error) {
	b, err := s.mcpLoginBackend()
	if err != nil {
		return nil, err
	}
	owner, err := mcpLoginOwner(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetLoginId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("login_id is required"))
	}
	done, err := b.CompleteMcpLogin(ctx, owner, req.Msg.GetLoginId(), req.Msg.GetCallbackQuery())
	if err != nil {
		return nil, mcpLoginError(err)
	}
	// Exactly one outcome. A continuation is the WHOLE answer: the final fields
	// describe nothing until a continuation-free round answers, so the two are
	// never mixed — a client must not read a half-finished login as a success.
	if done.Continuation != nil {
		return connect.NewResponse(&rafikiv1.CompleteMcpLoginResponse{
			Continuation: mcpBeginResponse(*done.Continuation),
		}), nil
	}
	return connect.NewResponse(&rafikiv1.CompleteMcpLoginResponse{
		Server:    done.Server,
		Resource:  done.Resource,
		AccountId: done.AccountID,
		Tools:     done.Tools,
	}), nil
}

func (s *Server) DeleteMcpCredential(
	ctx context.Context, req *connect.Request[rafikiv1.DeleteMcpCredentialRequest],
) (*connect.Response[rafikiv1.DeleteMcpCredentialResponse], error) {
	b, err := s.mcpLoginBackend()
	if err != nil {
		return nil, err
	}
	owner, err := mcpLoginOwner(ctx)
	if err != nil {
		return nil, err
	}
	sel := McpDeleteSelector{
		ConfigPath: req.Msg.GetConfigPath(),
		Server:     req.Msg.GetServer(),
		Resource:   req.Msg.GetResource(),
	}
	byEntry := sel.ConfigPath != "" || sel.Server != ""
	byResource := sel.Resource != ""
	switch {
	case byEntry && byResource:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("give either config_path and server, or resource, not both"))
	case byEntry && (sel.ConfigPath == "" || sel.Server == ""):
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("the entry form needs both config_path and server"))
	case !byEntry && !byResource:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("give either config_path and server, or resource"))
	}
	if err := b.DeleteMcpCredential(ctx, owner, sel); err != nil {
		return nil, mcpLoginError(err)
	}
	return connect.NewResponse(&rafikiv1.DeleteMcpCredentialResponse{}), nil
}
