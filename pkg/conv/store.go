// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.graveland.dev/rafiki/pkg/convtree"
)

const (
	defaultCacheBytes = 256 << 20
	defaultIdleTTL    = 5 * time.Minute
)

// headState is an entry's cached position. It is what an owner read uses
// instead of asking the backend, and what a write publishes after its rows are
// cached.
type headState struct {
	id          string
	nextOrdinal int
}

// entry is the per-conversation cache state.
type entry struct {
	// head is the conversation's position as this process last saw it. A write
	// stores the new state only after its rows are in the LRU, so a reader that
	// observes the new head always finds its nodes.
	head atomic.Pointer[headState]
	// writeMu serialises Append and RetractHead on this conversation.
	writeMu sync.Mutex
	// owner, loaded and lastAccess are guarded by Store.mu, as is byProvider.
	owner      bool
	loaded     bool
	lastAccess time.Time
	// byProvider maps a provider message id to a node id, so a lookup that is
	// already cached costs no query. It holds ids rather than nodes on purpose:
	// an evicted node's mapping stays correct, it just needs a Fetch.
	byProvider map[string]string
}

// Store is a conversation node cache in front of a Backend. It is safe for
// concurrent use: one writer per conversation, many snapshot readers.
type Store struct {
	backend    Backend
	cacheBytes int64
	ttl        time.Duration
	now        func() time.Time

	mu      sync.Mutex
	lru     *list.List // front = hottest, back = coldest; values are *Node
	nodes   map[string]*list.Element
	charged int64
	entries map[string]*entry
}

// NewStore returns a Store over b. Every zero option is the documented
// default: a nil Now is time.Now.
func NewStore(b Backend, o Options) *Store {
	if o.CacheBytes == 0 {
		o.CacheBytes = defaultCacheBytes
	}
	if o.IdleTTL == 0 {
		o.IdleTTL = defaultIdleTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Store{
		backend:    b,
		cacheBytes: o.CacheBytes,
		ttl:        o.IdleTTL,
		now:        o.Now,
		lru:        list.New(),
		nodes:      make(map[string]*list.Element),
		entries:    make(map[string]*entry),
	}
}

// openConfig is what OpenOption mutates.
type openConfig struct {
	owner bool
	lease Lease
}

// OpenOption customises a Handle.
type OpenOption func(*openConfig)

// AsOwner marks the handle as this process's writer for the conversation: its
// reads trust the cached head and it keeps the entry alive as the owner.
func AsOwner() OpenOption {
	return func(c *openConfig) { c.owner = true }
}

// WithLease fences the handle's writes on l.
func WithLease(l Lease) OpenOption {
	return func(c *openConfig) { c.lease = l }
}

// Handle is a conversation descriptor. It holds no cache state of its own:
// every call resolves (or recreates) the conversation's entry, so a swept entry
// costs a State call rather than a stale answer.
type Handle struct {
	store  *Store
	convID string
	owner  bool
	lease  Lease
}

// Open returns a handle on convID. It performs no I/O: the only work it does is
// the lazy sweep of entries that have been idle past the TTL.
func (s *Store) Open(convID string, opts ...OpenOption) *Handle {
	s.mu.Lock()
	s.sweepLocked()
	s.mu.Unlock()
	var cfg openConfig
	for _, o := range opts {
		o(&cfg)
	}
	return &Handle{store: s, convID: convID, owner: cfg.owner, lease: cfg.lease}
}

// Page reads one page of the conversation's transcript.
func (s *Store) Page(ctx context.Context, convID string, q PageQuery) ([]*Node, error) {
	sn, err := s.Open(convID).Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return sn.Page(q)
}

// Transcript reads the conversation's whole transcript.
func (s *Store) Transcript(ctx context.Context, convID string) ([]*Node, error) {
	sn, err := s.Open(convID).Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Node, 0, len(sn.nodes))
	for n := range sn.Transcript() {
		out = append(out, n)
	}
	return out, nil
}

// touch resolves convID's entry, creating it when absent, marks it live and
// records owner. Entries idle past the TTL are swept here and on every Open,
// which is the whole of the expiry mechanism: no goroutines, no timers.
func (s *Store) touch(convID string, owner bool) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.entries[convID]
	if e == nil {
		e = &entry{byProvider: make(map[string]string)}
		s.entries[convID] = e
	}
	if owner {
		e.owner = true
	}
	e.lastAccess = s.now()
	return e
}

// sweepLocked drops entries that have been idle past the TTL.
func (s *Store) sweepLocked() {
	now := s.now()
	for id, e := range s.entries {
		if now.Sub(e.lastAccess) > s.ttl {
			delete(s.entries, id)
		}
	}
}

// cachedHead returns the entry's position when it may be trusted: only an
// owner handle reading an owner entry whose head has been loaded answers
// without the backend.
func (s *Store) cachedHead(e *entry, owner bool) (headState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !owner || !e.owner || !e.loaded {
		return headState{}, false
	}
	hp := e.head.Load()
	if hp == nil {
		return headState{}, false
	}
	return *hp, true
}

