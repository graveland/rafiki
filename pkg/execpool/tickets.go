package execpool

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/executors"
)

// TicketGrant is everything the daemon knows about the executor a ticket admits
// before it connects: the fields a TRANSIENT executor is synthesised from, or
// the id of the durable row a row-bound ticket authenticates.
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
	// RowID, when set, admits the EXISTING durable row with this id: every
	// other field is ignored.
	//
	// The two shapes answer two different questions. A row-less grant stands
	// for a session executor that exists only while a control connection is
	// open, so the grant IS the row. A row-bound grant is how an executor that
	// already has a row — installed over ssh, say — presents a fresh one-shot
	// credential on each launch: the ticket buys this connection, and the row
	// is the identity, read from the store rather than synthesised. It is the
	// same authority a Bearer credential carries, only minted by a control
	// connection instead of issued at enrollment.
	RowID string
	// MintCredential is set only for a row-bound grant minted by the ssh
	// install path in its direct mode: redeeming it rotates the row's
	// credential and returns the new one on the 101 so the executor can
	// reconnect on its own. Ignored without RowID.
	MintCredential bool
	// OwnerUserID is the durable id of the identity this session executor
	// serves — "" for the UDS's unowned fallback, exactly like a child's own
	// OwnerUserID. It is what selection's ownership check compares against,
	// separate from Owner (the display name stamped into the "owner" label).
	OwnerUserID string
}

// Executor synthesises the in-memory row this grant stands for. It is the
// TRANSIENT shape only: a row-bound grant has no synthesised row, because the
// store's row is the identity (see RowID).
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
		Admits:      "owner=" + g.Owner,
		OwnerUserID: g.OwnerUserID,
		Enabled:     true,
		EnrolledAt:  time.Now(),
		LastSeenAt:  time.Now(),
	}
}

// TicketRegistry holds unredeemed session tickets in memory.
//
// In memory and never persisted: a ticket is meaningful only while the control
// connection that minted it is open, and a ticket surviving a daemon restart
// would outlive the connection whose authentication it stands for.
type TicketRegistry struct {
	mu sync.Mutex
	m  map[string]*ticketEntry
}

// ticketEntry is one unredeemed or in-flight ticket. Claimed means a connection
// has taken it and is deciding whether to admit it; the entry is still here so
// a RETRYABLE refusal can hand it back, and it is deleted outright once the
// redemption commits or the control connection revokes it.
type ticketEntry struct {
	grant TicketGrant
	held  bool
}

func NewTicketRegistry() *TicketRegistry {
	return &TicketRegistry{m: map[string]*ticketEntry{}}
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
	r.m[ticket] = &ticketEntry{grant: g}
	return ticket, nil
}

// Redeem consumes a ticket and returns its grant. A ticket redeems exactly
// once: replaying one must not authenticate a second executor onto the same
// identity, which is the same rule Pool.admit enforces for durable
// credentials.
//
// This is the cheap one-shot primitive for a caller that needs nothing between
// the read and the consumption. Pool.authorize uses Claim/Commit instead,
// because it has work that can fail in between.
func (r *TicketRegistry) Redeem(ticket string) (TicketGrant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[ticket]
	if !ok || e.held {
		return TicketGrant{}, false
	}
	delete(r.m, ticket)
	return e.grant, true
}

// Claim takes the ticket ATOMICALLY, returning its grant, and holds it until the
// caller Commits or Releases it.
//
// The claim is what makes the credential exchange safe: two connections
// presenting the same ticket cannot both get past this point, so exactly one of
// them can rotate the row's credential — a second rotation would invalidate the
// credential the first was just handed, which is a way to strand a legitimate
// executor by replaying its ticket.
//
// A claim is BOOKED, not spent: Pool.authorize Releases it on any refusal the
// peer may retry (a store outage, a 409 against a live incumbent, a failed
// rotation), so the retry presents a ticket that still works, and Commits it on
// success or on a refusal that retrying cannot fix. A ticket REVOKED while held
// is deleted, and Release then finds nothing to hand back — revocation always
// wins over a late release.
func (r *TicketRegistry) Claim(ticket string) (TicketGrant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[ticket]
	if !ok || e.held {
		return TicketGrant{}, false
	}
	e.held = true
	return e.grant, true
}

// Commit spends a claimed ticket for good. It reports whether the ticket was
// still there: false means it was REVOKED while held (a launch the daemon gave
// up on), and the caller must not admit the connection that claimed it.
func (r *TicketRegistry) Commit(ticket string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.m[ticket]
	delete(r.m, ticket)
	return ok
}

// Release hands a claimed ticket back, so a peer told to RETRY has something to
// retry with. It acts only on a ticket that is still held: a revoked (deleted)
// or already committed ticket stays gone, so a late release cannot resurrect
// one the daemon gave up on.
func (r *TicketRegistry) Release(ticket string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.m[ticket]; ok && e.held {
		e.held = false
	}
}

// Revoke discards an unredeemed ticket. This is how a closed control
// connection stops its executor from ever connecting.
func (r *TicketRegistry) Revoke(ticket string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, ticket)
}
