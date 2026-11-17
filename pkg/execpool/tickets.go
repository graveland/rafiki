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
//
// This is the ONE-SHOT GATE, and it closes last: Pool.authorize calls it only
// once the connection is admissible and the credential has been minted, so
// every retryable refusal on the way there leaves the ticket redeemable.
// Peek is how it reads the grant before that point.
func (r *TicketRegistry) Redeem(ticket string) (TicketGrant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.m[ticket]
	if ok {
		delete(r.m, ticket)
	}
	return g, ok
}

// Peek reads a ticket's grant WITHOUT consuming it.
//
// It exists so admission can do everything that can fail — resolve the row,
// check it is enabled, admit the connection, mint the credential — before the
// one-shot gate closes. Every one of those refusals is retryable, and each is
// only retryable because nothing has been consumed: a ticket spent by a
// connection that was then refused makes the retry a terminal 401 and takes the
// executor out of service over a database blip.
//
// It is NOT an alternative to Redeem for deciding one-shot-ness. Two concurrent
// attempts can both peek the same ticket; exactly one of them Redeems it, and
// the other is refused there — see Pool.authorize's SchemeTicket branch.
func (r *TicketRegistry) Peek(ticket string) (TicketGrant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.m[ticket]
	return g, ok
}

// sameGrant reports whether the grant a redemption returned is the one
// admission was decided against: the same row, the same executor, the same
// minting flag. Redeem is what makes a ticket one-shot, so a value that differs
// here is not the ticket this connection was authenticated on — admitting it
// would authorise against facts nobody checked.
func sameGrant(peeked, redeemed TicketGrant) bool {
	return peeked.RowID == redeemed.RowID &&
		peeked.ExecutorID == redeemed.ExecutorID &&
		peeked.MintCredential == redeemed.MintCredential
}

// Revoke discards an unredeemed ticket. This is how a closed control
// connection stops its executor from ever connecting.
func (r *TicketRegistry) Revoke(ticket string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, ticket)
}
