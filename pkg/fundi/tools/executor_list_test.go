package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// fakeExecutorLister answers with what the test seeded and records that it was
// asked — the listing takes no argument, so the call itself is the whole
// contract.
type fakeExecutorLister struct {
	rows  []protocol.ExecutorStatus
	err   error
	calls int
}

func (f *fakeExecutorLister) List(context.Context) ([]protocol.ExecutorStatus, error) {
	f.calls++
	return f.rows, f.err
}

// No binding, no tool: an agent whose daemon has no executor store must not see
// a verb whose every call can only answer "not configured" (the
// nil-means-decline rule the sandbox and recall tools follow).
func TestExecutorListDeclinesWithoutBinding(t *testing.T) {
	c := assert.NewAborting(t)
	tool, err := ExecutorListBlueprint{}.Materialize(ToolOpts{})
	c.NoError(err, "Materialize with no lister")
	c.Nil(tool, "executor_list must decline with no ExecutorLister bound")
}

// The listing takes no arguments at all. A parameter here would be an identity
// or an executor a model could name, and the binding exists so it cannot.
func TestExecutorListTakesNoArguments(t *testing.T) {
	c := assert.NewAborting(t)
	var schema map[string]any
	c.Require().NoError(
		json.Unmarshal(ExecutorListBlueprint{}.InputSchema().JSON(), &schema), "decode schema")
	c.Eq("object", schema["type"], "executor_list type")
	_, hasProps := schema["properties"]
	c.False(hasProps, "executor_list must take no arguments, got properties %v", schema["properties"])
}

// One block per executor, with the health and provenance facts a caller acts on
// and nothing invented for the fields that are unset.
func TestExecutorListRendersStatus(t *testing.T) {
	c := assert.NewAborting(t)
	connected := time.Date(2026, 11, 16, 18, 54, 34, 0, time.UTC)
	seen := time.Date(2026, 11, 16, 19, 9, 34, 0, time.UTC)
	lister := &fakeExecutorLister{rows: []protocol.ExecutorStatus{
		{
			ID:            "02cd8c2b7de7",
			Name:          "greyshift",
			Connected:     true,
			Enabled:       true,
			ConnectedAt:   &connected,
			LastSeen:      &seen,
			Isolation:     "none",
			WorkspaceMode: "pinned",
			Roots:         []string{"/Users/brent"},
			LaunchKinds:   []string{"fundi", "claude"},
			Labels:        map[string]string{"owner": "brent", "machine": "greyshift"},
			Annotations:   map[string]string{"built": "2026-11-15"},
		},
		{
			// A nameless, never-seen install whose launcher is still recorded:
			// the id is what a caller acts on, and "never seen" is the honest
			// answer rather than an omitted status line.
			ID:              "01a06ebe12b6",
			Enabled:         true,
			InstalledVia:    "greyshift",
			InstalledTarget: "brent@ublk-s2",
		},
	}}
	tool, err := ExecutorListBlueprint{}.Materialize(ToolOpts{Executors: lister})
	c.Require().NoError(err, "Materialize executor_list")
	c.Require().NotNil(tool, "a bound lister must materialize the tool")

	res, err := tool.Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err, "executor_list")
	want := "greyshift (02cd8c2b7de7)\n" +
		"  status: live, connected since 2026-11-16T18:54:34Z, last seen 2026-11-16T19:09:34Z\n" +
		"  workspace pinned, isolation none, launches fundi, claude\n" +
		"  roots: /Users/brent\n" +
		"  labels: machine=greyshift, owner=brent\n" +
		"  annotations: built=2026-11-15\n" +
		"\n" +
		"- (01a06ebe12b6)\n" +
		"  status: down, never seen\n" +
		"  workspace pinned\n" +
		"  installed: on brent@ublk-s2 via greyshift\n"
	c.Eq(want, res.Text, "rendered listing")
	c.Eq(1, lister.calls, "the tool asked the binding once per call")
}

// A disabled executor is not merely down: it will not connect until it is
// re-enabled, and a reader told only "down" would keep waiting for it.
func TestExecutorListSpellsOutDisabled(t *testing.T) {
	c := assert.NewAborting(t)
	lister := &fakeExecutorLister{rows: []protocol.ExecutorStatus{
		{ID: "e1", Name: "old-box", Enabled: false, WorkspaceMode: "pinned"},
	}}
	tool, err := ExecutorListBlueprint{}.Materialize(ToolOpts{Executors: lister})
	c.Require().NoError(err, "Materialize")
	res, err := tool.Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err, "executor_list")
	c.True(strings.Contains(res.Text, "status: disabled, never seen"), "a disabled executor must say so: %q", res.Text)
}

