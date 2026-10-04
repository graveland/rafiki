// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// namedChildScope is a ChildScope whose ChildID is fixed and non-empty -- the
// shape Spawn's operator-only-field guard must refuse against, distinct from
// emptyChildScope (childscope_test.go), which pins the separate empty-id
// refusal that runs AFTER this one in the handler.
type namedChildScope struct{}

func (namedChildScope) ChildID() string                          { return "c_caller" }
func (namedChildScope) Authorize(string) error                   { return nil }
func (namedChildScope) Subtree([]string) []protocol.ChildSummary { return nil }
func (namedChildScope) ConversationInScope(string) bool          { return false }

func childScopedServer(f connectapi.ChildLifecycle) *connectapi.Server {
	s := connectapi.NewServer(nil)
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return namedChildScope{} })
	s.SetChildLifecycle(f)
	return s
}

// setOperatorOnlyField sets req's field number n to a representative non-zero
// value of its declared kind, so the caller doesn't need a type switch per
// field name.
func setOperatorOnlyField(t *testing.T, req *rafikiv1.SpawnRequest, fd protoreflect.FieldDescriptor) {
	t.Helper()
	m := req.ProtoReflect()
	switch {
	case fd.IsMap():
		mp := m.Mutable(fd).Map()
		mp.Set(protoreflect.ValueOfString("k").MapKey(), protoreflect.ValueOfString("v"))
	case fd.IsList():
		m.Mutable(fd).List().Append(protoreflect.ValueOfString("v"))
	case fd.Kind() == protoreflect.BoolKind:
		m.Set(fd, protoreflect.ValueOfBool(true))
	case fd.Kind() == protoreflect.StringKind:
		m.Set(fd, protoreflect.ValueOfString("x"))
	default:
		t.Fatalf("field %s: unsupported kind %v for this test", fd.Name(), fd.Kind())
	}
}

// operatorOnlyFieldMin and operatorOnlyFieldMax record today's OPERATOR-ONLY
// SpawnRequest range (control.proto fields 15-29: config_dir through
// passthrough_auth). Production (verbs.go's firstOperatorOnlySet) no longer
// consults a range -- it fails closed on anything outside
// childAllowedSpawnFields -- so these constants live here, not in verbs.go,
// purely to give TestSpawnRequestFieldsAreClassified something explicit to
// check every field number against.
const (
	operatorOnlyFieldMin protoreflect.FieldNumber = 15
	operatorOnlyFieldMax protoreflect.FieldNumber = 29
)

// childAllowedFieldNumbers mirrors verbs.go's childAllowedSpawnFields (fields
// 1-14 and 30: cwd through script, the three budget fields, and
// skip_derived_index). It is kept as its own copy, not an import of the
// unexported production set, because this file is package connectapi_test --
// and duplicating it here is the point: TestSpawnRequestFieldsAreClassified
// fails the moment a field number exists that this list and
// operatorOnlyFieldMin/Max don't between them cover, forcing a conscious
// classification decision on both sides.
var childAllowedFieldNumbers = map[protoreflect.FieldNumber]bool{
	1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true,
	8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true,
	30: true,
}

// TestSpawnRefusesEveryOperatorOnlyFieldToAChildCaller walks SpawnRequest's
// OPERATOR-ONLY range (control.proto fields 15-29: config_dir through
// passthrough_auth) by descriptor NUMBER rather than a literal field list, so
// a field added later inside the range is exercised automatically -- the same
// property the production guard (firstOperatorOnlySet in verbs.go) claims for
// itself. Each subtest sets exactly ONE field on an otherwise-empty request
// and drives it through a childScoped Spawn call: the security boundary must
// refuse every one of them with PermissionDenied, naming the field, and the
// lifecycle must never be reached. exercised is counted and must be nonzero,
// so a renumbered or moved range can't make this test pass having tested
// nothing (W2a).
func TestSpawnRefusesEveryOperatorOnlyFieldToAChildCaller(t *testing.T) {
	fields := (&rafikiv1.SpawnRequest{}).ProtoReflect().Descriptor().Fields()
	exercised := 0
	for n := operatorOnlyFieldMin; n <= operatorOnlyFieldMax; n++ {
		fd := fields.ByNumber(n)
		if fd == nil {
			continue
		}
		exercised++
		t.Run(string(fd.Name()), func(t *testing.T) {
			c := assert.NewCollecting(t)
			f := &fakeLifecycle{}
			s := childScopedServer(f)
			req := &rafikiv1.SpawnRequest{Cwd: "/tmp"}
			setOperatorOnlyField(t, req, fd)

			_, err := s.Spawn(context.Background(), connect.NewRequest(req))
			c.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Spawn with %s set (child caller) err = %v, want PermissionDenied", fd.Name(), err)
			if !strings.Contains(err.Error(), string(fd.Name())) || !strings.Contains(err.Error(), "operator-only") {
				t.Errorf("error %q does not name %s as operator-only", err.Error(), fd.Name())
			}
			c.Eq("", f.got.Cwd, "Spawn reached the lifecycle despite the refusal: %+v", f.got)
		})
	}
	assert.NewAborting(t).NotEq(0, exercised, "exercised zero operator-only fields -- the range or the descriptor moved, and this test now proves nothing")
}

