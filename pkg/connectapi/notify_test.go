// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// fakeNotifier records what the handler handed it and answers with the canned
// child id or error.
type fakeNotifier struct {
	calls   []fakeNotifyCall
	childID string
	err     error
}

type fakeNotifyCall struct {
	ownerUserID string
	anonymous   bool
	target      string
	source      string
	key         string
	fields      map[string]string
}

func (f *fakeNotifier) Notify(_ context.Context, ownerUserID string, anonymous bool, target, source, key string, fields map[string]string) (string, error) {
	f.calls = append(f.calls, fakeNotifyCall{
		ownerUserID: ownerUserID,
		anonymous:   anonymous,
		target:      target,
		source:      source,
		key:         key,
		fields:      fields,
	})
	return f.childID, f.err
}

func notifyRequest(target, source, key string, fields map[string]string) *connect.Request[rafikiv1.NotifyRequest] {
	return connect.NewRequest(&rafikiv1.NotifyRequest{
		Target: target, Source: source, Key: key, Fields: fields,
	})
}

func TestNotifySetNotifierNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetNotifier(nil)
	_, err := s.Notify(context.Background(), notifyRequest("main", "github", "", nil))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err),
		"after SetNotifier(nil): want CodeUnavailable, got %v", err)
}

func TestNotifyUnwiredIsUnavailable(t *testing.T) {
	s := &Server{}
	_, err := s.Notify(context.Background(), notifyRequest("main", "github", "", nil))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err),
		"unwired notifier: want CodeUnavailable, got %v", err)
}

func TestNotifyEmptyTargetInvalid(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, target := range []string{"", "   ", "\t\n"} {
		notifier := &fakeNotifier{childID: "c_1"}
		s := &Server{}
		s.SetNotifier(notifier)
		_, err := s.Notify(context.Background(), notifyRequest(target, "github", "", nil))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err),
			"target %q: want CodeInvalidArgument, got %v", target, err)
		c.Eq(0, len(notifier.calls), "target %q: notifier must not be called", target)
	}
}

func TestNotifyPassesOwnerAndFields(t *testing.T) {
	c := assert.NewAborting(t)
	fields := map[string]string{"repo": "rafiki", "event": "push"}

	notifier := &fakeNotifier{childID: "c_child"}
	s := &Server{}
	s.SetNotifier(notifier)

	userCtx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u_1", Username: "brent", Via: server.ProvenanceUser})
	resp, err := s.Notify(userCtx, notifyRequest("  main  ", "github", "push:42", fields))
	c.Require().NoError(err, "user call")
	c.Eq("c_child", resp.Msg.GetChildId(), "child id")
	c.Require().Eq(1, len(notifier.calls), "user call recorded")
	got := notifier.calls[0]
	c.Eq("u_1", got.ownerUserID, "user owner")
	c.False(got.anonymous, "user call is not anonymous")
	c.Eq("main", got.target, "target is trimmed")
	c.Eq("github", got.source, "source")
	c.Eq("push:42", got.key, "key")
	c.EqDeep(fields, got.fields, "fields passed through")

	_, err = s.Notify(context.Background(), notifyRequest("main", "github", "", nil))
	c.Require().NoError(err, "anonymous call")
	c.Require().Eq(2, len(notifier.calls), "anonymous call recorded")
	got = notifier.calls[1]
	c.Eq("", got.ownerUserID, "anonymous owner")
	c.True(got.anonymous, "no identity is the anonymous unix-socket caller")
}

func TestNotifyMapsControllerErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		want connect.Code
	}{
		{"not found", protocol.ErrNotFound, connect.CodeNotFound},
		{"invalid args", protocol.ErrInvalidArgs, connect.CodeInvalidArgument},
		{"unavailable", protocol.ErrUnavailable, connect.CodeUnavailable},
		{"already exists", protocol.ErrAlreadyExists, connect.CodeAlreadyExists},
	} {
		notifier := &fakeNotifier{err: &ControllerError{Code: tc.code, Message: tc.name}}
		s := &Server{}
		s.SetNotifier(notifier)
		_, err := s.Notify(context.Background(), notifyRequest("main", "github", "", nil))
		assert.NewCollecting(t).Eq(tc.want, connect.CodeOf(err),
			"%s: want %v, got %v", tc.name, tc.want, err)
	}
}
