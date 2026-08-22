package execpool

import (
	"context"
	"errors"
	"sync"

	"go.graveland.dev/rafiki/pkg/executors"
)

// fakeStore is an executors.Store whose auth outcome the test controls. Only
// the methods the pool actually calls do anything; the rest exist to satisfy
// the interface and fail loudly if the pool ever starts using them.
type fakeStore struct {
	// mu guards executor. The health loop re-reads the row on its own goroutine
	// now, so a test mutating it (to relabel or disable) races the pool without
	// this — and -race would fail the suite rather than the assertion.
	mu       sync.Mutex
	executor executors.Executor
	deleted  bool

	// authErr, when non-nil, is what Authenticate returns.
	authErr error
	// authCalls counts Authenticate attempts, so a test can assert that a
	// transient failure was RETRIED rather than accepted as terminal.
	authCalls chan struct{}
}

func newFakeStore(id string) *fakeStore {
	return &fakeStore{
		executor:  executors.Executor{ID: id, DisplayName: id, Enabled: true},
		authCalls: make(chan struct{}, 64),
	}
}

func (f *fakeStore) Authenticate(context.Context, string) (executors.Executor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case f.authCalls <- struct{}{}:
	default:
	}
	if f.authErr != nil {
		return executors.Executor{}, f.authErr
	}
	return f.executor, nil
}

func (f *fakeStore) Enroll(context.Context, string, map[string]string) (executors.Executor, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.executor, "credential", nil
}

func (f *fakeStore) Create(context.Context, executors.NewToken) (executors.Executor, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.executor, "credential", nil
}

func (f *fakeStore) TouchSeen(context.Context, string) error { return nil }

func (f *fakeStore) Get(_ context.Context, id string) (executors.Executor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleted {
		return executors.Executor{}, executors.ErrNotFound
	}
	if id != f.executor.ID {
		return executors.Executor{}, errors.New("no such executor")
	}
	return f.executor, nil
}

// delete simulates the row having been hard-deleted from under a connected
// executor: Get now answers ErrNotFound specifically, not a generic error.
func (f *fakeStore) delete() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = true
}

// update mutates the stored row the way an operator would: `rafiki executor
// label` / `disable` are row updates and nothing else.
func (f *fakeStore) update(fn func(*executors.Executor)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.executor)
}

func (f *fakeStore) List(context.Context) ([]executors.Executor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []executors.Executor{f.executor}, nil
}

func (f *fakeStore) MintToken(context.Context, executors.NewToken) (string, error) {
	return "", errors.New("fakeStore: MintToken not used by the pool")
}

func (f *fakeStore) SetLabels(context.Context, string, map[string]string, []string) (executors.Executor, error) {
	return executors.Executor{}, errors.New("fakeStore: SetLabels not used by the pool")
}

func (f *fakeStore) SetEnabled(context.Context, string, bool) error {
	return errors.New("fakeStore: SetEnabled not used by the pool")
}

func (f *fakeStore) Delete(context.Context, string) error {
	return errors.New("fakeStore: Delete not used by the pool")
}

func (f *fakeStore) Annotate(context.Context, string, map[string]string, []string) error {
	return errors.New("fakeStore: Annotate not used by the pool")
}
