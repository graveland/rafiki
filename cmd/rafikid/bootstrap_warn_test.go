package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/users"
)

// countingStore answers CountActive from a script and nothing else.
type countingStore struct {
	calls chan struct{}
	steps []func() (int, error)
	n     int
}

func (c *countingStore) CountActive(context.Context) (int, error) {
	step := c.steps[min(c.n, len(c.steps)-1)]
	c.n++
	c.calls <- struct{}{}
	return step()
}

func (c *countingStore) Create(context.Context, string) (users.User, string, error) {
	panic("unused")
}
func (c *countingStore) Authenticate(context.Context, string) (users.Identity, error) {
	panic("unused")
}
func (c *countingStore) List(context.Context, bool, int) ([]users.User, error) { panic("unused") }
func (c *countingStore) Delete(context.Context, string) error                  { panic("unused") }

func drain(t *testing.T, ch chan struct{}, want int) {
	t.Helper()
	for i := 0; i < want; i++ {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("CountActive call %d never came", i+1)
		}
	}
}

// The warning must stop the moment a user exists — the window is closed and
// repeating the warning would train an operator to ignore it.
func TestTheUnclaimedWarningStopsOnceAUserExists(t *testing.T) {
	st := &countingStore{
		calls: make(chan struct{}, 8),
		steps: []func() (int, error){
			func() (int, error) { return 0, nil },
			func() (int, error) { return 1, nil },
		},
	}
	done := make(chan struct{})
	go func() {
		warnWhileUnclaimed(context.Background(), st, "127.0.0.1:0", time.Millisecond)
		close(done)
	}()
	drain(t, st.calls, 2)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the warning loop kept running after a user existed")
	}
}

// A store error is not evidence the window is closed. Returning on the first
// blip would silence the warning for the daemon's whole lifetime.
func TestAStoreErrorDoesNotEndTheUnclaimedWarning(t *testing.T) {
	st := &countingStore{
		calls: make(chan struct{}, 8),
		steps: []func() (int, error){
			func() (int, error) { return 0, errors.New("connection refused") },
			func() (int, error) { return 0, nil },
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		warnWhileUnclaimed(ctx, st, "127.0.0.1:0", time.Millisecond)
		close(done)
	}()
	// A third call proves the loop survived the error and kept checking.
	drain(t, st.calls, 3)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the warning loop ignored context cancellation")
	}
}
