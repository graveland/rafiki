package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

// fakeLSPNavClient provides canned navigation responses.
type fakeLSPNavClient struct {
	fakeLSPClient
	defLocs  []LSPLocation
	refLocs  []LSPLocation
	docSyms  []LSPLocation
	wsSyms   []LSPLocation
	chItems  []LSPCallHierarchyItem
	inCalls  []LSPCallHierarchyItem
	outCalls []LSPCallHierarchyItem
}

func (f *fakeLSPNavClient) Definition(_ context.Context, _ string, _, _ int) ([]LSPLocation, error) {
	return f.defLocs, nil
}
func (f *fakeLSPNavClient) References(_ context.Context, _ string, _, _ int) ([]LSPLocation, error) {
	return f.refLocs, nil
}
func (f *fakeLSPNavClient) DocumentSymbols(_ context.Context, _ string) ([]LSPLocation, error) {
	return f.docSyms, nil
}
func (f *fakeLSPNavClient) WorkspaceSymbols(_ context.Context, _ string) ([]LSPLocation, error) {
	return f.wsSyms, nil
}
func (f *fakeLSPNavClient) PrepareCallHierarchy(_ context.Context, _ string, _, _ int) ([]LSPCallHierarchyItem, error) {
	return f.chItems, nil
}
func (f *fakeLSPNavClient) IncomingCalls(_ context.Context, _ LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error) {
	return f.inCalls, nil
}
func (f *fakeLSPNavClient) OutgoingCalls(_ context.Context, _ LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error) {
	return f.outCalls, nil
}
func (f *fakeLSPNavClient) Rename(context.Context, string, int, int, string) ([]string, error) {
	return nil, nil
}
func (f *fakeLSPNavClient) Restart(context.Context, string) error { return nil }

func TestLSPDefinition_Execute(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeLSPNavClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		defLocs: []LSPLocation{
			{URI: "file:///tmp/lib.go", Line: 41, Col: 5},
		},
	}
	tool, err := LSPDefinitionBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspPosInput{Path: "main.go", Line: 10, Col: 3})))
	c.Require().NoError(err, "Execute")
	c.False(result.Text == "" || result.Text == "Definition: none", "expected non-empty definition result")
}

func TestLSPDefinition_Materialize_Declines(t *testing.T) {
	c := assert.NewCollecting(t)
	bp := LSPDefinitionBlueprint{}
	tool, err := bp.Materialize(ToolOpts{LSP: nil, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	c.Nil(tool, "expected nil tool when LSP is nil")
}

func TestLSPReferences_Execute(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeLSPNavClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		refLocs: []LSPLocation{
			{URI: "file:///tmp/a.go", Line: 5, Col: 0},
			{URI: "file:///tmp/b.go", Line: 12, Col: 0},
		},
	}
	tool, err := LSPReferencesBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspPosInput{Path: "main.go", Line: 10, Col: 3})))
	c.Require().NoError(err, "Execute")
	// Assert on the rendered output. This test previously only logged it,
	// so it could not fail except by panicking.
	for _, want := range []string{"/tmp/a.go:6:1", "/tmp/b.go:13:1"} {
		c.StrContains(result.Text, want, "references output missing")
	}
}

func TestLSPCallHierarchy_Incoming(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeLSPNavClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		chItems: []LSPCallHierarchyItem{
			{Name: "DoThing", URI: "file:///tmp/main.go", Line: 9, Col: 5},
		},
		inCalls: []LSPCallHierarchyItem{
			{Name: "main", URI: "file:///tmp/main.go", Line: 19, Col: 1},
		},
	}
	tool, err := LSPCallHierarchyBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspCHInput{
		Path: "main.go", Line: 10, Col: 6, Direction: "incoming",
	})))
	c.Require().NoError(err, "Execute")
	// The caller's name and location must both survive into the output.
	for _, want := range []string{"main", "/tmp/main.go:20:2"} {
		c.StrContains(result.Text, want, "incoming-calls output missing")
	}
}

func TestLSPCallHierarchy_InvalidDirection(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeLSPNavClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		chItems: []LSPCallHierarchyItem{
			{Name: "DoThing", URI: "file:///tmp/main.go", Line: 9, Col: 5},
		},
	}
	tool, err := LSPCallHierarchyBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	_, err = tool.Execute(context.Background(), ToolInput(mustJSON(lspCHInput{
		Path: "main.go", Line: 10, Col: 6, Direction: "sideways",
	})))
	c.Error(err, "expected error for invalid direction")
}

// mustJSON is shared across test files.
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