// TestSpawnRequestFieldsAreClassified walks every field number SpawnRequest's
// descriptor actually declares and fails unless each one is classified --
// either in childAllowedFieldNumbers or in [operatorOnlyFieldMin,
// operatorOnlyFieldMax]. A field landing outside both (field 30 and up,
// today) is still refused to a child caller by firstOperatorOnlySet's
// fail-closed default, but that's an accident of the guard's design, not a
// decision anyone made -- this test turns a new field into a build failure
// until someone puts it in one of the two buckets on purpose, on both sides
// (this file's classification and verbs.go's childAllowedSpawnFields).
func TestSpawnRequestFieldsAreClassified(t *testing.T) {
	c := assert.NewAborting(t)
	fields := (&rafikiv1.SpawnRequest{}).ProtoReflect().Descriptor().Fields()
	c.NotEq(0, fields.Len(), "SpawnRequest descriptor reports zero fields -- nothing was classified")
	classified := 0
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		n := fd.Number()
		switch {
		case childAllowedFieldNumbers[n]:
			classified++
		case n >= operatorOnlyFieldMin && n <= operatorOnlyFieldMax:
			classified++
		default:
			t.Errorf("SpawnRequest field %d (%s) is not classified as child-allowed or operator-only: "+
				"add it to childAllowedFieldNumbers or the operator-only range in this file, and to "+
				"childAllowedSpawnFields in verbs.go if it should be child-reachable", n, fd.Name())
		}
	}
	c.Eq(fields.Len(), classified, "classified")
}

// TestSpawnAdmitsSkipDerivedIndexFromAChild pins field 30 as child-allowed: a
// child caller may set ONLY skip_derived_index, and the value must reach the
// lifecycle's SpawnParams. It dies if 30 is dropped from childAllowedSpawnFields
// (the refusal fires) or from the proto->SpawnParams mapping (the param stays
// false).
func TestSpawnAdmitsSkipDerivedIndexFromAChild(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{}
	s := childScopedServer(f)

	req := &rafikiv1.SpawnRequest{Cwd: "/tmp", SkipDerivedIndex: true}
	_, err := s.Spawn(context.Background(), connect.NewRequest(req))
	c.Require().NoError(err, "child Spawn with only skip_derived_index refused")
	c.Eq(true, f.got.SkipDerivedIndex, "skip_derived_index did not reach SpawnParams: %+v", f.got)
}

// TestSpawnAdmitsAUserCallerWithEveryOperatorOnlyFieldSet is the positive
// mirror: the operator path (no child scope wired, matching every other
// Spawn test in this package that means to exercise it) may set every
// OPERATOR-ONLY field at once, and every one of them must reach SpawnParams.
func TestSpawnAdmitsAUserCallerWithEveryOperatorOnlyFieldSet(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	req := &rafikiv1.SpawnRequest{
		Cwd:                "/tmp",
		ConfigDir:          "/cfg",
		AppendSystemPrompt: "be nice",
		Thinking:           "high",
		NoSession:          true,
		ResumeSession:      "sess-1",
		ForkSession:        "sess-0",
		Extensions:         []string{"ext"},
		NoExtensions:       true,
		Verbose:            true,
		ExtraArgs:          []string{"--flag"},
		SkillsDirs:         []string{"/skills"},
		McpConfig:          "/mcp.json",
		Env:                map[string]string{"K": "V"},
		RecordRequests:     true,
		PassthroughAuth:    "on",
	}
	_, err := s.Spawn(context.Background(), connect.NewRequest(req))
	assert.NewAborting(t).NoError(err, "Spawn (operator, every operator-only field set) err")
	if f.got.ConfigDir != "/cfg" || f.got.AppendSystemPrompt != "be nice" || f.got.Thinking != "high" ||
		!f.got.NoSession || f.got.ResumeSession != "sess-1" || f.got.ForkSession != "sess-0" ||
		len(f.got.Extensions) != 1 || f.got.Extensions[0] != "ext" || !f.got.NoExtensions || !f.got.Verbose ||
		len(f.got.ExtraArgs) != 1 || f.got.ExtraArgs[0] != "--flag" ||
		len(f.got.SkillsDirs) != 1 || f.got.SkillsDirs[0] != "/skills" ||
		f.got.MCPConfig != "/mcp.json" || f.got.Env["K"] != "V" || !f.got.RecordRequests ||
		f.got.PassthroughAuth != "on" {
		t.Errorf("operator-only fields were not carried through: %+v", f.got)
	}
}

