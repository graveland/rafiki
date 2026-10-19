// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

const storeLeak = "pgx: failed to connect to host=db.internal user=rafiki password=hunter2"

// Each per-manager error mapper ends in a fallthrough for "everything that is
// not a sentinel I recognise". That used to wrap the raw error as Internal,
// which forwarded store text to the peer and flattened a ControllerError to a
// code-less Internal with no reason. It must now behave as ConnectErr does.
func TestBackendErrorMappersRedactStoreFailuresAndKeepReasons(t *testing.T) {
	mappers := map[string]func(error) error{
		"presetError":      presetError,
		"providerBanError": providerBanError,
		"pymoduleError":    pymoduleError,
		"recallError":      recallError,
		"repoError":        repoError,
		"routeError":       routeError,
		"skillError":       skillError,
	}
	for name, mapper := range mappers {
		t.Run(name, func(t *testing.T) {
			c := assert.NewCollecting(t)

			leaked := mapper(errors.New(storeLeak))
			c.Eq(connect.CodeInternal, connect.CodeOf(leaked), "store failure code")
			c.NotStrContains(leaked.Error(), "hunter2", "store text must not reach the peer")
			c.NotStrContains(leaked.Error(), "db.internal", "store text must not reach the peer")

			coded := mapper(&ControllerError{Code: protocol.ErrBudgetExhausted, Message: "over budget"})
			c.Eq(protocol.ErrBudgetExhausted, rpcreason.Reason(coded), "a ControllerError keeps its reason")
			c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(coded), "a ControllerError keeps its code")
		})
	}
}

type failingModels struct{ err error }

func (f failingModels) ListModels(context.Context, string, string) ([]ModelRow, error) {
	return nil, f.err
}

type failingQuota struct{ err error }

func (f failingQuota) RateLimitStatus(context.Context) (RateLimitStatus, bool, error) {
	return RateLimitStatus{}, false, f.err
}

type failingTasks struct{ err error }

func (f failingTasks) TaskList(context.Context, protocol.TaskListRequest) ([]tasks.Task, error) {
	return nil, f.err
}

// The handlers that return a backend error inline, with no mapper in between,
// get the same treatment.
func TestInlineBackendErrorsAreRedactedAndKeepReasons(t *testing.T) {
	calls := map[string]func(err error) error{
		"ListModels": func(err error) error {
			s := NewServer(nil)
			s.SetModelLister(failingModels{err})
			_, e := s.ListModels(context.Background(), connect.NewRequest(&rafikiv1.ListModelsRequest{}))
			return e
		},
		"GetRateLimitStatus": func(err error) error {
			s := NewServer(nil)
			s.SetQuotaReader(failingQuota{err})
			_, e := s.GetRateLimitStatus(context.Background(), connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{}))
			return e
		},
		"ListTasks": func(err error) error {
			s := NewServer(nil)
			s.SetTaskLister(failingTasks{err})
			_, e := s.ListTasks(context.Background(), connect.NewRequest(&rafikiv1.ListTasksRequest{}))
			return e
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			c := assert.NewCollecting(t)

			leaked := call(errors.New(storeLeak))
			c.Eq(connect.CodeInternal, connect.CodeOf(leaked), "store failure code")
			c.NotStrContains(leaked.Error(), "hunter2", "store text must not reach the peer")

			coded := call(&ControllerError{Code: protocol.ErrInvalidArgs, Message: "bad input"})
			c.Eq(protocol.ErrInvalidArgs, rpcreason.Reason(coded), "a ControllerError keeps its reason")
		})
	}
}
