// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// resolveTargetConnect is resolveTarget (cli_helpers.go) over the Connect
// control plane: the same active-marker fallback and the same matching
// semantics as pkg/client/resolve.go's ResolveWith, including its error
// strings.
//
//   - Empty input → the profile's active marker (getActive); still empty → a
//     clear error pointing at `rafiki list`.
//   - An input that already looks like a child id (the c_ prefix) is returned
//     as-is: no round trip, and no list can refine what is already an id.
//   - Otherwise ListChildren and match exact name, then exact child id, then
//     a unique prefix of either. The exact-id and id-prefix branches exist
//     for parity with ResolveWith even though today's ids all carry the c_
//     prefix and the fast path returns those first.
//
// The request carries no filter: resolution must see every child, not just
// the live ones.
func resolveTargetConnect(ctx context.Context, c rafikiv1connect.ControlClient, profileName, input, describe string) (string, error) {
	if input == "" {
		input = getActive(profileName)
		if input == "" {
			return "", fmt.Errorf("no child specified and no active marker; run `rafiki list` to see options")
		}
	}
	if strings.HasPrefix(input, "c_") {
		return input, nil
	}

	resp, err := c.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		return "", connectVerbErr(err, describe)
	}
	children := resp.Msg.GetChildren()

	// Exact name wins outright.
	for _, ch := range children {
		if ch.GetName() == input {
			return ch.GetChildId(), nil
		}
	}
	// Exact child id is next.
	for _, ch := range children {
		if ch.GetChildId() == input {
			return ch.GetChildId(), nil
		}
	}
	// Prefix match on name or child id; collect all candidates.
	var candidates []*rafikiv1.ChildSummary
	for _, ch := range children {
		if strings.HasPrefix(ch.GetName(), input) || strings.HasPrefix(ch.GetChildId(), input) {
			candidates = append(candidates, ch)
		}
	}
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no child matches %q", input)
	case 1:
		return candidates[0].GetChildId(), nil
	default:
		matches := make([]string, len(candidates))
		for i, ch := range candidates {
			if ch.GetName() != "" {
				matches[i] = ch.GetName()
			} else {
				matches[i] = ch.GetChildId()
			}
		}
		return "", fmt.Errorf("ambiguous identifier %q matches: %s",
			input, strings.Join(matches, ", "))
	}
}
