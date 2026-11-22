package execpool

import (
	"sync/atomic"
	"time"
)

// poolCounters are the Pool's cumulative event counts. Atomics, so a hot path
// never widens a critical section and a scrape never takes Pool.mu for them.
type poolCounters struct {
	connects       atomic.Uint64
	displaced      atomic.Uint64
	healthFailures atomic.Uint64
	parks          atomic.Uint64
	parkExpiries   atomic.Uint64

	jobRunning, jobExited, jobReplayComplete atomic.Uint64
	jobStreamEnds                            atomic.Uint64
	jobAcks, jobAckErrors                    atomic.Uint64
}

func (c *poolCounters) countJobEvent(k JobEventKind) {
	switch k {
	case JobEventRunning:
		c.jobRunning.Add(1)
	case JobEventExited:
		c.jobExited.Add(1)
	case JobEventReplayComplete:
		c.jobReplayComplete.Add(1)
	}
}

// ExecutorStat is one connected executor's monitoring view.
type ExecutorStat struct {
	ID string
	// Name is the executor's `machine` label, or its ID when it has none.
	Name string
	// Transient marks a ticket-authenticated executor with no database row. Its
	// ID is minted per session, so it is not a safe metric label.
	Transient   bool
	Draining    bool
	Network     string
	ConnectedAt time.Time
	// InFlight is the number of long-lived and tool RPCs open on the
	// executor's single connection, measured against the HTTP/2 stream limit.
	InFlight int64
}

// PoolStats is a point-in-time view of the pool for monitoring: gauges read
// under Pool.mu, counters cumulative since construction.
type PoolStats struct {
	Executors []ExecutorStat
	Parked    int

	// Connects counts connections installed; Displaced counts the subset that
	// replaced a still-registered connection of the same executor.
	Connects  uint64
	Displaced uint64
	// HealthFailures counts connections removed because a health check failed.
	HealthFailures uint64
	// Parks counts executors parked after a drop; ParkExpiries counts parks
	// that timed out into executor-lost.
	Parks        uint64
	ParkExpiries uint64

	JobRunningEvents, JobExitedEvents, JobReplayCompleteEvents uint64
	// JobStreamEnds counts WatchJobs subscriptions that ended and were
	// retried; JobAcks and JobAckErrors count AckJobExits calls and failures.
	JobStreamEnds uint64
	JobAcks       uint64
	JobAckErrors  uint64
}

// Stats returns the pool's current gauges and cumulative counters.
func (p *Pool) Stats() PoolStats {
	p.mu.RLock()
	st := PoolStats{Parked: len(p.parked)}
	for id, lc := range p.live {
		es := ExecutorStat{
			ID:          id,
			Name:        lc.executor.Labels["machine"],
			Transient:   lc.transient,
			Draining:    lc.draining,
			Network:     lc.network,
			ConnectedAt: lc.connectedAt,
		}
		if es.Name == "" {
			es.Name = id
		}
		if lc.client != nil && lc.client.inflight != nil {
			es.InFlight = lc.client.inflight.n.Load()
		}
		st.Executors = append(st.Executors, es)
	}
	p.mu.RUnlock()

	c := &p.counters
	st.Connects, st.Displaced = c.connects.Load(), c.displaced.Load()
	st.HealthFailures = c.healthFailures.Load()
	st.Parks, st.ParkExpiries = c.parks.Load(), c.parkExpiries.Load()
	st.JobRunningEvents = c.jobRunning.Load()
	st.JobExitedEvents = c.jobExited.Load()
	st.JobReplayCompleteEvents = c.jobReplayComplete.Load()
	st.JobStreamEnds = c.jobStreamEnds.Load()
	st.JobAcks, st.JobAckErrors = c.jobAcks.Load(), c.jobAckErrors.Load()
	return st
}
