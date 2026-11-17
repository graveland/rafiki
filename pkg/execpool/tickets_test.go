package execpool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/upgradeconn"

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

// Claim is what lets admission finish before the one-shot gate commits: it must
// not spend the ticket, and it must be ATOMIC, because two connections
// presenting one ticket must not both rotate the row's credential.
func TestTicketClaimIsAtomicAndReleasable(t *testing.T) {
	c := assert.NewAborting(t)
	r := NewTicketRegistry()
	g := TicketGrant{ExecutorID: "e1", Owner: "brent", RowID: "row-1", MintCredential: true}
	tk, err := r.Mint(g)
	c.NoError(err)

	held, ok := r.Claim(tk)
	c.True(ok, "a freshly minted ticket must be claimable")
	c.Eq(g.RowID, held.RowID, "the claim returns the grant that was minted, got")

	// A second presentation of the SAME ticket loses, or two connections would
	// both rotate the credential and the first one's plaintext would be dead.
	if _, again := r.Claim(tk); again {
		t.Fatal("a held ticket was claimable twice: two concurrent redemptions would " +
			"both rotate the row's credential and strand the winner")
	}
	if _, redeemed := r.Redeem(tk); redeemed {
		t.Fatal("a held ticket was redeemable: the one-shot gate must be the claim")
	}

	// Release hands it back for the peer that was told to retry.
	r.Release(tk)
	held, ok = r.Claim(tk)
	c.True(ok, "a released ticket must be claimable again, or every retry is a 401")
	c.Eq(g.RowID, held.RowID, "the re-claim returns the same grant, got")

	// Commit spends it for good.
	r.Commit(tk)
	if _, again := r.Claim(tk); again {
		t.Fatal("a committed ticket was claimable; one ticket is one connection")
	}

	// Revocation during a hold WINS over a late release: otherwise a launched
	// the daemon had given up on would become redeemable again.
	revoked, err := r.Mint(TicketGrant{ExecutorID: "e2"})
	c.NoError(err)
	if _, ok := r.Claim(revoked); !ok {
		t.Fatal("a fresh ticket must be claimable")
	}
	r.Revoke(revoked)
	r.Release(revoked)
	if _, ok := r.Claim(revoked); ok {
		t.Fatal("release resurrected a revoked ticket: a launch the daemon gave up on " +
			"must never become redeemable again")
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

// ─── row-bound tickets ─────────────────────────────────────────────────────

// A row-bound ticket authenticates an EXISTING durable row. It must not be
// mistaken for a transient one: `transient` decides whether refreshRow re-reads
// the row and whether the connection is reaped when a control connection
// closes, and a durable executor admitted as transient would silently stop
// honouring `rafiki executor disable`.
func TestTicketRowIDAdmitsTheDurableRow(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeStore("row-1")
	store.update(func(e *executors.Executor) {
		e.Labels = map[string]string{"machine": "durable-box", "owner": "brent"}
		e.Roots = []string{"/srv/work"}
	})

	p := New(store)
	// The grant's other fields are deliberately garbage: when RowID is set they
	// are ignored, and a synthesised executor would carry them.
	ticket, err := p.Tickets().Mint(TicketGrant{
		RowID:       "row-1",
		ExecutorID:  "sess-ignored",
		Owner:       "ignored",
		MachineName: "ignored",
	})
	c.NoError(err)

	a, hdr, err := p.authorize(ticketRequest(t, ticket))
	c.NoError(err, "a row-bound ticket must admit the row it names")
	c.Eq("row-1", a.executor.ID, "the admitted executor must be the row, got")
	c.Eq("durable-box", a.executor.Labels["machine"],
		"every gating fact must come from the ROW, not from the grant; got %v", a.executor.Labels)
	c.Eq("/srv/work", a.executor.Roots[0], "the row's roots must survive, got %v", a.executor.Roots)
	c.False(a.transient, "a row-bound ticket admits a DURABLE row; transient would make "+
		"refreshRow skip it and disable stop working")
	c.Empty(a.credential, "a ticket mints no credential")
	c.Eq("row-1", hdr.Get(upgradeconn.HeaderExecutorID), "the 101 must name the row, got")
}

// A disabled row is a revocation. The ticket proves the control connection
// asked for this row; it does not outrank the row's own switch.
func TestTicketRowIDDisabledRowRefused(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeStore("row-1")
	store.update(func(e *executors.Executor) { e.Enabled = false })

	p := New(store)
	ticket, err := p.Tickets().Mint(TicketGrant{RowID: "row-1"})
	c.NoError(err)

	_, _, err = p.authorize(ticketRequest(t, ticket))
	var ref *upgradeconn.Refusal
	c.False(!errors.As(err, &ref) || ref.Status != http.StatusUnauthorized,
		"a disabled row must be refused 401, got %v", err)
	c.StrContains(ref.Reason, "disabled", "the refusal must name the reason, got")
}

// An unknown row is an answer, not an outage: the install's row is gone, and
// no amount of retrying the same ticket will bring it back.
func TestTicketRowIDUnknownRowRefused(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(rowNotFoundStore{newFakeStore("row-1")})
	ticket, err := p.Tickets().Mint(TicketGrant{RowID: "row-missing"})
	c.NoError(err)

	_, _, err = p.authorize(ticketRequest(t, ticket))
	var ref *upgradeconn.Refusal
	c.False(!errors.As(err, &ref) || ref.Status != http.StatusUnauthorized,
		"an unknown row must be refused 401, got %v", err)
	// 401 is the status the executor's reconnect loop treats as terminal (see
	// TestSpentTicketIsRefusedTerminally): a row that does not exist is an
	// answer, not an outage, and no retry of the same ticket can bring it back.
}

// The row-less path is unchanged, byte for byte: a grant with no RowID is still
// a transient executor synthesised from the grant, and it still never touches
// the store — stubStore fails every call, so a store read would surface as a
// 503 rather than as the admission this asserts.
func TestTicketWithoutRowIDUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(stubStore{})
	ticket, err := p.Tickets().Mint(TicketGrant{
		ExecutorID:  "sess-01J0",
		Owner:       "brent",
		MachineName: "laptop",
	})
	c.NoError(err)

	a, hdr, err := p.authorize(ticketRequest(t, ticket))
	c.NoError(err, "a row-less ticket must still admit a transient executor")
	c.True(a.transient, "a grant with no RowID is a transient executor")
	c.Eq("sess-01J0", a.executor.ID, "the synthesised executor must carry the grant's id, got")
	c.Eq("session", a.executor.Labels["kind"], "the synthesised row is kind=session, got")
	c.Eq("owner=brent", a.executor.Admits, "a transient executor admits only its owner, got")
	c.Eq("sess-01J0", hdr.Get(upgradeconn.HeaderExecutorID), "the 101 must name the executor, got")
}

// ticketRequest is an upgrade request carrying a Ticket credential, for tests
// that drive Pool.authorize directly.
func ticketRequest(t *testing.T, ticket string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, upgradeconn.PathFor(upgradeconn.Executor), nil)
	req.Header.Set("Authorization", string(upgradeconn.SchemeTicket)+" "+ticket)
	return req
}

// rowNotFoundStore answers every Get the way the real store does for an id no
// row carries. fakeStore's own Get returns an unclassified error, which
// authRefusal rightly reports as retryable — a different case with its own
// test — so the unknown-row test needs a store that says what a deployment
// says.
type rowNotFoundStore struct{ *fakeStore }

func (rowNotFoundStore) Get(context.Context, string) (executors.Executor, error) {
	return executors.Executor{}, executors.ErrNotFound
}
