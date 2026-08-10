package tasks

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// NewMemoryStore returns an in-memory Store. It works without a database —
// state is lost on restart, which matches the old todo tool's behaviour.
func NewMemoryStore() Store {
	return &memoryStore{
		rows:     make(map[string]*Task),
		counters: make(map[string]int),
	}
}

type memoryStore struct {
	mu       sync.Mutex
	rows     map[string]*Task // id → task
	counters map[string]int   // "convID|parentID" → next ordinal
}

func (ms *memoryStore) counterKey(convID, parentID string) string {
	return convID + "|" + parentID
}

func (ms *memoryStore) nextOrdinal(convID, parentID string) int {
	k := ms.counterKey(convID, parentID)
	ms.counters[k]++
	return ms.counters[k]
}

func (ms *memoryStore) resolve(convID, handle string) (*Task, error) {
	if handle == "" {
		return nil, fmt.Errorf("%w: empty handle", ErrNotFound)
	}
	parts := strings.Split(handle, ".")
	// Build list of ordinals.
	ords := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, handle)
		}
		ords[i] = n
	}

	// Walk from root, matching ordinals at each level.
	var parentID string
	for depth, ord := range ords {
		found := false
		for _, t := range ms.rows {
			if t.ConversationID != convID {
				continue
			}
			if t.Ordinal != ord {
				continue
			}
			if (parentID == "" && t.ParentID == "") || (parentID != "" && t.ParentID == parentID) {
				if depth == len(ords)-1 {
					return t, nil
				}
				parentID = t.ID
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, handle)
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, handle)
}

func (ms *memoryStore) descendants(rootID string) []*Task {
	var out []*Task
	var walk func(pid string)
	walk = func(pid string) {
		for _, t := range ms.rows {
			if t.ParentID == pid {
				out = append(out, t)
				walk(t.ID)
			}
		}
	}
	walk(rootID)
	return out
}

func (ms *memoryStore) Add(ctx context.Context, convID, parentHandle string, items []NewTask) ([]Task, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var parentID string
	if parentHandle != "" {
		p, err := ms.resolve(convID, parentHandle)
		if err != nil {
			return nil, err
		}
		parentID = p.ID
	}

	addedIDs := make(map[string]bool, len(items))
	for _, item := range items {
		id := uuid.Must(uuid.NewV7()).String()
		addedIDs[id] = true
		t := &Task{
			ID:             id,
			ConversationID: convID,
			ParentID:       parentID,
			Ordinal:        ms.nextOrdinal(convID, parentID),
			Content:        item.Content,
			ActiveForm:     item.ActiveForm,
			Status:         StatusPending,
			Metadata:       item.Metadata,
		}
		if t.Metadata == nil {
			t.Metadata = map[string]string{}
		}
		ms.rows[id] = t
	}

	// Return only added tasks, with handles computed from the full tree.
	all := AssignHandles(ms.listLocked(convID))
	var out []Task
	for _, t := range all {
		if addedIDs[t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}

func (ms *memoryStore) Update(ctx context.Context, convID string, changes []Change) ([]Task, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, ch := range changes {
		if !ch.Status.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, ch.Status)
		}
		t, err := ms.resolve(convID, ch.Handle)
		if err != nil {
			return nil, err
		}
		t.Status = ch.Status
	}

	return AssignHandles(ms.listLocked(convID)), nil
}

func (ms *memoryStore) Drop(ctx context.Context, convID, handle, reason string) ([]Task, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if reason == "" {
		return nil, ErrDropReason
	}

	t, err := ms.resolve(convID, handle)
	if err != nil {
		return nil, err
	}

	// Collect the whole subtree.
	subtree := append([]*Task{t}, ms.descendants(t.ID)...)

	// Check for any live assignee.
	for _, st := range subtree {
		if st.Assignee != "" {
			return nil, fmt.Errorf("%w: %s", ErrAssigned, st.Assignee)
		}
	}

	// Drop every non-terminal node.
	for _, st := range subtree {
		if !st.Status.Terminal() {
			st.Status = StatusDropped
			st.DropReason = reason
		}
	}

	return AssignHandles(ms.listLocked(convID)), nil
}

func (ms *memoryStore) List(ctx context.Context, f ListFilter) ([]Task, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	filtered := ms.listLocked(f.ConversationID)
	filtered = slices.DeleteFunc(filtered, func(t Task) bool {
		if f.Assignee != "" && t.Assignee != f.Assignee {
			return true
		}
		if f.Status != "" && t.Status != f.Status {
			return true
		}
		if !f.IncludeDropped && t.Status == StatusDropped {
			return true
		}
		for k, v := range f.Metadata {
			if t.Metadata[k] != v {
				return true
			}
		}
		return false
	})

	return AssignHandles(filtered), nil
}

func (ms *memoryStore) Assign(ctx context.Context, convID, handle, assignee string) (Task, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Task{}, err
	}

	t, err := ms.resolve(convID, handle)
	if err != nil {
		return Task{}, err
	}
	t.Assignee = assignee
	return *t, nil
}

func (ms *memoryStore) OrphanAssigned(ctx context.Context, assignee string) (int, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, t := range ms.rows {
		if t.Assignee == assignee &&
			(t.Status == StatusPending || t.Status == StatusInProgress || t.Status == StatusBlocked) {
			t.Status = StatusOrphaned
			count++
		}
	}
	return count, nil
}

// listLocked returns all tasks for convID, unsorted. Caller must hold mu.
func (ms *memoryStore) listLocked(convID string) []Task {
	var out []Task
	for _, t := range ms.rows {
		if t.ConversationID == convID {
			out = append(out, *t)
		}
	}
	return out
}
