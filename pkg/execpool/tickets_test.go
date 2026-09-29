package execpool

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestTicketRedeemsOnceThenIsSpent(t *testing.T) {
	r := NewTicketRegistry()
	tk, err := r.Mint(TicketGrant{ExecutorID: "e1", Owner: "brent"})
	assert.NewAborting(t).NoError(err)

	if _, ok := r.Redeem(tk); !ok {
		t.Fatal("a freshly minted ticket must redeem")
	}
	if _, ok := r.Redeem(tk); ok {
		t.Fatal("a ticket is ONE-SHOT: a replayed ticket must not authenticate " +
			"a second executor onto the same identity")
	}
}

func TestTicketRevokeBeforeRedeem(t *testing.T) {
	r := NewTicketRegistry()
	tk, err := r.Mint(TicketGrant{ExecutorID: "e1", Owner: "brent"})
	assert.NewAborting(t).NoError(err)
	r.Revoke(tk)
	if _, ok := r.Redeem(tk); ok {
		t.Fatal("a revoked ticket must not redeem: revocation is how a closed " +
			"control connection stops its executor reconnecting")
	}
}

func TestTicketUnknownIsRefused(t *testing.T) {
	r := NewTicketRegistry()
	_, ok := r.Redeem("not-a-ticket")
	assert.NewAborting(t).False(ok, "an unknown ticket must be refused")
}

func TestTicketsAreDistinct(t *testing.T) {
	c := assert.NewAborting(t)
	r := NewTicketRegistry()
	a, _ := r.Mint(TicketGrant{ExecutorID: "e1"})
	b, _ := r.Mint(TicketGrant{ExecutorID: "e2"})
	c.NotEq(b, a, "two mints must produce different tickets")
	c.GreaterOrEqual(32, len(a), "a ticket is a bearer credential and must be unguessable; got")
}

func TestGrantBuildsADaemonWrittenExecutorRow(t *testing.T) {
	c := assert.NewAborting(t)
	g := TicketGrant{
		ExecutorID:  "e1",
		Owner:       "brent",
		MachineName: "abc123",
		Roots:       []string{"/Users/brent/src"},
	}
	e := g.Executor()

	c.Eq("owner=brent", e.Admits, "a transient executor must admit ONLY its owner, got")
	c.False(e.Labels["owner"] != "brent" || e.Labels["machine"] != "abc123", "owner and machine must be daemon-written labels, got %v", e.Labels)
	c.Eq("session", e.Labels["kind"], "kind=session is what sortCandidates ranks below a durable executor, got")
	c.Eq("none", e.Isolation, "an operator's own terminal is not sandboxed, got")
	c.Eq("pinned", e.WorkspaceMode, "want pinned, got")
	c.True(e.Enabled, "a redeemed grant is enabled")
	c.Empty(e.SelfReported, "NOTHING about a transient executor is self-reported; every "+
		"field here is written by the daemon from the authenticated connection")
}

// TestTicketGrantExecutorCarriesOwner pins the ownership half of the grant:
// the transient row a ticket stands for carries the grant's durable
// OwnerUserID — what selection's ownership rule compares — while the "owner"
// label and the Admits selector stay the display name. An empty id stays
// empty: the UDS's session executor is unowned, exactly like a child it
// spawns.
func TestTicketGrantExecutorCarriesOwner(t *testing.T) {
	c := assert.NewAborting(t)
	e := TicketGrant{ExecutorID: "e1", Owner: "brent", OwnerUserID: "u_brent"}.Executor()
	c.Eq("u_brent", e.OwnerUserID, "the transient row must carry the grant's OwnerUserID, got")
	c.Eq("owner=brent", e.Admits, "Admits must stay the DISPLAY name, got")

	unowned := TicketGrant{ExecutorID: "e2", Owner: "root"}.Executor()
	c.Empty(unowned.OwnerUserID, "a grant without an owner id must synthesise an unowned row, got %q", unowned.OwnerUserID)
}
