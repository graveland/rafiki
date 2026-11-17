package execpool

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"

	"github.com/multigres/testkit/assert"
)

// A direct install hands a self-dialling executor exactly one secret: a one-shot
// ticket on its FIRST connect. Everything after that is the credential the 101
// carried, written to CredentialFile and presented as Bearer from then on.
//
// These tests join the two halves that were built separately — the dial client
// (ConnectOptions.InstallTicket, OnEnrolled, CredentialFile) and the pool's
// authorize (TicketGrant{RowID, MintCredential} and store.RotateCredential) —
// against a real Pool and a real Connect over TLS. Each half can be right on its
// own while the chain between them is broken, and what the chain has to promise
// the remote machine is what these pin: the ticket buys one connection, the
// credential it mints survives a reconnect, a relaunch replaces it and kills its
// predecessor, and every answer that cannot change on a retry ends the process
// instead of looping.

// directInstallStore is the durable row a direct install's credential chain runs
// against: exactly one credential authenticates at a time, RotateCredential
// mints the next one, a rotation outage can be armed for one call, and the row
// can be deleted the way uninstall deletes it.
type directInstallStore struct {
	*fakeStore

	rowID string

	mu         sync.Mutex
	cred       string
	rotations  int
	rotateErrs []error
	auths      int
}

func newDirectInstallStore(id, cred string) *directInstallStore {
	return &directInstallStore{fakeStore: newFakeStore(id), rowID: id, cred: cred}
}

func (s *directInstallStore) Authenticate(ctx context.Context, credential string) (executors.Executor, error) {
	s.mu.Lock()
	current := s.cred
	s.mu.Unlock()
	if credential != current {
		return executors.Executor{}, executors.ErrNotFound
	}
	s.mu.Lock()
	s.auths++
	s.mu.Unlock()
	// The ROW is the authority, never the credential: a real store re-reads the
	// row on every connection, which is why a deleted row stops authenticating
	// the credential it was issued.
	return s.Get(ctx, s.rowID)
}

func (s *directInstallStore) RotateCredential(ctx context.Context, id string) (string, error) {
	s.mu.Lock()
	if len(s.rotateErrs) > 0 {
		err := s.rotateErrs[0]
		s.rotateErrs = s.rotateErrs[1:]
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	if _, err := s.Get(ctx, id); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotations++
	s.cred = fmt.Sprintf("minted-%d", s.rotations)
	return s.cred, nil
}

// failNextRotation arms one rotation outage, consumed by the next call.
func (s *directInstallStore) failNextRotation(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateErrs = append(s.rotateErrs, err)
}

func (s *directInstallStore) rotateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotations
}

func (s *directInstallStore) authCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auths
}

func (s *directInstallStore) currentCredential() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

// countingListener counts the connections the daemon ACCEPTED. Connect dials
// once per attempt, so the delta across a call is that call's attempt count —
// which is what "exactly one dial" means for a refusal that must not be retried.
type countingListener struct {
	net.Listener
	mu      sync.Mutex
	accepts int
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.accepts++
		l.mu.Unlock()
	}
	return c, err
}

func (l *countingListener) dials() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// directInstallFixture is one daemon endpoint — a real Pool on a real TLS
// listener, the shape servePool builds — plus the dial count and the deadline
// every test in this file is bounded by.
type directInstallFixture struct {
	addr  string
	pin   string
	pool  *Pool
	ln    *countingListener
	store *directInstallStore
	ctx   context.Context
}

func newDirectInstallFixture(t *testing.T, store *directInstallStore) *directInstallFixture {
	t.Helper()
	fastReconnect(t)

	cert := testCert(t)
	sum := sha256.Sum256(cert.Certificate[0])

	raw, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   ALPNProtocols,
	})
	assert.NewAborting(t).NoError(err, "tls.Listen")

	ln := &countingListener{Listener: raw}
	t.Cleanup(func() { _ = raw.Close() })

	p := New(store)
	go func() { _ = p.Serve(ln) }()

	// A regression in this chain loops rather than failing, so every test here
	// runs under a deadline: the loop dies instead of wedging the package.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return &directInstallFixture{
		addr: ln.Addr().String(), pin: fmt.Sprintf("%x", sum),
		pool: p, ln: ln, store: store, ctx: ctx,
	}
}

