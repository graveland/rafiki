package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

// executorToolsFor is what agentRuntimeOptions uses to filter the child's
// routed set to what the chosen executor actually serves. It reads the pool's
// last Describe; nil (not an empty list) means "unknown, don't filter".
func TestExecutorToolsForReadsDescribe(t *testing.T) {
	withTools := ex("exec-a", map[string]string{}, "")
	withTools.Describe = &executorpb.DescribeResponse{Tools: []string{"read", "bash"}}

	c := &Controller{execPool: &fakePool{live: []execpool.LiveExecutor{withTools}}}

	got := c.executorToolsFor("exec-a")
	if len(got) != 2 || got[0] != "read" || got[1] != "bash" {
		t.Fatalf("executorToolsFor = %v, want the executor's Describe.tools", got)
	}
	if got := c.executorToolsFor("exec-missing"); got != nil {
		t.Errorf("executorToolsFor for an unknown id = %v, want nil", got)
	}
	if got := (&Controller{}).executorToolsFor("exec-a"); got != nil {
		t.Errorf("executorToolsFor with no pool = %v, want nil", got)
	}
}
