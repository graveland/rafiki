package main

import (
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// selectExecutor picks an executor from the live pool based on the request's
// label selector. The refusal path is as important as the success path: a
// spawn whose grant no live executor satisfies fails IMMEDIATELY, naming what
// was required, what was live, and which predicate excluded each candidate.
func (c *Controller) selectExecutor(req protocol.SpawnRequest) (tools.ExecutorClient, error) {
	if c.execPool == nil {
		return nil, fmt.Errorf("executor selector requested but no executor listener is configured (set RAFIKI_EXECUTOR_LISTEN)")
	}

	sel, err := executors.ParseSelector(req.ExecutorSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid executor selector %q: %w", req.ExecutorSelector, err)
	}

	live := c.execPool.Live()
	if len(live) == 0 {
		return nil, fmt.Errorf("spawn refused: no executor satisfies %q (0 live executors)", req.ExecutorSelector)
	}

	// Collect matching executors.
	var matching []execpool.LiveExecutor
	for _, le := range live {
		if sel.Matches(le.Executor.Labels) {
			matching = append(matching, le)
		}
	}
	if len(matching) == 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "spawn refused: no executor satisfies %q.\n", req.ExecutorSelector)
		fmt.Fprintf(&sb, "  %d live executor(s):\n", len(live))
		for _, le := range live {
			fmt.Fprintf(&sb, "    %s (%s)  excluded: labels do not match selector\n", le.Executor.ID[:12], le.Executor.DisplayName)
		}
		return nil, fmt.Errorf("%s", sb.String())
	}

	// Pick the first matching executor.
	le := matching[0]
	cl, err := c.execPool.ClientFor(le.Executor.ID)
	if err != nil {
		return nil, fmt.Errorf("executor %s selected but not reachable: %w", le.Executor.ID[:12], err)
	}
	return cl, nil
}
