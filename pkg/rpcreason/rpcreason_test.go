// SPDX-License-Identifier: Apache-2.0

package rpcreason_test

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

func TestAttachReasonRoundTrips(t *testing.T) {
	c := assert.NewCollecting(t)
	e := connect.NewError(connect.CodeFailedPrecondition, errors.New("child c_1 already exited"))
	got := rpcreason.Attach(e, "child_exited")
	c.Require().Eq(e, got, "Attach returned a different error")
	// A client reads it back through the error interface, not the pointer.
	var err error = got
	c.Eq("child_exited", rpcreason.Reason(err), "Reason")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "CodeOf")
}

func TestReasonIgnoresForeignDomain(t *testing.T) {
	c := assert.NewCollecting(t)
	e := connect.NewError(connect.CodeInternal, errors.New("boom"))
	foreign, ferr := connect.NewErrorDetail(&errdetails.ErrorInfo{Reason: "someone_elses", Domain: "example.com"})
	c.Require().NoError(ferr, "NewErrorDetail")
	e.AddDetail(foreign)
	// A detail of a different proto type is skipped the same way.
	retry, rerr := connect.NewErrorDetail(&errdetails.RetryInfo{RetryDelay: nil})
	c.Require().NoError(rerr, "NewErrorDetail")
	e.AddDetail(retry)
	if got := rpcreason.Reason(e); got != "" {
		t.Errorf("Reason with only foreign details = %q, want \"\"", got)
	}
	// A rafiki detail added after foreign ones is still found.
	_ = rpcreason.Attach(e, "child_exited")
	c.Eq("child_exited", rpcreason.Reason(e), "Reason after Attach")
}

func TestReasonPlainErrorIsEmpty(t *testing.T) {
	if got := rpcreason.Reason(errors.New("boom")); got != "" {
		t.Errorf("Reason(plain error) = %q, want \"\"", got)
	}
	if got := rpcreason.Reason(nil); got != "" {
		t.Errorf("Reason(nil) = %q, want \"\"", got)
	}
	// A Connect error that carries no details at all.
	got := rpcreason.Reason(connect.NewError(connect.CodeInternal, errors.New("boom")))
	assert.NewCollecting(t).Eq("", got, "Reason(undecorated connect error) = %q, want \"\"", got)
}
