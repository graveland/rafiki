// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// fakeProviderLogin is a ProviderLoginBackend that records the owner it was
// handed, so a test can pin that the handler passes the CALLER's user id,
// never a value from the request, which carries no user at all.
type fakeProviderLogin struct {
	beginRes ProviderLoginBegin
	beginErr error
	compErr  error
	delErr   error

	owner   string
	arg     string // provider for Begin/Delete, login id for Complete
	query   string
	deleted []string
}

func (f *fakeProviderLogin) BeginProviderLogin(_ context.Context, ownerUserID, provider string) (ProviderLoginBegin, error) {
	f.owner, f.arg = ownerUserID, provider
	if f.beginErr != nil {
		return ProviderLoginBegin{}, f.beginErr
	}
	return f.beginRes, nil
}

func (f *fakeProviderLogin) CompleteProviderLogin(_ context.Context, ownerUserID, loginID, callbackQuery string) (string, string, error) {
	f.owner, f.arg, f.query = ownerUserID, loginID, callbackQuery
	if f.compErr != nil {
		return "", "", f.compErr
	}
	return "openai-responses-chatgpt", "acct_123", nil
}

func (f *fakeProviderLogin) DeleteProviderCredential(_ context.Context, ownerUserID, provider string) error {
	f.owner, f.arg = ownerUserID, provider
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, provider)
	return nil
}

// userCtx is a real user credential, the only caller shape these verbs admit.
func providerLoginUserCtx() context.Context {
	return server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
}

// TestSetProviderLoginBackendNilIsRefused pins the shared setter rule: a nil
// backend must leave the seam unwired rather than store &b for a nil
// interface, which would defeat the Unavailable path and nil-panic the first
// handler call.
func TestSetProviderLoginBackendNilIsRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	s.SetProviderLoginBackend(nil)

	_, err := s.BeginProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: "chatgpt"}))
	c.Require().Error(err, "BeginProviderLogin after SetProviderLoginBackend(nil)")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetProviderLoginBackend(nil): want Unavailable")
}

// TestProviderLoginUnavailableWhenUnwired: every verb answers Unavailable
// until the daemon wires its engine, rather than panicking or reporting an
// empty success.
func TestProviderLoginUnavailableWhenUnwired(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	ctx := providerLoginUserCtx()

	_, err := s.BeginProviderLogin(ctx, connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: "chatgpt"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "BeginProviderLogin unwired")

	_, err = s.CompleteProviderLogin(ctx, connect.NewRequest(&rafikiv1.CompleteProviderLoginRequest{LoginId: "l1"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "CompleteProviderLogin unwired")

	_, err = s.DeleteProviderCredential(ctx, connect.NewRequest(&rafikiv1.DeleteProviderCredentialRequest{Provider: "chatgpt"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "DeleteProviderCredential unwired")
}

// TestProviderLoginNeedsAUserCredential: a caller with no identity (the unix
// socket's local trust) or one that names no user id is refused
// FailedPrecondition by every verb, and the backend is never called: there is
// no user to store a provider grant against.
func TestProviderLoginNeedsAUserCredential(t *testing.T) {
	c := assert.NewCollecting(t)

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"nil identity", context.Background()},
		{"no user id", server.WithIdentity(context.Background(), &server.Identity{})},
	} {
		f := &fakeProviderLogin{}
		s := &Server{}
		s.SetProviderLoginBackend(f)

		_, err := s.BeginProviderLogin(tc.ctx,
			connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: "chatgpt"}))
		c.Require().Error(err, "%s: BeginProviderLogin", tc.name)
		c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "%s: BeginProviderLogin code", tc.name)

		_, err = s.CompleteProviderLogin(tc.ctx,
			connect.NewRequest(&rafikiv1.CompleteProviderLoginRequest{LoginId: "l1"}))
		c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "%s: CompleteProviderLogin code", tc.name)

		_, err = s.DeleteProviderCredential(tc.ctx,
			connect.NewRequest(&rafikiv1.DeleteProviderCredentialRequest{Provider: "chatgpt"}))
		c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "%s: DeleteProviderCredential code", tc.name)

		c.Eq("", f.owner, "%s: backend must not be reached without a user id", tc.name)
	}
}

// TestProviderLoginRefusesAChildCredential is the handler-layer half of the
// security boundary. Every one of these identities carries a real UserID, so a
// UserID-shaped check cannot tell them from a user: a per-child secret carries
// its owner's id (pkg/server/usertoken.go), so does the per-boot+session shape,
// and a {notify} token names the user whose agents it may notify. Each is
// refused PermissionDenied by every verb and the backend is never called, which
// is what keeps the refusal from resting on the policy table alone.
func TestProviderLoginRefusesAChildCredential(t *testing.T) {
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
		f := &fakeProviderLogin{}
		s := &Server{}
		s.SetProviderLoginBackend(f)

		_, err := s.BeginProviderLogin(ctx,
			connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: "chatgpt"}))
		c.Require().Error(err, "%s: BeginProviderLogin", tc.name)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s: BeginProviderLogin code", tc.name)

		_, err = s.CompleteProviderLogin(ctx,
			connect.NewRequest(&rafikiv1.CompleteProviderLoginRequest{LoginId: "l1"}))
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s: CompleteProviderLogin code", tc.name)

		_, err = s.DeleteProviderCredential(ctx,
			connect.NewRequest(&rafikiv1.DeleteProviderCredentialRequest{Provider: "chatgpt"}))
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s: DeleteProviderCredential code", tc.name)

		c.Eq("", f.owner, "%s: a non-user credential must never reach the backend", tc.name)
	}
}

