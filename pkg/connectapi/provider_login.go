// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/server"
)

// ProviderLoginBegin is what a started provider login tells the client: the
// login id the completion call names, the URL to open, and where the IdP will
// send the browser back. The redirect URI is fixed by the provider's
// registration, so the client must listen on RedirectPort at CallbackPath on
// every host in RedirectHosts, or accept a pasted redirect URL.
type ProviderLoginBegin struct {
	LoginID, AuthorizeURL, CallbackPath string
	RedirectPort                        uint32
	RedirectHosts                       []string
}

// ProviderLoginBackend is the daemon's provider-login engine. ownerUserID is
// never empty: the handler refuses identity-less callers before calling it.
//
// Tokens never cross this seam: Begin returns a URL, Complete returns the
// provider and the authenticated account id, and the credential itself stays
// in the daemon (encrypted at rest, never returned by any RPC).
type ProviderLoginBackend interface {
	BeginProviderLogin(ctx context.Context, ownerUserID, provider string) (ProviderLoginBegin, error)
	CompleteProviderLogin(ctx context.Context, ownerUserID, loginID, callbackQuery string) (provider, accountID string, err error)
	DeleteProviderCredential(ctx context.Context, ownerUserID, provider string) error
}

// SetProviderLoginBackend attaches the provider-login engine. Post-construction
// setter, same reason as SetPymoduleManager: the controller that owns the
// engine is built after this Server, while the listener is already serving. A
// nil backend is refused rather than stored: storing &b for a nil interface
// would make Load() return a non-nil pointer to a nil interface, defeating
// providerLoginBackend's Unavailable path and nil-panicking the first handler
// call instead.
func (s *Server) SetProviderLoginBackend(b ProviderLoginBackend) {
	if b == nil {
		return
	}
	s.providerLogin.Store(&b)
}

func (s *Server) providerLoginBackend() (ProviderLoginBackend, error) {
	p := s.providerLogin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("provider login backend not yet wired"))
	}
	return *p, nil
}

// providerLoginOwner is the caller's own user id, the only owner a provider
// credential is ever stored against. The check is on UserID rather than on
// IsUserCredential(): the policy gate refuses every non-user credential shape
// before a handler runs, and the unix socket's nil local trust has no user to
// store a credential for, which is exactly what FailedPrecondition says.
func providerLoginOwner(ctx context.Context) (string, error) {
	id := server.IdentityFromContext(ctx)
	if id == nil || id.UserID == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			errors.New("provider login needs a user credential; log in with `rafiki login` or use a profile with a user token"))
	}
	return id.UserID, nil
}

// providerLoginError maps a backend error. An error the daemon already coded
// keeps its code; anything else takes the package's standard fallthrough, so a
// *ControllerError carrying protocol.ErrNotFound (an absent credential)
// arrives as CodeNotFound and an uncoded failure is redacted to internal.
func providerLoginError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return unmappedErr(err, "mapper", "providerLoginError")
}

func (s *Server) BeginProviderLogin(
	ctx context.Context, req *connect.Request[rafikiv1.BeginProviderLoginRequest],
) (*connect.Response[rafikiv1.BeginProviderLoginResponse], error) {
	b, err := s.providerLoginBackend()
	if err != nil {
		return nil, err
	}
	owner, err := providerLoginOwner(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetProvider() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("provider is required"))
	}
	begin, err := b.BeginProviderLogin(ctx, owner, req.Msg.GetProvider())
	if err != nil {
		return nil, providerLoginError(err)
	}
	return connect.NewResponse(&rafikiv1.BeginProviderLoginResponse{
		LoginId:       begin.LoginID,
		AuthorizeUrl:  begin.AuthorizeURL,
		RedirectPort:  begin.RedirectPort,
		CallbackPath:  begin.CallbackPath,
		RedirectHosts: begin.RedirectHosts,
	}), nil
}

func (s *Server) CompleteProviderLogin(
	ctx context.Context, req *connect.Request[rafikiv1.CompleteProviderLoginRequest],
) (*connect.Response[rafikiv1.CompleteProviderLoginResponse], error) {
	b, err := s.providerLoginBackend()
	if err != nil {
		return nil, err
	}
	owner, err := providerLoginOwner(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetLoginId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("login_id is required"))
	}
	provider, accountID, err := b.CompleteProviderLogin(ctx, owner, req.Msg.GetLoginId(), req.Msg.GetCallbackQuery())
	if err != nil {
		return nil, providerLoginError(err)
	}
	return connect.NewResponse(&rafikiv1.CompleteProviderLoginResponse{
		Provider:  provider,
		AccountId: accountID,
	}), nil
}

func (s *Server) DeleteProviderCredential(
	ctx context.Context, req *connect.Request[rafikiv1.DeleteProviderCredentialRequest],
) (*connect.Response[rafikiv1.DeleteProviderCredentialResponse], error) {
	b, err := s.providerLoginBackend()
	if err != nil {
		return nil, err
	}
	owner, err := providerLoginOwner(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetProvider() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("provider is required"))
	}
	if err := b.DeleteProviderCredential(ctx, owner, req.Msg.GetProvider()); err != nil {
		return nil, providerLoginError(err)
	}
	return connect.NewResponse(&rafikiv1.DeleteProviderCredentialResponse{}), nil
}
