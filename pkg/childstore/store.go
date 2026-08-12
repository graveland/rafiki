package childstore

import (
	"errors"
	"sort"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/ring"
)

// ErrNotFound is returned when the requested ChildID does not exist.
var ErrNotFound = errors.New("session not found")

// LabelParent is the label key holding the parent child's ID.
const LabelParent = "rafiki/parent"

// LabelRoot is the label key holding the root (top-level parent) child's ID.
const LabelRoot = "rafiki/root"

// Store is a concurrent, indexed collection of Sessions.
//
// Primary storage is a flat map keyed by ChildID. Secondary indexes (byName,
// byCwd, byStatus) each hold a set of ChildIDs per key, allowing many-to-many
// lookups. All lookups use verify-on-read: the index yields candidates, but the
// primary map is the source of truth.
//
// Ordering invariants:
//   - Insert: primary first, then index entries.
//   - Delete: index entries first, then primary.
//   - Rename / SetStatus: remove-old-index, update primary, add-new-index (primary
//     update is the pivot so verify-on-read stays consistent).
type Store struct {
	sessions *xsync.Map[string, *Session]
	byName   *xsync.Map[string, *xsync.Map[string, struct{}]]
	byCwd    *xsync.Map[string, *xsync.Map[string, struct{}]]
	byStatus *xsync.Map[protocol.Status, *xsync.Map[string, struct{}]]
}

// New returns a ready-to-use Store.
func New() *Store {
	return &Store{
		sessions: xsync.NewMap[string, *Session](),
		byName:   xsync.NewMap[string, *xsync.Map[string, struct{}]](),
		byCwd:    xsync.NewMap[string, *xsync.Map[string, struct{}]](),
		byStatus: xsync.NewMap[protocol.Status, *xsync.Map[string, struct{}]](),
	}
}

// Insert adds sess to the store. The caller must not mutate sess after
// calling Insert; use the Store's mutation methods.
//
// Ordering: primary stored first, then index entries.
func (s *Store) Insert(sess *Session) {
	s.sessions.Store(sess.ChildID, sess)
	if sess.Name != "" {
		addToBucket(s.byName, sess.Name, sess.ChildID)
	}
	if sess.Cwd != "" {
		addToBucket(s.byCwd, sess.Cwd, sess.ChildID)
	}
	addToBucket(s.byStatus, sess.Status, sess.ChildID)
}

// Delete removes the session with the given id.
//
// Ordering: index entries removed first, then primary.
func (s *Store) Delete(id string) {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return
	}
	snap := sess.Snapshot()
	if snap.Name != "" {
		removeFromBucket(s.byName, snap.Name, id)
	}
	if snap.Cwd != "" {
		removeFromBucket(s.byCwd, snap.Cwd, id)
	}
	removeFromBucket(s.byStatus, snap.Status, id)
	s.sessions.Delete(id)
}

// Get returns a Snapshot for the given id.
func (s *Store) Get(id string) (Snapshot, bool) {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return Snapshot{}, false
	}
	return sess.Snapshot(), true
}

