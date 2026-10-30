// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/convtree"
)

// MemBackend is an in-process Backend: the conformance reference the SQL
// backend is measured against, and what a test that wants no database uses.
//
// Its CAS semantics match the Postgres one: a head or next-ordinal mismatch and
// a duplicate path hash are both ErrConflict. Leases are ignored — there is no
// second process to fence against, so a Commit carrying one simply proceeds.
type MemBackend struct {
	mu    sync.RWMutex
	convs map[string]*memConversation
	now   func() time.Time
}

// memConversation is one conversation's tree.
type memConversation struct {
	head         string
	nextOrdinal  int
	clearPending bool
	nodes        []*Node
	byID         map[string]*Node
	byHash       map[convtree.Hash]*Node
	byProvider   map[string]*Node
	children     map[string]int
}

// NewMemBackend returns an empty in-process backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{convs: make(map[string]*memConversation)}
}

// Ensure creates an empty conversation. It is idempotent: an existing
// conversation keeps its tree.
func (m *MemBackend) Ensure(convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.convs == nil {
		m.convs = make(map[string]*memConversation)
	}
	if _, ok := m.convs[convID]; !ok {
		m.convs[convID] = newMemConversation()
	}
}

func newMemConversation() *memConversation {
	return &memConversation{
		byID:       make(map[string]*Node),
		byHash:     make(map[convtree.Hash]*Node),
		byProvider: make(map[string]*Node),
		children:   make(map[string]int),
	}
}

// stamp is the created_at a Commit writes. Tests may replace it.
func (m *MemBackend) stamp() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// conversation returns convID's tree, or ErrNotFound.
func (m *MemBackend) conversation(convID string) (*memConversation, error) {
	c, ok := m.convs[convID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, convID)
	}
	return c, nil
}

// State returns the conversation's head, next ordinal and clear flag.
func (m *MemBackend) State(_ context.Context, convID string) (State, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.conversation(convID)
	if err != nil {
		return State{}, err
	}
	return State{HeadID: c.head, NextOrdinal: c.nextOrdinal, ClearPending: c.clearPending}, nil
}

// Walk returns the chain from fromID toward the root, head-first, following a
// root's DerivedFromID only when crossRoots is set. An id the conversation does
// not hold is ErrNotFound: a walk that answered with an empty chain instead
// would hide a head pointing out of its conversation.
func (m *MemBackend) Walk(_ context.Context, convID, fromID string, crossRoots bool) ([]Ref, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.conversation(convID)
	if err != nil {
		return nil, err
	}
	if fromID == "" {
		return nil, fmt.Errorf("%w: no node to walk from in conversation %s", ErrNotFound, convID)
	}
	var out []Ref
	for id := fromID; id != ""; {
		n, ok := c.byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: node %s is not in conversation %s", ErrNotFound, id, convID)
		}
		out = append(out, n.Ref)
		switch {
		case n.ParentID != "":
			id = n.ParentID
		case crossRoots && n.DerivedFromID != "":
			id = n.DerivedFromID
		default:
			id = ""
		}
	}
	return out, nil
}

// Fetch returns the nodes it holds, in the order asked for. Ids the
// conversation does not hold are omitted, as an IN query would.
func (m *MemBackend) Fetch(_ context.Context, convID string, ids []string) ([]*Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.conversation(convID)
	if err != nil {
		return nil, err
	}
	out := make([]*Node, 0, len(ids))
	for _, id := range ids {
		if n, ok := c.byID[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// Lookup returns the refs of the nodes held under hashes.
func (m *MemBackend) Lookup(_ context.Context, convID string, hashes []convtree.Hash) (map[convtree.Hash]Ref, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.conversation(convID)
	if err != nil {
		return nil, err
	}
	out := make(map[convtree.Hash]Ref, len(hashes))
	for _, h := range hashes {
		if n, ok := c.byHash[h]; ok {
			out[h] = n.Ref
		}
	}
	return out, nil
}

// Roots returns the conversation's parent-less nodes, newest ordinal first.
func (m *MemBackend) Roots(_ context.Context, convID string) ([]*Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.conversation(convID)
	if err != nil {
		return nil, err
	}
	var out []*Node
	for _, n := range c.nodes {
		if n.ParentID == "" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal > out[j].Ordinal })
	return out, nil
}

// ByProviderMessageID returns the newest node carrying providerMessageID.
func (m *MemBackend) ByProviderMessageID(_ context.Context, convID, providerMessageID string) (*Node, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.conversation(convID)
	if err != nil {
		return nil, false, err
	}
	n, ok := c.byProvider[providerMessageID]
	return n, ok, nil
}

// HasChild reports whether parentID has a child.
func (m *MemBackend) HasChild(_ context.Context, convID, parentID string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.conversation(convID)
	if err != nil {
		return false, err
	}
	return c.children[parentID] > 0, nil
}

// Commit applies one atomic tree write. The rows are copied, stamped with one
// created_at, and the head moves only when the expectation held. A row whose
// parent or derived-from names a node of ANOTHER conversation is refused: this
// check is what keeps one conversation's tree out of another's.
func (m *MemBackend) Commit(ctx context.Context, commit Commit) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.conversation(commit.ConversationID)
	if err != nil {
		return time.Time{}, err
	}
	if commit.ExpectHead != c.head || commit.ExpectNextOrdinal != c.nextOrdinal {
		return time.Time{}, fmt.Errorf("%w: %s", ErrConflict, commit.ConversationID)
	}
	seen := make(map[convtree.Hash]bool, len(commit.Rows))
	for _, row := range commit.Rows {
		if _, dup := c.byHash[row.PathHash]; dup {
			return time.Time{}, fmt.Errorf("%w: %s", ErrConflict, commit.ConversationID)
		}
		if seen[row.PathHash] {
			return time.Time{}, fmt.Errorf("%w: %s", ErrConflict, commit.ConversationID)
		}
		seen[row.PathHash] = true
	}
	if err := checkCommitParents(c.byID, commit); err != nil {
		return time.Time{}, err
	}

	created := m.stamp()
	for _, row := range commit.Rows {
		n := *row
		n.Content = append([]byte(nil), row.Content...)
		n.ToolUseIDs = append([]string(nil), row.ToolUseIDs...)
		n.ConversationID = commit.ConversationID
		n.CreatedAt = created
		c.nodes = append(c.nodes, &n)
		c.byID[n.ID] = &n
		c.byHash[n.PathHash] = &n
		if n.ParentID != "" {
			c.children[n.ParentID]++
		}
		if n.ProviderMessageID != "" {
			c.byProvider[n.ProviderMessageID] = &n
		}
	}
	c.nextOrdinal += len(commit.Rows)
	c.head = commit.NewHead
	if commit.ConsumeClearPending {
		c.clearPending = false
	}
	return created, nil
}

// checkCommitParents refuses a commit whose rows hang off nodes the
// conversation does not hold. A node of another conversation is exactly that
// case: its id is globally unique, so nothing but this check distinguishes it
// from a parent of the conversation being written.
func checkCommitParents(byID map[string]*Node, commit Commit) error {
	known := make(map[string]bool, len(commit.Rows))
	for _, row := range commit.Rows {
		known[row.ID] = true
	}
	for _, row := range commit.Rows {
		for _, id := range [2]string{row.ParentID, row.DerivedFromID} {
			if id == "" || known[id] {
				continue
			}
			if _, ok := byID[id]; !ok {
				return fmt.Errorf("%w: node %s is not in conversation %s", ErrNotFound, id, commit.ConversationID)
			}
		}
	}
	return nil
}
