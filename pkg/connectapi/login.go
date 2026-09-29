// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// errLoginUnconfigured is what both Login verbs answer while no backend is
// wired — the one state an unauthenticated caller can act on, so it must be
// legible rather than a bare unimplemented.
var errLoginUnconfigured = connect.NewError(connect.CodeFailedPrecondition,
	errors.New("OIDC login is not configured on this daemon (no oidc.toml)"))

// LoginBackend is the daemon's OIDC login engine. Unset = OIDC not configured.
// It is a post-construction setter like every other backend: the engine is
// built after this Server, and the HTTP listener is already serving when it is
// wired, hence the atomic pointer behind SetLoginBackend.
type LoginBackend interface {
	Begin(ctx context.Context, redirectPort uint32, clientHost string) (*rafikiv1.BeginLoginResponse, error)
	Complete(ctx context.Context, loginID, callbackQuery string) (*rafikiv1.CompleteLoginResponse, error)
}

// SetLoginBackend attaches the OIDC login engine. A nil backend is refused
// rather than stored, the same rule as SetUserAdmin: storing &b for a nil
// interface would make Load() return a non-nil pointer to a nil interface,
// defeating the Unconfigured path and nil-panicking the first handler call.
func (s *Server) SetLoginBackend(b LoginBackend) {
	if b == nil {
		return
	}
	s.login.Store(&b)
}

// LoginConfigured reports whether an OIDC login engine is wired — a pure nil
// check on the atomic pointer, safe to call before any backend is wired, when
// it answers false.
func (s *Server) LoginConfigured() bool {
	return s.login.Load() != nil
}

func (s *Server) loginBackend() (LoginBackend, error) {
	p := s.login.Load()
	if p == nil {
		return nil, errLoginUnconfigured
	}
	return *p, nil
}

// loginErr passes an already-coded error through untouched and redacts
// everything else to CodeInternal, logging the cause first, the same shape
// userAdminErr applies. The Login service answers callers with NO valid
// credential, so anything that might name the daemon's IdP or database stays
// off the wire.
func loginErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	slog.Error("connect: login call failed", "error", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// LoginRoutes builds the Login service's handler for a mount. It returns the
// path prefix and the handler the way Server.Routes does, so the daemon can
// mount it wherever it serves unauthenticated traffic. Nothing in this package
// mounts it: that is the wiring task's job.
func (s *Server) LoginRoutes(opts ...connect.HandlerOption) (string, http.Handler) {
	return rafikiv1connect.NewLoginHandler(loginService{s: s}, opts...)
}

// loginService implements rafikiv1connect.LoginHandler on top of the wired
// backend. It never reads caller identity — the Login service exists to
// mint a first or replacement credential.
type loginService struct {
	s *Server
}

func (h loginService) BeginLogin(
	ctx context.Context,
	req *connect.Request[rafikiv1.BeginLoginRequest],
) (*connect.Response[rafikiv1.BeginLoginResponse], error) {
	backend, err := h.s.loginBackend()
	if err != nil {
		return nil, err
	}
	resp, err := backend.Begin(ctx, req.Msg.GetRedirectPort(), req.Msg.GetClientHost())
	if err != nil {
		return nil, loginErr(err)
	}
	return connect.NewResponse(resp), nil
}

func (h loginService) CompleteLogin(
	ctx context.Context,
	req *connect.Request[rafikiv1.CompleteLoginRequest],
) (*connect.Response[rafikiv1.CompleteLoginResponse], error) {
	backend, err := h.s.loginBackend()
	if err != nil {
		return nil, err
	}
	resp, err := backend.Complete(ctx, req.Msg.GetLoginId(), req.Msg.GetCallbackQuery())
	if err != nil {
		return nil, loginErr(err)
	}
	return connect.NewResponse(resp), nil
}
