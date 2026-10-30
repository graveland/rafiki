// SPDX-License-Identifier: Apache-2.0

// Package conv is rafiki's conversation: an immutable, content-addressed tree
// of message nodes, the views over one conversation's tree, the chain-append
// rules that decide where a new chain attaches, and a node cache in front of a
// storage Backend.
//
// A node is identified by where it sits, not by its row id: its PathHash covers
// its parent's hash and its own canonical content (pkg/convtree), so appending
// a chain the conversation already holds inserts only the missing suffix and
// two histories that agree on every message converge on the same tree.
//
// Nodes are never mutated after they are published. An append inserts the new
// nodes into the cache and only then publishes the new head, so a reader that
// observes the new head always finds its nodes. One writer, many snapshot
// readers.
//
// The package is storage-agnostic and pgx-free: it may import the standard
// library, github.com/google/uuid, pkg/convtree, pkg/msg, pkg/msg/anthropic and
// pkg/slashcmd, and nothing else.
package conv

import (
	"context"
	"errors"
	"iter"
	"time"

	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/slashcmd"
)

// The Kind values a node can carry. KindCompactionSummary and KindClear are
// root kinds; KindCompactionTail tags a post-compaction copy of an earlier row,
// which every full-history reader drops.
const (
	KindCompactionSummary = "compaction_summary"
	KindCompactionTail    = "compaction_tail"
	KindClear             = "clear"
)

// Ref is a node's identity and its place in the tree, without its content.
type Ref struct {
	ID, ConversationID string
	ParentID           string // "" for a root
	DerivedFromID      string // "" unless a root derived from another node
	Ordinal, Depth     int
	Kind               string // "" for an ordinary row
	CreatedAt          time.Time
	PathHash           convtree.Hash
}

// Node is one message of a conversation tree. It is immutable once published:
// every field is set before the node enters the cache, and no code path writes
// to a node the cache can hand out.
type Node struct {
	Ref
	Msg               msg.Message // always msganthropic.Decode(role, Content)
	Content           []byte      // stored JSON: authoritative for hashing and re-storage
	ToolUseIDs        []string
	StopReason        string
	InputTokens       *int64
	OutputTokens      *int64
	ProviderMessageID string
}

// State is a conversation's mutable position: where its head is and which
// ordinal the next inserted node takes. ClearPending reports that a /clear is
// waiting for its boundary row.
type State struct {
	HeadID       string
	NextOrdinal  int
	ClearPending bool
}

// Lease is a write fence on one conversation. A zero Lease is unfenced.
type Lease struct {
	ConversationID, Holder, Token string
	ExpiresAt                     time.Time
}

// Held reports whether l fences anything.
func (l Lease) Held() bool { return l.ConversationID != "" && l.Token != "" }

// ErrNotFound reports a conversation the backend does not hold.
var ErrNotFound = errors.New("conv: conversation not found")

// ErrConflict reports that a write's expectation about the conversation no
// longer holds: the head or next ordinal moved, or the path hash it would
// insert is already there. It is retried by Append, never surfaced.
var ErrConflict = errors.New("conv: conversation head moved")

// ErrLeaseLost reports that another holder took the conversation over. It is
// returned as-is, never retried: the caller must stop writing AND stop acting.
var ErrLeaseLost = errors.New("store: conversation lease lost") // text kept: store.ErrLeaseLost aliases it

// Commit is one atomic tree write: the rows to insert and the head they leave
// behind.
type Commit struct {
	ConversationID      string
	ExpectHead          string
	ExpectNextOrdinal   int
	Rows                []*Node // new rows; ordinals ExpectNextOrdinal.., ids minted by the core
	NewHead             string  // "" only when the conversation ends with no head (retract of a lone root)
	Forks               int
	Fallback            bool
	ConsumeClearPending bool
	Lease               Lease // zero = unfenced
}

// Backend is the storage a Store sits in front of. Every method is
// conversation-scoped: a node id from another conversation is not found.
type Backend interface {
	State(ctx context.Context, convID string) (State, error)
	// Walk returns refs from fromID toward the root, head-first. crossRoots follows a
	// root's DerivedFromID; false stops at the first root. An id the conversation
	// does not hold — including a node of another conversation — is ErrNotFound,
	// never an empty chain: an empty walk would hide a head pointing out of its
	// conversation.
	Walk(ctx context.Context, convID, fromID string, crossRoots bool) ([]Ref, error)
	Fetch(ctx context.Context, convID string, ids []string) ([]*Node, error)
	Lookup(ctx context.Context, convID string, hashes []convtree.Hash) (map[convtree.Hash]Ref, error)
	Roots(ctx context.Context, convID string) ([]*Node, error)                                      // parent-less placed nodes, ordinal DESC
	ByProviderMessageID(ctx context.Context, convID, providerMessageID string) (*Node, bool, error) // placed rows only, newest ordinal
	HasChild(ctx context.Context, convID, parentID string) (bool, error)
	// Commit is atomic. A head/next_ordinal mismatch or a path_hash insert conflict is
	// ErrConflict; a fence failure is ErrLeaseLost. The fence is checked first, so a
	// taken-over writer always sees ErrLeaseLost, even when the head moved too — a
	// conflict is retried and a lost lease is not. A row whose ParentID or
	// DerivedFromID names a node the conversation does not hold is ErrNotFound,
	// never ErrConflict, so a caller does not retry a write that cannot land.
	// Returns the rows' created_at.
	Commit(ctx context.Context, c Commit) (time.Time, error)
}

