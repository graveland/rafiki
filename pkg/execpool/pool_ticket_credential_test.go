package execpool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/upgradeconn"

	"github.com/multigres/testkit/assert"
)

// ticketCredStore is fakeStore with the one thing a minting install ticket
// exercises: a durable credential that can be rotated, and an Authenticate that
// accepts only the CURRENT one — which is what makes "the old credential stops
// working" observable at the pool's boundary rather than at the store's.
type ticketCredStore struct {
	*fakeStore

	mu        sync.Mutex
	cred      string
	rotations int
	// rotateErr, when non-nil, is what RotateCredential returns — a store
	// outage, which must reach the peer as retryable.
	rotateErr error
	// getErr, when non-nil, is what the NEXT Get returns — a read outage on the
	// row, which must reach the peer as retryable AND leave the ticket
	// redeemable, so the retry that follows finds the store whole again.
	getErr error
	// onRotate, when set, runs inside RotateCredential — the hook a test uses to
	// change the world while the connection is mid-flight (revoking the ticket
	// admission is about to consume).
	onRotate func()
}

func newTicketCredStore(id, cred string) *ticketCredStore {
	return &ticketCredStore{fakeStore: newFakeStore(id), cred: cred}
}

// Get answers from fakeStore unless a read outage is armed for this call.
// RotateCredential reads through fakeStore instead, so a rotation's own row read
// never eats the outage armed for admission.
func (s *ticketCredStore) Get(ctx context.Context, id string) (executors.Executor, error) {
	s.mu.Lock()
	err := s.getErr
	s.getErr = nil
	s.mu.Unlock()
	if err != nil {
		return executors.Executor{}, err
	}
	return s.fakeStore.Get(ctx, id)
}

// failNextGet arms one row-read outage, consumed by the next Get.
func (s *ticketCredStore) failNextGet(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getErr = err
}

// revokeOnRotate arms the mid-flight hook: the rotation itself kills the ticket
// admission is holding, which is the revoke racing the consume.
func (s *ticketCredStore) revokeOnRotate(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRotate = fn
}

func (s *ticketCredStore) Authenticate(ctx context.Context, credential string) (executors.Executor, error) {
	s.mu.Lock()
	current := s.cred
	s.mu.Unlock()
	if credential != current {
		return executors.Executor{}, executors.ErrNotFound
	}
	return s.fakeStore.Authenticate(ctx, credential)
}

func (s *ticketCredStore) RotateCredential(ctx context.Context, id string) (string, error) {
	s.mu.Lock()
	rotateErr, hook := s.rotateErr, s.onRotate
	s.mu.Unlock()
	// The hook runs OUTSIDE the lock: it reaches into the pool, and a fake store
	// held across unknown code deadlocks the test rather than failing it.
	if hook != nil {
		hook()
	}
	if rotateErr != nil {
		return "", rotateErr
	}
	if _, err := s.fakeStore.Get(ctx, id); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotations++
	s.cred = fmt.Sprintf("minted-%d", s.rotations)
	return s.cred, nil
}

func (s *ticketCredStore) rotateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotations
}

func (s *ticketCredStore) currentCredential() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

func requireRefused(t *testing.T, err error, status int) *upgradeconn.Refused {
	t.Helper()
	c := assert.NewAborting(t)
	var ref *upgradeconn.Refused
	c.False(!errors.As(err, &ref) || ref.Status != status,
		"want a %d refusal, got %v", status, err)
	return ref
}

