package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeExecutorInstaller records what the tools handed it and answers with what
// the test seeded.
type fakeExecutorInstaller struct {
	res   protocol.ExecutorInstallResult
	err   error
	unErr error

	installs   []protocol.ExecutorInstallRequest
	uninstalls []string
}

func (f *fakeExecutorInstaller) Install(
	_ context.Context, req protocol.ExecutorInstallRequest,
) (protocol.ExecutorInstallResult, error) {
	f.installs = append(f.installs, req)
	return f.res, f.err
}

func (f *fakeExecutorInstaller) Uninstall(_ context.Context, executor string) error {
	f.uninstalls = append(f.uninstalls, executor)
	return f.unErr
}

// Neither verb exists without a binding: an agent that cannot install must not
// see a tool whose every call can only answer "not configured" (the
// nil-means-decline rule).
func TestExecutorInstallToolsDeclineWithoutBinding(t *testing.T) {
	c := assert.NewAborting(t)
	for _, bp := range []Materializer{ExecutorInstallBlueprint{}, ExecutorUninstallBlueprint{}} {
		tool, err := bp.Materialize(ToolOpts{})
		c.NoError(err, "Materialize(%T) with no installer", bp)
		c.Nil(tool, "Materialize(%T) must decline with no ExecutorInstaller bound", bp)
	}
}

// The install tool maps its arguments onto the request verbatim, hands it to
// the BOUND installer (no identity in the call), and renders the row the daemon
// answered with.
func TestExecutorInstallToolRendersRow(t *testing.T) {
	c := assert.NewAborting(t)
	inst := &fakeExecutorInstaller{res: protocol.ExecutorInstallResult{
		ID:        "exec-1",
		Name:      "ublk-s2",
		Via:       "laptop",
		Target:    "brent@ublk-s2",
		Connected: true,
	}}
	tool, err := ExecutorInstallBlueprint{}.Materialize(ToolOpts{ExecutorInstaller: inst})
	c.Require().NoError(err, "Materialize executor_install")
	c.Require().NotNil(tool, "the bound installer must materialize the tool")

	res, err := tool.Execute(context.Background(), ToolInput(
		`{"target":"brent@ublk-s2","via":"laptop","name":"ublk-s2","root":"/src",`+
			`"labels":{"env":"prod"},"admits":"owner=brent","mode":"direct"}`))
	c.NoError(err, "executor_install")
	c.Eq("installed ublk-s2 (exec-1) on brent@ublk-s2 via laptop; connected", res.Text, "success text")

	c.Require().Len(inst.installs, 1, "the installer saw exactly one request")
	c.EqDeep(protocol.ExecutorInstallRequest{
		Target: "brent@ublk-s2",
		Via:    "laptop",
		Name:   "ublk-s2",
		Root:   "/src",
		Labels: map[string]string{"env": "prod"},
		Admits: "owner=brent",
		Mode:   "direct",
	}, inst.installs[0], "the request handed to the installer")
}