func (f *directInstallFixture) options(t *testing.T, credFile, ticket string) ConnectOptions {
	t.Helper()
	_, handler := executorpbconnect.NewExecutorServiceHandler(&stubHandler{executorID: f.store.rowID})
	return ConnectOptions{
		Addr:           f.addr,
		ServerName:     "localhost",
		PinCert:        f.pin,
		CredentialFile: credFile,
		InstallTicket:  ticket,
		Handler:        handler,
	}
}

// mintInstallTicket mints what the launcher mints for a direct launch: a
// row-bound ticket flagged to rotate the credential it redeems.
func (f *directInstallFixture) mintInstallTicket(t *testing.T) string {
	t.Helper()
	ticket, err := f.pool.Tickets().Mint(TicketGrant{RowID: f.store.rowID, MintCredential: true})
	assert.NewAborting(t).NoError(err, "mint the install ticket")
	return ticket
}

func (f *directInstallFixture) waitJoined(t *testing.T) {
	t.Helper()
	waitFor(t, 5*time.Second, "the executor to join the pool", func() bool {
		live := f.pool.Live()
		return len(live) == 1 && live[0].Executor.ID == f.store.rowID
	})
}

func (f *directInstallFixture) waitLeft(t *testing.T) {
	t.Helper()
	waitFor(t, 5*time.Second, "the executor to leave the pool", func() bool { return len(f.pool.Live()) == 0 })
}

// installRun is one Connect in the background: the test asserts while it serves,
// then stop ends it the way the process ends — its context cancelled — and
// reports what Connect returned.
type installRun struct {
	cancel context.CancelFunc
	errs   chan error
}

func (f *directInstallFixture) start(o ConnectOptions) *installRun {
	ctx, cancel := context.WithCancel(f.ctx)
	run := &installRun{cancel: cancel, errs: make(chan error, 1)}
	go func() { run.errs <- Connect(ctx, o) }()
	return run
}

func (r *installRun) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.errs:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Connect did not return after its context was cancelled")
		return nil
	}
}

// oneShotConnect runs Connect to completion under the fixture's deadline, for
// the refusals where the returned error IS the subject.
func (f *directInstallFixture) oneShotConnect(o ConnectOptions) error {
	return Connect(f.ctx, o)
}

// credentialIn reads back the credential a connect stored, the way the next
// process will.
func credentialIn(t *testing.T, path string) string {
	t.Helper()
	cred, err := readCredential(path)
	assert.NewAborting(t).Require().NoError(err, "read the credential file at %s", path)
	return cred
}

// The first launch is the only one that holds a ticket. It must leave a
// credential on disk, and the reconnect — the same process after its connection
// dropped — must authenticate with that file instead of re-presenting a ticket
// the daemon has already spent.
func TestInstallDirectTicketThenBearerReconnect(t *testing.T) {
	c := assert.NewAborting(t)
	store := newDirectInstallStore("exec-direct-first", "credential-before-the-install")
	f := newDirectInstallFixture(t, store)

	credFile := filepath.Join(t.TempDir(), "state", "executor.cred")
	ticket := f.mintInstallTicket(t)

	var enrolled atomic.Int32
	o := f.options(t, credFile, ticket)
	o.OnEnrolled = func() { enrolled.Add(1) }

	first := f.start(o)
	f.waitJoined(t)

	c.Eq(int32(1), enrolled.Load(), "OnEnrolled must run once, after the credential is on disk; got")
	stored := credentialIn(t, credFile)
	c.NotEq("", stored, "the credential file must hold the credential the 101 carried")
	c.Eq(store.currentCredential(), stored, "the stored credential must be the one this redemption minted, got")
	st, err := os.Stat(credFile)
	c.Require().NoError(err, "stat the credential file")
	c.Eq(os.FileMode(0o600), st.Mode().Perm(), "the credential file must be 0600, got")
	c.Eq(1, store.rotateCount(), "one redemption rotates the row credential exactly once, rotations =")
	c.Eq(0, store.authCount(), "the ticket is dial 1's credential, so no Bearer auth may run, auths =")
	c.Eq(1, f.ln.dials(), "the first launch dials once, dials =")

	// The connection drops under the running launch. This is the reconnect a
	// long-lived executor makes from the SAME process and the same options
	// value, so what it presents on the second dial is whatever the redemption
	// left behind: presenting the ticket again would earn a 401 the loop treats
	// as terminal, and the executor would never come back.
	f.pool.Evict(store.rowID)
	waitFor(t, 5*time.Second, "the executor to rejoin after its connection dropped", func() bool {
		return len(f.pool.Live()) == 1 && store.authCount() == 1
	})

	c.Eq(2, f.ln.dials(), "the reconnect is one more dial, dials =")
	c.Eq(1, store.authCount(), "the reconnect authenticates the stored credential, and only once, auths =")
	c.Eq(1, store.rotateCount(), "a Bearer reconnect redeems no ticket and rotates nothing, rotations =")
	_, spent := f.pool.Tickets().Redeem(ticket)
	c.False(spent, "the install ticket must be spent exactly once, and it was still redeemable after two dials")

	c.ErrorIs(first.stop(t), context.Canceled, "a launch serves until its context ends")
}

