// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

// One writer appends while eight readers snapshot. The invariant is the one the
// write order exists for: a reader that sees a head always finds its nodes, so
// every snapshot is a complete, unbroken chain with strictly increasing
// ordinals.
func TestConcurrentReadersDuringAppend(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	s := conv.NewStore(b, conv.Options{})
	writer := s.Open(convID, conv.AsOwner())

	const (
		readers = 8
		rounds  = 20
	)
	stop, problems := startSnapshotReaders(ctx, s, convID, readers)
	for i := 0; i < rounds; i++ {
		_, err := writer.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
			Msg: textMsg(msg.User, fmt.Sprintf("round %d", i)),
		}}})
		c.NoError(err, "append %d", i)
	}
	stop()
	for err := range problems {
		c.NoError(err, "a concurrent reader saw an inconsistent snapshot")
	}

	path, err := writer.Working(ctx)
	c.NoError(err, "Working")
	c.Len(path, rounds, "every append landed on the chain")
}

// The same invariant with the entries expiring constantly: a short TTL and a
// clock the writer advances between rounds make every sweep interleave with the
// writers, so a publication that outlived its entry has somewhere to land.
func TestConcurrentReadersDuringAppendAcrossSweeps(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	s := conv.NewStore(b, conv.Options{Now: clk.Now, IdleTTL: time.Millisecond})
	writer := s.Open(convID, conv.AsOwner())

	const (
		readers = 8
		rounds  = 40
	)
	stop, problems := startSnapshotReaders(ctx, s, convID, readers)
	for i := 0; i < rounds; i++ {
		clk.Advance(2 * time.Millisecond)
		_, err := writer.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
			Msg: textMsg(msg.User, fmt.Sprintf("round %d", i)),
		}}})
		c.NoError(err, "append %d", i)
	}
	stop()
	for err := range problems {
		c.NoError(err, "a concurrent reader saw an inconsistent snapshot")
	}

	path, err := writer.Working(ctx)
	c.NoError(err, "Working")
	c.Len(path, rounds, "every append landed on the chain")
}

// startSnapshotReaders snapshots convID from readers goroutines until the
// returned stop function is called, and reports the first inconsistency each
// reader sees on the returned channel, which stop closes.
func startSnapshotReaders(ctx context.Context, s *conv.Store, convID string, readers int) (stop func(), problems chan error) {
	done := make(chan struct{})
	problems = make(chan error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader := s.Open(convID)
			for {
				select {
				case <-done:
					return
				default:
				}
				if err := checkSnapshot(ctx, reader); err != nil {
					problems <- err
					return
				}
			}
		}()
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			close(problems)
		})
	}
	return stop, problems
}

// checkSnapshot takes one snapshot and reports the first way it is not a
// complete, unbroken chain.
func checkSnapshot(ctx context.Context, reader *conv.Handle) error {
	sn, err := reader.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	last := -1
	var prev *conv.Node
	for n := range sn.History() {
		if n.Ordinal <= last {
			return fmt.Errorf("ordinal %d follows %d", n.Ordinal, last)
		}
		last = n.Ordinal
		if prev == nil {
			if n.ParentID != "" {
				return fmt.Errorf("history starts at %s, which is not a root", n.ID)
			}
		} else if n.ParentID != prev.ID {
			return fmt.Errorf("node %s has parent %s, want %s", n.ID, n.ParentID, prev.ID)
		}
		prev = n
	}
	if prev == nil {
		if sn.Head() != nil {
			return fmt.Errorf("an empty history reports head %s", sn.Head().ID)
		}
		return nil
	}
	if sn.Head().ID != prev.ID {
		return fmt.Errorf("head %s is not the last node %s", sn.Head().ID, prev.ID)
	}
	return nil
}
