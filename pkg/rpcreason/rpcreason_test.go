// SPDX-License-Identifier: Apache-2.0

package rpcreason_test

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"go.graveland.dev/rafiki/pkg/rpcreason"
)

func TestAttachReasonRoundTrips(t *testing.T) {
	e := connect.NewError(connect.CodeFailedPrecondition, errors.New("child c_1 already exited"))
	got := rpcreason.Attach(e, "child_exited")
	if got != e {
		t.Fatalf("Attach returned a different error: %v", got)
	}
	// A client reads it back through the error interface, not the pointer.
	var err error = got
	if reason := rpcreason.Reason(err); reason != "child_exited" {
		t.Errorf("Reason = %q, want child_exited", reason)
	}
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("CodeOf = %v, want FailedPrecondition", code)
	}
}

func TestReasonIgnoresForeignDomain(t *testing.T) {
	e := connect.NewError(connect.CodeInternal, errors.New("boom"))
	foreign, ferr := connect.NewErrorDetail(&errdetails.ErrorInfo{Reason: "someone_elses", Domain: "example.com"})
	if ferr != nil {
		t.Fatalf("NewErrorDetail: %v", ferr)
	}
	e.AddDetail(foreign)
	// A detail of a different proto type is skipped the same way.
	retry, rerr := connect.NewErrorDetail(&errdetails.RetryInfo{RetryDelay: nil})
	if rerr != nil {
		t.Fatalf("NewErrorDetail: %v", rerr)
	}
	e.AddDetail(retry)
	if got := rpcreason.Reason(e); got != "" {
		t.Errorf("Reason with only foreign details = %q, want \"\"", got)
	}
	// A rafiki detail added after foreign ones is still found.
	_ = rpcreason.Attach(e, "child_exited")
	if got := rpcreason.Reason(e); got != "child_exited" {
		t.Errorf("Reason after Attach = %q, want child_exited", got)
	}
}

func TestReasonPlainErrorIsEmpty(t *testing.T) {
	if got := rpcreason.Reason(errors.New("boom")); got != "" {
		t.Errorf("Reason(plain error) = %q, want \"\"", got)
	}
	if got := rpcreason.Reason(nil); got != "" {
		t.Errorf("Reason(nil) = %q, want \"\"", got)
	}
	// A Connect error that carries no details at all.
	if got := rpcreason.Reason(connect.NewError(connect.CodeInternal, errors.New("boom"))); got != "" {
		t.Errorf("Reason(undecorated connect error) = %q, want \"\"", got)
	}
}