// An install ticket in direct mode is the only hand a self-dialling executor
// gets: it must leave the 101 holding a durable credential, and the one it
// replaced must stop working, or the previous launch's credential stays live on
// the machine that was just reinstalled.
func TestRedeemInstallTicketRotatesCredential(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "the-old-credential")
	p := New(store)

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")

	conn, hdr, err := upgradeExchange(t, p, ticketHeader(ticket))
	c.NoError(err, "redeeming an install ticket must be admitted")
	c.Eq(1, store.rotateCount(), "the redemption must rotate the row's credential exactly once, got")
	minted := hdr.Get(upgradeconn.HeaderCredential)
	c.NotEq("", minted, "the 101 must carry the minted credential on %s; without it the "+
		"executor has nothing to reconnect with", upgradeconn.HeaderCredential)
	c.NotEq("the-old-credential", minted, "the 101 carried the OLD credential")
	c.Eq(minted, store.currentCredential(), "the header's plaintext must be the one the store now holds, got")
	// Free the executor id before the follow-up dials; a live incumbent would
	// be refused 409, which TestRedeemInstallTicketWithLiveIncumbentRotatesNothing
	// covers.
	c.NoError(conn.Close(), "close the ticket connection")

	oldErr := exchangeErr(t, p, bearerHeader("the-old-credential"))
	_ = requireRefused(t, oldErr, http.StatusUnauthorized)

	conn2, _, err := upgradeExchange(t, p, bearerHeader(minted))
	c.NoError(err, "the minted credential must authenticate the same row")
	c.NoError(conn2.Close(), "close")
}

// A row-bound ticket that is not an install hand-off carries the ticket's
// authority and nothing else: minting there would rotate a credential for a
// caller that never got one, quietly invalidating the executor that holds it.
func TestRedeemPlainRowTicketMintsNothing(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "durable-credential")
	p := New(store)

	ticket, err := p.Tickets().Mint(TicketGrant{Owner: "brent", RowID: "exec-1"})
	c.NoError(err, "mint row-bound ticket")

	conn, hdr, err := upgradeExchange(t, p, ticketHeader(ticket))
	c.NoError(err, "a row-bound ticket must still be admitted")
	c.Eq("", hdr.Get(upgradeconn.HeaderCredential),
		"a non-install ticket must carry no %s header, got", upgradeconn.HeaderCredential)
	c.Eq(0, store.rotateCount(), "a non-install ticket must rotate nothing, got")
	c.Eq("durable-credential", store.currentCredential(), "the live credential must be untouched, got")
	c.NoError(conn.Close(), "close")
}

// The rotation sits AFTER the enabled check on purpose: a disabled row refused
// its ticket must not lose the credential it still holds, or re-enabling it
// leaves the executor locked out until someone reissues a secret by hand.
func TestRedeemInstallTicketDisabledRowRefusedWithoutRotation(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "durable-credential")
	store.update(func(e *executors.Executor) { e.Enabled = false })
	p := New(store)

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")

	ref := requireRefused(t, exchangeErr(t, p, ticketHeader(ticket)), http.StatusUnauthorized)
	c.StrContains(ref.Reason, "disabled", "the refusal must name the reason, got")
	c.Eq(0, store.rotateCount(), "a disabled row must be refused BEFORE the rotation, rotations =")
	c.Eq("durable-credential", store.currentCredential(), "the refused ticket must not spend the live credential, got")
}

// "I could not rotate it" is not "your ticket is invalid". A 401 here is
// terminal to the executor, so a database blip during a fleet-wide relaunch
// would take every machine out of service at once.
func TestRotateCredentialStoreOutageIsRetryable(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "the-old-credential")
	store.rotateErr = errors.New("failed to connect to `host=db.internal user=rafiki`: connection refused")
	p := New(store)

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")

	err = exchangeErr(t, p, ticketHeader(ticket))
	ref := requireRefused(t, err, http.StatusServiceUnavailable)
	c.Eq("rafikid could not verify the credential right now; retry", ref.Reason,
		"a rotation outage must answer the fixed retryable text, got")
	for _, leak := range []string{"db.internal", "user=rafiki", "connection refused"} {
		c.NotStrContains(ref.Reason, leak, "the response leaked")
	}
	c.False(errors.Is(classifyRefusal(err), ErrEnrollmentRejected),
		"a rotation outage classified as terminal; the executor would exit permanently: %v", err)
	c.Eq("the-old-credential", store.currentCredential(),
		"a failed rotation must leave the stored credential alone, got")
}

