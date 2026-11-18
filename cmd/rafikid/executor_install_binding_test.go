// SPDX-License-Identifier: Apache-2.0

package main

// The two FACES of executor_install/executor_uninstall.
//
// A per-child credential reaches this capability twice: as an MCP tool (a
// claude child's own tool set) and as a Connect RPC (a fundi child's, and any
// scripted client's). CLAUDE.md's rule is that the two change together, and the
// only way to keep it is to drive both through the SAME install orchestrator
// and compare what each answers. These tests do exactly that: one fixture, one
// Controller, two faces.
//
// TestInstallFacesAdmitIdentically is the table, and
// TestInstallFacesDescriptionIsIdenticalForEveryKind pins that the faces answer
// every caller kind the same way: a claude child's native tools run on its
// launch host, but executor-routed tools reach the installed executor.

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// installFaceOutcome is one face's answer, normalized so the two can be
// compared: whether the call was refused, and the text a caller reads. The MCP
// face prefixes a tool error with the tool name; the comparison looks for the
// daemon's own sentence inside, which is the part both faces must share.
type installFaceOutcome struct {
	refused bool
	code    connect.Code
	text    string
}

// seedInstallChild inserts the stored row a child credential needs. Wave 3's
// rule refuses a credential naming no row BEFORE it computes any reachable set,
// so every child caller here must exist. ownerLabel is the attested `owner`
// label the install caller's display name comes from.
func seedInstallChild(t *testing.T, ctrl *Controller, childID, parentID, ownerLabel, kind string) {
	t.Helper()
	labels := map[string]string{"owner": ownerLabel, childstore.LabelRoot: childID}
	if parentID != "" {
		labels[childstore.LabelParent] = parentID
		labels[childstore.LabelRoot] = parentID
	}
	ctrl.st.Insert(&childstore.Session{
		ChildID:     childID,
		Kind:        kind,
		Status:      protocol.StatusIdle,
		StartedAt:   time.Now(),
		OwnerUserID: installTestOwner,
		Labels:      labels,
	})
}

// childIdentity is the connection identity a per-child credential yields: the
// owner's user id, the child's own id, and NO username — a child-provenance
// identity never carries one, which is what makes the `owner` label the only
// honest source of a display name.
func childIdentity(ownerID, childID string) *server.Identity {
	return &server.Identity{UserID: ownerID, ChildID: childID, Via: server.ProvenanceChildToken}
}

// mcpFaceInstall runs executor_install on the MCP face for one child credential.
func mcpFaceInstall(
	t *testing.T, ctrl *Controller, ownerID, childID string, req *rafikiv1.InstallExecutorRequest,
) installFaceOutcome {
	t.Helper()
	face, _ := mcpFaceFixture(t)
	face.SetController(ctrl)
	cs := mcpConnect(t, face.getServer(mcpChildRequest(ownerID, childID, false)))
	args := map[string]any{"target": req.GetTarget()}
	if req.GetVia() != "" {
		args["via"] = req.GetVia()
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "executor_install", Arguments: args,
	})
	assert.NewAborting(t).NoError(err, "executor_install over MCP is a tool result, not a transport error")
	return installFaceOutcome{refused: res.IsError, text: mcpToolResultText(t, res)}
}

// mcpFaceUninstall runs executor_uninstall on the MCP face for one child
// credential.
func mcpFaceUninstall(t *testing.T, ctrl *Controller, ownerID, childID, ref string) installFaceOutcome {
	t.Helper()
	face, _ := mcpFaceFixture(t)
	face.SetController(ctrl)
	cs := mcpConnect(t, face.getServer(mcpChildRequest(ownerID, childID, false)))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "executor_uninstall", Arguments: map[string]any{"executor": ref},
	})
	assert.NewAborting(t).NoError(err, "executor_uninstall over MCP is a tool result, not a transport error")
	return installFaceOutcome{refused: res.IsError, text: mcpToolResultText(t, res)}
}

