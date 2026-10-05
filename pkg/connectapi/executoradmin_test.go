// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeExecutorAdmin records what each handler handed it, so the tests pin both
// halves of the seam contract: the request reaches the seam unchanged, and the
// seam's answer (or error) rides out through the handler's mapping.
type fakeExecutorAdmin struct {
	enrollResp *rafikiv1.EnrollExecutorResponse
	enrollErr  error
	createResp *rafikiv1.CreateExecutorResponse
	createErr  error
	labelRow   ExecutorRow
	labelErr   error
	disableErr error
	enableErr  error
	deleteErr  error
	listRows   []ExecutorRow
	listErr    error

	sawEnroll   *rafikiv1.EnrollExecutorRequest
	sawCreate   *rafikiv1.CreateExecutorRequest
	sawLabel    *rafikiv1.LabelExecutorRequest
	sawRefs     []string
	sawSelector string
	sawLimit    int32
	calls       int
}

func (f *fakeExecutorAdmin) Enroll(_ context.Context, req *rafikiv1.EnrollExecutorRequest) (*rafikiv1.EnrollExecutorResponse, error) {
	f.calls++
	f.sawEnroll = req
	return f.enrollResp, f.enrollErr
}

func (f *fakeExecutorAdmin) Create(_ context.Context, req *rafikiv1.CreateExecutorRequest) (*rafikiv1.CreateExecutorResponse, error) {
	f.calls++
	f.sawCreate = req
	return f.createResp, f.createErr
}

func (f *fakeExecutorAdmin) Label(_ context.Context, req *rafikiv1.LabelExecutorRequest) (ExecutorRow, error) {
	f.calls++
	f.sawLabel = req
	return f.labelRow, f.labelErr
}

func (f *fakeExecutorAdmin) Disable(_ context.Context, executorID string) error {
	f.calls++
	f.sawRefs = append(f.sawRefs, executorID)
	return f.disableErr
}

func (f *fakeExecutorAdmin) Enable(_ context.Context, executorID string) error {
	f.calls++
	f.sawRefs = append(f.sawRefs, executorID)
	return f.enableErr
}

func (f *fakeExecutorAdmin) Delete(_ context.Context, executorID string) error {
	f.calls++
	f.sawRefs = append(f.sawRefs, executorID)
	return f.deleteErr
}

func (f *fakeExecutorAdmin) List(_ context.Context, selector string, limit int32) ([]ExecutorRow, error) {
	f.calls++
	f.sawSelector = selector
	f.sawLimit = limit
	return f.listRows, f.listErr
}

// TestExecutorAdminUnwiredIsUnavailable pins the seam's Unavailable path: with
// no backend attached, every handler refuses rather than nil-panicking — the
// reason SetExecutorAdmin refuses nil instead of storing &a.
func TestExecutorAdminUnwiredIsUnavailable(t *testing.T) {
	s := NewServer(nil)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"enroll", func() error {
			_, err := s.EnrollExecutor(context.Background(),
				connect.NewRequest(&rafikiv1.EnrollExecutorRequest{Ttl: durationpb.New(3600 * time.Second)}))
			return err
		}},
		{"create", func() error {
			_, err := s.CreateExecutor(context.Background(),
				connect.NewRequest(&rafikiv1.CreateExecutorRequest{}))
			return err
		}},
		{"label", func() error {
			_, err := s.LabelExecutor(context.Background(),
				connect.NewRequest(&rafikiv1.LabelExecutorRequest{ExecutorId: "exec-1", Set: map[string]string{"a": "b"}}))
			return err
		}},
		{"disable", func() error {
			_, err := s.DisableExecutor(context.Background(),
				connect.NewRequest(&rafikiv1.DisableExecutorRequest{ExecutorId: "exec-1"}))
			return err
		}},
		{"enable", func() error {
			_, err := s.EnableExecutor(context.Background(),
				connect.NewRequest(&rafikiv1.EnableExecutorRequest{ExecutorId: "exec-1"}))
			return err
		}},
		{"delete", func() error {
			_, err := s.DeleteExecutor(context.Background(),
				connect.NewRequest(&rafikiv1.DeleteExecutorRequest{ExecutorId: "exec-1"}))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "%s unwired: got %v, want CodeUnavailable", tc.name, err)
		})
	}
}

