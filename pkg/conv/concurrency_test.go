// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

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
	done := make(chan struct{})
	problems := make(chan error, readers)
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
				sn, err := reader.Snapshot(ctx)
				if err != nil {
					problems <- fmt.Errorf("snapshot: %w", err)
					return
				}
				last := -1
				var prev *conv.Node
				for n := range sn.History() {
					if n.Ordinal <= last {
						problems <- fmt.Errorf("ordinal %d follows %d", n.Ordinal, last)
						return
					}
					last = n.Ordinal
					if prev == nil {
						if n.ParentID != "" {
							problems <- fmt.Errorf("history starts at %s, which is not a root", n.ID)
							return
						}
					} else if n.ParentID != prev.ID {
						problems <- fmt.Errorf("node %s has parent %s, want %s", n.ID, n.ParentID, prev.ID)
						return
					}
					prev = n
				}
				if prev == nil {
					if sn.Head() != nil {
						problems <- fmt.Errorf("an empty history reports head %s", sn.Head().ID)
						return
					}
					continue
				}
				if sn.Head().ID != prev.ID {
					problems <- fmt.Errorf("head %s is not the last node %s", sn.Head().ID, prev.ID)
					return
				}
			}
		}()
	}

	for i := 0; i < rounds; i++ {
		_, err := writer.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
			Msg: textMsg(msg.User, fmt.Sprintf("round %d", i)),
		}}})
		c.NoError(err, "append %d", i)
	}
	close(done)
	wg.Wait()
	close(problems)
	for err := range problems {
		c.NoError(err, "a concurrent reader saw an inconsistent snapshot")
	}

	path, err := writer.Working(ctx)
	c.NoError(err, "Working")
	c.Len(path, rounds, "every append landed on the chain")
}