// TestExecutorInstallToolSchemaListsModeEnum pins the transport vocabulary the
// model is OFFERED: the three modes, the exact sentence that explains them, and
// mode's absence from `required`. The request comparison above pins that the
// value the model picks reaches the daemon; this pins that it is offered at
// all, which no request-shape assertion can see.
func TestExecutorInstallToolSchemaListsModeEnum(t *testing.T) {
	c := assert.NewAborting(t)
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Enum        []string `json:"enum"`
			Description string   `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	c.Require().NoError(json.Unmarshal(ExecutorInstallBlueprint{}.InputSchema().JSON(), &schema),
		"decode the executor_install schema")

	mode, ok := schema.Properties["mode"]
	c.Require().True(ok, "executor_install must offer mode; properties = %v", schema.Properties)
	c.Eq("string", mode.Type, "mode type")
	c.EqDeep([]string{"auto", "direct", "tunnel"}, mode.Enum,
		"mode enum: a schema missing one of the three would hide a transport from the model")
	c.Eq(executorInstallModeDescription, mode.Description, "mode description is the model-facing contract")
	c.EqDeep([]string{"target"}, schema.Required, "mode is optional: only target is required")
}

// An install that returns without a live connection says so: the connection is
// the thing the caller asked for, and silence would read as success.
func TestExecutorInstallToolReportsNotConnected(t *testing.T) {
	c := assert.NewAborting(t)
	inst := &fakeExecutorInstaller{res: protocol.ExecutorInstallResult{ID: "exec-1", Name: "box"}}
	tool, err := ExecutorInstallBlueprint{}.Materialize(ToolOpts{ExecutorInstaller: inst})
	c.Require().NoError(err, "Materialize executor_install")
	res, err := tool.Execute(context.Background(), ToolInput(`{"target":"box"}`))
	c.NoError(err, "executor_install")
	c.Eq("installed box (exec-1); not connected", res.Text, "success text")
}

// A missing target is refused at the TOOL, before the installer runs: the
// daemon's own refusal would arrive after a round trip and name a field the
// model cannot see.
func TestExecutorInstallToolRequiresTarget(t *testing.T) {
	c := assert.NewAborting(t)
	inst := &fakeExecutorInstaller{}
	tool, err := ExecutorInstallBlueprint{}.Materialize(ToolOpts{ExecutorInstaller: inst})
	c.Require().NoError(err, "Materialize executor_install")

	_, err = tool.Execute(context.Background(), ToolInput(`{"via":"laptop"}`))
	c.Require().Error(err, "an omitted target must be refused")
	c.StrContains(err.Error(), "target is required", "the refusal must name the field, got: %v", err)
	c.Empty(inst.installs, "a refused call reaches no installer")
}

// The installer's error travels with ssh's own words in it: the whole point of
// the verb is that "Host key verification failed" reaches the agent, so the
// tool must not paraphrase it away.
func TestExecutorInstallToolKeepsTheInstallerError(t *testing.T) {
	c := assert.NewAborting(t)
	inst := &fakeExecutorInstaller{err: errors.New("probe ublk-s2 failed: Host key verification failed.")}
	tool, err := ExecutorInstallBlueprint{}.Materialize(ToolOpts{ExecutorInstaller: inst})
	c.Require().NoError(err, "Materialize executor_install")
	_, err = tool.Execute(context.Background(), ToolInput(`{"target":"ublk-s2"}`))
	c.Require().Error(err, "an installer failure must surface")
	c.StrContains(err.Error(), "Host key verification failed", "ssh's message must survive, got: %v", err)
}

// executor_uninstall hands its one argument through and renders what it did.
func TestExecutorInstallUninstallToolCallsTheInstaller(t *testing.T) {
	c := assert.NewAborting(t)
	inst := &fakeExecutorInstaller{}
	tool, err := ExecutorUninstallBlueprint{}.Materialize(ToolOpts{ExecutorInstaller: inst})
	c.Require().NoError(err, "Materialize executor_uninstall")

	res, err := tool.Execute(context.Background(), ToolInput(`{"executor":"ublk-s2"}`))
	c.NoError(err, "executor_uninstall")
	c.Eq("uninstalled ublk-s2", res.Text, "success text")
	c.EqDeep([]string{"ublk-s2"}, inst.uninstalls, "the ref handed to the installer")

	_, err = tool.Execute(context.Background(), ToolInput(`{}`))
	c.Require().Error(err, "an omitted executor must be refused")
	c.StrContains(err.Error(), "executor is required", "the refusal must name the field, got: %v", err)
	c.Len(inst.uninstalls, 1, "a refused call reaches no installer")

	// The daemon's refusal travels unchanged: an uninstall of a row this
	// lineage did not install says so, and the agent needs those words.
	inst.unErr = errors.New("executor ublk-s2 belongs to another user")
	_, err = tool.Execute(context.Background(), ToolInput(`{"executor":"ublk-s2"}`))
	c.Require().Error(err, "a refused uninstall must surface")
	c.StrContains(err.Error(), "belongs to another user", "the daemon's words must survive, got: %v", err)
}

// Both verbs run on the daemon: their effect is an executor row and a machine
// reached through one of the caller's executors, never a file on the caller's
// own host -- which is what lets a claude child, whose own tools stop at its
// host, use them.
func TestExecutorInstallIsDaemonTier(t *testing.T) {
	c := assert.NewAborting(t)
	for _, name := range []string{"executor_install", "executor_uninstall"} {
		tier, ok := TierOf(name)
		c.True(ok, "%s has no tier — a missing entry is a confinement hole", name)
		c.Eq(TierDaemon, tier, "tier of %s", name)
		c.NotContains(ExecutorLocalTools(), name, "%s must never reach an executor's own registry", name)
	}
}