// connectFaceInstall runs the Connect InstallExecutor HANDLER — policy mapping
// and error mapping included — against the production adapter over the same
// Controller.
func connectFaceInstall(
	t *testing.T, ctrl *Controller, ownerID, childID string, req *rafikiv1.InstallExecutorRequest,
) installFaceOutcome {
	t.Helper()
	c := assert.NewAborting(t)
	srv := connectapi.NewServer(nil)
	srv.SetExecutorInstaller(connectExecutorInstaller{c: ctrl})
	ctx := server.WithIdentity(context.Background(), childIdentity(ownerID, childID))
	resp, err := srv.InstallExecutor(ctx, connect.NewRequest(req))
	if err != nil {
		return connectOutcome(c, err)
	}
	return installFaceOutcome{text: resp.Msg.GetExecutor().GetId()}
}

// connectFaceUninstall is connectFaceInstall's uninstall half.
func connectFaceUninstall(t *testing.T, ctrl *Controller, ownerID, childID, ref string) installFaceOutcome {
	t.Helper()
	c := assert.NewAborting(t)
	srv := connectapi.NewServer(nil)
	srv.SetExecutorInstaller(connectExecutorInstaller{c: ctrl})
	ctx := server.WithIdentity(context.Background(), childIdentity(ownerID, childID))
	_, err := srv.UninstallExecutor(ctx, connect.NewRequest(&rafikiv1.UninstallExecutorRequest{Executor: ref}))
	if err != nil {
		return connectOutcome(c, err)
	}
	return installFaceOutcome{}
}

// connectOutcome normalizes a Connect error: the code as the wire carries it and
// the message WITHOUT the "code: " prefix connect.Error.Error() adds, which the
// MCP face has no equivalent of.
func connectOutcome(c *assert.C, err error) installFaceOutcome {
	code := connect.CodeOf(err)
	text := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Message() != "" {
		text = ce.Message()
	}
	c.False(code == connect.CodeUnknown, "the handler answered an unclassified error: %v", err)
	return installFaceOutcome{refused: true, code: code, text: text}
}

// TestInstallFacesAdmitIdentically is the change-both-or-neither table: the same
// per-child credential, the same machine, the same Controller — and both faces
// must answer the same way. A widening or narrowing on one face alone shows up
// here as a mismatch, which is the failure mode nothing else would catch.
func TestInstallFacesAdmitIdentically(t *testing.T) {
	const (
		child = "c_kid"
		// A native executor the child may NOT reach: its admits selector
		// excludes the child's owner label, exactly as a machine enrolled for
		// somebody else would.
		unreachableVia = "faraway"
	)

	// establishedFixture is a fresh Controller with c_kid stored and one native
	// launcher it can reach, plus one it cannot.
	establishedFixture := func(t *testing.T) *installFixture {
		t.Helper()
		f := newInstallFixture(t)
		far := installLauncherLive("grey-2", unreachableVia, "work", "pinned")
		far.Executor.Admits = "owner=someone-else"
		f.pool.live = append(f.pool.live, far)
		seedInstallChild(t, f.ctrl, child, "", installTestOwnerName, protocol.KindFundi)
		// The remote process comes up and connects, so an admitted install
		// finishes instead of timing out.
		f.connectAs = "exec-1"
		return f
	}

	cases := []struct {
		name    string
		refused bool
		// want is the daemon's own sentence, which both faces must carry.
		want string
		mcp  func(t *testing.T, f *installFixture) installFaceOutcome
		conn func(t *testing.T, f *installFixture) installFaceOutcome
	}{
		{
			name: "a launcher in the caller's own lineage is admitted",
			mcp: func(t *testing.T, f *installFixture) installFaceOutcome {
				return mcpFaceInstall(t, f.ctrl, installTestOwner, child,
					installReq(installTestLauncherN))
			},
			conn: func(t *testing.T, f *installFixture) installFaceOutcome {
				return connectFaceInstall(t, f.ctrl, installTestOwner, child,
					installReq(installTestLauncherN))
			},
		},
		{
			name:    "a launcher the caller cannot reach is refused",
			refused: true,
			want:    "executor " + unreachableVia + " is not reachable by this agent",
			mcp: func(t *testing.T, f *installFixture) installFaceOutcome {
				return mcpFaceInstall(t, f.ctrl, installTestOwner, child, installReq(unreachableVia))
			},
			conn: func(t *testing.T, f *installFixture) installFaceOutcome {
				return connectFaceInstall(t, f.ctrl, installTestOwner, child, installReq(unreachableVia))
			},
		},
		{
			name:    "uninstalling a foreign lineage's executor is refused",
			refused: true,
			want:    "executor exec-1 belongs to another user",
			mcp: func(t *testing.T, f *installFixture) installFaceOutcome {
				return mcpFaceUninstall(t, f.ctrl, installTestOwner, child, "ublk-s2")
			},
			conn: func(t *testing.T, f *installFixture) installFaceOutcome {
				return connectFaceUninstall(t, f.ctrl, installTestOwner, child, "ublk-s2")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)

			// A fresh Controller per face: the admitted case installs, and an
			// install mutates the row set under it.
			mcpFixture := establishedFixture(t)
			if tc.refused {
				seedForeignInstall(t, mcpFixture)
			}
			gotMCP := tc.mcp(t, mcpFixture)

			connFixture := establishedFixture(t)
			if tc.refused {
				seedForeignInstall(t, connFixture)
			}
			gotConn := tc.conn(t, connFixture)

			c.Eq(tc.refused, gotMCP.refused, "MCP face refused? (text: %q)", gotMCP.text)
			c.Eq(tc.refused, gotConn.refused, "Connect face refused? (text: %q)", gotConn.text)
			if !tc.refused {
				// Both answered a row for the same machine through the same
				// orchestrator: the faces differ in how they RENDER an install, not
				// in whether they admit one.
				c.NotEmpty(gotConn.text, "the Connect face must answer an executor row id")
				c.StrContains(gotMCP.text, "installed ublk-s2",
					"the MCP face must render the row it installed")
				return
			}
			for face, got := range map[string]installFaceOutcome{"mcp": gotMCP, "connect": gotConn} {
				c.StrContains(got.text, tc.want, "%s face must carry the daemon's own refusal", face)
			}
			// The Connect handler and the MCP tool must refuse with the same
			// CLASS, not merely the same words: the code is what a client
			// branches on.
			c.Eq(connect.CodePermissionDenied, gotConn.code, "Connect refusal code for %q", tc.name)
		})
	}
}