// TestExecutorAdminEnrollRefusesNonPositiveTTL mirrors the framed dispatcher:
// a non-positive (or unset) ttl is refused before the seam is touched, rather
// than silently minting the Controller's 72h default.
func TestExecutorAdminEnrollRefusesNonPositiveTTL(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorAdmin{}
	s.SetExecutorAdmin(f)
	for _, ttl := range []*durationpb.Duration{nil, durationpb.New(0), durationpb.New(-time.Second)} {
		_, err := s.EnrollExecutor(context.Background(),
			connect.NewRequest(&rafikiv1.EnrollExecutorRequest{Ttl: ttl}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "ttl %v: got %v, want CodeInvalidArgument", ttl, err)
	}
	c.Eq(0, f.calls, "seam was called")
}

// TestExecutorAdminEnrollDelegatesToSeam pins the request/response pass-through:
// the wire request reaches the seam unchanged and the seam's response is what
// goes back out.
func TestExecutorAdminEnrollDelegatesToSeam(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorAdmin{enrollResp: &rafikiv1.EnrollExecutorResponse{Token: "tok-1"}}
	s.SetExecutorAdmin(f)
	resp, err := s.EnrollExecutor(context.Background(), connect.NewRequest(&rafikiv1.EnrollExecutorRequest{
		Name: "laptop", Labels: map[string]string{"env": "work"},
		Roots: []string{"/home/brent"}, Isolation: "container",
		WorkspaceMode: "workspace", Admits: "kind=claude", Ttl: durationpb.New(3600 * time.Second),
	}))
	c.Require().NoError(err)
	c.Eq("tok-1", resp.Msg.GetToken(), "token")
	got := f.sawEnroll
	c.False(got.GetName() != "laptop" || got.GetLabels()["env"] != "work" ||
		got.GetRoots()[0] != "/home/brent" || got.GetIsolation() != "container" ||
		got.GetWorkspaceMode() != "workspace" || got.GetAdmits() != "kind=claude" ||
		got.GetTtl().AsDuration() != 3600*time.Second, "seam saw %+v, want the request's fields", got)
}

// TestExecutorAdminCreateDelegatesToSeam is Enroll's pin on the stateless path:
// row id and the shown-once credential ride out.
func TestExecutorAdminCreateDelegatesToSeam(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorAdmin{createResp: &rafikiv1.CreateExecutorResponse{
		ExecutorId: "exec-created", Credential: "credential",
	}}
	s.SetExecutorAdmin(f)
	resp, err := s.CreateExecutor(context.Background(), connect.NewRequest(&rafikiv1.CreateExecutorRequest{
		Name: "laptop", Labels: map[string]string{"env": "work"}, Roots: []string{"/home/brent"},
	}))
	c.Require().NoError(err)
	if resp.Msg.GetExecutorId() != "exec-created" || resp.Msg.GetCredential() != "credential" {
		t.Errorf("response = %+v, want the seam's row id and credential", resp.Msg)
	}
	got := f.sawCreate
	c.False(got.GetName() != "laptop" || got.GetLabels()["env"] != "work" || got.GetRoots()[0] != "/home/brent", "seam saw %+v, want the request's fields", got)
}

// TestExecutorAdminLabelDelegatesToSeam pins the field pass-through and the row
// mapping: the updated row the seam returns becomes the response's executor.
func TestExecutorAdminLabelDelegatesToSeam(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorAdmin{labelRow: ExecutorRow{ID: "exec-1", Machine: "laptop", Enabled: true}}
	s.SetExecutorAdmin(f)
	resp, err := s.LabelExecutor(context.Background(), connect.NewRequest(&rafikiv1.LabelExecutorRequest{
		ExecutorId: "exec-1",
		Set:        map[string]string{"rack": "r1"},
		Remove:     []string{"env"},
	}))
	c.Require().NoError(err)
	if row := resp.Msg.GetExecutor(); row.GetId() != "exec-1" || row.GetMachine() != "laptop" || !row.GetEnabled() {
		t.Errorf("row = %+v, want the seam's row", row)
	}
	got := f.sawLabel
	c.False(got.GetExecutorId() != "exec-1" || got.GetSet()["rack"] != "r1" || got.GetRemove()[0] != "env", "seam saw %+v, want the request's fields", got)
}

// TestExecutorAdminLabelRequiresExecutorIDAndAChange mirrors the framed
// dispatcher's refusals: an empty id, and a change-less label call, are
// invalid arguments the seam never sees.
func TestExecutorAdminLabelRequiresExecutorIDAndAChange(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorAdmin{}
	s.SetExecutorAdmin(f)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.LabelExecutorRequest
	}{
		{"empty id", &rafikiv1.LabelExecutorRequest{Set: map[string]string{"a": "b"}}},
		{"id without a change", &rafikiv1.LabelExecutorRequest{ExecutorId: "exec-1"}},
		{"id with both empty", &rafikiv1.LabelExecutorRequest{ExecutorId: "exec-1", Set: map[string]string{}, Remove: nil}},
	} {
		_, err := s.LabelExecutor(context.Background(), connect.NewRequest(tc.req))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: got %v, want CodeInvalidArgument", tc.name, err)
	}
	c.Eq(0, f.calls, "seam was called")
}

