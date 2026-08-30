// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/store"
)

// trackLease records a lease this daemon holds for a child.
func (c *Controller) trackLease(childID string, l store.Lease) {
	c.heldLeasesMu.Lock()
	c.heldLeases[childID] = l
	c.heldLeasesMu.Unlock()
}

// dropLease forgets a lease without releasing it. Used when a resume fails —
// the lease expires on its own, which is the right outcome for a child this
// daemon could not actually start.
func (c *Controller) dropLease(childID string) {
	c.heldLeasesMu.Lock()
	delete(c.heldLeases, childID)
	c.heldLeasesMu.Unlock()
}

// holdsLease reports whether THIS daemon currently holds the conversation
// lease for childID — the only authority on which daemon actually owns a
// child, as opposed to which daemon merely attempted to resume it.
//
// This is load-bearing for recovery: resumeWithAutoRecovery can report
// success for a child whose in-process engine build is still running on its
// own goroutine (Runner.Start returns before Build completes) and whose lease
// acquisition was REFUSED — activateLiveChild's Idle-or-5s-timeout select
// does not distinguish "became idle" from "stalled because the build already
// failed", so a synchronous success return does not prove ownership. Every
// caller that is about to act as though it owns a resumed child (replaying
// its inbox, for one) must check this first rather than trusting that
// return value alone. c.trackLease is called only on a successful
// c.leases.Acquire, from inside OnConversationResolved — the single
// lease-acquisition site every agent path funnels through — so its absence
// here means either the acquire lost to another daemon or it has not
// completed yet; both are reasons not to act as the owner.
func (c *Controller) holdsLease(childID string) bool {
	c.heldLeasesMu.Lock()
	defer c.heldLeasesMu.Unlock()
	_, ok := c.heldLeases[childID]
	return ok
}

// releaseLease drops and releases the lease held for a child so another daemon
// can take the conversation immediately rather than after the TTL.
func (c *Controller) releaseLease(childID string) {
	c.heldLeasesMu.Lock()
	lease, ok := c.heldLeases[childID]
	delete(c.heldLeases, childID)
	c.heldLeasesMu.Unlock()
	if !ok || c.leases == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.leases.Release(ctx, lease); err != nil {
		slog.Warn("release lease on child exit",
			"childId", childID, "conversationId", lease.ConversationID, "error", err)
	}
}

// startLeaseRenewal keeps every held lease alive until the daemon stops.
//
// Renewal runs on its own goroutine, independent of turn progress. A thinking
// phase can exceed any sane TTL, so coupling renewal to turn completion would
// drop the lease of a perfectly healthy child mid-turn.
func (c *Controller) startLeaseRenewal(ctx context.Context) {
	if c.leases == nil {
		return
	}
	c.sweeperWg.Add(1)
	go func() {
		defer c.sweeperWg.Done()
		ticker := time.NewTicker(leaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Leases are NOT released here. main.go cancels this context
				// BEFORE ShutdownAllChildren, and a final compaction or
				// summarisation turn still writes for up to three minutes
				// after. Releasing now would let a peer daemon acquire the
				// conversation while this daemon's child is still writing to
				// it. main.go calls ReleaseAllLeases after shutdown instead.
				return
			case <-ticker.C:
				c.renewLeasesOnce(ctx)
			}
		}
	}()
}

// renewLeasesOnce renews every held lease, then stops the children whose leases
// are gone.
func (c *Controller) renewLeasesOnce(ctx context.Context) {
	c.heldLeasesMu.Lock()
	snapshot := make(map[string]store.Lease, len(c.heldLeases))
	for k, v := range c.heldLeases {
		snapshot[k] = v
	}
	c.heldLeasesMu.Unlock()

	renew := func(childID string) (bool, error) {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return c.leases.Renew(rctx, snapshot[childID], leaseTTL)
	}
	renewThenKill(snapshot, renew, c.onLeaseLost)
}

// renewThenKill renews every lease FIRST, then stops the lost children
// concurrently.
//
// Both halves matter. onLeaseLost blocks on Kill, which waits for the process
// to be reaped and then for handleChildExit to finish; a takeover invalidates
// leases in bulk, so killing inline while still iterating lets the unrenewed
// remainder expire behind the kills, turning a partial takeover into a total one.
func renewThenKill(
	held map[string]store.Lease,
	renew func(childID string) (bool, error),
	lost func(childID string),
) {
	var doomed []string
	for childID := range held {
		ok, err := renew(childID)
		if err != nil {
			// A transient database error is not an answer. Keep the lease and
			// retry next tick — five attempts fit inside one TTL precisely so a
			// single failure costs nothing.
			slog.Warn("lease renewal failed; will retry", "childId", childID, "error", err)
			continue
		}
		if !ok {
			doomed = append(doomed, childID)
		}
	}

	var wg sync.WaitGroup
	for _, childID := range doomed {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			lost(id)
		}(childID)
	}
	wg.Wait()
}

// onLeaseLost stops a child whose conversation was taken over.
//
// The database guard fences WRITES; it does not fence SIDE EFFECTS. An engine
// that keeps looping after losing its lease keeps calling the LLM and keeps
// running bash and write against a workspace, while nothing it does can be
// recorded. Two agents acting on the world when only one can write down what it
// did is worse than either a double write or a hard stop.
//
// No steer frame and no graceful wind-down: the child cannot record a farewell,
// because recording is exactly what it has lost the right to do.
func (c *Controller) onLeaseLost(childID string) {
	c.dropLease(childID)

	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	slog.Warn("conversation lease lost; stopping child",
		"childId", childID, "conversationId", snap.SessionID)

	if err := c.st.Update(childID, func(s *childstore.Session) {
		s.LastRetryError = "lease lost"
	}); err != nil {
		slog.Warn("record lease-lost reason", "childId", childID, "error", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Kill(ctx, childID, 5000, 5000); err != nil {
		slog.Error("stopping a child after lease loss failed", "childId", childID, "error", err)
	}
}

// ReleaseAllLeases drops every held lease on clean shutdown, so a restarting
// daemon's peers see the conversations as free immediately rather than after
// the TTL.
func (c *Controller) ReleaseAllLeases() {
	c.heldLeasesMu.Lock()
	snapshot := make([]store.Lease, 0, len(c.heldLeases))
	for _, v := range c.heldLeases {
		snapshot = append(snapshot, v)
	}
	c.heldLeases = make(map[string]store.Lease)
	c.heldLeasesMu.Unlock()

	for _, lease := range snapshot {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.leases.Release(ctx, lease); err != nil {
			slog.Warn("release lease", "conversationId", lease.ConversationID, "error", err)
		}
		cancel()
	}
}