// subtreeChildScope is a ChildScope whose Subtree answers a fixed set,
// ignoring statuses (status filtering is pinned elsewhere). It exists to
// exercise ListChildren's other filter fields on the childScoped path, where
// there is no framed protocol.ListFilter application to reuse (see verbs.go's
// matchesChildFilter doc comment).
type subtreeChildScope struct{ all []protocol.ChildSummary }

func (subtreeChildScope) ChildID() string                            { return "c_caller" }
func (subtreeChildScope) Authorize(string) error                     { return nil }
func (s subtreeChildScope) Subtree([]string) []protocol.ChildSummary { return s.all }
func (subtreeChildScope) ConversationInScope(string) bool            { return false }

func childIDsOf(cs []*rafikiv1.ChildSummary) map[string]bool {
	out := make(map[string]bool, len(cs))
	for _, c := range cs {
		out[c.GetChildId()] = true
	}
	return out
}

// TestListChildrenFiltersMatchListFilterSemantics pins name / name_contains /
// cwd_contains / since / labels / has_label on BOTH ListChildren branches --
// the operator path (ChildLister) and the childScoped path (ChildScope.
// Subtree) -- with the exact protocol.ListFilter semantics the framed plane's
// the child-list filter applies. Filters only ever narrow a child caller's subtree, never
// widen it.
func TestListChildrenFiltersMatchListFilterSemantics(t *testing.T) {
	all := []protocol.ChildSummary{
		{ChildID: "c_1", Name: "scout-1", Cwd: "/work/a", StartedAt: 100, Labels: map[string]string{"team": "a", "env": "prod"}},
		{ChildID: "c_2", Name: "scout-2", Cwd: "/work/b", StartedAt: 200, Labels: map[string]string{"team": "b"}},
	}
	cases := []struct {
		name string
		req  *rafikiv1.ListChildrenRequest
		want []string
	}{
		{"name exact", &rafikiv1.ListChildrenRequest{Name: "scout-1"}, []string{"c_1"}},
		{"name_contains", &rafikiv1.ListChildrenRequest{NameContains: "scout"}, []string{"c_1", "c_2"}},
		{"cwd_contains", &rafikiv1.ListChildrenRequest{CwdContains: "/work/b"}, []string{"c_2"}},
		{"since", &rafikiv1.ListChildrenRequest{Since: 150}, []string{"c_2"}},
		{"labels AND-match", &rafikiv1.ListChildrenRequest{Labels: map[string]string{"team": "a"}}, []string{"c_1"}},
		{"has_label", &rafikiv1.ListChildrenRequest{HasLabel: []string{"env"}}, []string{"c_1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			want := make(map[string]bool, len(tc.want))
			for _, id := range tc.want {
				want[id] = true
			}

			opServer := connectapi.NewServer(nil)
			opServer.SetChildLister(&fakeLister{all: all})
			opResp, err := opServer.ListChildren(context.Background(), connect.NewRequest(tc.req))
			c.Require().NoError(err, "ListChildren (operator)")
			if got := childIDsOf(opResp.Msg.GetChildren()); !mapsEqualBool(got, want) {
				t.Errorf("operator path child ids = %v, want %v", got, want)
			}

			scopedServer := connectapi.NewServer(nil)
			// ListChildren's Unavailable guard checks the lister before the
			// scope branch, so the childScoped path still needs one wired even
			// though it never reads from it.
			scopedServer.SetChildLister(&fakeLister{})
			scopedServer.SetChildScopeSource(func(context.Context) connectapi.ChildScope {
				return subtreeChildScope{all: all}
			})
			scopedResp, err := scopedServer.ListChildren(context.Background(), connect.NewRequest(tc.req))
			c.Require().NoError(err, "ListChildren (childScoped)")
			got := childIDsOf(scopedResp.Msg.GetChildren())
			c.True(mapsEqualBool(got, want), "childScoped path child ids = %v, want %v", got, want)
		})
	}
}