// storeHead records st on convID's live entry, which is e unless the entry was
// swept and recreated while the caller was working.
//
// A slower reader must never move the entry's position backwards: next ordinal
// only grows, so a state that is behind the one already stored is dropped. That
// is what stops a reader that asked the backend before a commit from clobbering
// the head the commit published.
func (s *Store) storeHead(convID string, e *entry, st headState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.entries[convID]
	if live == nil {
		live = e
		s.entries[convID] = live
	}
	if cur := live.head.Load(); cur != nil && cur.nextOrdinal > st.nextOrdinal {
		return
	}
	live.loaded = true
	live.lastAccess = s.now()
	live.head.Store(&st)
}

// getLocked returns a cached node and marks it hot.
func (s *Store) getLocked(id string) (*Node, bool) {
	el, ok := s.nodes[id]
	if !ok {
		return nil, false
	}
	s.lru.MoveToFront(el)
	return el.Value.(*Node), true
}

// putLocked inserts a node, charging its stored content, and evicts from the
// cold end past the budget. A node's provider message id is indexed here,
// because this is where a node of that conversation enters the cache.
func (s *Store) putLocked(n *Node) {
	if _, ok := s.nodes[n.ID]; ok {
		return
	}
	s.nodes[n.ID] = s.lru.PushFront(n)
	s.charged += int64(len(n.Content))
	if e := s.entries[n.ConversationID]; e != nil && n.ProviderMessageID != "" {
		e.byProvider[n.ProviderMessageID] = n.ID
	}
	for s.charged > s.cacheBytes && s.lru.Len() > 0 {
		s.evictLocked()
	}
}

// evictLocked drops the coldest node. Eviction drops index entries only:
// snapshots hold node references, so a reader mid-iteration is unaffected, and
// an evicted node is re-fetched through Walk + Fetch.
func (s *Store) evictLocked() {
	el := s.lru.Back()
	if el == nil {
		return
	}
	n := el.Value.(*Node)
	s.lru.Remove(el)
	delete(s.nodes, n.ID)
	s.charged -= int64(len(n.Content))
}

// putNodes caches nodes and keeps the live entries warm.
func (s *Store) putNodes(nodes ...*Node) {
	if len(nodes) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, n := range nodes {
		s.putLocked(n)
		if e := s.entries[n.ConversationID]; e != nil {
			e.lastAccess = now
		}
	}
}