// Nothing visible is a successful answer, not an error and not an empty string
// a model has to interpret.
func TestExecutorListSaysSoWhenNothingIsVisible(t *testing.T) {
	c := assert.NewAborting(t)
	tool, err := ExecutorListBlueprint{}.Materialize(
		ToolOpts{Executors: &fakeExecutorLister{}})
	c.Require().NoError(err, "Materialize")
	res, err := tool.Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err, "executor_list")
	c.Eq("no executors", res.Text, "empty listing text")
}

// A store read that fails is an ERROR, never its text dressed up as a
// successful listing: a caller that saw "permission denied" as the whole answer
// would read it as the state of its machines.
func TestExecutorListPropagatesTheStoreError(t *testing.T) {
	c := assert.NewAborting(t)
	boom := errors.New("executors: connection refused")
	tool, err := ExecutorListBlueprint{}.Materialize(
		ToolOpts{Executors: &fakeExecutorLister{err: boom}})
	c.Require().NoError(err, "Materialize")
	_, err = tool.Execute(context.Background(), ToolInput(`{}`))
	c.Require().NotNil(err, "the store's error must surface")
	c.True(errors.Is(err, boom), "want the store's error wrapped, got %v", err)
	c.True(strings.Contains(err.Error(), "executor_list:"), "an error names the tool that produced it: %v", err)
}

// TestExecutorListToolShowsModeAndLastLaunch pins the two facts an operator
// acts on after an install: which transport the recipe was created with, and
// how the daemon's last launch of it went. Both belong to an INSTALL, so an
// enrolled row renders neither; and because the outcome is in-memory, an
// install the daemon has not launched since it started renders the mode alone
// rather than an empty "last launch:" line.
func TestExecutorListToolShowsModeAndLastLaunch(t *testing.T) {
	c := assert.NewAborting(t)
	lister := &fakeExecutorLister{rows: []protocol.ExecutorStatus{
		{
			ID: "e-installed", Name: "ublk-s2", Enabled: true, WorkspaceMode: "pinned",
			InstalledVia: "laptop", InstalledTarget: "brent@ublk-s2",
			InstallMode: "direct", LastLaunch: "ok: direct launch started in 1.2s",
		},
		{
			ID: "e-quiet", Name: "ublk-s3", Enabled: true, WorkspaceMode: "pinned",
			InstalledVia: "laptop", InstalledTarget: "brent@ublk-s3",
			InstallMode: "tunnel",
		},
		{
			// Enrolled, never installed: no recipe, so no transport and no launch.
			ID: "e-enrolled", Name: "rack", Enabled: true, Connected: true, WorkspaceMode: "pinned",
		},
	}}
	tool, err := ExecutorListBlueprint{}.Materialize(ToolOpts{Executors: lister})
	c.Require().NoError(err, "Materialize executor_list")

	res, err := tool.Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err, "executor_list")
	want := "ublk-s2 (e-installed)\n" +
		"  status: down, never seen\n" +
		"  workspace pinned\n" +
		"  installed: on brent@ublk-s2 via laptop\n" +
		"  mode: direct\n" +
		"  last launch: ok: direct launch started in 1.2s\n" +
		"\n" +
		"ublk-s3 (e-quiet)\n" +
		"  status: down, never seen\n" +
		"  workspace pinned\n" +
		"  installed: on brent@ublk-s3 via laptop\n" +
		"  mode: tunnel\n" +
		"\n" +
		"rack (e-enrolled)\n" +
		"  status: live\n" +
		"  workspace pinned\n"
	c.Eq(want, res.Text, "rendered listing")

	// The enrolled row's block stops after its capabilities: a "mode:" line on a
	// machine nobody installed would read as a transport it does not have.
	enrolled := res.Text[strings.Index(res.Text, "rack (e-enrolled)"):]
	c.NotStrContains(enrolled, "mode:", "an enrolled executor has no install transport")
	c.NotStrContains(enrolled, "last launch:", "an enrolled executor has no launch to report")
}