// TestExecutorAdminDisableEnableDeleteDelegateToSeam pins the three one-field
// verbs: the executor ref reaches the seam, an empty one is refused untouched.
func TestExecutorAdminDisableEnableDeleteDelegateToSeam(t *testing.T) {
	// The three verbs differ only in request type, so each is spelled out: the
	// seam's ref, and the empty-ref refusal per verb.
	t.Run("disable", func(t *testing.T) {
		c := assert.NewCollecting(t)
		s := NewServer(nil)
		f := &fakeExecutorAdmin{}
		s.SetExecutorAdmin(f)
		if _, err := s.DisableExecutor(context.Background(),
			connect.NewRequest(&rafikiv1.DisableExecutorRequest{ExecutorId: "exec-1"})); err != nil {
			t.Fatal(err)
		}
		c.Eq("exec-1", f.sawRefs[len(f.sawRefs)-1], "seam saw")
		_, err := s.DisableExecutor(context.Background(), connect.NewRequest(&rafikiv1.DisableExecutorRequest{}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty id: got %v, want CodeInvalidArgument", err)
		c.Eq(1, f.calls, "seam was called")
	})
	t.Run("enable", func(t *testing.T) {
		c := assert.NewCollecting(t)
		s := NewServer(nil)
		f := &fakeExecutorAdmin{}
		s.SetExecutorAdmin(f)
		if _, err := s.EnableExecutor(context.Background(),
			connect.NewRequest(&rafikiv1.EnableExecutorRequest{ExecutorId: "exec-2"})); err != nil {
			t.Fatal(err)
		}
		c.Eq("exec-2", f.sawRefs[len(f.sawRefs)-1], "seam saw")
		_, err := s.EnableExecutor(context.Background(), connect.NewRequest(&rafikiv1.EnableExecutorRequest{}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty id: got %v, want CodeInvalidArgument", err)
		c.Eq(1, f.calls, "seam was called")
	})
	t.Run("delete", func(t *testing.T) {
		c := assert.NewCollecting(t)
		s := NewServer(nil)
		f := &fakeExecutorAdmin{}
		s.SetExecutorAdmin(f)
		if _, err := s.DeleteExecutor(context.Background(),
			connect.NewRequest(&rafikiv1.DeleteExecutorRequest{ExecutorId: "exec-3"})); err != nil {
			t.Fatal(err)
		}
		c.Eq("exec-3", f.sawRefs[len(f.sawRefs)-1], "seam saw")
		_, err := s.DeleteExecutor(context.Background(), connect.NewRequest(&rafikiv1.DeleteExecutorRequest{}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty id: got %v, want CodeInvalidArgument", err)
		c.Eq(1, f.calls, "seam was called")
	})
}

// TestExecutorAdminUncodedErrorIsRedactedAndCodedPasses pins the two error
// shapes the seam can hand a handler. An UNCODED error is infrastructure text:
// it reaches the client as Internal with the fixed redacted text, its raw
// cause never on the wire. A ControllerError keeps its code and authored
// message — translateExecutorErr's classifications are the point of mapping
// with ConnectErr.
func TestExecutorAdminUncodedErrorIsRedactedAndCodedPasses(t *testing.T) {
	t.Run("uncoded is internal and redacted", func(t *testing.T) {
		c := assert.NewCollecting(t)
		s := NewServer(nil)
		s.SetExecutorAdmin(&fakeExecutorAdmin{
			enrollErr: errors.New("pgx: conn to db.internal:5432 refused"),
		})
		_, err := s.EnrollExecutor(context.Background(),
			connect.NewRequest(&rafikiv1.EnrollExecutorRequest{Ttl: durationpb.New(3600 * time.Second)}))
		c.Require().Eq(connect.CodeInternal, connect.CodeOf(err), "got %v, want CodeInternal", err)
		c.False(err == nil || !strings.Contains(err.Error(), internalErrText), "err.Error() = %v, want the fixed redacted text", err)
		c.NotStrContains(err.Error(), "db.internal", "err.Error() = %v, want the raw cause redacted", err)
	})
	t.Run("controller error keeps code and message", func(t *testing.T) {
		c := assert.NewCollecting(t)
		s := NewServer(nil)
		s.SetExecutorAdmin(&fakeExecutorAdmin{enrollErr: &ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "that executor name is already taken for this owner",
		}})
		_, err := s.EnrollExecutor(context.Background(),
			connect.NewRequest(&rafikiv1.EnrollExecutorRequest{Name: "laptop", Ttl: durationpb.New(3600 * time.Second)}))
		c.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got %v, want CodeInvalidArgument", err)
		c.StrContains(err.Error(), "that executor name is already taken for this owner", "err.Error() = %v, want the authored message", err)
	})
}

// logRecorder is a slog handler that records every message routed to it, so
// a test can pin that the mapper did NOT log.
type logRecorder struct{ msgs *[]string }

func (h logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h logRecorder) Handle(_ context.Context, r slog.Record) error {
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h logRecorder) WithGroup(string) slog.Handler      { return h }

// TestExecutorAdminCodedErrorPassesThrough mirrors
// TestChildOpsCodedErrorPassesThrough: an already-coded *connect.Error from
// the seam takes the early return the sibling mappers have, so it reaches the
// client untouched — code preserved, authored text not redacted — and the
// mapper never logs it, because a coded error is a classification, not an
// infrastructure failure to investigate.
func TestExecutorAdminCodedErrorPassesThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	var msgs []string
	prev := slog.Default()
	slog.SetDefault(slog.New(logRecorder{msgs: &msgs}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := NewServer(nil)
	s.SetExecutorAdmin(&fakeExecutorAdmin{
		enrollErr: connect.NewError(connect.CodePermissionDenied,
			errors.New("conversation queries require a user credential")),
	})
	_, err := s.EnrollExecutor(context.Background(),
		connect.NewRequest(&rafikiv1.EnrollExecutorRequest{Ttl: durationpb.New(3600 * time.Second)}))
	c.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "got %v, want PermissionDenied", err)
	c.False(err == nil || !strings.Contains(err.Error(), "require a user credential"), "err.Error() = %v, want the authored text, not redaction", err)
	c.Empty(msgs, "mapper logged %d record(s) for a coded error", len(msgs))
}
