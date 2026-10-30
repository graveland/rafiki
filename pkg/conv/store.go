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

// providerRef is a provider message id's index entry: the newest node this entry
// has seen carrying the id, and that node's ordinal. id is empty once that node
// has been evicted, which keeps the ordinal — the high-water mark that stops a
// re-fetched older duplicate from being answered — without pointing at a node
// the cache no longer holds.
type providerRef struct {
	id      string
	ordinal int
}

// entry is the per-conversation cache state.
type entry struct {
	// head is the conversation's position as this process last saw it. A write
	// stores the new state only after its rows are in the LRU, so a reader that
	// observes the new head always finds its nodes.
	head atomic.Pointer[headState]
	// writeMu serialises Append and RetractHead on this conversation.
	writeMu sync.Mutex
	// owner, loaded, lastAccess, gen, lease, dead and byProvider are guarded by
	// Store.mu.
	owner      bool
	loaded     bool
	lastAccess time.Time
	// dead is set when the sweep drops this entry, and nothing clears it: a
	// swept entry is dead for good, so no path may publish into it or install it
	// as the conversation's entry again.
	dead bool
	// gen counts the writes this entry has published. A read that asked the
	// backend at generation g may store its answer only while the generation is
	// still g, which is what orders a read against a write that moved the head
	// WITHOUT advancing the ordinal (a retract, or a head-only move).
	gen uint64
	// lease is the fence key of the last handle that resolved the entry, so a
	// takeover invalidates a head the previous holder left behind.
	lease string
	// byProvider maps a provider message id to the newest node this entry has
	// seen carrying it, so an owner lookup that is already cached costs no
	// query. It is an optimisation for the owner, never a source of truth.
	byProvider map[string]providerRef
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
// default, including a negative CacheBytes, which cannot mean "cache nothing":
// a nil Now is time.Now.
func NewStore(b Backend, o Options) *Store {
	if o.CacheBytes <= 0 {
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

// leaseKey identifies the fence a handle writes under, "" when unfenced.
func (h *Handle) leaseKey() string {
	if !h.lease.Held() {
		return ""
	}
	return h.lease.ConversationID + "|" + h.lease.Holder + "|" + h.lease.Token
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
// records the handle's authority. Entries idle past the TTL are swept here and
// on every Open, which is the whole of the expiry mechanism: no goroutines, no
// timers.
//
// Becoming the owner, or arriving under a different fence, invalidates the
// cached head: a head a reader loaded before the takeover must not be trusted
// by the writer that took over.
func (s *Store) touch(convID string, owner bool, lease string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.entries[convID]
	if e == nil {
		e = &entry{byProvider: make(map[string]providerRef)}
		s.entries[convID] = e
	}
	if owner && !e.owner {
		e.owner = true
		e.loaded = false
	}
	if lease != "" && lease != e.lease {
		e.lease = lease
		e.loaded = false
	}
	e.lastAccess = s.now()
	return e
}

// lockEntry resolves convID's entry and takes its write lock, re-resolving
// after the lock so a writer never holds the lock of an entry that was swept
// and replaced. Entry creation happens only under Store.mu, so there is one
// entry — and therefore one writeMu — per conversation.
func (h *Handle) lockEntry() *entry {
	for {
		e := h.store.touch(h.convID, h.owner, h.leaseKey())
		e.writeMu.Lock()
		if h.store.isLive(h.convID, e) {
			return e
		}
		e.writeMu.Unlock()
	}
}

// isLive reports whether e is still the conversation's entry.
func (s *Store) isLive(convID string, e *entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveEntryLocked(convID, e) != nil
}

// sweepLocked drops entries that have been idle past the TTL. Dropping one kills
// it: dead is what tells a handle still holding it that its state is no longer
// the conversation's.
func (s *Store) sweepLocked() {
	now := s.now()
	for id, e := range s.entries {
		if now.Sub(e.lastAccess) > s.ttl {
			e.dead = true
			delete(s.entries, id)
		}
	}
}

// liveEntryLocked returns e while it is still convID's current entry, and nil
// once it is not. This is the one liveness test every publish path makes: an
// entry the sweep dropped is dead for good, so its value is DROPPED rather than
// installed into whatever entry the conversation has now. Installing it would
// publish a head older than the one already committed, and a read built from
// that head is wrong before any compare-and-swap runs.
func (s *Store) liveEntryLocked(convID string, e *entry) *entry {
	if e == nil || e.dead || s.entries[convID] != e {
		return nil
	}
	return e
}

// cachedHead returns the entry's position when it may be trusted: only an owner
// handle reading a live owner entry whose head has been loaded answers without
// the backend. A swept entry answers nothing, so its last known head cannot
// outlive it.
func (s *Store) cachedHead(convID string, e *entry, owner bool) (headState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveEntryLocked(convID, e)
	if live == nil || !owner || !live.owner || !live.loaded {
		return headState{}, false
	}
	hp := live.head.Load()
	if hp == nil {
		return headState{}, false
	}
	return *hp, true
}

// generation reads the entry's write generation, which a read captures before
// asking the backend and hands back to storeHeadRead. ok is false once the entry
// has been swept: the read's answer belongs to an entry that is no longer the
// conversation's, so it has nowhere to go.
func (s *Store) generation(convID string, e *entry) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveEntryLocked(convID, e)
	if live == nil {
		return 0, false
	}
	return live.gen, true
}

// storeHeadRead stores a state a READ observed, but only into the entry it
// observed it from, and only while no write has published since the read
// captured its generation. A read that raced a retract or a head-only move
// carries the same next ordinal as the state the writer published, so the
// generation — not the ordinal — is what orders them; entry identity is what
// stops a stalled read landing in the entry that replaced its own, where a
// generation that has since caught up would compare equal.
func (s *Store) storeHeadRead(convID string, e *entry, gen uint64, st headState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveEntryLocked(convID, e)
	if live == nil || live.gen != gen {
		return
	}
	live.loaded = true
	live.lastAccess = s.now()
	live.head.Store(&st)
}

// storeHeadWrite publishes a state a WRITER resolved or committed, advancing
// the generation so every read in flight against the old position is dropped. A
// writer whose entry was swept while it was committing publishes nothing: the
// next reader resolves a fresh entry and reads the backend, which is the head
// the commit left.
func (s *Store) storeHeadWrite(convID string, e *entry, st headState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveEntryLocked(convID, e)
	if live == nil {
		return
	}
	live.gen++
	live.loaded = true
	live.lastAccess = s.now()
	live.head.Store(&st)
}

// lookupLocked returns a cached node of THIS conversation. A node id from
// another conversation is a miss, never a hit: the id space is global but a
// conversation's tree is not.
func (s *Store) lookupLocked(convID, id string) (*Node, bool) {
	el, ok := s.nodes[id]
	if !ok {
		return nil, false
	}
	n := el.Value.(*Node)
	if n.ConversationID != convID {
		return nil, false
	}
	return n, true
}

// touchLocked marks a cached node hot.
func (s *Store) touchLocked(id string) {
	if el, ok := s.nodes[id]; ok {
		s.lru.MoveToFront(el)
	}
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
		// The newest ordinal wins: a re-fetched older node must not displace
		// the reply a resume is looking for, and it must not even lower the mark
		// of a newer node that has since been evicted.
		if cur, ok := e.byProvider[n.ProviderMessageID]; !ok || n.Ordinal >= cur.ordinal {
			e.byProvider[n.ProviderMessageID] = providerRef{id: n.ID, ordinal: n.Ordinal}
		}
	}
	for s.charged > s.cacheBytes && s.lru.Len() > 0 {
		s.evictLocked()
	}
}

// evictLocked drops the coldest node. Eviction costs no reader anything:
// snapshots hold node references, so a reader mid-iteration is unaffected, and
// an evicted node is re-fetched through Walk + Fetch. Only the provider index
// notices, and only by forgetting which node held the id.
func (s *Store) evictLocked() {
	el := s.lru.Back()
	if el == nil {
		return
	}
	n := el.Value.(*Node)
	s.lru.Remove(el)
	delete(s.nodes, n.ID)
	s.charged -= int64(len(n.Content))
	if e := s.entries[n.ConversationID]; e != nil && n.ProviderMessageID != "" {
		if cur, ok := e.byProvider[n.ProviderMessageID]; ok && cur.id == n.ID {
			// The ordinal mark stays: an older duplicate re-fetched after this
			// node was evicted must still be known to be older than the newest
			// this entry has seen.
			e.byProvider[n.ProviderMessageID] = providerRef{ordinal: cur.ordinal}
		}
	}
}

// putNodes caches nodes of convID and keeps the entry warm. A node belonging to
// another conversation is never adopted.
func (s *Store) putNodes(convID string, nodes ...*Node) {
	if len(nodes) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, n := range nodes {
		if n == nil || n.ConversationID != convID {
			continue
		}
		s.putLocked(n)
		if e := s.entries[convID]; e != nil {
			e.lastAccess = now
		}
	}
}

// cachedNode returns one cached node of convID.
func (s *Store) cachedNode(convID, id string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.lookupLocked(convID, id)
	if ok {
		s.touchLocked(id)
	}
	return n, ok
}

// cachedPath walks from toward the root entirely out of the cache. ok is false
// on the first miss, which is what sends the caller to the backend.
//
// Recency is applied afterwards, root first and head last, so the head — the
// node every read starts at — ends hottest and eviction drops the oldest part
// of the history rather than the part a reader needs next.
func (s *Store) cachedPath(convID, from string, crossRoots bool) ([]*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rev []*Node
	for id := from; id != ""; {
		n, ok := s.lookupLocked(convID, id)
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
	for i := len(rev) - 1; i >= 0; i-- {
		s.touchLocked(rev[i].ID)
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
	if path, ok := h.store.cachedPath(h.convID, from, crossRoots); ok {
		return path, nil
	}
	refs, err := h.store.backend.Walk(ctx, h.convID, from, crossRoots)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		// from is not "" here, so a walk that reached nothing names a node this
		// conversation does not hold — another conversation's, or none.
		return nil, fmt.Errorf("conv: walk: node %s not in conversation %s", from, h.convID)
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
		if n, ok := s.lookupLocked(convID, r.ID); ok {
			out[i] = n
			continue
		}
		missing = append(missing, r.ID)
	}
	for i := len(refs) - 1; i >= 0; i-- {
		if out[i] != nil {
			s.touchLocked(refs[i].ID)
		}
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
		if n.ConversationID == convID {
			byID[n.ID] = n
		}
	}
	s.putNodes(convID, fetched...)
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
	if n, ok := s.cachedNode(convID, id); ok {
		return n.Ref, nil
	}
	fetched, err := s.backend.Fetch(ctx, convID, []string{id})
	if err != nil {
		return Ref{}, fmt.Errorf("read node %s: %w", id, err)
	}
	for _, n := range fetched {
		if n.ID == id && n.ConversationID == convID {
			s.putNodes(convID, n)
			return n.Ref, nil
		}
	}
	return Ref{}, fmt.Errorf("node %s not in conversation %s", id, convID)
}

// node returns one node of convID, from the cache or the backend, and nil when
// the conversation does not hold it.
func (s *Store) node(ctx context.Context, convID, id string) (*Node, error) {
	if n, ok := s.cachedNode(convID, id); ok {
		return n, nil
	}
	fetched, err := s.backend.Fetch(ctx, convID, []string{id})
	if err != nil {
		return nil, fmt.Errorf("read node %s: %w", id, err)
	}
	for _, n := range fetched {
		if n.ID == id && n.ConversationID == convID {
			s.putNodes(convID, n)
			return n, nil
		}
	}
	return nil, nil
}

// readState resolves the conversation's position for a read. An owner entry
// whose head is loaded answers from memory; anything else asks the backend
// once and stores what it said — unless a write published in between, which is
// also what corrects an entry that is ahead of the backend.
func (h *Handle) readState(ctx context.Context, e *entry) (State, error) {
	if st, ok := h.store.cachedHead(h.convID, e, h.owner); ok {
		return State{HeadID: st.id, NextOrdinal: st.nextOrdinal}, nil
	}
	gen, live := h.store.generation(h.convID, e)
	st, err := h.store.backend.State(ctx, h.convID)
	if err != nil {
		return State{}, err
	}
	if live {
		h.store.storeHeadRead(h.convID, e, gen, headState{id: st.HeadID, nextOrdinal: st.NextOrdinal})
	}
	return st, nil
}

// State returns the conversation's position, always from the backend: it is
// the answer a caller asks for precisely when it does not trust its cache.
func (h *Handle) State(ctx context.Context) (State, error) {
	e := h.store.touch(h.convID, h.owner, h.leaseKey())
	gen, live := h.store.generation(h.convID, e)
	st, err := h.store.backend.State(ctx, h.convID)
	if err != nil {
		return State{}, err
	}
	if live {
		h.store.storeHeadRead(h.convID, e, gen, headState{id: st.HeadID, nextOrdinal: st.NextOrdinal})
	}
	return st, nil
}

// Working returns the working path: the chain from the head to the nearest
// root, chronological.
func (h *Handle) Working(ctx context.Context) ([]*Node, error) {
	e := h.store.touch(h.convID, h.owner, h.leaseKey())
	st, err := h.readState(ctx, e)
	if err != nil {
		return nil, err
	}
	return h.pathFrom(ctx, st.HeadID, false)
}

// Snapshot returns the conversation's whole tree, crossing every root.
func (h *Handle) Snapshot(ctx context.Context) (*Snapshot, error) {
	e := h.store.touch(h.convID, h.owner, h.leaseKey())
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
// caller names the head it is replaying, which must belong to this
// conversation.
func (h *Handle) RequestAt(ctx context.Context, headID string, trimFrom, trimTo *int) ([]*Node, error) {
	h.store.touch(h.convID, h.owner, h.leaseKey())
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

// NodeByProviderMessageID returns the node an upstream message id names.
//
// The conversation's own index is an OWNER-only optimisation, exactly like the
// cached head: only an owner handle on an owner entry answers from it, and only
// while the index still holds the newest node it has seen for that id. Every
// other reader — capture is a reader by design — asks the backend, whose answer
// is the newest ordinal. That matters because a provider id is not unique: an
// index that answered whatever it happened to hold could name an older duplicate
// than the backend would, and capture uses this node as previous_message_id.
func (h *Handle) NodeByProviderMessageID(ctx context.Context, providerMessageID string) (*Node, bool, error) {
	e := h.store.touch(h.convID, h.owner, h.leaseKey())
	if providerMessageID != "" {
		if ref, ok := h.store.indexedProvider(h.convID, e, h.owner, providerMessageID); ok {
			n, err := h.store.node(ctx, h.convID, ref.id)
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
	h.store.putNodes(h.convID, n)
	return n, true, nil
}

// indexedProvider returns the index entry for providerMessageID when it may be
// trusted: an owner handle on a live owner entry, and a node the index still
// holds as the newest it has seen for that id. An entry whose newest node was
// evicted keeps the ordinal but no id, so this reports a miss and the caller
// asks the backend rather than answering with an older duplicate.
func (s *Store) indexedProvider(convID string, e *entry, owner bool, providerMessageID string) (providerRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveEntryLocked(convID, e)
	if live == nil || !owner || !live.owner {
		return providerRef{}, false
	}
	ref, ok := live.byProvider[providerMessageID]
	if !ok || ref.id == "" {
		return providerRef{}, false
	}
	return ref, true
}

// Roots returns the conversation's parent-less nodes, newest ordinal first.
func (h *Handle) Roots(ctx context.Context) ([]*Node, error) {
	h.store.touch(h.convID, h.owner, h.leaseKey())
	roots, err := h.store.backend.Roots(ctx, h.convID)
	if err != nil {
		return nil, err
	}
	h.store.putNodes(h.convID, roots...)
	return roots, nil
}

// NodesByHash returns the refs of the nodes the conversation holds under
// hashes.
func (h *Handle) NodesByHash(ctx context.Context, hashes []convtree.Hash) (map[convtree.Hash]Ref, error) {
	h.store.touch(h.convID, h.owner, h.leaseKey())
	return h.store.backend.Lookup(ctx, h.convID, hashes)
}
