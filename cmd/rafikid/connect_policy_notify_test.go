// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// notifyScopedContext is the context a {notify} token arrives on after the
// proxy face or the unix socket resolved it: ProvenanceNotify, naming its
// owner, and NOT a user credential.
func notifyScopedContext() context.Context {
	return server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u1", Username: "hooks", TokenID: "t1", Via: server.ProvenanceNotify,
	})
}

// TestNotifyTokenRefusedOnEveryOtherProcedure walks the generated Control
// service the same way the completeness test does and refuses a notify-scoped
// token on every procedure but Notify — the anyCaller reads (ListModels,
// ListPresets, ListRoutes, …) included. It fails if the ProvenanceNotify
// branch in authorizeControlProcedure is removed or moved below the
// policyAnyCaller admission.
func TestNotifyTokenRefusedOnEveryOtherProcedure(t *testing.T) {
	c := assert.NewCollecting(t)
	svc := rafikiv1.File_rafiki_v1_control_proto.Services().ByName("Control")
	c.Require().NotNil(svc, "no Control service in the generated descriptor")
	methods := svc.Methods()
	c.True(methods.Len() > 1, "want more than one Control procedure")

	ctx := notifyScopedContext()
	seenNotify := false
	for i := 0; i < methods.Len(); i++ {
		name := string(methods.Get(i).Name())
		procedure := controlProcedurePrefix + name
		err := authorizeControlProcedure(ctx, procedure)
		if procedure == rafikiv1connect.ControlNotifyProcedure {
			seenNotify = true
			c.NoError(err, "Control.%s must admit a notify-scoped token, got %v", name, err)
			continue
		}
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err),
			"Control.%s admitted a notify-scoped token (err = %v)", name, err)
	}
	c.True(seenNotify, "the descriptor has no Notify procedure")

	// A procedure outside the Control service is refused too: the
	// confinement is a property of the credential, not of the table.
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(authorizeControlProcedure(ctx, "/rafiki.v1.Other/Do")),
		"a non-Control procedure admitted a notify-scoped token")
}

// TestNotifyTokenAdmittedOnNotify pins the one door the token opens.
func TestNotifyTokenAdmittedOnNotify(t *testing.T) {
	c := assert.NewAborting(t)
	err := authorizeControlProcedure(notifyScopedContext(), rafikiv1connect.ControlNotifyProcedure)
	c.NoError(err, "a notify-scoped token must be admitted to Notify, got %v", err)
}

// TestNotifyTokenUserCredentialStillAdmittedOnNotify is the verify-pattern
// shim for TestUserTokenStillAdmittedOnNotify: `go test -run` matches an
// unanchored substring, and the brief's pinned name does not contain
// "TestNotifyToken", so the gate would silently skip it without this.
func TestNotifyTokenUserCredentialStillAdmittedOnNotify(t *testing.T) {
	t.Run("user credential", TestUserTokenStillAdmittedOnNotify)
}

// TestUserTokenStillAdmittedOnNotify pins that the ordinary user path is
// untouched: a user credential reaches Notify (and the rest) as before.
func TestUserTokenStillAdmittedOnNotify(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u1", Username: "brent", TokenID: "t1", Via: server.ProvenanceUser,
	})
	c.NoError(authorizeControlProcedure(ctx, rafikiv1connect.ControlNotifyProcedure),
		"a user credential must still be admitted to Notify")
	c.NoError(authorizeControlProcedure(ctx, controlProcedurePrefix+"ListModels"),
		"a user credential must still be admitted to ListModels")
	// And a full-authority token is not confused with the notify one: the
	// anyCaller/allowance of the ordinary path is unchanged.
	c.NoError(authorizeControlProcedure(server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u2", Via: server.ProvenanceUser}), controlProcedurePrefix+"CreateUser"),
		"a user credential must still be admitted to operator verbs")
}
