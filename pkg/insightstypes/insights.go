// SPDX-License-Identifier: Apache-2.0

// Package insightstypes holds the pure-data types for the conversations schema.
// These are the shape definitions (Stats, Transcript, ConversationSummary,
// filters) — no database driver. The query implementations live in pkg/insights.
package insightstypes

import (
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/routing"
)

// ErrNotFound is returned when a requested conversation does not exist. Callers
// (e.g. the gRPC handler) match it with errors.Is to map to a NotFound status.
var ErrNotFound = errors.New("insights: conversation not found")

// Pricer resolves a model id to its per-token list price. It is injected (the
// server passes ModelCatalog.Pricing) so insights carries no catalog/network
// concern. ok=false leaves the model unpriced.
type Pricer func(model string) (routing.ModelPricing, bool)

// Path selects a capture path in filters. It maps to the immutable driven_by
// column: the proxy (client-driven) path vs. the in-process (server-driven) one.
type Path string

const (
	PathAny    Path = ""       // no path filter
	PathProxy  Path = "proxy"  // driven_by = 'client'
	PathDirect Path = "direct" // driven_by = 'server'
)

// validate rejects a non-empty Path that is neither proxy nor direct, so a
// typo'd filter errors loudly instead of silently returning unfiltered results.
func (p Path) Validate() error {
	switch p {
	case PathAny, PathProxy, PathDirect:
		return nil
	default:
		return fmt.Errorf("invalid path %q: use %q or %q", string(p), PathProxy, PathDirect)
	}
}

// drivenBy maps a Path filter to the driven_by column value, or "" for no filter.
func (p Path) DrivenBy() string {
	switch p {
	case PathProxy:
		return "client"
	case PathDirect:
		return "server"
	default:
		return ""
	}
}

// argList accumulates positional query arguments and hands back the $N
// placeholder for each, so a dynamic WHERE stays parameterized.