// Options configures a Store. Every zero value is the documented default, not
// an off switch: CacheBytes 0 is 256 MiB and IdleTTL 0 is five minutes.
type Options struct {
	CacheBytes int64         // 0 = 256 MiB
	IdleTTL    time.Duration // 0 = 5 minutes
	Now        func() time.Time
}

// PageQuery bounds a Page read. Every field is optional; the zero value reads
// the whole transcript.
type PageQuery struct {
	// AfterOrdinal keeps nodes whose ordinal is strictly greater.
	AfterOrdinal *int
	// Before keeps nodes whose CreatedAt is strictly earlier.
	Before *time.Time
	// Limit keeps only the newest N nodes left after the other bounds, widened
	// backwards so the page never splits nodes that share a CreatedAt: a client
	// pages with the oldest time it received as the next Before, and a split
	// group's older half would carry that same time and be skipped for good. A
	// page can therefore hold more than Limit nodes. Limit must be positive.
	Limit *int
}

// Append describes one append onto the conversation tree: where the chain
// starts and the nodes to append under it. Exactly one parent mode is set.
type Append struct {
	// Parent: exactly one of ParentID, FromHead, Root.
	ParentID string
	FromHead bool   // parent = current head; no head makes Nodes[0] a RootKindRoot root
	Root     string // convtree.RootKind*
	// Root only: at most one of DerivedFromID, DerivedFromHead.
	DerivedFromID       string
	DerivedFromHead     bool
	Nodes               []NewNode // empty only with ParentID: then only the head moves
	Fallback            bool
	ConsumeClearPending bool
}

// NewNode is one message to append, before the core assigns its identity.
type NewNode struct {
	Msg               msg.Message // Role always read from here
	Content           []byte      // optional stored bytes; nil = msganthropic.EncodeContent(Msg)
	ToolUseIDs        []string    // nil = derived from Msg (tool_use ids and tool_result ids)
	StopReason        string
	InputTokens       *int64
	OutputTokens      *int64
	Kind              string // non-root rows only (KindCompactionTail)
	ProviderMessageID string
}

// Result is what an append did: the identity of every input node, in order,
// inserted or already present, and how many of them this call inserted.
type Result struct {
	Nodes    []*Node
	Inserted int
}

// IsClearBoundary reports whether n is the synthetic user row a /clear writes.
func IsClearBoundary(n *Node) bool {
	if n == nil || n.Kind != KindClear || n.Msg.Role != msg.User || len(n.Msg.Content) != 1 {
		return false
	}
	text, ok := n.Msg.Content[0].(msg.Text)
	return ok && text.Text == slashcmd.ClearBoundaryText
}

// Snapshot is one conversation's whole tree as it stood when the snapshot was
// taken: every node the walk reached, chronological (root first). Its views
// iterate the slice with no lock, and the nodes it holds stay readable after
// the cache has evicted them.
type Snapshot struct {
	nodes []*Node
}

// Head returns the newest node of the tree, or nil for an empty conversation.
func (sn *Snapshot) Head() *Node {
	if len(sn.nodes) == 0 {
		return nil
	}
	return sn.nodes[len(sn.nodes)-1]
}

// History yields every node of the tree, chronological, crossing compaction
// and clear boundaries.
func (sn *Snapshot) History() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for _, n := range sn.nodes {
			if !yield(n) {
				return
			}
		}
	}
}

// Working yields the suffix from the nearest root: the path a new request
// replays.
func (sn *Snapshot) Working() iter.Seq[*Node] {
	start := 0
	for i, n := range sn.nodes {
		if n.ParentID == "" {
			start = i
		}
	}
	return func(yield func(*Node) bool) {
		for _, n := range sn.nodes[start:] {
			if !yield(n) {
				return
			}
		}
	}
}

// Transcript yields History minus KindCompactionTail: the rows a reader renders
// as the conversation.
func (sn *Snapshot) Transcript() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for _, n := range sn.nodes {
			if n.Kind == KindCompactionTail {
				continue
			}
			if !yield(n) {
				return
			}
		}
	}
}
