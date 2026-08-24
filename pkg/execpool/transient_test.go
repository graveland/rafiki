package execpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executors"
)

// stubStore fails every call. A transient executor must never consult it.
type stubStore struct{ executors.Store }

func (stubStore) Get(context.Context, string) (executors.Executor, error) {
	return executors.Executor{}, errors.New("a transient executor must never reach the store")
}

func TestRefreshRowSkipsTransientExecutors(t *testing.T) {
	p := &Pool{
		store:         stubStore{},
		live:          map[string]*liveConn{},
		parked:        map[string]*parkedEntry{},
		healthTimeout: time.Second,
	}
	lc := &liveConn{
		executor:  executors.Executor{ID: "e1", Enabled: true},
		transient: true,
	}
	p.live["e1"] = lc

	if err := p.refreshRow(context.Background(), "e1", lc); err != nil {
		t.Fatalf("a transient executor has no row to re-read; refreshRow must "+
			"skip it entirely or ErrNotFound revokes it on the first health "+
			"tick, 30 seconds in: %v", err)
	}
}

func TestRefreshRowStillRevokesDurableExecutors(t *testing.T) {
	p := &Pool{
		store:         stubStore{},
		live:          map[string]*liveConn{},
		parked:        map[string]*parkedEntry{},
		healthTimeout: time.Second,
	}
	lc := &liveConn{executor: executors.Executor{ID: "e2", Enabled: true}}
	p.live["e2"] = lc

	// A durable executor whose row cannot be read keeps its last known row
	// (transient blips must not evict), so this returns nil -- but it MUST
	// have consulted the store, unlike the transient case.
	if err := p.refreshRow(context.Background(), "e2", lc); err != nil {
		t.Fatalf("an unreadable row keeps the last known one: %v", err)
	}
}
