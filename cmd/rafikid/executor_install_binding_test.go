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
// TestInstallFacesAdmitIdentically is the table; the note test pins the one
// place the faces deliberately differ (a claude child is told it cannot use
// what it installs, because its own tools stop at the host it runs on).

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

// TestInstallFacesClaudeDescriptionNote pins the one deliberate difference
// between the faces: a claude child cannot run workspace tools on the executor
// it installs, so executor_install says so — and a fundi child, which addresses
// executors directly, is not told that lie.
func TestInstallFacesClaudeDescriptionNote(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	seedInstallChild(t, f.ctrl, "c_claude", "", installTestOwnerName, protocol.KindClaude)
	seedInstallChild(t, f.ctrl, "c_fundi", "", installTestOwnerName, protocol.KindFundi)

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

	claude := descriptionFor("c_claude")
	c.StrContains(claude, "You cannot run workspace tools on it yourself; agents you spawn can.",
		"a claude child must be told it cannot use the executor it installs")

	fundi := descriptionFor("c_fundi")
	c.NotStrContains(fundi, "You cannot run workspace tools",
		"a fundi child addresses executors directly and must not be told otherwise")
	c.StrContains(fundi, "Install and start a rafiki executor",
		"the fundi child still gets the tool")
}