// A store READ outage while resolving a row-bound ticket is the same class of
// answer: retryable, and therefore it must cost nothing. The ticket is consumed
// only after admission and the rotation, so a Get that fails leaves it
// redeemable and the SAME ticket works once the store is back — the guard that
// dies if consumption moves back above the row read.
func TestRedeemInstallTicketStoreOutageLeavesTheTicketRedeemable(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "the-old-credential")
	p := New(store)

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")

	store.failNextGet(errors.New("failed to connect to `host=db.internal user=rafiki`: connection refused"))

	err = exchangeErr(t, p, ticketHeader(ticket))
	ref := requireRefused(t, err, http.StatusServiceUnavailable)
	c.Eq("rafikid could not verify the credential right now; retry", ref.Reason,
		"a row-read outage must answer the fixed retryable text, got")
	c.False(errors.Is(classifyRefusal(err), ErrEnrollmentRejected),
		"a row-read outage classified as terminal; the executor would exit permanently: %v", err)
	c.Eq(0, store.rotateCount(), "a row the daemon could not read must rotate nothing, rotations =")

	conn, hdr, err := upgradeExchange(t, p, ticketHeader(ticket))
	c.NoError(err, "the SAME ticket must redeem once the store is back; a ticket spent by the "+
		"outage would make this a terminal 401 and strand the executor")
	c.Eq(1, store.rotateCount(), "the retry must complete the one rotation, rotations =")
	minted := hdr.Get(upgradeconn.HeaderCredential)
	c.NotEq("", minted, "the retried redemption must still hand over %s", upgradeconn.HeaderCredential)
	c.Eq(minted, store.currentCredential(), "the header's plaintext must be the one the store holds, got")
	c.NoError(conn.Close(), "close")

	// ...and the retry that succeeded is what spent it.
	c.ErrorIs(classifyRefusal(exchangeErr(t, p, ticketHeader(ticket))), ErrEnrollmentRejected,
		"the ticket must be spent by the redemption that succeeded; replaying it is terminal")
	c.Eq(1, store.rotateCount(), "a replay after the successful retry rotated again, rotations =")
}

// A ticket redeems once. A second redemption is terminal and must not rotate
// again — otherwise whoever replays a captured ticket keeps invalidating the
// credential of the executor that legitimately holds it.
//
// The consumption is the LAST step of a successful redemption (after admission
// and the rotation), which is what leaves every retryable refusal ahead of it
// holding an unspent ticket. The replay below therefore finds it gone.
func TestRedeemInstallTicketIsOneShot(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "the-old-credential")
	p := New(store)

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")

	conn, _, err := upgradeExchange(t, p, ticketHeader(ticket))
	c.NoError(err, "the first redemption must be admitted")
	c.NoError(conn.Close(), "close")
	c.Eq(1, store.rotateCount(), "first redemption rotations =")

	err = exchangeErr(t, p, ticketHeader(ticket))
	ref := requireRefused(t, err, http.StatusUnauthorized)
	c.StrContains(ref.Reason, "session ticket is unknown, already used, or revoked",
		"a spent ticket must say so, got")
	c.Eq(1, store.rotateCount(), "a replayed ticket rotated the credential again, rotations =")
	c.ErrorIs(classifyRefusal(err), ErrEnrollmentRejected,
		"a spent ticket must classify as terminal; retrying cannot revive it")
}

// A refused ticket must not spend the credential a live executor holds.
//
// The rotation is what invalidates that credential, so it may run only after
// every refusal path has returned — including p.admit's 409 when a healthy
// incumbent answers. Rotating before admission would leave the incumbent on a
// dead secret, and its next reconnect would be a terminal 401: the exact shape
// of a reinstall knocking its own machine out of service.
func TestRedeemInstallTicketWithLiveIncumbentRotatesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "the-incumbent-credential")
	p := New(store)
	p.healthInterval = time.Hour // the incumbent answers; nothing should evict it

	incumbent := joinAsCredential(t, p, &stubHandler{executorID: "exec-1"}, "the-incumbent-credential")
	waitFor(t, 5*time.Second, "the incumbent to join", func() bool { return len(p.Live()) == 1 })

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")

	ref := requireRefused(t, exchangeErr(t, p, ticketHeader(ticket)), http.StatusConflict)
	c.StrContains(ref.Reason, "already live", "the refusal must say the incumbent is live, got")

	c.Eq(0, store.rotateCount(),
		"a 409-refused ticket rotated the credential, rotations =")
	c.Eq("the-incumbent-credential", store.currentCredential(),
		"the refused ticket spent the credential the live executor holds, got")

	_, err = store.Authenticate(t.Context(), "the-incumbent-credential")
	c.NoError(err, "the incumbent's credential must still authenticate after the refusal")
	c.Len(p.Live(), 1, "the incumbent must still be the live connection")
	c.NoError(incumbent.Close(), "close the incumbent")
}

