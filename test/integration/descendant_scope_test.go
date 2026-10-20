package integration_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// TestKillAndCloseDescendantScopes drives the scoped Kill/Close verbs against a
// real daemon: an exited-only close takes just the exited subagent and leaves
// the live sibling and the target, and a descendants-only kill stops the
// remaining subagent while the target keeps running.
func TestKillAndCloseDescendantScopes(t *testing.T) {
	c := assert.NewAborting(t)
	d := bootDaemon(t)
	client := d.control(t)

	top := d.spawnChild(t)
	exitedKid := d.spawnChildUnder(t, top)
	liveKid := d.spawnChildUnder(t, top)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := client.Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: exitedKid}))
	c.NoError(err, "kill the first subagent")
	c.Eq("exited", getChild(t, client, exitedKid).GetStatus(), "first subagent exited")

	closed, err := client.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{
		ChildId: top,
		Scope:   rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY,
	}))
	c.NoError(err, "exited-only close")
	c.EqDeep([]string{exitedKid}, closed.Msg.GetDescendantIds(), "only the exited subagent is closed")

	listed := map[string]string{}
	for _, s := range listChildren(t, client) {
		listed[s.GetChildId()] = s.GetStatus()
	}
	_, hasExited := listed[exitedKid]
	c.False(hasExited, "the closed subagent is gone")
	c.NotEq("exited", listed[top], "the target is untouched")
	c.NotEq("", listed[liveKid], "the live subagent is still listed")
	c.NotEq("exited", listed[liveKid], "the live subagent is still running")

	killed, err := client.Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{
		ChildId: top,
		Scope:   rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY,
	}))
	c.NoError(err, "descendants-only kill")
	c.EqDeep([]string{liveKid}, killed.Msg.GetDescendantIds(), "the remaining subagent is stopped")
	c.Eq("exited", getChild(t, client, liveKid).GetStatus(), "the subagent exited")
	c.NotEq("exited", getChild(t, client, top).GetStatus(), "the target keeps running")

	_, err = client.Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: top}))
	c.NoError(err, "cleanup kill")
}
