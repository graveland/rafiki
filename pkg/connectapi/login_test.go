// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// fakeLoginBackend records what it was called with and replays canned
// responses/errors.
type fakeLoginBackend struct {
	beginResp    *rafikiv1.BeginLoginResponse
	beginErr     error
	completeResp *rafikiv1.CompleteLoginResponse
	completeErr  error

	gotBegin    [2]any // redirectPort, clientHost
	gotComplete [2]any // loginID, callbackQuery
}

func (f *fakeLoginBackend) Begin(_ context.Context, redirectPort uint32, clientHost string) (*rafikiv1.BeginLoginResponse, error) {
	f.gotBegin = [2]any{redirectPort, clientHost}
	return f.beginResp, f.beginErr
}

func (f *fakeLoginBackend) Complete(_ context.Context, loginID, callbackQuery string) (*rafikiv1.CompleteLoginResponse, error) {
	f.gotComplete = [2]any{loginID, callbackQuery}
	return f.completeResp, f.completeErr
}

// TestLoginUnconfigured pins the no-backend state both verbs answer with:
// FailedPrecondition with a legible reason, for a caller whose credential is
// missing or expired and who therefore cannot be told much else.
func TestLoginUnconfigured(t *testing.T) {
	c := assert.NewAborting(t)
	s := &Server{}
	ctx := context.Background()

	_, err := loginService{s: s}.BeginLogin(ctx, connect.NewRequest(&rafikiv1.BeginLoginRequest{RedirectPort: 8765, ClientHost: "laptop"}))
	c.Require().Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "BeginLogin code")
	c.StrContains(err.Error(), "OIDC login is not configured", "BeginLogin message")

	_, err = loginService{s: s}.CompleteLogin(ctx, connect.NewRequest(&rafikiv1.CompleteLoginRequest{LoginId: "l1", CallbackQuery: "code=x"}))
	c.Require().Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "CompleteLogin code")
	c.StrContains(err.Error(), "no oidc.toml", "CompleteLogin message")
}

// TestSetLoginBackendNilIsRefused pins the CLAUDE.md setter rule directly: a
// nil backend is refused, never stored — a stored pointer to a nil interface
// would defeat the Unconfigured path and nil-panic the first handler call.
func TestSetLoginBackendNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetLoginBackend(nil)
	assert.NewAborting(t).Nil(s.login.Load(), "SetLoginBackend(nil) stored a pointer")
}

// TestLoginPassesThroughCodedErrors proves the handler passes a backend's
// already-coded error through untouched (the engine's own refusal is authored
// text) and redacts an uncoded one to CodeInternal — logging the cause first,
// so an IdP or database failure never names its host on the wire.
func TestLoginPassesThroughCodedErrors(t *testing.T) {
	ctx := context.Background()
	coded := connect.NewError(connect.CodeInvalidArgument, errors.New("state mismatch"))
	outage := errors.New("dial tcp idp.internal:443: connection refused")

	for _, tc := range []struct {
		name     string
		err      error
		wantCode connect.Code
	}{
		{"coded passes through", coded, connect.CodeInvalidArgument},
		{"uncoded redacted", outage, connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			s := &Server{}
			s.SetLoginBackend(&fakeLoginBackend{beginErr: tc.err, completeErr: tc.err})

			_, err := loginService{s: s}.BeginLogin(ctx, connect.NewRequest(&rafikiv1.BeginLoginRequest{}))
			c.Require().Eq(tc.wantCode, connect.CodeOf(err), "BeginLogin code")
			_, err = loginService{s: s}.CompleteLogin(ctx, connect.NewRequest(&rafikiv1.CompleteLoginRequest{}))
			c.Require().Eq(tc.wantCode, connect.CodeOf(err), "CompleteLogin code")

			if tc.wantCode == connect.CodeInternal {
				_, err := loginService{s: s}.BeginLogin(ctx, connect.NewRequest(&rafikiv1.BeginLoginRequest{}))
				assert.NewCollecting(t).NotStrContains(err.Error(), outage.Error(),
					"uncoded error text leaked onto the wire; it must be redacted")
			}
		})
	}
}

// TestLoginConfiguredReflectsBackend pins LoginConfigured as a pure nil check:
// false before a backend is wired, true after — the flag the daemon's wiring
// consults to decide whether CreateUser mints a bootstrap token.
func TestLoginConfiguredReflectsBackend(t *testing.T) {
	c := assert.NewAborting(t)
	s := &Server{}
	c.False(s.LoginConfigured(), "LoginConfigured before any backend is wired")

	s.SetLoginBackend(&fakeLoginBackend{})
	c.True(s.LoginConfigured(), "LoginConfigured after SetLoginBackend")

	s.SetLoginBackend(nil)
	c.True(s.LoginConfigured(), "a nil SetLoginBackend must not unconfigure a wired backend")
}

// TestLoginRoundTrip proves the handlers pass requests and responses through
// the seam unmodified.
func TestLoginRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLoginBackend{
		beginResp:    &rafikiv1.BeginLoginResponse{LoginId: "l1", AuthorizeUrl: "https://idp/authorize", RedirectPort: 9999},
		completeResp: &rafikiv1.CompleteLoginResponse{Token: "rfk_login", Username: "alice", ExpiresAtUnix: 100},
	}
	s := &Server{}
	s.SetLoginBackend(f)
	ctx := context.Background()

	begin, err := loginService{s: s}.BeginLogin(ctx, connect.NewRequest(&rafikiv1.BeginLoginRequest{RedirectPort: 8765, ClientHost: "laptop"}))
	c.NoError(err, "BeginLogin")
	if begin.Msg.GetLoginId() != "l1" || begin.Msg.GetAuthorizeUrl() != "https://idp/authorize" || begin.Msg.GetRedirectPort() != 9999 {
		t.Fatalf("BeginLogin response = %+v", begin.Msg)
	}
	if f.gotBegin != [2]any{uint32(8765), "laptop"} {
		t.Fatalf("Begin saw = %v", f.gotBegin)
	}

	complete, err := loginService{s: s}.CompleteLogin(ctx, connect.NewRequest(&rafikiv1.CompleteLoginRequest{LoginId: "l1", CallbackQuery: "code=x&state=y"}))
	c.NoError(err, "CompleteLogin")
	if complete.Msg.GetToken() != "rfk_login" || complete.Msg.GetUsername() != "alice" || complete.Msg.GetExpiresAtUnix() != 100 {
		t.Fatalf("CompleteLogin response = %+v", complete.Msg)
	}
	if f.gotComplete != [2]any{"l1", "code=x&state=y"} {
		t.Fatalf("Complete saw = %v", f.gotComplete)
	}

	// LoginRoutes builds the service's handler without mounting it anywhere.
	path, handler := s.LoginRoutes()
	c.Eq("/rafiki.v1.Login/", path, "route prefix")
	c.NotNil(handler, "handler")
}