// A 409 says "retry shortly", so the retry has to be possible. The ticket is
// consumed only after p.admit has succeeded, which means a 409 against a healthy
// incumbent costs nothing: the SAME ticket redeems once the incumbent is gone —
// the guard that dies if consumption moves back above admit.
func TestRedeemInstallTicketAdmitRefusalLeavesTheTicketRedeemable(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "the-incumbent-credential")
	p := New(store)
	p.healthInterval = time.Hour // the incumbent answers; nothing should evict it

	incumbent := joinAsCredential(t, p, &stubHandler{executorID: "exec-1"}, "the-incumbent-credential")
	waitFor(t, 5*time.Second, "the incumbent to join", func() bool { return len(p.Live()) == 1 })

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")

	ref := requireRefused(t, exchangeErr(t, p, ticketHeader(ticket)), http.StatusConflict)
	c.StrContains(ref.Reason, "retry shortly", "a 409 must invite a retry, got")
	c.Eq(0, store.rotateCount(), "a 409-refused ticket rotated the credential, rotations =")
	c.Eq("the-incumbent-credential", store.currentCredential(),
		"the refused ticket spent the credential the live executor holds, got")

	c.NoError(incumbent.Close(), "close the incumbent")
	waitFor(t, 5*time.Second, "the incumbent to leave", func() bool { return len(p.Live()) == 0 })

	conn, hdr, err := upgradeExchange(t, p, ticketHeader(ticket))
	c.NoError(err, "the SAME ticket must redeem once the incumbent is gone; a ticket spent by "+
		"the 409 would make the retry it invited a terminal 401")
	c.Eq(1, store.rotateCount(), "the admitted retry must rotate once, rotations =")
	minted := hdr.Get(upgradeconn.HeaderCredential)
	c.NotEq("", minted, "the retried redemption must still hand over %s", upgradeconn.HeaderCredential)
	c.Eq(minted, store.currentCredential(), "the header's plaintext must be the one the store holds, got")
	c.NoError(conn.Close(), "close")
}

