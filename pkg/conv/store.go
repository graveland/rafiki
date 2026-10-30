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

// headState is one observation of a conversation's position, stamped with the
// version of the entry it was installed into. A state with cached unset is an
// invalidation: it carries a version so an in-flight install loses to it, but no
// position.
type headState struct {
	version     int64
	cached      bool
	id          string
	nextOrdinal int
	at          time.Time
}

// ver is the entry's version: 0 until the first install or invalidation.
func (h *headState) ver() int64 {
	if h == nil {
		return 0
	}
	return h.version
}

// entry is the per-conversation cache state.
type entry struct {
	// writeMu serialises Append and RetractHead while this entry is the
	// conversation's.
	writeMu sync.Mutex
	// head is the entry's cached position and its version in one pointer, so an
	// install is a single compare-and-swap against the exact state an update
	// started from: there is no window between winning the version and storing
	// the position. nil means nothing was ever installed.
	head atomic.Pointer[headState]
	// lastAccess and dead are guarded by Store.mu.
	lastAccess time.Time
	// dead is set when the sweep drops this entry, and nothing clears it: a
	// swept entry is dead for good, so no path may install it as the
	// conversation's entry again.
	dead bool
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
	lease Lease
	owner bool
}

// OpenOption customises a Handle.
type OpenOption func(*openConfig)

// AsOwner marks the handle as this process's writer for the conversation, which
// is what lets its reads serve the cached head: the writer's own commits refresh
// it, so a warm owner read makes no backend call. A handle without it asks the
// backend for the head on every read, because the conversation's writer may be
// another process.
func AsOwner() OpenOption {
	return func(c *openConfig) { c.owner = true }
}

// WithLease fences the handle's writes on l.
func WithLease(l Lease) OpenOption {
	return func(c *openConfig) { c.lease = l }
}

