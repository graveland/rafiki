// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
)

func TestConnectErrFormatPrefersTheRafikiReason(t *testing.T) {
	e := rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New("child c_1 already exited")),
		protocol.ErrChildExited)
	// The reason names rafiki's precise cause; Connect's coarser code is
	// dropped when one is attached.
	want := "child_exited: child c_1 already exited"
	if got := formatConnectErr(e); got != want {
		t.Fatalf("formatConnectErr = %q, want %q", got, want)
	}
}

func TestConnectErrFormatFallsBackToCodeAndMessage(t *testing.T) {
	e := connect.NewError(connect.CodeNotFound, errors.New("no such child"))
	want := "not_found: no such child"
	if got := formatConnectErr(e); got != want {
		t.Fatalf("formatConnectErr = %q, want %q", got, want)
	}
}

func TestConnectErrFormatUnwrapsWrappedErrors(t *testing.T) {
	e := rpcreason.Attach(
		connect.NewError(connect.CodePermissionDenied, errors.New("operator-only spawn field")),
		protocol.ErrInvalidArgs)
	wrapped := fmt.Errorf("spawn: %w", e)
	want := "invalid_args: operator-only spawn field"
	if got := formatConnectErr(wrapped); got != want {
		t.Fatalf("formatConnectErr(wrapped) = %q, want %q", got, want)
	}

	plain := connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
	wrappedPlain := fmt.Errorf("dial: %w", plain)
	if got := formatConnectErr(wrappedPlain); got != "unavailable: connection refused" {
		t.Fatalf("formatConnectErr(wrapped plain) = %q", got)
	}
}

func TestConnectErrFormatNonConnectError(t *testing.T) {
	err := errors.New("ordinary failure")
	if got := formatConnectErr(err); got != "ordinary failure" {
		t.Fatalf("formatConnectErr = %q, want the error's own text", got)
	}
}

func TestConnectErrFormatNilIsSafe(t *testing.T) {
	if got := formatConnectErr(nil); got != "" {
		t.Fatalf("formatConnectErr(nil) = %q, want empty", got)
	}
}
