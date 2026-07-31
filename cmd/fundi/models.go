package main

import (
	"context"
	"sort"

	"go.graveland.dev/rafiki/pkg/models"
)

// completeModel returns tab-completion candidates for the --model flag.
// It combines all sources from the shared models package: user-defined models
// from ~/.pi/agent/models.json, a curated static list, and live enumeration
// of Ollama and LM Studio models (best-effort; silently skipped if not
// reachable).  Errors are swallowed so completion never blocks or errors out.
func completeModel(_ string) []string {
	list := models.List(context.Background())
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}