// Handle is a conversation descriptor. It holds no cache state of its own:
// every call resolves (or recreates) the conversation's entry, so a swept entry
// costs a lock, never a stale answer.
type Handle struct {
	store  *Store
	convID string
	lease  Lease
	owner  bool
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
	return &Handle{store: s, convID: convID, lease: cfg.lease, owner: cfg.owner}
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

// touch resolves convID's entry, creating it when absent, and marks it live.
// Entries idle past the TTL are swept here and on every Open, which is the whole
// of the expiry mechanism: no goroutines, no timers.
func (s *Store) touch(convID string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.entries[convID]
	if e == nil {
		e = &entry{}
		s.entries[convID] = e
	}
	e.lastAccess = s.now()
	return e
}

// lockEntry resolves convID's entry and takes its write lock, re-resolving
// after the lock so a writer never holds the lock of an entry that was swept
// and replaced. Creation happens only under Store.mu, but that does not make the
// lock unique: a held writeMu can still be swept, so two writers may briefly
// hold the locks of two entries of the same conversation. What keeps that safe
// is the commit's compare-and-swap, which refuses the second writer.
func (h *Handle) lockEntry() *entry {
	for {
		e := h.store.touch(h.convID)
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
	return !e.dead && s.entries[convID] == e
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
// cold end past the budget.
func (s *Store) putLocked(n *Node) {
	if _, ok := s.nodes[n.ID]; ok {
		return
	}
	s.nodes[n.ID] = s.lru.PushFront(n)
	s.charged += int64(len(n.Content))
	for s.charged > s.cacheBytes && s.lru.Len() > 0 {
		s.evictLocked()
	}
}

// evictLocked drops the coldest node. Eviction costs no reader anything:
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

// State returns the conversation's position, always from the backend and never
// through the cached head: it carries ClearPending, which is not cached.
func (h *Handle) State(ctx context.Context) (State, error) {
	h.store.touch(h.convID)
	return h.store.backend.State(ctx, h.convID)
}

// headID returns the conversation's head: for an owner, the entry's cached head
// while it is fresh; otherwise the answer of one update, which also leaves the
// entry holding what it read.
func (h *Handle) headID(ctx context.Context) (string, error) {
	e := h.store.touch(h.convID)
	if hs := e.head.Load(); h.owner && hs != nil && hs.cached && h.store.now().Sub(hs.at) <= h.store.ttl {
		return hs.id, nil
	}
	st, err := h.store.refresh(ctx, h.convID)
	if err != nil {
		return "", err
	}
	return st.HeadID, nil
}

// refresh installs what the backend says the conversation's position is now.
func (s *Store) refresh(ctx context.Context, convID string) (State, error) {
	return s.update(ctx, convID, func(_ *entry, cur State) (State, error) { return cur, nil })
}

// refreshAfterCommit is what a write does once its commit is durable. The write
// has happened whether or not the backend can answer now, so a failed refresh is
// absorbed by invalidating the cached head: the next read asks the backend.
func (s *Store) refreshAfterCommit(ctx context.Context, convID string) {
	if _, err := s.refresh(ctx, convID); err != nil {
		s.invalidate(convID)
	}
}

// update is the only place a cached head is installed. It reads the backend's
// State once per iteration, applies fn to it and installs the result with one
// compare-and-swap against the entry state the iteration began from; a lost swap
// or a dead entry restarts the loop with a fresh read, so fn never sees a value
// from an earlier iteration and nothing but a backend read in the same iteration
// is ever installed.
//
// A write never installs its own commit result. It calls update with an identity
// fn (a refresh), so "is my commit newer than yours" is never asked: the cache
// only ever holds a state some update read from the backend and won the swap for.
//
// How each stale-read finding is impossible:
//
//	F1/R1  a delayed publication installs an older head: a write refreshes
//	       (reads the backend) instead of publishing its commit result.
//	R3     an empty entry never learns, an in-flight read installs the
//	       pre-commit head: the install is a swap against the state the read
//	       began from, every install and invalidation replaces that state, so a
//	       read that began before the commit's refresh loses.
//	R4     a writer's stale State observation overwrites a newer commit: writers
//	       install nothing but through update's swap.
//	R5/R6  a delayed commit outranks a newer one; active reads keep the stale
//	       entry warm: no commit result is ever installed, and an installed head
//	       older than the idle TTL is refreshed whatever touched the entry since.
//
// The version orders this process's observations only. Another daemon's commits
// are not seen until the cached head is older than the TTL.
func (s *Store) update(ctx context.Context, convID string, fn func(e *entry, cur State) (State, error)) (State, error) {
	for {
		e := s.touch(convID)
		old := e.head.Load()
		cur, err := s.backend.State(ctx, convID)
		if err != nil {
			return State{}, err
		}
		next, err := fn(e, cur)
		if err != nil {
			return State{}, err
		}
		if s.install(convID, e, old, next) {
			return next, nil
		}
	}
}

// install swaps e's head from old to next, and fails when e is no longer the
// conversation's entry or its head is no longer old. The identity check and the
// swap share Store.mu with the sweep, so a dead entry can never win.
func (s *Store) install(convID string, e *entry, old *headState, next State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.dead || s.entries[convID] != e {
		return false
	}
	return e.head.CompareAndSwap(old, &headState{
		version:     old.ver() + 1,
		cached:      true,
		id:          next.HeadID,
		nextOrdinal: next.NextOrdinal,
		at:          s.now(),
	})
}

// invalidate drops the conversation's cached position without installing one:
// the replacement state carries a newer version, so an update that began before
// it loses its swap.
func (s *Store) invalidate(convID string) {
	s.mu.Lock()
	e := s.entries[convID]
	s.mu.Unlock()
	if e == nil {
		return
	}
	for {
		old := e.head.Load()
		if e.head.CompareAndSwap(old, &headState{version: old.ver() + 1}) {
			return
		}
	}
}

// Working returns the working path: the chain from the head to the nearest
// root, chronological.
func (h *Handle) Working(ctx context.Context) ([]*Node, error) {
	head, err := h.headID(ctx)
	if err != nil {
		return nil, err
	}
	return h.pathFrom(ctx, head, false)
}

// Snapshot returns the conversation's whole tree, crossing every root.
func (h *Handle) Snapshot(ctx context.Context) (*Snapshot, error) {
	head, err := h.headID(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := h.pathFrom(ctx, head, true)
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
	h.store.touch(h.convID)
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

// NodeByProviderMessageID returns the newest node an upstream message id names,
// always from the backend.
//
// A provider message id is not unique — `ConvBackend.ByProviderMessageID` orders
// `ordinal DESC LIMIT 1` — and the cache cannot know which duplicates exist, so
// it answers nothing from memory: a cached node is only the newest among the
// nodes this process happens to hold. Capture uses this node as
// `previous_message_id`, so a cheaper answer is not worth a wrong attachment.
func (h *Handle) NodeByProviderMessageID(ctx context.Context, providerMessageID string) (*Node, bool, error) {
	h.store.touch(h.convID)
	n, ok, err := h.store.backend.ByProviderMessageID(ctx, h.convID, providerMessageID)
	if err != nil || !ok {
		return nil, ok, err
	}
	h.store.putNodes(h.convID, n)
	return n, true, nil
}

// Roots returns the conversation's parent-less nodes, newest ordinal first.
func (h *Handle) Roots(ctx context.Context) ([]*Node, error) {
	h.store.touch(h.convID)
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
	h.store.touch(h.convID)
	return h.store.backend.Lookup(ctx, h.convID, hashes)
}
