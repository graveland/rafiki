// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// formatVenvFailures renders zero or more failed venv-build results as a
// human-readable block, one line per failing module: "dependency install
// failed on <name>: <error>". Returns "" when every result is Ready == true
// or results is empty. Both agent_pymodules.go's pymoduleWriter and
// mcp_pymodules.go's mcpPyModuleStore render their post-put/post-delete sync
// through this one helper, so the two faces word failures identically.
//
// The input is the pushAll aggregation: its ORDER is nondeterministic across
// executors (each push appends concurrently) and it deliberately keeps
// DUPLICATES -- the same module may fail on more than one executor. This
// helper is therefore order-agnostic and does not deduplicate: every failing
// entry gets its own line, because each is an independently failing install.
func formatVenvFailures(results []*executorpb.PyModuleVenvResult) string {
	var lines []string
	for _, r := range results {
		if r == nil || r.GetReady() {
			continue
		}
		lines = append(lines, fmt.Sprintf("dependency install failed on %s: %s", r.GetName(), r.GetError()))
	}
	return strings.Join(lines, "\n")
}