// List returns snapshots for all sessions, sorted by StartedAt descending
// (most recently started first).
func (s *Store) List() []Snapshot {
	var out []Snapshot
	s.sessions.Range(func(_ string, sess *Session) bool {
		out = append(out, sess.Snapshot())
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// FindByName returns snapshots for all sessions with the given name.
// Verify-on-read filters any stale index entries.
func (s *Store) FindByName(name string) []Snapshot {
	return lookupBucket(s.byName, s.sessions, name, func(snap Snapshot) bool {
		return snap.Name == name
	})
}

// FindByCwd returns snapshots for all sessions whose working directory matches.
// Verify-on-read filters any stale index entries.
func (s *Store) FindByCwd(cwd string) []Snapshot {
	return lookupBucket(s.byCwd, s.sessions, cwd, func(snap Snapshot) bool {
		return snap.Cwd == cwd
	})
}

// FindByStatus returns snapshots for all sessions in the given status.
// Verify-on-read filters any stale index entries.
func (s *Store) FindByStatus(status protocol.Status) []Snapshot {
	return lookupBucket(s.byStatus, s.sessions, status, func(snap Snapshot) bool {
		return snap.Status == status
	})
}


// Update applies fn to the session under its lock. The caller is responsible
// for keeping index entries in sync if fn mutates indexed fields (Name, Cwd,
// Status). Prefer the dedicated Rename / SetStatus methods for indexed mutations.
//
// fn must not invoke Store mutation methods on the same session id —
// fn is called while sess.mu is held, and sync.Mutex is not reentrant.
// Use Rename / SetStatus / direct method calls outside of an Update closure.
func (s *Store) Update(id string, fn func(*Session)) error {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return ErrNotFound
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	fn(sess)
	return nil
}

// Rename changes the name of the session and updates the name index atomically.
//
// The pivot pattern keeps verify-on-read consistent: remove the old index
// entry, update the primary record, add the new index entry. A reader that
// hits the window between remove-old and add-new will find no entry for this
// session under either name — acceptable because index entries are advisory
// and the primary is always the source of truth.
func (s *Store) Rename(id, newName string) error {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return ErrNotFound
	}

	sess.mu.Lock()
	old := sess.Name
	sess.mu.Unlock()

	if old == newName {
		return nil
	}

	if old != "" {
		removeFromBucket(s.byName, old, id)
	}
	sess.mu.Lock()
	sess.Name = newName
	sess.mu.Unlock()
	if newName != "" {
		addToBucket(s.byName, newName, id)
	}
	return nil
}

// SetStatus transitions the session to newStatus and returns the previous status.
// Returns (prev, true) on success or ("", false) if the id is not found.
//
// Uses the same pivot pattern as Rename: remove old index entry, update
// primary, add new index entry.
func (s *Store) SetStatus(id string, newStatus protocol.Status) (prev protocol.Status, ok bool) {
	sess, found := s.sessions.Load(id)
	if !found {
		return "", false
	}

	sess.mu.Lock()
	prev = sess.Status
	sess.mu.Unlock()

	if prev == newStatus {
		return prev, true
	}

	removeFromBucket(s.byStatus, prev, id)
	sess.mu.Lock()
	sess.Status = newStatus
	sess.mu.Unlock()
	addToBucket(s.byStatus, newStatus, id)

	return prev, true
}

// SetLabels atomically applies a set/remove mutation to the session's label map.
// Set entries are merged, then Remove entries are deleted. Returns the full
// post-mutation labels as a defensive copy, or ErrNotFound when id is absent.
func (s *Store) SetLabels(id string, set map[string]string, remove []string) (map[string]string, error) {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return nil, ErrNotFound
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Labels == nil && (len(set) > 0) {
		sess.Labels = make(map[string]string, len(set))
	}
	for _, k := range remove {
		delete(sess.Labels, k)
	}
	for k, v := range set {
		if sess.Labels == nil {
			sess.Labels = make(map[string]string)
		}
		sess.Labels[k] = v
	}
	return copyLabels(sess.Labels), nil
}

// MarkExited atomically transitions the session to StatusExited and records
// exit details. The status-index update and field mutations happen under a
// single sess.mu hold so a concurrent Snapshot() cannot observe Status=Exited
// with ExitedRing still nil — the race that occurs if SetStatus and Update are
// called as separate operations.
func (s *Store) MarkExited(id string, exitedAt time.Time, exitCode int, exitSignal string, exitedRing []ring.Event, exitedRenderRing []ring.Event) bool {
	sess, found := s.sessions.Load(id)
	if !found {
		return false
	}

	sess.mu.Lock()
	prev := sess.Status
	sess.Status = protocol.StatusExited
	sess.ExitedAt = exitedAt
	code := exitCode
	sess.ExitCode = &code
	sess.ExitSignal = exitSignal
	sess.ExitedRing = exitedRing
	sess.ExitedRenderRing = exitedRenderRing
	sess.mu.Unlock()

	if prev != protocol.StatusExited {
		removeFromBucket(s.byStatus, prev, id)
		addToBucket(s.byStatus, protocol.StatusExited, id)
	}
	return true
}

// ─── generic bucket helpers ──────────────────────────────────────────────────

// addToBucket inserts id into the set stored under key in idx, creating the
// set if it doesn't exist yet.
func addToBucket[K comparable](idx *xsync.Map[K, *xsync.Map[string, struct{}]], key K, id string) {
	bucket, _ := idx.LoadOrCompute(key, func() (*xsync.Map[string, struct{}], bool) {
		return xsync.NewMap[string, struct{}](), false
	})
	bucket.Store(id, struct{}{})
}

// removeFromBucket deletes id from the set stored under key in idx.
// A missing key or missing id is a no-op.
func removeFromBucket[K comparable](idx *xsync.Map[K, *xsync.Map[string, struct{}]], key K, id string) {
	if bucket, ok := idx.Load(key); ok {
		bucket.Delete(id)
	}
}

// lookupBucket returns snapshots for all IDs in the set under key. Each
// candidate is verified against the primary map (verify-on-read): if the
// snapshot doesn't satisfy the verify predicate, it's filtered out.
func lookupBucket[K comparable](
	idx *xsync.Map[K, *xsync.Map[string, struct{}]],
	primary *xsync.Map[string, *Session],
	key K,
	verify func(Snapshot) bool,
) []Snapshot {
	bucket, ok := idx.Load(key)
	if !ok {
		return nil
	}
	var out []Snapshot
	bucket.Range(func(id string, _ struct{}) bool {
		if sess, ok := primary.Load(id); ok {
			snap := sess.Snapshot()
			if verify(snap) {
				out = append(out, snap)
			}
		}
		return true
	})
	return out
}