// cachedNode returns one cached node.
func (s *Store) cachedNode(id string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

// cachedPath walks from toward the root entirely out of the cache. ok is false
// on the first miss, which is what sends the caller to the backend.
func (s *Store) cachedPath(from string, crossRoots bool) ([]*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rev []*Node
	for id := from; id != ""; {
		n, ok := s.getLocked(id)
		if !ok {
			return nil, false
		}
		rev = append(rev, n)
		switch {
		case n.ParentID != "":
			id = n.ParentID
		case crossRoots && n.DerivedFromID != "":
			id = n.DerivedFromID
		default:
			id = ""
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, true
}

// pathFrom returns the chain from fromID toward the root, chronological. A
// cached walk answers it with no backend call; the first miss falls back to one
// Walk and one Fetch of the ids the cache lacks.
func (h *Handle) pathFrom(ctx context.Context, from string, crossRoots bool) ([]*Node, error) {
	if from == "" {
		return nil, nil
	}
	if path, ok := h.store.cachedPath(from, crossRoots); ok {
		return path, nil
	}
	refs, err := h.store.backend.Walk(ctx, h.convID, from, crossRoots)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	nodes, err := h.store.nodesFor(ctx, h.convID, refs)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	}
	return nodes, nil
}

// nodesFor resolves refs, head-first, to nodes, fetching the ones the cache
// does not hold in one call.
func (s *Store) nodesFor(ctx context.Context, convID string, refs []Ref) ([]*Node, error) {
	out := make([]*Node, len(refs))
	var missing []string
	s.mu.Lock()
	for i, r := range refs {
		if n, ok := s.getLocked(r.ID); ok {
			out[i] = n
			continue
		}
		missing = append(missing, r.ID)
	}
	s.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	fetched, err := s.backend.Fetch(ctx, convID, missing)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Node, len(fetched))
	for _, n := range fetched {
		byID[n.ID] = n
	}
	s.putNodes(fetched...)
	for i, r := range refs {
		if out[i] != nil {
			continue
		}
		n, ok := byID[r.ID]
		if !ok {
			return nil, fmt.Errorf("conv: fetch: node %s not in conversation %s", r.ID, convID)
		}
		out[i] = n
	}
	return out, nil
}

// nodeRef returns one node's ref, from the cache or the backend.
func (s *Store) nodeRef(ctx context.Context, convID, id string) (Ref, error) {
	if n, ok := s.cachedNode(id); ok {
		return n.Ref, nil
	}
	fetched, err := s.backend.Fetch(ctx, convID, []string{id})
	if err != nil {
		return Ref{}, fmt.Errorf("read node %s: %w", id, err)
	}
	for _, n := range fetched {
		if n.ID == id {
			s.putNodes(n)
			return n.Ref, nil
		}
	}
	return Ref{}, fmt.Errorf("node %s not in conversation %s", id, convID)
}

// node returns one node, from the cache or the backend, and nil when the
// conversation does not hold it.
func (s *Store) node(ctx context.Context, convID, id string) (*Node, error) {
	if n, ok := s.cachedNode(id); ok {
		return n, nil
	}
	fetched, err := s.backend.Fetch(ctx, convID, []string{id})
	if err != nil {
		return nil, fmt.Errorf("read node %s: %w", id, err)
	}
	for _, n := range fetched {
		if n.ID == id {
			s.putNodes(n)
			return n, nil
		}
	}
	return nil, nil
}

// readState resolves the conversation's position for a read. An owner entry
// whose head is loaded answers from memory; anything else asks the backend
// once and stores what it said.
func (h *Handle) readState(ctx context.Context, e *entry) (State, error) {
	if st, ok := h.store.cachedHead(e, h.owner); ok {
		return State{HeadID: st.id, NextOrdinal: st.nextOrdinal}, nil
	}
	st, err := h.store.backend.State(ctx, h.convID)
	if err != nil {
		return State{}, err
	}
	h.store.storeHead(h.convID, e, headState{id: st.HeadID, nextOrdinal: st.NextOrdinal})
	return st, nil
}

// State returns the conversation's position, always from the backend: it is
// the answer a caller asks for precisely when it does not trust its cache.
func (h *Handle) State(ctx context.Context) (State, error) {
	e := h.store.touch(h.convID, h.owner)
	st, err := h.store.backend.State(ctx, h.convID)
	if err != nil {
		return State{}, err
	}
	h.store.storeHead(h.convID, e, headState{id: st.HeadID, nextOrdinal: st.NextOrdinal})
	return st, nil
}

// Working returns the working path: the chain from the head to the nearest
// root, chronological.
func (h *Handle) Working(ctx context.Context) ([]*Node, error) {
	e := h.store.touch(h.convID, h.owner)
	st, err := h.readState(ctx, e)
	if err != nil {
		return nil, err
	}
	return h.pathFrom(ctx, st.HeadID, false)
}

// Snapshot returns the conversation's whole tree, crossing every root.
func (h *Handle) Snapshot(ctx context.Context) (*Snapshot, error) {
	e := h.store.touch(h.convID, h.owner)
	st, err := h.readState(ctx, e)
	if err != nil {
		return nil, err
	}
	nodes, err := h.pathFrom(ctx, st.HeadID, true)
	if err != nil {
		return nil, err
	}
	return &Snapshot{nodes: nodes}, nil
}

// RequestAt returns the working path ending at headID, minus working positions
// [trimFrom, trimTo) when both are set. It reads no conversation state: the
// caller names the head it is replaying.
func (h *Handle) RequestAt(ctx context.Context, headID string, trimFrom, trimTo *int) ([]*Node, error) {
	h.store.touch(h.convID, h.owner)
	nodes, err := h.pathFrom(ctx, headID, false)
	if err != nil {
		return nil, err
	}
	if trimFrom == nil || trimTo == nil {
		return nodes, nil
	}
	lo, hi := *trimFrom, *trimTo
	if lo < 0 {
		lo = 0
	}
	if hi > len(nodes) {
		hi = len(nodes)
	}
	if lo >= hi {
		return nodes, nil
	}
	kept := make([]*Node, 0, len(nodes)-(hi-lo))
	kept = append(kept, nodes[:lo]...)
	kept = append(kept, nodes[hi:]...)
	return kept, nil
}

// NodeByProviderMessageID returns the node an upstream message id names. The
// conversation's own index answers it when the node is cached, so a resume
// costs no query; otherwise the backend is asked once.
func (h *Handle) NodeByProviderMessageID(ctx context.Context, providerMessageID string) (*Node, bool, error) {
	e := h.store.touch(h.convID, h.owner)
	if providerMessageID != "" {
		h.store.mu.Lock()
		id, ok := e.byProvider[providerMessageID]
		h.store.mu.Unlock()
		if ok {
			n, err := h.store.node(ctx, h.convID, id)
			if err != nil {
				return nil, false, err
			}
			if n != nil {
				return n, true, nil
			}
		}
	}
	n, ok, err := h.store.backend.ByProviderMessageID(ctx, h.convID, providerMessageID)
	if err != nil || !ok {
		return nil, ok, err
	}
	h.store.putNodes(n)
	return n, true, nil
}

// Roots returns the conversation's parent-less nodes, newest ordinal first.
func (h *Handle) Roots(ctx context.Context) ([]*Node, error) {
	h.store.touch(h.convID, h.owner)
	roots, err := h.store.backend.Roots(ctx, h.convID)
	if err != nil {
		return nil, err
	}
	h.store.putNodes(roots...)
	return roots, nil
}

// NodesByHash returns the refs of the nodes the conversation holds under
// hashes.
func (h *Handle) NodesByHash(ctx context.Context, hashes []convtree.Hash) (map[convtree.Hash]Ref, error) {
	h.store.touch(h.convID, h.owner)
	return h.store.backend.Lookup(ctx, h.convID, hashes)
}
