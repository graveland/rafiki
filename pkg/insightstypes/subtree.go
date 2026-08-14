// SPDX-License-Identifier: Apache-2.0

package insightstypes

// SubtreeSelector names the conversations belonging to one agent subtree.
//
// Two lists, because a child's conversation is reachable by two different
// routes depending on how it ran, and a subtree routinely mixes them:
//
//   - ConversationIDs — the in-process fundi path, where the daemon knows the
//     conversation UUID directly (childstore.Snapshot.SessionID).
//   - ExternalRefs — the proxy path, where the daemon sets
//     X-Rafiki-Session: <childID> and the row correlates on external_ref.
//
// A rollup that follows only one of these under-reports a mixed subtree, and
// under-reporting a budget is the failure direction that costs money.
type SubtreeSelector struct {
	ConversationIDs []string
	ExternalRefs    []string
}