// TestBeginProviderLoginMapsTheBackendAnswer pins the request/response
// mapping, that the backend receives the CALLER's user id (the request
// carries none), and that an empty provider never reaches it.
func TestBeginProviderLoginMapsTheBackendAnswer(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeProviderLogin{beginRes: ProviderLoginBegin{
		LoginID:       "login-7",
		AuthorizeURL:  "https://auth.example/authorize?x=1",
		RedirectPort:  1455,
		CallbackPath:  "/callback",
		RedirectHosts: []string{"127.0.0.1", "localhost"},
	}}
	s := &Server{}
	s.SetProviderLoginBackend(f)

	_, err := s.BeginProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: ""}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty provider")
	c.Eq("", f.owner, "an empty provider must not reach the backend")

	resp, err := s.BeginProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: "chatgpt"}))
	c.Require().NoError(err, "BeginProviderLogin")
	c.Eq("u1", f.owner, "backend sees the caller's user id, not one from the request")
	c.Eq("chatgpt", f.arg, "provider passed through")
	c.Eq("login-7", resp.Msg.GetLoginId(), "login_id")
	c.Eq("https://auth.example/authorize?x=1", resp.Msg.GetAuthorizeUrl(), "authorize_url")
	c.Eq(uint32(1455), resp.Msg.GetRedirectPort(), "redirect_port")
	c.Eq("/callback", resp.Msg.GetCallbackPath(), "callback_path")
	c.EqDeep([]string{"127.0.0.1", "localhost"}, resp.Msg.GetRedirectHosts(), "redirect_hosts")
}

// TestCompleteProviderLoginMapsTheBackendAnswer: the raw callback query rides
// the request verbatim, and the response carries the provider and account id,
// never a token.
func TestCompleteProviderLoginMapsTheBackendAnswer(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeProviderLogin{}
	s := &Server{}
	s.SetProviderLoginBackend(f)

	_, err := s.CompleteProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteProviderLoginRequest{LoginId: ""}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty login_id")

	resp, err := s.CompleteProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteProviderLoginRequest{
			LoginId:       "login-7",
			CallbackQuery: "code=abc&state=xyz",
		}))
	c.Require().NoError(err, "CompleteProviderLogin")
	c.Eq("u1", f.owner, "backend sees the caller's user id")
	c.Eq("login-7", f.arg, "login id passed through")
	c.Eq("code=abc&state=xyz", f.query, "callback query passed through verbatim")
	c.Eq("openai-responses-chatgpt", resp.Msg.GetProvider(), "provider")
	c.Eq("acct_123", resp.Msg.GetAccountId(), "account id")
}

// TestDeleteProviderCredentialMapsTheRequest: the caller's own provider grant
// is what is removed, and an empty provider never reaches the backend.
func TestDeleteProviderCredentialMapsTheRequest(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeProviderLogin{}
	s := &Server{}
	s.SetProviderLoginBackend(f)

	_, err := s.DeleteProviderCredential(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.DeleteProviderCredentialRequest{}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty provider")

	_, err = s.DeleteProviderCredential(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.DeleteProviderCredentialRequest{Provider: "chatgpt"}))
	c.Require().NoError(err, "DeleteProviderCredential")
	c.Eq("u1", f.owner, "backend sees the caller's user id")
	c.EqDeep([]string{"chatgpt"}, f.deleted, "deleted provider")
}

// TestProviderLoginErrorMapping pins the error paths a backend can take: an
// absent credential or an unknown/expired/foreign login id arrives as
// CodeNotFound (a ControllerError carrying protocol.ErrNotFound, the package's
// typed mapping), a provider that is unknown or does not use login
// authentication as CodeInvalidArgument (protocol.ErrInvalidArgs), an
// already-coded refusal keeps its code, and an uncoded failure is redacted to
// internal rather than forwarding text the daemon did not author.
func TestProviderLoginErrorMapping(t *testing.T) {
	c := assert.NewCollecting(t)

	notFound := &ControllerError{Code: protocol.ErrNotFound, Message: "no credential for provider"}
	f := &fakeProviderLogin{delErr: notFound}
	s := &Server{}
	s.SetProviderLoginBackend(f)

	_, err := s.DeleteProviderCredential(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.DeleteProviderCredentialRequest{Provider: "chatgpt"}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "absent credential")

	// An unknown provider, or one whose kind does not authenticate by login,
	// is the caller's mistake about the request, not a missing row: the backend
	// promotes it as protocol.ErrInvalidArgs and it arrives InvalidArgument,
	// never NotFound (which would suggest a credential exists somewhere).
	f = &fakeProviderLogin{beginErr: &ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: "provider \"nope\" is not configured for login",
	}}
	s = &Server{}
	s.SetProviderLoginBackend(f)

	_, err = s.BeginProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: "nope"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "unknown provider")

	coded := connect.NewError(connect.CodePermissionDenied, errors.New("refused"))
	f = &fakeProviderLogin{beginErr: coded}
	s = &Server{}
	s.SetProviderLoginBackend(f)

	_, err = s.BeginProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.BeginProviderLoginRequest{Provider: "chatgpt"}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "an already-coded refusal keeps its code")

	f = &fakeProviderLogin{compErr: errors.New("dial tcp 10.0.0.1:5432: connect: refused")}
	s = &Server{}
	s.SetProviderLoginBackend(f)

	_, err = s.CompleteProviderLogin(providerLoginUserCtx(),
		connect.NewRequest(&rafikiv1.CompleteProviderLoginRequest{LoginId: "l1"}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "an uncoded failure is internal")
	c.Eq(connect.NewError(connect.CodeInternal, errors.New(internalErrText)).Error(), err.Error(),
		"an uncoded failure's text is redacted")
}
