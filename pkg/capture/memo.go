// SPDX-License-Identifier: Apache-2.0

package capture

import "sync"

// memoCap bounds each memo table. A table that reaches it is emptied: every
// entry is a shortcut for a read the store can always repeat, so dropping the
// lot costs one read per live conversation, never a wrong answer.
const memoCap = 4096

// captureMemo remembers answers the capture path would otherwise re-ask the
// database on every turn of a long session. It is shared by every copy of a
// CaptureStore (WithLease and WithConv copy the struct, not this).
//
// Each entry is only ever a fact that cannot change, or a value read moments
// earlier within the same turn:
//
//   - modelSet: a conversation whose model column is already set. The backfill
//     is first-seen-wins (WHERE model IS NULL), so once the column is set no
//     later UPDATE can match.
//   - threads: the thread of a turn's predecessor, read at the start of the turn
//     by ThreadOfPredecessorInSession and consumed once, at its end, by
//     RecordThread, which would otherwise run the identical query again.
type captureMemo struct {
	mu       sync.Mutex
	modelSet map[string]struct{}
	threads  map[string]threadMemo
}

type threadMemo struct {
	threadID string
	found    bool // false: the predecessor did not resolve (the ErrNoRows case)
}

func newCaptureMemo() *captureMemo {
	return &captureMemo{modelSet: map[string]struct{}{}, threads: map[string]threadMemo{}}
}

func (m *captureMemo) modelKnown(convID string) bool {
	if m == nil || convID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.modelSet[convID]
	return ok
}

func (m *captureMemo) markModel(convID string) {
	if m == nil || convID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.modelSet) >= memoCap {
		clear(m.modelSet)
	}
	m.modelSet[convID] = struct{}{}
}

func threadKey(session, prevMessageID string) string { return session + "\x00" + prevMessageID }

func (m *captureMemo) putThread(session, prevMessageID, threadID string, found bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.threads) >= memoCap {
		clear(m.threads)
	}
	m.threads[threadKey(session, prevMessageID)] = threadMemo{threadID: threadID, found: found}
}

// takeThread returns and removes the remembered predecessor thread.
func (m *captureMemo) takeThread(session, prevMessageID string) (threadMemo, bool) {
	if m == nil {
		return threadMemo{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := threadKey(session, prevMessageID)
	v, ok := m.threads[k]
	if ok {
		delete(m.threads, k)
	}
	return v, ok
}
