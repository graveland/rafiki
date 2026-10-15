// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// budgetGateExempt are the tools a halted child may still call: how a
// subagent hands back its results.
var budgetGateExempt = map[string]bool{"agent_report": true, "agent_result": true}

// budgetGated wraps t so Execute refuses while halted reports true.
type budgetGated struct {
	tools.Tool
	halted func() (string, bool)
}

func (g budgetGated) Execute(ctx context.Context, in tools.ToolInput) (tools.ToolResult, error) {
	if root, h := g.halted(); h {
		return tools.ToolResult{}, fmt.Errorf("budget exhausted on %s: no further rafiki tool calls will run. Report your results with agent_report or agent_result and stop; a raised budget lifts this", root)
	}
	return g.Tool.Execute(ctx, in)
}

// gateToolsOnBudget wraps every non-exempt tool.
func gateToolsOnBudget(ts []tools.Tool, halted func() (string, bool)) []tools.Tool {
	out := make([]tools.Tool, 0, len(ts))
	for _, t := range ts {
		if budgetGateExempt[t.Name()] {
			out = append(out, t)
			continue
		}
		out = append(out, budgetGated{Tool: t, halted: halted})
	}
	return out
}