// A relaunch is the second launch of the same row: the machine still holds the
// previous credential on disk and the launcher mints a fresh ticket for it. The
// new ticket must replace that credential, and the replaced one must stop
// authenticating at once — otherwise the previous process, still running
// somewhere, keeps its way in.
func TestInstallDirectRelaunchRotatesAndKillsTheOldCredential(t *testing.T) {
	c := assert.NewAborting(t)
	store := newDirectInstallStore("exec-direct-relaunch", "credential-before-the-install")
	f := newDirectInstallFixture(t, store)

	credFile := filepath.Join(t.TempDir(), "executor.cred")

	firstTicket := f.mintInstallTicket(t)
	firstRun := f.start(f.options(t, credFile, firstTicket))
	f.waitJoined(t)
	firstCred := credentialIn(t, credFile)
	c.NotEq("", firstCred, "the first launch must store a credential")
	c.ErrorIs(firstRun.stop(t), context.Canceled, "the first launch serves until its context ends")
	f.waitLeft(t)

	secondTicket := f.mintInstallTicket(t)
	c.NotEq(firstTicket, secondTicket, "each launch gets its own ticket")

	secondRun := f.start(f.options(t, credFile, secondTicket))
	f.waitJoined(t)

	secondCred := credentialIn(t, credFile)
	c.NotEq(firstCred, secondCred, "a relaunch must replace the credential on disk, got the previous one back")
	c.Eq(store.currentCredential(), secondCred, "the file must hold the credential the relaunch minted, got")
	c.Eq(2, store.rotateCount(), "each launch rotates the row credential once, rotations =")
	c.ErrorIs(secondRun.stop(t), context.Canceled, "the relaunched executor serves until its context ends")
	f.waitLeft(t)

	// The replaced credential is dead, and the answer is terminal: a machine
	// still running the old process must stop rather than loop.
	dials := f.ln.dials()
	old := f.options(t, "", "")
	old.Credential = firstCred
	started := time.Now()
	err := f.oneShotConnect(old)
	c.ErrorIs(err, ErrEnrollmentRejected, "the credential a relaunch replaced must be refused terminally, got %v", err)
	c.Eq(1, f.ln.dials()-dials, "a 401 on a replaced credential must not be retried, dials =")
	c.LessOrEqual(time.Second, time.Since(started), "a terminal refusal must stop at once, took")
	c.Eq(2, store.rotateCount(), "a refused credential must rotate nothing, rotations =")

	// ...and the replacement still authenticates the same row.
	lastRun := f.start(f.options(t, credFile, ""))
	f.waitJoined(t)
	c.Eq(1, store.authCount(), "the relaunched credential must authenticate the row, auths =")
	c.ErrorIs(lastRun.stop(t), context.Canceled, "the executor serves until its context ends")
}

// A ticket redeems once. Replaying a spent one cannot authenticate anything, and
// the answer cannot change on a retry, so the executor must stop after exactly
// one dial — a replay that keeps redialling spins for the life of the process.
func TestInstallDirectSpentTicketIsTerminalNotRetried(t *testing.T) {
	c := assert.NewAborting(t)
	store := newDirectInstallStore("exec-direct-spent", "credential-before-the-install")
	f := newDirectInstallFixture(t, store)

	credFile := filepath.Join(t.TempDir(), "executor.cred")
	ticket := f.mintInstallTicket(t)

	run := f.start(f.options(t, credFile, ticket))
	f.waitJoined(t)
	c.ErrorIs(run.stop(t), context.Canceled, "the launch serves until its context ends")
	f.waitLeft(t)

	dials := f.ln.dials()
	replay := filepath.Join(t.TempDir(), "replay", "executor.cred")
	err := f.oneShotConnect(f.options(t, replay, ticket))
	c.ErrorIs(err, ErrEnrollmentRejected, "a replayed install ticket must be terminal, got %v", err)
	c.StrContains(err.Error(), "already used", "the refusal must say the ticket is spent, got")
	c.Eq(1, f.ln.dials()-dials, "a spent ticket must be answered in exactly one dial, dials =")
	c.Eq(1, store.rotateCount(), "a replayed ticket must rotate nothing, rotations =")
	_, readable := os.Stat(replay)
	c.True(os.IsNotExist(readable), "a refused replay must write no credential, stat error = %v", readable)
}