// A ticket revoked while its connection is mid-flight is not one this daemon
// authenticated: the consume is the last word, so the answer is the same
// terminal 401 a spent ticket earns and NO credential rides it — handing a
// credential to a launch the daemon has given up on would leave that launch's
// credential live with nobody holding it.
//
// Asserted at authorize rather than over the wire because upgradeconn.Dial
// discards a refusal's headers, and "carries no Rafiki-Credential" is the whole
// subject; the rotation itself is expected to have happened by then (that cost
// is what authorize's consume comment records).
func TestRedeemInstallTicketRevokedBeforeConsumeIsTerminal(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "the-old-credential")
	p := New(store)

	ticket, err := p.Tickets().Mint(TicketGrant{
		Owner: "brent", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint install ticket")
	store.revokeOnRotate(func() { p.Tickets().Revoke(ticket) })

	a, hdr, err := p.authorize(ticketRequest(t, ticket))
	var ref *upgradeconn.Refusal
	c.True(errors.As(err, &ref), "a revoked ticket must be refused, got %v", err)
	c.Eq(http.StatusUnauthorized, ref.Status, "a revoked ticket is terminal, got status")
	c.StrContains(ref.Reason, "session ticket is unknown, already used, or revoked",
		"a revoked ticket must say so, got")
	c.Eq("", hdr.Get(upgradeconn.HeaderCredential),
		"a refused redemption must carry no %s header, got", upgradeconn.HeaderCredential)
	c.Empty(a.credential, "a refused redemption must admit nothing, got %+v", a)
	c.Eq(1, store.rotateCount(),
		"the rotation runs before the commit, so it has already happened here, rotations =")
	_, still := p.Tickets().Claim(ticket)
	c.False(still, "a revoked ticket must stay revoked; no release may put it back")
}

// A ticket is presented by one process, but a REPLAY is not ours to refuse by
// convention: two connections holding the same ticket must not both rotate the
// row's credential, because the second rotation would invalidate the plaintext
// the first was just handed and the executor that won would exit on its next
// reconnect. The claim is what makes that impossible.
func TestRedeemInstallTicketConcurrentPresentationsRotateOnce(t *testing.T) {
	c := assert.NewAborting(t)
	store := newTicketCredStore("exec-1", "durable-credential")
	p := New(store)
	ticket, err := p.Tickets().Mint(TicketGrant{
		ExecutorID: "exec-1", RowID: "exec-1", MintCredential: true,
	})
	c.NoError(err, "mint")

	type result struct {
		hdr http.Header
		err error
	}
	const racers = 4
	start := make(chan struct{})
	results := make(chan result, racers)
	for range racers {
		go func() {
			<-start
			_, hdr, aerr := p.authorize(ticketRequest(t, ticket))
			results <- result{hdr: hdr, err: aerr}
		}()
	}
	close(start)

	var admitted, refused int
	for range racers {
		res := <-results
		if res.err == nil {
			admitted++
			c.NotEq("", res.hdr.Get(upgradeconn.HeaderCredential),
				"the one admitted presentation must leave with the minted credential, got")
			continue
		}
		var ref *upgradeconn.Refusal
		c.True(errors.As(res.err, &ref), "a losing presentation must be refused, got %v", res.err)
		c.Eq(http.StatusUnauthorized, ref.Status, "a spent ticket is terminal, got status")
		c.Eq("", res.hdr.Get(upgradeconn.HeaderCredential),
			"a refused presentation must carry no credential header, got")
		refused++
	}
	c.Eq(1, admitted, "exactly one presentation of one ticket may be admitted, admitted =")
	c.Eq(racers-1, refused, "every other presentation is refused, refused =")
	c.Eq(1, store.rotateCount(),
		"the row's credential must be rotated ONCE for one ticket; a second rotation "+
			"invalidates what the winner was handed, rotations =")
}

// joinAsCredential performs the full executor link against p with a Bearer
// credential the caller chooses — joinViaUpgradeConn hardcodes one, and these
// tests need the row's real credential so the incumbent's survival is
// observable at the store as well as in the pool.
func joinAsCredential(t *testing.T, p *Pool, handler executorpbconnect.ExecutorServiceHandler, credential string) net.Conn {
	t.Helper()
	c := assert.NewAborting(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	c.NoError(err)
	t.Cleanup(func() { _ = ln.Close() })

	mux := http.NewServeMux()
	mux.Handle(upgradeconn.PathFor(upgradeconn.Executor), p.UpgradeHandler())
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	dialed, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig(t))
	c.NoError(err)
	t.Cleanup(func() { _ = dialed.Close() })
	c.NoError(dialed.Handshake())

	upConn, _, err := upgradeconn.Dial(dialed, upgradeconn.Executor, "localhost",
		http.Header{"Authorization": {string(upgradeconn.SchemeBearer) + " " + credential}})
	c.NoError(err, "the incumbent must be admitted")

	_, h := executorpbconnect.NewExecutorServiceHandler(handler)
	go func() { _ = ServeInverted(t.Context(), upConn, h) }()
	return upConn
}

// exchangeErr runs one upgrade exchange and returns only its error, for the
// refusal assertions that need no connection; a successful 101 is closed at
// once so it never counts as an incumbent on a later dial.
func exchangeErr(t *testing.T, p *Pool, hdr http.Header) error {
	t.Helper()
	conn, _, err := upgradeExchange(t, p, hdr)
	if err == nil {
		_ = conn.Close()
	}
	return err
}