// seedForeignInstall gives the fixture an installed executor a DIFFERENT child
// installed, which is what makes an uninstall from c_kid a foreign-row refusal
// rather than a not-found.
func seedForeignInstall(t *testing.T, f *installFixture) {
	t.Helper()
	c := assert.NewAborting(t)
	f.execs.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "ublk-s2"},
		OwnerUserID: installTestOwner, Enabled: true, Isolation: "none",
		WorkspaceMode: "pinned", Roots: []string{"~"},
	}
	c.Require().NoError(f.installs.Insert(context.Background(), executorinstall.Row{
		ID: "recipe-1", OwnerUserID: installTestOwner, ExecutorID: "exec-1",
		LauncherExecutorID: installTestLauncher, Target: "brent@ublk-s2",
		Instance: "work", Platform: "linux/amd64", InstalledByChild: "c_sib",
	}), "seed a recipe installed by a sibling")
}

// TestInstallFacesDescriptionIsIdenticalForEveryKind pins that executor_install
// carries no per-kind caveat: native Claude tools stay on the launch host, but
// a rafiki-mode claude child's executor-routed tools reach the executor it
// installs, so telling any caller it cannot use the executor would be false.
func TestInstallFacesDescriptionIsIdenticalForEveryKind(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	seedInstallChild(t, f.ctrl, "c_claude", "", installTestOwnerName, protocol.KindClaude)
	seedInstallChild(t, f.ctrl, "c_fundi", "", installTestOwnerName, protocol.KindFundi)
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_rafiki_mode", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
		StartedAt: time.Now(), OwnerUserID: installTestOwner,
		WorkspaceTools: protocol.WorkspaceToolsRafiki,
		Labels:         map[string]string{"owner": installTestOwnerName, childstore.LabelRoot: "c_rafiki_mode"},
	})

	descriptionFor := func(childID string) string {
		t.Helper()
		face, _ := mcpFaceFixture(t)
		face.SetController(f.ctrl)
		cs := mcpConnect(t, face.getServer(mcpChildRequest(installTestOwner, childID, false)))
		res, err := cs.ListTools(context.Background(), nil)
		c.NoError(err, "ListTools on the MCP face")
		for _, tool := range res.Tools {
			if tool.Name == "executor_install" {
				return tool.Description
			}
		}
		t.Fatalf("the MCP face did not offer executor_install to %s", childID)
		return ""
	}

	for _, id := range []string{"c_claude", "c_rafiki_mode", "c_fundi"} {
		d := descriptionFor(id)
		c.NotStrContains(d, "You cannot run workspace tools", "%s must not be told it cannot use the executor", id)
		c.StrContains(d, "Install and start a rafiki executor", "%s still gets the tool", id)
	}
}

