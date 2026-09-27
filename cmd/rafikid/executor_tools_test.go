package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

// executorToolsFor is what agentRuntimeOptions uses to filter the child's
// routed set to what the chosen executor actually serves. It reads the pool's
// last Describe; nil (not an empty list) means "unknown, don't filter".
func TestExecutorToolsForReadsDescribe(t *testing.T) {
	ck := assert.NewCollecting(t)
	withTools := ex("exec-a", map[string]string{}, "")
	withTools.Describe = &executorpb.DescribeResponse{Tools: []string{"read", "bash"}}

	c := &Controller{execPool: &fakePool{live: []execpool.LiveExecutor{withTools}}}

	got := c.executorToolsFor("exec-a")
	ck.Require().False(len(got) != 2 || got[0] != "read" || got[1] != "bash", "executorToolsFor = %v, want the executor's Describe.tools", got)
	ck.Nil(c.executorToolsFor("exec-missing"), "executorToolsFor for an unknown id")
	ck.Nil((&Controller{}).executorToolsFor("exec-a"), "executorToolsFor with no pool")
}
