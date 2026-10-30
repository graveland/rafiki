package msg_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/msg"
)

// TestRoleZeroIsInvalid pins the zero-value trap: Role(0) is a bug in the
// caller, not a default worth guessing at, so it must never read as a role.
func TestRoleZeroIsInvalid(t *testing.T) {
	c := assert.NewAborting(t)

	c.Eq("invalid", msg.Role(0).String(), "the zero Role must not name a role")

	_, err := msg.ParseRole("tool")
	c.Error(err, "an unmodelled role is not a role rafiki stores")
	c.EqualError(err, `msg: unknown role "tool"`)

	for _, tc := range []struct {
		text string
		want msg.Role
	}{
		{text: "user", want: msg.User},
		{text: "assistant", want: msg.Assistant},
		{text: "system", want: msg.System},
	} {
		got, err := msg.ParseRole(tc.text)
		c.NoError(err, "ParseRole(%q)", tc.text)
		c.Eq(tc.want, got, "ParseRole(%q)", tc.text)
		c.Eq(tc.text, got.String(), "%v must render back to its wire spelling", got)
	}
}

// TestBlockKindsCoverEveryBlockType proves BlockKinds is complete: every type
// in the package that answers block() is registered, and nothing else is.
func TestBlockKindsCoverEveryBlockType(t *testing.T) {
	c := assert.NewAborting(t)

	entries, err := os.ReadDir(".")
	c.NoError(err, "read the package directory")

	var declared []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, 0)
		c.NoError(err, "parse %s", name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "block" {
				continue
			}
			declared = append(declared, blockReceiver(c, name, fn))
		}
	}

	var registered []string
	for _, b := range msg.BlockKinds() {
		registered = append(registered, reflect.TypeOf(b).Name())
	}

	sort.Strings(declared)
	sort.Strings(registered)
	c.EqDeep(declared, registered, "BlockKinds must name every block type exactly once")
}

// blockReceiver returns the type name of a method's value receiver.
func blockReceiver(c *assert.C, file string, fn *ast.FuncDecl) string {
	field := fn.Recv.List[0]
	typ := field.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	ident, ok := typ.(*ast.Ident)
	c.True(ok, "%s: block() on an unexpected receiver in %s", fn.Name.Name, file)
	return ident.Name
}

// TestToolUseIDsMatchesStore pins the linkage storage records: the tool_use and
// tool_result ids in block order, with empty ones skipped.
func TestToolUseIDsMatchesStore(t *testing.T) {
	c := assert.NewAborting(t)

	m := msg.Message{
		Role: msg.User,
		Content: []msg.Block{
			msg.Text{Text: "ignored"},
			msg.ToolUse{ID: "toolu_1", Name: "read_file", Input: []byte(`{"path":"a"}`)},
			msg.ToolUse{ID: "", Name: "nameless"},
			msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: "ok"}}},
			msg.ToolUse{ID: "toolu_2", Name: "bash"},
			msg.ToolResult{ToolUseID: "", Content: []msg.Block{msg.Text{Text: "no id"}}},
			msg.ToolResult{ToolUseID: "toolu_2"},
		},
	}

	c.EqDeep([]string{"toolu_1", "toolu_1", "toolu_2", "toolu_2"}, msg.ToolUseIDs(m))

	c.EqDeep([]string(nil), msg.ToolUseIDs(msg.Message{Role: msg.Assistant}), "a message with no tools has no ids")
}

// TestOpaqueToolBlocksYieldIDs pins that an id survives a block the codec kept
// verbatim: a tool_use carrying a caller, or a tool_result carrying anything the
// typed form cannot reproduce, is still a tool call, so it must yield exactly
// what the typed path yields for the same ids.
func TestOpaqueToolBlocksYieldIDs(t *testing.T) {
	c := assert.NewAborting(t)

	opaque := func(raw string) msg.Block {
		return msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(raw)}
	}

	want := []string{"toolu_1", "toolu_1", "toolu_2", "toolu_2"}
	typed := msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.ToolUse{ID: "toolu_1", Name: "read_file"},
		msg.ToolResult{ToolUseID: "toolu_1"},
		msg.ToolUse{ID: "toolu_2", Name: "bash"},
		msg.ToolResult{ToolUseID: "toolu_2"},
	}}
	verbatim := msg.Message{Role: msg.Assistant, Content: []msg.Block{
		opaque(`{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a"},"caller":{"type":"direct"}}`),
		opaque(`{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok","citations":[]}]}`),
		opaque(`{"type":"tool_use","id":"toolu_2","name":"bash","input":{},"caller":{"type":"direct"}}`),
		opaque(`{"type":"tool_result","tool_use_id":"toolu_2","content":"done"}`),
	}}

	c.EqDeep(want, msg.ToolUseIDs(typed), "the typed path")
	c.EqDeep(want, msg.ToolUseIDs(verbatim), "an Opaque tool block must still yield its id, in block order")

	c.EqDeep([]string(nil), msg.ToolUseIDs(msg.Message{Role: msg.Assistant, Content: []msg.Block{
		opaque(`{"type":"redacted_thinking","data":"opaque"}`),
		opaque(`{"type":"tool_use","id":"","name":"nameless","input":{}}`),
		opaque(`{"type":"tool_result","tool_use_id":""}`),
		opaque(`5`),
	}}), "only a non-empty id on a tool block counts")

	// A typed block carrying residue is an ordinary typed block: its id is the
	// one the core holds, not one read out of Ext.
	withExt := msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.ToolUse{ID: "toolu_1", Name: "read_file", Ext: msg.Ext{
			Dialect: msg.DialectAnthropic,
			Raw:     `{"caller":{"type":"direct"}}`,
		}},
		msg.ToolResult{ToolUseID: "toolu_1", Ext: msg.Ext{
			Dialect: msg.DialectAnthropic,
			Raw:     `{"caller":{"type":"direct"}}`,
		}},
	}}
	c.EqDeep([]string{"toolu_1", "toolu_1"}, msg.ToolUseIDs(withExt), "a typed block with Ext yields its id")
}

// TestOpaqueType pins the classifier a boundary reader needs: an Opaque block
// reports the wire type it declares, and anything that is not a typed JSON
// object reports none.
func TestOpaqueType(t *testing.T) {
	c := assert.NewAborting(t)

	opaque := func(raw string) msg.Opaque {
		return msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(raw)}
	}

	c.Eq("tool_result", msg.OpaqueType(opaque(`{"type":"tool_result","tool_use_id":"t1","caller":{"type":"direct"}}`)))
	c.Eq("tool_use", msg.OpaqueType(opaque(`{"type":"tool_use","id":"t1","name":"read"}`)))
	c.Eq("redacted_thinking", msg.OpaqueType(opaque(`{"type":"redacted_thinking","data":"x"}`)))
	c.Eq("", msg.OpaqueType(opaque(`{"tool_use_id":"t1"}`)), "a block declaring no type")
	c.Eq("", msg.OpaqueType(opaque(`5`)), "raw JSON that is not an object")
	c.Eq("", msg.OpaqueType(opaque(``)), "empty raw")
}
