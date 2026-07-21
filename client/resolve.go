package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"git.graveland.dev/brent/fundi/protocol"
)

// Lister is the subset of Client that Resolve needs. A concrete *Client
// implements this via its List method; tests pass a fake.
type Lister interface {
	List(ctx context.Context, filter protocol.ListFilter) ([]protocol.ChildSummary, error)
}

// ResolveWith resolves a user-supplied identifier to a childId.
//
// Algorithm:
//  1. Empty input → error.
//  2. Starts with "c_" → fast path, return as-is (no round-trip needed).
//  3. Call List, match exact name, then exact childId, then prefix of either.
//  4. Multiple prefix matches → ambiguity error naming the candidates.
//  5. No match → error.
func ResolveWith(ctx context.Context, l Lister, input string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("empty identifier")
	}
	// Fast path: childIds carry the c_ prefix; no list needed.
	if strings.HasPrefix(input, "c_") {
		return input, nil
	}

	children, err := l.List(ctx, protocol.ListFilter{})
	if err != nil {
		return "", fmt.Errorf("list children: %w", err)
	}

	// Exact name wins outright.
	for _, ch := range children {
		if ch.Name == input {
			return ch.ChildID, nil
		}
	}
	// Exact childId is next.
	for _, ch := range children {
		if ch.ChildID == input {
			return ch.ChildID, nil
		}
	}
	// Prefix match on name or childId; collect all candidates.
	var candidates []protocol.ChildSummary
	for _, ch := range children {
		if strings.HasPrefix(ch.Name, input) || strings.HasPrefix(ch.ChildID, input) {
			candidates = append(candidates, ch)
		}
	}
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no child matches %q", input)
	case 1:
		return candidates[0].ChildID, nil
	default:
		matches := make([]string, len(candidates))
		for i, ch := range candidates {
			if ch.Name != "" {
				matches[i] = ch.Name
			} else {
				matches[i] = ch.ChildID
			}
		}
		return "", fmt.Errorf("ambiguous identifier %q matches: %s",
			input, strings.Join(matches, ", "))
	}
}

// Resolve looks up a childId from the user's input (childId, name, or
// unambiguous prefix). Delegates to ResolveWith using c as the Lister.
func (c *Client) Resolve(ctx context.Context, input string) (string, error) {
	return ResolveWith(ctx, c, input)
}

// List calls ctrl_list and returns the children slice. It implements the
// Lister interface so *Client can be passed wherever a Lister is accepted.
func (c *Client) List(ctx context.Context, filter protocol.ListFilter) ([]protocol.ChildSummary, error) {
	resp, err := c.Request(ctx, protocol.ListRequest{
		Type:   protocol.TypeCtrlList,
		Filter: &filter,
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("ctrl_list: %s", FormatError(resp))
	}
	var data protocol.ListResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}
	return data.Children, nil
}

// FormatError formats an error response's Error field for human display.
// Returns "unknown error" when resp or resp.Error is nil.
func FormatError(resp *protocol.Response) string {
	if resp == nil || resp.Error == nil {
		return "unknown error"
	}
	return fmt.Sprintf("%s: %s", resp.Error.Code, resp.Error.Message)
}
