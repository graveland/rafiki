package execpool

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/executors"
)

// TicketGrant is everything the daemon knows about a transient executor before
// it connects.
//
// Every field is written by the daemon from an AUTHENTICATED control
// connection — never from anything the executor says about itself. That is
// what lets a transient executor have no database row without weakening the
// rule the row exists to enforce: the invariant is not "a row is the
// authority", it is "every access-gating fact is written by the daemon from
// something it verified".
type TicketGrant struct {
	ExecutorID  string
	Owner       string
	MachineName string
	Roots       []string
}

// Executor synthesises the in-memory row this grant stands for.
func (g TicketGrant) Executor() executors.Executor {
	return executors.Executor{
		ID: g.ExecutorID,
		// The machine label IS the name; there is no separate display name.
		Labels: map[string]string{
			"owner":   g.Owner,
			"machine": g.MachineName,
			"kind":    "session",
		},
		// Empty, and that is the point: nothing about a transient executor is
		// self-reported.
		SelfReported: map[string]string{},
		Roots:        g.Roots,
		// An operator's own terminal is not a container, and it is not
		// interchangeable with anyone else's machine.
		Isolation:     "none",
		WorkspaceMode: "pinned",
		// Nobody else's children land on this operator's terminal.
		Admits:     "owner=" + g.Owner,
		Enabled:    true,
		EnrolledAt: time.Now(),
		LastSeenAt: time.Now(),
	}
}

// TicketRegistry holds unredeemed session tickets in memory.
//
// In memory and never persisted: a ticket is meaningful only while the control
// connection that minted it is open, and a ticket surviving a daemon restart
// would outlive the connection whose authentication it stands for.
type TicketRegistry struct {
	mu sync.Mutex
	m  map[string]TicketGrant
}

func NewTicketRegistry() *TicketRegistry {
	return &TicketRegistry{m: map[string]TicketGrant{}}
}

// Mint issues a one-shot ticket for g.
func (r *TicketRegistry) Mint(g TicketGrant) (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mint executor session ticket: %w", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf[:])

	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[ticket] = g
	return ticket, nil
}

// Redeem consumes a ticket. A ticket redeems exactly once: replaying one must
// not authenticate a second executor onto the same identity, which is the same
// rule Pool.admit enforces for durable credentials.
func (r *TicketRegistry) Redeem(ticket string) (TicketGrant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.m[ticket]
	if ok {
		delete(r.m, ticket)
	}
	return g, ok
}

// Revoke discards an unredeemed ticket. This is how a closed control
// connection stops its executor from ever connecting.
func (r *TicketRegistry) Revoke(ticket string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, ticket)
}
