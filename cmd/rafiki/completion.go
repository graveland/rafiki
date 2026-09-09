// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// completionDeadline bounds a completion RPC. A shell is blocked while this
// runs, so the budget is small and a miss is an acceptable outcome — the old
// client-side path used the same 2s reasoning for its OpenRouter fetch.
const completionDeadline = 2 * time.Second

// completionChild is the slice of ChildSummary completion actually uses. A
// local struct rather than the proto type because it is what gets cached, and
// a cache file should not be a generated wire shape it must stay in sync with.
type completionChild struct {
	ChildID string            `json:"childId"`
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// completionSkill is the slice of a ListSkills row completion uses.
type completionSkill struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// skillCacheTTL and userCacheTTL mirror childCacheTTL. Both mutate rarely,
// but nothing drops their entries on a mutation — `rafiki skills rm` and
// `rafiki user rm` change daemon state no completion verb observes — so a
// short TTL is what bounds the staleness a user notices (delete a skill,
// immediately TAB for it). The mutating verbs DO drop the entries now (see
// dropSkillCompletionCache/dropUserCompletionCache), which is the same
// self-inflicted-staleness fix dropChildCompletionCache makes for children;
// the TTL then only ever hides another actor's change.
const (
	skillCacheTTL = childCacheTTL
	userCacheTTL  = childCacheTTL
)

// completionEndpointKey names the endpoint a cached answer came from, so two
// daemons' answers never share a file. It asks the endpoint resolver rather
// than recomputing the answer from the environment: reads, writes and drops
// name the same endpoint that was actually dialed, and a profile switch moves
// the key with it. When the resolver refuses (a remote profile with no token)
// the profile's URL/socket is still a stable key for the drop path — going
// through resolveProfile rather than a second resolver, since a completion
// handler must never exit or print.
func completionEndpointKey(cmd *cobra.Command) string {
	ep, err := newConnectEndpoint(cmd)
	if err == nil {
		return ep.identity
	}
	p, err := resolveProfile(cmd)
	if err != nil {
		return ""
	}
	if p.URL != "" {
		return p.URL
	}
	return p.Socket
}

// completionChildrenCached returns the cached rows, or nil on any miss.
func completionChildrenCached(cmd *cobra.Command, ttl time.Duration) []completionChild {
	var out []completionChild
	if cacheRead("children", completionEndpointKey(cmd), ttl, &out) {
		return out
	}
	return nil
}

// dropChildCompletionCache invalidates the child cache. Called by every verb
// that changes the child set — create, kill, close, label — which is what
// makes childCacheTTL safe at 15s: the staleness a user notices is the one
// they just caused.
func dropChildCompletionCache(cmd *cobra.Command) {
	cacheDrop("children", completionEndpointKey(cmd))
}

// completionChildren returns every child the daemon knows, cached.
//
// It goes through newConnectEndpoint rather than dialing a socket directly.
// That is the whole fix: the previous implementation called
// client.Dial(socketFromCmd(cmd)) and never consulted RAFIKI_URL, so with a
// remote daemon it dialed a socket that was not there and returned nil —
// silently, because completion swallows every error by design.
//
// Every failure yields no candidates. A completion handler must never exit,
// never block long, and never print.
func completionChildren(cmd *cobra.Command) []completionChild {
	if rows := completionChildrenCached(cmd, childCacheTTL); rows != nil {
		return rows
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil // remote with no token: a 401 is the only possible outcome
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionDeadline)
	defer cancel()

	resp, err := ep.control().ListChildren(ctx,
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		return nil
	}
	out := make([]completionChild, 0, len(resp.Msg.GetChildren()))
	for _, ch := range resp.Msg.GetChildren() {
		out = append(out, completionChild{
			ChildID: ch.GetChildId(),
			Name:    ch.GetName(),
			Status:  ch.GetStatus(),
			Labels:  ch.GetLabels(),
		})
	}
	cacheWrite("children", completionEndpointKey(cmd), out)
	return out
}

// ─── skills ────────────────────────────────────────────────────────────────────

// dropSkillCompletionCache invalidates the skill cache. Called by every verb
// that changes the skill set — add, import, rm, enable, disable — so the 15s
// skillCacheTTL never hides a change the user just made themselves.
func dropSkillCompletionCache(cmd *cobra.Command) {
	cacheDrop("skills", completionEndpointKey(cmd))
}

// completionSkills returns the daemon's skill rows, cached.
//
// It reads the same ListSkills source `rafiki skills list` reads (default
// scope: enabled skills only, matching that verb's default). Every failure
// yields no candidates: a completion handler must never exit, never block
// long, and never print.
func completionSkills(cmd *cobra.Command) []completionSkill {
	var rows []completionSkill
	if cacheRead("skills", completionEndpointKey(cmd), skillCacheTTL, &rows) {
		return rows
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil // remote with no token: a 401 is the only possible outcome
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionDeadline)
	defer cancel()

	resp, err := ep.control().ListSkills(ctx,
		connect.NewRequest(&rafikiv1.ListSkillsRequest{}))
	if err != nil {
		return nil
	}
	out := make([]completionSkill, 0, len(resp.Msg.GetRows()))
	for _, r := range resp.Msg.GetRows() {
		out = append(out, completionSkill{
			Namespace: r.GetNamespace(),
			Name:      r.GetName(),
		})
	}
	cacheWrite("skills", completionEndpointKey(cmd), out)
	return out
}

// completeSkills returns qualified "ns:name" candidates for skills
// show/rm/enable/disable, which all take the same <namespace:name> ref.
//
// A bare prefix (no colon typed) may name the skill itself: splitQualified
// only resolves a colonless ref against the DEFAULT namespace, so the
// candidate is still the full qualified form — completing it always inserts
// something the verb accepts, whatever namespace the skill lives in.
func completeSkills(cmd *cobra.Command, toComplete string) []string {
	// A colon already fixes the namespace; past it only the qualified form can
	// match, and the bare-name fallback below would offer unrelated
	// namespaces' skills.
	qualifiedOnly := strings.Contains(toComplete, ":")
	seen := make(map[string]struct{})
	var out []string
	for _, s := range completionSkills(cmd) {
		ref := s.Namespace + ":" + s.Name
		if _, ok := seen[ref]; ok {
			continue
		}
		if strings.HasPrefix(ref, toComplete) ||
			(!qualifiedOnly && strings.HasPrefix(s.Name, toComplete)) {
			seen[ref] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}

// ─── users ─────────────────────────────────────────────────────────────────────

// dropUserCompletionCache invalidates the user cache. Called by user create
// and user rm, the verbs that change the name set.
func dropUserCompletionCache(cmd *cobra.Command) {
	cacheDrop("users", completionEndpointKey(cmd))
}

// completeUsers returns username candidates for `rafiki user rm <name>`,
// sourcing them from whatever `rafiki user list` calls: ctrl_user_list, the
// framed control protocol.
//
// There is no Connect RPC for user rows, so this cannot ride the Connect
// endpoint the other helpers dial — instead it dials the framed control plane
// the way mustDial does, minus the exit: dialDaemon resolves the endpoint
// from the same profile resolver (resolveProfile) and returns an error
// rather than exiting, which is what a completion handler requires. The
// cache key still goes through newConnectEndpoint's identity, so reads and
// drops name the same endpoint every other completion cache entry does.
func completeUsers(cmd *cobra.Command, toComplete string) []string {
	return filterByPrefix(completionUserNames(cmd), toComplete)
}

// completionUserNames returns every active username the daemon knows, cached.
func completionUserNames(cmd *cobra.Command) []string {
	var names []string
	if cacheRead("users", completionEndpointKey(cmd), userCacheTTL, &names) {
		return names
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionDeadline)
	defer cancel()

	c, err := dialDaemon(ctx, cmd)
	if err != nil {
		return nil
	}
	defer c.Close()

	resp, err := c.Request(ctx, protocol.UserListRequest{Type: protocol.TypeCtrlUserList})
	if err != nil || !resp.Success {
		return nil
	}
	// ctrl_user_list wraps its rows (decodeUserList documents the shape); the
	// completion needs only the username field.
	var payload struct {
		Users []struct {
			Username string `json:"username"`
		} `json:"users"`
	}
	if err := json.Unmarshal(resp.Data, &payload); err != nil {
		return nil
	}
	for _, u := range payload.Users {
		if u.Username != "" {
			names = append(names, u.Username)
		}
	}
	cacheWrite("users", completionEndpointKey(cmd), names)
	return names
}
