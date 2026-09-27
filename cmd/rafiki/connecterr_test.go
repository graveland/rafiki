// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

func TestConnectErrFormatPrefersTheRafikiReason(t *testing.T) {
	e := rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New("child c_1 already exited")),
		protocol.ErrChildExited)
	// The reason names rafiki's precise cause; Connect's coarser code is
	// dropped when one is attached.
	want := "child_exited: child c_1 already exited"
	assert.NewAborting(t).Eq(want, formatConnectErr(e), "formatConnectErr")
}

func TestConnectErrFormatFallsBackToCodeAndMessage(t *testing.T) {
	e := connect.NewError(connect.CodeNotFound, errors.New("no such child"))
	want := "not_found: no such child"
	assert.NewAborting(t).Eq(want, formatConnectErr(e), "formatConnectErr")
}

func TestConnectErrFormatUnwrapsWrappedErrors(t *testing.T) {
	c := assert.NewAborting(t)
	e := rpcreason.Attach(
		connect.NewError(connect.CodePermissionDenied, errors.New("operator-only spawn field")),
		protocol.ErrInvalidArgs)
	wrapped := fmt.Errorf("spawn: %w", e)
	want := "invalid_args: operator-only spawn field"
	c.Eq(want, formatConnectErr(wrapped), "formatConnectErr(wrapped)")

	plain := connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
	wrappedPlain := fmt.Errorf("dial: %w", plain)
	c.Eq("unavailable: connection refused", formatConnectErr(wrappedPlain), "formatConnectErr(wrapped plain) =")
}

func TestConnectErrFormatNonConnectError(t *testing.T) {
	err := errors.New("ordinary failure")
	assert.NewAborting(t).Eq("ordinary failure", formatConnectErr(err), "formatConnectErr")
}

func TestConnectErrFormatNilIsSafe(t *testing.T) {
	assert.NewAborting(t).Eq("", formatConnectErr(nil), "formatConnectErr(nil)")
}
