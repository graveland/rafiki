// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// maxAppendAttempts bounds how many times a write re-resolves and retries
// after the head moved underneath it. A wrong declaration costs a retry, never
// corruption.
const maxAppendAttempts = 5

// chainParent is the resolved parent slot a chain hangs from.
type chainParent struct {
	// id is the parent ROW, empty for a root.
	id string
	// slot is the hash the first node chains from: the parent's path hash, or
	// a RootSeed for a root.
	slot convtree.Hash
	// depth is the depth Nodes[0] gets.
	depth int
}

// Append appends a chain of messages, inserting only the suffix the
// conversation does not already hold, and returns the identity of every node of
// the chain in order.
//
// Existing nodes of a chain must form a PREFIX of it. A gap means the tree
// disagrees with the chain being appended, which is reported rather than
// repaired — silently rewriting history is the failure this whole scheme exists
// to prevent.
func (h *Handle) Append(ctx context.Context, a Append) (Result, error) {
	if err := a.validate(); err != nil {
		return Result{}, err
	}
	e := h.lockEntry()
	defer e.writeMu.Unlock()

	for attempt := 0; attempt < maxAppendAttempts; attempt++ {
		st, err := h.writeState(ctx, e, attempt > 0)
		if err != nil {
			return Result{}, err
		}
		res, err := h.appendAttempt(ctx, e, a, st)
		if err == nil {
			return res, nil
		}
		if errors.Is(err, ErrLeaseLost) || !errors.Is(err, ErrConflict) {
			return Result{}, err
		}
	}
	return Result{}, fmt.Errorf("conv: append: %w after 5 attempts", ErrConflict)
}

// writeState resolves the position an append writes from. The cached head is
// trusted only for an owner entry; fresh forces the backend, which is what a
// conflict demands.
func (h *Handle) writeState(ctx context.Context, e *entry, fresh bool) (State, error) {
	if !fresh {
		if st, ok := h.store.cachedHead(h.convID, e, h.owner); ok {
			return State{HeadID: st.id, NextOrdinal: st.nextOrdinal}, nil
		}
	}
	st, err := h.store.backend.State(ctx, h.convID)
	if err != nil {
		return State{}, err
	}
	h.store.storeHeadWrite(h.convID, e, headState{id: st.HeadID, nextOrdinal: st.NextOrdinal})
	return st, nil
}