func mapsEqualBool(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// captureSlog swaps the default logger for a text handler over a buffer, runs
// run, and returns what was logged. Every captureSlog call replaces the
// default for the duration of one run — the tests using it never go parallel.
func captureSlog(t *testing.T, run func() error) (string, error) {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	err := run()
	return buf.String(), err
}

// TestKillChildExitedKeepsCodeAndReason pins Kill's child-exited
// connectapi.ControllerError: it reaches the client as FailedPrecondition — the code
// errCodeTable maps ErrChildExited to — with the precise reason riding the
// google.rpc.ErrorInfo detail and the daemon's authored message forwarded.
// The raw-Internal wrap this replaces told every client the daemon was broken
// and carried no branchable reason.
func TestKillChildExitedKeepsCodeAndReason(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{killErr: &connectapi.ControllerError{
		Code:    protocol.ErrChildExited,
		Message: "child has already exited",
	}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.Kill(context.Background(),
		connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_1"}))
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.Eq(protocol.ErrChildExited, rpcreason.Reason(err), "Reason")
	c.False(err == nil || !strings.Contains(err.Error(), "child has already exited"), "err = %v, want the daemon's authored message", err)
}

// TestSendChildExitedMapsToFailedPrecondition pins the same classification on
// Send: the controller's send validation (validateSendTarget) answers an
// exited or shutting-down child with an authored connectapi.ControllerError, which the
// raw-Internal wrap flattened into a code-less Internal.
func TestSendChildExitedMapsToFailedPrecondition(t *testing.T) {
	c := assert.NewCollecting(t)
	acc := &fakeAccepter{err: &connectapi.ControllerError{
		Code:    protocol.ErrChildExited,
		Message: "child has exited",
	}}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)

	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("x"),
	}))
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.Eq(protocol.ErrChildExited, rpcreason.Reason(err), "Reason")
}

// TestSpawnControllerErrorKeepsItsCode pins the Spawn side: a budget refusal
// arrives as InvalidArgument with its reason, not Internal.
func TestSpawnControllerErrorKeepsItsCode(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{spawnErr: &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: "max_depth below the floor",
	}})

	_, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq(protocol.ErrInvalidArgs, rpcreason.Reason(err), "Reason")
}

// TestSendSpawnKillUncodedErrorsAreRedactedAndLogged walks Send, Spawn and Kill's
// uncoded path: an error that is not a connectapi.ControllerError is infrastructure text
// this codebase did not author, so the caller sees only the fixed internal
// text with no reason and no raw fragment, while the cause is logged with the
// verb's context so it is not lost.
func TestSendSpawnKillUncodedErrorsAreRedactedAndLogged(t *testing.T) {
	raw := "pgx: failed to connect to host=db.internal user=rafiki database=rafiki: connection refused"
	cases := []struct {
		name      string
		wantInLog []string
		call      func(t *testing.T) error
	}{
		{
			name:      "send",
			wantInLog: []string{"connect: send failed", "c_1", raw},
			call: func(t *testing.T) error {
				s := connectapi.NewServer(nil)
				s.SetInbox(&fakeAccepter{err: errors.New(raw)})
				_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
					ChildId: "c_1", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("x"),
				}))
				return err
			},
		},
		{
			name:      "spawn",
			wantInLog: []string{"connect: spawn failed", "/work", raw},
			call: func(t *testing.T) error {
				s := connectapi.NewServer(nil)
				s.SetChildLifecycle(&fakeLifecycle{spawnErr: errors.New(raw)})
				_, err := s.Spawn(context.Background(),
					connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
				return err
			},
		},
		{
			name:      "kill",
			wantInLog: []string{"connect: kill failed", "c_1", raw},
			call: func(t *testing.T) error {
				s := connectapi.NewServer(nil)
				s.SetChildLifecycle(&fakeLifecycle{killErr: errors.New(raw)})
				_, err := s.Kill(context.Background(),
					connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_1"}))
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			logs, err := captureSlog(t, func() error { return tc.call(t) })
			c.Eq(connect.CodeInternal, connect.CodeOf(err), "code")
			c.False(err == nil || !strings.Contains(err.Error(), "internal error"), "err = %v, want the fixed internal text", err)
			c.NotStrContains(err.Error(), "db.internal", "err = %v, want the raw cause redacted", err)
			c.Eq("", rpcreason.Reason(err), "Reason")
			for _, want := range tc.wantInLog {
				c.StrContains(logs, want, "log")
			}
		})
	}
}
