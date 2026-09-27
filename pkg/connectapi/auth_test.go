// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"

	"github.com/multigres/testkit/assert"
)

// callWith runs the interceptor around a handler that records whether it ran.
func callWith(t *testing.T, token, header string) (ran bool, err error) {
	t.Helper()
	interceptor := connectapi.NewAuthInterceptor(token)
	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		ran = true
		return nil, nil
	})
	req := connect.NewRequest(&struct{}{})
	if header != "" {
		req.Header().Set("Authorization", header)
	}
	_, err = interceptor.WrapUnary(next)(context.Background(), req)
	return ran, err
}

// An empty configured token disables the check. That is the UDS mount: trust
// is the 0600 socket in the 0700 directory.
func TestEmptyTokenDisablesTheCheck(t *testing.T) {
	c := assert.NewAborting(t)
	ran, err := callWith(t, "", "")
	c.NoError(err, "want the call admitted, got error")
	c.True(ran, "want the handler to run")
}

func TestCorrectBearerTokenIsAdmitted(t *testing.T) {
	c := assert.NewAborting(t)
	ran, err := callWith(t, "s3cret", "Bearer s3cret")
	c.NoError(err, "want the call admitted, got error")
	c.True(ran, "want the handler to run")
}

func TestWrongTokenIsUnauthenticated(t *testing.T) {
	c := assert.NewAborting(t)
	ran, err := callWith(t, "s3cret", "Bearer wrong")
	c.Error(err, "want an error for a wrong token")
	c.Eq(connect.CodeUnauthenticated, connect.CodeOf(err), "want CodeUnauthenticated, got")
	c.False(ran, "the handler must not run for a rejected call")
}

func TestMissingTokenIsUnauthenticated(t *testing.T) {
	c := assert.NewAborting(t)
	ran, err := callWith(t, "s3cret", "")
	c.Error(err, "want an error when no credential is presented")
	c.Eq(connect.CodeUnauthenticated, connect.CodeOf(err), "want CodeUnauthenticated, got")
	c.False(ran, "the handler must not run for a rejected call")
}