// appendAttempt resolves, hashes and commits one append against st. It has no
// side effect until Commit succeeds; a Commit error leaves the cache untouched
// and is retried by the caller.
func (h *Handle) appendAttempt(ctx context.Context, e *entry, a Append, st State) (Result, error) {
	parent, isRoot, rootKind, derivedFromID, err := h.resolveParent(ctx, a, st.HeadID)
	if err != nil {
		return Result{}, err
	}

	msgs := make([]convtree.Msg, len(a.Nodes))
	decoded := make([]msg.Message, len(a.Nodes))
	contents := make([][]byte, len(a.Nodes))
	for i, n := range a.Nodes {
		content, err := nodeContent(n)
		if err != nil {
			return Result{}, err
		}
		role := n.Msg.Role.String()
		message, err := msganthropic.Decode(role, content)
		if err != nil {
			return Result{}, fmt.Errorf("conv: append: decode node %d: %w", i, err)
		}
		contents[i] = content
		msgs[i] = convtree.Msg{Role: role, Content: content}
		decoded[i] = message
	}
	hashes, err := convtree.Chain(parent.slot, msgs)
	if err != nil {
		return Result{}, fmt.Errorf("conv: append: hash chain: %w", err)
	}

	existing, err := h.store.backend.Lookup(ctx, h.convID, hashes)
	if err != nil {
		return Result{}, fmt.Errorf("conv: append: read existing chain: %w", err)
	}

	// m is the first node the conversation does not already hold; everything
	// before it must be present and everything from it on must be absent.
	m := len(hashes)
	for i, hash := range hashes {
		if _, ok := existing[hash]; !ok {
			m = i
			break
		}
	}
	for i := m; i < len(hashes); i++ {
		if _, ok := existing[hashes[i]]; ok {
			return Result{}, fmt.Errorf("conv: append: tree invariant broken: node %d exists but node %d does not", i, m)
		}
	}

	forks := 0
	if m < len(hashes) {
		forkParent := parent.id
		if m > 0 {
			forkParent = existing[hashes[m-1]].ID
		}
		if forkParent != "" {
			has, err := h.store.backend.HasChild(ctx, h.convID, forkParent)
			if err != nil {
				return Result{}, fmt.Errorf("conv: append: fork check: %w", err)
			}
			if has {
				forks = 1
			}
		}
	}

	// The chain's existing prefix is resolved before the commit: a read that
	// failed afterwards would report a failure for a write that happened.
	var held []*Node
	if m > 0 {
		refs := make([]Ref, m)
		for i := 0; i < m; i++ {
			refs[i] = existing[hashes[i]]
		}
		held, err = h.store.nodesFor(ctx, h.convID, refs)
		if err != nil {
			return Result{}, err
		}
	}

	ids := make([]string, len(hashes))
	for i := range hashes {
		if ref, ok := existing[hashes[i]]; ok {
			ids[i] = ref.ID
		}
	}
	rows := make([]*Node, 0, len(hashes)-m)
	for i := m; i < len(hashes); i++ {
		ids[i] = uuid.Must(uuid.NewV7()).String()

		nodeParent := parent.id
		if i > 0 {
			nodeParent = ids[i-1]
		}
		var derivedFrom string
		if isRoot && i == 0 {
			derivedFrom = derivedFromID
		}
		kind := a.Nodes[i].Kind
		if isRoot && i == 0 {
			kind = ""
			if rootKind == convtree.RootKindCompaction || rootKind == convtree.RootKindClear {
				kind = rootKind
			}
		}
		// A published node owns its slices and pointed-to values: the caller's
		// NewNode must not be able to mutate a node through aliasing.
		toolUseIDs := a.Nodes[i].ToolUseIDs
		if toolUseIDs == nil {
			toolUseIDs = msg.ToolUseIDs(decoded[i])
		}
		rows = append(rows, &Node{
			Ref: Ref{
				ID:             ids[i],
				ConversationID: h.convID,
				ParentID:       nodeParent,
				DerivedFromID:  derivedFrom,
				Ordinal:        st.NextOrdinal + (i - m),
				Depth:          parent.depth + i,
				Kind:           kind,
				PathHash:       hashes[i],
			},
			Msg:               decoded[i],
			Content:           append([]byte(nil), contents[i]...),
			ToolUseIDs:        append([]string(nil), toolUseIDs...),
			StopReason:        a.Nodes[i].StopReason,
			InputTokens:       copyInt64(a.Nodes[i].InputTokens),
			OutputTokens:      copyInt64(a.Nodes[i].OutputTokens),
			ProviderMessageID: a.Nodes[i].ProviderMessageID,
		})
	}

	headID := a.ParentID
	if len(ids) > 0 {
		headID = ids[len(ids)-1]
	}

	// The backend sees copies: the value it committed and the value the cache
	// publishes must not be the same memory, or stamping CreatedAt on the
	// published node would reach into a row the backend already holds.
	committed := make([]*Node, len(rows))
	for i, n := range rows {
		committed[i] = copyNode(n)
	}
	created, err := h.store.backend.Commit(ctx, Commit{
		ConversationID:      h.convID,
		ExpectHead:          st.HeadID,
		ExpectNextOrdinal:   st.NextOrdinal,
		Rows:                committed,
		NewHead:             headID,
		Forks:               forks,
		Fallback:            a.Fallback,
		ConsumeClearPending: a.ConsumeClearPending,
		Lease:               h.lease,
	})
	if err != nil {
		return Result{}, err
	}
	for _, n := range rows {
		n.CreatedAt = created
	}
	// Rows first, head second: a reader that observes the new head must find
	// its nodes. The commit's position is published to whatever entry the
	// conversation has now, even when this writer's own entry was swept while it
	// was committing.
	h.store.putNodes(h.convID, rows...)
	h.store.storeCommitted(h.convID, e, headState{id: headID, nextOrdinal: st.NextOrdinal + len(rows)})

	nodes := make([]*Node, 0, len(hashes))
	nodes = append(nodes, held...)
	nodes = append(nodes, rows...)
	return Result{Nodes: nodes, Inserted: len(rows)}, nil
}

// resolveParent finds the slot the chain starts from. isRoot reports whether
// Nodes[0] is a root (an explicit Root, or a FromHead append on a conversation
// with no head yet), rootKind the resolved root kind, and derivedFromID the node
// a root is derived from ("" when none).
func (h *Handle) resolveParent(ctx context.Context, a Append, head string) (parent chainParent, isRoot bool, rootKind, derivedFromID string, err error) {
	if a.ParentID != "" || (a.FromHead && head != "") {
		parentID := a.ParentID
		if parentID == "" {
			parentID = head
		}
		ref, err := h.store.nodeRef(ctx, h.convID, parentID)
		if err != nil {
			return chainParent{}, false, "", "", fmt.Errorf("conv: append: %w", err)
		}
		return chainParent{id: parentID, slot: ref.PathHash, depth: ref.Depth + 1}, false, "", "", nil
	}

	rootKind = a.Root
	if rootKind == "" {
		rootKind = convtree.RootKindRoot
	}
	derivedFromID = a.DerivedFromID
	if derivedFromID == "" && a.DerivedFromHead {
		derivedFromID = head
	}
	if derivedFromID == "" {
		return chainParent{slot: convtree.RootSeed(rootKind, nil)}, true, rootKind, "", nil
	}
	ref, err := h.store.nodeRef(ctx, h.convID, derivedFromID)
	if err != nil {
		return chainParent{}, false, "", "", fmt.Errorf("conv: append: derived-from: %w", err)
	}
	return chainParent{slot: convtree.RootSeed(rootKind, &ref.PathHash)}, true, rootKind, derivedFromID, nil
}

