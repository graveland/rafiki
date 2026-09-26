// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
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
// 1-14: cwd through script, including the three budget fields). It is kept
// as its own copy, not an import of the unexported production set, because
// this file is package connectapi_test -- and duplicating it here is the
// point: TestSpawnRequestFieldsAreClassified fails the moment a field number
// exists that this list and operatorOnlyFieldMin/Max don't between them
// cover, forcing a conscious classification decision on both sides.
var childAllowedFieldNumbers = map[protoreflect.FieldNumber]bool{
	1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true,
	8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true,
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
			f := &fakeLifecycle{}
			s := childScopedServer(f)
			req := &rafikiv1.SpawnRequest{Cwd: "/tmp"}
			setOperatorOnlyField(t, req, fd)

			_, err := s.Spawn(context.Background(), connect.NewRequest(req))
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("Spawn with %s set (child caller) err = %v, want PermissionDenied", fd.Name(), err)
			}
			if !strings.Contains(err.Error(), string(fd.Name())) || !strings.Contains(err.Error(), "operator-only") {
				t.Errorf("error %q does not name %s as operator-only", err.Error(), fd.Name())
			}
			if f.got.Cwd != "" {
				t.Errorf("Spawn reached the lifecycle despite the refusal: %+v", f.got)
			}
		})
	}
	if exercised == 0 {
		t.Fatal("exercised zero operator-only fields -- the range or the descriptor moved, and this test now proves nothing")
	}
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
	fields := (&rafikiv1.SpawnRequest{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() == 0 {
		t.Fatal("SpawnRequest descriptor reports zero fields -- nothing was classified")
	}
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
	if classified != fields.Len() {
		t.Fatalf("classified %d of %d SpawnRequest fields", classified, fields.Len())
	}
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
	if _, err := s.Spawn(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatalf("Spawn (operator, every operator-only field set) err = %v, want nil", err)
	}
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
// ctrl_list applies. Filters only ever narrow a child caller's subtree, never
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
			want := make(map[string]bool, len(tc.want))
			for _, id := range tc.want {
				want[id] = true
			}

			opServer := connectapi.NewServer(nil)
			opServer.SetChildLister(&fakeLister{all: all})
			opResp, err := opServer.ListChildren(context.Background(), connect.NewRequest(tc.req))
			if err != nil {
				t.Fatalf("ListChildren (operator): %v", err)
			}
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
			if err != nil {
				t.Fatalf("ListChildren (childScoped): %v", err)
			}
			if got := childIDsOf(scopedResp.Msg.GetChildren()); !mapsEqualBool(got, want) {
				t.Errorf("childScoped path child ids = %v, want %v", got, want)
			}
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