// Uninstall with the launcher unreachable deletes the row and leaves the remote
// running. Its next reconnect must be the credential check acting on the row it
// no longer has: a terminal 401, so the machine takes itself out of service
// rather than redialling a daemon that will never admit it again.
func TestInstallDirectDeletedRowIsTerminalForTheRemote(t *testing.T) {
	c := assert.NewAborting(t)
	store := newDirectInstallStore("exec-direct-uninstalled", "credential-before-the-install")
	f := newDirectInstallFixture(t, store)

	credFile := filepath.Join(t.TempDir(), "executor.cred")
	run := f.start(f.options(t, credFile, f.mintInstallTicket(t)))
	f.waitJoined(t)
	issued := credentialIn(t, credFile)
	c.NotEq("", issued, "the launch must store a credential")
	c.ErrorIs(run.stop(t), context.Canceled, "the launch serves until its context ends")
	f.waitLeft(t)

	store.delete()

	dials := f.ln.dials()
	err := f.oneShotConnect(f.options(t, credFile, ""))
	c.ErrorIs(err, ErrEnrollmentRejected, "an executor whose row was deleted must be refused terminally, got %v", err)
	c.Eq(1, f.ln.dials()-dials, "a deleted row must be answered in exactly one dial, dials =")
	c.Eq(1, store.rotateCount(), "nothing may rotate for a deleted row, rotations =")
	c.Eq(issued, credentialIn(t, credFile), "a refused reconnect must leave the credential file alone, got")

	_, gerr := store.Get(t.Context(), store.rowID)
	c.ErrorIs(gerr, executors.ErrNotFound, "the row must be gone; the refusal came from the row, not the credential")
}

// "I could not rotate it" is not "your ticket is invalid". A store failure
// during the redemption answers the retryable 503, so the executor redials — and
// the ticket must still be redeemable, because the redemption bought nothing and
// spending it would turn that retry into a terminal 401. A database blip during
// a fleet-wide relaunch must not take every machine out of service at once.
func TestInstallDirectStoreOutageDuringRedemptionIsRetried(t *testing.T) {
	c := assert.NewAborting(t)
	store := newDirectInstallStore("exec-direct-outage", "credential-before-the-install")
	f := newDirectInstallFixture(t, store)
	store.failNextRotation(errors.New("failed to connect to `host=db.internal user=rafiki`: connection refused"))

	credFile := filepath.Join(t.TempDir(), "executor.cred")
	ticket := f.mintInstallTicket(t)

	started := time.Now()
	run := f.start(f.options(t, credFile, ticket))
	f.waitJoined(t)

	c.Eq(2, f.ln.dials(), "the interrupted redemption must be retried, so the launch dials twice, dials =")
	c.LessOrEqual(time.Second, time.Since(started), "the retry must come back at once, took")
	c.Eq(1, store.rotateCount(), "the retry must complete the one rotation, rotations =")
	c.Eq(store.currentCredential(), credentialIn(t, credFile), "the credential the retry minted must be on disk, got")
	_, spent := f.pool.Tickets().Redeem(ticket)
	c.False(spent, "the ticket must be spent by the redemption that succeeded")

	c.ErrorIs(run.stop(t), context.Canceled, "the launch serves until its context ends")

	// The credential the retry minted is the one the next reconnect uses.
	f.waitLeft(t)
	finalRun := f.start(f.options(t, credFile, ""))
	f.waitJoined(t)
	c.Eq(1, store.authCount(), "the credential written after the retry must authenticate the row, auths =")
	c.Eq(3, f.ln.dials(), "the reconnect after the retry is one more dial, dials =")
	c.ErrorIs(finalRun.stop(t), context.Canceled, "the executor serves until its context ends")
}
