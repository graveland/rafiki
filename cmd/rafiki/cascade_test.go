package main

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func kid(id, parent, status string, native bool) *rafikiv1.ChildSummary {
	labels := map[string]string{}
	if parent != "" {
		labels["rafiki/parent"] = parent
	}
	if native {
		labels["rafiki/native-subagent"] = "1"
	}
	return &rafikiv1.ChildSummary{ChildId: id, Status: status, Labels: labels}
}

func ids(cs []*rafikiv1.ChildSummary) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.GetChildId()
	}
	return out
}

func TestSubagentsOf(t *testing.T) {
	c := assert.NewCollecting(t)
	children := []*rafikiv1.ChildSummary{
		kid("a", "", "idle", false),
		kid("b", "a", "idle", false),
		kid("c", "b", "exited", false),
		kid("n", "a", "idle", true),
		kid("z", "", "idle", false),
	}
	c.EqDeep([]string{"b", "c"}, ids(subagentsOf(children, "a", false)), "a's subagents for a close: every one, natives excluded")
	c.EqDeep([]string{"b"}, ids(subagentsOf(children, "a", true)), "a's subagents for a stop: only the live ones")
	c.Eq(0, len(subagentsOf(children, "z", false)), "a childless target")
}

func TestIncludeSubagentsObeysAnExplicitFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, want := range []bool{true, false} {
		cmd := newStopCmd()
		c.NoError(cmd.Flags().Set(includeSubagentsFlag, map[bool]string{true: "true", false: "false"}[want]), "set flag")
		got, err := includeSubagents(context.Background(), cmd, nil, "a", "a", false)
		c.NoError(err, "explicit flag needs no daemon round trip")
		c.Eq(want, got, "include")
	}
}