// fakeInstallController records the proto request the binding built and answers
// the row it was seeded with. It stands in for *Controller so the binding's
// field mapping can be pinned without a store, a pool or an ssh target.
type fakeInstallController struct {
	row      *rafikiv1.ExecutorRow
	uninstl  []string
	sawReq   *rafikiv1.InstallExecutorRequest
	sawCalls int
}

func (f *fakeInstallController) InstallExecutor(
	_ context.Context, _ installCaller, req *rafikiv1.InstallExecutorRequest,
) (*rafikiv1.ExecutorRow, error) {
	f.sawCalls++
	f.sawReq = req
	return f.row, nil
}

func (f *fakeInstallController) UninstallExecutor(_ context.Context, _ installCaller, ref string) error {
	f.sawCalls++
	f.uninstl = append(f.uninstl, ref)
	return nil
}

// TestExecutorInstallBindingPassesMode pins that executor_install's `mode`
// argument reaches the Controller unchanged — including "" when the model asked
// for nothing, and including a value the daemon will refuse. The binding is not
// a validator: the vocabulary and the refusal are the daemon's, so the one rule
// lives in one place and the model reads the daemon's own message.
func TestExecutorInstallBindingPassesMode(t *testing.T) {
	c := assert.NewAborting(t)
	for _, mode := range []string{"", "auto", "direct", "tunnel", "sideways"} {
		ctrl := &fakeInstallController{row: &rafikiv1.ExecutorRow{Id: "exec-1", Machine: "ublk-s2"}}
		b := &executorInstallBinding{c: ctrl, caller: installCaller{OwnerUserID: installTestOwner}}
		res, err := b.Install(context.Background(), protocol.ExecutorInstallRequest{
			Target: "brent@ublk-s2", Name: "ublk-s2", Mode: mode,
		})
		c.Require().NoError(err, "Install(mode=%q)", mode)
		c.Require().NotNil(ctrl.sawReq, "the binding must reach the controller (mode=%q)", mode)
		c.Eq(mode, ctrl.sawReq.GetMode(), "mode %q must ride through unchanged", mode)
		c.Eq("brent@ublk-s2", ctrl.sawReq.GetTarget(), "target (mode=%q)", mode)
		c.Eq("ublk-s2", ctrl.sawReq.GetName(), "name (mode=%q)", mode)
		c.Eq("exec-1", res.ID, "the row's id (mode=%q)", mode)
	}
}

// TestExecutorInstallMCPFaceAdvertisesTheModeEnum pins that `mode` reaches the
// MCP face and not only the fundi runtime: both are served the same blueprint,
// and a claude child picking a transport reads THIS schema. The enum is
// asserted rather than the field's mere presence, because a parameter with no
// vocabulary is one a model fills with a guess.
func TestExecutorInstallMCPFaceAdvertisesTheModeEnum(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	seedInstallChild(t, f.ctrl, "c_kid", "", installTestOwnerName, protocol.KindFundi)
	face, _ := mcpFaceFixture(t)
	face.SetController(f.ctrl)
	cs := mcpConnect(t, face.getServer(mcpChildRequest(installTestOwner, "c_kid", false)))

	tool, ok := mcpToolsByName(t, cs)["executor_install"]
	c.Require().True(ok, "the MCP face must offer executor_install")
	schema, ok := tool.InputSchema.(map[string]any)
	c.Require().True(ok, "tool schema is %T, want an object", tool.InputSchema)
	props, ok := schema["properties"].(map[string]any)
	c.Require().True(ok, "the install schema has no properties object")
	raw, ok := props["mode"].(map[string]any)
	c.Require().True(ok, "the MCP face must advertise mode; properties = %v", props)
	c.Eq("string", raw["type"], "mode type")
	enum, ok := raw["enum"].([]any)
	c.Require().True(ok, "mode enum = %v", raw["enum"])
	var got []string
	for _, v := range enum {
		s, _ := v.(string)
		got = append(got, s)
	}
	c.EqDeep([]string{"auto", "direct", "tunnel"}, got, "mode enum on the MCP face")
}