// nodeContent returns the stored bytes a node is hashed and published as.
func nodeContent(n NewNode) ([]byte, error) {
	if n.Content == nil {
		encoded, err := msganthropic.EncodeContent(n.Msg)
		if err != nil {
			return nil, fmt.Errorf("conv: append: encode node content: %w", err)
		}
		return append([]byte(nil), StorableJSON(encoded)...), nil
	}
	return append([]byte(nil), StorableJSON(n.Content)...), nil
}

// validate refuses a request that does not describe exactly one well-formed
// append, before any I/O.
func (a Append) validate() error {
	modes := 0
	if a.ParentID != "" {
		modes++
	}
	if a.FromHead {
		modes++
	}
	if a.Root != "" {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("conv: append: exactly one of ParentID, FromHead or Root is required")
	}
	if len(a.Nodes) == 0 && a.ParentID == "" {
		return fmt.Errorf("conv: append: no nodes to append")
	}
	switch a.Root {
	case "", convtree.RootKindRoot, convtree.RootKindCompaction, convtree.RootKindClear:
	default:
		return fmt.Errorf("conv: append: unknown root kind %q", a.Root)
	}
	if a.Root == "" && (a.DerivedFromID != "" || a.DerivedFromHead) {
		return fmt.Errorf("conv: append: derived-from requires a root")
	}
	if a.DerivedFromID != "" && a.DerivedFromHead {
		return fmt.Errorf("conv: append: derived-from is DerivedFromID or DerivedFromHead, not both")
	}
	return nil
}

// RetractHead moves the conversation's head back to the parent of id, which
// must be the head and a user row. It is a head move, never a delete: the
// retracted row survives, unreachable from the working path, so a retry can
// append a different node at the same position.
//
// The head and role checks are pre-checks: a refusal still fences the lease
// first, because a writer that has lost the conversation must learn that rather
// than that its head is wrong. The fence is a commit that moves the head onto
// itself and writes no row, which is the only fence this interface offers.
func (h *Handle) RetractHead(ctx context.Context, id string) error {
	e := h.lockEntry()
	defer e.writeMu.Unlock()

	refusal := fmt.Errorf("conv: retract head: %s is not the head user row", id)
	for attempt := 0; attempt < maxAppendAttempts; attempt++ {
		st, err := h.store.backend.State(ctx, h.convID)
		if err != nil {
			return err
		}
		h.store.storeHeadWrite(h.convID, e, headState{id: st.HeadID, nextOrdinal: st.NextOrdinal})

		newHead, refused, err := h.retractTarget(ctx, id, st)
		if err != nil {
			return err
		}
		if refused {
			_, err := h.store.backend.Commit(ctx, Commit{
				ConversationID:    h.convID,
				ExpectHead:        st.HeadID,
				ExpectNextOrdinal: st.NextOrdinal,
				NewHead:           st.HeadID,
				Lease:             h.lease,
			})
			switch {
			case err == nil:
				return refusal
			case errors.Is(err, ErrLeaseLost):
				return err
			case errors.Is(err, ErrConflict):
				continue
			default:
				return err
			}
		}

		_, err = h.store.backend.Commit(ctx, Commit{
			ConversationID:    h.convID,
			ExpectHead:        st.HeadID,
			ExpectNextOrdinal: st.NextOrdinal,
			NewHead:           newHead,
			Lease:             h.lease,
		})
		if err == nil {
			h.store.storeCommitted(h.convID, e, headState{id: newHead, nextOrdinal: st.NextOrdinal})
			return nil
		}
		if errors.Is(err, ErrLeaseLost) || !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("conv: retract head: %w after 5 attempts", ErrConflict)
}

// retractTarget reports the head a retract of id would leave, or that id is not
// retractable at all.
func (h *Handle) retractTarget(ctx context.Context, id string, st State) (newHead string, refused bool, err error) {
	if st.HeadID != id {
		return "", true, nil
	}
	node, err := h.store.node(ctx, h.convID, id)
	if err != nil {
		return "", false, fmt.Errorf("conv: retract head: %w", err)
	}
	if node == nil || node.Msg.Role != msg.User {
		return "", true, nil
	}
	return node.ParentID, false, nil
}

// copyNode returns a node whose stored content, tool-use ids and token values
// are its own. Msg is shared: a decoded message is never written to after it is
// published.
func copyNode(n *Node) *Node {
	cp := *n
	cp.Content = append([]byte(nil), n.Content...)
	cp.ToolUseIDs = append([]string(nil), n.ToolUseIDs...)
	cp.InputTokens = copyInt64(n.InputTokens)
	cp.OutputTokens = copyInt64(n.OutputTokens)
	return &cp
}

// copyInt64 returns a fresh pointer to v's value.
func copyInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
